package book

import (
	"fmt"
	"math/rand/v2"
	"testing"
)

// These are the D004 workloads (bench-results/d004) run against the
// production tree. Each models one side of a book with n resting levels
// around a mid price, mostly churning near the top, where real order flow
// lands.
var levelBenchSizes = []int{100, 1_000, 10_000}

func filledTree(side Side, n int) levelTree {
	t := newLevelTree(side)
	for i := range n {
		if side == Buy {
			t.insert(Price(100_000 - i))
		} else {
			t.insert(Price(100_000 + i))
		}
	}
	return t
}

// nearTop draws an offset from the best with a roughly half-normal
// distribution (sd = n/20), so most activity is near the top.
func nearTop(r *rand.Rand, n int) int {
	o := int(r.NormFloat64() * float64(n) / 20)
	if o < 0 {
		o = -o
	}
	return o % n
}

func BenchmarkLevelTreeFind(b *testing.B) {
	for _, n := range levelBenchSizes {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			t, r := filledTree(Sell, n), rand.New(rand.NewPCG(1, 2))
			ps := make([]Price, 4096)
			for i := range ps {
				ps[i] = Price(100_000 + nearTop(r, n))
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if t.find(ps[i&4095]) == nil {
					b.Fatal("miss")
				}
			}
		})
	}
}

// BenchmarkLevelTreeChurnNearTop: a level near the top empties and another
// appears, keeping the level count constant. This is the dominant level-set
// operation under order flow.
func BenchmarkLevelTreeChurnNearTop(b *testing.B) {
	for _, n := range levelBenchSizes {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			t := newLevelTree(Sell)
			present := map[Price]bool{}
			for i := range n {
				p := Price(100_000 + 2*i) // even prices present
				t.insert(p)
				present[p] = true
			}
			r := rand.New(rand.NewPCG(3, 4))
			ops := make([][2]Price, 4096) // precomputed valid remove/insert pairs
			for i := range ops {
				var del, ins Price
				for {
					del = Price(100_000 + nearTop(r, 2*n))
					if present[del] {
						break
					}
				}
				for {
					ins = Price(100_000 + nearTop(r, 2*n))
					if !present[ins] && ins != del {
						break
					}
				}
				delete(present, del)
				present[ins] = true
				ops[i] = [2]Price{del, ins}
			}
			// Run the ops forward, then backward, so the tree returns to its
			// starting state every 8192 iterations.
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				k := i % 8192
				if k < 4096 {
					t.remove(t.find(ops[k][0]))
					t.insert(ops[k][1])
				} else {
					k = 8191 - k
					t.remove(t.find(ops[k][1]))
					t.insert(ops[k][0])
				}
			}
		})
	}
}

// BenchmarkLevelTreeSweepBest: an aggressive order empties the best level and
// the next best must be found, while a new level opens at the back.
func BenchmarkLevelTreeSweepBest(b *testing.B) {
	for _, n := range levelBenchSizes {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			t := filledTree(Sell, n)
			next := Price(100_000 + n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				t.remove(t.best)
				t.insert(next)
				next++
				if t.best == nil {
					b.Fatal("empty")
				}
			}
		})
	}
}

// BenchmarkLevelTreeTop20 reads the 20 best levels, as an L2 depth query does.
func BenchmarkLevelTreeTop20(b *testing.B) {
	for _, n := range levelBenchSizes {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			t := filledTree(Buy, n)
			var sink Qty
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				c := 0
				for l := t.best; l != nil && c < 20; l = l.next() {
					sink += l.totalQty
					c++
				}
			}
			_ = sink
		})
	}
}

// BenchmarkLevelTreeBuild inserts n levels in random order (startup, snapshot
// restore) and reports allocations.
func BenchmarkLevelTreeBuild(b *testing.B) {
	for _, n := range levelBenchSizes {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			ps := rand.New(rand.NewPCG(5, 6)).Perm(n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				t := newLevelTree(Sell)
				for _, p := range ps {
					t.insert(Price(p))
				}
			}
		})
	}
}
