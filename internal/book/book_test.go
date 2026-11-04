package book

import (
	"math/rand/v2"
	"slices"
	"testing"
)

// checkBook fails t unless every structural invariant of b holds: both trees
// are valid red-black trees, every level's queue is consistent with its
// totals, every resting order is a Limit GTC order at its level's price on
// its tree's side with Filled + Remaining = Qty and Remaining > 0, time
// priority (RestedSeq) increases from head to tail, the ID map and the queues
// hold exactly the same orders, and the book is not crossed. It is O(N) in
// the number of resting orders.
func checkBook(t *testing.T, b *Book) {
	t.Helper()
	seen := 0
	for _, s := range []Side{Buy, Sell} {
		tree := b.side(s)
		if err := checkTree(tree); err != nil {
			t.Fatalf("%v tree: %v", s, err)
		}
		for l := tree.best; l != nil; l = l.next() {
			ids := queueIDs(t, l)
			if len(ids) == 0 {
				t.Fatalf("%v level %d is empty but still in the tree", s, l.price)
			}
			var lastSeq uint64
			for o := l.head; o != nil; o = o.next {
				switch {
				case o.Side != s || o.Price != l.price:
					t.Fatalf("order %d (%v @ %d) sits in the %v level at %d", o.ID, o.Side, o.Price, s, l.price)
				case o.Type != Limit || o.TIF != GTC:
					t.Fatalf("order %d is %v %v but resting", o.ID, o.Type, o.TIF)
				case o.Remaining <= 0 || o.Filled < 0 || o.Filled+o.Remaining != o.Qty:
					t.Fatalf("order %d: Qty %d, Filled %d, Remaining %d", o.ID, o.Qty, o.Filled, o.Remaining)
				case o.RestedSeq <= lastSeq || o.RestedSeq > b.seq:
					t.Fatalf("order %d: RestedSeq %d after %d (last Seq %d)", o.ID, o.RestedSeq, lastSeq, b.seq)
				case b.orders[o.ID] != o:
					t.Fatalf("order %d is queued but the ID map does not point at it", o.ID)
				}
				lastSeq = o.RestedSeq
				seen++
			}
		}
	}
	if seen != len(b.orders) {
		t.Fatalf("queues hold %d orders, the ID map %d", seen, len(b.orders))
	}
	if bid, ask := b.bids.best, b.asks.best; bid != nil && ask != nil && Buy.Crosses(bid.price, ask.price) {
		t.Fatalf("book is crossed: best bid %d, best ask %d", bid.price, ask.price)
	}
}

// apply applies cmd, checks the book afterwards, and returns the events.
func apply(t *testing.T, b *Book, cmd Command) []Event {
	t.Helper()
	before := b.seq
	evs := b.Apply(cmd, nil)
	for i, e := range evs {
		if e.Seq != before+uint64(i)+1 || e.Timestamp != cmd.Timestamp {
			t.Fatalf("event %d of %+v: Seq %d, Timestamp %d", i, cmd, e.Seq, e.Timestamp)
		}
	}
	if len(evs) == 0 || b.seq != evs[len(evs)-1].Seq {
		t.Fatalf("%+v produced %d events, last Seq %d", cmd, len(evs), b.seq)
	}
	checkBook(t, b)
	return evs
}

func gtc(id OrderID, s Side, p Price, q Qty, ts int64) Command {
	return Command{Kind: CommandNewOrder, OrderID: id, Side: s, Type: Limit, TIF: GTC, Price: p, Qty: q, Timestamp: ts}
}

func cancelCmd(id OrderID, ts int64) Command {
	return Command{Kind: CommandCancel, OrderID: id, Timestamp: ts}
}

func TestNewBookIsEmpty(t *testing.T) {
	b := New()
	checkBook(t, b)
	if b.LastSeq() != 0 || len(b.orders) != 0 || b.bids.best != nil || b.asks.best != nil {
		t.Fatal("new book is not empty")
	}
	if _, ok := b.Order(1); ok {
		t.Fatal("Order(1) found an order in a new book")
	}
}

func TestApplyRejectsInvalidCommandAndChangesNothing(t *testing.T) {
	b := New()
	apply(t, b, gtc(1, Sell, 100, 10, 1))
	bad := gtc(2, Buy, 100, 0, 2) // qty 0
	got := apply(t, b, bad)
	want := []Event{{Seq: 3, Timestamp: 2, Kind: EventRejected, OrderID: 2, Reason: ReasonInvalidQty}}
	if !slices.Equal(got, want) {
		t.Fatalf("events = %+v\nwant %+v", got, want)
	}
	if o, ok := b.Order(1); !ok || o.Remaining != 10 {
		t.Fatalf("resting order changed: %+v, %v", o, ok)
	}
}

func TestApplyRestTradeAndCancel(t *testing.T) {
	b := New()
	got := apply(t, b, gtc(1, Sell, 101, 10, 10))
	want := []Event{
		{Seq: 1, Timestamp: 10, Kind: EventAccepted, OrderID: 1, Side: Sell, Price: 101, Qty: 10},
		{Seq: 2, Timestamp: 10, Kind: EventRested, OrderID: 1, Side: Sell, Price: 101, Qty: 10, LevelSide: Sell, LevelQty: 10},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("rest: events = %+v\nwant %+v", got, want)
	}

	// A buy at 102 crosses and trades at the maker's 101, then rests 2 @ 102.
	got = apply(t, b, gtc(2, Buy, 102, 12, 11))
	want = []Event{
		{Seq: 3, Timestamp: 11, Kind: EventAccepted, OrderID: 2, Side: Buy, Price: 102, Qty: 12},
		{Seq: 4, Timestamp: 11, Kind: EventTrade, OrderID: 2, MakerID: 1, Side: Buy, Price: 101, Qty: 10, LevelSide: Sell, LevelQty: 0},
		{Seq: 5, Timestamp: 11, Kind: EventRested, OrderID: 2, Side: Buy, Price: 102, Qty: 2, LevelSide: Buy, LevelQty: 2},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("trade: events = %+v\nwant %+v", got, want)
	}
	if _, ok := b.Order(1); ok {
		t.Fatal("filled maker is still resting")
	}
	o, ok := b.Order(2)
	wantOrder := Order{ID: 2, Side: Buy, Type: Limit, TIF: GTC, Price: 102, Qty: 12, Filled: 10, Remaining: 2, Timestamp: 11, RestedSeq: 5}
	if !ok || o != wantOrder {
		t.Fatalf("Order(2) = %+v, %v\nwant %+v", o, ok, wantOrder)
	}

	got = apply(t, b, cancelCmd(2, 12))
	want = []Event{{Seq: 6, Timestamp: 12, Kind: EventCanceled, OrderID: 2, Side: Buy, Price: 102, Qty: 2, Reason: ReasonUserCancel, LevelSide: Buy, LevelQty: 0}}
	if !slices.Equal(got, want) {
		t.Fatalf("cancel: events = %+v\nwant %+v", got, want)
	}

	got = apply(t, b, cancelCmd(2, 13))
	want = []Event{{Seq: 7, Timestamp: 13, Kind: EventRejected, OrderID: 2, Reason: ReasonUnknownOrder}}
	if !slices.Equal(got, want) {
		t.Fatalf("second cancel: events = %+v\nwant %+v", got, want)
	}
	if b.LastSeq() != 7 {
		t.Fatalf("LastSeq = %d, want 7", b.LastSeq())
	}
}

func TestApplyRejectsDuplicateRestingID(t *testing.T) {
	b := New()
	apply(t, b, gtc(1, Buy, 99, 5, 1))
	got := apply(t, b, gtc(1, Sell, 120, 5, 2))
	want := []Event{{Seq: 3, Timestamp: 2, Kind: EventRejected, OrderID: 1, Reason: ReasonDuplicateOrderID}}
	if !slices.Equal(got, want) {
		t.Fatalf("events = %+v\nwant %+v", got, want)
	}
}

// TestApplyRandomGTC drives a book with a seeded mix of GTC orders around a
// mid price and cancels of random IDs, checking every invariant after every
// command and quantity conservation per order at the end. The full property
// and fuzz tests come in M2; this one guards M1's matching and cancel paths.
func TestApplyRandomGTC(t *testing.T) {
	for seed := uint64(1); seed <= 5; seed++ {
		r := rand.New(rand.NewPCG(seed, 0xb00c))
		b := New()
		accepted := map[OrderID]Qty{} // order -> submitted qty
		moved := map[OrderID]Qty{}    // order -> traded + canceled qty
		var nextID OrderID
		for ts := int64(1); ts <= 3_000; ts++ {
			var cmd Command
			if nextID > 0 && r.IntN(10) < 3 {
				cmd = cancelCmd(1+OrderID(r.Int64N(int64(nextID))), ts)
			} else {
				nextID++
				side := Buy
				if r.IntN(2) == 0 {
					side = Sell
				}
				cmd = gtc(nextID, side, Price(1_000+r.IntN(21)-10), Qty(1+r.IntN(20)), ts)
			}
			for _, e := range apply(t, b, cmd) {
				switch e.Kind {
				case EventAccepted:
					accepted[e.OrderID] = e.Qty
				case EventTrade:
					moved[e.OrderID] += e.Qty
					moved[e.MakerID] += e.Qty
				case EventCanceled:
					moved[e.OrderID] += e.Qty
				case EventRejected, EventRested:
				}
			}
		}
		for id, q := range accepted {
			var resting Qty
			if o, ok := b.Order(id); ok {
				resting = o.Remaining
			}
			if moved[id]+resting != q {
				t.Fatalf("seed %d, order %d: accepted %d, traded+canceled %d, resting %d", seed, id, q, moved[id], resting)
			}
		}
	}
}

// TestApplyAllocations pins the hot paths that must not allocate once the
// output buffer has grown: a taker that fully fills against a resting maker,
// and a cancel. Resting a new order allocates its node by design.
func TestApplyAllocations(t *testing.T) {
	const runs = 200
	b := New()
	for id := OrderID(1); id <= 2*runs+10; id++ {
		b.Apply(gtc(id, Sell, 100+Price(id%5), 1, 0), nil)
	}
	buf := make([]Event, 0, 16)
	taker := OrderID(1_000_000)
	trade := testing.AllocsPerRun(runs, func() {
		taker++
		buf = b.Apply(gtc(taker, Buy, 200, 1, 0), buf[:0])
	})
	if trade != 0 {
		t.Errorf("full-fill trade allocates %.1f times per Apply", trade)
	}
	victim := OrderID(2*runs + 10)
	cancel := testing.AllocsPerRun(runs, func() {
		buf = b.Apply(cancelCmd(victim, 0), buf[:0])
		victim--
	})
	if cancel != 0 {
		t.Errorf("cancel allocates %.1f times per Apply", cancel)
	}
	checkBook(t, b)
}
