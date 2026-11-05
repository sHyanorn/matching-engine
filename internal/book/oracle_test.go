package book

import (
	"fmt"
	"slices"
)

// This file holds the test oracles that follow a book from its event stream
// alone, so the property and fuzz tests never take an expected value from
// the state of the book under test.

// restingModel is the resting book as the event stream describes it: every
// order that rested and has not yet been filled or canceled, with the
// quantities and the time priority (RestedSeq) the events imply.
type restingModel struct {
	orders map[OrderID]Order
}

func newRestingModel() *restingModel {
	return &restingModel{orders: make(map[OrderID]Order)}
}

// record applies one command's events. A Rested order takes its RestedSeq
// from the Rested event's own Seq and its Qty from the command's Accepted
// event; trades reduce makers, and a fully filled or canceled order leaves.
func (m *restingModel) record(evs []Event) error {
	var accepted Event
	for _, e := range evs {
		switch e.Kind {
		case EventAccepted:
			accepted = e
		case EventRested:
			if accepted.Kind != EventAccepted || accepted.OrderID != e.OrderID {
				return fmt.Errorf("order %d rested without being accepted", e.OrderID)
			}
			m.orders[e.OrderID] = Order{
				ID: e.OrderID, Side: e.Side, Type: Limit, TIF: GTC, Price: e.Price,
				Qty: accepted.Qty, Filled: accepted.Qty - e.Qty, Remaining: e.Qty,
				Timestamp: e.Timestamp, RestedSeq: e.Seq,
			}
		case EventTrade:
			o, ok := m.orders[e.MakerID]
			if !ok {
				return fmt.Errorf("trade with maker %d, which is not resting", e.MakerID)
			}
			o.Filled += e.Qty
			o.Remaining -= e.Qty
			if o.Remaining == 0 {
				delete(m.orders, e.MakerID)
			} else {
				m.orders[e.MakerID] = o
			}
		case EventCanceled:
			// A user cancel removes a resting order; an IOC remainder never
			// rested, so deleting it is a no-op.
			delete(m.orders, e.OrderID)
		case EventRejected:
		}
	}
	return nil
}

// check requires the book's resting orders to equal the model's exactly,
// RestedSeq and Timestamp included. It allocates nothing.
func (m *restingModel) check(b *Book) error {
	if len(b.orders) != len(m.orders) {
		return fmt.Errorf("book rests %d orders, the event stream %d", len(b.orders), len(m.orders))
	}
	for id, o := range b.orders {
		if want, ok := m.orders[id]; !ok || o.Order != want {
			return fmt.Errorf("resting order %d is %+v; from the event stream %+v (found %v)", id, o.Order, want, ok)
		}
	}
	return nil
}

// ledger accumulates, per order, the quantities the event stream moves, for
// the conservation check of the package documentation.
type ledger map[OrderID]*orderQty

// record applies one command's events. When an ID is accepted again (legal
// once its earlier order has left the book), the earlier order is checked
// first: it is gone, so its Remaining is 0 and Traded + Canceled must equal
// what was accepted.
func (lg ledger) record(evs []Event) error {
	for _, e := range evs {
		switch e.Kind {
		case EventAccepted:
			if q, ok := lg[e.OrderID]; ok && q.Traded+q.Canceled != q.Accepted {
				return fmt.Errorf("order %d (before reuse): accepted %d, traded %d + canceled %d", e.OrderID, q.Accepted, q.Traded, q.Canceled)
			}
			lg[e.OrderID] = &orderQty{Accepted: e.Qty}
		case EventTrade:
			lg[e.OrderID].Traded += e.Qty
			lg[e.MakerID].Traded += e.Qty
		case EventCanceled:
			lg[e.OrderID].Canceled += e.Qty
		case EventRejected, EventRested:
		}
	}
	return nil
}

// check verifies Accepted = Traded + Canceled + Remaining for the latest
// order under every ID, with Remaining read from the book.
func (lg ledger) check(b *Book) error {
	for id, q := range lg {
		var resting Qty
		if o, ok := b.Order(id); ok {
			resting = o.Remaining
		}
		if q.Traded+q.Canceled+resting != q.Accepted {
			return fmt.Errorf("order %d: accepted %d, traded %d + canceled %d + resting %d", id, q.Accepted, q.Traded, q.Canceled, resting)
		}
	}
	return nil
}

// sameBook reports the first difference between the states of two books:
// full depth on both sides, every resting order, LastSeq and order count.
func sameBook(a, b *Book) error {
	for _, s := range []Side{Buy, Sell} {
		if da, db := a.Depth(s, 1<<30, nil), b.Depth(s, 1<<30, nil); !slices.Equal(da, db) {
			return fmt.Errorf("%v depth differs:\n%v\n%v", s, da, db)
		}
	}
	for id, o := range a.orders {
		if r, ok := b.Order(id); !ok || r != o.Order {
			return fmt.Errorf("order %d is %+v in one book and %+v, %v in the other", id, o.Order, r, ok)
		}
	}
	if a.LastSeq() != b.LastSeq() || len(a.orders) != len(b.orders) {
		return fmt.Errorf("books end at Seq %d with %d orders and at Seq %d with %d", a.LastSeq(), len(a.orders), b.LastSeq(), len(b.orders))
	}
	return nil
}
