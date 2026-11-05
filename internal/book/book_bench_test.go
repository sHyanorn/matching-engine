package book

import (
	"runtime"
	"testing"

	"github.com/sHyanorn/matching-engine/internal/flow"
)

// orderFlow is n commands of the SPEC §9 flow (internal/flow), with order
// IDs counted from base+1.
func orderFlow(seed uint64, n int, sd float64, base OrderID) []Command {
	g := flow.New(seed, sd)
	cmds := make([]Command, n)
	for i := range cmds {
		op := g.Next()
		id := base + OrderID(op.Order)
		if op.Cancel {
			cmds[i] = cancel(id)
			continue
		}
		side := Sell
		if op.Buy {
			side = Buy
		}
		cmds[i] = limit(id, side, Price(op.Price), Qty(op.Qty))
	}
	return cmds
}

// allocCount counts heap allocations in the timed part of a benchmark.
// Go's allocs/op is an integer division, so a path that allocates on some
// commands only prints as 0 allocs/op; allocs/cmd keeps the fraction.
type allocCount struct{ total, since uint64 }

func mallocs() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.Mallocs
}

func (c *allocCount) resume() { c.since = mallocs() }
func (c *allocCount) pause()  { c.total += mallocs() - c.since }

// applyFlow replays cmds b.N times over, one command per op, on the book
// that start returns (untimed) before every pass, and reports cmds/s,
// trades/cmd and allocs/cmd.
func applyFlow(b *testing.B, cmds []Command, start func() *Book) {
	bk := start()
	buf := make([]Event, 0, 256)
	var trades int
	var allocs allocCount
	b.ReportAllocs()
	b.ResetTimer()
	allocs.resume()
	for i := 0; i < b.N; i++ {
		if i > 0 && i%len(cmds) == 0 {
			b.StopTimer()
			allocs.pause()
			bk = start()
			allocs.resume()
			b.StartTimer()
		}
		buf = bk.Apply(cmds[i%len(cmds)], buf[:0])
		for _, e := range buf {
			if e.Kind == EventTrade {
				trades++
			}
		}
	}
	b.StopTimer()
	allocs.pause()
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "cmds/s")
	b.ReportMetric(float64(trades)/float64(b.N), "trades/cmd")
	b.ReportMetric(float64(allocs.total)/float64(b.N), "allocs/cmd")
}

var flowSpreads = []struct {
	name string
	sd   float64
}{{"sd=5", 5}, {"sd=50", 50}}

// BenchmarkApply replays the flow, starting each pass from an empty book so
// the workload does not depend on b.N. ns/op is per command.
func BenchmarkApply(b *testing.B) {
	for _, tc := range flowSpreads {
		b.Run(tc.name, func(b *testing.B) {
			applyFlow(b, orderFlow(1, 1<<16, tc.sd, 0), New)
		})
	}
}

// prefill rests n orders on levels price levels per side, far enough from
// flow.Mid that the flow never reaches them, with IDs 1 to n.
func prefill(bk *Book, n, levels int) {
	buf := make([]Event, 0, 8)
	for i := range n {
		side, p := Buy, Price(flow.Mid-1_000-i/2%levels)
		if i%2 == 1 {
			side, p = Sell, Price(flow.Mid+1_000+i/2%levels)
		}
		buf = bk.Apply(limit(OrderID(1+i), side, p, 10), buf[:0])
	}
}

// BenchmarkApplyDeep replays the flow on a book that also holds resting
// orders outside the flow's price range, as a book in steady state does.
// After each pass the flow's resting orders are canceled (untimed), so
// every pass starts from the same depth.
func BenchmarkApplyDeep(b *testing.B) {
	for _, depth := range []struct {
		name   string
		orders int
	}{{"resting=10k", 10_000}, {"resting=1M", 1_000_000}} {
		for _, tc := range flowSpreads {
			b.Run(depth.name+"/"+tc.name, func(b *testing.B) {
				bk := New()
				prefill(bk, depth.orders, 1_000)
				base := OrderID(depth.orders)
				cmds := orderFlow(1, 1<<16, tc.sd, base)
				var orders OrderID
				for _, c := range cmds {
					if c.Kind == CommandNewOrder {
						orders++
					}
				}
				buf := make([]Event, 0, 8)
				applyFlow(b, cmds, func() *Book {
					for id := base + 1; id <= base+orders; id++ {
						buf = bk.Apply(cancel(id), buf[:0])
					}
					return bk
				})
			})
		}
	}
}

// BenchmarkRestingOrders builds a book of 1M resting orders on 1,000 levels
// per side (SPEC §9's memory measurement) and reports the heap it holds
// once built, per order and in total.
func BenchmarkRestingOrders(b *testing.B) {
	const n = 1_000_000
	var held uint64
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		before := liveHeap()
		b.StartTimer()
		bk := New()
		prefill(bk, n, 1_000)
		b.StopTimer()
		held = liveHeap() - before
		runtime.KeepAlive(bk)
		b.StartTimer()
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/n, "ns/order")
	b.ReportMetric(float64(held)/n, "B/order")
	b.ReportMetric(float64(held)/(1<<20), "MiB")
}

// liveHeap returns the bytes of live heap objects after a full GC.
func liveHeap() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}
