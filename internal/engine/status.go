package engine

import "github.com/sHyanorn/matching-engine/internal/book"

// OrderState is where an order is in its life.
type OrderState uint8

// The order states.
const (
	StateOpen     OrderState = iota + 1 // resting in the book, possibly partly filled
	StateFilled                         // fully filled
	StateCanceled                       // canceled by its owner, or the unfilled part of an IOC or market order
	StateRejected                       // refused by the book (for example fok_unfillable)
)

var stateNames = [...]string{StateOpen: "open", StateFilled: "filled", StateCanceled: "canceled", StateRejected: "rejected"}

func (s OrderState) String() string {
	if int(s) < len(stateNames) && stateNames[s] != "" {
		return stateNames[s]
	}
	return "unknown"
}

// OrderStatus is what the engine knows about one order: the book's view
// of it (for a finished order, as it was when it finished, with Remaining
// 0), its symbol and client_id, its state and, for a canceled or rejected
// order, why.
type OrderStatus struct {
	book.Order
	Symbol   string
	ClientID string
	State    OrderState
	Reason   book.Reason
}

// Finished reports whether the order can no longer trade.
func (s OrderStatus) Finished() bool { return s.State != StateOpen }

const defaultHistory = 100_000

// orderTracker follows every order of one market from its events: the
// resting ones, and the most recently finished ones in a ring buffer
// (D022). It is owned by the market's goroutine and rebuilt by replay.
type orderTracker struct {
	symbol string
	open   map[book.OrderID]*OrderStatus
	ring   []OrderStatus // finished orders, oldest overwritten first
	next   int           // ring index the next finished order goes to
	index  map[book.OrderID]int
}

func newOrderTracker(symbol string, history int) *orderTracker {
	return &orderTracker{
		symbol: symbol,
		open:   make(map[book.OrderID]*OrderStatus),
		ring:   make([]OrderStatus, 0, history),
		index:  make(map[book.OrderID]int, history),
	}
}

// lookup returns the status of order id, if it is open or remembered.
func (t *orderTracker) lookup(id book.OrderID) (OrderStatus, bool) {
	if s, ok := t.open[id]; ok {
		return *s, true
	}
	if i, ok := t.index[id]; ok {
		return t.ring[i], true
	}
	return OrderStatus{}, false
}

// track updates the statuses for one applied command and its events, and
// returns the status of the order the command concerns: the new order, or
// the order a cancel named (whether or not it was still resting).
func (t *orderTracker) track(c book.Command, clientID string, evs []book.Event) (OrderStatus, bool) {
	var taker *OrderStatus
	if c.Kind == book.CommandNewOrder {
		taker = &OrderStatus{
			Order: book.Order{
				ID: c.OrderID, Side: c.Side, Type: c.Type, TIF: c.TIF,
				Price: c.Price, Qty: c.Qty, Timestamp: c.Timestamp,
			},
			Symbol: t.symbol, ClientID: clientID,
		}
	}
	for _, e := range evs {
		switch e.Kind {
		case book.EventTrade:
			taker.Filled += e.Qty
			if m := t.open[e.MakerID]; m != nil {
				m.Filled += e.Qty
				m.Remaining -= e.Qty
				if m.Remaining == 0 {
					m.State = StateFilled
					t.finish(m)
				}
			}
		case book.EventRested:
			taker.Remaining, taker.RestedSeq, taker.State = e.Qty, e.Seq, StateOpen
			t.open[taker.ID] = taker
		case book.EventCanceled:
			s := taker
			if c.Kind == book.CommandCancel {
				s = t.open[e.OrderID]
			}
			if s != nil {
				s.State, s.Reason = StateCanceled, e.Reason
				t.finish(s)
			}
		case book.EventRejected:
			// A duplicate shares its ID with a resting order, whose status
			// must stay; the engine never issues one.
			if taker != nil && e.Reason != book.ReasonDuplicateOrderID {
				taker.State, taker.Reason = StateRejected, e.Reason
				t.finish(taker)
			}
			return t.lookup(c.OrderID)
		case book.EventAccepted:
		}
	}
	if taker != nil && taker.State == 0 { // traded in full, never rested
		taker.State = StateFilled
		t.finish(taker)
	}
	return t.lookup(c.OrderID)
}

// finish moves s out of the open orders into the ring of finished ones.
func (t *orderTracker) finish(s *OrderStatus) {
	delete(t.open, s.ID)
	s.Remaining = 0
	if cap(t.ring) == 0 {
		return
	}
	if len(t.ring) < cap(t.ring) {
		t.ring = append(t.ring, *s)
		t.index[s.ID] = len(t.ring) - 1
		return
	}
	delete(t.index, t.ring[t.next].ID)
	t.ring[t.next] = *s
	t.index[s.ID] = t.next
	t.next = (t.next + 1) % cap(t.ring)
}
