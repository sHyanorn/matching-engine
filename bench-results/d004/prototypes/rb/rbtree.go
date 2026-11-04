package d004

// This file implements levelSet as a classic red-black tree with parent
// pointers, following Cormen, Leiserson, Rivest and Stein, "Introduction to
// Algorithms", 3rd edition, chapter 13 (CLRS below). The insert and delete
// fixups keep the CLRS case numbers so the code can be read side by side
// with the book.
//
// Why classic rather than left-leaning (Sedgewick's LLRB):
//
//   - Pointer stability. LLRB deletion of an inner node copies the
//     successor's key and value into the node being deleted. Here that would
//     move a Level and invalidate the *Level an earlier Insert or Find
//     handed out. CLRS deletion relinks the successor node into the deleted
//     node's place instead, so no Level ever moves.
//   - Fewer restructurings on the hot path. Emptied levels are deleted
//     constantly (every sweep of the best level, every cancel that empties
//     a level). CLRS delete does at most 3 rotations; LLRB delete rotates
//     and flips colors at every level on the way down the search path and
//     again on the way back up.
//   - Parent pointers. They give Walk an O(1) amortized step and give Delete
//     the next best level in O(1), with no stack and no recursion. LLRB is
//     usually written recursively without them.
//
// The price is more code: every fixup case has a mirror image, written out
// in full rather than folded together with direction-indexed children,
// because the mirrored form is what a reader can check against CLRS.
//
// How the two sides share one tree: the tree is ordered by priority, not by
// price. before(a, b) reports whether price a is better than price b on this
// side (lower for asks, higher for bids), and it is the only place the side
// matters. So the leftmost node is always the best level, Best returns a
// cached pointer to it, and Walk is a plain in-order traversal for both
// sides.
//
// The side lives in the comparator, not in stored negated keys: Level.Price
// must stay the real price, so stored keys would cost a second 8-byte field
// per node, and -p overflows at math.MinInt64. Inside the comparator the
// side is applied with a bit flip rather than an if (see before), because
// the benchmarks measured the if as a clear cost on every descent.

// rbTree is the ordered set of price levels for one side of the book, stored
// as a red-black tree whose in-order sequence runs from the best price to
// the worst.
//
// Invariants, checked after every operation by rbtree_internal_test.go:
//
//  1. Order: every node in n's left subtree is before (better than) n, and
//     every node in its right subtree is after (worse than) n. No two nodes
//     hold the same price.
//  2. Color: every node is red or black; nil children count as black.
//  3. The root is black.
//  4. A red node has no red child.
//  5. Every path from a node down to a nil child passes through the same
//     number of black nodes, the node's black height.
//  6. Links: root.parent is nil, and n.left.parent == n and
//     n.right.parent == n for every non-nil child.
//  7. Cache: best is the leftmost node (nil if the tree is empty), and size
//     is the number of nodes.
//
// Properties 4 and 5 bound the height by 2*log2(P+1) for P levels, which is
// where the O(log P) cost of Find, Insert and Delete comes from.
type rbTree struct {
	root *node
	best *node // leftmost node: the best price on this side
	size int
	// flip is 0 on the ask side (lowest price first) and -1, all bits set,
	// on the bid side (highest price first). Only before reads it.
	flip Price
}

// node is one price level in the tree. Level is embedded so that the *Level
// handed out by Insert and Find points into the node itself. The tree only
// ever relinks nodes and never copies a Level from one node to another, so
// that pointer stays valid until its own level is deleted.
type node struct {
	Level
	left, right, parent *node
	red                 bool // false means black; a nil *node is black
}

// newLevels returns an empty level set. descending selects the bid side,
// where the best price is the highest; otherwise it is the ask side, where
// the best price is the lowest.
func newLevels(descending bool) levelSet {
	t := &rbTree{}
	if descending {
		t.flip = -1
	}
	return t
}

// before reports whether price a has priority over price b on this side:
// a < b for asks, a > b for bids. It is the tree's only ordering decision.
//
// One comparison without a branch serves both sides. On the ask side flip
// is 0 and p^0 is p. On the bid side flip is -1 and p^-1 is ^p, which equals
// -1-p: it reverses the order of every int64 and, unlike -p, cannot
// overflow. Written as "if bids { return a > b }", the side test put a
// second branch in every step of every tree descent: Find, SweepBest and
// ChurnNearTop ran 4% to 18% slower, and Find over 100 levels took more
// than twice as long (D004 benchmarks).
func (t *rbTree) before(a, b Price) bool { return a^t.flip < b^t.flip }

// Find returns the level at p, or nil if there is none. O(log P).
func (t *rbTree) Find(p Price) *Level {
	if n := t.lookup(p); n != nil {
		return &n.Level
	}
	return nil
}

// lookup returns the node holding p, or nil.
func (t *rbTree) lookup(p Price) *node {
	n := t.root
	for n != nil && n.Price != p {
		if t.before(p, n.Price) {
			n = n.left
		} else {
			n = n.right
		}
	}
	return n
}

// Insert adds a new, empty level at p and returns it. O(log P), with at most
// two rotations.
//
// p must not already have a level. Insert panics if it does: two levels at
// one price would split that price's time priority and corrupt the book, so
// the caller's bug is stopped here rather than discovered later.
func (t *rbTree) Insert(p Price) *Level {
	// Walk down as in any binary search tree to the nil link where p
	// belongs.
	var parent *node
	for cur := t.root; cur != nil; {
		parent = cur
		switch {
		case p == cur.Price:
			panic("d004: Insert at a price that already has a level")
		case t.before(p, cur.Price):
			cur = cur.left
		default:
			cur = cur.right
		}
	}

	// Attach the new node there as a red leaf. A red leaf leaves every black
	// height unchanged (property 5); only property 4 (or 3, at the root) can
	// now be broken, and insertFixup repairs that.
	n := &node{Level: Level{Price: p}, parent: parent, red: true}
	switch {
	case parent == nil:
		t.root = n
	case t.before(p, parent.Price):
		parent.left = n
	default:
		parent.right = n
	}
	t.size++

	// Rotations never change the in-order sequence, so the new node is the
	// new best exactly when it beats the old best.
	if t.best == nil || t.before(p, t.best.Price) {
		t.best = n
	}

	t.insertFixup(n)
	return &n.Level
}

// insertFixup restores property 4 after the red node n was attached (CLRS
// RB-INSERT-FIXUP). While n and its parent are both red, n's parent is not
// the root (the root is black), so n has a grandparent, which is black. The
// cases depend on n's uncle, the grandparent's other child:
//
//   - Case 1, red uncle: recolor, which moves the red-red problem two levels
//     up. This is the only case that loops.
//   - Case 2, black uncle and n an inner child: rotate n outward, which
//     turns it into case 3.
//   - Case 3, black uncle and n an outer child: recolor and rotate the
//     grandparent, which ends the loop.
//
// So an insert does at most two rotations.
func (t *rbTree) insertFixup(n *node) {
	for isRed(n.parent) {
		parent := n.parent
		grand := parent.parent
		if parent == grand.left {
			uncle := grand.right
			if isRed(uncle) {
				// Case 1. Move grand's black down to both its children:
				// black heights are unchanged, and grand is now red and may
				// clash with its own parent.
				parent.red = false
				uncle.red = false
				grand.red = true
				n = grand
				continue
			}
			if n == parent.right {
				// Case 2. Rotate so the old parent becomes the outer child
				// of n, then handle that as case 3.
				n = parent
				t.rotateLeft(n)
				parent = n.parent
			}
			// Case 3. Rotate grand down to the right: parent takes its place
			// and its black color, and n and grand are parent's red children.
			parent.red = false
			grand.red = true
			t.rotateRight(grand)
		} else {
			// The mirror image of the branch above.
			uncle := grand.left
			if isRed(uncle) {
				// Case 1.
				parent.red = false
				uncle.red = false
				grand.red = true
				n = grand
				continue
			}
			if n == parent.left {
				// Case 2.
				n = parent
				t.rotateRight(n)
				parent = n.parent
			}
			// Case 3.
			parent.red = false
			grand.red = true
			t.rotateLeft(grand)
		}
	}
	// Case 1 may have painted the root red; painting it black adds one to
	// every black height at once, so property 5 still holds.
	t.root.red = false
}

// Delete removes the level at p. O(log P), with at most three rotations.
//
// p must have a level. Delete panics if it does not, for the same reason as
// Insert: the caller's view of the book no longer matches the book.
func (t *rbTree) Delete(p Price) {
	z := t.lookup(p)
	if z == nil {
		panic("d004: Delete of a price that has no level")
	}
	if z == t.best {
		// The next best level is z's in-order successor. The leftmost node
		// has no left child, so its right child is either nil or a red leaf
		// (properties 4 and 5), and the successor is that child or z's
		// parent: O(1).
		// The successor node is not moved by the removal below, so the
		// pointer stays good.
		t.best = successor(z)
	}
	t.remove(z)
	t.size--
}

// remove unlinks z from the tree and restores the red-black properties (CLRS
// RB-DELETE).
//
// If z has at most one child, that child (possibly nil) takes z's place.
// Otherwise z's successor y, the leftmost node of z's right subtree, has no
// left child: y's right child takes y's place, and then y takes z's place
// and z's color. Either way nodes are relinked, never copied, so the Level
// inside every other node stays where it is.
//
// The node that left its position is z in the first case and y in the
// second. If it was black, every path that went through that position is now
// one black short. deleteFixup repairs this starting at x, the node that
// moved into the position. x may be nil, so its parent is passed separately.
func (t *rbTree) remove(z *node) {
	var x, xParent *node
	removedRed := z.red
	switch {
	case z.left == nil:
		x, xParent = z.right, z.parent
		t.transplant(z, z.right)
	case z.right == nil:
		x, xParent = z.left, z.parent
		t.transplant(z, z.left)
	default:
		y := leftmost(z.right)
		removedRed = y.red
		x = y.right
		if y.parent == z {
			// y is z's right child and keeps its right subtree; x stays
			// under y.
			xParent = y
		} else {
			// Detach y, leaving its right child in its place, then give y
			// z's right subtree.
			xParent = y.parent
			t.transplant(y, y.right)
			y.right = z.right
			y.right.parent = y
		}
		t.transplant(z, y)
		y.left = z.left
		y.left.parent = y
		y.red = z.red
	}
	if !removedRed {
		t.deleteFixup(x, xParent)
	}
	// A caller may still hold a stale *Level into z by mistake. Clearing the
	// links stops that from keeping other nodes reachable.
	z.left, z.right, z.parent = nil, nil, nil
}

// deleteFixup restores property 5 after a black node was removed from the
// position now held by x, whose parent is parent (CLRS RB-DELETE-FIXUP).
//
// Think of x as carrying one extra black that must be placed somewhere. If x
// is red, or x is the root, painting x black settles it. Otherwise x is
// black (possibly nil) and its sibling w cannot be nil: before the removal,
// the paths through w had as many black nodes as the paths through the
// removed black node, which is at least one. That is also why
// "x == parent.left" is a safe test when x is nil: parent cannot have two
// nil children here. The cases are:
//
//   - Case 1, red w: rotate w above parent, so x gets a black sibling and
//     one of cases 2 to 4 applies.
//   - Case 2, black w with two black children: paint w red, which takes one
//     black off w's side too, and pass the extra black up to parent. This
//     is the only case that loops.
//   - Case 3, black w whose inner child is red and outer child black: rotate
//     w outward, which turns it into case 4.
//   - Case 4, black w whose outer child is red: rotate parent toward x and
//     recolor, which adds a black above x and keeps every other path the
//     same. Done.
//
// So a delete does at most three rotations (cases 1, 3 and 4 once each).
func (t *rbTree) deleteFixup(x, parent *node) {
	for x != t.root && !isRed(x) {
		if x == parent.left {
			w := parent.right
			if isRed(w) {
				// Case 1.
				w.red = false
				parent.red = true
				t.rotateLeft(parent)
				w = parent.right
			}
			if !isRed(w.left) && !isRed(w.right) {
				// Case 2. If parent is red (always so after case 1), the loop
				// ends and parent is painted black below.
				w.red = true
				x = parent
				parent = x.parent
				continue
			}
			if !isRed(w.right) {
				// Case 3.
				w.left.red = false
				w.red = true
				t.rotateRight(w)
				w = parent.right
			}
			// Case 4. w takes parent's place and color; parent and w's old
			// outer child become black.
			w.red = parent.red
			parent.red = false
			w.right.red = false
			t.rotateLeft(parent)
			return
		}
		// The mirror image of the branch above.
		w := parent.left
		if isRed(w) {
			// Case 1.
			w.red = false
			parent.red = true
			t.rotateRight(parent)
			w = parent.left
		}
		if !isRed(w.left) && !isRed(w.right) {
			// Case 2.
			w.red = true
			x = parent
			parent = x.parent
			continue
		}
		if !isRed(w.left) {
			// Case 3.
			w.right.red = false
			w.red = true
			t.rotateLeft(w)
			w = parent.left
		}
		// Case 4.
		w.red = parent.red
		parent.red = false
		w.left.red = false
		t.rotateRight(parent)
		return
	}
	if x != nil {
		x.red = false
	}
}

// Best returns the best level, or nil if the set is empty. O(1): the tree
// keeps a pointer to its leftmost node up to date on every Insert and
// Delete.
func (t *rbTree) Best() *Level {
	if t.best == nil {
		return nil
	}
	return &t.best.Level
}

// Walk calls fn on each level from the best outward until fn returns false.
// fn may change a level's fields other than Price, but must not insert or
// delete levels.
//
// Visiting the first k levels costs O(k + log P): an in-order traversal
// crosses each tree edge at most twice, once going down and once coming
// back up, and the only edges it touches are those among the visited nodes
// and on the path from them back toward the root.
func (t *rbTree) Walk(fn func(*Level) bool) {
	for n := t.best; n != nil; n = successor(n) {
		if !fn(&n.Level) {
			return
		}
	}
}

// Len returns the number of levels.
func (t *rbTree) Len() int { return t.size }

// leftmost returns the leftmost node, the best price, of the non-empty
// subtree rooted at n.
func leftmost(n *node) *node {
	for n.left != nil {
		n = n.left
	}
	return n
}

// successor returns the node after n in order, the next worse price, or nil
// if n is the last node.
func successor(n *node) *node {
	if n.right != nil {
		return leftmost(n.right)
	}
	// Climb until we arrive from a left child: that ancestor is next.
	for n.parent != nil && n == n.parent.right {
		n = n.parent
	}
	return n.parent
}

// isRed reports whether n is a red node. nil children count as black.
func isRed(n *node) bool { return n != nil && n.red }

// transplant puts the subtree rooted at v (possibly nil) where the subtree
// rooted at u is: under u's parent, or at the root (CLRS RB-TRANSPLANT). u's
// own links are left for the caller to update.
func (t *rbTree) transplant(u, v *node) {
	switch {
	case u.parent == nil:
		t.root = v
	case u == u.parent.left:
		u.parent.left = v
	default:
		u.parent.right = v
	}
	if v != nil {
		v.parent = u.parent
	}
}

// rotateLeft lifts x's right child y into x's place, with x as y's left
// child. y must not be nil.
//
//	  x              y
//	 / \            / \
//	a   y    =>    x   c
//	   / \        / \
//	  b   c      a   b
//
// The in-order sequence a x b y c is unchanged, so properties 1 and 7 still
// hold.
func (t *rbTree) rotateLeft(x *node) {
	y := x.right
	x.right = y.left
	if y.left != nil {
		y.left.parent = x
	}
	t.transplant(x, y)
	y.left = x
	x.parent = y
}

// rotateRight is the mirror image of rotateLeft: it lifts x's left child y
// into x's place, with x as y's right child. y must not be nil.
//
//	    x          y
//	   / \        / \
//	  y   c  =>  a   x
//	 / \            / \
//	a   b          b   c
func (t *rbTree) rotateRight(x *node) {
	y := x.left
	x.left = y.right
	if y.right != nil {
		y.right.parent = x
	}
	t.transplant(x, y)
	y.right = x
	x.parent = y
}
