package oe

import (
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/engine"
	"github.com/sHyanorn/matching-engine/internal/spin"
)

// BenchmarkAnswerAtLimit reports the median, 99th percentile and longest time
// Reply takes to give back a place of a session that has maxTaken commands
// unanswered while its reader waits for one. The benchmark goroutine calls
// Reply, standing in for the symbol's goroutine. The reader spins with
// Spin=on and blocks with Spin=off. ns/op also holds the time the reader is
// given to start waiting again.
func BenchmarkAnswerAtLimit(b *testing.B) {
	const settle = 50 * time.Microsecond
	for _, spinning := range []bool{false, true} {
		name := "Spin=off"
		if spinning {
			name = "Spin=on"
		}
		b.Run(name, func(b *testing.B) {
			c := attached(b, NewServer(ServerOptions{Symbols: testSymbols}))
			ss, n := c.ss, b.N
			ss.cur = nil // the replies are queued nowhere
			if spinning {
				c.lease = spin.NewPool([]int{0}, nil).Reserve("reader")
			}
			ss.taken = maxTaken
			var admitted atomic.Int64
			go func() {
				for seq := uint64(1); seq <= uint64(n); seq++ {
					if ok, _ := c.admit(seq, book.CommandNewOrder, 0, 0); !ok {
						return
					}
					admitted.Add(1)
				}
			}()
			answers := make([]time.Duration, n)
			for i := range n {
				for admitted.Load() < int64(i) { // the reader has taken the place the last answer gave back
				}
				for t0 := time.Now(); time.Since(t0) < settle; { // and is waiting for the next
				}
				t0 := time.Now()
				ss.Reply(uint64(i+1), engine.Reply{})
				answers[i] = time.Since(t0)
			}
			for admitted.Load() < int64(n) {
			}
			slices.Sort(answers)
			b.ReportMetric(float64(answers[n/2]), "p50-ns/answer")
			b.ReportMetric(float64(answers[n*99/100]), "p99-ns/answer")
			b.ReportMetric(float64(answers[n-1]), "max-ns/answer")
		})
	}
}
