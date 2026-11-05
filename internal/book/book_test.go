package book

import (
	"math/rand/v2"
	"slices"
	"testing"
)

// checkBook fails t if b.CheckInvariants reports a violation.
func checkBook(t *testing.T, b *Book) {
	t.Helper()
	if err := b.CheckInvariants(); err != nil {
		t.Fatal(err)
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
	apply(t, b, at(1, limit(1, Sell, 100, 10)))
	bad := at(2, limit(2, Buy, 100, 0)) // qty 0
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
	got := apply(t, b, at(10, limit(1, Sell, 101, 10)))
	want := []Event{
		{Seq: 1, Timestamp: 10, Kind: EventAccepted, OrderID: 1, Side: Sell, Price: 101, Qty: 10},
		{Seq: 2, Timestamp: 10, Kind: EventRested, OrderID: 1, Side: Sell, Price: 101, Qty: 10, LevelSide: Sell, LevelQty: 10},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("rest: events = %+v\nwant %+v", got, want)
	}

	// A buy at 102 crosses and trades at the maker's 101, then rests 2 @ 102.
	got = apply(t, b, at(11, limit(2, Buy, 102, 12)))
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

	got = apply(t, b, at(12, cancel(2)))
	want = []Event{{Seq: 6, Timestamp: 12, Kind: EventCanceled, OrderID: 2, Side: Buy, Price: 102, Qty: 2, Reason: ReasonUserCancel, LevelSide: Buy, LevelQty: 0}}
	if !slices.Equal(got, want) {
		t.Fatalf("cancel: events = %+v\nwant %+v", got, want)
	}

	got = apply(t, b, at(13, cancel(2)))
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
	apply(t, b, at(1, limit(1, Buy, 99, 5)))
	got := apply(t, b, at(2, limit(1, Sell, 120, 5)))
	want := []Event{{Seq: 3, Timestamp: 2, Kind: EventRejected, OrderID: 1, Reason: ReasonDuplicateOrderID}}
	if !slices.Equal(got, want) {
		t.Fatalf("events = %+v\nwant %+v", got, want)
	}
}

// TestApplyRandomGTC drives a book with a seeded mix of GTC orders around a
// mid price and cancels of random IDs. After every command it checks the
// book's structural invariants (checkBook) and the command's events against
// price-time priority, maker prices and level totals (checkCommandEvents,
// shared with the scenario tests); at the end it checks quantity
// conservation per order. The full property and fuzz tests come in M2; this
// one guards M1's matching and cancel paths.
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
				cmd = at(ts, cancel(1+OrderID(r.Int64N(int64(nextID)))))
			} else {
				nextID++
				side := Buy
				if r.IntN(2) == 0 {
					side = Sell
				}
				cmd = at(ts, limit(nextID, side, Price(1_000+r.IntN(21)-10), Qty(1+r.IntN(20))))
			}
			before := make(map[OrderID]Order, len(b.orders))
			for id, o := range b.orders {
				before[id] = o.Order
			}
			evs := apply(t, b, cmd)
			if err := checkCommandEvents(cmd, evs, before); err != nil {
				t.Fatalf("seed %d, %+v: %v\nevents:%s", seed, cmd, err, formatEvents(evs))
			}
			for _, e := range evs {
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
// output buffer has grown: a taker that fully fills against a resting maker
// and a cancel, each both when the level survives and when it empties and
// is removed from the tree, plus an IOC remainder, a rejected FOK and a
// market fill. Resting a new order allocates its node by design. Every
// measured call is checked to take the intended path (the number and kind
// of its events), because AllocsPerRun rounds down: a path taken by only
// some calls could hide one allocation per call.
func TestApplyAllocations(t *testing.T) {
	const runs = 200 // AllocsPerRun makes runs+1 calls, the first a warm-up
	tests := []struct {
		name    string
		resting func(id OrderID) Command // resting order id, for id 1..runs+2
		op      func(i OrderID) Command  // measured command i, for i 1..runs+1
		events  int                      // number of events op emits
		want    EventKind                // kind of op's last event
		// empties says whether op's last event reports LevelQty 0: it
		// emptied a level, or (IOC remainder, FOK rejection) touched none.
		empties bool
	}{
		{
			name:    "full fill, level stays",
			resting: func(id OrderID) Command { return limit(id, Sell, 100, 1) },
			op:      func(i OrderID) Command { return limit(1_000_000+i, Buy, 100, 1) },
			events:  2,
			want:    EventTrade,
		},
		{
			name:    "full fill sweeps the best level",
			resting: func(id OrderID) Command { return limit(id, Sell, 100+Price(id), 1) },
			op:      func(i OrderID) Command { return limit(1_000_000+i, Buy, MaxPrice, 1) },
			events:  2,
			want:    EventTrade,
			empties: true,
		},
		{
			name:    "cancel, level stays",
			resting: func(id OrderID) Command { return limit(id, Buy, 50, 1) },
			op:      cancel,
			events:  1,
			want:    EventCanceled,
		},
		{
			// One maker per price: IOC i, limited to maker i's price, takes
			// that one lot (removing its level) and cancels the other lot.
			name:    "IOC trades, then its remainder is canceled",
			resting: func(id OrderID) Command { return limit(id, Sell, 100+Price(id), 1) },
			op:      func(i OrderID) Command { return ioc(1_000_000+i, Buy, 100+Price(i), 2) },
			events:  3,
			want:    EventCanceled,
			empties: true,
		},
		{
			name:    "FOK that cannot fill is rejected",
			resting: func(id OrderID) Command { return limit(id, Sell, 100, 1) },
			op:      func(i OrderID) Command { return fok(1_000_000+i, Buy, 100, 1_000_000) },
			events:  1,
			want:    EventRejected,
			empties: true,
		},
		{
			name:    "market order fills in full",
			resting: func(id OrderID) Command { return limit(id, Buy, 100, 1) },
			op:      func(i OrderID) Command { return market(1_000_000+i, Sell, 1) },
			events:  2,
			want:    EventTrade,
		},
		{
			// Prices are a permutation of 1..runs+2 (37 is coprime with
			// runs+2 = 202), so cancelling in ID order removes inner nodes
			// of the tree, not just the best.
			name:    "cancel removes an inner level",
			resting: func(id OrderID) Command { return limit(id, Buy, 1+Price(id*37%(runs+2)), 1) },
			op:      cancel,
			events:  1,
			want:    EventCanceled,
			empties: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := New()
			for id := OrderID(1); id <= runs+2; id++ {
				if evs := b.Apply(tt.resting(id), nil); evs[len(evs)-1].Kind != EventRested {
					t.Fatalf("resting order %d did not rest: %+v", id, evs)
				}
			}
			buf := make([]Event, 0, 16)
			var i OrderID
			allocs := testing.AllocsPerRun(runs, func() {
				i++
				buf = b.Apply(tt.op(i), buf[:0])
				last := buf[len(buf)-1]
				if len(buf) != tt.events || last.Kind != tt.want || (last.LevelQty == 0) != tt.empties {
					t.Fatalf("call %d took the wrong path: %s", i, formatEvents(buf))
				}
			})
			if allocs != 0 {
				t.Errorf("allocates %.1f times per Apply", allocs)
			}
			checkBook(t, b)
		})
	}
}

// TestAvailable pins the FOK pre-check: it adds whole opposite levels from
// the best price outward, stops at the order's limit, and stops as soon as
// the total reaches the order's quantity, so it walks only the levels a fill
// would consume.
func TestAvailable(t *testing.T) {
	b := New()
	for i, p := range []Price{100, 101, 102} { // asks of 5 each
		b.Apply(limit(OrderID(1+i), Sell, p, 5), nil)
	}
	for i, p := range []Price{99, 98, 97} { // bids of 4 each
		b.Apply(limit(OrderID(10+i), Buy, p, 4), nil)
	}
	tests := []struct {
		name string
		cmd  Command
		want Qty
	}{
		{"buy stops once the quantity is reached", fok(20, Buy, 102, 6), 10},
		{"buy stops at its limit", fok(20, Buy, 101, 100), 10},
		{"buy below the best ask finds nothing", fok(20, Buy, 99, 1), 0},
		{"buy at the best ask exactly", fok(20, Buy, 100, 5), 5},
		{"buy reaching its qty exactly with deeper levels crossing", fok(20, Buy, 102, 5), 5},
		{"market buy reaching its qty exactly at the best level", marketFOK(20, Buy, 5), 5},
		{"sell reaching its qty exactly with deeper levels crossing", fok(20, Sell, 1, 4), 4},
		{"market buy counts every level it needs", marketFOK(20, Buy, 100), 15},
		{"market buy needing one level", marketFOK(20, Buy, 1), 5},
		{"sell stops at its limit", fok(20, Sell, 98, 100), 8},
		{"sell stops once the quantity is reached", fok(20, Sell, 1, 5), 8},
		{"sell above the best bid finds nothing", fok(20, Sell, 100, 1), 0},
		{"market sell takes all bids", marketFOK(20, Sell, 1_000), 12},
	}
	for _, tt := range tests {
		if got := b.available(tt.cmd); got != tt.want {
			t.Errorf("%s: available = %d, want %d", tt.name, got, tt.want)
		}
	}
}
