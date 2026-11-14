package book

import (
	"errors"
	"fmt"
	"math"
)

// CheckInvariants verifies that the book's internal state is consistent and
// returns the first violation it finds, or nil. It checks:
//
//   - each side's price levels form a valid red-black tree ordered by
//     priority, with the cached best level and size correct (levelTree),
//     and that order is the side's real one: bids fall and asks rise from
//     the best level outward;
//   - no level in a tree is empty, and each level's order count and total
//     quantity equal those of its queue, whose links are consistent;
//   - every resting order is a Limit GTC order on its tree's side at its
//     level's price, with Remaining > 0 and Filled + Remaining = Qty;
//   - time priority: RestedSeq strictly increases from the head to the tail
//     of every queue and never exceeds LastSeq;
//   - the order index is consistent (orderIndex.check) and holds exactly
//     the orders in the queues;
//   - the book is not crossed: the best bid is below the best ask.
//
// It does not check what only the event history can show, such as quantity
// conservation across trades and cancels or deterministic replay; the
// property and fuzz tests check those from the event stream.
//
// It costs O(N + P) for N resting orders and P levels, so it is meant for
// tests and fuzzing, never for the matching hot path.
func (b *Book) CheckInvariants() error {
	if err := b.orders.check(); err != nil {
		return fmt.Errorf("order index: %w", err)
	}
	queued := 0
	for _, s := range [...]Side{Buy, Sell} {
		tree := b.side(s)
		if err := tree.check(); err != nil {
			return fmt.Errorf("%v levels: %w", s, err)
		}
		var prev *levelNode
		for l := tree.best; l != nil; l = l.next() {
			// check proves the tree ordered by its own comparator; this
			// proves that order is the side's real price order, with raw
			// comparisons, so best really is the best price.
			if prev != nil && (s == Buy && l.price >= prev.price || s == Sell && l.price <= prev.price) {
				return fmt.Errorf("%v levels: %d comes after %d", s, l.price, prev.price)
			}
			prev = l
			n, err := b.checkLevel(s, l)
			if err != nil {
				return fmt.Errorf("%v level %d: %w", s, l.price, err)
			}
			queued += n
		}
	}
	// Every queued order is in the index under its own ID (checkLevel), and
	// queued orders are distinct nodes; equal counts then mean the index
	// holds nothing else.
	if queued != b.orders.len() {
		return fmt.Errorf("order index holds %d orders, the queues %d", b.orders.len(), queued)
	}
	if bid, ask := b.bids.best, b.asks.best; bid != nil && ask != nil && Buy.Crosses(bid.price, ask.price) {
		return fmt.Errorf("book is crossed: best bid %d, best ask %d", bid.price, ask.price)
	}
	return nil
}

// checkLevel checks one level of side s and its queue, and returns how many
// orders the queue holds.
func (b *Book) checkLevel(s Side, l *levelNode) (int, error) {
	if l.head == nil {
		return 0, errors.New("empty level is still in the tree")
	}
	count := 0
	var total Qty
	var prev *orderNode
	for o := l.head; o != nil; o = o.next {
		switch {
		case o.prev != prev:
			return 0, fmt.Errorf("order %d: prev link does not point at the order before it", o.ID)
		case o.level != l:
			return 0, fmt.Errorf("order %d: level link does not point at its level", o.ID)
		case o.Side != s:
			return 0, fmt.Errorf("order %d: side %v in a %v level", o.ID, o.Side, s)
		case o.Price != l.price:
			return 0, fmt.Errorf("order %d: price %d in the level at %d", o.ID, o.Price, l.price)
		case o.Type != Limit || o.TIF != GTC:
			return 0, fmt.Errorf("order %d: a %v %v order is resting; only Limit GTC orders rest", o.ID, o.Type, o.TIF)
		case o.Remaining <= 0:
			return 0, fmt.Errorf("order %d: Remaining %d is not positive", o.ID, o.Remaining)
		case o.Filled < 0 || o.Filled+o.Remaining != o.Qty:
			return 0, fmt.Errorf("order %d: Filled %d + Remaining %d != Qty %d", o.ID, o.Filled, o.Remaining, o.Qty)
		case prev != nil && o.RestedSeq <= prev.RestedSeq:
			return 0, fmt.Errorf("order %d: time priority broken, RestedSeq %d after %d", o.ID, o.RestedSeq, prev.RestedSeq)
		case o.RestedSeq == 0 || o.RestedSeq > b.seq:
			return 0, fmt.Errorf("order %d: RestedSeq %d outside 1..LastSeq %d", o.ID, o.RestedSeq, b.seq)
		case b.orders.get(o.ID) != o:
			return 0, fmt.Errorf("order %d: the order index does not point at the queued order", o.ID)
		}
		count++
		total += o.Remaining
		prev = o
	}
	switch {
	case l.tail != prev:
		return 0, errors.New("tail is not the last order of the queue")
	case int(l.count) != count:
		return 0, fmt.Errorf("order count is %d, the queue holds %d", l.count, count)
	case l.totalQty != total:
		return 0, fmt.Errorf("total quantity is %d, the queue holds %d", l.totalQty, total)
	}
	return count, nil
}

// check verifies every invariant documented on levelTree and returns the
// first violation it finds, or nil. It costs O(P).
func (t *levelTree) check() error {
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
	// Not sized from t.size: a corrupted size must be reported, not make
	// the allocation panic or exhaust memory.
	var inorder []*levelNode
	height := 0
	var walk func(n, lo, hi *levelNode, depth int) (blackHeight int, err error)
	walk = func(n, lo, hi *levelNode, depth int) (int, error) {
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
		lh, err := walk(n.left, lo, n, depth+1)
		if err != nil {
			return 0, err
		}
		inorder = append(inorder, n)
		rh, err := walk(n.right, n, hi, depth+1)
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
	if _, err := walk(t.root, nil, nil, 0); err != nil {
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

// priceOf returns n's price for an error message, or nil for no node.
func priceOf(n *levelNode) any {
	if n == nil {
		return nil
	}
	return n.price
}
