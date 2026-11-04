package book

import (
	"math/rand/v2"
	"testing"
)

// flow is a reproducible order flow in the shape SPEC §9 describes: GTC
// limit prices normally distributed around a mid price, about 30% cancels of
// earlier orders (some already filled, so they are rejected, as in real
// flow). IDs are relative to the start of the flow.
func flow(seed uint64, n int, sd float64) []Command {
	r := rand.New(rand.NewPCG(seed, 0xf10))
	const mid = 10_000
	cmds := make([]Command, 0, n)
	var nextID OrderID
	for len(cmds) < n {
		if nextID > 0 && r.IntN(100) < 30 {
			cmds = append(cmds, cancel(1+OrderID(r.Int64N(int64(nextID)))))
			continue
		}
		nextID++
		side, off := Buy, r.NormFloat64()*sd
		if r.IntN(2) == 0 {
			side = Sell
		}
		// Buyers center slightly below mid and sellers slightly above, so
		// a fraction of orders cross and trade, the rest rest.
		p := mid + Price(off)
		if side == Buy {
			p -= 2
		} else {
			p += 2
		}
		cmds = append(cmds, limit(nextID, side, p, Qty(1+r.IntN(100))))
	}
	return cmds
}

// BenchmarkApply replays the flow, starting each pass from an empty book so
// the workload does not depend on b.N. ns/op is per command.
func BenchmarkApply(b *testing.B) {
	for _, tc := range []struct {
		name string
		sd   float64
	}{{"sd=5", 5}, {"sd=50", 50}} {
		b.Run(tc.name, func(b *testing.B) {
			cmds := flow(1, 1<<16, tc.sd)
			bk := New()
			buf := make([]Event, 0, 256)
			var trades int
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if i > 0 && i%len(cmds) == 0 {
					b.StopTimer()
					bk = New()
					b.StartTimer()
				}
				buf = bk.Apply(cmds[i%len(cmds)], buf[:0])
				for _, e := range buf {
					if e.Kind == EventTrade {
						trades++
					}
				}
			}
			b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "cmds/s")
			b.ReportMetric(float64(trades)/float64(b.N), "trades/cmd")
		})
	}
}
