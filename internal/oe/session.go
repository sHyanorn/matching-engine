package oe

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/engine"
)

// maxTaken is how many NewOrder and Cancel frames a session may have passed
// to the engine and not yet had answered. At that many its reader stops
// reading, so TCP holds the client back.
const maxTaken = 1024

var errNoEngine = errors.New("oe: no engine to take commands")

// session is the state of one client, which outlives its connections: the
// client_seq it has taken, the server_seq it has sent, the commands the
// engine has not answered and the last output it sent, held for a resume.
// It is the engine's Sink: replies are queued, on the goroutine of the
// symbol, to the connection it has now, if any.
type session struct {
	srv    *Server
	id     uint64
	attach sync.Mutex // one Login at a time takes the session over

	mu            sync.Mutex
	room          sync.Cond // on mu: where a reader without a core waits for wakeReaderLocked
	cur           *conn     // the connection it has, nil between two
	lastClientSeq uint64    // owned by the reader of cur; it hands the session over by ending
	nextServerSeq uint64    // of the next Event or Done
	taken         int       // commands passed to the engine and not yet answered
	drain         drain
	finished      bool        // every command is answered after a Logout or at shutdown
	gone          bool        // dropped: it can no longer be resumed
	held          held        // its last Events and Dones
	expiry        func() bool // stops the timer that drops it after its connection ended
	scratch       [96]byte    // a frame being encoded

	gen atomic.Uint64 // counts the calls of wakeReaderLocked; a spinning reader polls it, holding no lock
}

// newSession opens a session with a random number other than 0. Numbers
// counted from 1 would start again after a restart, and a Login that named
// one from the earlier process could then name another client's session.
func (s *Server) newSession() (*session, error) {
	ss := &session{srv: s, nextServerSeq: 1, held: held{limit: s.opts.Held}}
	ss.room.L = &ss.mu
	for {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, fmt.Errorf("oe: session number: %w", err)
		}
		ss.id = le.Uint64(b[:])
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return nil, fmt.Errorf("%w: the server is shutting down", errEnd)
		}
		if _, taken := s.sessions[ss.id]; ss.id != 0 && !taken {
			s.sessions[ss.id] = ss
			s.mu.Unlock()
			return ss, nil
		}
		s.mu.Unlock()
	}
}

// heldFirstLocked is the lowest server_seq a resume can ask for.
func (ss *session) heldFirstLocked() uint64 {
	if ss.held.empty() {
		return ss.nextServerSeq
	}
	return ss.held.first
}

// dropLocked ends the session for good, apart from taking it out of the
// server's table.
func (ss *session) dropLocked() {
	ss.gone = true
	if ss.expiry != nil {
		ss.expiry()
		ss.expiry = nil
	}
	ss.held.b = nil
}

// detach is the end of c's life as the session's connection. The session is
// dropped if it is ending, and otherwise kept for a resume.
func (ss *session) detach(c *conn) {
	ss.mu.Lock()
	if ss.cur != c { // a newer connection has taken the session over
		ss.mu.Unlock()
		return
	}
	ss.cur = nil
	remove := ss.finished || ss.drain != drainNone
	if remove {
		ss.dropLocked()
	} else {
		ss.expiry = ss.srv.opts.afterFunc(ss.srv.opts.ResumeWindow, func() { ss.srv.expire(ss) })
	}
	ss.mu.Unlock()
	if remove {
		ss.srv.remove(ss)
	}
}

// beginDrainLocked makes the session answer the commands it has taken and
// take no more, and end when none is left: with EndOfSession if the server
// is shutting down.
func (ss *session) beginDrainLocked(d drain) {
	if ss.drain == drainNone {
		ss.drain = d
	}
	ss.wakeReaderLocked() // a reader waiting for room answers shutting_down
	if ss.taken == 0 {
		ss.endLocked()
	}
}

// endLocked ends a session that has answered every command it took.
func (ss *session) endLocked() {
	ss.finished = true
	c := ss.cur
	if c == nil || c.state != running {
		return
	}
	if ss.drain == drainShutdown {
		c.out = EndOfSession{}.Append(c.out)
	}
	c.state = ending
	c.signal()
}

// admit takes the frame with client_seq seq. It reports false if the frame
// is to go no further: it is a resend, which is dropped; it was answered
// with shutting_down; or the connection is over. A number that skips one
// is an error. A command, of kind kind, waits for one of the session's
// maxTaken places first; symbol is the index a NewOrder names and id the
// order a Cancel names. kind is 0 for a frame that is not a command. The
// number is taken in the same step as the place, so that a command is
// either passed to the engine or left for the client to send again.
func (c *conn) admit(seq uint64, kind book.CommandKind, symbol uint8, id book.OrderID) (bool, error) {
	command := kind != 0
	ss := c.ss
	ss.mu.Lock()
	defer ss.mu.Unlock()
	for {
		switch {
		case c.state != running:
			return false, errEnd
		case seq <= ss.lastClientSeq:
			return false, nil
		case seq != ss.lastClientSeq+1:
			return false, fmt.Errorf("oe: client_seq %d after %d, want %d", seq, ss.lastClientSeq, ss.lastClientSeq+1)
		case !command || ss.drain != drainNone || ss.taken < maxTaken:
			ss.lastClientSeq = seq
			if command && ss.drain != drainNone {
				c.srv.countRefused(kind, symbol, id, engine.ErrClosed) // the error REST counts at shutdown
				ss.emitDoneLocked(seq, Done{Status: StatusShuttingDown, OrderID: id})
				c.signal()
				return false, nil
			}
			if command {
				ss.taken++
			}
			return true, nil
		}
		c.waitRoom()
	}
}

// waitRoom releases ss.mu, waits for a change that can end admit's wait (a
// place given back, a drain beginning, the connection ending), takes ss.mu
// again and returns; admit rechecks. A spinning reader polls ss.gen instead
// of blocking, and holds no lock while it does: the symbol's goroutine needs
// ss.mu to give a place back.
func (c *conn) waitRoom() {
	ss := c.ss
	if c.lease == nil {
		ss.room.Wait()
		return
	}
	seen := ss.gen.Load() // under ss.mu, so no change to what admit checked goes unseen
	ss.mu.Unlock()
	for ss.gen.Load() == seen {
	}
	ss.mu.Lock()
}

// wakeReaderLocked wakes the reader that waits in waitRoom, however it waits.
// Every change that can end the wait calls it with ss.mu held.
func (ss *session) wakeReaderLocked() {
	ss.gen.Add(1)
	ss.room.Broadcast()
}

// newOrder passes a NewOrder to the engine, or answers it at once.
func (c *conn) newOrder(m NewOrder) error {
	opts := &c.srv.opts
	if opts.Engine == nil {
		return errNoEngine
	}
	if ok, err := c.admit(m.ClientSeq, book.CommandNewOrder, m.Symbol, 0); !ok {
		return err
	}
	if int(m.Symbol) >= len(opts.Symbols) {
		return c.refuse(m.ClientSeq, book.CommandNewOrder, m.Symbol, 0, engine.ErrUnknownSymbol)
	}
	id, err := opts.Engine.SendOrder(c.ss, m.ClientSeq, engine.NewOrder{
		Symbol: opts.Symbols[m.Symbol], Side: m.Side, Type: m.Type, TIF: m.TIF, Price: m.Price, Qty: m.Qty,
	})
	if err != nil {
		return c.refuse(m.ClientSeq, book.CommandNewOrder, m.Symbol, id, err)
	}
	return nil
}

// cancel passes a Cancel to the engine, or answers it at once.
func (c *conn) cancel(m Cancel) error {
	opts := &c.srv.opts
	if opts.Engine == nil {
		return errNoEngine
	}
	if ok, err := c.admit(m.ClientSeq, book.CommandCancel, 0, m.OrderID); !ok {
		return err
	}
	if err := opts.Engine.SendCancel(c.ss, m.ClientSeq, m.OrderID); err != nil {
		return c.refuse(m.ClientSeq, book.CommandCancel, 0, m.OrderID, err)
	}
	return nil
}

// refuse counts and answers a command that was not queued, for the reason
// err, and gives back its place. symbol is the index a NewOrder names.
func (c *conn) refuse(seq uint64, kind book.CommandKind, symbol uint8, id book.OrderID, err error) error {
	c.srv.countRefused(kind, symbol, id, err)
	d := refusalDone(kind, id, err)
	ss := c.ss
	ss.mu.Lock()
	ss.emitDoneLocked(seq, d)
	ss.answeredLocked()
	ss.mu.Unlock()
	ss.Flush()
	return nil
}

// countRefused reports a command that is answered without reaching the
// engine's queue, for the reason err. A NewOrder for a symbol that does not
// exist is not counted, as over REST.
func (s *Server) countRefused(kind book.CommandKind, symbol uint8, id book.OrderID, err error) {
	n := s.opts.Counts
	switch {
	case n == nil:
	case kind == book.CommandCancel:
		n.CountCancel(id, nil, engine.OrderStatus{}, err)
	case int(symbol) < len(s.opts.Symbols):
		n.CountOrder(s.opts.Symbols[symbol], nil, err)
	}
}

// countReply reports a command the engine has answered, whether or not its
// session still has a connection or still exists.
func (s *Server) countReply(r engine.Reply) {
	n := s.opts.Counts
	switch {
	case n == nil:
	case r.Kind == book.CommandCancel:
		n.CountCancel(r.OrderID, r.Events, r.Status, r.Err)
	default:
		if symbol, ok := s.opts.Engine.SymbolOf(r.OrderID); ok {
			n.CountOrder(symbol, r.Events, r.Err)
		}
	}
}

// answeredLocked gives back the place of a command that has been answered.
func (ss *session) answeredLocked() {
	ss.taken--
	ss.wakeReaderLocked()
	if ss.drain != drainNone && ss.taken == 0 {
		ss.endLocked()
	}
}

// emitDoneLocked sends the Done frame of a command that has no events.
func (ss *session) emitDoneLocked(seq uint64, d Done) {
	d.ServerSeq, d.ClientSeq = ss.nextServerSeq, seq
	ss.emitLocked(d.Append(ss.scratch[:0]))
}

// emitLocked numbers the frame fr, which is one of ss.scratch and has the
// server_seq the session sends next, holds it for a resume and queues it
// on the connection, if there is one. A connection with more output
// waiting than it may is closed.
func (ss *session) emitLocked(fr []byte) {
	if ss.gone {
		return
	}
	seq := le.Uint64(fr[HeaderSize:])
	ss.nextServerSeq = seq + 1
	ss.held.add(fr, seq)
	if c := ss.cur; c != nil && c.state == running {
		c.out = append(c.out, fr...)
		if c.overLocked() {
			c.abortLocked(errSlowConsumer)
		}
	}
}

// Reply queues the reply to a command: its events, then its Done. It runs
// on the goroutine of the command's symbol and does not wait for the
// writer or the client; a connection that falls too far behind is closed.
func (ss *session) Reply(seq uint64, r engine.Reply) {
	ss.srv.countReply(r)
	d, withEvents := replyDone(r)
	ss.mu.Lock()
	if withEvents {
		for _, e := range r.Events {
			ss.emitLocked(Event{ServerSeq: ss.nextServerSeq, ClientSeq: seq, Event: e}.Append(ss.scratch[:0]))
		}
	}
	ss.emitDoneLocked(seq, d)
	ss.answeredLocked()
	ss.mu.Unlock()
}

// Flush wakes the writer, once the engine has queued all it has for now.
func (ss *session) Flush() {
	ss.mu.Lock()
	c := ss.cur
	ss.mu.Unlock()
	if c != nil {
		c.signal()
	}
}

// replyDone is the Done for a command the engine has answered, and whether
// the command's events are sent with it.
func replyDone(r engine.Reply) (Done, bool) {
	d := Done{OrderID: r.OrderID}
	switch {
	case len(r.Events) == 0 && r.Err == nil: // the engine always answers with events
		d.Status = StatusInternal
		return d, false
	case len(r.Events) == 0 && errors.Is(r.Err, engine.ErrLogFailed):
		d.Status = StatusLogFailed
		return d, false
	case r.Err != nil && !errors.Is(r.Err, engine.ErrLogFailed):
		d.Status = StatusInternal
		return d, false
	case r.Err != nil: // applied, and the log failed
		d.Status = StatusLogFailedApplied
	case r.Kind == book.CommandCancel && r.Events[0].Kind == book.EventRejected:
		// A cancel of an order that is not resting: REST answers it with a
		// status and no events.
		d.Status = StatusUnknownOrder
		if r.Status.ID != 0 && r.Status.Finished() {
			d.Status = StatusOrderFinished
		}
		return d, false
	default:
		d.Status = StatusOK
	}
	if r.Status.ID != 0 {
		d.Reason, d.State = r.Status.Reason, r.Status.State
	}
	return d, true
}

// refusalDone is the Done for a command that failed before it was queued,
// with err from the engine. id is the order the command is about, 0 if it
// has none yet.
func refusalDone(kind book.CommandKind, id book.OrderID, err error) Done {
	d := Done{OrderID: id}
	var inv *engine.InvalidError
	switch {
	case errors.As(err, &inv):
		// The engine sets a new order's ID itself, so for one these reasons
		// are a server bug (D009); a cancel names its ID, which may be 0.
		if inv.Reason == book.ReasonInvalidCommand || (inv.Reason == book.ReasonInvalidOrderID && kind == book.CommandNewOrder) {
			d.Status = StatusInternal
		} else {
			d.Status, d.Reason = StatusInvalid, inv.Reason
		}
	case errors.Is(err, engine.ErrUnknownSymbol):
		d.Status = StatusUnknownSymbol
	case errors.Is(err, engine.ErrUnknownOrder):
		d.Status = StatusUnknownOrder
	case errors.Is(err, engine.ErrBusy):
		d.Status = StatusBusy
	case errors.Is(err, engine.ErrClosed), errors.Is(err, context.Canceled):
		d.Status = StatusShuttingDown
	default:
		d.Status = StatusInternal
	}
	return d
}
