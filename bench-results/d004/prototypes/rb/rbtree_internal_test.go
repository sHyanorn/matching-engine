package d004

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

// verify checks every invariant documented on rbTree and returns the first
// violation it finds, or nil. It costs O(P).
func verify(t *rbTree) error {
	if t.root != nil {
		if t.root.parent != nil {
			return errors.New("links: root has a parent")
		}
		if t.root.red {
			return errors.New("root is red")
		}
	}

	// One recursive pass checks order against the bounds inherited from the
	// ancestors (property 1), red-red (4), black heights (5) and child links
	// (6), and records the in-order sequence and the height.
	inorder := make([]*node, 0, t.size)
	height := 0
	var check func(n, lo, hi *node, depth int) (blackHeight int, err error)
	check = func(n, lo, hi *node, depth int) (int, error) {
		if n == nil {
			height = max(height, depth)
			return 0, nil
		}
		if lo != nil && !t.before(lo.Price, n.Price) {
			return 0, fmt.Errorf("order: %d is in the right subtree of %d", n.Price, lo.Price)
		}
		if hi != nil && !t.before(n.Price, hi.Price) {
			return 0, fmt.Errorf("order: %d is in the left subtree of %d", n.Price, hi.Price)
		}
		if (n.left != nil && n.left.parent != n) || (n.right != nil && n.right.parent != n) {
			return 0, fmt.Errorf("links: a child of %d has the wrong parent", n.Price)
		}
		if n.red && (isRed(n.left) || isRed(n.right)) {
			return 0, fmt.Errorf("red node %d has a red child", n.Price)
		}
		lh, err := check(n.left, lo, n, depth+1)
		if err != nil {
			return 0, err
		}
		inorder = append(inorder, n)
		rh, err := check(n.right, n, hi, depth+1)
		if err != nil {
			return 0, err
		}
		if lh != rh {
			return 0, fmt.Errorf("black height: %d has %d on the left, %d on the right", n.Price, lh, rh)
		}
		if !n.red {
			lh++
		}
		return lh, nil
	}
	if _, err := check(t.root, nil, nil, 0); err != nil {
		return err
	}

	if t.size != len(inorder) {
		return fmt.Errorf("size is %d, tree has %d nodes", t.size, len(inorder))
	}
	var leftmostNode *node
	if len(inorder) > 0 {
		leftmostNode = inorder[0]
	}
	if t.best != leftmostNode {
		return fmt.Errorf("best is %v, leftmost is %v", priceOf(t.best), priceOf(leftmostNode))
	}
	if b := t.Best(); (b == nil) != (t.best == nil) || (b != nil && b != &t.best.Level) {
		return errors.New("best: Best() does not return the cached node's Level")
	}
	// Implied by properties 4 and 5; checked directly because it is the
	// O(log P) claim itself.
	if float64(height) > 2*math.Log2(float64(t.size+1)) {
		return fmt.Errorf("height %d exceeds 2*log2(%d+1)", height, t.size)
	}
	// Walk uses parent links and successor, not recursion, so compare it
	// with the in-order sequence pointer by pointer.
	i := 0
	var walkErr error
	t.Walk(func(l *Level) bool {
		if i >= len(inorder) || l != &inorder[i].Level {
			walkErr = fmt.Errorf("walk: visit %d is %d, not the in-order node", i, l.Price)
			return false
		}
		i++
		return true
	})
	if walkErr == nil && i != len(inorder) {
		walkErr = fmt.Errorf("walk: visited %d levels, tree has %d", i, len(inorder))
	}
	return walkErr
}

func priceOf(n *node) any {
	if n == nil {
		return nil
	}
	return n.Price
}

// harness applies operations to a tree, checks every invariant after each
// one, and remembers every *Level handed out so it can check that each stays
// valid until its level is deleted.
type harness struct {
	tb     testing.TB
	name   string
	tree   *rbTree
	desc   bool
	levels map[Price]*Level // levels handed out and not yet deleted
	prices []Price          // the keys of levels, for drawing one at random
	step   int
}

func newHarness(tb testing.TB, name string, desc bool) *harness {
	return &harness{
		tb:     tb,
		name:   name,
		tree:   newLevels(desc).(*rbTree),
		desc:   desc,
		levels: map[Price]*Level{},
	}
}

// tag is a value stored in each level's TotalQty, so a Level that was copied
// into another node, rather than left in place, is detected.
func tag(p Price) int64 { return int64(p)*7 + 3 }

func (h *harness) has(p Price) bool { _, ok := h.levels[p]; return ok }

func (h *harness) insert(p Price) {
	h.tb.Helper()
	l := h.tree.Insert(p)
	if l == nil || l.Price != p {
		h.tb.Fatalf("%s step %d: Insert(%d) = %v", h.name, h.step, p, l)
	}
	l.TotalQty = tag(p)
	h.levels[p] = l
	h.prices = append(h.prices, p)
	h.after("Insert", p)
}

func (h *harness) delete(p Price) {
	h.tb.Helper()
	h.tree.Delete(p)
	delete(h.levels, p)
	i := slices.Index(h.prices, p)
	h.prices[i] = h.prices[len(h.prices)-1]
	h.prices = h.prices[:len(h.prices)-1]
	h.after("Delete", p)
}

// best returns the best price according to the harness's own records, not
// the tree's cache.
func (h *harness) best() Price {
	b := h.prices[0]
	for _, p := range h.prices[1:] {
		if h.tree.before(p, b) {
			b = p
		}
	}
	return b
}

func (h *harness) randomPresent(r *rand.Rand) Price { return h.prices[r.IntN(len(h.prices))] }

// after checks the tree after the operation op(p).
func (h *harness) after(op string, p Price) {
	h.tb.Helper()
	h.step++
	if err := verify(h.tree); err != nil {
		h.tb.Fatalf("%s step %d, after %s(%d): %v", h.name, h.step, op, p, err)
	}
	if h.tree.Len() != len(h.levels) {
		h.tb.Fatalf("%s step %d: Len = %d, want %d", h.name, h.step, h.tree.Len(), len(h.levels))
	}
	// Pointer stability is O(P log P) to check, so do it every 32 steps,
	// and always while the tree is small and restructures the most.
	if h.step%32 == 0 || h.tree.Len() < 32 {
		h.checkPointers()
	}
}

func (h *harness) checkPointers() {
	h.tb.Helper()
	for p, l := range h.levels {
		if f := h.tree.Find(p); f != l {
			h.tb.Fatalf("%s step %d: Find(%d) returned a different *Level than Insert", h.name, h.step, p)
		}
		if l.Price != p || l.TotalQty != tag(p) {
			h.tb.Fatalf("%s step %d: level %d holds {%d %d}: its data moved", h.name, h.step, p, l.Price, l.TotalQty)
		}
	}
}

// Workloads. Each drives a harness with a seeded generator; between them
// they reach every case of both fixups on both sides (see the coverage
// profile).

// randomSmall keeps a small set (about 2/3 of a span of 32 prices), so the
// tree is often nearly empty and the fixups run near the root.
func randomSmall(h *harness, r *rand.Rand) {
	for range 20_000 {
		p := Price(1 + r.IntN(32))
		switch {
		case !h.has(p):
			h.insert(p)
		case r.IntN(2) == 0:
			h.delete(p)
		}
	}
}

// growShrink inserts random prices from a wide span while mostly growing,
// then deletes random levels until the set is empty.
func growShrink(h *harness, r *rand.Rand) {
	const steps = 6_000
	for i := range steps {
		insertProb := 75
		if i >= steps/2 {
			insertProb = 25
		}
		if len(h.prices) == 0 || r.IntN(100) < insertProb {
			p := Price(1 + r.IntN(1<<20))
			if !h.has(p) {
				h.insert(p)
			}
		} else {
			h.delete(h.randomPresent(r))
		}
	}
	for len(h.prices) > 0 {
		h.delete(h.randomPresent(r))
	}
}

// churnNearTop mimics order flow: it holds about 500 levels, and each round
// either sweeps the best level or empties one near the top, then opens a new
// level near the top, sometimes better than the best.
func churnNearTop(h *harness, r *rand.Rand) {
	const mid, n = 100_000, 500
	sign := Price(1) // worse prices are higher on the ask side
	if h.desc {
		sign = -1
	}
	for i := range n {
		h.insert(mid + sign*Price(2*i))
	}
	for range 6_000 {
		best := h.best()
		if r.IntN(2) == 0 {
			h.delete(best)
		} else {
			for { // a present level at most 80 ticks behind the best
				p := best + sign*Price(r.IntN(80))
				if h.has(p) {
					h.delete(p)
					break
				}
			}
		}
		best = h.best()
		for { // a new level from 10 ticks better than the best to 50 worse
			p := best + sign*Price(r.IntN(60)-10)
			if !h.has(p) {
				h.insert(p)
				break
			}
		}
	}
}

// sequential runs ordered patterns: ascending and descending inserts, drains
// from the best end, the worst end and both ends alternately, and a zig-zag
// fill from the middle outward. Sorted input is the classic bad case for an
// unbalanced tree, and draining from the best is what an aggressive order
// does.
func sequential(h *harness, _ *rand.Rand) {
	const n = 600
	for p := Price(1); p <= n; p++ {
		h.insert(p)
	}
	for len(h.prices) > 0 {
		h.delete(h.best())
	}
	for p := Price(n); p >= 1; p-- {
		h.insert(p)
	}
	for len(h.prices) > 0 { // from the worst end
		w := h.prices[0]
		for _, p := range h.prices {
			if h.tree.before(w, p) {
				w = p
			}
		}
		h.delete(w)
	}
	for i := Price(0); i < n; i++ { // zig-zag: 300, 301, 299, 302, 298, ...
		if i%2 == 0 {
			h.insert(n/2 + i/2)
		} else {
			h.insert(n/2 - 1 - i/2)
		}
	}
	for lo, hi := Price(1), Price(n-1); lo <= hi; lo, hi = lo+1, hi-1 {
		h.delete(hi)
		if lo != hi && h.has(lo) {
			h.delete(lo)
		}
	}
	for len(h.prices) > 0 {
		h.delete(h.prices[0])
	}
}

// TestInvariants applies every workload to both sides with several seeds
// and checks all invariants after every single operation.
func TestInvariants(t *testing.T) {
	workloads := []struct {
		name string
		run  func(*harness, *rand.Rand)
	}{
		{"randomSmall", randomSmall},
		{"growShrink", growShrink},
		{"churnNearTop", churnNearTop},
		{"sequential", sequential},
	}
	seeds := []uint64{1, 2, 3}
	if testing.Short() {
		seeds = seeds[:1]
	}
	for _, w := range workloads {
		for _, desc := range []bool{false, true} {
			for _, seed := range seeds {
				name := fmt.Sprintf("%s/desc=%v/seed=%d", w.name, desc, seed)
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					h := newHarness(t, name, desc)
					w.run(h, rand.New(rand.NewPCG(seed, 0x5eed)))
					h.checkPointers()
				})
			}
		}
	}
}

// TestBefore checks the side comparator on every pair of boundary values,
// where negating a price would overflow, and then runs those values through
// both trees.
func TestBefore(t *testing.T) {
	ps := []Price{math.MinInt64, math.MinInt64 + 1, -2, -1, 0, 1, 2, math.MaxInt64 - 1, math.MaxInt64}
	asks, bids := newLevels(false).(*rbTree), newLevels(true).(*rbTree)
	for _, a := range ps {
		for _, b := range ps {
			if got := asks.before(a, b); got != (a < b) {
				t.Errorf("asks: before(%d, %d) = %v", a, b, got)
			}
			if got := bids.before(a, b); got != (a > b) {
				t.Errorf("bids: before(%d, %d) = %v", a, b, got)
			}
		}
	}
	for _, desc := range []bool{false, true} {
		h := newHarness(t, fmt.Sprintf("extremes/desc=%v", desc), desc)
		for _, i := range []int{4, 0, 8, 1, 7, 2, 6, 3, 5} {
			h.insert(ps[i])
		}
		for len(h.prices) > 0 {
			h.delete(h.best())
		}
	}
}

// shape renders a tree in preorder as price, color (R or B) and children:
// "4B(2R,6R)". A nil child is "." and a leaf has no parentheses.
func shape(n *node) string {
	if n == nil {
		return "."
	}
	c := "B"
	if n.red {
		c = "R"
	}
	if n.left == nil && n.right == nil {
		return fmt.Sprintf("%d%s", n.Price, c)
	}
	return fmt.Sprintf("%d%s(%s,%s)", n.Price, c, shape(n.left), shape(n.right))
}

// TestKnownShapes pins the exact trees CLRS insertion produces for sorted
// input, worked out by hand case by case. For a bid side, inserting 10
// down to 1 puts each new level last, as 1 to 10 does for an ask side, so
// both give the same shape.
func TestKnownShapes(t *testing.T) {
	asc := newLevels(false).(*rbTree)
	for p := Price(1); p <= 10; p++ {
		asc.Insert(p)
	}
	const want = "4B(2B(1B,3B),6B(5B,8R(7B,9B(.,10R))))"
	if got := shape(asc.root); got != want {
		t.Fatalf("asc 1..10:\n got %s\nwant %s", got, want)
	}

	desc := newLevels(true).(*rbTree)
	for p := Price(10); p >= 1; p-- {
		desc.Insert(p)
	}
	const wantDesc = "7B(9B(10B,8B),5B(6B,3R(4B,2B(.,1R))))"
	if got := shape(desc.root); got != wantDesc {
		t.Fatalf("desc 10..1:\n got %s\nwant %s", got, wantDesc)
	}

	// Deleting the root 4 (two children, successor 5 is not its child)
	// relinks 5 into the root and runs the delete fixup on 6's left.
	asc.Delete(4)
	if err := verify(asc); err != nil {
		t.Fatal(err)
	}
	if asc.root.Price != 5 || asc.Find(5) != &asc.root.Level {
		t.Fatalf("after Delete(4) the root is %d, want node 5 relinked", asc.root.Price)
	}
}

// TestDeterministic checks that the same operations give the same tree,
// node for node and color for color.
func TestDeterministic(t *testing.T) {
	build := func() string {
		tree := newLevels(true).(*rbTree)
		r := rand.New(rand.NewPCG(42, 0))
		present := map[Price]bool{}
		for range 20_000 { // toggle random prices in and out
			p := Price(1 + r.IntN(2_000))
			if present[p] {
				tree.Delete(p)
			} else {
				tree.Insert(p)
			}
			present[p] = !present[p]
		}
		return shape(tree.root)
	}
	if a, b := build(), build(); a != b {
		t.Fatal("two runs of the same operations built different trees")
	}
}

// TestPreconditionPanics checks that Insert of a present price and Delete of
// a missing one panic and leave the tree intact.
func TestPreconditionPanics(t *testing.T) {
	tree := newLevels(false).(*rbTree)
	for p := Price(1); p <= 20; p++ {
		tree.Insert(p)
	}
	before := shape(tree.root)
	mustPanic := func(what string, f func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Fatalf("%s did not panic", what)
			}
		}()
		f()
	}
	mustPanic("Insert(7) of a present price", func() { tree.Insert(7) })
	mustPanic("Delete(99) of a missing price", func() { tree.Delete(99) })
	mustPanic("Delete on an empty set", func() { newLevels(true).Delete(1) })
	if err := verify(tree); err != nil {
		t.Fatal(err)
	}
	if shape(tree.root) != before {
		t.Fatal("a refused call changed the tree")
	}
}

// TestVerifyCatchesCorruption makes sure verify is not vacuous: each
// deliberate corruption of a valid tree must be reported, with the right
// complaint.
func TestVerifyCatchesCorruption(t *testing.T) {
	// fresh builds a valid ask-side tree of 1..30; prices are inserted
	// ascending, so the tree has red leaves on its right edge.
	fresh := func() *rbTree {
		tree := newLevels(false).(*rbTree)
		for p := Price(1); p <= 30; p++ {
			tree.Insert(p)
		}
		if err := verify(tree); err != nil {
			t.Fatal(err)
		}
		return tree
	}
	find := func(tree *rbTree, ok func(*node) bool) *node {
		var hit *node
		var visit func(*node)
		visit = func(n *node) {
			if n == nil || hit != nil {
				return
			}
			if ok(n) {
				hit = n
				return
			}
			visit(n.left)
			visit(n.right)
		}
		visit(tree.root)
		if hit == nil {
			t.Fatal("no node fits the corruption")
		}
		return hit
	}
	isLeaf := func(n *node) bool { return n.left == nil && n.right == nil }

	cases := []struct {
		want    string
		corrupt func(*rbTree)
	}{
		{"root is red", func(tr *rbTree) { tr.root.red = true }},
		{"red child", func(tr *rbTree) {
			// A black node with a red child, painted red.
			find(tr, func(n *node) bool { return !n.red && n != tr.root && (isRed(n.left) || isRed(n.right)) }).red = true
		}},
		{"black height", func(tr *rbTree) {
			find(tr, func(n *node) bool { return n.red && isLeaf(n) }).red = false
		}},
		{"order", func(tr *rbTree) { tr.root.Price, tr.root.left.Price = tr.root.left.Price, tr.root.Price }},
		{"links", func(tr *rbTree) { tr.root.left.left.parent = tr.root }},
		{"links", func(tr *rbTree) { tr.root.parent = tr.root.left }},
		{"best", func(tr *rbTree) { tr.best = tr.root }},
		{"size", func(tr *rbTree) { tr.size++ }},
	}
	for _, c := range cases {
		tree := fresh()
		c.corrupt(tree)
		err := verify(tree)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("corruption %q: verify returned %v", c.want, err)
		}
	}
}
