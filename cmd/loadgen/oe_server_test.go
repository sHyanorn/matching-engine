package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sHyanorn/matching-engine/internal/api"
	"github.com/sHyanorn/matching-engine/internal/engine"
	"github.com/sHyanorn/matching-engine/internal/oe"
)

// TestOEAgainstServer runs the OE1 mode against the engine's session
// server, as the loopback runs do.
func TestOEAgainstServer(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := oe.NewServer(oe.ServerOptions{Symbols: []string{"BTC-USD"}})
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	cfg := config{oeAddr: ln.Addr().String(), rate: 1000, warmup: 200 * time.Millisecond, duration: time.Second,
		spin: true, maxSendLag: time.Hour}
	run, err := runOE(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := run.report()
	rateAside(t, r)
	if r.Outcomes["ping"] != 1000 || r.Outcomes["pong"] != 1000 || r.Latency["pong"].Count != 1000 {
		t.Errorf("outcomes %v, %d round trips; want 1,000 Pings answered", r.Outcomes, r.Latency["pong"].Count)
	}
	// The hiccup samples are the pacer's gaps at every write of the window.
	// With every Ping answered, those are the gaps at the answered Pings,
	// which is what the report gave before the order flow.
	var gaps []int64
	for j := range run.total {
		if d := run.due(j); d >= int64(cfg.warmup) && run.got[j] && run.gap[j] != 0 {
			gaps = append(gaps, run.gap[j])
		}
	}
	if want := summarize(gaps); !reflect.DeepEqual(r.Latency["hiccup"], want) || want.Count == 0 {
		t.Errorf("hiccup %+v, want the gaps at the answered Pings, %+v", r.Latency["hiccup"], want)
	}
}

// serveEngine starts an engine with one symbol and an OE1 server for it,
// and returns the engine, the api.Server that counts the server's commands
// and the server's address.
func serveEngine(t *testing.T, opts oe.ServerOptions) (*engine.Engine, *api.Server, string) {
	t.Helper()
	eng, err := engine.New(engine.Config{Symbols: []string{"BTC-USD"}, DataDir: t.TempDir(), SyncInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	counts := api.New(eng, api.Options{})
	opts.Engine, opts.Counts, opts.Symbols = eng, counts, eng.Symbols()
	srv := oe.NewServer(opts)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		if err := eng.Close(); err != nil {
			t.Error(err)
		}
	})
	return eng, counts, ln.Addr().String()
}

// TestOEOrdersAgainstEngine sends the order flow to the engine over its
// session server and checks what loadgen counted against the book and
// against the engine's counters.
func TestOEOrdersAgainstEngine(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	eng, counts, addr := serveEngine(t, oe.ServerOptions{})
	cfg := config{oeAddr: addr, orders: true, symbol: "BTC-USD", seed: 1, sd: 5,
		rate: 2000, warmup: 200 * time.Millisecond, duration: time.Second, spin: true, maxSendLag: time.Hour}
	run, err := runOE(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := run.report()
	rateAside(t, r)
	offered, answered := 0, 0
	for k, n := range r.Outcomes {
		offered += n
		if k != "cancel_skipped" {
			answered += n
		}
	}
	if offered != 2000 {
		t.Errorf("outcomes %v add up to %d, want the 2,000 requests of the window", r.Outcomes, offered)
	}
	for _, k := range []string{"order_rested", "order_traded", "cancel_canceled", "cancel_finished"} {
		if r.Outcomes[k] == 0 {
			t.Errorf("no %s in %v", k, r.Outcomes)
		}
	}
	for _, k := range []string{"reply", "service", "send_lag"} {
		if n := r.Latency[k].Count; n != answered {
			t.Errorf("%d %s samples, want one per command of the window, %d", n, k, answered)
		}
	}
	if r.Trades.Replies < r.Outcomes["order_traded"] {
		t.Errorf("%d trades in the replies of at least %d orders that traded", r.Trades.Replies, r.Outcomes["order_traded"])
	}

	dones, bare := 0, 0 // of the whole run
	tally := map[outcome]int{}
	for _, out := range run.res[:run.nsent] {
		tally[out]++
		if out != noDone {
			dones++
		}
		if out == cancelUnknown || out == cancelFinished {
			bare++
		}
	}
	depth, err := eng.Depth(context.Background(), "BTC-USD", 1)
	if err != nil {
		t.Fatal(err)
	}
	// The book numbers its events from 1 and the session sends each as an
	// Event, numbered with the Dones, but for the one rejection behind a
	// cancel answered unknown_order or order_finished.
	if events := run.serverSeq - uint64(dones); events+uint64(bare) != depth.LastSeq || dones != run.nsent {
		t.Errorf("%d Events and %d cancels answered without one, for %d of %d commands; the book is at event %d",
			events, bare, dones, run.nsent, depth.LastSeq)
	}

	// Every Done is in, so every command of the run is counted.
	w := httptest.NewRecorder()
	counts.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for series, want := range map[string]int{
		`matching_engine_orders_total{result="accepted",symbol="BTC-USD"}`:  tally[orderRested] + tally[orderTraded],
		`matching_engine_cancels_total{result="canceled",symbol="BTC-USD"}`: tally[cancelCanceled],
		`matching_engine_cancels_total{result="finished",symbol="BTC-USD"}`: tally[cancelFinished],
		`matching_engine_cancels_total{result="unknown",symbol="BTC-USD"}`:  tally[cancelUnknown],
		`matching_engine_trades_total{symbol="BTC-USD"}`:                    run.trades,
	} {
		if sample := fmt.Sprintf("%s %d\n", series, want); !strings.Contains(w.Body.String(), sample) {
			t.Errorf("/metrics lacks %q, loadgen's own count of the run", strings.TrimSpace(sample))
		}
	}
}

// An engine without the symbol is found out at the login, before any load.
func TestOEOrdersUnknownSymbol(t *testing.T) {
	// The server's Heartbeat is the first frame after its Symbol frames.
	_, _, addr := serveEngine(t, oe.ServerOptions{Heartbeat: 10 * time.Millisecond})
	cfg := config{oeAddr: addr, orders: true, symbol: "ETH-USD", rate: 1000, duration: time.Second}
	_, err := measureOE(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), `the engine has no symbol "ETH-USD"`) {
		t.Errorf("measureOE = %v, want an error naming the symbol the engine lacks", err)
	}
}
