package spintest_test

import (
	"log/slog"
	"slices"
	"testing"

	"github.com/sHyanorn/matching-engine/internal/spin"
	"github.com/sHyanorn/matching-engine/internal/spin/spintest"
)

func TestSomeGivesUpToTheCoresAsked(t *testing.T) {
	pool, _ := spintest.Some(t, 2)
	if n := len(pool.CPUs()); n < 1 || n > 2 {
		t.Errorf("a pool of %d cores, want 1 or 2", n)
	}
	if all, err := spin.New(nil); err == nil && !slices.Equal(pool.CPUs(), all.CPUs()[:len(pool.CPUs())]) {
		t.Errorf("pool %v is not the first cores of %v", pool.CPUs(), all.CPUs())
	}
}

func TestNewGivesExactlyTheCoresAsked(t *testing.T) {
	pool, _ := spintest.New(t, 1)
	if n := len(pool.CPUs()); n != 1 {
		t.Errorf("a pool of %d cores, want 1", n)
	}
}

// A test that needs more cores than there are is skipped, not failed.
func TestTooFewCoresSkipTheTest(t *testing.T) {
	for name, need := range map[string]func(*testing.T){
		"Require": func(t *testing.T) { spintest.Require(t, 1<<20) },
		"New":     func(t *testing.T) { spintest.New(t, 1<<20) },
	} {
		t.Run(name, func(inner *testing.T) {
			inner.Cleanup(func() {
				if !inner.Skipped() {
					t.Errorf("%s did not skip a test that needs a million cores", name)
				}
			})
			need(inner)
		})
	}
}

func TestRequireLetsAnEnoughTestRun(t *testing.T) {
	spintest.Require(t, 1)
	spintest.Require(t, 0)
}

func TestLogRecordsWhatThePoolDoes(t *testing.T) {
	pool, log := spintest.New(t, 1)
	first := pool.Reserve("symbol A")
	if second := pool.Reserve("symbol B"); second != nil {
		t.Fatal("a second core from a pool of one")
	}
	if got := log.Unserved(); !slices.Equal(got, []string{"symbol B"}) {
		t.Errorf("Unserved = %v, want [symbol B]", got)
	}
	if got := log.Pinned(); len(got) != 0 {
		t.Errorf("Pinned = %v before any pin", got)
	}
	pinned := make(chan bool, 1)
	go func() { // locked for good: the thread ends with the goroutine
		defer first.Release()
		pinned <- first.Pin()
	}()
	if !<-pinned {
		t.Skip("this machine does not let the test pin a thread")
	}
	if got, want := log.Pinned(), pool.CPUs()[0]; len(got) != 1 || got["symbol A"] != want {
		t.Errorf("Pinned = %v, want symbol A on CPU %d", got, want)
	}
	recs := log.Records()
	if len(recs) != 2 || recs[0].Msg != "spin: no free core, running without spin" || recs[1].Msg != "spin: pinned" {
		t.Fatalf("Records = %+v, want the refusal and then the pin", recs)
	}
	recs[0].Msg = "changed"
	if log.Records()[0].Msg == "changed" {
		t.Error("Records shares its slice with the log")
	}
}

func TestAllocsPerRun(t *testing.T) {
	var sink []byte
	calls := 0
	if got := spintest.AllocsPerRun(100, func() { calls++ }); got != 0 {
		t.Errorf("%v allocations for a function that makes none", got)
	}
	if calls != 101 {
		t.Errorf("the function ran %d times, want once to warm up and 100 more", calls)
	}
	if got := spintest.AllocsPerRun(100, func() { sink = make([]byte, 1<<10) }); got != 1 {
		t.Errorf("%v allocations for a function that makes one", got)
	}
	_ = sink
}

func TestLogIsAHandler(t *testing.T) {
	var h slog.Handler = &spintest.Log{}
	if !h.Enabled(t.Context(), slog.LevelDebug) {
		t.Error("the log leaves out debug records")
	}
	if h.WithAttrs([]slog.Attr{slog.Int("a", 1)}) != h || h.WithGroup("g") != h {
		t.Error("WithAttrs or WithGroup made another handler")
	}
}
