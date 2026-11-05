package book

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
)

// genCommands returns n reproducible random commands that between them
// reach every path of Apply. Prices cluster tightly around a mid price so
// orders cross and levels fill and empty constantly; IDs are mostly fresh
// but sometimes reuse a recent one, which hits both duplicate rejections
// and legal reuse after a fill or cancel; a share of commands is invalid.
func genCommands(seed uint64, n int) []Command {
	r := rand.New(rand.NewPCG(seed, 0x9e3779b97f4a7c15))
	const mid = 10_000
	cmds := make([]Command, 0, n)
	var lastID OrderID
	recentID := func() OrderID { // an ID near the most recent ones
		if lastID == 0 {
			return 1
		}
		back := OrderID(r.IntN(min(int(lastID), 200)))
		return lastID - back
	}
	for i := range n {
		ts := int64(i + 1)
		roll := r.IntN(100)
		if roll < 25 { // cancel, usually of a recent order
			cmds = append(cmds, at(ts, cancel(recentID())))
			continue
		}
		var id OrderID
		if r.IntN(50) == 0 {
			id = recentID()
		} else {
			lastID++
			id = lastID
		}
		side := Buy
		if r.IntN(2) == 0 {
			side = Sell
		}
		price := Price(mid + int(r.NormFloat64()*8))
		switch r.IntN(100) {
		case 0:
			price = 1
		case 1:
			price = MaxPrice
		}
		qty := Qty(1 + r.IntN(50))
		if r.IntN(20) == 0 {
			qty = Qty(1 + r.IntN(1_000))
		}
		var c Command
		switch {
		case roll < 70:
			c = limit(id, side, price, qty)
		case roll < 80:
			c = ioc(id, side, price, qty)
		case roll < 87:
			c = fok(id, side, price, qty)
		case roll < 92:
			c = market(id, side, qty)
		case roll < 95:
			c = marketFOK(id, side, qty)
		default: // invalid in one of several ways
			c = limit(id, side, price, qty)
			switch r.IntN(6) {
			case 0:
				c.Qty = 0
			case 1:
				c.Price = 0
			case 2:
				c.Kind = 9
			case 3:
				c.Type, c.TIF = Market, GTC
			case 4:
				c.OrderID = 0
			case 5:
				c = cancel(recentID())
				c.Qty = 1
			}
		}
		cmds = append(cmds, at(ts, c))
	}
	return cmds
}

// ledger accumulates, per order, the quantities the event stream moves, for
// the conservation check of the package documentation.
type ledger map[OrderID]*orderQty

func (lg ledger) record(evs []Event) {
	for _, e := range evs {
		switch e.Kind {
		case EventAccepted:
			lg[e.OrderID] = &orderQty{Accepted: e.Qty}
		case EventTrade:
			lg[e.OrderID].Traded += e.Qty
			lg[e.MakerID].Traded += e.Qty
		case EventCanceled:
			lg[e.OrderID].Canceled += e.Qty
		case EventRejected, EventRested:
		}
	}
}

// check verifies Accepted = Traded + Canceled + Remaining for every order
// the stream accepted, with Remaining read from the book.
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

// TestPropertyRandomCommands is the property test of SPEC §7: it applies
// 100,000 seeded random commands (10,000 with -short) and after every one
// checks CheckInvariants, gap-free Seq and the copied timestamp (apply), and
// the command's events against an independent derivation from the resting
// orders before it (checkCommandEvents: grammar, price-time priority, maker
// prices, level totals, FOK all or nothing). After the run it checks
// quantity conservation for every order, then replays the same commands into
// a fresh book and requires the identical event stream and final state.
func TestPropertyRandomCommands(t *testing.T) {
	n := 100_000
	if testing.Short() {
		n = 10_000
	}
	cmds := genCommands(1, n)

	b := New()
	lg := ledger{}
	var stream []Event
	kinds := map[EventKind]int{}
	reasons := map[Reason]int{}
	maxResting := 0
	before := make(map[OrderID]Order)
	for i, c := range cmds {
		clear(before)
		for id, o := range b.orders {
			before[id] = o.Order
		}
		evs := apply(t, b, c)
		if err := checkCommandEvents(c, evs, before); err != nil {
			t.Fatalf("command %d %+v: %v\nevents:%s", i, c, err, formatEvents(evs))
		}
		lg.record(evs)
		stream = append(stream, evs...)
		for _, e := range evs {
			kinds[e.Kind]++
			reasons[e.Reason]++
		}
		maxResting = max(maxResting, len(b.orders))
	}
	if err := lg.check(b); err != nil {
		t.Fatal(err)
	}

	// The run must have exercised every event kind and every reason the
	// book can produce, or the generator has drifted and the test proves
	// less than it claims.
	for k := EventAccepted; k <= EventCanceled; k++ {
		if kinds[k] == 0 {
			t.Errorf("no %v events in %d commands", k, n)
		}
	}
	for _, r := range []Reason{
		ReasonInvalidCommand, ReasonInvalidOrderID, ReasonMarketGTC, ReasonInvalidPrice, ReasonInvalidQty,
		ReasonUnexpectedField, ReasonDuplicateOrderID, ReasonUnknownOrder, ReasonFOKUnfillable,
		ReasonUserCancel, ReasonIOCRemainder,
	} {
		if reasons[r] == 0 {
			t.Errorf("no %v in %d commands", r, n)
		}
	}
	t.Logf("%d commands, %d events, %d trades, up to %d resting orders", n, len(stream), kinds[EventTrade], maxResting)

	// Deterministic replay (SPEC §7): the same commands give the same events
	// and the same book.
	replay := New()
	var again []Event
	for _, c := range cmds {
		again = replay.Apply(c, again)
	}
	if !slices.Equal(stream, again) {
		i := 0
		for i < min(len(stream), len(again)) && stream[i] == again[i] {
			i++
		}
		t.Fatalf("replay diverged at event %d of %d/%d", i, len(stream), len(again))
	}
	for _, s := range []Side{Buy, Sell} {
		if a, b := b.Depth(s, 1<<30, nil), replay.Depth(s, 1<<30, nil); !slices.Equal(a, b) {
			t.Fatalf("replayed %v depth differs", s)
		}
	}
	for id, o := range b.orders {
		if r, ok := replay.Order(id); !ok || r != o.Order {
			t.Fatalf("replayed order %d = %+v, %v; want %+v", id, r, ok, o.Order)
		}
	}
	if replay.LastSeq() != b.LastSeq() || len(replay.orders) != len(b.orders) {
		t.Fatalf("replay ends at Seq %d with %d orders, want %d with %d", replay.LastSeq(), len(replay.orders), b.LastSeq(), len(b.orders))
	}
}
