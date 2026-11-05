package book

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
)

// Table-driven matching scenarios (TASKS.md M1 and M2). Each scenario applies
// its setup commands to a fresh book, then the command under test, and
// compares that command's events, the full depth of both sides and chosen
// resting orders with exact values worked out by hand from the package
// documentation, the Event field table and DECISIONS.md D010, D011 and D013.
//
// Independently of those values, scenarioRun.apply checks after every
// command, setup included, what the documentation promises for any command:
// gap-free Seq, copied timestamps, the event grammar, price-time priority,
// "Rejected changed nothing", quantity conservation per order, and depth
// that is sorted, never crossed and agrees with the level totals in the
// event stream.

// scenario is one row of a scenario table.
type scenario struct {
	name  string
	setup []Command // applied first; their events are checked only by scenarioRun.apply
	cmd   Command   // the command under test

	want     []Event // exactly the events of cmd, every field
	wantBids []Level // full depth after cmd, best first; nil when the side is empty
	wantAsks []Level

	// Optional, after cmd: resting orders whose state matters, IDs that
	// must not be resting, and quantity conservation per order over the
	// whole scenario.
	wantOrders map[OrderID]Order
	wantGone   []OrderID
	wantQty    map[OrderID]orderQty
}

// orderQty splits an order's Accepted.Qty as the conservation formula of the
// package documentation does: Accepted = Traded + Canceled + Remaining.
type orderQty struct{ Accepted, Traded, Canceled, Remaining Qty }

// The runner stamps each command before applying it: setup[i] with
// setupTS(i) and the command under test with cmdTS. All commands of a
// scenario thus have distinct timestamps, so copying the wrong one into an
// event or an Order fails. The command constructors leave Timestamp 0.
const cmdTS int64 = 1_000_000

func setupTS(i int) int64 { return 1_000 + int64(i) }

// scenarioMaxDepth is the n the runner passes to Depth to get a whole side;
// no scenario comes close to it.
const scenarioMaxDepth = 64

// limit returns a new limit GTC order.
func limit(id OrderID, side Side, price Price, qty Qty) Command {
	return Command{Kind: CommandNewOrder, OrderID: id, Side: side, Type: Limit, TIF: GTC, Price: price, Qty: qty}
}

// ioc returns a new limit IOC order.
func ioc(id OrderID, side Side, price Price, qty Qty) Command {
	c := limit(id, side, price, qty)
	c.TIF = IOC
	return c
}

// fok returns a new limit FOK order.
func fok(id OrderID, side Side, price Price, qty Qty) Command {
	c := limit(id, side, price, qty)
	c.TIF = FOK
	return c
}

// market returns a new market IOC order.
func market(id OrderID, side Side, qty Qty) Command {
	return Command{Kind: CommandNewOrder, OrderID: id, Side: side, Type: Market, TIF: IOC, Qty: qty}
}

// marketFOK returns a new market FOK order.
func marketFOK(id OrderID, side Side, qty Qty) Command {
	c := market(id, side, qty)
	c.TIF = FOK
	return c
}

// cancel returns a cancel of order id.
func cancel(id OrderID) Command { return Command{Kind: CommandCancel, OrderID: id} }

// at returns c with Timestamp ts.
func at(ts int64, c Command) Command {
	c.Timestamp = ts
	return c
}

// Expected events of the command under test. Each sets Timestamp to cmdTS
// and exactly the fields the Event field table gives its kind; every other
// field stays zero.

func evAccepted(seq uint64, id OrderID, side Side, price Price, qty Qty) Event {
	return Event{Seq: seq, Timestamp: cmdTS, Kind: EventAccepted, OrderID: id, Side: side, Price: price, Qty: qty}
}

func evRejected(seq uint64, id OrderID, why Reason) Event {
	return Event{Seq: seq, Timestamp: cmdTS, Kind: EventRejected, OrderID: id, Reason: why}
}

// evTrade: taker, on side, trades qty with maker at the maker's price, after
// which the maker's level on the other side holds levelQty.
func evTrade(seq uint64, taker, maker OrderID, side Side, price Price, qty, levelQty Qty) Event {
	return Event{
		Seq: seq, Timestamp: cmdTS, Kind: EventTrade, OrderID: taker, MakerID: maker,
		Side: side, Price: price, Qty: qty, LevelSide: side.Opposite(), LevelQty: levelQty,
	}
}

// evRested: qty of order id rests at price, after which its level holds
// levelQty.
func evRested(seq uint64, id OrderID, side Side, price Price, qty, levelQty Qty) Event {
	return Event{
		Seq: seq, Timestamp: cmdTS, Kind: EventRested, OrderID: id,
		Side: side, Price: price, Qty: qty, LevelSide: side, LevelQty: levelQty,
	}
}

// evCanceled: a cancel removed resting order id, which had qty left at
// price, after which its level holds levelQty.
func evCanceled(seq uint64, id OrderID, side Side, price Price, qty, levelQty Qty) Event {
	return Event{
		Seq: seq, Timestamp: cmdTS, Kind: EventCanceled, OrderID: id, Side: side, Price: price,
		Qty: qty, Reason: ReasonUserCancel, LevelSide: side, LevelQty: levelQty,
	}
}

// evIOCRemainder: the untraded qty of an IOC order with limit price, or of a
// market order (price 0), is canceled. It never rested, so no level changes.
func evIOCRemainder(seq uint64, id OrderID, side Side, price Price, qty Qty) Event {
	return Event{
		Seq: seq, Timestamp: cmdTS, Kind: EventCanceled, OrderID: id, Side: side, Price: price,
		Qty: qty, Reason: ReasonIOCRemainder,
	}
}

// lvl is one expected depth level.
func lvl(price Price, qty Qty, orders int) Level {
	return Level{Price: price, Qty: qty, Orders: orders}
}

func TestM1Scenarios(t *testing.T) { runScenarios(t, m1Scenarios) }

func TestM2Scenarios(t *testing.T) { runScenarios(t, m2Scenarios) }

// TestScenarioTables checks the tables themselves, without a book: the M1
// table needs nothing but limit GTC orders and cancels (commands that fail
// Validate aside), and the expected values are consistent with each other.
func TestScenarioTables(t *testing.T) {
	tables := []struct {
		name    string
		table   []scenario
		min     int
		onlyGTC bool
	}{
		{"m1", m1Scenarios, 24, true},
		{"m2", m2Scenarios, 12, false},
	}
	for _, tt := range tables {
		if len(tt.table) < tt.min {
			t.Errorf("%s: %d scenarios, want at least %d", tt.name, len(tt.table), tt.min)
		}
		names := make(map[string]bool)
		for _, sc := range tt.table {
			where := tt.name + "/" + sc.name
			if names[sc.name] {
				t.Errorf("%s: duplicate scenario name", where)
			}
			names[sc.name] = true
			if len(sc.want) == 0 {
				t.Errorf("%s: no events wanted; every command produces at least one", where)
			}
			for i, e := range sc.want {
				if e.Timestamp != cmdTS || (i > 0 && e.Seq != sc.want[i-1].Seq+1) {
					t.Errorf("%s: want[%d] = %+v: want Timestamp cmdTS and consecutive Seq", where, i, e)
				}
			}
			for id, o := range sc.wantOrders {
				if o.ID != id || o.Qty != o.Filled+o.Remaining || o.Remaining <= 0 || o.RestedSeq == 0 {
					t.Errorf("%s: wantOrders[%d] = %+v is not a resting order", where, id, o)
				}
			}
			for id, q := range sc.wantQty {
				if q.Accepted != q.Traded+q.Canceled+q.Remaining {
					t.Errorf("%s: wantQty[%d] = %+v does not add up", where, id, q)
				}
			}
			if !tt.onlyGTC {
				continue
			}
			for _, c := range append(slices.Clone(sc.setup), sc.cmd) {
				if c.Validate() == ReasonNone && c.Kind == CommandNewOrder && (c.Type != Limit || c.TIF != GTC) {
					t.Errorf("%s: %+v is not a limit GTC order", where, c)
				}
			}
		}
	}
}

func runScenarios(t *testing.T, table []scenario) {
	for _, sc := range table {
		t.Run(sc.name, func(t *testing.T) { runScenario(t, sc) })
	}
}

// outSentinel is put in front of the output buffer of the command under test:
// Apply must append after it and leave it alone.
var outSentinel = Event{Seq: 1 << 63, Timestamp: -1, Kind: EventKind(255), OrderID: 1 << 63}

func runScenario(t *testing.T, sc scenario) {
	r := newScenarioRun(t, sc)
	var buf []Event
	for i, c := range sc.setup {
		c.Timestamp = setupTS(i)
		buf = r.apply(fmt.Sprintf("setup[%d] %+v", i, c), c, buf[:0])
	}
	if t.Failed() {
		t.Fatal("setup did not behave as documented; the command under test was not applied")
	}

	cmd := sc.cmd
	cmd.Timestamp = cmdTS
	out := r.apply(fmt.Sprintf("cmd %+v", cmd), cmd, append(make([]Event, 0, 16), outSentinel))
	if got := out[1:]; !slices.Equal(got, sc.want) {
		t.Errorf("events of %+v:\ngot:%s\nwant:%s", cmd, formatEvents(got), formatEvents(sc.want))
	}
	if got := r.depth(Buy); !slices.Equal(got, normLevels(sc.wantBids)) {
		t.Errorf("bids = %+v, want %+v", got, normLevels(sc.wantBids))
	}
	if got := r.depth(Sell); !slices.Equal(got, normLevels(sc.wantAsks)) {
		t.Errorf("asks = %+v, want %+v", got, normLevels(sc.wantAsks))
	}
	for _, id := range slices.Sorted(maps.Keys(sc.wantOrders)) {
		want := sc.wantOrders[id]
		if got, ok := r.b.Order(id); !ok || got != want {
			t.Errorf("Order(%d) = %+v, %v\n                 want %+v, true", id, got, ok, want)
		}
	}
	for _, id := range sc.wantGone {
		if got, ok := r.b.Order(id); ok {
			t.Errorf("Order(%d) = %+v, true; want it gone from the book", id, got)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(sc.wantQty)) {
		if got, want := r.qtyOf(id), sc.wantQty[id]; got != want {
			t.Errorf("order %d: quantities %+v, want %+v", id, got, want)
		}
	}
}

// scenarioRun applies one scenario's commands to a book and follows what the
// events say, to check the book against it after every command.
type scenarioRun struct {
	t      *testing.T
	b      *Book
	ids    []OrderID                // every non-zero OrderID the scenario uses, sorted
	seq    uint64                   // Seq of the last event seen
	l2     map[l2Key]Qty            // non-empty levels, followed from LevelUpdate alone
	ledger map[OrderID]*ledgerEntry // latest accepted order with each ID
}

type l2Key struct {
	side  Side
	price Price
}

// ledgerEntry is what the events so far say about one accepted order.
type ledgerEntry struct {
	order    Order // the Order it must be while it rests, except Remaining
	canceled Qty
}

func newScenarioRun(t *testing.T, sc scenario) *scenarioRun {
	t.Helper()
	b := New()
	if got := b.LastSeq(); got != 0 {
		t.Fatalf("New().LastSeq() = %d, want 0", got)
	}
	var ids []OrderID
	for _, c := range append(slices.Clone(sc.setup), sc.cmd) {
		if c.OrderID != 0 && !slices.Contains(ids, c.OrderID) {
			ids = append(ids, c.OrderID)
		}
	}
	slices.Sort(ids)
	r := &scenarioRun{t: t, b: b, ids: ids, l2: make(map[l2Key]Qty), ledger: make(map[OrderID]*ledgerEntry)}
	if n := len(r.resting()); n != 0 || r.depth(Buy) != nil || r.depth(Sell) != nil {
		t.Fatalf("New() is not empty: %d orders, bids %+v, asks %+v", n, r.depth(Buy), r.depth(Sell))
	}
	return r
}

// apply applies c with out as the output buffer, checks the result and
// returns what Apply returned. Whatever c is, it checks that:
//   - Apply keeps out and appends at least one event;
//   - Seq runs 1, 2, 3, ... across the scenario, LastSeq follows it, and
//     every event carries c.Timestamp;
//   - the events are exactly what the grammar, the field table and
//     price-time priority give for c and the orders resting before it
//     (checkCommandEvents);
//   - a Rejected command changed nothing;
//   - every resting order is what its events say, and every order that left
//     the book accounted for its Accepted.Qty (checkOrders);
//   - depth is sorted, never crossed, and agrees with the resting orders and
//     with the level totals of the events (checkDepth).
func (r *scenarioRun) apply(label string, c Command, out []Event) []Event {
	t := r.t
	t.Helper()
	before, bidsBefore, asksBefore := r.resting(), r.depth(Buy), r.depth(Sell)

	prefix := slices.Clone(out)
	res := r.b.Apply(c, out)
	if len(res) <= len(prefix) || !slices.Equal(res[:len(prefix)], prefix) {
		t.Fatalf("%s: Apply returned%s\nwant the %d events passed in, then at least one more", label, formatEvents(res), len(prefix))
	}
	evs := res[len(prefix):]

	for _, e := range evs {
		r.seq++
		if e.Seq != r.seq {
			t.Errorf("%s: %+v: Seq %d, want %d (gap-free from 1)", label, e, e.Seq, r.seq)
			r.seq = e.Seq
		}
		if e.Timestamp != c.Timestamp {
			t.Errorf("%s: %+v: Timestamp %d, want the command's %d", label, e, e.Timestamp, c.Timestamp)
		}
	}
	if got := r.b.LastSeq(); got != r.seq {
		t.Errorf("%s: LastSeq() = %d, want %d", label, got, r.seq)
	}
	if err := checkCommandEvents(c, evs, before); err != nil {
		t.Errorf("%s: %v\nevents:%s", label, err, formatEvents(evs))
	}
	r.record(c, evs)

	after, bids, asks := r.resting(), r.depth(Buy), r.depth(Sell)
	if evs[0].Kind == EventRejected &&
		(!maps.Equal(after, before) || !slices.Equal(bids, bidsBefore) || !slices.Equal(asks, asksBefore)) {
		t.Errorf("%s: Rejected, but the book changed: bids %+v -> %+v, asks %+v -> %+v, orders %+v -> %+v",
			label, bidsBefore, bids, asksBefore, asks, before, after)
	}
	r.checkOrders(label, after)
	r.checkDepth(label, evs, bids, asks, after)
	return res
}

// resting returns every order of the scenario the book holds, by ID.
func (r *scenarioRun) resting() map[OrderID]Order {
	m := make(map[OrderID]Order)
	for _, id := range r.ids {
		if o, ok := r.b.Order(id); ok {
			m[id] = o
		}
	}
	return m
}

// depth returns the whole of side s, best first, or nil if it is empty.
func (r *scenarioRun) depth(s Side) []Level {
	r.t.Helper()
	lv := r.b.Depth(s, scenarioMaxDepth, nil)
	if len(lv) >= scenarioMaxDepth {
		r.t.Fatalf("Depth(%v, %d) returned %d levels; cannot tell whether that is all", s, scenarioMaxDepth, len(lv))
	}
	return normLevels(lv)
}

// record updates the ledger with the events of c.
func (r *scenarioRun) record(c Command, evs []Event) {
	for _, e := range evs {
		switch e.Kind {
		case EventAccepted:
			// A new life for the ID: a reused ID's old order has left the book.
			r.ledger[e.OrderID] = &ledgerEntry{order: Order{
				ID: e.OrderID, Side: e.Side, Type: c.Type, TIF: c.TIF,
				Price: e.Price, Qty: e.Qty, Timestamp: e.Timestamp,
			}}
		case EventTrade:
			r.entry(e.OrderID).order.Filled += e.Qty
			r.entry(e.MakerID).order.Filled += e.Qty
		case EventRested:
			r.entry(e.OrderID).order.RestedSeq = e.Seq
		case EventCanceled:
			r.entry(e.OrderID).canceled += e.Qty
		case EventRejected:
			// Changes no order.
		}
	}
}

func (r *scenarioRun) entry(id OrderID) *ledgerEntry {
	if l, ok := r.ledger[id]; ok {
		return l
	}
	r.t.Errorf("an event names order %d, which was never accepted", id)
	return new(ledgerEntry)
}

// qtyOf returns the conservation terms of order id over the whole scenario.
func (r *scenarioRun) qtyOf(id OrderID) orderQty {
	l, ok := r.ledger[id]
	if !ok {
		return orderQty{}
	}
	q := orderQty{Accepted: l.order.Qty, Traded: l.order.Filled, Canceled: l.canceled}
	if o, ok := r.b.Order(id); ok {
		q.Remaining = o.Remaining
	}
	return q
}

// checkOrders checks quantity conservation per order: a resting order is
// exactly what its events say (so Accepted.Qty = traded + Remaining), and an
// order that left the book has Accepted.Qty = traded + canceled.
func (r *scenarioRun) checkOrders(label string, after map[OrderID]Order) {
	t := r.t
	t.Helper()
	for _, id := range r.ids {
		l, accepted := r.ledger[id]
		o, rests := after[id]
		switch {
		case rests && !accepted:
			t.Errorf("%s: order %d rests but was never accepted: %+v", label, id, o)
		case rests:
			want := l.order
			want.Remaining = want.Qty - want.Filled
			if o != want || l.canceled != 0 || o.Type != Limit || o.TIF != GTC || o.RestedSeq == 0 || o.Remaining <= 0 {
				t.Errorf("%s: Order(%d) = %+v\nits events say %+v, canceled %d; a resting order is limit GTC with RestedSeq > 0 and Remaining > 0",
					label, id, o, want, l.canceled)
			}
		case accepted && l.order.Qty != l.order.Filled+l.canceled:
			t.Errorf("%s: order %d left the book with Accepted.Qty %d != traded %d + canceled %d",
				label, id, l.order.Qty, l.order.Filled, l.canceled)
		}
	}
}

// checkDepth checks the depth of both sides after the events evs: sorted best
// first, never crossed, equal to the resting orders summed by level, and
// equal to the level totals the events carried, both for the levels evs
// touched (the last event touching a level carries its total) and for the
// whole book followed from LevelUpdate alone. It also checks that Depth(s, n,
// dst) appends to dst and stops after n levels.
func (r *scenarioRun) checkDepth(label string, evs []Event, bids, asks []Level, after map[OrderID]Order) {
	t := r.t
	t.Helper()
	sides := [...]struct {
		side   Side
		levels []Level
	}{{Buy, bids}, {Sell, asks}}
	levelsOf := func(s Side) []Level {
		if s == Buy {
			return bids
		}
		return asks
	}

	for _, sd := range sides {
		for i, l := range sd.levels {
			sorted := i == 0 || (sd.side == Buy && l.Price < sd.levels[i-1].Price) || (sd.side == Sell && l.Price > sd.levels[i-1].Price)
			if !sorted || l.Qty <= 0 || l.Orders <= 0 {
				t.Errorf("%s: %v depth %+v: want strictly best first, Qty > 0 and Orders > 0", label, sd.side, sd.levels)
				break
			}
		}
	}
	if len(bids) > 0 && len(asks) > 0 && Buy.Crosses(bids[0].Price, asks[0].Price) {
		t.Errorf("%s: book crossed: best bid %d >= best ask %d", label, bids[0].Price, asks[0].Price)
	}

	// The last event touching a level carries the level's total afterwards.
	last := make(map[l2Key]Qty)
	for _, e := range evs {
		if lu, ok := e.LevelUpdate(); ok {
			last[l2Key{lu.Side, lu.Price}] = lu.Qty
		}
	}
	for _, k := range slices.SortedFunc(maps.Keys(last), compareL2Key) {
		var inDepth Qty
		if i := slices.IndexFunc(levelsOf(k.side), func(l Level) bool { return l.Price == k.price }); i >= 0 {
			inDepth = levelsOf(k.side)[i].Qty
		}
		if inDepth != last[k] {
			t.Errorf("%s: last event touching %v %d says LevelQty %d, but Depth has %d", label, k.side, k.price, last[k], inDepth)
		}
		if last[k] == 0 {
			delete(r.l2, k)
		} else {
			r.l2[k] = last[k]
		}
	}

	// The whole depth follows from the stream, and from the resting orders.
	type total struct {
		qty    Qty
		orders int
	}
	fromDepth, fromStream := make(map[l2Key]total), make(map[l2Key]total)
	for _, sd := range sides {
		for _, l := range sd.levels {
			fromDepth[l2Key{sd.side, l.Price}] = total{l.Qty, l.Orders}
		}
	}
	for k, q := range r.l2 {
		fromStream[k] = total{q, fromDepth[k].orders} // the stream carries no order counts
	}
	if !maps.Equal(fromStream, fromDepth) {
		t.Errorf("%s: levels followed from LevelUpdate events %v, but Depth has %v", label, r.l2, fromDepth)
	}
	fromOrders := make(map[l2Key]total)
	for _, o := range after {
		k := l2Key{o.Side, o.Price}
		fromOrders[k] = total{fromOrders[k].qty + o.Remaining, fromOrders[k].orders + 1}
	}
	if !maps.Equal(fromOrders, fromDepth) {
		t.Errorf("%s: resting orders sum to levels %v, but Depth has %v", label, fromOrders, fromDepth)
	}

	// Depth(s, n, dst) appends the first n levels to dst.
	sentinel := Level{Price: -1, Qty: -1, Orders: -1}
	for _, sd := range sides {
		for n := range len(sd.levels) + 2 {
			if got, want := normLevels(r.b.Depth(sd.side, n, nil)), sd.levels[:min(n, len(sd.levels))]; !slices.Equal(got, want) {
				t.Errorf("%s: Depth(%v, %d, nil) = %+v, want %+v", label, sd.side, n, got, want)
			}
		}
		got := r.b.Depth(sd.side, scenarioMaxDepth, []Level{sentinel})
		if len(got) == 0 || got[0] != sentinel || !slices.Equal(got[1:], sd.levels) {
			t.Errorf("%s: Depth(%v, %d, [sentinel]) = %+v, want the sentinel followed by %+v", label, sd.side, scenarioMaxDepth, got, sd.levels)
		}
	}
}

func compareL2Key(a, b l2Key) int {
	return cmp.Or(cmp.Compare(a.side, b.side), cmp.Compare(a.price, b.price))
}

// checkCommandEvents checks evs, the events c produced, given before, the
// orders resting just before c. It derives the expected events from the
// event grammar and field table of the package documentation and from
// price-time priority: a new order trades with the crossing makers best
// price first and, within a price, by RestedSeq, each trade taking the
// smaller of what the taker and the maker have left. Seq and Timestamp are
// checked by the caller, so the expected events copy them.
//
// The property and fuzz tests pass before from a restingModel, built from
// the event stream alone, so neither the expected makers nor their time
// priority are taken from the book under test.
func checkCommandEvents(c Command, evs []Event, before map[OrderID]Order) error {
	if len(evs) == 0 {
		return errors.New("no events")
	}
	// Level totals before c, updated as the expected events change them.
	level := make(map[l2Key]Qty)
	for _, o := range before {
		level[l2Key{o.Side, o.Price}] += o.Remaining
	}
	match := func(e, want Event) error {
		want.Seq, want.Timestamp = e.Seq, e.Timestamp
		if e != want {
			return fmt.Errorf("got  %+v\nwant %+v", e, want)
		}
		return nil
	}
	only := func(want Event) error {
		if len(evs) != 1 {
			return fmt.Errorf("%d events, want only %+v", len(evs), want)
		}
		return match(evs[0], want)
	}
	rejected := func(why Reason) Event {
		return Event{Kind: EventRejected, OrderID: c.OrderID, Reason: why}
	}

	if why := c.Validate(); why != ReasonNone {
		return only(rejected(why))
	}
	if c.Kind == CommandCancel {
		o, ok := before[c.OrderID]
		if !ok {
			return only(rejected(ReasonUnknownOrder))
		}
		k := l2Key{o.Side, o.Price}
		return only(Event{
			Kind: EventCanceled, OrderID: o.ID, Side: o.Side, Price: o.Price, Qty: o.Remaining,
			Reason: ReasonUserCancel, LevelSide: o.Side, LevelQty: level[k] - o.Remaining,
		})
	}

	// A valid new order: the makers it can trade with, in priority order.
	var makers []Order
	var crossable Qty
	for _, o := range before {
		if o.Side == c.Side.Opposite() && (c.Type == Market || c.Side.Crosses(c.Price, o.Price)) {
			makers = append(makers, o)
			crossable += o.Remaining
		}
	}
	slices.SortFunc(makers, func(a, b Order) int {
		switch {
		case a.Price == b.Price:
			return cmp.Compare(a.RestedSeq, b.RestedSeq)
		case c.Side == Buy:
			return cmp.Compare(a.Price, b.Price) // lowest ask first
		default:
			return cmp.Compare(b.Price, a.Price) // highest bid first
		}
	})

	// Stateful checks, in the book's fixed order: a resting ID wins over an
	// unfillable FOK, so the reason is deterministic.
	if _, dup := before[c.OrderID]; dup {
		return only(rejected(ReasonDuplicateOrderID))
	}
	if c.TIF == FOK && crossable < c.Qty {
		return only(rejected(ReasonFOKUnfillable))
	}
	if evs[0].Kind == EventRejected {
		return fmt.Errorf("valid order rejected with %v (crossable qty %d)", evs[0].Reason, crossable)
	}
	if err := match(evs[0], Event{Kind: EventAccepted, OrderID: c.OrderID, Side: c.Side, Price: c.Price, Qty: c.Qty}); err != nil {
		return err
	}

	left, rest := c.Qty, evs[1:]
	for _, m := range makers {
		if left == 0 {
			break
		}
		if len(rest) == 0 {
			return fmt.Errorf("no trade with maker %+v, next in priority", m)
		}
		q := min(left, m.Remaining)
		k := l2Key{m.Side, m.Price}
		level[k] -= q
		want := Event{
			Kind: EventTrade, OrderID: c.OrderID, MakerID: m.ID, Side: c.Side, Price: m.Price,
			Qty: q, LevelSide: m.Side, LevelQty: level[k],
		}
		if err := match(rest[0], want); err != nil {
			return err
		}
		left, rest = left-q, rest[1:]
	}
	if left == 0 {
		if len(rest) != 0 {
			return fmt.Errorf("events after the order was filled:%s", formatEvents(rest))
		}
		return nil // a fully filled order ends with its last trade
	}

	// What is left rests (GTC) or is canceled (IOC, limit or market). A FOK
	// order never gets here: it passed the crossable check above, so the
	// makers covered its whole quantity and it returned as filled.
	want := Event{Kind: EventCanceled, OrderID: c.OrderID, Side: c.Side, Price: c.Price, Qty: left, Reason: ReasonIOCRemainder}
	if c.TIF == GTC {
		k := l2Key{c.Side, c.Price}
		want = Event{Kind: EventRested, OrderID: c.OrderID, Side: c.Side, Price: c.Price, Qty: left, LevelSide: c.Side, LevelQty: level[k] + left}
	}
	if len(rest) != 1 {
		return fmt.Errorf("%d events after the trades, want only %+v", len(rest), want)
	}
	return match(rest[0], want)
}

func formatEvents(evs []Event) string {
	if len(evs) == 0 {
		return " none"
	}
	var b strings.Builder
	for _, e := range evs {
		fmt.Fprintf(&b, "\n\t%+v", e)
	}
	return b.String()
}

// normLevels returns lv, or nil if it is empty, so that an empty side prints
// and compares the same whether the book returned nil or an empty slice.
func normLevels(lv []Level) []Level {
	if len(lv) == 0 {
		return nil
	}
	return lv
}

// threeBidsAt100 rests bids #1 1, #2 2 and #3 3 at 100, in that order
// (Seq 1-6).
var threeBidsAt100 = []Command{limit(1, Buy, 100, 1), limit(2, Buy, 100, 2), limit(3, Buy, 100, 3)}

// multiLevelSetup interleaves bids and asks into three levels per side, some
// with two orders (Seq 1-18):
//
//	bids  99: #1 5, #5 4    98: #7 6           97: #3 2, #9 3
//	asks 101: #4 7         102: #2 3, #6 1    104: #8 2
var multiLevelSetup = []Command{
	limit(1, Buy, 99, 5),
	limit(2, Sell, 102, 3),
	limit(3, Buy, 97, 2),
	limit(4, Sell, 101, 7),
	limit(5, Buy, 99, 4),
	limit(6, Sell, 102, 1),
	limit(7, Buy, 98, 6),
	limit(8, Sell, 104, 2),
	limit(9, Buy, 97, 3),
}

// m1Scenarios uses only limit GTC orders and cancels, plus commands that fail
// Validate.
var m1Scenarios = append([]scenario{
	// Resting without trading.
	{
		name: "empty book: buy rests",
		cmd:  limit(1, Buy, 100, 10),
		want: []Event{
			evAccepted(1, 1, Buy, 100, 10),
			evRested(2, 1, Buy, 100, 10, 10),
		},
		wantBids: []Level{lvl(100, 10, 1)},
		wantOrders: map[OrderID]Order{
			1: {ID: 1, Side: Buy, Type: Limit, TIF: GTC, Price: 100, Qty: 10, Remaining: 10, Timestamp: cmdTS, RestedSeq: 2},
		},
	},
	{
		name: "empty book: sell rests",
		cmd:  limit(1, Sell, 105, 7),
		want: []Event{
			evAccepted(1, 1, Sell, 105, 7),
			evRested(2, 1, Sell, 105, 7, 7),
		},
		wantAsks: []Level{lvl(105, 7, 1)},
		wantOrders: map[OrderID]Order{
			1: {ID: 1, Side: Sell, Type: Limit, TIF: GTC, Price: 105, Qty: 7, Remaining: 7, Timestamp: cmdTS, RestedSeq: 2},
		},
	},
	{
		name:  "buy joins its level behind the resting order",
		setup: []Command{limit(1, Buy, 100, 10)},
		cmd:   limit(2, Buy, 100, 5),
		want: []Event{
			evAccepted(3, 2, Buy, 100, 5),
			evRested(4, 2, Buy, 100, 5, 15),
		},
		wantBids: []Level{lvl(100, 15, 2)},
		wantOrders: map[OrderID]Order{
			1: {ID: 1, Side: Buy, Type: Limit, TIF: GTC, Price: 100, Qty: 10, Remaining: 10, Timestamp: setupTS(0), RestedSeq: 2},
			2: {ID: 2, Side: Buy, Type: Limit, TIF: GTC, Price: 100, Qty: 5, Remaining: 5, Timestamp: cmdTS, RestedSeq: 4},
		},
	},
	{
		name:  "sell joins its level behind two resting orders",
		setup: []Command{limit(1, Sell, 101, 3), limit(2, Sell, 101, 4)},
		cmd:   limit(3, Sell, 101, 5),
		want: []Event{
			evAccepted(5, 3, Sell, 101, 5),
			evRested(6, 3, Sell, 101, 5, 12),
		},
		wantAsks: []Level{lvl(101, 12, 3)},
		wantOrders: map[OrderID]Order{
			2: {ID: 2, Side: Sell, Type: Limit, TIF: GTC, Price: 101, Qty: 4, Remaining: 4, Timestamp: setupTS(1), RestedSeq: 4},
			3: {ID: 3, Side: Sell, Type: Limit, TIF: GTC, Price: 101, Qty: 5, Remaining: 5, Timestamp: cmdTS, RestedSeq: 6},
		},
	},
	{
		name:  "buy inside the spread becomes the best bid",
		setup: []Command{limit(1, Buy, 99, 5), limit(2, Sell, 103, 5)},
		cmd:   limit(3, Buy, 101, 2),
		want: []Event{
			evAccepted(5, 3, Buy, 101, 2),
			evRested(6, 3, Buy, 101, 2, 2),
		},
		wantBids: []Level{lvl(101, 2, 1), lvl(99, 5, 1)},
		wantAsks: []Level{lvl(103, 5, 1)},
	},
	{
		name:  "sell inside the spread becomes the best ask",
		setup: []Command{limit(1, Buy, 99, 5), limit(2, Sell, 103, 5)},
		cmd:   limit(3, Sell, 102, 4),
		want: []Event{
			evAccepted(5, 3, Sell, 102, 4),
			evRested(6, 3, Sell, 102, 4, 4),
		},
		wantBids: []Level{lvl(99, 5, 1)},
		wantAsks: []Level{lvl(102, 4, 1), lvl(103, 5, 1)},
	},
	{
		name:  "sell one tick above the best bid rests without trading",
		setup: []Command{limit(1, Buy, 100, 5)},
		cmd:   limit(2, Sell, 101, 5),
		want: []Event{
			evAccepted(3, 2, Sell, 101, 5),
			evRested(4, 2, Sell, 101, 5, 5),
		},
		wantBids: []Level{lvl(100, 5, 1)},
		wantAsks: []Level{lvl(101, 5, 1)},
		wantOrders: map[OrderID]Order{
			1: {ID: 1, Side: Buy, Type: Limit, TIF: GTC, Price: 100, Qty: 5, Remaining: 5, Timestamp: setupTS(0), RestedSeq: 2},
			2: {ID: 2, Side: Sell, Type: Limit, TIF: GTC, Price: 101, Qty: 5, Remaining: 5, Timestamp: cmdTS, RestedSeq: 4},
		},
	},
	{
		name:  "buy one tick below the best ask rests without trading",
		setup: []Command{limit(1, Sell, 101, 5)},
		cmd:   limit(2, Buy, 100, 5),
		want: []Event{
			evAccepted(3, 2, Buy, 100, 5),
			evRested(4, 2, Buy, 100, 5, 5),
		},
		wantBids: []Level{lvl(100, 5, 1)},
		wantAsks: []Level{lvl(101, 5, 1)},
	},

	// Trading with one maker.
	{
		name:  "buy at the ask price fills exactly",
		setup: []Command{limit(1, Sell, 100, 10)},
		cmd:   limit(2, Buy, 100, 10),
		want: []Event{
			evAccepted(3, 2, Buy, 100, 10),
			evTrade(4, 2, 1, Buy, 100, 10, 0),
		},
		wantGone: []OrderID{1, 2},
		wantQty: map[OrderID]orderQty{
			1: {Accepted: 10, Traded: 10},
			2: {Accepted: 10, Traded: 10},
		},
	},
	{
		name:  "sell at the bid price fills exactly",
		setup: []Command{limit(1, Buy, 100, 10)},
		cmd:   limit(2, Sell, 100, 10),
		want: []Event{
			evAccepted(3, 2, Sell, 100, 10),
			evTrade(4, 2, 1, Sell, 100, 10, 0),
		},
		wantGone: []OrderID{1, 2},
	},
	{
		name:  "buy taker partially fills one maker and rests the remainder",
		setup: []Command{limit(1, Sell, 100, 4)},
		cmd:   limit(2, Buy, 100, 10),
		want: []Event{
			evAccepted(3, 2, Buy, 100, 10),
			evTrade(4, 2, 1, Buy, 100, 4, 0),
			evRested(5, 2, Buy, 100, 6, 6),
		},
		wantBids: []Level{lvl(100, 6, 1)},
		wantOrders: map[OrderID]Order{
			2: {ID: 2, Side: Buy, Type: Limit, TIF: GTC, Price: 100, Qty: 10, Filled: 4, Remaining: 6, Timestamp: cmdTS, RestedSeq: 5},
		},
		wantGone: []OrderID{1},
		wantQty: map[OrderID]orderQty{
			2: {Accepted: 10, Traded: 4, Remaining: 6},
		},
	},
	{
		name:  "sell taker partially fills one maker and rests the remainder at its limit",
		setup: []Command{limit(1, Buy, 100, 3)},
		cmd:   limit(2, Sell, 99, 8),
		want: []Event{
			evAccepted(3, 2, Sell, 99, 8),
			evTrade(4, 2, 1, Sell, 100, 3, 0),
			evRested(5, 2, Sell, 99, 5, 5),
		},
		wantAsks: []Level{lvl(99, 5, 1)},
		wantOrders: map[OrderID]Order{
			2: {ID: 2, Side: Sell, Type: Limit, TIF: GTC, Price: 99, Qty: 8, Filled: 3, Remaining: 5, Timestamp: cmdTS, RestedSeq: 5},
		},
		wantGone: []OrderID{1},
	},
	{
		name:  "taker fills the head maker exactly and leaves the next one untouched",
		setup: []Command{limit(1, Sell, 100, 5), limit(2, Sell, 100, 7)},
		cmd:   limit(3, Buy, 100, 5),
		want: []Event{
			evAccepted(5, 3, Buy, 100, 5),
			evTrade(6, 3, 1, Buy, 100, 5, 7),
		},
		wantAsks: []Level{lvl(100, 7, 1)},
		wantOrders: map[OrderID]Order{
			2: {ID: 2, Side: Sell, Type: Limit, TIF: GTC, Price: 100, Qty: 7, Remaining: 7, Timestamp: setupTS(1), RestedSeq: 4},
		},
		wantGone: []OrderID{1, 3},
	},
	{
		name:  "partially filled maker stays at the head with reduced qty and its RestedSeq",
		setup: []Command{limit(1, Sell, 100, 10), limit(2, Sell, 100, 3)},
		cmd:   limit(3, Buy, 100, 4),
		want: []Event{
			evAccepted(5, 3, Buy, 100, 4),
			evTrade(6, 3, 1, Buy, 100, 4, 9),
		},
		wantAsks: []Level{lvl(100, 9, 2)},
		wantOrders: map[OrderID]Order{
			1: {ID: 1, Side: Sell, Type: Limit, TIF: GTC, Price: 100, Qty: 10, Filled: 4, Remaining: 6, Timestamp: setupTS(0), RestedSeq: 2},
			2: {ID: 2, Side: Sell, Type: Limit, TIF: GTC, Price: 100, Qty: 3, Remaining: 3, Timestamp: setupTS(1), RestedSeq: 4},
		},
		wantGone: []OrderID{3},
	},
	{
		name:  "partially filled maker keeps time priority for the next taker",
		setup: []Command{limit(1, Sell, 100, 10), limit(2, Sell, 100, 3), limit(3, Buy, 100, 4)},
		cmd:   limit(4, Buy, 100, 7),
		want: []Event{
			evAccepted(7, 4, Buy, 100, 7),
			evTrade(8, 4, 1, Buy, 100, 6, 3),
			evTrade(9, 4, 2, Buy, 100, 1, 2),
		},
		wantAsks: []Level{lvl(100, 2, 1)},
		wantOrders: map[OrderID]Order{
			2: {ID: 2, Side: Sell, Type: Limit, TIF: GTC, Price: 100, Qty: 3, Filled: 1, Remaining: 2, Timestamp: setupTS(1), RestedSeq: 4},
		},
		wantGone: []OrderID{1, 3, 4},
		wantQty: map[OrderID]orderQty{
			1: {Accepted: 10, Traded: 10},
			2: {Accepted: 3, Traded: 1, Remaining: 2},
		},
	},

	// Trading with several makers.
	{
		name:  "taker sweeps one level in arrival order, not ID order",
		setup: []Command{limit(30, Sell, 100, 2), limit(10, Sell, 100, 3), limit(20, Sell, 100, 4)},
		cmd:   limit(5, Buy, 100, 8),
		want: []Event{
			evAccepted(7, 5, Buy, 100, 8),
			evTrade(8, 5, 30, Buy, 100, 2, 7),
			evTrade(9, 5, 10, Buy, 100, 3, 4),
			evTrade(10, 5, 20, Buy, 100, 3, 1),
		},
		wantAsks: []Level{lvl(100, 1, 1)},
		wantOrders: map[OrderID]Order{
			20: {ID: 20, Side: Sell, Type: Limit, TIF: GTC, Price: 100, Qty: 4, Filled: 3, Remaining: 1, Timestamp: setupTS(2), RestedSeq: 6},
		},
		wantGone: []OrderID{5, 10, 30},
	},
	{
		name: "buy taker sweeps ask levels best first and rests at its limit",
		setup: []Command{
			limit(1, Sell, 101, 5),
			limit(2, Sell, 100, 3),
			limit(3, Sell, 103, 4),
			limit(4, Sell, 102, 2),
		},
		cmd: limit(5, Buy, 102, 20),
		want: []Event{
			evAccepted(9, 5, Buy, 102, 20),
			evTrade(10, 5, 2, Buy, 100, 3, 0),
			evTrade(11, 5, 1, Buy, 101, 5, 0),
			evTrade(12, 5, 4, Buy, 102, 2, 0),
			evRested(13, 5, Buy, 102, 10, 10),
		},
		wantBids: []Level{lvl(102, 10, 1)},
		wantAsks: []Level{lvl(103, 4, 1)},
		wantOrders: map[OrderID]Order{
			3: {ID: 3, Side: Sell, Type: Limit, TIF: GTC, Price: 103, Qty: 4, Remaining: 4, Timestamp: setupTS(2), RestedSeq: 6},
			5: {ID: 5, Side: Buy, Type: Limit, TIF: GTC, Price: 102, Qty: 20, Filled: 10, Remaining: 10, Timestamp: cmdTS, RestedSeq: 13},
		},
		wantGone: []OrderID{1, 2, 4},
	},
	{
		name: "sell taker sweeps bid levels best first and rests at its limit",
		setup: []Command{
			limit(1, Buy, 98, 4),
			limit(2, Buy, 100, 3),
			limit(3, Buy, 99, 5),
			limit(4, Buy, 97, 1),
		},
		cmd: limit(5, Sell, 99, 10),
		want: []Event{
			evAccepted(9, 5, Sell, 99, 10),
			evTrade(10, 5, 2, Sell, 100, 3, 0),
			evTrade(11, 5, 3, Sell, 99, 5, 0),
			evRested(12, 5, Sell, 99, 2, 2),
		},
		wantBids: []Level{lvl(98, 4, 1), lvl(97, 1, 1)},
		wantAsks: []Level{lvl(99, 2, 1)},
		wantOrders: map[OrderID]Order{
			1: {ID: 1, Side: Buy, Type: Limit, TIF: GTC, Price: 98, Qty: 4, Remaining: 4, Timestamp: setupTS(0), RestedSeq: 2},
			5: {ID: 5, Side: Sell, Type: Limit, TIF: GTC, Price: 99, Qty: 10, Filled: 8, Remaining: 2, Timestamp: cmdTS, RestedSeq: 12},
		},
		wantGone: []OrderID{2, 3},
	},
	{
		name:  "buy taker gets each maker's lower price and ends with its last trade",
		setup: []Command{limit(1, Sell, 100, 2), limit(2, Sell, 101, 3)},
		cmd:   limit(3, Buy, 105, 5),
		want: []Event{
			evAccepted(5, 3, Buy, 105, 5),
			evTrade(6, 3, 1, Buy, 100, 2, 0),
			evTrade(7, 3, 2, Buy, 101, 3, 0),
		},
		wantGone: []OrderID{1, 2, 3},
	},
	{
		name:  "sell taker gets the maker's higher price",
		setup: []Command{limit(1, Buy, 105, 5)},
		cmd:   limit(2, Sell, 100, 3),
		want: []Event{
			evAccepted(3, 2, Sell, 100, 3),
			evTrade(4, 2, 1, Sell, 105, 3, 2),
		},
		wantBids: []Level{lvl(105, 2, 1)},
		wantOrders: map[OrderID]Order{
			1: {ID: 1, Side: Buy, Type: Limit, TIF: GTC, Price: 105, Qty: 5, Filled: 3, Remaining: 2, Timestamp: setupTS(0), RestedSeq: 2},
		},
		wantGone: []OrderID{2},
	},
	{
		name:  "level emptied by a trade leaves the depth and the other levels stay",
		setup: []Command{limit(1, Sell, 100, 3), limit(2, Sell, 101, 4), limit(3, Buy, 99, 2)},
		cmd:   limit(4, Buy, 100, 3),
		want: []Event{
			evAccepted(7, 4, Buy, 100, 3),
			evTrade(8, 4, 1, Buy, 100, 3, 0),
		},
		wantBids: []Level{lvl(99, 2, 1)},
		wantAsks: []Level{lvl(101, 4, 1)},
		wantGone: []OrderID{1, 4},
	},

	// Cancels.
	{
		name:  "cancel the head of a queue",
		setup: threeBidsAt100,
		cmd:   cancel(1),
		want: []Event{
			evCanceled(7, 1, Buy, 100, 1, 5),
		},
		wantBids: []Level{lvl(100, 5, 2)},
		wantOrders: map[OrderID]Order{
			2: {ID: 2, Side: Buy, Type: Limit, TIF: GTC, Price: 100, Qty: 2, Remaining: 2, Timestamp: setupTS(1), RestedSeq: 4},
			3: {ID: 3, Side: Buy, Type: Limit, TIF: GTC, Price: 100, Qty: 3, Remaining: 3, Timestamp: setupTS(2), RestedSeq: 6},
		},
		wantGone: []OrderID{1},
	},
	{
		name:  "cancel the middle of a queue",
		setup: threeBidsAt100,
		cmd:   cancel(2),
		want: []Event{
			evCanceled(7, 2, Buy, 100, 2, 4),
		},
		wantBids: []Level{lvl(100, 4, 2)},
		wantOrders: map[OrderID]Order{
			1: {ID: 1, Side: Buy, Type: Limit, TIF: GTC, Price: 100, Qty: 1, Remaining: 1, Timestamp: setupTS(0), RestedSeq: 2},
			3: {ID: 3, Side: Buy, Type: Limit, TIF: GTC, Price: 100, Qty: 3, Remaining: 3, Timestamp: setupTS(2), RestedSeq: 6},
		},
		wantGone: []OrderID{2},
	},
	{
		name:  "cancel the tail of a queue",
		setup: threeBidsAt100,
		cmd:   cancel(3),
		want: []Event{
			evCanceled(7, 3, Buy, 100, 3, 3),
		},
		wantBids: []Level{lvl(100, 3, 2)},
		wantOrders: map[OrderID]Order{
			1: {ID: 1, Side: Buy, Type: Limit, TIF: GTC, Price: 100, Qty: 1, Remaining: 1, Timestamp: setupTS(0), RestedSeq: 2},
			2: {ID: 2, Side: Buy, Type: Limit, TIF: GTC, Price: 100, Qty: 2, Remaining: 2, Timestamp: setupTS(1), RestedSeq: 4},
		},
		wantGone: []OrderID{3},
	},
	{
		name:  "after the middle is canceled the queue trades head then tail",
		setup: append(slices.Clone(threeBidsAt100), cancel(2)),
		cmd:   limit(4, Sell, 100, 4),
		want: []Event{
			evAccepted(8, 4, Sell, 100, 4),
			evTrade(9, 4, 1, Sell, 100, 1, 3),
			evTrade(10, 4, 3, Sell, 100, 3, 0),
		},
		wantGone: []OrderID{1, 2, 3, 4},
	},
	{
		name:  "cancel the only order of a level removes the level",
		setup: []Command{limit(1, Sell, 100, 5), limit(2, Sell, 101, 6)},
		cmd:   cancel(1),
		want: []Event{
			evCanceled(5, 1, Sell, 100, 5, 0),
		},
		wantAsks: []Level{lvl(101, 6, 1)},
		wantOrders: map[OrderID]Order{
			2: {ID: 2, Side: Sell, Type: Limit, TIF: GTC, Price: 101, Qty: 6, Remaining: 6, Timestamp: setupTS(1), RestedSeq: 4},
		},
		wantGone: []OrderID{1},
	},
	{
		name:  "after the head is canceled the next order at that level trades first",
		setup: []Command{limit(1, Sell, 100, 5), limit(2, Sell, 100, 6), cancel(1)},
		cmd:   limit(3, Buy, 100, 2),
		want: []Event{
			evAccepted(6, 3, Buy, 100, 2),
			evTrade(7, 3, 2, Buy, 100, 2, 4),
		},
		wantAsks: []Level{lvl(100, 4, 1)},
		wantOrders: map[OrderID]Order{
			2: {ID: 2, Side: Sell, Type: Limit, TIF: GTC, Price: 100, Qty: 6, Filled: 2, Remaining: 4, Timestamp: setupTS(1), RestedSeq: 4},
		},
		wantGone: []OrderID{1, 3},
	},
	{
		name:  "cancel of a partially filled maker removes only what is left",
		setup: []Command{limit(1, Sell, 100, 10), limit(2, Buy, 100, 4)},
		cmd:   cancel(1),
		want: []Event{
			evCanceled(5, 1, Sell, 100, 6, 0),
		},
		wantGone: []OrderID{1, 2},
		wantQty: map[OrderID]orderQty{
			1: {Accepted: 10, Traded: 4, Canceled: 6},
		},
	},
	{
		name:  "cancel of a taker that rested after a partial fill",
		setup: []Command{limit(1, Sell, 100, 4), limit(2, Buy, 101, 10)},
		cmd:   cancel(2),
		want: []Event{
			evCanceled(6, 2, Buy, 101, 6, 0),
		},
		wantGone: []OrderID{1, 2},
		wantQty: map[OrderID]orderQty{
			1: {Accepted: 4, Traded: 4},
			2: {Accepted: 10, Traded: 4, Canceled: 6},
		},
	},
	{
		name:  "cancel of an unknown id is rejected",
		setup: []Command{limit(1, Buy, 100, 5)},
		cmd:   cancel(42),
		want: []Event{
			evRejected(3, 42, ReasonUnknownOrder),
		},
		wantBids: []Level{lvl(100, 5, 1)},
		wantOrders: map[OrderID]Order{
			1: {ID: 1, Side: Buy, Type: Limit, TIF: GTC, Price: 100, Qty: 5, Remaining: 5, Timestamp: setupTS(0), RestedSeq: 2},
		},
	},
	{
		name:  "cancel of a fully filled maker is rejected as unknown",
		setup: []Command{limit(1, Sell, 100, 5), limit(2, Buy, 100, 5)},
		cmd:   cancel(1),
		want: []Event{
			evRejected(5, 1, ReasonUnknownOrder),
		},
		wantGone: []OrderID{1, 2},
	},
	{
		name:  "cancel of a taker that filled without resting is rejected as unknown",
		setup: []Command{limit(1, Sell, 100, 5), limit(2, Buy, 100, 5)},
		cmd:   cancel(2),
		want: []Event{
			evRejected(5, 2, ReasonUnknownOrder),
		},
		wantGone: []OrderID{1, 2},
	},
	{
		name:  "second cancel of the same order is rejected as unknown",
		setup: []Command{limit(1, Buy, 100, 5), limit(2, Buy, 99, 1), cancel(1)},
		cmd:   cancel(1),
		want: []Event{
			evRejected(6, 1, ReasonUnknownOrder),
		},
		wantBids: []Level{lvl(99, 1, 1)},
		wantOrders: map[OrderID]Order{
			2: {ID: 2, Side: Buy, Type: Limit, TIF: GTC, Price: 99, Qty: 1, Remaining: 1, Timestamp: setupTS(1), RestedSeq: 4},
		},
		wantGone: []OrderID{1},
	},

	// Order IDs.
	{
		name:  "duplicate id of a resting order on the other side is rejected without trading",
		setup: []Command{limit(1, Sell, 100, 5)},
		cmd:   limit(1, Buy, 100, 5),
		want: []Event{
			evRejected(3, 1, ReasonDuplicateOrderID),
		},
		wantAsks: []Level{lvl(100, 5, 1)},
		wantOrders: map[OrderID]Order{
			1: {ID: 1, Side: Sell, Type: Limit, TIF: GTC, Price: 100, Qty: 5, Remaining: 5, Timestamp: setupTS(0), RestedSeq: 2},
		},
	},
	{
		name:  "duplicate id of a resting order on the same side is rejected",
		setup: []Command{limit(1, Buy, 100, 5), limit(2, Buy, 99, 3)},
		cmd:   limit(2, Buy, 98, 1),
		want: []Event{
			evRejected(5, 2, ReasonDuplicateOrderID),
		},
		wantBids: []Level{lvl(100, 5, 1), lvl(99, 3, 1)},
		wantOrders: map[OrderID]Order{
			2: {ID: 2, Side: Buy, Type: Limit, TIF: GTC, Price: 99, Qty: 3, Remaining: 3, Timestamp: setupTS(1), RestedSeq: 4},
		},
	},
	{
		// The book only sees duplicates among resting orders (OrderID docs).
		name:  "id of a fully filled order can be reused",
		setup: []Command{limit(1, Sell, 100, 5), limit(2, Buy, 100, 5)},
		cmd:   limit(1, Buy, 99, 3),
		want: []Event{
			evAccepted(5, 1, Buy, 99, 3),
			evRested(6, 1, Buy, 99, 3, 3),
		},
		wantBids: []Level{lvl(99, 3, 1)},
		wantOrders: map[OrderID]Order{
			1: {ID: 1, Side: Buy, Type: Limit, TIF: GTC, Price: 99, Qty: 3, Remaining: 3, Timestamp: cmdTS, RestedSeq: 6},
		},
	},
	{
		name:  "id of a canceled order can be reused",
		setup: []Command{limit(1, Sell, 100, 5), cancel(1)},
		cmd:   limit(1, Sell, 101, 2),
		want: []Event{
			evAccepted(4, 1, Sell, 101, 2),
			evRested(5, 1, Sell, 101, 2, 2),
		},
		wantAsks: []Level{lvl(101, 2, 1)},
		wantOrders: map[OrderID]Order{
			1: {ID: 1, Side: Sell, Type: Limit, TIF: GTC, Price: 101, Qty: 2, Remaining: 2, Timestamp: cmdTS, RestedSeq: 5},
		},
	},

	// A deeper book.
	{
		name:  "interleaved bids and asks build a multi-level book",
		setup: multiLevelSetup,
		cmd:   limit(10, Sell, 101, 5),
		want: []Event{
			evAccepted(19, 10, Sell, 101, 5),
			evRested(20, 10, Sell, 101, 5, 12),
		},
		wantBids: []Level{lvl(99, 9, 2), lvl(98, 6, 1), lvl(97, 5, 2)},
		wantAsks: []Level{lvl(101, 12, 2), lvl(102, 4, 2), lvl(104, 2, 1)},
		wantOrders: map[OrderID]Order{
			4:  {ID: 4, Side: Sell, Type: Limit, TIF: GTC, Price: 101, Qty: 7, Remaining: 7, Timestamp: setupTS(3), RestedSeq: 8},
			9:  {ID: 9, Side: Buy, Type: Limit, TIF: GTC, Price: 97, Qty: 3, Remaining: 3, Timestamp: setupTS(8), RestedSeq: 18},
			10: {ID: 10, Side: Sell, Type: Limit, TIF: GTC, Price: 101, Qty: 5, Remaining: 5, Timestamp: cmdTS, RestedSeq: 20},
		},
	},
	{
		name:  "buy sweep through a multi-level book updates totals and order counts",
		setup: multiLevelSetup,
		cmd:   limit(10, Buy, 102, 9),
		want: []Event{
			evAccepted(19, 10, Buy, 102, 9),
			evTrade(20, 10, 4, Buy, 101, 7, 0),
			evTrade(21, 10, 2, Buy, 102, 2, 2),
		},
		wantBids: []Level{lvl(99, 9, 2), lvl(98, 6, 1), lvl(97, 5, 2)},
		wantAsks: []Level{lvl(102, 2, 2), lvl(104, 2, 1)},
		wantOrders: map[OrderID]Order{
			2: {ID: 2, Side: Sell, Type: Limit, TIF: GTC, Price: 102, Qty: 3, Filled: 2, Remaining: 1, Timestamp: setupTS(1), RestedSeq: 4},
			6: {ID: 6, Side: Sell, Type: Limit, TIF: GTC, Price: 102, Qty: 1, Remaining: 1, Timestamp: setupTS(5), RestedSeq: 12},
		},
		wantGone: []OrderID{4, 10},
	},
	{
		name:  "sell sweep through a multi-level book updates totals and order counts",
		setup: multiLevelSetup,
		cmd:   limit(10, Sell, 97, 16),
		want: []Event{
			evAccepted(19, 10, Sell, 97, 16),
			evTrade(20, 10, 1, Sell, 99, 5, 4),
			evTrade(21, 10, 5, Sell, 99, 4, 0),
			evTrade(22, 10, 7, Sell, 98, 6, 0),
			evTrade(23, 10, 3, Sell, 97, 1, 4),
		},
		wantBids: []Level{lvl(97, 4, 2)},
		wantAsks: []Level{lvl(101, 7, 1), lvl(102, 4, 2), lvl(104, 2, 1)},
		wantOrders: map[OrderID]Order{
			3: {ID: 3, Side: Buy, Type: Limit, TIF: GTC, Price: 97, Qty: 2, Filled: 1, Remaining: 1, Timestamp: setupTS(2), RestedSeq: 6},
			9: {ID: 9, Side: Buy, Type: Limit, TIF: GTC, Price: 97, Qty: 3, Remaining: 3, Timestamp: setupTS(8), RestedSeq: 18},
		},
		wantGone: []OrderID{1, 5, 7, 10},
	},
}, invalidCommandScenarios()...)

// invalidCommandScenarios covers commands that fail Validate. The book must
// answer each with exactly Rejected(the Validate reason), echoing the
// command's OrderID, so Seq advances by one, and change nothing, although
// most of them would trade with, rest beside or cancel the resting bid #1
// 5 @ 100 or ask #2 5 @ 101 (Seq 1-4) if they were accepted.
func invalidCommandScenarios() []scenario {
	setup := []Command{limit(1, Buy, 100, 5), limit(2, Sell, 101, 5)}
	orders := map[OrderID]Order{
		1: {ID: 1, Side: Buy, Type: Limit, TIF: GTC, Price: 100, Qty: 5, Remaining: 5, Timestamp: setupTS(0), RestedSeq: 2},
		2: {ID: 2, Side: Sell, Type: Limit, TIF: GTC, Price: 101, Qty: 5, Remaining: 5, Timestamp: setupTS(1), RestedSeq: 4},
	}
	edit := func(c Command, f func(*Command)) Command {
		f(&c)
		return c
	}
	buy := limit(3, Buy, 101, 5) // valid; it would trade with #2
	cases := []struct {
		name string
		cmd  Command
		why  Reason
	}{
		{"zero command", Command{}, ReasonInvalidCommand},
		{"unknown kind echoes its order id", edit(buy, func(c *Command) { c.Kind = 3 }), ReasonInvalidCommand},
		{"new order with id 0", edit(buy, func(c *Command) { c.OrderID = 0 }), ReasonInvalidOrderID},
		{"side 0", edit(buy, func(c *Command) { c.Side = 0 }), ReasonInvalidSide},
		{"side 3", edit(buy, func(c *Command) { c.Side = 3 }), ReasonInvalidSide},
		{"type 0", edit(buy, func(c *Command) { c.Type = 0 }), ReasonInvalidType},
		{"tif 0", edit(buy, func(c *Command) { c.TIF = 0 }), ReasonInvalidTIF},
		{"tif 4", edit(buy, func(c *Command) { c.TIF = 4 }), ReasonInvalidTIF},
		{"market gtc", edit(buy, func(c *Command) { c.Type, c.Price = Market, 0 }), ReasonMarketGTC},
		{"market with a price", edit(buy, func(c *Command) { c.Type, c.TIF = Market, IOC }), ReasonInvalidPrice},
		{"limit price 0", edit(buy, func(c *Command) { c.Price = 0 }), ReasonInvalidPrice},
		{"limit price above MaxPrice", edit(buy, func(c *Command) { c.Price = MaxPrice + 1 }), ReasonInvalidPrice},
		{"negative sell price", limit(3, Sell, -100, 5), ReasonInvalidPrice},
		{"qty 0", edit(buy, func(c *Command) { c.Qty = 0 }), ReasonInvalidQty},
		{"qty above MaxQty", edit(buy, func(c *Command) { c.Qty = MaxQty + 1 }), ReasonInvalidQty},
		{"invalid qty wins over a duplicate id", limit(1, Buy, 100, -5), ReasonInvalidQty},
		{"cancel with id 0", cancel(0), ReasonInvalidOrderID},
		{"cancel with a qty", edit(cancel(1), func(c *Command) { c.Qty = 5 }), ReasonUnexpectedField},
		{"cancel with a side", edit(cancel(2), func(c *Command) { c.Side = Sell }), ReasonUnexpectedField},
		{"cancel with a price", edit(cancel(2), func(c *Command) { c.Price = 101 }), ReasonUnexpectedField},
	}
	var out []scenario
	for _, tc := range cases {
		out = append(out, scenario{
			name:       "invalid: " + tc.name,
			setup:      setup,
			cmd:        tc.cmd,
			want:       []Event{evRejected(5, tc.cmd.OrderID, tc.why)},
			wantBids:   []Level{lvl(100, 5, 1)},
			wantAsks:   []Level{lvl(101, 5, 1)},
			wantOrders: orders,
		})
	}
	return out
}

// m2Scenarios covers market, IOC and FOK orders (M2; DECISIONS.md D010).
var m2Scenarios = []scenario{
	// Market orders.
	{
		name:  "market buy sweeps ask levels with no limit",
		setup: []Command{limit(1, Sell, 100, 2), limit(2, Sell, 105, 3), limit(3, Sell, 200, 4)},
		cmd:   market(4, Buy, 8),
		want: []Event{
			evAccepted(7, 4, Buy, 0, 8),
			evTrade(8, 4, 1, Buy, 100, 2, 0),
			evTrade(9, 4, 2, Buy, 105, 3, 0),
			evTrade(10, 4, 3, Buy, 200, 3, 1),
		},
		wantAsks: []Level{lvl(200, 1, 1)},
		wantOrders: map[OrderID]Order{
			3: {ID: 3, Side: Sell, Type: Limit, TIF: GTC, Price: 200, Qty: 4, Filled: 3, Remaining: 1, Timestamp: setupTS(2), RestedSeq: 6},
		},
		wantGone: []OrderID{1, 2, 4},
	},
	{
		name:  "market sell sweeps bid levels down to price 1 and fills exactly",
		setup: []Command{limit(1, Buy, 100, 2), limit(2, Buy, 90, 3), limit(3, Buy, 1, 5)},
		cmd:   market(4, Sell, 10),
		want: []Event{
			evAccepted(7, 4, Sell, 0, 10),
			evTrade(8, 4, 1, Sell, 100, 2, 0),
			evTrade(9, 4, 2, Sell, 90, 3, 0),
			evTrade(10, 4, 3, Sell, 1, 5, 0),
		},
		wantGone: []OrderID{1, 2, 3, 4},
		wantQty: map[OrderID]orderQty{
			4: {Accepted: 10, Traded: 10},
		},
	},
	{
		name: "market into an empty book is canceled in full at price 0",
		cmd:  market(1, Buy, 10),
		want: []Event{
			evAccepted(1, 1, Buy, 0, 10),
			evIOCRemainder(2, 1, Buy, 0, 10),
		},
		wantGone: []OrderID{1},
		wantQty: map[OrderID]orderQty{
			1: {Accepted: 10, Canceled: 10},
		},
	},
	{
		name:  "market sell with only asks in the book is canceled in full",
		setup: []Command{limit(1, Sell, 100, 5)},
		cmd:   market(2, Sell, 3),
		want: []Event{
			evAccepted(3, 2, Sell, 0, 3),
			evIOCRemainder(4, 2, Sell, 0, 3),
		},
		wantAsks: []Level{lvl(100, 5, 1)},
		wantOrders: map[OrderID]Order{
			1: {ID: 1, Side: Sell, Type: Limit, TIF: GTC, Price: 100, Qty: 5, Remaining: 5, Timestamp: setupTS(0), RestedSeq: 2},
		},
		wantGone: []OrderID{2},
	},
	{
		name:  "market partially fills, then its remainder is canceled",
		setup: []Command{limit(1, Sell, 100, 3), limit(2, Sell, 101, 2)},
		cmd:   market(3, Buy, 10),
		want: []Event{
			evAccepted(5, 3, Buy, 0, 10),
			evTrade(6, 3, 1, Buy, 100, 3, 0),
			evTrade(7, 3, 2, Buy, 101, 2, 0),
			evIOCRemainder(8, 3, Buy, 0, 5),
		},
		wantGone: []OrderID{1, 2, 3},
		wantQty: map[OrderID]orderQty{
			3: {Accepted: 10, Traded: 5, Canceled: 5},
		},
	},

	// IOC limit orders.
	{
		name:  "IOC partially fills, then its remainder is canceled at the limit price",
		setup: []Command{limit(1, Sell, 100, 3), limit(2, Sell, 102, 5)},
		cmd:   ioc(3, Buy, 101, 10),
		want: []Event{
			evAccepted(5, 3, Buy, 101, 10),
			evTrade(6, 3, 1, Buy, 100, 3, 0),
			evIOCRemainder(7, 3, Buy, 101, 7),
		},
		wantAsks: []Level{lvl(102, 5, 1)},
		wantOrders: map[OrderID]Order{
			2: {ID: 2, Side: Sell, Type: Limit, TIF: GTC, Price: 102, Qty: 5, Remaining: 5, Timestamp: setupTS(1), RestedSeq: 4},
		},
		wantGone: []OrderID{1, 3},
		wantQty: map[OrderID]orderQty{
			3: {Accepted: 10, Traded: 3, Canceled: 7},
		},
	},
	{
		name:  "IOC sell empties a level, then its remainder is canceled",
		setup: []Command{limit(1, Buy, 100, 4), limit(2, Buy, 100, 6)},
		cmd:   ioc(3, Sell, 100, 12),
		want: []Event{
			evAccepted(5, 3, Sell, 100, 12),
			evTrade(6, 3, 1, Sell, 100, 4, 6),
			evTrade(7, 3, 2, Sell, 100, 6, 0),
			evIOCRemainder(8, 3, Sell, 100, 2),
		},
		wantGone: []OrderID{1, 2, 3},
	},
	{
		name:  "IOC that fills in full ends with its last trade",
		setup: []Command{limit(1, Buy, 100, 4), limit(2, Buy, 100, 6)},
		cmd:   ioc(3, Sell, 100, 5),
		want: []Event{
			evAccepted(5, 3, Sell, 100, 5),
			evTrade(6, 3, 1, Sell, 100, 4, 6),
			evTrade(7, 3, 2, Sell, 100, 1, 5),
		},
		wantBids: []Level{lvl(100, 5, 1)},
		wantOrders: map[OrderID]Order{
			2: {ID: 2, Side: Buy, Type: Limit, TIF: GTC, Price: 100, Qty: 6, Filled: 1, Remaining: 5, Timestamp: setupTS(1), RestedSeq: 4},
		},
		wantGone: []OrderID{1, 3},
	},
	{
		name:  "IOC that does not cross is accepted and canceled in full",
		setup: []Command{limit(1, Sell, 105, 5), limit(2, Buy, 100, 1)},
		cmd:   ioc(3, Buy, 104, 4),
		want: []Event{
			evAccepted(5, 3, Buy, 104, 4),
			evIOCRemainder(6, 3, Buy, 104, 4),
		},
		wantBids: []Level{lvl(100, 1, 1)},
		wantAsks: []Level{lvl(105, 5, 1)},
		wantGone: []OrderID{3},
	},

	// FOK orders.
	{
		name:  "FOK fills in full across several levels",
		setup: []Command{limit(1, Sell, 100, 2), limit(2, Sell, 101, 3), limit(3, Sell, 102, 4)},
		cmd:   fok(4, Buy, 102, 7),
		want: []Event{
			evAccepted(7, 4, Buy, 102, 7),
			evTrade(8, 4, 1, Buy, 100, 2, 0),
			evTrade(9, 4, 2, Buy, 101, 3, 0),
			evTrade(10, 4, 3, Buy, 102, 2, 2),
		},
		wantAsks: []Level{lvl(102, 2, 1)},
		wantOrders: map[OrderID]Order{
			3: {ID: 3, Side: Sell, Type: Limit, TIF: GTC, Price: 102, Qty: 4, Filled: 2, Remaining: 2, Timestamp: setupTS(2), RestedSeq: 6},
		},
		wantGone: []OrderID{1, 2, 4},
	},
	{
		name:  "FOK exactly fillable takes all the quantity within its limit",
		setup: []Command{limit(1, Buy, 100, 4), limit(2, Buy, 99, 6), limit(3, Buy, 98, 5)},
		cmd:   fok(4, Sell, 99, 10),
		want: []Event{
			evAccepted(7, 4, Sell, 99, 10),
			evTrade(8, 4, 1, Sell, 100, 4, 0),
			evTrade(9, 4, 2, Sell, 99, 6, 0),
		},
		wantBids: []Level{lvl(98, 5, 1)},
		wantOrders: map[OrderID]Order{
			3: {ID: 3, Side: Buy, Type: Limit, TIF: GTC, Price: 98, Qty: 5, Remaining: 5, Timestamp: setupTS(2), RestedSeq: 6},
		},
		wantGone: []OrderID{1, 2, 4},
	},
	{
		name:  "FOK short by one lot is only rejected and changes nothing",
		setup: []Command{limit(1, Buy, 100, 4), limit(2, Buy, 99, 6), limit(3, Buy, 98, 5)},
		cmd:   fok(4, Sell, 99, 11),
		want: []Event{
			evRejected(7, 4, ReasonFOKUnfillable),
		},
		wantBids: []Level{lvl(100, 4, 1), lvl(99, 6, 1), lvl(98, 5, 1)},
		wantOrders: map[OrderID]Order{
			1: {ID: 1, Side: Buy, Type: Limit, TIF: GTC, Price: 100, Qty: 4, Remaining: 4, Timestamp: setupTS(0), RestedSeq: 2},
			2: {ID: 2, Side: Buy, Type: Limit, TIF: GTC, Price: 99, Qty: 6, Remaining: 6, Timestamp: setupTS(1), RestedSeq: 4},
			3: {ID: 3, Side: Buy, Type: Limit, TIF: GTC, Price: 98, Qty: 5, Remaining: 5, Timestamp: setupTS(2), RestedSeq: 6},
		},
		wantGone: []OrderID{4},
	},
	{
		name:  "FOK that could fill only beyond its limit is rejected",
		setup: []Command{limit(1, Sell, 100, 2), limit(2, Sell, 103, 10)},
		cmd:   fok(3, Buy, 102, 5),
		want: []Event{
			evRejected(5, 3, ReasonFOKUnfillable),
		},
		wantAsks: []Level{lvl(100, 2, 1), lvl(103, 10, 1)},
		wantOrders: map[OrderID]Order{
			1: {ID: 1, Side: Sell, Type: Limit, TIF: GTC, Price: 100, Qty: 2, Remaining: 2, Timestamp: setupTS(0), RestedSeq: 2},
			2: {ID: 2, Side: Sell, Type: Limit, TIF: GTC, Price: 103, Qty: 10, Remaining: 10, Timestamp: setupTS(1), RestedSeq: 4},
		},
		wantGone: []OrderID{3},
	},
	{
		name:     "FOK that reuses a resting id and cannot fill is rejected as a duplicate",
		setup:    []Command{limit(1, Sell, 100, 2)},
		cmd:      fok(1, Buy, 100, 5),
		want:     []Event{evRejected(3, 1, ReasonDuplicateOrderID)},
		wantAsks: []Level{lvl(100, 2, 1)},
		wantOrders: map[OrderID]Order{
			1: {ID: 1, Side: Sell, Type: Limit, TIF: GTC, Price: 100, Qty: 2, Remaining: 2, Timestamp: setupTS(0), RestedSeq: 2},
		},
	},
	{
		name: "FOK into an empty book is rejected",
		cmd:  fok(1, Buy, 100, 1),
		want: []Event{
			evRejected(1, 1, ReasonFOKUnfillable),
		},
		wantGone: []OrderID{1},
	},
	{
		name:  "market FOK fills in full across levels",
		setup: []Command{limit(1, Sell, 100, 2), limit(2, Sell, 150, 3)},
		cmd:   marketFOK(3, Buy, 5),
		want: []Event{
			evAccepted(5, 3, Buy, 0, 5),
			evTrade(6, 3, 1, Buy, 100, 2, 0),
			evTrade(7, 3, 2, Buy, 150, 3, 0),
		},
		wantGone: []OrderID{1, 2, 3},
	},
	{
		name:  "market FOK larger than the whole other side is rejected",
		setup: []Command{limit(1, Sell, 100, 2), limit(2, Sell, 150, 3)},
		cmd:   marketFOK(3, Buy, 6),
		want: []Event{
			evRejected(5, 3, ReasonFOKUnfillable),
		},
		wantAsks: []Level{lvl(100, 2, 1), lvl(150, 3, 1)},
		wantOrders: map[OrderID]Order{
			1: {ID: 1, Side: Sell, Type: Limit, TIF: GTC, Price: 100, Qty: 2, Remaining: 2, Timestamp: setupTS(0), RestedSeq: 2},
			2: {ID: 2, Side: Sell, Type: Limit, TIF: GTC, Price: 150, Qty: 3, Remaining: 3, Timestamp: setupTS(1), RestedSeq: 4},
		},
		wantGone: []OrderID{3},
	},

	// More market, IOC and FOK cases, added with M2.
	{
		name:  "market sell fills the resting bids at one price in FIFO order",
		setup: []Command{limit(1, Buy, 100, 3), limit(2, Buy, 100, 4), limit(3, Buy, 99, 5)},
		cmd:   market(4, Sell, 5),
		want: []Event{
			evAccepted(7, 4, Sell, 0, 5),
			evTrade(8, 4, 1, Sell, 100, 3, 4),
			evTrade(9, 4, 2, Sell, 100, 2, 2),
		},
		wantBids: []Level{lvl(100, 2, 1), lvl(99, 5, 1)},
		wantOrders: map[OrderID]Order{
			2: {ID: 2, Side: Buy, Type: Limit, TIF: GTC, Price: 100, Qty: 4, Filled: 2, Remaining: 2, Timestamp: setupTS(1), RestedSeq: 4},
		},
		wantGone: []OrderID{1, 4},
	},
	{
		name:  "market buy at the price and quantity bounds",
		setup: []Command{limit(1, Sell, MaxPrice, MaxQty)},
		cmd:   market(2, Buy, MaxQty),
		want: []Event{
			evAccepted(3, 2, Buy, 0, MaxQty),
			evTrade(4, 2, 1, Buy, MaxPrice, MaxQty, 0),
		},
		wantGone: []OrderID{1, 2},
		wantQty: map[OrderID]orderQty{
			2: {Accepted: MaxQty, Traded: MaxQty},
		},
	},
	{
		name:  "IOC buy sweeps two levels, stops at its limit and cancels the rest",
		setup: []Command{limit(1, Sell, 100, 2), limit(2, Sell, 101, 3), limit(3, Sell, 103, 4)},
		cmd:   ioc(4, Buy, 102, 10),
		want: []Event{
			evAccepted(7, 4, Buy, 102, 10),
			evTrade(8, 4, 1, Buy, 100, 2, 0),
			evTrade(9, 4, 2, Buy, 101, 3, 0),
			evIOCRemainder(10, 4, Buy, 102, 5),
		},
		wantAsks: []Level{lvl(103, 4, 1)},
		wantGone: []OrderID{1, 2, 4},
		wantQty: map[OrderID]orderQty{
			4: {Accepted: 10, Traded: 5, Canceled: 5},
		},
	},
	{
		name:  "FOK sell fills exactly across bid levels in price then time order",
		setup: []Command{limit(1, Buy, 101, 2), limit(2, Buy, 101, 1), limit(3, Buy, 100, 4), limit(4, Buy, 99, 9)},
		cmd:   fok(5, Sell, 100, 7),
		want: []Event{
			evAccepted(9, 5, Sell, 100, 7),
			evTrade(10, 5, 1, Sell, 101, 2, 1),
			evTrade(11, 5, 2, Sell, 101, 1, 0),
			evTrade(12, 5, 3, Sell, 100, 4, 0),
		},
		wantBids: []Level{lvl(99, 9, 1)},
		wantGone: []OrderID{1, 2, 3, 5},
		wantQty: map[OrderID]orderQty{
			5: {Accepted: 7, Traded: 7},
		},
	},
	// Conservation per order over a mix of every kind of exit.
	{
		name: "conservation holds per order across IOC, market and cancel",
		setup: []Command{
			limit(1, Sell, 100, 5), // Seq 1-2
			limit(2, Sell, 101, 5), // 3-4
			ioc(3, Buy, 100, 2),    // 5-6: takes 2 of #1
			limit(4, Buy, 99, 3),   // 7-8
			market(5, Sell, 1),     // 9-10: takes 1 of #4
			cancel(4),              // 11: removes #4's last 2
		},
		cmd: market(6, Buy, 12),
		want: []Event{
			evAccepted(12, 6, Buy, 0, 12),
			evTrade(13, 6, 1, Buy, 100, 3, 0),
			evTrade(14, 6, 2, Buy, 101, 5, 0),
			evIOCRemainder(15, 6, Buy, 0, 4),
		},
		wantGone: []OrderID{1, 2, 3, 4, 5, 6},
		wantQty: map[OrderID]orderQty{
			1: {Accepted: 5, Traded: 5},
			2: {Accepted: 5, Traded: 5},
			3: {Accepted: 2, Traded: 2},
			4: {Accepted: 3, Traded: 1, Canceled: 2},
			5: {Accepted: 1, Traded: 1},
			6: {Accepted: 12, Traded: 8, Canceled: 4},
		},
	},
}
