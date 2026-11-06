package book

import (
	"math/rand/v2"
	"slices"
	"testing"
)

// genCommands returns n reproducible random commands that between them
// reach every path of Apply. Prices cluster tightly around a mid price so
// orders cross and levels fill and empty constantly; IDs are mostly fresh
// but sometimes reuse a recent one, which hits both duplicate rejections
// and legal reuse after a fill or cancel; a share of commands is invalid,
// in every way Validate can report; and a rare market sweep clears one
// side of the book.
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
		if r.IntN(2_000) == 0 {
			// A sweep: a market order that clears the whole opposite side,
			// extreme prices included, so empty sides and market remainders
			// recur throughout the run, not only while the book is filling.
			cmds = append(cmds, at(ts, market(id, side, MaxQty)))
			continue
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
			switch r.IntN(9) {
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
			case 6:
				c.Side = 0
			case 7:
				c.Type = 9
			case 8:
				c.TIF = 0
			}
		}
		cmds = append(cmds, at(ts, c))
	}
	return cmds
}

// TestPropertyRandomCommands is the property test of SPEC §7: it applies
// 100,000 seeded random commands (10,000 with -short). After every command
// it checks CheckInvariants, gap-free Seq and the copied timestamp (apply);
// the command's events against checkCommandEvents, fed with the resting book
// as the event stream alone describes it (restingModel), so neither the
// expected makers nor their time priority come from the book under test;
// that the book's resting orders equal that model; and, through the ledger,
// quantity conservation of any order whose ID is reused. At the end it checks
// conservation for every order, that the run reached every event kind,
// reason, order type and path it is meant to, and that replaying the
// commands into a fresh book gives the identical event stream and state.
func TestPropertyRandomCommands(t *testing.T) {
	n := 100_000
	if testing.Short() {
		n = 10_000
	}
	cmds := genCommands(1, n)

	type orderKind struct {
		Type OrderType
		TIF  TIF
	}
	b := New()
	model := newRestingModel()
	lg := ledger{}
	var stream []Event
	kinds := map[EventKind]int{}
	reasons := map[Reason]int{}
	accepted := map[orderKind]int{}
	traded := map[orderKind]int{}
	extremes := map[Price]int{}
	// Paths that depend on the book's state rather than on one command; the
	// sweep and empty-side ones must also be reached in the second half, so
	// a long run cannot cover them only near its start.
	var marketRemainder, marketFOKRejected, fokExact int
	var sideEmptied, intoEmptySide [2]int // [first half, second half]
	maxResting := 0
	for i, c := range cmds {
		half := i * 2 / n
		bestBefore := [2]*levelNode{b.bids.best, b.asks.best}
		_, dup := model.orders[c.OrderID]
		freshNew := c.Validate() == ReasonNone && c.Kind == CommandNewOrder && !dup
		if freshNew && b.side(c.Side.Opposite()).best == nil {
			intoEmptySide[half]++
		}
		if freshNew && c.TIF == FOK && b.available(c) == c.Qty {
			fokExact++
		}

		evs := apply(t, b, c)
		if err := checkCommandEvents(c, evs, model.orders); err != nil {
			t.Fatalf("command %d %+v: %v\nevents:%s", i, c, err, formatEvents(evs))
		}
		if err := model.record(evs); err != nil {
			t.Fatalf("command %d %+v: %v", i, c, err)
		}
		if err := model.check(b); err != nil {
			t.Fatalf("command %d %+v: %v", i, c, err)
		}
		if err := lg.record(evs); err != nil {
			t.Fatalf("command %d %+v: %v", i, c, err)
		}
		stream = append(stream, evs...)

		for _, e := range evs {
			kinds[e.Kind]++
			reasons[e.Reason]++
			if e.Reason == ReasonIOCRemainder && c.Type == Market {
				marketRemainder++
			}
		}
		if c.Type == Market && c.TIF == FOK && evs[0].Reason == ReasonFOKUnfillable {
			marketFOKRejected++
		}
		if c.Kind == CommandNewOrder && evs[0].Kind == EventAccepted {
			k := orderKind{c.Type, c.TIF}
			accepted[k]++
			if len(evs) > 1 && evs[1].Kind == EventTrade {
				traded[k]++
			}
			if c.Price == 1 || c.Price == MaxPrice {
				extremes[c.Price]++
			}
		}
		bestAfter := [2]*levelNode{b.bids.best, b.asks.best}
		for s, before := range bestBefore {
			if before != nil && bestAfter[s] == nil {
				sideEmptied[half]++
			}
		}
		maxResting = max(maxResting, len(b.orders))
	}
	if err := lg.check(b); err != nil {
		t.Fatal(err)
	}

	// The run must have reached everything below, or the generator has
	// drifted and the test proves less than it claims.
	for k := EventAccepted; k <= EventCanceled; k++ {
		if kinds[k] == 0 {
			t.Errorf("no %v events in %d commands", k, n)
		}
	}
	for r := ReasonInvalidCommand; r <= ReasonIOCRemainder; r++ { // every reason
		if reasons[r] == 0 {
			t.Errorf("no %v in %d commands", r, n)
		}
	}
	for _, k := range []orderKind{{Limit, GTC}, {Limit, IOC}, {Limit, FOK}, {Market, IOC}, {Market, FOK}} {
		if accepted[k] == 0 {
			t.Errorf("no accepted %v %v order in %d commands", k.Type, k.TIF, n)
		}
	}
	for _, k := range []orderKind{{Limit, IOC}, {Market, IOC}} { // an accepted FOK always trades
		if traded[k] == 0 {
			t.Errorf("no %v %v order traded in %d commands", k.Type, k.TIF, n)
		}
	}
	for _, p := range []Price{1, MaxPrice} {
		if extremes[p] == 0 {
			t.Errorf("no accepted order at price %d in %d commands", p, n)
		}
	}
	for name, got := range map[string]int{
		"market order remainder canceled":    marketRemainder,
		"market FOK rejected":                marketFOKRejected,
		"FOK exactly fillable":               fokExact,
		"side emptied (first half)":          sideEmptied[0],
		"side emptied (second half)":         sideEmptied[1],
		"new order into an empty side (1st)": intoEmptySide[0],
		"new order into an empty side (2nd)": intoEmptySide[1],
	} {
		if got == 0 {
			t.Errorf("path not reached in %d commands: %s", n, name)
		}
	}
	t.Logf("%d commands, %d events, %d trades, up to %d resting orders; sides emptied %d+%d times, FOK exact %d",
		n, len(stream), kinds[EventTrade], maxResting, sideEmptied[0], sideEmptied[1], fokExact)

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
	if err := sameBook(b, replay); err != nil {
		t.Fatalf("replay: %v", err)
	}
}
