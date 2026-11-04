package book

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

// checkTree checks every invariant documented on levelTree and returns the
// first violation it finds, or nil. It costs O(P).
func checkTree(t *levelTree) error {
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
	inorder := make([]*levelNode, 0, t.size)
	height := 0
	var check func(n, lo, hi *levelNode, depth int) (blackHeight int, err error)
	check = func(n, lo, hi *levelNode, depth int) (int, error) {
		if n == nil {
			height = max(height, depth)
			return 0, nil
		}
		if lo != nil && !t.before(lo.price, n.price) {
			return 0, fmt.Errorf("order: %d is in the right subtree of %d", n.price, lo.price)
		}
		if hi != nil && !t.before(n.price, hi.price) {
			return 0, fmt.Errorf("order: %d is in the left subtree of %d", n.price, hi.price)
		}
		if (n.left != nil && n.left.parent != n) || (n.right != nil && n.right.parent != n) {
			return 0, fmt.Errorf("links: a child of %d has the wrong parent", n.price)
		}
		if n.red && (isRed(n.left) || isRed(n.right)) {
			return 0, fmt.Errorf("red node %d has a red child", n.price)
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
			return 0, fmt.Errorf("black height: %d has %d on the left, %d on the right", n.price, lh, rh)
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
	var leftmostNode *levelNode
	if len(inorder) > 0 {
		leftmostNode = inorder[0]
	}
	if t.best != leftmostNode {
		return fmt.Errorf("best is %v, leftmost is %v", priceOf(t.best), priceOf(leftmostNode))
	}
	// Implied by properties 4 and 5; checked directly because it is the
	// O(log P) claim itself.
	if float64(height) > 2*math.Log2(float64(t.size+1)) {
		return fmt.Errorf("height %d exceeds 2*log2(%d+1)", height, t.size)
	}
	// next uses parent links, not recursion, so compare the walk from best
	// with the in-order sequence node by node.
	i := 0
	for n := t.best; n != nil; n = n.next() {
		if i >= len(inorder) || n != inorder[i] {
			return fmt.Errorf("next: visit %d is %d, not the in-order node", i, n.price)
		}
		i++
	}
	if i != len(inorder) {
		return fmt.Errorf("next: visited %d levels, tree has %d", i, len(inorder))
	}
	return nil
}

func priceOf(n *levelNode) any {
	if n == nil {
		return nil
	}
	return n.price
}

// treeHarness applies operations to a tree, checks every invariant after
// each one, and remembers every node handed out so it can check that each
// stays valid until its level is removed.
type treeHarness struct {
	tb     testing.TB
	name   string
	tree   levelTree
	side   Side
	levels map[Price]*levelNode // nodes handed out and not yet removed
	prices []Price              // the keys of levels, for drawing one at random
	step   int
}

func newTreeHarness(tb testing.TB, name string, side Side) *treeHarness {
	return &treeHarness{tb: tb, name: name, tree: newLevelTree(side), side: side, levels: map[Price]*levelNode{}}
}

// tag is a value stored in each level's totalQty, so a priceLevel that was
// copied into another node, rather than left in place, is detected.
func tag(p Price) Qty { return Qty(p)*7 + 3 }

func (h *treeHarness) has(p Price) bool { _, ok := h.levels[p]; return ok }

func (h *treeHarness) insert(p Price) {
	h.tb.Helper()
	n := h.tree.insert(p)
	if n == nil || n.price != p {
		h.tb.Fatalf("%s step %d: insert(%d) = %v", h.name, h.step, p, priceOf(n))
	}
	n.totalQty = tag(p)
	h.levels[p] = n
	h.prices = append(h.prices, p)
	h.after("insert", p)
}

func (h *treeHarness) remove(p Price) {
	h.tb.Helper()
	n := h.tree.find(p)
	if n != h.levels[p] {
		h.tb.Fatalf("%s step %d: find(%d) returned a different node than insert", h.name, h.step, p)
	}
	h.tree.remove(n)
	delete(h.levels, p)
	i := slices.Index(h.prices, p)
	h.prices[i] = h.prices[len(h.prices)-1]
	h.prices = h.prices[:len(h.prices)-1]
	h.after("remove", p)
}

// best returns the best price according to the harness's own records, not
// the tree's cache.
func (h *treeHarness) best() Price {
	b := h.prices[0]
	for _, p := range h.prices[1:] {
		if h.tree.before(p, b) {
			b = p
		}
	}
	return b
}

func (h *treeHarness) randomPresent(r *rand.Rand) Price { return h.prices[r.IntN(len(h.prices))] }

// after checks the tree after the operation op(p).
func (h *treeHarness) after(op string, p Price) {
	h.tb.Helper()
	h.step++
	if err := checkTree(&h.tree); err != nil {
		h.tb.Fatalf("%s step %d, after %s(%d): %v", h.name, h.step, op, p, err)
	}
	if h.tree.size != len(h.levels) {
		h.tb.Fatalf("%s step %d: size = %d, want %d", h.name, h.step, h.tree.size, len(h.levels))
	}
	// Pointer stability is O(P log P) to check, so do it every 32 steps,
	// and always while the tree is small and restructures the most.
	if h.step%32 == 0 || h.tree.size < 32 {
		h.checkPointers()
	}
}

func (h *treeHarness) checkPointers() {
	h.tb.Helper()
	for p, n := range h.levels {
		if f := h.tree.find(p); f != n {
			h.tb.Fatalf("%s step %d: find(%d) returned a different node than insert", h.name, h.step, p)
		}
		if n.price != p || n.totalQty != tag(p) {
			h.tb.Fatalf("%s step %d: level %d holds {%d %d}: its data moved", h.name, h.step, p, n.price, n.totalQty)
		}
	}
}

// Workloads. Each drives a harness with a seeded generator; between them
// they reach every case of both fixups on both sides (see the coverage
// profile).

// randomSmall keeps a small set (about 2/3 of a span of 32 prices), so the
// tree is often nearly empty and the fixups run near the root.
func randomSmall(h *treeHarness, r *rand.Rand) {
	for range 10_000 {
		p := Price(1 + r.IntN(32))
		switch {
		case !h.has(p):
			h.insert(p)
		case r.IntN(2) == 0:
			h.remove(p)
		}
	}
}

// growShrink inserts random prices from a wide span while mostly growing,
// then removes random levels until the set is empty.
func growShrink(h *treeHarness, r *rand.Rand) {
	const steps = 3_000
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
			h.remove(h.randomPresent(r))
		}
	}
	for len(h.prices) > 0 {
		h.remove(h.randomPresent(r))
	}
}

// churnNearTop mimics order flow: it holds about 200 levels, and each round
// either sweeps the best level or empties one near the top, then opens a new
// level near the top, sometimes better than the best.
func churnNearTop(h *treeHarness, r *rand.Rand) {
	const mid, n = 100_000, 200
	sign := Price(1) // worse prices are higher on the ask side
	if h.side == Buy {
		sign = -1
	}
	for i := range n {
		h.insert(mid + sign*Price(2*i))
	}
	for range 3_000 {
		best := h.best()
		if r.IntN(2) == 0 {
			h.remove(best)
		} else {
			for { // a present level at most 80 ticks behind the best
				p := best + sign*Price(r.IntN(80))
				if h.has(p) {
					h.remove(p)
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
func sequential(h *treeHarness, _ *rand.Rand) {
	const n = 300
	for p := Price(1); p <= n; p++ {
		h.insert(p)
	}
	for len(h.prices) > 0 {
		h.remove(h.best())
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
		h.remove(w)
	}
	for i := Price(0); i < n; i++ { // zig-zag: 150, 149, 151, 148, ...
		if i%2 == 0 {
			h.insert(n/2 + i/2)
		} else {
			h.insert(n/2 - 1 - i/2)
		}
	}
	for lo, hi := Price(1), Price(n-1); lo <= hi; lo, hi = lo+1, hi-1 {
		h.remove(hi)
		if lo != hi && h.has(lo) {
			h.remove(lo)
		}
	}
	for len(h.prices) > 0 {
		h.remove(h.prices[0])
	}
}

// TestLevelTreeInvariants applies every workload to both sides and checks
// all invariants after every single operation.
func TestLevelTreeInvariants(t *testing.T) {
	workloads := []struct {
		name string
		run  func(*treeHarness, *rand.Rand)
	}{
		{"randomSmall", randomSmall},
		{"growShrink", growShrink},
		{"churnNearTop", churnNearTop},
		{"sequential", sequential},
	}
	seeds := []uint64{1, 2}
	if testing.Short() {
		seeds = seeds[:1]
	}
	for _, w := range workloads {
		for _, side := range []Side{Sell, Buy} {
			for _, seed := range seeds {
				name := fmt.Sprintf("%s/%v/seed=%d", w.name, side, seed)
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					h := newTreeHarness(t, name, side)
					w.run(h, rand.New(rand.NewPCG(seed, 0x5eed)))
					h.checkPointers()
				})
			}
		}
	}
}

func TestLevelTreeEmpty(t *testing.T) {
	for _, side := range []Side{Buy, Sell} {
		tree := newLevelTree(side)
		if tree.size != 0 || tree.best != nil || tree.find(1) != nil {
			t.Fatalf("%v: new tree is not empty", side)
		}
		if err := checkTree(&tree); err != nil {
			t.Fatal(err)
		}
	}
}

// TestLevelTreeBefore checks the side comparator on every pair of boundary
// values, where negating a price would overflow, and then runs those values
// through both trees.
func TestLevelTreeBefore(t *testing.T) {
	ps := []Price{math.MinInt64, math.MinInt64 + 1, -2, -1, 0, 1, 2, math.MaxInt64 - 1, math.MaxInt64}
	asks, bids := newLevelTree(Sell), newLevelTree(Buy)
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
	for _, side := range []Side{Sell, Buy} {
		h := newTreeHarness(t, fmt.Sprintf("extremes/%v", side), side)
		for _, i := range []int{4, 0, 8, 1, 7, 2, 6, 3, 5} {
			h.insert(ps[i])
		}
		for len(h.prices) > 0 {
			h.remove(h.best())
		}
	}
}

// shape renders a tree in preorder as price, color (R or B) and children:
// "4B(2R,6R)". A nil child is "." and a leaf has no parentheses.
func shape(n *levelNode) string {
	if n == nil {
		return "."
	}
	c := "B"
	if n.red {
		c = "R"
	}
	if n.left == nil && n.right == nil {
		return fmt.Sprintf("%d%s", n.price, c)
	}
	return fmt.Sprintf("%d%s(%s,%s)", n.price, c, shape(n.left), shape(n.right))
}

// TestLevelTreeKnownShapes pins the exact trees CLRS insertion produces for
// sorted input, worked out by hand case by case. On the bid side, inserting
// 10 down to 1 puts each new level last, as 1 to 10 does on the ask side, so
// both give the same shape.
func TestLevelTreeKnownShapes(t *testing.T) {
	asc := newLevelTree(Sell)
	for p := Price(1); p <= 10; p++ {
		asc.insert(p)
	}
	const want = "4B(2B(1B,3B),6B(5B,8R(7B,9B(.,10R))))"
	if got := shape(asc.root); got != want {
		t.Fatalf("asks 1..10:\n got %s\nwant %s", got, want)
	}

	desc := newLevelTree(Buy)
	for p := Price(10); p >= 1; p-- {
		desc.insert(p)
	}
	const wantDesc = "7B(9B(10B,8B),5B(6B,3R(4B,2B(.,1R))))"
	if got := shape(desc.root); got != wantDesc {
		t.Fatalf("bids 10..1:\n got %s\nwant %s", got, wantDesc)
	}

	// Removing the root 4 (two children, successor 5 is not its child)
	// relinks node 5 into the root and runs the delete fixup on 6's left.
	five := asc.find(5)
	asc.remove(asc.find(4))
	if err := checkTree(&asc); err != nil {
		t.Fatal(err)
	}
	if asc.root != five {
		t.Fatalf("after removing 4 the root is %d, want node 5 relinked", asc.root.price)
	}
}

// TestLevelTreeDeterministic checks that the same operations give the same
// tree, node for node and color for color.
func TestLevelTreeDeterministic(t *testing.T) {
	build := func() string {
		tree := newLevelTree(Buy)
		r := rand.New(rand.NewPCG(42, 0))
		for range 20_000 { // toggle random prices in and out
			p := Price(1 + r.IntN(2_000))
			if n := tree.find(p); n != nil {
				tree.remove(n)
			} else {
				tree.insert(p)
			}
		}
		return shape(tree.root)
	}
	if a, b := build(), build(); a != b {
		t.Fatal("two runs of the same operations built different trees")
	}
}

// TestLevelTreeInsertDuplicatePanics checks that inserting a present price
// panics and leaves the tree intact.
func TestLevelTreeInsertDuplicatePanics(t *testing.T) {
	tree := newLevelTree(Sell)
	for p := Price(1); p <= 20; p++ {
		tree.insert(p)
	}
	before := shape(tree.root)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("insert(7) of a present price did not panic")
			}
		}()
		tree.insert(7)
	}()
	if err := checkTree(&tree); err != nil {
		t.Fatal(err)
	}
	if shape(tree.root) != before {
		t.Fatal("the refused insert changed the tree")
	}
}

// TestCheckTreeCatchesCorruption makes sure checkTree is not vacuous: each
// deliberate corruption of a valid tree must be reported, with the right
// complaint.
func TestCheckTreeCatchesCorruption(t *testing.T) {
	// fresh builds a valid ask-side tree of 1..30; prices are inserted
	// ascending, so the tree has red leaves on its right edge.
	fresh := func() *levelTree {
		tree := newLevelTree(Sell)
		for p := Price(1); p <= 30; p++ {
			tree.insert(p)
		}
		if err := checkTree(&tree); err != nil {
			t.Fatal(err)
		}
		return &tree
	}
	find := func(tree *levelTree, ok func(*levelNode) bool) *levelNode {
		var hit *levelNode
		var visit func(*levelNode)
		visit = func(n *levelNode) {
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
	isLeaf := func(n *levelNode) bool { return n.left == nil && n.right == nil }

	cases := []struct {
		want    string
		corrupt func(*levelTree)
	}{
		{"root is red", func(tr *levelTree) { tr.root.red = true }},
		{"red child", func(tr *levelTree) {
			// A black node with a red child, painted red.
			find(tr, func(n *levelNode) bool { return !n.red && n != tr.root && (isRed(n.left) || isRed(n.right)) }).red = true
		}},
		{"black height", func(tr *levelTree) {
			find(tr, func(n *levelNode) bool { return n.red && isLeaf(n) }).red = false
		}},
		{"order", func(tr *levelTree) { tr.root.price, tr.root.left.price = tr.root.left.price, tr.root.price }},
		{"links", func(tr *levelTree) { tr.root.left.left.parent = tr.root }},
		{"links", func(tr *levelTree) { tr.root.parent = tr.root.left }},
		{"best", func(tr *levelTree) { tr.best = tr.root }},
		{"size", func(tr *levelTree) { tr.size++ }},
	}
	for _, c := range cases {
		tree := fresh()
		c.corrupt(tree)
		err := checkTree(tree)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("corruption %q: checkTree returned %v", c.want, err)
		}
	}
}
