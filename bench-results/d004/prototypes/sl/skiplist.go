package d004

import "fmt"

// This file implements levelSet as a skip list (W. Pugh, "Skip Lists: A
// Probabilistic Alternative to Balanced Trees", CACM 1990).
//
// # Shape
//
// A skip list is a sorted linked list with extra "express lanes". Every node
// is on level 0, which links all levels best first. A node of height h is
// also on levels 1..h-1, and each higher level links only the nodes tall
// enough to reach it. An ask side holding 101, 102, 104, 105, 107 and 108
// might look like this:
//
//	level 2:  head ──────────────────────────► 105 ──────────────────► nil
//	level 1:  head ─────────► 102 ───────────► 105 ──────────► 108 ──► nil
//	level 0:  head ─► 101 ──► 102 ──► 104 ───► 105 ──► 107 ──► 108 ──► nil
//
// The head is a sentinel with a tower of maxHeight pointers and no price; it
// sits before every real node, so no link update needs a special case for
// "insert at the front".
//
// # Invariants
//
//  1. Level 0 holds every node exactly once, in strictly increasing side
//     order (see [skipList.before]): ascending prices for asks, descending
//     for bids. The first node on level 0 is therefore the best level.
//  2. Level k >= 1 holds exactly the nodes whose height is greater than k, in
//     the same order. Each level is thus a sublist of the one below it.
//  3. Every node's height is in [1, maxHeight]. It is chosen when the node is
//     inserted and never changes; the node never moves in memory, which is
//     what keeps the *Level handed out by Insert and Find valid.
//  4. height is the tallest node's height, 0 when the set is empty, and
//     head.next[k] is nil for every k >= height.
//  5. length is the number of nodes on level 0.
//
// # Search
//
// To find where price p belongs, start at the head on the highest level in
// use, move right while the next node's price comes before p, then drop one
// level and repeat. On level 0 the walk ends at the last node before p, and
// p is either its successor or absent. The node reached on each level is
// p's predecessor on that level, which is exactly the set of links Insert
// and Delete must rewrite.
//
// # Cost
//
// With tower heights drawn independently with P(height > h) = 4^-h, a search
// is expected to make about 3 moves per level over log4(P) levels, so Find,
// Insert and Delete are expected O(log P). The expectation is over the height
// generator, not over the prices: it holds for any sequence of operations
// that was not chosen by someone who can predict the generator (see
// Determinism below). A search for a price d levels away from the best makes
// expected O(log d) moves plus one comparison per level in use, because the
// head's first successor on each high level is usually already past p. Order
// flow concentrates near the best, so this is the common case.
//
// Best is O(1) because the best level is head.next[0]; Insert and Delete keep
// that link current as part of the ordinary relinking, so there is no
// separate cache to invalidate. Walk follows level 0, so visiting n levels
// costs O(n). The head has no price and is never compared, so every int64
// is a valid price; no value is reserved as a sentinel.
//
// # Determinism
//
// Tower heights come from a pseudo-random generator ([splitMix64]) owned by
// each set and seeded with a constant. Only Insert draws from it, exactly
// once per call, so a node's height depends only on how many levels the set
// had inserted before it, never on prices, Find or Walk. The same sequence
// of operations therefore always builds the same structure, on every run and
// replay. Results never depend on the heights at all: any seed gives the
// same contents, order and return values, only the speed differs.
//
// The weakness is that the heights are predictable. Someone who knows the
// seed and can count the new levels a side has created (an L2 market data
// feed shows them) knows the next height. They can create and cancel levels
// far from the best until the next height is 1, then create a level near
// the best, and repeat, building a long run of height-1 nodes that every
// search near the best must walk one by one: O(run length) instead of
// O(log P). TestPredictableHeightsCanBeAttacked measures this. Deriving
// heights from a hash of the price instead makes the structure independent
// of history, but is easier to attack (pick prices that hash to height 1;
// no state to track). A seed that outsiders cannot learn, passed to
// newSkipList from configuration and recorded with the command log for
// replay, removes the predictability without giving up determinism.

const (
	// branching is 1/p: a tower of height h grows to h+1 with probability
	// 1/branching. With 4 rather than 2 a node has 1/(1-1/4) = 1.33 links on
	// average instead of 2, and a search for P levels takes about the same
	// number of comparisons ((1/p)·log_{1/p}P = 2·log2 P for both), spread
	// over half as many levels. The price is a higher variance of the search
	// cost; Pugh recommends 1/4 unless that variance matters. Measured on
	// this package's benchmarks, p = 1/2 was 10-50% slower and used 10% more
	// memory.
	branching = 4

	// maxHeight caps tower heights and sizes the head. The useful height for
	// P levels is about log4(P): 10 for a million levels, the most a side
	// can have at the 1M-resting-order memory target. The cap only shortens
	// nodes that would have grown taller than 12, one in 4^11 ≈ 4.2M, so it
	// leaves the expected cost unchanged up to about 16M levels per side.
	// Searches start at the height in use, not at the cap, so a higher cap
	// would cost only head and stack space.
	maxHeight = 12

	// defaultSeed seeds the height generator of every set created by
	// newLevels. Any value works; it is fixed so that the same sequence of
	// operations always builds the same structure, on every run and replay.
	defaultSeed uint64 = 0xd004
)

// node is one price level in the skip list. Level is embedded so that the
// *Level handed to callers is a pointer into the node itself.
type node struct {
	Level

	// next[k] is the following node on level k, or nil at the end of that
	// level. len(next) is the node's height; it never changes.
	next []*node
}

// skipList is a levelSet ordered best first. The zero value is not usable;
// create one with newLevels or newSkipList.
type skipList struct {
	// descending orders the set by decreasing price (bids) instead of
	// increasing price (asks).
	descending bool

	// head is the sentinel before the first node, with a full tower of
	// maxHeight links. Its Level is unused.
	head *node

	// height is the number of levels in use: the tallest node's height, or
	// 0 when the set is empty. Searches start at level height-1.
	height int

	// length is the number of price levels in the set.
	length int

	// heights draws a tower height for each inserted node.
	heights splitMix64
}

// newLevels returns an empty set of price levels for one side of the book,
// ordered best first: descending prices for bids, ascending for asks. Every
// set starts from the same seed, so its structure depends only on the
// operations applied to it.
func newLevels(descending bool) levelSet { return newSkipList(descending, defaultSeed) }

// newSkipList is newLevels with an explicit seed for the height generator.
// Different seeds give different but equally valid structures; the contents,
// order and every result are the same whatever the seed.
func newSkipList(descending bool, seed uint64) *skipList {
	return &skipList{
		descending: descending,
		head:       &node{next: make([]*node, maxHeight)},
		heights:    splitMix64{state: seed},
	}
}

// before reports whether price a comes strictly before price b in the side's
// order, that is, whether a is the better price.
func (s *skipList) before(a, b Price) bool {
	if s.descending {
		return a > b
	}
	return a < b
}

// seek finds where price p belongs. For each level k in use it stores in
// update[k] the last node on level k whose price comes before p (the head if
// there is none); entries at or above s.height are left untouched. It
// returns the first node on level 0 whose price does not come before p,
// which is the node at p if p is present, or nil if every node comes before
// p.
func (s *skipList) seek(p Price, update *[maxHeight]*node) *node {
	x := s.head
	for k := s.height - 1; k >= 0; k-- {
		for x.next[k] != nil && s.before(x.next[k].Price, p) {
			x = x.next[k]
		}
		update[k] = x
	}
	return x.next[0]
}

// Find returns the level at p, or nil if there is none. Expected O(log P).
func (s *skipList) Find(p Price) *Level {
	var update [maxHeight]*node
	if n := s.seek(p, &update); n != nil && n.Price == p {
		return &n.Level
	}
	return nil
}

// Insert adds a new, empty level at p and returns it. The returned pointer
// stays valid until Delete(p). Expected O(log P).
//
// Inserting a price that is already present is a bug in the caller (the book
// must reuse the existing level), and Insert panics rather than corrupt the
// set.
func (s *skipList) Insert(p Price) *Level {
	var update [maxHeight]*node
	if n := s.seek(p, &update); n != nil && n.Price == p {
		panic(fmt.Sprintf("skiplist: Insert(%d): level already exists", p))
	}

	h := towerHeight(s.heights.next())
	// Levels the set has not used yet are empty, so on them the new node's
	// predecessor is the head.
	for k := s.height; k < h; k++ {
		update[k] = s.head
	}
	s.height = max(s.height, h)

	n := &node{Level: Level{Price: p}, next: make([]*node, h)}
	for k := range h {
		n.next[k] = update[k].next[k]
		update[k].next[k] = n
	}
	s.length++
	return &n.Level
}

// Delete removes the level at p. Expected O(log P); deleting the best level
// costs O(height), since every predecessor is then the head.
//
// Deleting a price that is not present is a bug in the caller, and Delete
// panics rather than ignore it.
//
// The removed node's links are cleared, so a stale *Level that a caller
// still holds cannot keep the rest of the list reachable.
func (s *skipList) Delete(p Price) {
	var update [maxHeight]*node
	n := s.seek(p, &update)
	if n == nil || n.Price != p {
		panic(fmt.Sprintf("skiplist: Delete(%d): no such level", p))
	}

	// n is on levels 0..len(n.next)-1, all below s.height, and on each of
	// them update[k] is its predecessor, so update[k].next[k] == n.
	for k, succ := range n.next {
		update[k].next[k] = succ
	}
	n.next = nil
	s.length--

	// If n was the only node on the top levels, those levels are now empty.
	for s.height > 0 && s.head.next[s.height-1] == nil {
		s.height--
	}
}

// Best returns the best level, or nil if the set is empty. O(1).
func (s *skipList) Best() *Level {
	if n := s.head.next[0]; n != nil {
		return &n.Level
	}
	return nil
}

// Walk calls fn on each level from the best outward until fn returns false
// or the levels run out. Visiting n levels is O(n). fn must not insert or
// delete levels.
func (s *skipList) Walk(fn func(*Level) bool) {
	for n := s.head.next[0]; n != nil; n = n.next[0] {
		if !fn(&n.Level) {
			return
		}
	}
}

// Len returns the number of levels. O(1).
func (s *skipList) Len() int { return s.length }

// towerHeight turns a uniformly random 64-bit word into a tower height in
// [1, maxHeight] with P(height > h) = branching^-h. It reads the word two
// bits (one base-4 digit) at a time, starting from the least significant:
// each zero digit, which has probability 1/4, adds one to the height. 64
// bits hold 32 digits, more than the 11 that maxHeight can use, so one word
// per insertion is enough.
func towerHeight(r uint64) int {
	h := 1
	for h < maxHeight && r%branching == 0 {
		h++
		r /= branching
	}
	return h
}

// splitMix64 is the SplitMix64 pseudo-random generator (Steele, Lea and
// Flood, OOPSLA 2014), with the constants of Vigna's reference
// implementation. It is small, fast, passes BigCrush, accepts any seed
// including zero, and its low bits are as good as its high bits, which
// towerHeight relies on. It is not cryptographically secure, and does not
// need to be: see "Determinism" at the top of this file.
type splitMix64 struct {
	state uint64
}

// next advances the generator and returns its next output.
func (g *splitMix64) next() uint64 {
	g.state += 0x9e3779b97f4a7c15
	z := g.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}
