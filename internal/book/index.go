package book

import (
	"errors"
	"fmt"
	"iter"
)

// orderIndex maps the ID of every resting order to its node, for O(1)
// cancels and duplicate checks (D038).
//
// It is a hash table with open addressing and linear probing. An ID's home
// slot is the top bits of the ID times 2^64/φ (Fibonacci hashing), which
// spreads consecutive IDs and IDs that share their low bits, as the
// engine's do (D019). A delete shifts the later entries of its probe run
// back into the hole (backward-shift deletion), so the table needs no
// tombstones of its own. ID 0 marks an empty slot: the book never rests an
// order with ID 0, which Validate rejects, and a lookup of 0 finds nothing.
//
// The table grows when it is half full. Copying it at once would stall one
// command for milliseconds at a large size, so growth is gradual: a table
// twice the size takes every new entry, and each insert moves growStep
// slots of the old table into it. Until the old table has been emptied,
// lookups try the new table first, and an entry deleted or moved out of the
// old table leaves a tombstone there, its ID with a nil node, so the old
// table's probe runs stay unbroken. Each live ID is in exactly one table.
// The new table is still allocated in one piece: when the runtime reuses
// heap memory for it, clearing that memory costs the insert that starts
// the growth time in proportion to the table's size.
type orderIndex struct {
	cur  idTable
	old  idTable // the table being emptied; no slots when not growing
	next int     // the first slot of old not yet moved
	n    int     // live entries in both tables
}

// growStep is how many slots of the old table each insert moves while the
// index grows. Growth starts when n reaches half of the old table's C
// slots; the new table has 2C, so it fills to half only after C/2 more
// inserts. Any step of at least 2 empties the old table before then; 8
// does it within C/8 inserts and bounds the extra work of one insert.
const growStep = 8

// minIndexSlots is the size of a new book's table; a power of two.
const minIndexSlots = 64

func newOrderIndex() orderIndex {
	return orderIndex{cur: newIDTable(minIndexSlots)}
}

// len returns the number of IDs in the index.
func (x *orderIndex) len() int { return x.n }

// get returns the node indexed under id, or nil.
func (x *orderIndex) get(id OrderID) *orderNode {
	if o := x.cur.get(id); o != nil {
		return o
	}
	if x.old.slots == nil {
		return nil
	}
	return x.old.get(id) // nil for a tombstone too
}

// put indexes o under id, which must not be in the index and must not be
// 0, and moves growStep slots of the old table if the index is growing.
func (x *orderIndex) put(id OrderID, o *orderNode) {
	if id == 0 {
		panic("book: order ID 0 in the order index") // see D014
	}
	if x.old.slots == nil && 2*(x.n+1) > len(x.cur.slots) {
		x.old = x.cur
		x.cur = newIDTable(2 * len(x.old.slots))
	}
	x.cur.put(id, o)
	x.n++
	if x.old.slots != nil {
		x.move(growStep)
	}
}

// del removes id from the index; an ID that is not there, 0 included,
// changes nothing.
func (x *orderIndex) del(id OrderID) {
	if x.cur.del(id) || x.old.slots != nil && x.old.bury(id) {
		x.n--
	}
}

// move moves up to k slots of the old table into the current one, and
// drops the old table once all of its slots have been moved.
func (x *orderIndex) move(k int) {
	end := min(x.next+k, len(x.old.slots))
	for i := x.next; i < end; i++ {
		if s := &x.old.slots[i]; s.node != nil {
			x.cur.put(s.id, s.node)
			s.node = nil
		}
	}
	x.next = end
	if end == len(x.old.slots) {
		x.old, x.next = idTable{}, 0
	}
}

// all yields every ID in the index with its node, in no particular order.
// The index must not change during the iteration.
func (x *orderIndex) all() iter.Seq2[OrderID, *orderNode] {
	return func(yield func(OrderID, *orderNode) bool) {
		for _, t := range [...]*idTable{&x.cur, &x.old} {
			for _, s := range t.slots {
				if s.node != nil && !yield(s.id, s.node) {
					return
				}
			}
		}
	}
}

// check verifies the index's own invariants and returns the first
// violation it finds, or nil: every live entry is the one a lookup of its
// ID finds, so it is reachable and unique; the current table holds no
// tombstones; no ID is live in both tables; the old table holds nothing
// live before next; and n counts the live entries. It costs O(n) expected.
func (x *orderIndex) check() error {
	live := 0
	for i, s := range x.cur.slots {
		switch {
		case s.id == 0:
			continue
		case s.node == nil:
			return fmt.Errorf("slot %d holds a tombstone for %d in the current table", i, s.id)
		case x.cur.get(s.id) != s.node:
			return fmt.Errorf("slot %d holds %d, which a lookup of its ID does not find", i, s.id)
		case x.old.slots != nil && x.old.get(s.id) != nil:
			return fmt.Errorf("%d is in both tables", s.id)
		}
		live++
	}
	for i, s := range x.old.slots {
		switch {
		case s.node == nil:
			continue
		case i < x.next:
			return fmt.Errorf("old slot %d holds %d, but slots before %d have been moved", i, s.id, x.next)
		case x.old.get(s.id) != s.node:
			return fmt.Errorf("old slot %d holds %d, which a lookup of its ID does not find", i, s.id)
		}
		live++
	}
	if live != x.n {
		return fmt.Errorf("counts %d entries, holds %d", x.n, live)
	}
	if x.old.slots == nil && x.next != 0 {
		return errors.New("not growing, but next is not 0")
	}
	return nil
}

// idTable is one open-addressing table of an orderIndex.
type idTable struct {
	slots []idSlot // len is a power of two
	shift uint8    // 64 - log2(len(slots))
}

// idSlot is a slot of an idTable: empty when id is 0, a tombstone when id
// is set and node is nil (old tables only), an entry otherwise.
type idSlot struct {
	id   OrderID
	node *orderNode
}

func newIDTable(size int) idTable {
	shift := uint8(64)
	for s := size; s > 1; s >>= 1 {
		shift--
	}
	return idTable{slots: make([]idSlot, size), shift: shift}
}

// home returns the slot where a probe for id starts.
func (t *idTable) home(id OrderID) int {
	return int(uint64(id) * 0x9E3779B97F4A7C15 >> t.shift)
}

// find returns the slot of id and true, or the empty slot where its probe
// run ends and false. The table must have an empty slot.
func (t *idTable) find(id OrderID) (int, bool) {
	mask := len(t.slots) - 1
	for i := t.home(id); ; i = (i + 1) & mask {
		switch t.slots[i].id {
		case 0:
			return i, false
		case id:
			return i, true
		}
	}
}

// get returns the node of id, or nil if id is absent or a tombstone.
func (t *idTable) get(id OrderID) *orderNode {
	i, ok := t.find(id)
	if !ok {
		return nil
	}
	return t.slots[i].node
}

// put stores id, which must be absent, in the empty slot ending its run.
func (t *idTable) put(id OrderID, o *orderNode) {
	i, ok := t.find(id)
	if ok {
		panic("book: order ID already in the order index") // see D014
	}
	t.slots[i] = idSlot{id, o}
}

// del removes id by backward-shift deletion and reports whether it was
// there. Each later entry of the run whose home is not between the hole
// and itself moves back into the hole, which then moves to its slot, so
// every remaining entry stays reachable from its home.
func (t *idTable) del(id OrderID) bool {
	i, ok := t.find(id)
	if !ok {
		return false
	}
	mask := len(t.slots) - 1
	for j := (i + 1) & mask; t.slots[j].id != 0; j = (j + 1) & mask {
		// The entry at j may fill the hole at i if i lies on its probe
		// path, from its home h up to j: (j-h) >= (j-i), cyclically.
		if (j-t.home(t.slots[j].id))&mask >= (j-i)&mask {
			t.slots[i] = t.slots[j]
			i = j
		}
	}
	t.slots[i] = idSlot{}
	return true
}

// bury turns the live entry of id into a tombstone and reports whether
// there was one. Other entries stay where they are.
func (t *idTable) bury(id OrderID) bool {
	i, ok := t.find(id)
	if !ok || t.slots[i].node == nil {
		return false
	}
	t.slots[i].node = nil
	return true
}
