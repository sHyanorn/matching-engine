package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sHyanorn/matching-engine/internal/api"
	"github.com/sHyanorn/matching-engine/internal/engine"
	"github.com/sHyanorn/matching-engine/internal/testclock"
)

// startEngine serves an engine for BTC-USD on a loopback port and returns
// its address. clock, if not nil, stamps the engine's commands. The log is
// almost never fsynced: these tests measure loadgen, not the disk. They run
// a load in real time, for seconds each, so -short skips them.
func startEngine(t *testing.T, clock func() int64) string {
	t.Helper()
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	eng, err := engine.New(engine.Config{
		Symbols: []string{"BTC-USD"}, DataDir: t.TempDir(), Clock: clock, SyncInterval: time.Hour,
	})
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

// realClock stamps commands with the system time and can stall the book.
func realClock() *testclock.Clock {
	return testclock.New(func(int64) int64 { return time.Now().UnixNano() })
}

func testConfig(addr string) config {
	// Enough connections that a run under the race detector keeps up.
	return config{addr: addr, symbol: "BTC-USD", rate: 2000, warmup: 200 * time.Millisecond, duration: time.Second, conns: 16, seed: 1, sd: 5}
}

func TestMeasure(t *testing.T) {
	cfg := testConfig(startEngine(t, nil))
	cfg.rate, cfg.duration = 1000, 4*time.Second
	r, err := measure(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Whether the target rate went out depends on how busy the machine
	// is; any other problem is measure's or the engine's.
	for _, p := range r.Problems {
		if strings.HasPrefix(p, "achieved ") {
			t.Log(p)
		} else {
			t.Errorf("invalid run: %s", p)
		}
	}
	// Every request due in the window is counted once, sent or not, and
	// none is sent before it is due.
	offered := 0
	for _, n := range r.Outcomes {
		offered += n
	}
	if want := int(cfg.rate * cfg.duration.Seconds()); offered != want || !(r.Achieved > 0 && r.Achieved <= cfg.rate) {
		t.Errorf("%d requests at %.0f/s in the window, want %d at up to %.0f/s", offered, r.Achieved, want, cfg.rate)
	}
	if lag := r.Latency["send_lag"].Curve[0]; lag[1] < 0 { // the curve starts at the least lag
		t.Errorf("a request was sent %.0f µs before it was due", -lag[1])
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
	if r.Latency["hiccup"].Count < 1000 { // a 1 ms sleep loop over 4 s
		t.Errorf("%d hiccup samples, want one per 1 ms sleep", r.Latency["hiccup"].Count)
	}
	if n := r.Latency["trade_push"].Count; n != r.Outcomes["order_traded"] {
		t.Errorf("%d trade push samples, want one per order that traded, %d", n, r.Outcomes["order_traded"])
	}
	if tc := r.Trades; tc.Replies < r.Outcomes["order_traded"] || tc.Pushes != tc.Replies || tc.Metric != tc.Replies {
		t.Errorf("trades %+v, want equal counts, at least one per order that traded (%d)", tc, r.Outcomes["order_traded"])
	}
}

// A stall of the book's goroutine delays every request due during it, not
// only those in flight, and the latency shows it: it counts from when each
// request was due. The service time, which counts from the actual send,
// hides most of it.
func TestStallIsChargedToEveryDelayedRequest(t *testing.T) {
	const stall = 400 * time.Millisecond
	clock := realClock()
	stalled, release := clock.StallAt(800) // about 0.2 s into the measured second
	addr := startEngine(t, clock.Now)
	go func() {
		<-stalled
		time.Sleep(stall)
		release()
	}()
	r, err := measure(context.Background(), testConfig(addr))
	if err != nil {
		t.Fatal(err)
	}
	if n := r.Outcomes["busy"] + r.Outcomes["error"]; n > 0 {
		t.Fatalf("%d requests failed: %v", n, r.Problems)
	}
	// About stall*rate = 800 requests were due during the stall, 40% of
	// those measured, and the first three quarters of them waited over
	// 100 ms; only 16 were in flight when it began.
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

func TestWarmupFailuresInvalidateRun(t *testing.T) {
	upstream := &url.URL{Scheme: "http", Host: startEngine(t, nil)}
	rp := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(upstream) }}
	const fail = 5
	var posts atomic.Int64
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The first 5 new orders, all due in the first milliseconds of the
		// warmup, get a 500. Everything else, the stream included, is relayed.
		if r.Method == http.MethodPost && posts.Add(1) <= fail {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"code":"internal","message":"injected"}}`)
			return
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(hs.Close)
	cfg := testConfig(strings.TrimPrefix(hs.URL, "http://"))
	cfg.rate, cfg.warmup, cfg.duration = 1000, 200*time.Millisecond, 4*time.Second
	r, err := measure(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if posts.Load() <= fail {
		t.Fatalf("only %d orders sent", posts.Load())
	}
	want := fmt.Sprintf("%d requests failed, %d of them in the warmup", fail, fail)
	if r.Valid || !slices.ContainsFunc(r.Problems, func(p string) bool { return strings.HasPrefix(p, want) }) {
		t.Errorf("valid %t, problems %v; want %q", r.Valid, r.Problems, want)
	}
}

func TestRunWritesReport(t *testing.T) {
	out := filepath.Join(t.TempDir(), "r.json")
	var stdout bytes.Buffer
	err := run(context.Background(), []string{
		"-addr", startEngine(t, nil), "-rate", "1000", "-warmup", "200ms", "-duration", "4s", "-conns", "16", "-out", out,
	}, &stdout)
	data, readErr := os.ReadFile(out)
	if readErr != nil {
		t.Fatal(err, readErr)
	}
	var r report
	if err := json.Unmarshal(data, &r); err != nil || r.TargetRate != 1000 || r.Latency["reply"].Count == 0 {
		t.Fatalf("report %s: %v", data, err)
	}
	// Whether the run is valid is measure's verdict (TestMeasure); run
	// passes it on in its table and its exit.
	verdict := "valid\n"
	if !r.Valid {
		verdict = "invalid: " + strings.Join(r.Problems, "; ") + "\n"
	}
	if !strings.Contains(stdout.String(), "order to trade push") || !strings.HasSuffix(stdout.String(), verdict) {
		t.Errorf("stdout:\n%s", stdout.String())
	}
	if (err == nil) != r.Valid {
		t.Errorf("run = %v for a report with problems %v", err, r.Problems)
	}
	for _, rate := range []string{"0", "NaN", "Inf", "1e300"} {
		if err := run(context.Background(), []string{"-rate", rate}, &stdout); err == nil {
			t.Errorf("run accepted -rate %s", rate)
		}
	}
}

// Canceling a run stops it at once, even while the engine is stalled and
// requests pile up.
func TestCancelDuringStall(t *testing.T) {
	clock := realClock()
	stalled, release := clock.StallAt(100)
	cfg := testConfig(startEngine(t, clock.Now))
	t.Cleanup(release)         // before the engine's cleanup, which waits for the book
	cfg.duration = time.Minute // far longer than the test may take
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := measure(ctx, cfg)
		done <- err
	}()
	<-stalled
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("measure = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("measure did not return within 1 s of the cancel")
	}
}

func TestWokeLate(t *testing.T) {
	ms := func(f float64) int64 { return int64(f * float64(time.Millisecond)) }
	for _, tc := range []struct {
		late time.Duration
		want []int64
	}{
		{200 * time.Microsecond, []int64{ms(0.2)}},
		{1500 * time.Microsecond, []int64{ms(1.5)}},
		{5500 * time.Microsecond, []int64{ms(5.5), ms(4.5), ms(3.5), ms(2.5), ms(1.5)}},
		{3 * time.Millisecond, []int64{ms(3), ms(2), ms(1)}},
	} {
		if got := wokeLate(nil, tc.late); !slices.Equal(got, tc.want) {
			t.Errorf("wokeLate(%v) = %v, want %v", tc.late, got, tc.want)
		}
	}
}
