package oe

import (
	"context"
	"errors"

	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/engine"
)

// maxTaken is how many NewOrder and Cancel frames a session may have passed
// to the engine and not yet had answered. At that many its reader stops
// reading, so TCP holds the client back.
const maxTaken = 1024

var errNoEngine = errors.New("oe: no engine to take commands")

// newOrder passes a NewOrder to the engine, or answers it at once.
func (ss *session) newOrder(m NewOrder) error {
	opts := &ss.srv.opts
	if ok, err := ss.next(m.ClientSeq); !ok {
		return err
	}
	if opts.Engine == nil {
		return errNoEngine
	}
	if int(m.Symbol) >= len(opts.Symbols) {
		return ss.respond(m.ClientSeq, Done{Status: StatusUnknownSymbol})
	}
	if ok, err := ss.reserve(m.ClientSeq, 0); !ok {
		return err
	}
	id, err := opts.Engine.SendOrder(ss, m.ClientSeq, engine.NewOrder{
		Symbol: opts.Symbols[m.Symbol], Side: m.Side, Type: m.Type, TIF: m.TIF, Price: m.Price, Qty: m.Qty,
	})
	if err != nil {
		return ss.refuse(m.ClientSeq, book.CommandNewOrder, id, err)
	}
	return nil
}

// cancel passes a Cancel to the engine, or answers it at once.
func (ss *session) cancel(m Cancel) error {
	opts := &ss.srv.opts
	if ok, err := ss.next(m.ClientSeq); !ok {
		return err
	}
	if opts.Engine == nil {
		return errNoEngine
	}
	if ok, err := ss.reserve(m.ClientSeq, m.OrderID); !ok {
		return err
	}
	if err := opts.Engine.SendCancel(ss, m.ClientSeq, m.OrderID); err != nil {
		return ss.refuse(m.ClientSeq, book.CommandCancel, m.OrderID, err)
	}
	return nil
}

// reserve takes one of the session's maxTaken places for a command, waiting
// while none is free. It reports false if the command is not to go on: it
// was answered with shutting_down, or the session is over. id is the order
// the command is about, if it names one.
func (ss *session) reserve(seq uint64, id book.OrderID) (bool, error) {
	ss.mu.Lock()
	for ss.taken >= maxTaken && ss.state == running && ss.drain == drainNone {
		ss.room.Wait()
	}
	switch {
	case ss.state != running:
		ss.mu.Unlock()
		return false, errEnd
	case ss.drain != drainNone:
		ss.appendDone(seq, Done{Status: StatusShuttingDown, OrderID: id})
		ss.mu.Unlock()
		ss.signal()
		return false, nil
	}
	ss.taken++
	ss.mu.Unlock()
	return true, nil
}

// respond answers a command at once, without a place taken.
func (ss *session) respond(seq uint64, d Done) error {
	ss.mu.Lock()
	if ss.state != running {
		ss.mu.Unlock()
		return errEnd
	}
	ss.appendDone(seq, d)
	ss.mu.Unlock()
	ss.signal()
	return nil
}

// refuse answers a command the engine would not take, and gives back its
// place.
func (ss *session) refuse(seq uint64, kind book.CommandKind, id book.OrderID, err error) error {
	d := refusalDone(kind, id, err)
	ss.mu.Lock()
	if ss.state == running {
		ss.appendDone(seq, d)
	}
	ss.answeredLocked()
	ss.mu.Unlock()
	ss.signal()
	return nil
}

// answeredLocked gives back the place of a command that has been answered.
func (ss *session) answeredLocked() {
	ss.taken--
	ss.room.Signal()
	if ss.drain != drainNone && ss.taken == 0 {
		ss.endLocked()
	}
}

// appendDone queues the Done frame of a command that has no events.
func (ss *session) appendDone(seq uint64, d Done) {
	d.ServerSeq, d.ClientSeq = ss.nextServerSeq, seq
	ss.nextServerSeq++
	ss.out = d.Append(ss.out)
	ss.checkUnsentLocked()
}

// checkUnsentLocked ends a session that has more output waiting than it may.
func (ss *session) checkUnsentLocked() {
	if len(ss.out)+ss.inFlight > ss.srv.opts.MaxUnsent {
		ss.abortLocked(errSlowConsumer)
	}
}

// Reply queues the reply to a command: its events, then its Done. It runs
// on the goroutine of the command's symbol and does not wait for the
// writer or the client; a session that falls too far behind is closed.
func (ss *session) Reply(seq uint64, r engine.Reply) {
	d, withEvents := replyDone(r)
	ss.mu.Lock()
	if ss.state == running {
		if withEvents {
			for _, e := range r.Events {
				ss.out = Event{ServerSeq: ss.nextServerSeq, ClientSeq: seq, Event: e}.Append(ss.out)
				ss.nextServerSeq++
			}
		}
		ss.appendDone(seq, d)
	}
	ss.answeredLocked()
	ss.mu.Unlock()
}

// Flush wakes the writer, once the engine has queued all it has for now.
func (ss *session) Flush() { ss.signal() }

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
