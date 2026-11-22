package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/sHyanorn/matching-engine/internal/book"
)

// fakeStream is the HTTP side of an engine as loadgen reads it: /metrics
// with a trades counter, and the stream of trades, which pushes each trade
// fed to trade, held back by delay. It raises the counter by perTrade for
// each trade, and tamper, if set, changes the pushes of each trade.
type fakeStream struct {
	addr     string
	delay    time.Duration
	perTrade int
	tamper   func(n int, msg []byte) [][]byte

	// probe, if set, is asked what to note at each read of /metrics.
	probe func() [2]int64

	mu      sync.Mutex
	n       int // trades so far
	counter int
	reads   [][2]int64
	pushes  chan timedPush
}

type timedPush struct {
	at  time.Time
	msg []byte
}

func newFakeStream(t *testing.T) *fakeStream {
	t.Helper()
	s := &fakeStream{perTrade: 1, pushes: make(chan timedPush, 1<<16)}
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", s.metrics)
	mux.HandleFunc("/v1/stream", s.stream)
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)
	s.addr = strings.TrimPrefix(hs.URL, "http://")
	return s
}

func (s *fakeStream) metrics(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.probe != nil {
		s.reads = append(s.reads, s.probe())
	}
	_, _ = fmt.Fprintf(w, "matching_engine_trades_total{symbol=%q} %d\n", fakeSymbol, s.counter)
}

// stream takes the subscription and then pushes what trade is given.
func (s *fakeStream) stream(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = c.CloseNow() }()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	if _, _, err := c.Read(ctx); err != nil { // the subscribe
		return
	}
	if c.Write(ctx, websocket.MessageText, []byte(`{"type":"subscribed","channel":"trades:`+fakeSymbol+`"}`)) != nil {
		return
	}
	go func() { // takes the close
		for {
			if _, _, err := c.Read(ctx); err != nil {
				cancel()
				return
			}
		}
	}()
	for {
		select {
		case p := <-s.pushes:
			time.Sleep(time.Until(p.at.Add(s.delay)))
			if c.Write(ctx, websocket.MessageText, p.msg) != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// trade is what fakeOE calls for each trade Event: it counts it and queues
// its push, numbered as a channel numbers them.
func (s *fakeStream) trade(e book.Event) {
	s.mu.Lock()
	s.n++
	n := s.n
	s.counter += s.perTrade
	s.mu.Unlock()
	msg, err := json.Marshal(map[string]any{
		"type": "trade", "seq": n, "book_seq": e.Seq, "price": e.Price, "qty": e.Qty,
		"taker_order_id": e.OrderID, "maker_order_id": e.MakerID,
	})
	if err != nil {
		panic(err)
	}
	out := [][]byte{msg}
	if s.tamper != nil {
		out = s.tamper(n, msg)
	}
	at := time.Now()
	for _, m := range out {
		s.pushes <- timedPush{at, m}
	}
}

// state returns the trades so far, the counter, and what probe noted.
func (s *fakeStream) state() (n, counter int, reads [][2]int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n, s.counter, slices.Clone(s.reads)
}

// trades returns how many trades the fake has been given.
func (s *fakeStream) trades() int {
	n, _, _ := s.state()
	return n
}

// bump raises the counter without a trade.
func (s *fakeStream) bump() {
	s.mu.Lock()
	s.counter++
	s.mu.Unlock()
}

// pushConfig is the config of flow T against a fake and its fakeStream.
func pushConfig(dial func() (net.Conn, error), s *fakeStream) config {
	cfg := oeConfig(dial)
	cfg.orders, cfg.push, cfg.symbol, cfg.seed, cfg.sd = true, true, fakeSymbol, 7, 5
	cfg.httpAddr, cfg.pushWait = s.addr, 300*time.Millisecond
	return cfg
}

// A correct run of flow T is valid, counts the trades of the whole run the
// same from the replies, the pushes and the counter, and has a trade_push
// sample for each order of the window that traded.
func TestOEPushRun(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	s := newFakeStream(t)
	rep, err := measureOE(context.Background(), pushConfig(listenOE(t, fakeOE{trade: s.trade}), s))
	if err != nil {
		t.Fatal(err)
	}
	rateAside(t, rep)
	tc, n := rep.Trades, s.trades()
	if tc.Replies < 100 || tc.Pushes != tc.Replies || tc.Metric != tc.Replies || tc.Replies != n {
		t.Errorf("trades %+v for %d trades of the fake, want equal counts and over 100", tc, n)
	}
	if n, want := rep.Latency["trade_push"].Count, rep.Outcomes["order_traded"]; n != want || n == 0 {
		t.Errorf("%d trade_push samples, want one for each of the %d orders of the window that traded", n, want)
	}
	for _, k := range []string{"reply", "service", "send_lag"} {
		if rep.Latency[k].Count == 0 {
			t.Errorf("no %s samples: the run of flow T must report the rows of R3 too", k)
		}
	}
}

// A push that is missing, repeated, out of order or altered makes the run
// invalid, as it does in the REST mode: the same cases, with the same
// problems.
func TestOEPushCheckCatchesBadPushes(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	const k = 20 // the push to spoil
	for _, tc := range badPushes(k) {
		t.Run(tc.name, func(t *testing.T) {
			s := newFakeStream(t)
			s.tamper = func(n int, msg []byte) [][]byte { return tc.tamper(t, n, msg) }
			rep, err := measureOE(context.Background(), pushConfig(listenOE(t, fakeOE{trade: s.trade}), s))
			if err != nil {
				t.Fatal(err)
			}
			if rep.Trades.Replies <= k {
				t.Fatalf("only %d trades, want over %d", rep.Trades.Replies, k)
			}
			if rep.Valid || !problemWith(rep, tc.want) {
				t.Errorf("valid %v, problems %q; want a problem containing %q", rep.Valid, rep.Problems, tc.want)
			}
		})
	}
}

// An order that traded and got no push at all makes the run invalid, however
// the counts come out.
func TestOEPushMissingForAnOrder(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	s := newFakeStream(t)
	s.tamper = func(n int, msg []byte) [][]byte {
		if n == 60 { // after the warmup's trades, about 35
			return nil
		}
		return [][]byte{msg}
	}
	rep, err := measureOE(context.Background(), pushConfig(listenOE(t, fakeOE{trade: s.trade}), s))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Valid || !problemWith(rep, "1 orders that traded got no trade push") {
		t.Errorf("valid %v, problems %q; want invalid by an order that got no push", rep.Valid, rep.Problems)
	}
}

// trade_push is the time from an order's due time to the first push for it,
// whether the push comes before the order's Done or after it, and counted
// from the due time, not from the send.
func TestOEPushIsTimedFromTheDueTime(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	if runtime.GOOS == "windows" {
		t.Skip("Go's clock on Windows advances once per tick (bench-results/m6-windows)")
	}
	for _, c := range []struct {
		name                string
		doneDelay, pushHold time.Duration // the server holds back each Done, and each push
		writeHold           time.Duration // the pacer holds back each write
		minimum, belowP50   float64       // of the trade_push samples, in µs; 0: not checked
	}{
		// The push goes out as the order arrives and the Done 5 ms later.
		{"push before the Done", 5 * time.Millisecond, 0, 0, 0, 4000},
		// The Done goes out as the order arrives and the push 30 ms later.
		{"push after the Done", 0, 30 * time.Millisecond, 0, 30_000, 0},
		// Each write leaves 3 ms after the orders in it are due, and the
		// push comes after the write.
		{"orders sent late", 0, 0, 3 * time.Millisecond, 3000, 0},
	} {
		s := newFakeStream(t)
		s.delay = c.pushHold
		cfg := pushConfig(listenOE(t, fakeOE{delay: c.doneDelay, trade: s.trade}), s)
		if c.writeHold != 0 {
			cfg.beforeSend = func(int) { time.Sleep(c.writeHold) }
		}
		rep, err := measureOE(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		rateAside(t, rep)
		push := rep.Latency["trade_push"]
		if push.Count == 0 || push.Count != rep.Outcomes["order_traded"] || rep.Trades.Pushes != rep.Trades.Replies {
			t.Errorf("%s: %d trade_push samples for %d orders that traded, trades %+v", c.name, push.Count, rep.Outcomes["order_traded"], rep.Trades)
			continue
		}
		if least := push.Curve[0][1]; least < c.minimum {
			t.Errorf("%s: the least trade_push %.0f µs, want at least %.0f", c.name, least, c.minimum)
		}
		if c.belowP50 != 0 && push.P50 > c.belowP50 {
			t.Errorf("%s: trade_push p50 %.0f µs, want under %.0f: the push came before the Done", c.name, push.P50, c.belowP50)
		}
	}
}

// With -http the counter is read before the session opens and after it has
// closed, and never while it is open; the rise is compared with the trades
// in the replies.
func TestOECounterCheck(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	for _, c := range []struct {
		name     string
		perTrade int
		want     string // in a problem; "" for a valid run
	}{
		{"a rise equal to the trades", 1, ""},
		{"a rise below the trades", 0, "matching_engine_trades_total rose by 0 for "},
		{"a rise above the trades", 2, "matching_engine_trades_total rose by "},
	} {
		s := newFakeStream(t)
		s.perTrade = c.perTrade
		var open atomic.Int32
		var taken atomic.Int64
		s.probe = func() [2]int64 { return [2]int64{int64(open.Load()), taken.Load()} }
		f := fakeOE{trade: s.trade, sessions: &open, reply: func(any, *fakeAnswer) { taken.Add(1) }}
		cfg := ordersConfig(listenOE(t, f))
		cfg.httpAddr = s.addr
		rep, err := measureOE(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		if c.want == "" {
			rateAside(t, rep)
		} else if rep.Valid || !problemWith(rep, c.want) {
			t.Errorf("%s: valid %v, problems %q; want invalid by %q", c.name, rep.Valid, rep.Problems, c.want)
		}
		n, counter, reads := s.state()
		if tc := rep.Trades; tc.Replies != n || tc.Replies == 0 || tc.Metric != counter || tc.Pushes != 0 {
			t.Errorf("%s: trades %+v for %d trades of the fake and a counter at %d, want replies and metric from them and no pushes", c.name, tc, n, counter)
		}
		if _, ok := rep.Latency["trade_push"]; ok {
			t.Errorf("%s: -http alone reports trade_push", c.name)
		}
		// Before the first command, and after the last, with no session open.
		if want := [][2]int64{{0, 0}, {0, taken.Load()}}; !slices.Equal(reads, want) || taken.Load() == 0 {
			t.Errorf("%s: /metrics read with (sessions open, commands taken) %v, want %v", c.name, reads, want)
		}
	}
}

// Without -http nothing is read.
func TestOENoCounterWithoutHTTP(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	s := newFakeStream(t)
	s.probe = func() [2]int64 { return [2]int64{} }
	rep, err := measureOE(context.Background(), ordersConfig(listenOE(t, fakeOE{trade: s.trade})))
	if err != nil {
		t.Fatal(err)
	}
	rateAside(t, rep)
	if _, _, reads := s.state(); len(reads) != 0 || rep.Trades.Metric != 0 || rep.Trades.Replies == 0 {
		t.Errorf("%d reads of /metrics, counter rise %d for %d trades; want no read and no rise", len(reads), rep.Trades.Metric, rep.Trades.Replies)
	}
}

// R1 and R2 have no trades, so a counter that rises anyway, or that cannot be
// read, makes the run invalid.
func TestOEFlowsCheckTheCounter(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	for name, flow := range map[string]func(func() (net.Conn, error)) config{"R1": iocConfig, "R2": ackConfig} {
		s := newFakeStream(t)
		cfg := flow(listenOE(t, fakeOE{}))
		cfg.httpAddr = s.addr
		rep, err := measureOE(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		rateAside(t, rep)
		if rep.Trades != (tradeCounts{}) {
			t.Errorf("%s: trades %+v, want none", name, rep.Trades)
		}

		// The counter rises by one during the run.
		s = newFakeStream(t)
		cfg = flow(listenOE(t, fakeOE{}))
		cfg.httpAddr = s.addr
		bumped := false
		cfg.beforeSend = func(i int) { // i may skip 5, if the pacer is late
			if i >= 5 && !bumped {
				bumped = true
				s.bump()
			}
		}
		rep, err = measureOE(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		if want := "matching_engine_trades_total rose by 1 for 0 trades in the replies"; rep.Valid || !problemWith(rep, want) {
			t.Errorf("%s: valid %v, problems %q; want invalid by %q", name, rep.Valid, rep.Problems, want)
		}
	}

	// An address that answers nothing: the run is made, and invalid for it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	cfg := iocConfig(listenOE(t, fakeOE{}))
	cfg.httpAddr = addr
	rep, err := measureOE(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Valid || !problemWith(rep, "reading matching_engine_trades_total: ") || rep.Latency["reply"].Count == 0 {
		t.Errorf("valid %v, problems %q, %d replies; want a run invalid for the counter it could not read", rep.Valid, rep.Problems, rep.Latency["reply"].Count)
	}
}

// With -push, a stream that cannot be subscribed to stops the run before the
// session opens.
func TestOEPushNeedsTheStream(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	cfg := pushConfig(nil, &fakeStream{addr: addr})
	cfg.oeDial = func() (net.Conn, error) { t.Error("the session was opened"); return nil, net.ErrClosed }
	_, err = measureOE(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "subscribing to trades:"+fakeSymbol) {
		t.Errorf("measureOE = %v, want an error about subscribing to trades:%s", err, fakeSymbol)
	}
}
