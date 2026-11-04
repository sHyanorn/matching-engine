package d004

import (
	"cmp"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
)

// reference is the expected content of a set: the *Level that Insert
// returned for each price, plus the prices in side order (best first),
// computed independently of skipList.before.
type reference struct {
	descending bool
	levels     map[Price]*Level
	order      []Price
}

func newReference(descending bool) *reference {
	return &reference{descending: descending, levels: map[Price]*Level{}}
}

func (r *reference) compare(a, b Price) int {
	if r.descending {
		return cmp.Compare(b, a)
	}
	return cmp.Compare(a, b)
}

func (r *reference) add(p Price, l *Level) {
	i, _ := slices.BinarySearchFunc(r.order, p, r.compare)
	r.order = slices.Insert(r.order, i, p)
	r.levels[p] = l
}

func (r *reference) remove(p Price) {
	i, _ := slices.BinarySearchFunc(r.order, p, r.compare)
	r.order = slices.Delete(r.order, i, i+1)
	delete(r.levels, p)
}

// checkInvariants verifies the invariants documented at the top of
// skiplist.go, and that s holds exactly the levels in ref, at the pointers
// Insert returned for them.
func checkInvariants(t *testing.T, s *skipList, ref *reference, step int) {
	t.Helper()
	fail := func(format string, args ...any) {
		t.Helper()
		t.Fatalf("step %d: %s", step, fmt.Sprintf(format, args...))
	}

	if len(s.head.next) != maxHeight {
		fail("head has %d links, want %d", len(s.head.next), maxHeight)
	}

	// Read every level, bounding each walk so that a cycle fails the test
	// instead of hanging it.
	var levels [maxHeight][]*node
	for k := range maxHeight {
		for n := s.head.next[k]; n != nil; n = n.next[k] {
			if len(levels[k]) == len(ref.order) {
				fail("level %d has more than %d nodes", k, len(ref.order))
			}
			if len(n.next) <= k {
				fail("node %d of height %d is linked on level %d", n.Price, len(n.next), k)
			}
			levels[k] = append(levels[k], n)
		}
	}

	// Invariant 1: level 0 is the whole set, strictly in side order.
	if len(levels[0]) != len(ref.order) {
		fail("level 0 has %d nodes, want %d", len(levels[0]), len(ref.order))
	}
	for i, n := range levels[0] {
		if n.Price != ref.order[i] {
			fail("level 0 position %d holds %d, want %d", i, n.Price, ref.order[i])
		}
		if ref.levels[n.Price] != &n.Level {
			fail("level %d is not at the pointer Insert returned", n.Price)
		}
	}

	// Invariants 1 and 2: every level is strictly sorted, and every node on
	// level k >= 1 is on level k-1, in the same order (a merge-style
	// subsequence check).
	for k := range maxHeight {
		for i := 1; i < len(levels[k]); i++ {
			if a, b := levels[k][i-1].Price, levels[k][i].Price; !s.before(a, b) {
				fail("level %d: %d is followed by %d", k, a, b)
			}
		}
		if k == 0 {
			continue
		}
		below := levels[k-1]
		j := 0
		for _, n := range levels[k] {
			for j < len(below) && below[j] != n {
				j++
			}
			if j == len(below) {
				fail("node %d is on level %d but not, in order, on level %d", n.Price, k, k-1)
			}
			j++
		}
	}

	// Invariants 2 and 3: heights are in bounds, and level k holds every
	// node taller than k. With the checks above (only nodes taller than k
	// are linked on level k, each at most once), counting is enough.
	tallest := 0
	var taller [maxHeight]int // taller[k]: nodes whose height exceeds k
	for _, n := range levels[0] {
		h := len(n.next)
		if h < 1 || h > maxHeight {
			fail("node %d has height %d, want 1..%d", n.Price, h, maxHeight)
		}
		tallest = max(tallest, h)
		for k := range h {
			taller[k]++
		}
	}
	for k := range maxHeight {
		if len(levels[k]) != taller[k] {
			fail("level %d has %d nodes, but %d nodes are taller than %d", k, len(levels[k]), taller[k], k)
		}
	}

	// Invariant 4: height is the tallest node's height (0 when empty); the
	// count check above already showed that levels from there up are empty.
	if s.height != tallest {
		fail("height = %d, tallest node has height %d", s.height, tallest)
	}

	// Invariant 5 and the public view of it.
	if s.length != len(ref.order) || s.Len() != len(ref.order) {
		fail("length = %d, Len() = %d, want %d", s.length, s.Len(), len(ref.order))
	}
	best := s.Best()
	switch {
	case len(ref.order) == 0 && best != nil:
		fail("Best() = %d on an empty set", best.Price)
	case len(ref.order) > 0 && best != ref.levels[ref.order[0]]:
		fail("Best() = %v, want the level at %d", best, ref.order[0])
	}
}

// TestInvariantsUnderRandomOps applies long seeded random sequences of
// operations to both sides and checks every invariant after each one. The
// sequences alternate between growing and shrinking phases so that the set
// repeatedly empties (height back to 0) and grows tall again, and they mix
// in the edge cases matching produces: a new best, a new worst, deleting the
// best (a sweep) and deleting the worst.
func TestInvariantsUnderRandomOps(t *testing.T) {
	const steps = 6000
	for _, descending := range []bool{false, true} {
		for seed := uint64(1); seed <= 8; seed++ {
			t.Run(fmt.Sprintf("descending=%v/seed=%d", descending, seed), func(t *testing.T) {
				r := rand.New(rand.NewPCG(seed, 99))
				// Vary the height seed too, so different tower shapes are
				// exercised, and the price span, so that some runs are dense
				// (many collisions) and some sparse.
				s := newSkipList(descending, r.Uint64())
				ref := newReference(descending)
				span := []int64{8, 64, 1000, 1_000_000}[seed%4]
				randomPrice := func() Price { return Price(1 + r.Int64N(span)) }

				insert := func(p Price) {
					if _, ok := ref.levels[p]; ok {
						return
					}
					if s.Find(p) != nil {
						t.Fatalf("Find(%d) found a level that was never inserted", p)
					}
					l := s.Insert(p)
					if l == nil || l.Price != p || l.TotalQty != 0 {
						t.Fatalf("Insert(%d) = %+v", p, l)
					}
					l.TotalQty = int64(p) // later checks see the same Level
					ref.add(p, l)
				}
				remove := func(p Price) {
					s.Delete(p)
					ref.remove(p)
				}

				grow := true
				for step := range steps {
					if step%500 == 0 {
						grow = !grow
					}
					n := len(ref.order)
					op := r.IntN(10)
					switch {
					case n == 0 || (grow && op < 6) || (!grow && op < 2):
						insert(randomPrice())
					case op == 6: // a new best level, next to the old one
						if descending {
							insert(ref.order[0] + 1)
						} else if ref.order[0] > 1 {
							insert(ref.order[0] - 1)
						}
					case op == 7:
						remove(ref.order[0]) // a sweep empties the best
					case op == 8:
						remove(ref.order[n-1])
					default:
						if r.IntN(2) == 0 {
							remove(ref.order[r.IntN(n)])
						} else {
							p := ref.order[r.IntN(n)]
							if l := s.Find(p); l != ref.levels[p] || l.TotalQty != int64(p) {
								t.Fatalf("Find(%d) = %+v, want %+v", p, l, ref.levels[p])
							}
						}
					}
					checkInvariants(t, s, ref, step)
				}
				for len(ref.order) > 0 { // drain from the best, as matching does
					remove(ref.order[0])
					checkInvariants(t, s, ref, -1)
				}
			})
		}
	}
}

// shape describes a set's structure: each level's price and tower height,
// best first.
func shape(s *skipList) []string {
	var out []string
	for n := s.head.next[0]; n != nil; n = n.next[0] {
		out = append(out, fmt.Sprintf("%d:%d", n.Price, len(n.next)))
	}
	return out
}

// TestDeterministicStructure checks that the same operations always build
// the same structure (same prices with the same tower heights), which is
// what makes replays reproduce performance as well as results, and that the
// generator is the only source of the heights: another seed gives the same
// contents with different towers.
func TestDeterministicStructure(t *testing.T) {
	for _, descending := range []bool{false, true} {
		a := newLevels(descending).(*skipList)
		b := newLevels(descending).(*skipList)
		other := newSkipList(descending, defaultSeed+1)
		r := rand.New(rand.NewPCG(7, 7))
		present := map[Price]bool{}
		for range 20_000 {
			p := Price(1 + r.IntN(2000))
			for _, s := range []*skipList{a, b, other} {
				if present[p] {
					s.Delete(p)
				} else {
					s.Insert(p)
				}
			}
			present[p] = !present[p]
		}
		if !slices.Equal(shape(a), shape(b)) || a.height != b.height {
			t.Fatalf("descending=%v: two sets given the same operations differ", descending)
		}
		var pa, po []Price
		a.Walk(func(l *Level) bool { pa = append(pa, l.Price); return true })
		other.Walk(func(l *Level) bool { po = append(po, l.Price); return true })
		if !slices.Equal(pa, po) {
			t.Fatalf("descending=%v: the seed changed the contents", descending)
		}
		if slices.Equal(shape(a), shape(other)) {
			t.Fatalf("descending=%v: a different seed built identical towers", descending)
		}
	}
}

func TestTowerHeight(t *testing.T) {
	for _, tc := range []struct {
		r    uint64
		want int
	}{
		{1, 1}, {2, 1}, {3, 1}, // lowest base-4 digit nonzero
		{4, 2}, {8, 2}, {12, 2},
		{16, 3}, {64, 4},
		{1 << 20, 11},
		{1 << 21, 11},        // 10 zero digits, then the digit 2
		{1 << 22, maxHeight}, // 11 zero digits: exactly the cap
		{1 << 40, maxHeight}, // capped
		{0, maxHeight},       // capped
		{math.MaxUint64, 1},
	} {
		if got := towerHeight(tc.r); got != tc.want {
			t.Errorf("towerHeight(%#x) = %d, want %d", tc.r, got, tc.want)
		}
	}
}

// TestHeightDistribution checks that heights drawn from the generator
// follow P(height = h) = (3/4)·(1/4)^(h-1). The seed is fixed, so this is a
// regression test, not a flaky statistical one; the 5-sigma bound only says
// how far from ideal the fixed sample may be.
func TestHeightDistribution(t *testing.T) {
	const draws = 1 << 20
	g := splitMix64{state: defaultSeed}
	var count [maxHeight + 1]int
	for range draws {
		count[towerHeight(g.next())]++
	}
	for h := 1; h <= 7; h++ {
		p := 0.75 * math.Pow(0.25, float64(h-1))
		want := p * draws
		sigma := math.Sqrt(draws * p * (1 - p))
		if z := math.Abs(float64(count[h])-want) / sigma; z > 5 {
			t.Errorf("height %d: %d draws, want %.0f ± %.0f (z = %.1f)", h, count[h], want, sigma, z)
		}
	}
}

// TestSplitMix64KnownAnswers pins the generator to the published reference
// implementation (Vigna's splitmix64.c), seeded with 1234567.
func TestSplitMix64KnownAnswers(t *testing.T) {
	g := splitMix64{state: 1234567}
	for i, want := range []uint64{
		6457827717110365317,
		3203168211198807973,
		9817491932198370423,
		4593380528125082431,
		16408922859458223821,
	} {
		if got := g.next(); got != want {
			t.Fatalf("output %d = %d, want %d", i, got, want)
		}
	}
}

// searchSteps returns how many nodes a search for p (as done by seek)
// compares against p: one per move to the right plus one per level for
// the node that makes it drop down.
func searchSteps(s *skipList, p Price) int {
	steps := 0
	x := s.head
	for k := s.height - 1; k >= 0; k-- {
		for n := x.next[k]; n != nil; n = x.next[k] {
			steps++
			if !s.before(n.Price, p) {
				break
			}
			x = n
		}
	}
	return steps
}

func meanSearchSteps(s *skipList, ps []Price) float64 {
	total := 0
	for _, p := range ps {
		total += searchSteps(s, p)
	}
	return float64(total) / float64(len(ps))
}

// TestSearchCostIsLogarithmic builds large sets in order and in random order
// and checks that the height in use and the mean search cost stay close to
// their expected values, log4(P) and about 2·log2(P).
func TestSearchCostIsLogarithmic(t *testing.T) {
	const levels = 1 << 16 // log4 = 8, 2·log2 = 32
	ps := make([]Price, levels)
	for i := range ps {
		ps[i] = Price(1 + i)
	}
	shuffled := slices.Clone(ps)
	rand.New(rand.NewPCG(1, 1)).Shuffle(len(shuffled), func(i, j int) {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	})
	for name, order := range map[string][]Price{"ascending": ps, "shuffled": shuffled} {
		s := newSkipList(false, defaultSeed)
		for _, p := range order {
			s.Insert(p)
		}
		mean := meanSearchSteps(s, ps)
		t.Logf("%s: P = %d, height = %d, mean search steps = %.1f", name, levels, s.height, mean)
		if s.height < 6 || s.height > maxHeight-1 {
			t.Errorf("%s: height = %d, want about 8", name, s.height)
		}
		if mean > 48 {
			t.Errorf("%s: mean search steps = %.1f, want about 32", name, mean)
		}
	}
}

// TestPredictableHeightsCanBeAttacked documents the limitation described
// under "Determinism" in skiplist.go and checks its mitigation. An attacker who knows the seed
// predicts each new level's height, creates and deletes a level far from the
// best whenever the prediction is taller than 1, and only then creates a
// level near the best. Against the default seed this builds a run of
// height-1 nodes at the top of the book, and searches there walk it node by
// node. Against a seed the attacker does not know, the same script has no
// effect.
func TestPredictableHeightsCanBeAttacked(t *testing.T) {
	const run = 1024
	attack := func(victimSeed uint64) float64 {
		s := newSkipList(false, victimSeed)
		// The attacker's model of the generator: it assumes the default
		// seed and advances once per new level, as Insert does.
		predicted := splitMix64{state: defaultSeed}
		insert := func(p Price) {
			s.Insert(p)
			predicted.next()
		}
		nextHeight := func() int {
			peek := predicted
			return towerHeight(peek.next())
		}
		for p := Price(10_000); p < 14_096; p++ { // honest levels behind
			insert(p)
		}
		targets := make([]Price, run)
		for i := range targets {
			for nextHeight() != 1 {
				insert(1_000_000) // burn a tall height far from the best
				s.Delete(1_000_000)
			}
			targets[i] = Price(1 + i)
			insert(targets[i])
		}
		return meanSearchSteps(s, targets)
	}

	attacked := attack(defaultSeed)
	secret := attack(0x5ec2e7)
	t.Logf("mean search steps near the best: %.1f with a known seed, %.1f with a secret one", attacked, secret)
	if secret > 40 {
		t.Errorf("with a secret seed, mean search steps = %.1f, want O(log P)", secret)
	}
	if attacked < 10*secret {
		t.Errorf("the attack did not degrade searches (%.1f vs %.1f): update this test and the Determinism comment", attacked, secret)
	}
}

func mustPanic(t *testing.T, name string, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s did not panic", name)
		}
	}()
	f()
}

func TestMisusePanics(t *testing.T) {
	s := newSkipList(false, defaultSeed)
	mustPanic(t, "Delete on an empty set", func() { s.Delete(5) })
	for _, p := range []Price{10, 20, 30} {
		s.Insert(p)
	}
	mustPanic(t, "Insert of an existing level", func() { s.Insert(20) })
	mustPanic(t, "Delete between levels", func() { s.Delete(15) })
	mustPanic(t, "Delete before the best", func() { s.Delete(5) })
	mustPanic(t, "Delete past the end", func() { s.Delete(35) })
	if s.Len() != 3 {
		t.Fatalf("Len = %d after refused operations, want 3", s.Len())
	}
}

// TestDeleteDetachesNode checks that a deleted node holds no links, so a
// stale *Level cannot keep other nodes reachable.
func TestDeleteDetachesNode(t *testing.T) {
	s := newSkipList(true, defaultSeed)
	for p := Price(1); p <= 100; p++ {
		s.Insert(p)
	}
	var victim *node
	for n := s.head.next[0]; n != nil; n = n.next[0] {
		if n.Price == 50 {
			victim = n
		}
	}
	s.Delete(50)
	if victim.next != nil {
		t.Fatalf("deleted node still has links: %v", victim.next)
	}
}

func TestReadsDoNotAllocate(t *testing.T) {
	s := newSkipList(false, defaultSeed)
	for p := Price(1); p <= 1000; p++ {
		s.Insert(p)
	}
	if n := testing.AllocsPerRun(100, func() {
		_ = s.Find(500)
		_ = s.Find(5000)
		_ = s.Best()
	}); n != 0 {
		t.Fatalf("Find and Best allocate %.1f times per call", n)
	}
	if n := testing.AllocsPerRun(100, func() {
		s.Delete(500)
		s.Insert(500)
	}); n > 2 {
		t.Fatalf("Delete+Insert allocates %.1f times, want at most 2", n)
	}
}
