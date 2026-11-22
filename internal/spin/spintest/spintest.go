// Package spintest has what tests of code that spins share: a pool of the
// cores this machine can spare, and a record of what the pool logged.
package spintest

import (
	"context"
	"log/slog"
	"runtime"
	"sync"
	"testing"

	"github.com/sHyanorn/matching-engine/internal/spin"
)

// Require skips the test unless this machine can spare cores cores to spin
// on, as spin.New counts them, and lets a thread be pinned to one.
func Require(tb testing.TB, cores int) {
	tb.Helper()
	if n := len(spare(tb)); n < cores {
		tb.Skipf("this machine can spare %d cores to spin on, the test needs %d", n, cores)
	}
}

// New returns a pool of exactly cores cores of this machine, lowest first,
// and skips the test as Require does.
func New(tb testing.TB, cores int) (*spin.Pool, *Log) {
	tb.Helper()
	Require(tb, cores)
	return pool(spare(tb)[:cores])
}

// Some returns a pool of up to most cores, as many as the machine can
// spare, and skips the test if it can spare none.
func Some(tb testing.TB, most int) (*spin.Pool, *Log) {
	tb.Helper()
	Require(tb, 1)
	cpus := spare(tb)
	return pool(cpus[:min(most, len(cpus))])
}

// spare returns the CPUs of the cores this machine can spare, and skips the
// test where spinning is not supported or a thread cannot be pinned.
func spare(tb testing.TB) []int {
	tb.Helper()
	p, err := spin.New(nil)
	if err != nil {
		tb.Skip(err)
	}
	cpus := p.CPUs()
	if len(cpus) > 0 && !canPin(cpus[0]) {
		tb.Skip("this system does not let a thread be pinned to a CPU")
	}
	return cpus
}

var pinProbe struct {
	once sync.Once
	ok   bool
}

// canPin reports whether a thread can be pinned to cpu, which a container
// may forbid. It is tried once.
func canPin(cpu int) bool {
	pinProbe.once.Do(func() {
		done := make(chan bool)
		go func() { // locked for good: the thread ends with the goroutine
			l := spin.NewPool([]int{cpu}, nil).Reserve("probe")
			defer l.Release()
			done <- l.Pin()
		}()
		pinProbe.ok = <-done
	})
	return pinProbe.ok
}

func pool(cpus []int) (*spin.Pool, *Log) {
	l := &Log{}
	return spin.NewPool(cpus, slog.New(l)), l
}

// Log is a slog.Handler that keeps the records it is given.
type Log struct {
	mu      sync.Mutex
	records []Record
}

// Record is one log record: its message and its attributes by key.
type Record struct {
	Msg   string
	Attrs map[string]any
}

func (l *Log) Enabled(context.Context, slog.Level) bool { return true }

func (l *Log) Handle(_ context.Context, r slog.Record) error {
	rec := Record{Msg: r.Message, Attrs: map[string]any{}}
	r.Attrs(func(a slog.Attr) bool {
		rec.Attrs[a.Key] = a.Value.Any()
		return true
	})
	l.mu.Lock()
	l.records = append(l.records, rec)
	l.mu.Unlock()
	return nil
}

func (l *Log) WithAttrs([]slog.Attr) slog.Handler { return l }

func (l *Log) WithGroup(string) slog.Handler { return l }

// Records returns what has been logged, in order.
func (l *Log) Records() []Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Record(nil), l.records...)
}

// Pinned returns the CPU each goroutine that pinned itself got, by what it
// was for.
func (l *Log) Pinned() map[string]int {
	pinned := map[string]int{}
	for _, r := range l.Records() {
		if r.Msg == "spin: pinned" {
			pinned[r.Attrs["for"].(string)] = int(r.Attrs["cpu"].(int64))
		}
	}
	return pinned
}

// Unserved returns what each goroutine that found no core free was for, in
// the order they asked.
func (l *Log) Unserved() []string {
	var what []string
	for _, r := range l.Records() {
		if r.Msg == "spin: no free core, running without spin" {
			what = append(what, r.Attrs["for"].(string))
		}
	}
	return what
}

// AllocsPerRun is testing.AllocsPerRun for code that spins. That one sets
// GOMAXPROCS to 1 while it counts, and then the spinning goroutines, each of
// which keeps its P, leave nothing to run the function with. Like it, this
// runs f once to warm up, then runs times, and returns the average number of
// allocations, in the whole process, as a whole number.
func AllocsPerRun(runs int, f func()) float64 {
	f()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range runs {
		f()
	}
	runtime.ReadMemStats(&after)
	return float64((after.Mallocs - before.Mallocs) / uint64(runs))
}
