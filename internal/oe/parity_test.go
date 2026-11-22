package oe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sHyanorn/matching-engine/internal/api"
	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/engine"
	"github.com/sHyanorn/matching-engine/internal/flow"
)

// parityCommand is one command in the forms of both protocols. A cancel
// names the order of an earlier command by its index, or an ID outright.
type parityCommand struct {
	cancel bool
	target int          // index of the command whose order to cancel, or -1
	id     book.OrderID // the ID to cancel when target is -1

	// A new order, as REST names its fields and as OE1 numbers them.
	symbol            string
	symIdx            uint8
	side, typ, tif    string
	sideN, typN, tifN uint8
	price, qty        int64
}

func order(symIdx uint8, side book.Side, typ book.OrderType, tif book.TIF, price, qty int64) parityCommand {
	return parityCommand{
		target: -1, symbol: testSymbols[symIdx], symIdx: symIdx,
		side: side.String(), typ: typ.String(), tif: tif.String(), sideN: uint8(side), typN: uint8(typ), tifN: uint8(tif),
		price: price, qty: qty,
	}
}

func cancelOf(target int) parityCommand { return parityCommand{cancel: true, target: target} }

func cancelID(id book.OrderID) parityCommand { return parityCommand{cancel: true, target: -1, id: id} }

// parityCommands is a stretch of the SPEC §9 flow on each symbol, with a
// case of every kind of answer between them.
func parityCommands(n int) []parityCommand {
	var cmds []parityCommand
	edge := func(c ...parityCommand) { cmds = append(cmds, c...) }
	buy, sell, limit, market := book.Buy, book.Sell, book.Limit, book.Market
	bad := func(c parityCommand, f func(*parityCommand)) parityCommand { f(&c); return c }
	base := order(0, buy, limit, book.GTC, 100, 5)

	edge(
		order(0, sell, limit, book.GTC, 105, 10), order(0, sell, limit, book.GTC, 106, 10), order(1, buy, limit, book.GTC, 50, 3),
		order(0, buy, limit, book.IOC, 105, 4),   // trades, then the order is filled
		order(0, buy, limit, book.IOC, 104, 4),   // nothing to trade: the whole order is the remainder
		order(0, buy, limit, book.FOK, 105, 100), // cannot fill: rejected
		order(0, buy, limit, book.FOK, 106, 12),  // fills across two levels
		order(0, sell, market, book.IOC, 0, 7),   // takes the bid side, and the rest is canceled
		order(1, buy, market, book.FOK, 0, 1),    // market FOK on a side with nothing to trade
		bad(base, func(c *parityCommand) { c.side, c.sideN = "bogus", 9 }),
		bad(base, func(c *parityCommand) { c.typ, c.typN = "bogus", 9 }),
		bad(base, func(c *parityCommand) { c.tif, c.tifN = "bogus", 9 }),
		bad(base, func(c *parityCommand) { c.price = 0 }),
		bad(base, func(c *parityCommand) { c.price = int64(book.MaxPrice) + 1 }),
		bad(base, func(c *parityCommand) { c.qty = 0 }),
		bad(base, func(c *parityCommand) { c.qty = int64(book.MaxQty) + 1 }),
		order(0, buy, market, book.GTC, 0, 1), // market_gtc
		order(0, buy, market, book.IOC, 5, 1), // a market order with a price
		bad(base, func(c *parityCommand) { c.symbol, c.symIdx = "NOPE", 9 }),
		cancelOf(0), cancelOf(0), // the second finds it canceled
		cancelOf(3), // the order that was filled
		// The first order's counter with the other symbol's index: that
		// book rejects it.
		cancelID(1<<8|1),
		cancelID(book.OrderID(1<<40)<<8),
		cancelID(book.OrderID(1<<20)<<8|5), // a symbol index that is not configured
		cancelID(0),
	)
	for i, seed := range []uint64{1, 2} {
		g := flow.New(seed, 3)
		ids := []int{-1} // by the flow's order number: the index of the command
		for range n / 2 {
			op := g.Next()
			if op.Cancel {
				cmds = append(cmds, cancelOf(ids[op.Order]))
				continue
			}
			side := sell
			if op.Buy {
				side = buy
			}
			ids = append(ids, len(cmds))
			cmds = append(cmds, order(uint8(i), side, limit, book.GTC, op.Price, op.Qty))
		}
	}
	return cmds
}

// restReply is what a REST command answered.
type restReply struct {
	code   int
	errMsg string // the error code, if any
	order  struct {
		ID     book.OrderID `json:"order_id"`
		Status string       `json:"status"`
		Reason string       `json:"reason"`
	}
	events []restEvent
}

type restEvent struct {
	Seq         uint64       `json:"seq"`
	TS          int64        `json:"ts"`
	Type        string       `json:"type"`
	OrderID     book.OrderID `json:"order_id"`
	MakerID     book.OrderID `json:"maker_id"`
	Side        string       `json:"side"`
	Price       int64        `json:"price"`
	Qty         int64        `json:"qty"`
	Reason      string       `json:"reason"`
	LevelSide   string       `json:"level_side"`
	LevelQty    int64        `json:"level_qty"`
	LevelOrders uint32       `json:"level_orders"`
}

func restDo(t *testing.T, h http.Handler, method, path string, body any) restReply {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, rd))
	r := restReply{code: w.Code}
	if w.Code != http.StatusOK {
		var e struct{ Error struct{ Code string } }
		if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body)
		}
		r.errMsg = e.Error.Code
		return r
	}
	var b struct {
		Order *struct {
			ID     book.OrderID `json:"order_id"`
			Status string       `json:"status"`
			Reason string       `json:"reason"`
		} `json:"order"`
		Events []restEvent `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil || b.Order == nil {
		t.Fatalf("%s %s: %v: %s", method, path, err, w.Body)
	}
	r.order.ID, r.order.Status, r.order.Reason, r.events = b.Order.ID, b.Order.Status, b.Order.Reason, b.Events
	return r
}

// counters returns the samples of the orders, cancels and trades counters
// that h serves on /metrics: each value as it is written, by its series.
func counters(t *testing.T, h http.Handler) map[string]string {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /metrics: status %d", w.Code)
	}
	samples := map[string]string{}
	for line := range strings.SplitSeq(w.Body.String(), "\n") {
		series, value, _ := strings.Cut(line, " ")
		for _, name := range []string{"matching_engine_orders_total", "matching_engine_cancels_total", "matching_engine_trades_total"} {
			if strings.HasPrefix(series, name+"{") {
				samples[series] = value
			}
		}
	}
	return samples
}

// TestMatchesREST sends the same commands to two engines, one over REST
// and one over OE1, and checks that every reply says the same, event for
// event, that the two command logs are the same bytes, and that the orders,
// cancels and trades counters of the two end the same.
func TestMatchesREST(t *testing.T) {
	n := 4_000
	if testing.Short() {
		n = 400
	}
	clock := func() func() int64 {
		var now atomic.Int64
		return func() int64 { return now.Add(1) }
	}
	dirA, dirB := t.TempDir(), t.TempDir()
	a, err := engine.New(engine.Config{Symbols: testSymbols, DataDir: dirA, Clock: clock()})
	if err != nil {
		t.Fatal(err)
	}
	b, err := engine.New(engine.Config{Symbols: testSymbols, DataDir: dirB, Clock: clock()})
	if err != nil {
		t.Fatal(err)
	}
	h := api.New(a, api.Options{}).Handler()
	counts := api.New(b, api.Options{})
	srv, addr := startServer(t, ServerOptions{Engine: b, Counts: counts, Symbols: b.Symbols()})
	c := dial(t, addr)
	c.login()

	var ids []book.OrderID // the order each command made, 0 for none
	for i, cmd := range parityCommands(n) {
		id := cmd.id
		if cmd.target >= 0 {
			id = ids[cmd.target]
		}
		var rest restReply
		if cmd.cancel {
			rest = restDo(t, h, http.MethodDelete, fmt.Sprintf("/v1/orders/%d", id), nil)
			if id == 0 && rest.errMsg != "invalid_order_id" {
				t.Fatalf("command %d: REST answered a cancel of ID 0 with %+v", i, rest)
			}
		} else {
			rest = restDo(t, h, http.MethodPost, "/v1/orders", map[string]any{
				"symbol": cmd.symbol, "side": cmd.side, "type": cmd.typ, "tif": cmd.tif, "price": cmd.price, "qty": cmd.qty,
			})
		}

		seq := uint64(i + 1)
		var f frame = Cancel{ClientSeq: seq, OrderID: id}
		if !cmd.cancel {
			f = NewOrder{ClientSeq: seq, Symbol: cmd.symIdx, Side: book.Side(cmd.sideN), Type: book.OrderType(cmd.typN), TIF: book.TIF(cmd.tifN), Price: book.Price(cmd.price), Qty: book.Qty(cmd.qty)}
		}
		c.send(f)
		var evs []Event
		var done Done
		for {
			fr, err := c.next()
			if err != nil {
				t.Fatalf("command %d: %v", i, err)
			}
			if e, ok := fr.(Event); ok {
				evs = append(evs, e)
				continue
			}
			done = fr.(Done)
			break
		}
		compareReplies(t, i, cmd, rest, evs, done)
		if !cmd.cancel {
			ids = append(ids, done.OrderID)
			if rest.code == http.StatusOK && done.OrderID != rest.order.ID {
				t.Fatalf("command %d: order %d over OE1, %d over REST", i, done.OrderID, rest.order.ID)
			}
		} else {
			ids = append(ids, 0)
		}
	}
	rest, oe1 := counters(t, h), counters(t, counts.Handler())
	if !maps.Equal(rest, oe1) {
		t.Errorf("the counters differ:\nREST %v\nOE1  %v", rest, oe1)
	}
	for _, series := range []string{
		`matching_engine_orders_total{result="accepted",symbol="BTC-USD"}`,
		`matching_engine_orders_total{result="rejected",symbol="BTC-USD"}`,
		`matching_engine_orders_total{result="invalid",symbol="BTC-USD"}`,
		`matching_engine_cancels_total{result="canceled",symbol="BTC-USD"}`,
		`matching_engine_cancels_total{result="finished",symbol="BTC-USD"}`,
		`matching_engine_cancels_total{result="unknown",symbol="ETH-USD"}`,
		`matching_engine_trades_total{symbol="BTC-USD"}`,
	} {
		if v := rest[series]; v == "" || v == "0" {
			t.Errorf("%s is %q over REST: the commands do not reach it", series, v)
		}
	}
	srv.BeginShutdown()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	for _, s := range testSymbols {
		la, errA := os.ReadFile(filepath.Join(dirA, s+".log"))
		lb, errB := os.ReadFile(filepath.Join(dirB, s+".log"))
		if errA != nil || errB != nil {
			t.Fatal(errA, errB)
		}
		if !bytes.Equal(la, lb) {
			t.Errorf("%s: the command logs differ (%d and %d bytes)", s, len(la), len(lb))
		}
	}
}

func compareReplies(t *testing.T, i int, cmd parityCommand, rest restReply, evs []Event, done Done) {
	t.Helper()
	fail := func(format string, args ...any) {
		t.Fatalf("command %d (%+v): %s\nREST %+v\nOE1  %+v %+v", i, cmd, fmt.Sprintf(format, args...), rest, evs, done)
	}
	var want Status
	switch {
	case rest.code == http.StatusOK:
		want = StatusOK
	case rest.code == http.StatusBadRequest:
		want = StatusInvalid
		if done.Reason.String() != rest.errMsg {
			fail("reason %v, REST %q", done.Reason, rest.errMsg)
		}
	case rest.errMsg == "unknown_symbol":
		want = StatusUnknownSymbol
	case rest.errMsg == "unknown_order":
		want = StatusUnknownOrder
	case rest.code == http.StatusConflict:
		want = StatusOrderFinished
	default:
		fail("REST answered %d %s", rest.code, rest.errMsg)
	}
	if done.Status != want {
		fail("status %v, want %v", done.Status, want)
	}
	if len(evs) != len(rest.events) {
		fail("%d events, REST %d", len(evs), len(rest.events))
	}
	name := func(s book.Side) string {
		if s == 0 {
			return ""
		}
		return s.String()
	}
	reason := func(r book.Reason) string {
		if r == 0 {
			return ""
		}
		return r.String()
	}
	for k, e := range evs {
		w := rest.events[k]
		got := restEvent{Seq: e.Seq, TS: e.Timestamp, Type: e.Kind.String(), OrderID: e.OrderID, MakerID: e.MakerID,
			Side: name(e.Side), Price: int64(e.Price), Qty: int64(e.Qty), Reason: reason(e.Reason),
			LevelSide: name(e.LevelSide), LevelQty: int64(e.LevelQty), LevelOrders: e.LevelOrders}
		if got != w {
			fail("event %d: %+v, REST %+v", k, got, w)
		}
		if e.ClientSeq != uint64(i+1) {
			fail("event %d has client_seq %d", k, e.ClientSeq)
		}
	}
	if want == StatusOK {
		if done.State.String() != rest.order.Status || reason(done.Reason) != rest.order.Reason {
			fail("order %v %q, REST %q %q", done.State, done.Reason, rest.order.Status, rest.order.Reason)
		}
	}
	if want == StatusInvalid && !cmd.cancel && done.OrderID == 0 {
		fail("an invalid new order has the ID it took, not 0")
	}
}

// TestSessionSeesOnlyItsOwnEvents: a client whose order is filled by
// another client's gets no frame for it.
func TestSessionSeesOnlyItsOwnEvents(t *testing.T) {
	e, err := engine.New(engine.Config{Symbols: testSymbols, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() }) // after the server closes
	_, addr := startServer(t, ServerOptions{Engine: e, Symbols: e.Symbols()})
	a, b := dial(t, addr), dial(t, addr)
	a.login()
	b.login()

	a.send(NewOrder{ClientSeq: 1, Symbol: 0, Side: book.Sell, Type: book.Limit, TIF: book.GTC, Price: 100, Qty: 5})
	var rested uint64
	for {
		f, err := a.next()
		if err != nil {
			t.Fatal(err)
		}
		if d, ok := f.(Done); ok {
			rested = uint64(d.OrderID)
			break
		}
	}
	b.send(NewOrder{ClientSeq: 1, Symbol: 0, Side: book.Buy, Type: book.Limit, TIF: book.IOC, Price: 100, Qty: 5})
	trades := 0
	for {
		f, err := b.next()
		if err != nil {
			t.Fatal(err)
		}
		if ev, ok := f.(Event); ok && ev.Kind == book.EventTrade {
			trades++
			if uint64(ev.MakerID) != rested {
				t.Errorf("B traded against order %d, want A's %d", ev.MakerID, rested)
			}
		}
		if _, ok := f.(Done); ok {
			break
		}
	}
	if trades != 1 {
		t.Fatalf("B saw %d trades, want 1", trades)
	}
	a.send(Ping{ClientSeq: 2}) // anything for A would come before this Pong
	a.expect(Pong{ClientSeq: 2})
}
