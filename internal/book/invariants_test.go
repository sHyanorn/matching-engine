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
	order := func(b *Book, id OrderID) *orderNode { return b.orders.get(id) }
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
			b.orders.del(3)
		}},
		{"side sell in a buy level", func(b *Book) { order(b, 1).Side = Sell }},
		{"price 100 in the level at 99", func(b *Book) { order(b, 1).Price = 100 }},
		{"a limit ioc order is resting", func(b *Book) { order(b, 1).TIF = IOC }},
		{"a market gtc order is resting", func(b *Book) { order(b, 1).Type = Market }},
		{"Remaining 0 is not positive", func(b *Book) { order(b, 1).Remaining = 0 }},
		{"Filled 1 + Remaining 5 != Qty 5", func(b *Book) { order(b, 1).Filled = 1 }},
		{"Filled -1 + Remaining 6 != Qty 5", func(b *Book) {
			// Keep the level total in step, so the order check fires first.
			o := order(b, 1)
			o.Filled, o.Remaining = -1, o.Qty+1
			o.level.totalQty++
		}},
		{"time priority broken", func(b *Book) {
			o1, o2 := order(b, 1), order(b, 2)
			o1.RestedSeq, o2.RestedSeq = o2.RestedSeq, o1.RestedSeq
		}},
		{"time priority broken, RestedSeq 2 after 2", func(b *Book) { order(b, 2).RestedSeq = order(b, 1).RestedSeq }},
		{"RestedSeq 0 outside 1..LastSeq", func(b *Book) { order(b, 1).RestedSeq = 0 }},
		{"outside 1..LastSeq", func(b *Book) { order(b, 6).RestedSeq = b.seq + 1 }},
		{"sell levels: size is -1, tree has 2 nodes", func(b *Book) { b.asks.size = -1 }},
		{"the order index does not point at the queued order", func(b *Book) {
			i, _ := b.orders.cur.find(2)
			b.orders.cur.slots[i].node = order(b, 1)
		}},
		{"order index holds 7 orders, the queues 6", func(b *Book) { b.orders.put(999, order(b, 1)) }},
		{"order index: counts 7 entries, holds 6", func(b *Book) { b.orders.n++ }},
		{"holds a tombstone for 3 in the current table", func(b *Book) {
			// A tombstone left in the current table: order 3 is gone from
			// the index, though still queued.
			i, _ := b.orders.cur.find(3)
			b.orders.cur.slots[i].node = nil
		}},
		{"free list: node 0 holds order 6", func(b *Book) {
			// A resting order on the free list. Order 6 is alone at 103,
			// so its next link is nil and its queue stays intact.
			b.free = order(b, 6)
		}},
		{"free list: node 0 links back into the list", func(b *Book) {
			// A node released twice links to itself.
			o := order(b, 6)
			b.Apply(cancel(6), nil)
			b.release(o)
		}},
		{"free list: node 2 links back into the list", func(b *Book) {
			// Released twice with another node released in between.
			o := order(b, 6)
			b.Apply(cancel(6), nil)
			b.Apply(cancel(3), nil)
			b.release(o)
		}},
		{"buy level 99: order 0: prev link", func(b *Book) {
			// Released while still queued and indexed: order 1 still
			// links to the reset node.
			b.release(order(b, 2))
		}},
		{"order index holds 6 orders, the queues 5", func(b *Book) {
			// Released after leaving its queue, but still indexed.
			o := order(b, 2)
			o.level.unlinkOrder(o)
			b.release(o)
		}},
		{"sell levels: root is red", func(b *Book) { b.asks.root.red = true }},
		{"book is crossed: best bid 200, best ask 101", func(b *Book) {
			// A well-formed bid level above the best ask.
			l := b.bids.insert(200)
			o := &orderNode{Order: Order{ID: 7, Side: Buy, Type: Limit, TIF: GTC, Price: 200, Qty: 1, Remaining: 1, RestedSeq: b.seq}}
			l.pushBack(o)
			b.orders.put(7, o)
		}},
		{"book is crossed: best bid 101, best ask 101", func(b *Book) {
			// A well-formed bid level exactly at the best ask: SPEC §7 wants
			// the best bid strictly below it.
			l := b.bids.insert(101)
			o := &orderNode{Order: Order{ID: 7, Side: Buy, Type: Limit, TIF: GTC, Price: 101, Qty: 1, Remaining: 1, RestedSeq: b.seq}}
			l.pushBack(o)
			b.orders.put(7, o)
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

// TestCheckInvariantsCatchesSwappedSides builds a book whose trees order
// each side the wrong way. Each tree is valid under its own comparator, so
// only the raw price-order check can see that the best bid is really the
// worst, and that the book below is crossed (bid 101 against ask 100).
func TestCheckInvariantsCatchesSwappedSides(t *testing.T) {
	b := &Book{bids: newLevelTree(Sell), asks: newLevelTree(Buy), orders: newOrderIndex()}
	for _, c := range []Command{limit(1, Buy, 99, 5), limit(2, Buy, 101, 5), limit(3, Sell, 100, 5)} {
		b.Apply(c, nil)
	}
	const want = "buy levels: 101 comes after 99"
	if err := b.CheckInvariants(); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("CheckInvariants = %v, want an error containing %q", err, want)
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
