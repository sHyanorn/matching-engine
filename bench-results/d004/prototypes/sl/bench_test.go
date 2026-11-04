package d004

import (
	"fmt"
	"math/rand/v2"
	"testing"
)

// Workloads model one side of a book with P resting levels around a mid
// price, mostly churning near the top (where real order flow lands).
var sizes = []int{100, 1_000, 10_000}

func filled(desc bool, n int) levelSet {
	s := newLevels(desc)
	for i := range n {
		s.Insert(Price(100_000 + i))
	}
	return s
}

// nearTop draws a price offset from the best with a roughly half-normal
// distribution (sd = n/20), so most activity is near the top.
func nearTop(r *rand.Rand, n int) int {
	o := int(r.NormFloat64() * float64(n) / 20)
	if o < 0 {
		o = -o
	}
	return o % n
}

func BenchmarkFind(b *testing.B) {
	for _, n := range sizes {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			s, r := filled(false, n), rand.New(rand.NewPCG(1, 2))
			ps := make([]Price, 4096)
			for i := range ps {
				ps[i] = Price(100_000 + nearTop(r, n))
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if s.Find(ps[i&4095]) == nil {
					b.Fatal("miss")
				}
			}
		})
	}
}

// BenchmarkChurnNearTop: a level near the top empties and another appears,
// keeping P constant. This is the dominant level-set operation under flow.
func BenchmarkChurnNearTop(b *testing.B) {
	for _, n := range sizes {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			s := newLevels(false)
			for i := range n {
				s.Insert(Price(100_000 + 2*i)) // even prices present
			}
			r := rand.New(rand.NewPCG(3, 4))
			ops := make([][2]Price, 4096)
			present := map[Price]bool{}
			for i := range n {
				present[Price(100_000+2*i)] = true
			}
			for i := range ops { // precompute valid delete/insert pairs
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
			// Undo the precomputation's effect by replaying from the same start
			// each 4096 ops: run ops forward, then backward.
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				k := i % 8192
				if k < 4096 {
					s.Delete(ops[k][0])
					s.Insert(ops[k][1])
				} else {
					k = 8191 - k
					s.Delete(ops[k][1])
					s.Insert(ops[k][0])
				}
			}
		})
	}
}

// BenchmarkSweepBest: an aggressive order empties the best level; the next
// best must be found. Measures Delete(best) + Best(), refilling behind.
func BenchmarkSweepBest(b *testing.B) {
	for _, n := range sizes {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			s := filled(false, n)
			next := Price(100_000 + n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				best := s.Best()
				s.Delete(best.Price)
				s.Insert(next) // keep P constant: add at the back
				next++
				if s.Best() == nil {
					b.Fatal("empty")
				}
			}
		})
	}
}

func BenchmarkWalk20(b *testing.B) {
	for _, n := range sizes {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			s := filled(true, n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				c := 0
				s.Walk(func(l *Level) bool { c++; return c < 20 })
			}
		})
	}
}

// BenchmarkBuild measures inserting n levels in random order (startup,
// snapshot restore) and reports bytes per level.
func BenchmarkBuild(b *testing.B) {
	for _, n := range sizes {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			r := rand.New(rand.NewPCG(5, 6))
			ps := r.Perm(n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				s := newLevels(false)
				for _, p := range ps {
					s.Insert(Price(p))
				}
			}
		})
	}
}
