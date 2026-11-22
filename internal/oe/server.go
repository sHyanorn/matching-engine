package oe

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
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
	MaxUnsent    int           // bytes of output a session may have waiting to be written, apart from a resume's replay; 1 MB
	WriteTimeout time.Duration // longest a write may take; 10 s
	Held         int           // bytes of a session's output kept for a resume; 1 MB
	ResumeWindow time.Duration // how long a session is kept after its connection ends; 60 s

	// afterFunc starts a timer, as time.AfterFunc does, and returns its stop
	// function. Tests replace it.
	afterFunc func(d time.Duration, f func()) (stop func() bool)
}

// Commands is what a Server needs of the engine; *engine.Engine has it.
type Commands interface {
	SendOrder(s engine.Sink, tag uint64, o engine.NewOrder) (book.OrderID, error)
	SendCancel(s engine.Sink, tag uint64, id book.OrderID) error
}

// Server serves OE1 sessions.
//
// A session outlives its connection for ResumeWindow, and a Login that
// names it takes over a connection that is still attached. Each connection
// has a reader, which decodes every whole frame it has, passes commands to
// the engine and queues the answers it can give at once, and a writer,
// which sends all that is queued with one write whenever anything is. The
// engine's goroutine for a symbol queues the replies to commands
// (session.go).
type Server struct {
	opts ServerOptions

	mu       sync.Mutex
	ln       net.Listener
	lnErr    error // of closing ln
	conns    map[*conn]struct{}
	sessions map[uint64]*session
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
	if opts.Held <= 0 {
		opts.Held = 1 << 20
	}
	if opts.ResumeWindow <= 0 {
		opts.ResumeWindow = time.Minute
	}
	if opts.afterFunc == nil {
		opts.afterFunc = func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	return &Server{opts: opts, conns: map[*conn]struct{}{}, sessions: map[uint64]*session{}}
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
		nc, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			return err
		}
		c := &conn{srv: s, nc: nc, wake: make(chan struct{}, 1), readerDone: make(chan struct{})}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = nc.Close()
			continue
		}
		s.conns[c] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go c.serve()
	}
}

// BeginShutdown stops accepting connections and ends every session: a
// command that arrives from now on is answered with shutting_down, a
// session that has answered every command it took gets EndOfSession and
// its connection is closed, and a session without a connection is dropped.
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
	for c := range s.conns {
		if !c.loggedIn.Load() {
			_ = c.nc.Close() // nothing to say to a client that has not logged in
		}
	}
	for id, ss := range s.sessions {
		ss.mu.Lock()
		if ss.cur == nil {
			ss.dropLocked()
			delete(s.sessions, id)
		} else {
			ss.beginDrainLocked(drainShutdown)
		}
		ss.mu.Unlock()
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

// remove takes ss out of the table of sessions.
func (s *Server) remove(ss *session) {
	s.mu.Lock()
	delete(s.sessions, ss.id)
	s.mu.Unlock()
}

// expire drops ss if it still has no connection when its time is up.
func (s *Server) expire(ss *session) {
	ss.mu.Lock()
	if ss.cur != nil || ss.gone {
		ss.mu.Unlock()
		return
	}
	ss.dropLocked()
	ss.mu.Unlock()
	s.remove(ss)
	s.opts.Logger.Info("oe session expired", "session", ss.id)
}

// errEnd ends a connection after a refused Login, or one that was not the
// session's any more.
var errEnd = errors.New("oe: connection ended")

// errLogout ends the reading of a connection at the client's request. The
// session then ends when every command it took has been answered.
var errLogout = errors.New("oe: logout")

// errSlowConsumer ends a connection whose unsent output passed MaxUnsent.
var errSlowConsumer = errors.New("oe: slow consumer")

// errTakenOver ends a connection that a Login for its session replaced.
var errTakenOver = errors.New("oe: session taken over by a new connection")

// state is how far a connection's writer has got towards closing.
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

// conn is one connection. Its reader goroutine is the one that runs serve.
// Once logged in, the fields below ss are guarded by ss.mu.
type conn struct {
	srv        *Server
	nc         net.Conn
	wake       chan struct{}
	readerDone chan struct{} // closed when the reader has finished with the session
	loggedIn   atomic.Bool

	ss         *session
	writerDone chan struct{} // closed when the writer has exited; nil until it starts
	out        []byte        // frames queued for the writer
	inFlight   int           // bytes of the write in progress
	replay     int           // bytes of the answer to a resume not yet written; the first write carries them
	state      state
	abortErr   error // why the connection was aborted from elsewhere
}

func (c *conn) serve() {
	err := c.read()
	ss := c.ss
	if ss == nil { // not logged in: say why, if there is something to say
		c.flush()
	} else {
		ss.mu.Lock()
		if c.abortErr != nil {
			err = c.abortErr
		}
		if c.state == running && !errors.Is(err, errLogout) {
			// What is queued answers frames already taken, so it is sent
			// before the connection closes, unless there is too much of it.
			c.state = ending
			if errors.Is(err, errSlowConsumer) {
				c.state = aborted
			}
		}
		st, done := c.state, c.writerDone
		ss.mu.Unlock()
		if st == aborted {
			_ = c.nc.Close() // unblocks a write in progress
		}
		c.signal()
		<-done
	}
	_ = c.nc.Close()
	if ss != nil {
		ss.detach(c)
	}
	close(c.readerDone)

	s := c.srv
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	var id uint64
	if ss != nil {
		id = ss.id
	}
	s.opts.Logger.Info("oe connection closed", "session", id, "remote", c.nc.RemoteAddr().String(), "reason", err.Error())
	s.wg.Done()
}

// read decodes the frames that arrive, until an error or the end of the
// connection.
func (c *conn) read() error {
	buf := make([]byte, 64<<10)
	have := 0
	for {
		if err := c.nc.SetReadDeadline(time.Now().Add(c.srv.opts.Idle)); err != nil {
			return err
		}
		n, rerr := c.nc.Read(buf[have:])
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
			if err := c.handle(t, body); err != nil {
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

func (c *conn) handle(t Type, body []byte) error {
	if c.ss == nil {
		if t != TypeLogin {
			return fmt.Errorf("oe: %q frame before Login", byte(t))
		}
		return c.login(DecodeLogin(body))
	}
	switch t {
	case TypeNewOrder:
		return c.newOrder(DecodeNewOrder(body))
	case TypeCancel:
		return c.cancel(DecodeCancel(body))
	case TypePing:
		p := DecodePing(body)
		if ok, err := c.admit(p.ClientSeq, 0, false); !ok {
			return err
		}
		return queue(c, Pong(p))
	case TypeHeartbeat:
		return nil
	case TypeLogout:
		c.ss.mu.Lock()
		if c.state == running {
			c.ss.beginDrainLocked(drainLogout)
		}
		c.ss.mu.Unlock()
		return errLogout
	default:
		return fmt.Errorf("oe: %q frame not served", byte(t))
	}
}

// login opens a new session or resumes one, or queues why not.
func (c *conn) login(m Login) error {
	s := c.srv
	refuse := func(reason RejectReason, detail string) error {
		c.out = LoginRejected{Reason: reason}.Append(c.out)
		return fmt.Errorf("%w: login refused: %v (%s)", errEnd, reason, detail)
	}
	if m.Version != Version {
		return refuse(RejectBadVersion, fmt.Sprintf("version %d, want %d", m.Version, Version))
	}
	if m.Session == 0 {
		ss, err := s.newSession()
		if err != nil {
			return err
		}
		ss.mu.Lock()
		c.out = LoginAccepted{Session: ss.id, NextServerSeq: 1}.Append(c.out)
		for i, name := range s.opts.Symbols {
			c.out = Symbol{Index: uint8(i), Name: name}.Append(c.out)
		}
		c.bindLocked(ss)
		ss.mu.Unlock()
		c.start()
		return nil
	}

	s.mu.Lock()
	ss := s.sessions[m.Session]
	s.mu.Unlock()
	if ss == nil {
		return refuse(RejectUnknownSession, fmt.Sprintf("session %d is not held", m.Session))
	}
	ss.attach.Lock() // one Login at a time takes the session over
	defer ss.attach.Unlock()
	ss.mu.Lock()
	old := ss.cur
	reason, detail := c.resumeLocked(ss, m.NextServerSeq)
	ss.mu.Unlock()
	if reason != 0 {
		return refuse(reason, detail)
	}
	if old != nil {
		// The reader and writer of old are done with the session when this
		// closes. c takes no frame before that, so that the commands of the
		// session reach a symbol in the order sent.
		<-old.readerDone
	}
	c.start()
	s.opts.Logger.Info("oe session resumed", "session", ss.id, "next_server_seq", m.NextServerSeq)
	return nil
}

// resumeLocked attaches c to ss for a Login that expects server_seq k next:
// it closes the connection ss has, if any, and queues LoginAccepted and the
// frames held from k on. If it cannot, it returns why and leaves ss and its
// connection as they were. The caller holds ss.mu. The output of ss is c's
// from here on. A connection closed here takes no more frames, so
// lastClientSeq stays until c takes one, but its reader may still be
// passing one to the engine.
func (c *conn) resumeLocked(ss *session, k uint64) (RejectReason, string) {
	switch first := ss.heldFirstLocked(); {
	case ss.gone || ss.drain != drainNone || k == 0 || k > ss.nextServerSeq:
		return RejectUnknownSession, fmt.Sprintf("session %d is not held, or has not sent server_seq %d (next %d)", ss.id, k, ss.nextServerSeq)
	case k < first:
		return RejectReplayLost, fmt.Sprintf("session %d holds server_seq %d on, the client wants %d", ss.id, first, k)
	}
	if old := ss.cur; old != nil {
		old.abortLocked(errTakenOver)
	}
	c.out = LoginAccepted{Session: ss.id, NextServerSeq: k, LastClientSeq: ss.lastClientSeq}.Append(c.out)
	c.out = append(c.out, ss.held.from(k)...)
	c.replay = len(c.out)
	c.bindLocked(ss)
	return 0, ""
}

// bindLocked makes c the connection of ss.
func (c *conn) bindLocked(ss *session) {
	c.ss = ss
	ss.cur = c
	if ss.expiry != nil {
		ss.expiry()
		ss.expiry = nil
	}
}

// start starts c's writer, once it has a session and its first output.
func (c *conn) start() {
	c.writerDone = make(chan struct{})
	go c.write(c.writerDone)
	c.loggedIn.Store(true)
	c.signal()
}

// queue appends f to the connection's output and wakes the writer. A
// connection that is ending takes nothing more: EndOfSession is its last
// frame.
func queue[F interface{ Append([]byte) []byte }](c *conn, f F) error {
	ss := c.ss
	ss.mu.Lock()
	if c.state != running {
		ss.mu.Unlock()
		return errEnd
	}
	c.out = f.Append(c.out)
	over := c.overLocked()
	ss.mu.Unlock()
	if over {
		return errSlowConsumer
	}
	c.signal()
	return nil
}

// overLocked reports whether c has more output waiting than it may. The
// answer to a resume does not count: LoginAccepted and the frames replayed,
// which the client asked for and which are at most what the session holds.
func (c *conn) overLocked() bool {
	return len(c.out)+c.inFlight-c.replay > c.srv.opts.MaxUnsent
}

func (c *conn) signal() {
	select {
	case c.wake <- struct{}{}:
	default: // the writer has a wake-up pending
	}
}

// abortLocked closes the connection now, for the reason err. The caller
// holds c.ss.mu.
func (c *conn) abortLocked(err error) {
	if c.state == aborted {
		return
	}
	c.state, c.abortErr = aborted, err
	_ = c.nc.Close() // unblocks the reader and a write in progress
	c.ss.room.Broadcast()
}

// write sends what is queued, one write at a time, and a Heartbeat after
// Heartbeat without sending, until the connection ends.
func (c *conn) write(done chan<- struct{}) {
	defer close(done)
	ss, opts := c.ss, &c.srv.opts
	hb := time.NewTimer(opts.Heartbeat)
	defer hb.Stop()
	var buf []byte
	for {
		select {
		case <-c.wake:
		case <-hb.C:
			ss.mu.Lock()
			c.out = Heartbeat{}.Append(c.out)
			ss.mu.Unlock()
		}
		ss.mu.Lock()
		buf, c.out = c.out, buf[:0]
		c.inFlight = len(buf)
		st := c.state
		ss.mu.Unlock()
		if st == aborted {
			return
		}
		if len(buf) > 0 {
			err := c.writeNow(buf)
			ss.mu.Lock()
			c.inFlight, c.replay = 0, 0
			if err != nil {
				c.abortLocked(err)
			}
			ss.mu.Unlock()
			if err != nil {
				return // the reader sees the closed connection and ends
			}
			hb.Reset(opts.Heartbeat)
		}
		if st == ending {
			_ = c.nc.Close() // the reader may still be waiting for input
			return
		}
	}
}

func (c *conn) writeNow(b []byte) error {
	if err := c.nc.SetWriteDeadline(time.Now().Add(c.srv.opts.WriteTimeout)); err != nil {
		return err
	}
	_, err := c.nc.Write(b)
	return err
}

// flush writes what is queued from the reader's goroutine, before the
// connection has a writer.
func (c *conn) flush() {
	if len(c.out) > 0 {
		_ = c.writeNow(c.out) // the connection closes next either way
	}
}
