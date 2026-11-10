package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/testclock"
)

// faultyLog wraps a market's log file and injects failures: writes stop
// after limit bytes (a short write, as on a full disk), and Sync can fail.
// If stall is set, Sync waits for it to close, after closing syncing once.
type faultyLog struct {
	f       *os.File
	limit   int // bytes still accepted; negative means no limit
	syncErr error
	stall   chan struct{}
	syncing chan struct{}
	once    sync.Once
}

var errInjected = errors.New("injected failure")

func (l *faultyLog) Write(p []byte) (int, error) {
	if l.limit < 0 {
		return l.f.Write(p)
	}
	n, err := l.f.Write(p[:min(len(p), l.limit)])
	l.limit -= n
	if err == nil && n < len(p) {
		err = errInjected
	}
	return n, err
}

func (l *faultyLog) Sync() error {
	if l.stall != nil {
		l.once.Do(func() { close(l.syncing) })
		<-l.stall
	}
	if l.syncErr != nil {
		return l.syncErr
	}
	return l.f.Sync()
}

func (l *faultyLog) Close() error { return l.f.Close() }

// injectFault swaps the log of e's first market for l. It must run before
// the first request is sent: the send orders it before the goroutine's
// first use of the file.
func injectFault(e *Engine, l *faultyLog) {
	m := e.markets[0]
	l.f = m.log.file.(*os.File)
	m.log.file = l
}

// recordSize is the encoded size of a command with no client_id.
const recordSize = 4 + payloadFixed + 4

// sameDepth fails t unless a restarted engine on cfg's directory has the
// depth want, which the live engine showed.
func sameDepth(t *testing.T, cfg Config, want Depth) {
	t.Helper()
	cfg.Clock = nil
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer mustClose(t, e)
	got, _ := e.Depth(context.Background(), "A", 100)
	if !slices.Equal(got.Bids, want.Bids) || !slices.Equal(got.Asks, want.Asks) || got.LastSeq != want.LastSeq {
		t.Fatalf("after restart depth = %+v, live it was %+v", got, want)
	}
}

// TestShortWriteAppliesOnlyLoggedCommands cuts a batch's write short in
// the middle of its third record. The book applies exactly the commands
// that reached the log; the rest fail with ErrLogFailed and are never
// applied; the market accepts no more commands; and a restarted engine
// has the same book. When every batch is fsynced, the logged commands of
// the failed batch report ErrLogFailed too, with their events: they were
// applied, but nothing will fsync them.
func TestShortWriteAppliesOnlyLoggedCommands(t *testing.T) {
	for _, interval := range []time.Duration{0, -1} {
		t.Run(fmt.Sprintf("SyncInterval=%v", interval), func(t *testing.T) { shortWrite(t, interval) })
	}
}

func shortWrite(t *testing.T, interval time.Duration) {
	ctx := context.Background()
	g := testclock.New(nil)
	cfg := Config{Symbols: []string{"A"}, DataDir: t.TempDir(), Clock: g.Now, SyncInterval: interval}
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// The first batch (one order) and two records of the second fit.
	injectFault(e, &faultyLog{limit: 3*recordSize + 20})

	stalled, release := g.Stall()
	first := make(chan error, 1)
	go func() {
		_, err := e.PlaceOrder(ctx, limitOrder("A", book.Buy, 100, 1))
		first <- err
	}()
	<-stalled
	errs := make([]error, 4)
	events := make([][]book.Event, 4)
	var wg sync.WaitGroup
	for i := range 4 { // queued while the first batch is stalled: one batch
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := e.PlaceOrder(ctx, limitOrder("A", book.Buy, book.Price(90-i*10), 1))
			errs[i], events[i] = err, r.Events
		}()
		waitFor(t, "the order to be queued", func() bool { return len(e.markets[0].in) == i+1 })
	}
	release()
	wg.Wait()
	if err := <-first; err != nil {
		t.Fatalf("first order: %v", err)
	}
	for i, err := range errs {
		switch logged := i < 2; {
		case logged && interval < 0 && (!errors.Is(err, ErrLogFailed) || len(events[i]) == 0):
			t.Errorf("order %d was logged, not fsynced, but got %v, %d events", i, err, len(events[i]))
		case logged && interval >= 0 && (err != nil || len(events[i]) == 0):
			t.Errorf("order %d was logged but got %v, %d events", i, err, len(events[i]))
		case !logged && (!errors.Is(err, ErrLogFailed) || len(events[i]) != 0):
			t.Errorf("order %d was not logged but got %v, %d events", i, err, len(events[i]))
		}
	}
	if _, err := e.PlaceOrder(ctx, limitOrder("A", book.Buy, 1, 1)); !errors.Is(err, ErrLogFailed) {
		t.Fatalf("order after the failure: %v, want ErrLogFailed", err)
	}
	live, _ := e.Depth(ctx, "A", 100)
	if len(live.Bids) != 3 {
		t.Fatalf("live bids = %+v, want the 3 logged orders", live.Bids)
	}
	if err := e.Close(); !errors.Is(err, ErrLogFailed) {
		t.Fatalf("Close = %v, want ErrLogFailed", err)
	}
	if err := e.Close(); !errors.Is(err, ErrLogFailed) {
		t.Fatalf("second Close = %v, want the same error", err)
	}
	sameDepth(t, cfg, live)
}

func TestFailedWriteRefusesCommands(t *testing.T) {
	ctx := context.Background()
	cfg := Config{Symbols: []string{"A"}, DataDir: t.TempDir()}
	e, _ := New(cfg)
	injectFault(e, &faultyLog{limit: 0})
	for range 2 {
		r, err := e.PlaceOrder(ctx, limitOrder("A", book.Buy, 100, 1))
		if !errors.Is(err, ErrLogFailed) || len(r.Events) != 0 {
			t.Fatalf("got %v, %d events; want ErrLogFailed and nothing applied", err, len(r.Events))
		}
	}
	live, _ := e.Depth(ctx, "A", 1)
	if live.LastSeq != 0 {
		t.Fatalf("LastSeq = %d after failed writes", live.LastSeq)
	}
	if err := e.Close(); !errors.Is(err, ErrLogFailed) {
		t.Fatalf("Close = %v", err)
	}
	sameDepth(t, cfg, live)
}

// TestFailedSyncEveryBatch: with an fsync after every batch, a failed fsync
// leaves the batch in the file, so it is applied, and its caller gets the
// events with ErrLogFailed: applied, durability unknown.
func TestFailedSyncEveryBatch(t *testing.T) {
	ctx := context.Background()
	cfg := Config{Symbols: []string{"A"}, DataDir: t.TempDir(), SyncInterval: -1}
	e, _ := New(cfg)
	injectFault(e, &faultyLog{limit: -1, syncErr: errInjected})
	r, err := e.PlaceOrder(ctx, limitOrder("A", book.Buy, 100, 1))
	if !errors.Is(err, ErrLogFailed) || len(r.Events) != 2 {
		t.Fatalf("got %v, %d events; want ErrLogFailed with the events", err, len(r.Events))
	}
	if r, err := e.PlaceOrder(ctx, limitOrder("A", book.Buy, 99, 1)); !errors.Is(err, ErrLogFailed) || len(r.Events) != 0 {
		t.Fatalf("next order: %v, %d events; want ErrLogFailed, not applied", err, len(r.Events))
	}
	live, _ := e.Depth(ctx, "A", 100)
	if err := e.Close(); !errors.Is(err, ErrLogFailed) {
		t.Fatalf("Close = %v", err)
	}
	sameDepth(t, cfg, live)
}

// TestFailedSyncOnTick: with periodic fsync, replies do not wait for it
// (D017). A failed periodic fsync keeps what was applied and refuses later
// commands.
func TestFailedSyncOnTick(t *testing.T) {
	ctx := context.Background()
	cfg := Config{Symbols: []string{"A"}, DataDir: t.TempDir(), SyncInterval: 5 * time.Millisecond}
	e, _ := New(cfg)
	injectFault(e, &faultyLog{limit: -1, syncErr: errInjected})
	deadline := time.Now().Add(5 * time.Second)
	applied := 0
	for {
		r, err := e.PlaceOrder(ctx, limitOrder("A", book.Buy, book.Price(100+applied), 1))
		if err != nil {
			if !errors.Is(err, ErrLogFailed) || len(r.Events) != 0 {
				t.Fatalf("got %v, %d events", err, len(r.Events))
			}
			break
		}
		applied++
		if time.Now().After(deadline) {
			t.Fatal("the failing fsync never stopped the market")
		}
		time.Sleep(time.Millisecond)
	}
	live, _ := e.Depth(ctx, "A", 1000)
	if len(live.Bids) != applied {
		t.Fatalf("%d bids, %d orders were applied", len(live.Bids), applied)
	}
	if err := e.Close(); !errors.Is(err, ErrLogFailed) {
		t.Fatalf("Close = %v", err)
	}
	sameDepth(t, cfg, live)
}

// TestQueuedCommandOutlivesContext: once a command is queued its result is
// waited for, even after its context ends (D020). A query is not: it gives
// up with its context, and the engine is unharmed.
func TestQueuedCommandOutlivesContext(t *testing.T) {
	g := testclock.New(nil)
	e := newTestEngine(t, Config{Symbols: []string{"A"}, MaxBatch: 1, Clock: g.Now})
	t.Cleanup(g.ReleaseAll)
	stalled, release := g.Stall()
	type result struct {
		r   Result
		err error
	}
	done := make(chan result, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		r, err := e.PlaceOrder(ctx, limitOrder("A", book.Buy, 100, 1))
		done <- result{r, err}
	}()
	<-stalled // the order is being processed
	query, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := e.Depth(query, "A", 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("query on a stalled market: %v", err)
	}
	time.Sleep(40 * time.Millisecond) // well past the order's deadline
	release()
	res := <-done
	if res.err != nil || len(res.r.Events) != 2 {
		t.Fatalf("order whose context ended while queued: %v, %d events", res.err, len(res.r.Events))
	}
	if _, found, err := e.Order(context.Background(), res.r.OrderID); !found || err != nil {
		t.Fatalf("Order = %v, %v", found, err)
	}
}

// TestCanceledSubscribeDoesNotLeak cancels a Subscribe after it was queued.
// The subscription the caller never received must be removed, long before
// its large buffer could fill and drop it as a slow consumer.
func TestCanceledSubscribeDoesNotLeak(t *testing.T) {
	g := testclock.New(nil)
	var subs atomic.Int64
	var e *Engine
	clock := func() int64 {
		subs.Store(int64(len(e.markets[0].subs))) // read on the market's goroutine
		return g.Now()
	}
	e = newTestEngine(t, Config{Symbols: []string{"A"}, MaxBatch: 1, Clock: clock})
	t.Cleanup(g.ReleaseAll)
	stalled, release := g.Stall()
	go func() { _, _ = e.PlaceOrder(context.Background(), limitOrder("A", book.Buy, 100, 1)) }()
	<-stalled
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	const buffer = 1024
	if _, err := e.Subscribe(ctx, "A", buffer, 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Subscribe = %v, want its deadline", err)
	}
	release()
	for range 100 { // far fewer updates than the buffer holds
		_, _ = e.PlaceOrder(context.Background(), limitOrder("A", book.Buy, 100, 1))
		if subs.Load() == 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the abandoned subscription is still there after 100 updates")
}

// TestConcurrentCloseWaits: a second Close while the first is in progress
// waits for the shutdown too.
func TestConcurrentCloseWaits(t *testing.T) {
	g := testclock.New(nil)
	e, err := New(Config{Symbols: []string{"A"}, DataDir: t.TempDir(), Clock: g.Now})
	if err != nil {
		t.Fatal(err)
	}
	stalled, release := g.Stall()
	go func() { _, _ = e.PlaceOrder(context.Background(), limitOrder("A", book.Buy, 100, 1)) }()
	<-stalled
	closed := make(chan error, 2)
	for range 2 {
		go func() { closed <- e.Close() }()
	}
	select {
	case err := <-closed:
		t.Fatalf("Close returned %v while the market was still running", err)
	case <-time.After(30 * time.Millisecond):
	}
	release()
	for range 2 {
		if err := <-closed; err != nil {
			t.Fatal(err)
		}
	}
}

// lockedBuffer is a bytes.Buffer a logger on another goroutine can write.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestLogFailureReported fails an fsync, which runs on the idle tick or
// with the first command. LogErr reports it and the logger gets it once,
// without another command having to arrive; the other symbol is unaffected.
func TestLogFailureReported(t *testing.T) {
	var logs lockedBuffer
	e, err := New(Config{
		Symbols: []string{"A", "B"}, DataDir: t.TempDir(), SyncInterval: 5 * time.Millisecond,
		Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.Close(); !errors.Is(err, ErrLogFailed) {
			t.Errorf("Close = %v, want ErrLogFailed", err)
		}
	})
	injectFault(e, &faultyLog{limit: -1, syncErr: errInjected})
	ctx := context.Background()
	if e.LogErr("A") != nil || e.LogErr("X") != nil {
		t.Fatal("LogErr is set before any failure")
	}
	if _, err := e.PlaceOrder(ctx, limitOrder("A", book.Buy, 100, 1)); err != nil {
		t.Fatalf("first order: %v; a reply does not wait for the interval fsync", err)
	}
	waitFor(t, "LogErr(A) to report the failed fsync", func() bool { return e.LogErr("A") != nil })
	if err := e.LogErr("A"); !errors.Is(err, ErrLogFailed) || !errors.Is(err, errInjected) {
		t.Fatalf("LogErr(A) = %v, want ErrLogFailed wrapping the cause", err)
	}
	if _, err := e.PlaceOrder(ctx, limitOrder("A", book.Buy, 100, 1)); !errors.Is(err, ErrLogFailed) {
		t.Fatalf("order after the failure: %v, want ErrLogFailed", err)
	}
	if _, err := e.PlaceOrder(ctx, limitOrder("B", book.Buy, 100, 1)); err != nil || e.LogErr("B") != nil {
		t.Fatalf("symbol B: order %v, LogErr %v; want both nil", err, e.LogErr("B"))
	}
	if got := logs.String(); strings.Count(got, `"level":"ERROR"`) != 1 || !strings.Contains(got, `"symbol":"A"`) {
		t.Fatalf("logs %q, want one error for A", got)
	}
}

// TestOnSyncReportsEveryFsync checks that the hook sees each fsync of the
// log.
func TestOnSyncReportsEveryFsync(t *testing.T) {
	var mu sync.Mutex
	var syncs []string
	e, err := New(Config{
		Symbols: []string{"A"}, DataDir: t.TempDir(), SyncInterval: -1,
		OnSync: func(symbol string, took time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			if took < 0 {
				t.Errorf("fsync took %v", took)
			}
			syncs = append(syncs, symbol)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for range 3 {
		if _, err := e.PlaceOrder(ctx, limitOrder("A", book.Buy, 100, 1)); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	got := slices.Clone(syncs)
	mu.Unlock()
	if !slices.Equal(got, []string{"A", "A", "A"}) {
		t.Fatalf("OnSync calls %v, want one per order for A", got)
	}
	mustClose(t, e)
}

// TestCommandsDoNotWaitForIntervalFsync stalls an interval fsync: commands
// go on meanwhile (D031). Once the fsync returns its failure, the log
// stops as after any other.
func TestCommandsDoNotWaitForIntervalFsync(t *testing.T) {
	e, err := New(Config{Symbols: []string{"A"}, DataDir: t.TempDir(), SyncInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	l := &faultyLog{limit: -1, syncErr: errInjected, stall: make(chan struct{}), syncing: make(chan struct{})}
	injectFault(e, l)
	ctx := context.Background()
	if _, err := e.PlaceOrder(ctx, limitOrder("A", book.Buy, 100, 1)); err != nil {
		t.Fatal(err)
	}
	<-l.syncing
	done := make(chan error, 1)
	go func() {
		for range 10 {
			if _, err := e.PlaceOrder(ctx, limitOrder("A", book.Buy, 100, 1)); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("order during the fsync: %v", err)
		}
	case <-time.After(5 * time.Second):
		close(l.stall)
		t.Fatal("orders waited for the stalled fsync")
	}
	close(l.stall)
	waitFor(t, "the failed fsync to be taken up", func() bool { return e.LogErr("A") != nil })
	if _, err := e.PlaceOrder(ctx, limitOrder("A", book.Buy, 100, 1)); !errors.Is(err, ErrLogFailed) {
		t.Fatalf("order after the failed fsync: %v, want ErrLogFailed", err)
	}
	if err := e.Close(); !errors.Is(err, ErrLogFailed) {
		t.Fatalf("Close = %v, want ErrLogFailed", err)
	}
}

// TestCloseWaitsForIntervalFsync checks that Close returns only once the
// helper's fsync in progress is done, so nothing touches the log after it.
func TestCloseWaitsForIntervalFsync(t *testing.T) {
	e, err := New(Config{Symbols: []string{"A"}, DataDir: t.TempDir(), SyncInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	l := &faultyLog{limit: -1, stall: make(chan struct{}), syncing: make(chan struct{})}
	injectFault(e, l)
	if _, err := e.PlaceOrder(context.Background(), limitOrder("A", book.Buy, 100, 1)); err != nil {
		t.Fatal(err)
	}
	<-l.syncing
	closed := make(chan error, 1)
	go func() { closed <- e.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned %v during an fsync", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(l.stall)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}
