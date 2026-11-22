package engine

import (
	"context"

	"github.com/sHyanorn/matching-engine/internal/book"
)

// Reply is the outcome of a command sent with SendOrder or SendCancel.
// OrderID, Status and Err are as for [Result] and its error; Err is nil or
// wraps ErrLogFailed.
type Reply struct {
	Kind    book.CommandKind
	OrderID book.OrderID
	// Events is the book's own buffer: it is valid only until Sink.Reply
	// returns, and the sink must not keep or modify it.
	Events []book.Event
	Status OrderStatus
	Err    error
}

// A Sink receives the replies to the commands sent to it, on the goroutine
// of the commands' symbol, so neither method may block. Sinks are compared
// with ==, so use a pointer type.
type Sink interface {
	// Reply is called once for each command, in the order the symbol applied
	// them, and tag is the one given to the send.
	Reply(tag uint64, r Reply)
	// Flush is called after the symbol's goroutine has finished a batch in
	// which it called Reply, once for each run of consecutive Replies to the
	// sink within the batch.
	Flush()
}

// prepareOrder checks o and takes its ID. cmd.OrderID is that ID, also when
// err is not nil, and 0 if none was taken.
func (e *Engine) prepareOrder(ctx context.Context, o NewOrder) (m *market, cmd book.Command, err error) {
	m, ok := e.bySymbol[o.Symbol]
	if !ok {
		return nil, cmd, ErrUnknownSymbol
	}
	if len(o.ClientID) > MaxClientIDLen {
		return nil, cmd, ErrClientIDLen
	}
	if err := ctx.Err(); err != nil {
		return nil, cmd, err // nothing submitted, so no ID is taken
	}
	counter, err := e.nextID()
	if err != nil {
		return nil, cmd, err
	}
	cmd = book.Command{
		Kind: book.CommandNewOrder, OrderID: makeOrderID(counter, m.index),
		Side: o.Side, Type: o.Type, TIF: o.TIF, Price: o.Price, Qty: o.Qty,
	}
	if r := cmd.Validate(); r != book.ReasonNone {
		return nil, cmd, &InvalidError{Reason: r}
	}
	return m, cmd, nil
}

// prepareCancel checks a cancel of order id and finds its market.
func (e *Engine) prepareCancel(id book.OrderID) (*market, book.Command, error) {
	cmd := book.Command{Kind: book.CommandCancel, OrderID: id}
	if r := cmd.Validate(); r != book.ReasonNone {
		return nil, cmd, &InvalidError{Reason: r}
	}
	m, ok := e.marketOf(id)
	if !ok {
		return nil, cmd, ErrUnknownOrder
	}
	return m, cmd, nil
}

// SendOrder is PlaceOrder without the wait: it validates the order, takes
// its ID and queues it, and returns the ID. The reply, with tag, goes to s
// once the order has been applied. An error returned here means nothing was
// queued and s gets no reply; the ID is the one taken, 0 if none was (see
// PlaceOrder for the errors).
func (e *Engine) SendOrder(s Sink, tag uint64, o NewOrder) (book.OrderID, error) {
	m, cmd, err := e.prepareOrder(context.Background(), o)
	if err != nil {
		return cmd.OrderID, err
	}
	return cmd.OrderID, e.enqueue(m, request{kind: reqCommand, cmd: cmd, clientID: o.ClientID, sink: s, tag: tag})
}

// SendCancel is CancelOrder without the wait, as SendOrder is for
// PlaceOrder.
func (e *Engine) SendCancel(s Sink, tag uint64, id book.OrderID) error {
	m, cmd, err := e.prepareCancel(id)
	if err != nil {
		return err
	}
	return e.enqueue(m, request{kind: reqCommand, cmd: cmd, sink: s, tag: tag})
}

// enqueue puts r on m's queue without waiting. It holds the read lock while
// sending, so Close cannot close the queue under it.
func (e *Engine) enqueue(m *market, r request) error {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.closed {
		return ErrClosed
	}
	select {
	case m.in <- r:
		return nil
	default:
		return ErrBusy
	}
}
