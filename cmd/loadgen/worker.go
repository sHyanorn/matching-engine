package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// worker sends requests over one keep-alive connection and records what
// its measured requests saw. Only its goroutine touches it until the run
// ends.
type worker struct {
	l      *loader
	client *http.Client
	body   []byte

	outcomes     map[string]int
	warmupFailed int // requests in the warmup that failed; not in outcomes
	firstErr     string
	sent         int      // measured requests sent before the window ended
	trades       tradeSum // in the replies, warmup included
	// Latency samples in nanoseconds, of measured requests that succeeded:
	// planned to reply, sent to reply, and planned to sent.
	reply, service, lag []int64
}

// newWorker opens the worker's connection.
func (l *loader) newWorker(ctx context.Context) (*worker, error) {
	w := &worker{
		l: l,
		client: &http.Client{
			Transport: &http.Transport{MaxConnsPerHost: 1, MaxIdleConnsPerHost: 1, DisableCompression: true},
			Timeout:   30 * time.Second,
		},
		outcomes: map[string]int{},
	}
	status, _, err := w.do(ctx, http.MethodGet, "/healthz", nil)
	if err == nil && status != http.StatusOK {
		err = fmt.Errorf("/healthz answered %d", status)
	}
	return w, err
}

// handle sends one request of the flow and records its outcome.
func (w *worker) handle(ctx context.Context, j job) {
	method, path, body := http.MethodPost, "/v1/orders", w.orderBody(j)
	if j.op.Cancel {
		id := w.l.ids[j.op.Order].Load()
		if id == 0 { // its order's reply is not in yet, or the order failed
			if j.measured {
				w.outcomes["cancel_skipped"]++
			}
			return
		}
		method, path, body = http.MethodDelete, "/v1/orders/"+strconv.FormatUint(id, 10), nil
	}
	sent := time.Now()
	status, data, err := w.do(ctx, method, path, body)
	done := time.Now()

	outcome, id, traded := classify(j, status, data, err, &w.trades)
	if id != 0 {
		w.l.ids[j.op.Order].Store(id)
		if traded {
			w.l.join.reply(id, j.planned, j.measured)
		}
	}
	failed := outcome == "busy" || outcome == "error"
	if failed && w.firstErr == "" {
		w.firstErr = fmt.Sprintf("%s %s: %d %s %v", method, path, status, bytes.TrimSpace(data), err)
	}
	if !j.measured {
		if failed {
			w.warmupFailed++
		}
		return
	}
	w.outcomes[outcome]++
	if sent.Before(w.l.end) {
		w.sent++
	}
	if failed {
		return
	}
	w.reply = append(w.reply, int64(done.Sub(j.planned)))
	w.service = append(w.service, int64(done.Sub(sent)))
	w.lag = append(w.lag, int64(sent.Sub(j.planned)))
}

// classify names a reply's outcome. For a new order it also returns the
// order's ID and whether the order traded, and adds its trades to trades.
func classify(j job, status int, data []byte, err error, trades *tradeSum) (outcome string, id uint64, traded bool) {
	switch {
	case err != nil:
		return "error", 0, false
	case status == http.StatusServiceUnavailable:
		return "busy", 0, false
	case j.op.Cancel:
		switch status {
		case http.StatusOK:
			return "cancel_canceled", 0, false
		case http.StatusConflict:
			return "cancel_finished", 0, false // filled before the cancel came
		case http.StatusNotFound:
			return "cancel_unknown", 0, false // finished long ago and forgotten
		}
		return "error", 0, false
	case status != http.StatusOK:
		return "error", 0, false
	}
	// Decoding the whole reply would cost loadgen more CPU than the
	// engine spends on the request, so read what is needed in place: the
	// reply starts with the order's ID, and an execution is an event of
	// type trade (docs/API.md).
	rest, ok := bytes.CutPrefix(data, replyPrefix)
	end := bytes.IndexByte(rest, ',')
	if !ok || end < 0 {
		return "error", 0, false
	}
	id, err = strconv.ParseUint(string(rest[:end]), 10, 64)
	if err != nil || id == 0 {
		return "error", 0, false
	}
	n := trades.n
	if !scanTrades(rest[end:], trades) {
		return "error", 0, false
	}
	if trades.n > n {
		return "order_traded", id, true
	}
	return "order_rested", id, false
}

var (
	replyPrefix = []byte(`{"order":{"order_id":`)
	tradeEvent  = []byte(`"type":"trade"`)
)

// orderBody is the JSON of j's new order, in a buffer the worker reuses.
func (w *worker) orderBody(j job) []byte {
	if j.op.Cancel {
		return nil
	}
	side := "sell"
	if j.op.Buy {
		side = "buy"
	}
	w.body = append(w.body[:0], `{"symbol":`...)
	w.body = strconv.AppendQuote(w.body, w.l.cfg.symbol)
	w.body = append(w.body, `,"side":"`...)
	w.body = append(w.body, side...)
	w.body = append(w.body, `","type":"limit","price":`...)
	w.body = strconv.AppendInt(w.body, j.op.Price, 10)
	w.body = append(w.body, `,"qty":`...)
	w.body = strconv.AppendInt(w.body, j.op.Qty, 10)
	w.body = append(w.body, '}')
	return w.body
}

// do makes one request and reads the whole reply, so the connection can
// be reused.
func (w *worker) do(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, w.l.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := w.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	data, err := io.ReadAll(res.Body)
	_ = res.Body.Close() // fully read; nothing left to report
	return res.StatusCode, data, err
}

// tradeJoin pairs each order that traded with the first trade push for it,
// which may come before or after the order's reply.
type tradeJoin struct {
	mu      sync.Mutex
	pushed  map[uint64]time.Time // first push of an order whose reply is not in
	waiting map[uint64]job       // orders that traded, whose push is not in
	samples []int64              // planned to push, of measured orders
}

func (t *tradeJoin) init() {
	t.pushed = map[uint64]time.Time{}
	t.waiting = map[uint64]job{}
}

// push notes the first trade push for order id, at time at.
func (t *tradeJoin) push(id uint64, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if j, ok := t.waiting[id]; ok {
		delete(t.waiting, id)
		t.record(j, at)
		return
	}
	t.pushed[id] = at
}

// reply notes that order id, due at planned, traded.
func (t *tradeJoin) reply(id uint64, planned time.Time, measured bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	j := job{planned: planned, measured: measured}
	if at, ok := t.pushed[id]; ok {
		delete(t.pushed, id)
		t.record(j, at)
		return
	}
	t.waiting[id] = j
}

func (t *tradeJoin) record(j job, at time.Time) {
	if j.measured {
		t.samples = append(t.samples, int64(at.Sub(j.planned)))
	}
}

// wait waits up to timeout for the pushes still missing: the first push
// of every measured order that traded, and as many pushes in all as the
// replies held trades. It returns how many measured orders never got
// their first push.
func (t *tradeJoin) wait(timeout time.Duration, pushed *atomic.Int64, trades int) int {
	for deadline := time.Now().Add(timeout); ; time.Sleep(10 * time.Millisecond) {
		t.mu.Lock()
		missing := 0
		for _, j := range t.waiting {
			if j.measured {
				missing++
			}
		}
		t.mu.Unlock()
		if missing == 0 && pushed.Load() >= int64(trades) || time.Now().After(deadline) {
			return missing
		}
	}
}

// report gathers the workers' results. A run is valid only if the target
// rate was met, every request succeeded, and every trade was pushed once
// and in order (check.go).
func (l *loader) report(workers []*worker, replied tradeSum, missing int, streamErr error, rose int, metricsErr error) report {
	r := report{
		Symbol: l.cfg.symbol, TargetRate: l.cfg.rate, Warmup: l.cfg.warmup.Seconds(), Duration: l.cfg.duration.Seconds(),
		Conns: l.cfg.conns, Seed: l.cfg.seed, SD: l.cfg.sd, Go: runtime.Version(), CPUs: runtime.NumCPU(),
		Outcomes: map[string]int{},
	}
	var sent, warmupFailed int
	var firstErr string
	var reply, service, lag []int64
	for _, w := range workers {
		for k, n := range w.outcomes {
			r.Outcomes[k] += n
		}
		sent += w.sent
		warmupFailed += w.warmupFailed
		if firstErr == "" {
			firstErr = w.firstErr
		}
		reply = append(reply, w.reply...)
		service = append(service, w.service...)
		lag = append(lag, w.lag...)
	}
	r.Achieved = float64(sent) / l.cfg.duration.Seconds()
	if r.Achieved < 0.99*l.cfg.rate {
		r.Problems = append(r.Problems, fmt.Sprintf("achieved %.0f/s, below 99%% of the target", r.Achieved))
	}
	if n := r.Outcomes["busy"] + r.Outcomes["error"] + warmupFailed; n > 0 {
		r.Problems = append(r.Problems, fmt.Sprintf("%d requests failed, %d of them in the warmup; the first: %s", n, warmupFailed, firstErr))
	}
	if missing > 0 {
		r.Problems = append(r.Problems, fmt.Sprintf("%d orders that traded got no trade push", missing))
	}
	if streamErr != nil {
		r.Problems = append(r.Problems, fmt.Sprintf("trade stream ended: %v", streamErr))
	}
	tp := &l.tape
	pushed := int(tp.pushed.Load())
	r.Trades = tradeCounts{Replies: replied.n, Pushes: pushed, Metric: rose}
	switch {
	case tp.problem != "":
		r.Problems = append(r.Problems, tp.problem)
	case pushed != replied.n:
		r.Problems = append(r.Problems, fmt.Sprintf("%d trade pushes for %d trades in the replies", pushed, replied.n))
	case tp.sum != replied.sum:
		r.Problems = append(r.Problems, "the trade pushes differ from the trades in the replies")
	}
	switch {
	case metricsErr != nil:
		r.Problems = append(r.Problems, fmt.Sprintf("reading matching_engine_trades_total: %v", metricsErr))
	case rose != replied.n:
		r.Problems = append(r.Problems, fmt.Sprintf("matching_engine_trades_total rose by %d for %d trades in the replies", rose, replied.n))
	}
	r.Valid = len(r.Problems) == 0
	r.Latency = map[string]summary{
		"trade_push": summarize(l.join.samples),
		"reply":      summarize(reply),
		"service":    summarize(service),
		"send_lag":   summarize(lag),
	}
	return r
}
