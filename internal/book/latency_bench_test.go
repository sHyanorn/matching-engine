package book

import (
	"math"
	"slices"
	"testing"
	"time"
)

// BenchmarkApplyLatency times every Apply on its own and reports the
// distribution of the time one command takes, in ns: p50 to p99.99, and the
// largest over all passes. Each sample includes reading the clock twice;
// clock-p50-ns and clock-p99-ns are the same percentiles for an empty timed
// region, measured in the same run, so that cost can be seen. ns/op is the
// mean per command, clock included.
//
// The flows are those of BenchmarkApply and BenchmarkApplyDeep. empty-to-1M
// rests 1M orders on an empty book, so every growth of the order index
// happens inside a timed command.
func BenchmarkApplyLatency(b *testing.B) {
	for _, tc := range flowSpreads {
		b.Run(tc.name, func(b *testing.B) {
			applyLatency(b, orderFlow(1, 1<<16, tc.sd, 0), New)
		})
	}
	b.Run("resting=1M/sd=5", func(b *testing.B) {
		cmds, start := deepFlow(1_000_000, 5)
		applyLatency(b, cmds, start)
	})
	b.Run("empty-to-1M", func(b *testing.B) {
		cmds := make([]Command, 1_000_000)
		for i := range cmds {
			cmds[i] = restingOrder(i, 1_000)
		}
		applyLatency(b, cmds, New)
	})
}

// applyLatency replays cmds b.N times over, each pass on the book that
// start returns (untimed), and reports the percentiles of the time each
// command took.
func applyLatency(b *testing.B, cmds []Command, start func() *Book) {
	lat := make([]time.Duration, 0, b.N*len(cmds))
	buf := make([]Event, 0, 256)
	b.ResetTimer()
	for range b.N {
		b.StopTimer()
		bk := start()
		b.StartTimer()
		for _, c := range cmds {
			t0 := time.Now()
			buf = bk.Apply(c, buf[:0])
			lat = append(lat, time.Since(t0))
		}
	}
	b.StopTimer()

	clock := make([]time.Duration, len(cmds))
	for i := range clock {
		t0 := time.Now()
		clock[i] = time.Since(t0)
	}
	slices.Sort(lat)
	slices.Sort(clock)
	b.ReportMetric(float64(b.Elapsed())/float64(len(lat)), "ns/op")
	for _, p := range []struct {
		name string
		q    float64
	}{{"p50", 0.5}, {"p90", 0.9}, {"p99", 0.99}, {"p99.9", 0.999}, {"p99.99", 0.9999}} {
		b.ReportMetric(float64(percentile(lat, p.q)), p.name+"-ns")
	}
	b.ReportMetric(float64(lat[len(lat)-1]), "max-ns")
	b.ReportMetric(float64(percentile(clock, 0.5)), "clock-p50-ns")
	b.ReportMetric(float64(percentile(clock, 0.99)), "clock-p99-ns")
}

// percentile returns the q-quantile of sorted, a non-empty ascending slice,
// by the nearest-rank method: the smallest value at or above which a
// fraction q of the samples lie.
func percentile(sorted []time.Duration, q float64) time.Duration {
	return sorted[max(int(math.Ceil(q*float64(len(sorted))))-1, 0)]
}

func TestPercentile(t *testing.T) {
	s := []time.Duration{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	for _, c := range []struct {
		q    float64
		want time.Duration
	}{{0, 1}, {0.1, 1}, {0.5, 5}, {0.9, 9}, {0.95, 10}, {0.99, 10}, {1, 10}} {
		if got := percentile(s, c.q); got != c.want {
			t.Errorf("percentile(1..10, %v) = %v, want %v", c.q, got, c.want)
		}
	}
}
