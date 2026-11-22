package oe

import (
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/engine"
)

// ServerOptions configures a Server. A zero duration or size takes the
// value docs/API.md gives.
type ServerOptions struct {
	Engine  Commands     // takes the sessions' orders and cancels; nil: none are served
	Symbols []string     // the engine's symbols, in index order
	Logger  *slog.Logger // nil: no logging

	Heartbeat    time.Duration // send a Heartbeat after this long without sending; 1 s
	Idle         time.Duration // close a connection on which nothing arrived for this long; 15 s
	MaxUnsent    int           // bytes of output a session may have waiting to be written; 1 MB
	WriteTimeout time.Duration // longest a write may take; 10 s
}

// Commands is what a Server needs of the engine; *engine.Engine has it.
type Commands interface {
	SendOrder(s engine.Sink, tag uint64, o engine.NewOrder) (book.OrderID, error)
	SendCancel(s engine.Sink, tag uint64, id book.OrderID) error
}

// Server serves OE1 sessions. This version takes a Login to a new session,
// NewOrder, Cancel, Ping, Heartbeat and Logout; resuming a session is not
// served yet.
//
// Each connection has a reader, which decodes every whole frame it has,
// passes commands to the engine and queues the answers it can give at once,
// and a writer, which sends all that is queued with one write whenever
// anything is. The engine's goroutine for a symbol queues the replies to
// commands (session.go).
type Server struct {
	opts ServerOptions

	mu       sync.Mutex
	ln       net.Listener
	lnErr    error // of closing ln
	sessions map[*session]struct{}
	closed   bool // no new connections, and sessions are ending
	wg       sync.WaitGroup
}

// NewServer returns a Server with opts.
func NewServer(opts ServerOptions) *Server {
	if opts.Heartbeat <= 0 {
		opts.Heartbeat = time.Second
	}
	if opts.Idle <= 0 {
		opts.Idle = 15 * time.Second
	}
	if opts.MaxUnsent <= 0 {
		opts.MaxUnsent = 1 << 20
	}
	if opts.WriteTimeout <= 0 {
		opts.WriteTimeout = 10 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	return &Server{opts: opts, sessions: map[*session]struct{}{}}
}

// Serve accepts connections on ln until Close, and then returns nil.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ln.Close()
	}
	s.ln = ln
	s.mu.Unlock()
	for {
		c, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			return err
		}
		ss := newSession(s, c)
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = c.Close()
			continue
		}
		s.sessions[ss] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go ss.serve()
	}
}

// BeginShutdown stops accepting connections and ends every session: a
// command that arrives from now on is answered with shutting_down, and a
// session that has answered every command it took gets EndOfSession and is
// closed.
func (s *Server) BeginShutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.ln != nil {
		s.lnErr = s.ln.Close()
		s.ln = nil
	}
	for ss := range s.sessions {
		ss.beginDrain(drainShutdown)
	}
}

// Close begins the shutdown and waits until every connection is closed,
// which a client that does not read can delay by up to two write timeouts:
// the write in progress, then the last one.
func (s *Server) Close() error {
	s.BeginShutdown()
	s.wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lnErr
}

// errEnd ends a session after a refused Login.
var errEnd = errors.New("oe: session ended")

// errLogout ends the reading of a session at the client's request. The
// session then ends when every command it took has been answered.
var errLogout = errors.New("oe: logout")

// errSlowConsumer ends a session whose unsent output passed MaxUnsent.
var errSlowConsumer = errors.New("oe: slow consumer")

// state is how far a session's writer has got towards closing.
type state uint8

const (
	running state = iota
	ending        // send what is queued, then close
	aborted       // close now
)

// drain says why a session answers the commands it has taken and takes no
// more.
type drain uint8

const (
	drainNone     drain = iota
	drainLogout         // the client sent Logout
	drainShutdown       // the server is shutting down: EndOfSession follows
)

// session is one connection. The reader goroutine owns id and
// lastClientSeq; mu guards the rest, which the engine's goroutines reach
// through Reply.
type session struct {
	srv  *Server
	conn net.Conn

	id            uint64 // 0 until logged in
	lastClientSeq uint64
	writerDone    chan struct{} // closed when the writer has exited; nil before login

	mu            sync.Mutex
	room          sync.Cond // on mu: taken fell below maxTaken, or the session is ending
	out           []byte    // frames queued for the writer
	inFlight      int       // bytes of the write in progress
	state         state
	drain         drain
	abortErr      error  // why the session was aborted from Reply
	nextServerSeq uint64 // of the next Event or Done
	taken         int    // commands passed to the engine and not yet answered
	wake          chan struct{}
}

func newSession(s *Server, c net.Conn) *session {
	ss := &session{srv: s, conn: c, wake: make(chan struct{}, 1)}
	ss.room.L = &ss.mu
	return ss
}

func (ss *session) serve() {
	err := ss.read()
	ss.mu.Lock()
	if ss.abortErr != nil {
		err = ss.abortErr
	}
	if ss.state == running && !errors.Is(err, errLogout) {
		// What is queued answers frames already taken, so it is sent
		// before the connection closes, unless there is too much of it.
		ss.state = ending
		if errors.Is(err, errSlowConsumer) {
			ss.state = aborted
		}
	}
	st, done := ss.state, ss.writerDone
	ss.mu.Unlock()
	if st == aborted {
		_ = ss.conn.Close() // unblocks a write in progress
	}
	if done != nil {
		ss.signal()
		<-done
	} else if st == ending { // not logged in: no writer yet
		ss.flush()
	}
	_ = ss.conn.Close()

	s := ss.srv
	s.mu.Lock()
	delete(s.sessions, ss)
	s.mu.Unlock()
	s.opts.Logger.Info("oe session closed", "session", ss.id, "remote", ss.conn.RemoteAddr().String(), "reason", err.Error())
	s.wg.Done()
}

// beginDrain makes the session answer the commands it has taken and take
// no more, and end when none is left: with EndOfSession if the server is
// shutting down. A session that has not logged in has nothing to answer
// and ends at once.
func (ss *session) beginDrain(d drain) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.state != running {
		return
	}
	if ss.writerDone == nil {
		ss.state = aborted
		_ = ss.conn.Close()
		return
	}
	if ss.drain == drainNone {
		ss.drain = d
	}
	ss.room.Broadcast() // a reader waiting for room answers shutting_down
	if ss.taken == 0 {
		ss.endLocked()
	}
}

// endLocked ends a session that has answered every command it took.
func (ss *session) endLocked() {
	if ss.state != running {
		return
	}
	if ss.drain == drainShutdown {
		ss.out = EndOfSession{}.Append(ss.out)
	}
	ss.state = ending
	ss.signal()
}

// abortLocked closes the connection now, for the reason err.
func (ss *session) abortLocked(err error) {
	if ss.state == aborted {
		return
	}
	ss.state, ss.abortErr = aborted, err
	_ = ss.conn.Close() // unblocks the reader and a write in progress
	ss.room.Broadcast()
}

// read decodes the frames that arrive, until an error or the end of the
// session.
func (ss *session) read() error {
	buf := make([]byte, 64<<10)
	have := 0
	for {
		if err := ss.conn.SetReadDeadline(time.Now().Add(ss.srv.opts.Idle)); err != nil {
			return err
		}
		n, rerr := ss.conn.Read(buf[have:])
		have += n
		off := 0
		for {
			t, body, size, err := Next(buf[off:have])
			if err != nil {
				return err
			}
			if size == 0 {
				break
			}
			if err := ss.handle(t, body); err != nil {
				return err
			}
			off += size
		}
		have = copy(buf, buf[off:have])
		if rerr != nil {
			return rerr
		}
	}
}

func (ss *session) handle(t Type, body []byte) error {
	if ss.id == 0 {
		if t != TypeLogin {
			return fmt.Errorf("oe: %q frame before Login", byte(t))
		}
		return ss.login(DecodeLogin(body))
	}
	switch t {
	case TypeNewOrder:
		return ss.newOrder(DecodeNewOrder(body))
	case TypeCancel:
		return ss.cancel(DecodeCancel(body))
	case TypePing:
		p := DecodePing(body)
		if ok, err := ss.next(p.ClientSeq); !ok {
			return err
		}
		return queue(ss, Pong(p))
	case TypeHeartbeat:
		return nil
	case TypeLogout:
		ss.beginDrain(drainLogout)
		return errLogout
	default:
		return fmt.Errorf("oe: %q frame not served", byte(t))
	}
}

// next takes a command's client_seq. It reports false for a resend, which
// is dropped, and for a number that skips one, which is an error.
func (ss *session) next(seq uint64) (bool, error) {
	switch {
	case seq <= ss.lastClientSeq:
		return false, nil
	case seq != ss.lastClientSeq+1:
		return false, fmt.Errorf("oe: client_seq %d after %d, want %d", seq, ss.lastClientSeq, ss.lastClientSeq+1)
	}
	ss.lastClientSeq = seq
	return true, nil
}

// login opens a new session, or queues why not.
func (ss *session) login(m Login) error {
	var reason RejectReason
	switch {
	case m.Version != Version:
		reason = RejectBadVersion
	case m.Session != 0:
		reason = RejectUnknownSession // sessions end with their connection
	}
	if reason != 0 {
		ss.out = LoginRejected{Reason: reason}.Append(ss.out)
		return fmt.Errorf("%w: login refused: %v (version %d, want %d; session %d, want 0)", errEnd, reason, m.Version, Version, m.Session)
	}
	id, err := newSessionID()
	if err != nil {
		return err
	}
	s := ss.srv
	ss.id = id
	ss.mu.Lock()
	ss.nextServerSeq = 1
	ss.out = LoginAccepted{Session: ss.id, NextServerSeq: 1}.Append(ss.out)
	for i, name := range s.opts.Symbols {
		ss.out = Symbol{Index: uint8(i), Name: name}.Append(ss.out)
	}
	ss.writerDone = make(chan struct{})
	go ss.write(ss.writerDone)
	ss.mu.Unlock()
	ss.signal()
	return nil
}

// newSessionID returns a random session number other than 0. Numbers
// counted from 1 would start again after a restart, and a client of the
// earlier process could then resume another client's session.
func newSessionID() (uint64, error) {
	var b [8]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			return 0, fmt.Errorf("oe: session number: %w", err)
		}
		if id := le.Uint64(b[:]); id != 0 {
			return id, nil
		}
	}
}

// queue appends f to the session's output and wakes the writer. A session
// that is ending takes nothing more: EndOfSession is its last frame.
func queue[F interface{ Append([]byte) []byte }](ss *session, f F) error {
	ss.mu.Lock()
	if ss.state != running {
		ss.mu.Unlock()
		return errEnd
	}
	ss.out = f.Append(ss.out)
	over := len(ss.out)+ss.inFlight > ss.srv.opts.MaxUnsent
	ss.mu.Unlock()
	if over {
		return errSlowConsumer
	}
	ss.signal()
	return nil
}

func (ss *session) signal() {
	select {
	case ss.wake <- struct{}{}:
	default: // the writer has a wake-up pending
	}
}

// write sends what is queued, one write at a time, and a Heartbeat after
// Heartbeat without sending, until the session ends.
func (ss *session) write(done chan<- struct{}) {
	defer close(done)
	opts := &ss.srv.opts
	hb := time.NewTimer(opts.Heartbeat)
	defer hb.Stop()
	var buf []byte
	for {
		select {
		case <-ss.wake:
		case <-hb.C:
			ss.mu.Lock()
			ss.out = Heartbeat{}.Append(ss.out)
			ss.mu.Unlock()
		}
		ss.mu.Lock()
		buf, ss.out = ss.out, buf[:0]
		ss.inFlight = len(buf)
		st := ss.state
		ss.mu.Unlock()
		if st == aborted {
			return
		}
		if len(buf) > 0 {
			err := ss.writeNow(buf)
			ss.mu.Lock()
			ss.inFlight = 0
			if err != nil {
				ss.abortLocked(err)
			}
			ss.mu.Unlock()
			if err != nil {
				return // the reader sees the closed connection and ends the session
			}
			hb.Reset(opts.Heartbeat)
		}
		if st == ending {
			_ = ss.conn.Close() // the reader may still be waiting for input
			return
		}
	}
}

func (ss *session) writeNow(b []byte) error {
	if err := ss.conn.SetWriteDeadline(time.Now().Add(ss.srv.opts.WriteTimeout)); err != nil {
		return err
	}
	_, err := ss.conn.Write(b)
	return err
}

// flush writes what is queued from the reader's goroutine, before the
// session has a writer.
func (ss *session) flush() {
	if len(ss.out) > 0 {
		_ = ss.writeNow(ss.out) // the connection closes next either way
	}
}
