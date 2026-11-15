package oe

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

// ServerOptions configures a Server. A zero duration or size takes the
// value docs/API.md gives.
type ServerOptions struct {
	Symbols []string     // the engine's symbols, in index order
	Logger  *slog.Logger // nil: no logging

	Heartbeat    time.Duration // send a Heartbeat after this long without sending; 1 s
	Idle         time.Duration // close a connection on which nothing arrived for this long; 15 s
	MaxUnsent    int           // bytes of output a session may have waiting to be written; 1 MB
	WriteTimeout time.Duration // longest a write may take; 10 s
}

// Server serves OE1 sessions. This version takes a Login to a new session,
// Ping, Heartbeat and Logout; NewOrder, Cancel and resuming a session are
// not served yet.
//
// Each connection has a reader, which decodes every whole frame it has
// and queues the answers, and a writer, which sends all that is queued
// with one write whenever anything is.
type Server struct {
	opts ServerOptions

	mu       sync.Mutex
	ln       net.Listener
	sessions map[*session]struct{}
	last     uint64 // the last session number given
	closed   bool
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
		ss := &session{srv: s, conn: c, wake: make(chan struct{}, 1)}
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

// Close stops accepting, ends every session with EndOfSession and waits
// until their connections are closed, which a client that does not read
// can delay by up to two write timeouts: the write in progress, then the
// last one.
func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	var err error
	if s.ln != nil {
		err = s.ln.Close()
		s.ln = nil
	}
	for ss := range s.sessions {
		ss.shutdown()
	}
	s.mu.Unlock()
	s.wg.Wait()
	return err
}

// errEnd ends a session at the client's request, or after a refused
// Login.
var errEnd = errors.New("oe: session ended")

// errSlowConsumer ends a session whose unsent output passed MaxUnsent.
var errSlowConsumer = errors.New("oe: slow consumer")

// state is how far a session's writer has got towards closing.
type state uint8

const (
	running state = iota
	ending        // send what is queued, then close
	aborted       // close now
)

// session is one connection. The reader goroutine owns id and
// lastClientSeq; mu guards out, state and writerDone.
type session struct {
	srv  *Server
	conn net.Conn

	id            uint64 // 0 until logged in
	lastClientSeq uint64
	writerDone    chan struct{} // closed when the writer has exited; nil before login

	mu       sync.Mutex
	out      []byte // frames queued for the writer
	inFlight int    // bytes of the write in progress
	state    state
	wake     chan struct{}
}

func (ss *session) serve() {
	err := ss.read()
	ss.mu.Lock()
	if ss.state == running {
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

// shutdown ends the session with EndOfSession. It runs under the server's
// lock.
func (ss *session) shutdown() {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.state != running {
		return
	}
	if ss.writerDone == nil { // not logged in: nothing to say
		ss.state = aborted
		_ = ss.conn.Close()
		return
	}
	ss.out = EndOfSession{}.Append(ss.out)
	ss.state = ending
	ss.signal()
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

// handle acts on one frame from the client.
func (ss *session) handle(t Type, body []byte) error {
	if ss.id == 0 {
		if t != TypeLogin {
			return fmt.Errorf("oe: %q frame before Login", byte(t))
		}
		return ss.login(DecodeLogin(body))
	}
	switch t {
	case TypePing:
		p := DecodePing(body)
		switch {
		case p.ClientSeq <= ss.lastClientSeq:
			return nil // a resend
		case p.ClientSeq != ss.lastClientSeq+1:
			return fmt.Errorf("oe: client_seq %d after %d", p.ClientSeq, ss.lastClientSeq)
		}
		ss.lastClientSeq = p.ClientSeq
		return queue(ss, Pong(p))
	case TypeHeartbeat:
		return nil
	case TypeLogout:
		return errEnd
	default:
		return fmt.Errorf("oe: %q frame not served", byte(t))
	}
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
		return fmt.Errorf("%w: login refused: %v", errEnd, reason)
	}
	s := ss.srv
	s.mu.Lock()
	s.last++
	ss.id = s.last
	s.mu.Unlock()
	ss.mu.Lock()
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
			ss.mu.Unlock()
			if err != nil {
				_ = ss.conn.Close() // the reader sees it and ends the session
				return
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
