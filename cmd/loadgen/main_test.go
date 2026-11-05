package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sHyanorn/matching-engine/internal/api"
	"github.com/sHyanorn/matching-engine/internal/engine"
)

// startEngine serves an engine for BTC-USD on a loopback port and returns
// its address. clock, if not nil, stamps the engine's commands.
func startEngine(t *testing.T, clock func() int64) string {
	t.Helper()
	eng, err := engine.New(engine.Config{Symbols: []string{"BTC-USD"}, DataDir: t.TempDir(), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	srv := api.New(eng, api.Options{})
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		srv.CloseStreams()
		hs.Close()
		if err := eng.Close(); err != nil {
			t.Error(err)
		}
	})
	return strings.TrimPrefix(hs.URL, "http://")
}

func testConfig(addr string) config {
	return config{addr: addr, symbol: "BTC-USD", rate: 2000, warmup: 200 * time.Millisecond, duration: time.Second, conns: 8, seed: 1, sd: 5}
}

func TestMeasure(t *testing.T) {
	cfg := testConfig(startEngine(t, nil))
	// 1% of the window, the shortfall a valid run may have, must be more
	// than a busy machine's scheduling delay.
	cfg.rate, cfg.duration = 1000, 2*time.Second
	r, err := measure(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Valid {
		t.Fatalf("invalid run: %v", r.Problems)
	}
	succeeded := 0
	for _, k := range []string{"order_rested", "order_traded", "cancel_canceled", "cancel_finished"} {
		if r.Outcomes[k] == 0 {
			t.Errorf("no %s in %v", k, r.Outcomes)
		}
		succeeded += r.Outcomes[k]
	}
	succeeded += r.Outcomes["cancel_unknown"]
	for _, k := range []string{"reply", "service", "send_lag"} {
		if n := r.Latency[k].Count; n != succeeded {
			t.Errorf("%d %s samples, want one per succeeded request, %d", n, k, succeeded)
		}
	}
	if r.Latency["hiccup"].Count < 500 { // a 1 ms sleep loop over 2 s
		t.Errorf("%d hiccup samples, want one per 1 ms sleep", r.Latency["hiccup"].Count)
	}
	if n := r.Latency["trade_push"].Count; n != r.Outcomes["order_traded"] {
		t.Errorf("%d trade push samples, want one per order that traded, %d", n, r.Outcomes["order_traded"])
	}
}

// A stall of the book's goroutine delays every request due during it, not
// only those in flight, and the latency shows it: it counts from when each
// request was due. The service time, which counts from the actual send,
// hides most of it.
func TestStallIsChargedToEveryDelayedRequest(t *testing.T) {
	const stall = 400 * time.Millisecond
	var cmds atomic.Int64
	clock := func() int64 {
		if cmds.Add(1) == 800 { // about 0.2 s into the measured second
			time.Sleep(stall)
		}
		return time.Now().UnixNano()
	}
	r, err := measure(context.Background(), testConfig(startEngine(t, clock)))
	if err != nil {
		t.Fatal(err)
	}
	if n := r.Outcomes["busy"] + r.Outcomes["error"]; n > 0 {
		t.Fatalf("%d requests failed: %v", n, r.Problems)
	}
	// About stall*rate = 800 requests were due during the stall, 40% of
	// those measured, and the first three quarters of them waited over
	// 100 ms; only 8 were in flight when it began.
	const limit = 100_000 // µs
	if p := r.Latency["reply"].P90; p < limit {
		t.Errorf("p90 request to reply %.0f µs, want over %d", p, limit)
	}
	if p := r.Latency["send_lag"].P90; p < limit {
		t.Errorf("p90 send lag %.0f µs, want over %d", p, limit)
	}
	if p := r.Latency["service"].P90; p >= limit {
		t.Errorf("p90 service time %.0f µs, want under %d", p, limit)
	}
}

func TestRunWritesReport(t *testing.T) {
	out := filepath.Join(t.TempDir(), "r.json")
	var stdout bytes.Buffer
	err := run(context.Background(), []string{
		"-addr", startEngine(t, nil), "-rate", "1000", "-warmup", "0s", "-duration", "500ms", "-conns", "4", "-out", out,
	}, &stdout)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "order to trade push") || !strings.HasSuffix(stdout.String(), "valid\n") {
		t.Errorf("stdout:\n%s", stdout.String())
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var r report
	if err := json.Unmarshal(data, &r); err != nil || !r.Valid || r.TargetRate != 1000 || r.Latency["reply"].Count == 0 {
		t.Fatalf("report %s: %v", data, err)
	}
	if err := run(context.Background(), []string{"-rate", "0"}, &stdout); err == nil {
		t.Error("run accepted -rate 0")
	}
}
