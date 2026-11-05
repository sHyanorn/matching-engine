package book

import (
	"strings"
	"testing"
)

// TestCheckInvariantsCatchesCorruption makes sure CheckInvariants is not
// vacuous: each deliberate corruption of a valid book must be reported, with
// the right complaint.
func TestCheckInvariantsCatchesCorruption(t *testing.T) {
	// fresh builds a valid book:
	//
	//	asks  103: 6 (1)
	//	      101: 4 (2), 5 (6)
	//	bids   99: 1 (5), 2 (3)
	//	       98: 3 (4)
	fresh := func() *Book {
		b := New()
		for _, c := range []Command{
			limit(1, Buy, 99, 5), limit(2, Buy, 99, 3), limit(3, Buy, 98, 4),
			limit(4, Sell, 101, 2), limit(5, Sell, 101, 6), limit(6, Sell, 103, 1),
		} {
			b.Apply(c, nil)
		}
		if err := b.CheckInvariants(); err != nil {
			t.Fatalf("fresh book: %v", err)
		}
		return b
	}
	order := func(b *Book, id OrderID) *orderNode { return b.orders[id] }
	level := func(b *Book, s Side, p Price) *levelNode { return b.side(s).find(p) }

	cases := []struct {
		want    string
		corrupt func(b *Book)
	}{
		{"total quantity", func(b *Book) { level(b, Buy, 99).totalQty++ }},
		{"order count", func(b *Book) { level(b, Buy, 99).count++ }},
		{"tail is not the last order", func(b *Book) { level(b, Buy, 99).tail = order(b, 1) }},
		{"prev link", func(b *Book) { order(b, 2).prev = nil }},
		{"level link", func(b *Book) { order(b, 2).level = level(b, Buy, 98) }},
		{"empty level", func(b *Book) {
			o := order(b, 3)
			o.level.unlinkOrder(o)
			delete(b.orders, 3)
		}},
		{"side sell in a buy level", func(b *Book) { order(b, 1).Side = Sell }},
		{"price 100 in the level at 99", func(b *Book) { order(b, 1).Price = 100 }},
		{"only Limit GTC orders rest", func(b *Book) { order(b, 1).TIF = IOC }},
		{"Remaining 0 is not positive", func(b *Book) { order(b, 1).Remaining = 0 }},
		{"Filled 1 + Remaining 5 != Qty 5", func(b *Book) { order(b, 1).Filled = 1 }},
		{"time priority broken", func(b *Book) {
			o1, o2 := order(b, 1), order(b, 2)
			o1.RestedSeq, o2.RestedSeq = o2.RestedSeq, o1.RestedSeq
		}},
		{"outside 1..LastSeq", func(b *Book) { order(b, 6).RestedSeq = b.seq + 1 }},
		{"the ID map does not point at the queued order", func(b *Book) { b.orders[2] = order(b, 1) }},
		{"ID map holds 7 orders, the queues 6", func(b *Book) { b.orders[999] = order(b, 1) }},
		{"sell levels: root is red", func(b *Book) { b.asks.root.red = true }},
		{"book is crossed: best bid 200, best ask 101", func(b *Book) {
			// A well-formed bid level above the best ask.
			l := b.bids.insert(200)
			o := &orderNode{Order: Order{ID: 7, Side: Buy, Type: Limit, TIF: GTC, Price: 200, Qty: 1, Remaining: 1, RestedSeq: b.seq}}
			l.pushBack(o)
			b.orders[7] = o
		}},
	}
	for _, c := range cases {
		b := fresh()
		c.corrupt(b)
		err := b.CheckInvariants()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("corruption %q: CheckInvariants returned %v", c.want, err)
		}
	}
}

// TestCheckInvariantsOnEmptyBook checks the trivial case and that the
// method is safe to call on a book that has seen only rejected commands.
func TestCheckInvariantsOnEmptyBook(t *testing.T) {
	b := New()
	if err := b.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
	b.Apply(cancel(1), nil)
	b.Apply(Command{}, nil)
	if err := b.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
}
