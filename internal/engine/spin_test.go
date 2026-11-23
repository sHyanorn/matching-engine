package engine

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/spin"
	"github.com/sHyanorn/matching-engine/internal/spin/spintest"
)

// TestSpinSymbolsTakeCoresInTheirOrder: symbols take the cores of the pool
// in the order of Config.Symbols, and one that finds none free runs as it
// does without a pool. Every symbol is served, and the cores go back when
// the engine closes.
func TestSpinSymbolsTakeCoresInTheirOrder(t *testing.T) {
	pool, log := spintest.New(t, 2)
	syms := []string{"A", "B", "C"}
	e, err := New(Config{Symbols: syms, DataDir: t.TempDir(), Spin: pool})
	if err != nil {
		t.Fatal(err)
	}
	if pool.Free() != 0 {
		t.Errorf("%d cores free after New, want 0: the cores are reserved before New returns", pool.Free())
	}
	if got := log.Unserved(); !slices.Equal(got, []string{"symbol C"}) {
		t.Errorf("symbols without a core: %v, want [symbol C]", got)
	}
	waitFor(t, "the markets to pin themselves", func() bool { return len(log.Pinned()) == 2 })
	cpus := pool.CPUs()
	if got := log.Pinned(); got["symbol A"] != cpus[0] || got["symbol B"] != cpus[1] {
		t.Errorf("pinned %v, want A on CPU %d and B on CPU %d", got, cpus[0], cpus[1])
	}
	for _, s := range syms {
		if r, err := e.PlaceOrder(context.Background(), limitOrder(s, book.Buy, 100, 1)); err != nil || len(r.Events) == 0 {
			t.Fatalf("PlaceOrder on %s: %+v, %v", s, r, err)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if pool.Free() != 2 {
		t.Errorf("%d cores free after Close, want 2: it waits for every symbol's goroutine", pool.Free())
	}
}

// TestReserveCoreWithoutAPoolAllocatesNothing: reserveCore allocates nothing
// and returns no lease when there is no pool.
func TestReserveCoreWithoutAPoolAllocatesNothing(t *testing.T) {
	symbol := "BTC-USD"
	var core *spin.Lease
	if allocs := testing.AllocsPerRun(100, func() { core = reserveCore(nil, symbol) }); allocs != 0 {
		t.Errorf("%v allocations to reserve the core of a symbol without a pool, want 0", allocs)
	}
	if core != nil {
		t.Errorf("reserved %+v without a pool", core)
	}
}

// A spinning symbol stops when its queue is closed, with orders waiting in
// it, and a client that sends as it closes gets a result or ErrClosed.
func TestSpinCloseDrainsAndRefuses(t *testing.T) {
	pool, log := spintest.Some(t, 1)
	for range 20 {
		closeDuringTraffic(t, pool)
		if n, all := pool.Free(), len(pool.CPUs()); n != all {
			t.Fatalf("%d cores free after Close, want %d", n, all)
		}
	}
	if len(log.Pinned()) == 0 {
		t.Error("no symbol pinned itself")
	}
}

func TestSpinConcurrentClients(t *testing.T) {
	pool, log := spintest.Some(t, 3)
	concurrentClients(t, pool)
	if len(log.Pinned()) == 0 {
		t.Error("no symbol pinned itself")
	}
}

// A spinning symbol takes the fsync timer's tick while it has no command, as
// a blocked one does.
func TestSpinFsyncsWhileIdle(t *testing.T) {
	pool, _ := spintest.New(t, 1)
	synced := make(chan struct{}, 1)
	// The interval is long enough for the order to be written before the
	// first fsync, which is then due only on the timer.
	e := newTestEngine(t, Config{
		Symbols: []string{"A"}, SyncInterval: 200 * time.Millisecond, Spin: pool,
		OnSync: func(string, time.Duration) {
			select {
			case synced <- struct{}{}:
			default:
			}
		},
	})
	start := time.Now()
	if _, err := e.PlaceOrder(context.Background(), limitOrder("A", book.Buy, 100, 1)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-synced:
		if d := time.Since(start); d < 100*time.Millisecond {
			t.Errorf("fsync %v after the order, want it on the timer, after about 200ms", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no fsync within 5s of an order with nothing else to do")
	}
}

// The allocation test of the queue, the book, the log and the sink, with the
// symbol spinning.
func TestSpinSendAllocatesNothing(t *testing.T) {
	pool, log := spintest.New(t, 1)
	sendAllocatesNothing(t, pool, spintest.AllocsPerRun)
	if len(log.Pinned()) != 1 {
		t.Errorf("pinned %v, want the symbol", log.Pinned())
	}
}
