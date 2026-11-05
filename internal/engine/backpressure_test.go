package engine

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sHyanorn/matching-engine/internal/book"
)

// gateClock is a Clock that, once armed, blocks the first goroutine that
// calls it until released. Every other call returns at once. Blocking one
// symbol's goroutine inside its batch lets a test fill that symbol's queue.
type gateClock struct {
	armed   atomic.Bool
	stopped chan struct{} // closed once a caller is blocked
	release chan struct{}
	now     atomic.Int64
}

func newGateClock() *gateClock {
	return &gateClock{stopped: make(chan struct{}), release: make(chan struct{})}
}

func (g *gateClock) Now() int64 {
	if g.armed.CompareAndSwap(true, false) {
		close(g.stopped)
		<-g.release
	}
	return g.now.Add(1)
}

// waitFor polls cond until it holds or the test times out.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestFullQueueReturnsBusy stalls symbol A's goroutine, fills A's queue and
// checks that further orders and cancels on A get ErrBusy at once, that
// queries on A wait instead, that symbol B is unaffected, and that every
// queued order is applied once A resumes.
func TestFullQueueReturnsBusy(t *testing.T) {
	ctx := context.Background()
	const queue = 4
	g := newGateClock()
	e := newTestEngine(t, Config{Symbols: []string{"A", "B"}, QueueSize: queue, MaxBatch: 1, Clock: g.Now})

	// The first order on A blocks A's goroutine inside its clock call.
	g.armed.Store(true)
	var wg sync.WaitGroup
	var applied atomic.Int32
	place := func() {
		defer wg.Done()
		if _, err := e.PlaceOrder(ctx, limitOrder("A", book.Buy, 100, 1)); err != nil {
			t.Error(err)
			return
		}
		applied.Add(1)
	}
	wg.Add(1)
	go place()
	<-g.stopped

	// Fill A's queue exactly.
	a := e.bySymbol["A"]
	for range queue {
		wg.Add(1)
		go place()
	}
	waitFor(t, "A's queue to fill", func() bool { return len(a.in) == queue })

	start := time.Now()
	if _, err := e.PlaceOrder(ctx, limitOrder("A", book.Buy, 100, 1)); !errors.Is(err, ErrBusy) {
		t.Fatalf("order on a full queue: err = %v, want ErrBusy", err)
	}
	if _, err := e.CancelOrder(ctx, makeOrderID(1, a.index)); !errors.Is(err, ErrBusy) {
		t.Fatalf("cancel on a full queue: err = %v, want ErrBusy", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("ErrBusy took %v; it must not wait", d)
	}

	// B has its own goroutine and queue.
	if _, err := e.PlaceOrder(ctx, limitOrder("B", book.Sell, 100, 1)); err != nil {
		t.Fatalf("order on B while A is stalled: %v", err)
	}

	// A query waits for room rather than failing, bounded by its context.
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := e.Depth(short, "A", 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("query on a full queue: err = %v, want it to wait until its deadline", err)
	}

	close(g.release)
	wg.Wait()
	if got := applied.Load(); got != queue+1 {
		t.Fatalf("%d orders applied after A resumed, want %d", got, queue+1)
	}
	d, err := e.Depth(ctx, "A", 1)
	if err != nil || len(d.Bids) != 1 || d.Bids[0].Orders != queue+1 {
		t.Fatalf("A's depth = %+v, %v; want %d resting orders", d, err, queue+1)
	}
}
