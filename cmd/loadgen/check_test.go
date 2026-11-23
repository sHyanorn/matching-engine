package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// A counter that could not be read after the run gives no rise: the report
// keeps the metric at 0, where the rise would be minus the first read.
func TestTradeProblemsLeavesTheMetricAtZeroWithoutTheRead(t *testing.T) {
	var l loader
	counts, problems := l.tradeProblems(tradeSum{n: 3}, false, 0, nil, 3, nil)
	if counts.Metric != 3 || len(problems) != 0 {
		t.Errorf("a counter that rose by 3 for 3 trades: metric %d, problems %q", counts.Metric, problems)
	}
	counts, problems = l.tradeProblems(tradeSum{n: 3}, false, 0, nil, -7, errors.New("gone"))
	if counts.Metric != 0 || len(problems) != 1 || !strings.Contains(problems[0], "reading matching_engine_trades_total: gone") {
		t.Errorf("a counter that could not be read: metric %d, problems %q", counts.Metric, problems)
	}
}

// tamperProxy relays HTTP to the engine at addr unchanged, and the
// WebSocket stream through tamper, which gets each trade push numbered from
// 1 and returns the messages to send in its place.
func tamperProxy(t *testing.T, addr string, tamper func(n int, msg []byte) [][]byte) string {
	t.Helper()
	upstream := &url.URL{Scheme: "http", Host: addr}
	rp := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(upstream) }}
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/stream" {
			rp.ServeHTTP(w, r)
			return
		}
		client, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = client.CloseNow() }()
		server, _, err := websocket.Dial(r.Context(), "ws://"+addr+"/v1/stream", nil)
		if err != nil {
			return
		}
		defer func() { _ = server.CloseNow() }()
		ctx := r.Context()
		go func() { // client to server, unchanged
			for {
				typ, data, err := client.Read(ctx)
				if err != nil || server.Write(ctx, typ, data) != nil {
					_ = server.CloseNow()
					return
				}
			}
		}()
		n := 0
		for {
			typ, data, err := server.Read(ctx)
			if err != nil {
				_ = client.Close(websocket.StatusNormalClosure, "")
				return
			}
			out := [][]byte{data}
			if bytes.Contains(data, []byte(`"type":"trade"`)) {
				n++
				out = tamper(n, data)
			}
			for _, m := range out {
				if client.Write(ctx, typ, m) != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(hs.Close)
	return strings.TrimPrefix(hs.URL, "http://")
}

// edit returns msg with field set to v.
func edit(t *testing.T, msg []byte, field string, v any) []byte {
	t.Helper()
	var m map[string]any
	d := json.NewDecoder(bytes.NewReader(msg))
	d.UseNumber()
	if err := d.Decode(&m); err != nil {
		t.Fatal(err)
	}
	m[field] = v
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// badPush spoils the trade pushes in one way the check must catch: tamper
// gets each push numbered from 1 and returns the messages to send in its
// place, and want is what the problem found must contain.
type badPush struct {
	name   string
	tamper func(t *testing.T, n int, msg []byte) [][]byte
	want   string
}

// badPushes lists the ways to spoil the push numbered k.
func badPushes(k int) []badPush {
	var held []byte
	return []badPush{
		{"missing", func(t *testing.T, n int, msg []byte) [][]byte {
			if n == k {
				return nil
			}
			return [][]byte{msg}
		}, "trade push seq 21 after 19"},
		{"duplicated", func(t *testing.T, n int, msg []byte) [][]byte {
			if n == k {
				return [][]byte{msg, msg}
			}
			return [][]byte{msg}
		}, "trade push seq 20 after 20"},
		{"out of order", func(t *testing.T, n int, msg []byte) [][]byte {
			switch n {
			case k:
				held = msg
				return nil
			case k + 1:
				return [][]byte{msg, held}
			}
			return [][]byte{msg}
		}, "trade push seq 21 after 19"},
		// The engine could lose a trade before numbering its push, or
		// number a duplicate: seq is then gap-free, but the count is off.
		{"missing, renumbered", func(t *testing.T, n int, msg []byte) [][]byte {
			switch {
			case n == k:
				return nil
			case n > k:
				return [][]byte{edit(t, msg, "seq", n-1)}
			}
			return [][]byte{msg}
		}, "trade pushes for"},
		{"duplicated, renumbered", func(t *testing.T, n int, msg []byte) [][]byte {
			switch {
			case n == k:
				return [][]byte{msg, edit(t, msg, "seq", n+1)}
			case n > k:
				return [][]byte{edit(t, msg, "seq", n+1)}
			}
			return [][]byte{msg}
		}, "trade push book_seq"},
		{"altered", func(t *testing.T, n int, msg []byte) [][]byte {
			if n == k {
				return [][]byte{edit(t, msg, "qty", 1_000_000)}
			}
			return [][]byte{msg}
		}, "the trade pushes differ from the trades in the replies"},
	}
}

// TestTradeCheckCatchesBadPushes runs loadgen through a proxy that spoils
// one trade push in each way the check must catch.
func TestTradeCheckCatchesBadPushes(t *testing.T) {
	const k = 20 // the push to spoil
	for _, tc := range badPushes(k) {
		t.Run(tc.name, func(t *testing.T) {
			addr := tamperProxy(t, startEngine(t, nil), func(n int, msg []byte) [][]byte { return tc.tamper(t, n, msg) })
			cfg := testConfig(addr)
			cfg.warmup, cfg.duration, cfg.pushWait = 100*time.Millisecond, 500*time.Millisecond, time.Second
			r, err := measure(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if r.Trades.Replies <= k {
				t.Fatalf("only %d trades, want over %d", r.Trades.Replies, k)
			}
			if r.Valid || !strings.Contains(strings.Join(r.Problems, "; "), tc.want) {
				t.Errorf("valid %v, problems %q; want a problem containing %q", r.Valid, r.Problems, tc.want)
			}
		})
	}
}

// A series the engine does not export is a wrong symbol or endpoint, not a
// counter at 0.
func TestTradesTotalNeedsItsSeries(t *testing.T) {
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("matching_engine_trades_total{symbol=\"BTC-USD\"} 7\n"))
	}))
	defer hs.Close()
	l := &loader{cfg: config{symbol: "BTC-USD"}, base: hs.URL}
	if n, err := l.tradesTotal(context.Background()); err != nil || n != 7 {
		t.Errorf("tradesTotal for an exported symbol = %d, %v, want 7, nil", n, err)
	}
	l.cfg.symbol = "ETH-USD"
	if n, err := l.tradesTotal(context.Background()); err == nil || !strings.Contains(err.Error(), "ETH-USD") {
		t.Errorf("tradesTotal for a symbol without a series = %d, %v, want an error naming the symbol", n, err)
	}
}

func TestScanTrades(t *testing.T) {
	// The events of a buy that traded with two makers, then rested.
	events := []byte(`"events":[{"seq":5,"ts":1,"type":"accepted","order_id":768,"side":"buy","price":101,"qty":9},` +
		`{"seq":6,"ts":1,"type":"trade","order_id":768,"maker_id":256,"side":"buy","price":100,"qty":3,"level_side":"sell","level_qty":0,"level_orders":0},` +
		`{"seq":7,"ts":1,"type":"trade","order_id":768,"maker_id":512,"side":"buy","price":101,"qty":4,"level_side":"sell","level_qty":2,"level_orders":1},` +
		`{"seq":8,"ts":1,"type":"rested","order_id":768,"side":"buy","price":101,"qty":2,"level_side":"buy","level_qty":2,"level_orders":1}]}`)
	var got tradeSum
	if !scanTrades(events, &got) {
		t.Fatal("scanTrades = false")
	}
	var want tradeSum
	want.add(trade{bookSeq: 6, taker: 768, maker: 256, price: 100, qty: 3})
	want.add(trade{bookSeq: 7, taker: 768, maker: 512, price: 101, qty: 4})
	if got != want {
		t.Errorf("scanTrades = %+v, want %+v", got, want)
	}
	if scanTrades([]byte(`{"seq":6,"ts":1,"type":"trade","order_id":768}`), &got) {
		t.Error("scanTrades accepted a trade event without maker_id, price and qty")
	}
}
