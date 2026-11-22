package main

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sHyanorn/matching-engine/internal/api"
	"github.com/sHyanorn/matching-engine/internal/oe"
)

// serveHTTP serves the HTTP side of the engine that serveEngine started,
// whose api.Server also counts the OE1 commands, and returns its address.
func serveHTTP(t *testing.T, srv *api.Server) string {
	t.Helper()
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { // before the engine's, which waits for the streams
		srv.CloseStreams()
		hs.Close()
	})
	return strings.TrimPrefix(hs.URL, "http://")
}

// wantMetrics checks that srv's /metrics holds each series at its value.
func wantMetrics(t *testing.T, srv *api.Server, series map[string]int) {
	t.Helper()
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for name, want := range series {
		if sample := fmt.Sprintf("%s %d\n", name, want); !strings.Contains(w.Body.String(), sample) {
			t.Errorf("/metrics lacks %q", strings.TrimSpace(sample))
		}
	}
}

// engineFlowConfig is the config of a run of one flow against the engine at
// addr, whose HTTP side is at httpAddr, and the number of orders the run
// sends.
func engineFlowConfig(addr, httpAddr string) (config, int) {
	cfg := config{oeAddr: addr, httpAddr: httpAddr, symbol: "BTC-USD", seed: 1, sd: 5,
		rate: 2000, warmup: 200 * time.Millisecond, duration: time.Second, spin: true, maxSendLag: time.Hour}
	return cfg, int(cfg.rate * (cfg.warmup + cfg.duration).Seconds())
}

// R1 against the engine: every order is answered ok, canceled with the IOC
// remainder after the two Events of an order that does not trade, and the
// book, which has had only IOC buys against no ask, is left empty; the
// counters of the engine are loadgen's own.
func TestOEIOCAgainstEngine(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	eng, counts, addr := serveEngine(t, oe.ServerOptions{})
	cfg, total := engineFlowConfig(addr, serveHTTP(t, counts))
	cfg.ioc = true
	rep, err := measureOE(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	rateAside(t, rep)
	window := int(cfg.rate * cfg.duration.Seconds())
	if want := map[string]int{"order_canceled": window}; !maps.Equal(rep.Outcomes, want) {
		t.Errorf("outcomes %v, want %v", rep.Outcomes, want)
	}
	for _, k := range []string{"reply", "service", "send_lag"} {
		if n := rep.Latency[k].Count; n != window {
			t.Errorf("%d %s samples, want one per order of the window, %d", n, k, window)
		}
	}
	if rep.Trades != (tradeCounts{}) || rep.Mode != "oe_ioc" {
		t.Errorf("mode %q, trades %+v, want oe_ioc and no trades", rep.Mode, rep.Trades)
	}
	depth, err := eng.Depth(context.Background(), "BTC-USD", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(depth.Bids)+len(depth.Asks) != 0 || depth.LastSeq != uint64(2*total) {
		t.Errorf("book with %d bids and %d asks at event %d, want it empty at event %d: accepted and canceled for each of %d orders",
			len(depth.Bids), len(depth.Asks), depth.LastSeq, 2*total, total)
	}
	wantMetrics(t, counts, map[string]int{
		`matching_engine_orders_total{result="accepted",symbol="BTC-USD"}`:  total,
		`matching_engine_cancels_total{result="canceled",symbol="BTC-USD"}`: 0,
		`matching_engine_trades_total{symbol="BTC-USD"}`:                    0,
	})
}

// R2 against the engine: every order rests and is canceled, the book is left
// empty, and the orders, cancels and trades the engine counted are what
// loadgen counted.
func TestOEAckCancelAgainstEngine(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	eng, counts, addr := serveEngine(t, oe.ServerOptions{})
	cfg, total := engineFlowConfig(addr, serveHTTP(t, counts))
	cfg.ackCancel = true
	rep, err := measureOE(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	rateAside(t, rep)
	window := int(cfg.rate * cfg.duration.Seconds())
	if want := map[string]int{"order_rested": window, "cancel_canceled": window}; !maps.Equal(rep.Outcomes, want) || rep.Cancels != window {
		t.Errorf("outcomes %v and %d Cancels sent, want %v and %d", rep.Outcomes, rep.Cancels, want, window)
	}
	for _, k := range []string{"reply", "cancel", "order_to_cancel", "send_lag"} {
		if n := rep.Latency[k].Count; n != window {
			t.Errorf("%d %s samples, want one per order of the window, %d", n, k, window)
		}
	}
	if rep.Trades != (tradeCounts{}) || rep.Mode != "oe_ack_cancel" {
		t.Errorf("mode %q, trades %+v, want oe_ack_cancel and no trades", rep.Mode, rep.Trades)
	}
	depth, err := eng.Depth(context.Background(), "BTC-USD", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(depth.Bids)+len(depth.Asks) != 0 || depth.LastSeq != uint64(3*total) {
		t.Errorf("book with %d bids and %d asks at event %d, want it empty at event %d: accepted and rested, then canceled, for each of %d orders",
			len(depth.Bids), len(depth.Asks), depth.LastSeq, 3*total, total)
	}
	wantMetrics(t, counts, map[string]int{
		`matching_engine_orders_total{result="accepted",symbol="BTC-USD"}`:  total,
		`matching_engine_cancels_total{result="canceled",symbol="BTC-USD"}`: total,
		`matching_engine_trades_total{symbol="BTC-USD"}`:                    0,
	})
}

// Flow T against the engine: its trade pushes arrive once, in order, and as
// many as the trade Events in the replies and the rise of the counter, and
// each order of the window that traded has a push to time.
func TestOEPushAgainstEngine(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	_, counts, addr := serveEngine(t, oe.ServerOptions{})
	cfg, _ := engineFlowConfig(addr, serveHTTP(t, counts))
	cfg.orders, cfg.push = true, true
	rep, err := measureOE(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	rateAside(t, rep)
	tc := rep.Trades
	if tc.Replies < rep.Outcomes["order_traded"] || tc.Replies == 0 || tc.Pushes != tc.Replies || tc.Metric != tc.Replies {
		t.Errorf("trades %+v for %d orders that traded, want equal counts and at least one trade for each order", tc, rep.Outcomes["order_traded"])
	}
	if n := rep.Latency["trade_push"].Count; n != rep.Outcomes["order_traded"] {
		t.Errorf("%d trade_push samples, want one per order that traded, %d", n, rep.Outcomes["order_traded"])
	}
	wantMetrics(t, counts, map[string]int{`matching_engine_trades_total{symbol="BTC-USD"}`: tc.Replies})
}
