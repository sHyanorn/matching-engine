package book

import (
	"math/rand/v2"
	"strings"
	"testing"
)

// TestOrderIndexMatchesMap drives the index with random inserts, deletes
// and lookups, and checks it against a map after every operation, and with
// check whenever a growth starts or ends and every 997 operations, which
// land at varied points of a growth. The ID shapes cover
// consecutive IDs, the engine's IDs (a counter above the symbol index,
// D019), random 64-bit IDs and a small range that is reused, so deletes
// and reinserts meet tombstones while the index grows.
func TestOrderIndexMatchesMap(t *testing.T) {
	ops := 100_000
	if testing.Short() {
		ops = 10_000
	}
	for _, shape := range []struct {
		name string
		id   func(r *rand.Rand, counter uint64) OrderID
	}{
		{"consecutive", func(_ *rand.Rand, c uint64) OrderID { return OrderID(c) }},
		{"engine", func(_ *rand.Rand, c uint64) OrderID { return OrderID(c<<8 | 3) }},
		{"random", func(r *rand.Rand, _ uint64) OrderID { return OrderID(r.Uint64() | 1) }},
		{"reused", func(r *rand.Rand, _ uint64) OrderID { return OrderID(1 + r.IntN(4096)) }},
	} {
		t.Run(shape.name, func(t *testing.T) {
			r := rand.New(rand.NewPCG(1, 2))
			x := newOrderIndex()
			model := map[OrderID]*orderNode{}
			var live []OrderID // the model's IDs, for picking one at random
			var counter uint64
			check := func(op int, what string, id OrderID) {
				t.Helper()
				if got, want := x.get(id), model[id]; got != want {
					t.Fatalf("op %d, %s %d: get = %p, want %p", op, what, id, got, want)
				}
				if x.len() != len(model) {
					t.Fatalf("op %d, %s %d: len = %d, want %d", op, what, id, x.len(), len(model))
				}
			}
			for op := range ops {
				wasGrowing := x.old.slots != nil
				switch p := r.IntN(100); {
				case p < 55:
					counter++
					id := shape.id(r, counter)
					if model[id] != nil {
						continue
					}
					next, growing := x.next, x.old.slots != nil
					o := &orderNode{}
					x.put(id, o)
					model[id] = o
					live = append(live, id)
					// One insert moves at most growStep old slots, and the
					// current table never fills past half.
					if growing && x.old.slots != nil && x.next-next > growStep {
						t.Fatalf("op %d: one insert moved %d slots", op, x.next-next)
					}
					if 2*x.len() > len(x.cur.slots) {
						t.Fatalf("op %d: %d entries in a table of %d", op, x.len(), len(x.cur.slots))
					}
					check(op, "put", id)
				case p < 90:
					var id OrderID
					switch q := r.IntN(10); {
					case q < 7 && len(live) > 0:
						i := r.IntN(len(live))
						id = live[i]
						live[i] = live[len(live)-1]
						live = live[:len(live)-1]
					case q < 9:
						id = shape.id(r, counter+1+uint64(r.IntN(100)))
						if model[id] != nil {
							continue
						}
					}
					x.del(id)
					delete(model, id)
					check(op, "del", id)
				default:
					id := shape.id(r, uint64(r.IntN(int(counter)+1)))
					check(op, "get", id)
				}
				if wasGrowing != (x.old.slots != nil) || op%997 == 0 {
					if err := x.check(); err != nil {
						t.Fatalf("op %d: %v", op, err)
					}
				}
			}
			seen := 0
			for id, o := range x.all() {
				if model[id] != o {
					t.Fatalf("all yields %d -> %p, want %p", id, o, model[id])
				}
				seen++
			}
			if seen != len(model) {
				t.Fatalf("all yields %d entries, want %d", seen, len(model))
			}
			t.Logf("%d live IDs in %d slots", len(model), len(x.cur.slots))
		})
	}
}

// TestIDTableDeleteKeepsRunsReachable fills a 16-slot table to half with
// IDs whose homes cluster at its end, so probe runs wrap around to the
// start, then deletes them in random orders: every remaining ID must stay
// reachable after each backward shift.
func TestIDTableDeleteKeepsRunsReachable(t *testing.T) {
	probe := newIDTable(16)
	var ids []OrderID // homes 14, 15, 0, 1 alternating
	for id, want := OrderID(1), 0; len(ids) < 8; id++ {
		if probe.home(id) == []int{14, 15, 14, 0, 15, 1, 14, 0}[want] {
			ids = append(ids, id)
			want++
		}
	}
	r := rand.New(rand.NewPCG(3, 4))
	for trial := range 2000 {
		tb := newIDTable(16)
		nodes := map[OrderID]*orderNode{}
		for _, id := range ids {
			nodes[id] = &orderNode{}
			tb.put(id, nodes[id])
		}
		for _, i := range r.Perm(len(ids)) {
			if !tb.del(ids[i]) {
				t.Fatalf("trial %d: del(%d) found nothing", trial, ids[i])
			}
			delete(nodes, ids[i])
			for id, o := range nodes {
				if tb.get(id) != o {
					t.Fatalf("trial %d: %d unreachable after deleting %d", trial, id, ids[i])
				}
			}
		}
		for i, s := range tb.slots {
			if s != (idSlot{}) {
				t.Fatalf("trial %d: slot %d not empty after deleting every ID", trial, i)
			}
		}
	}
}

// TestOrderIndexIDZero checks that 0, the empty-slot marker, is never
// found or deleted, and cannot be inserted.
func TestOrderIndexIDZero(t *testing.T) {
	x := newOrderIndex()
	o := &orderNode{}
	for id := OrderID(1); id <= 40; id++ { // grows: 40 > 64/2
		x.put(id, o)
	}
	if x.get(0) != nil {
		t.Fatal("get(0) found an entry")
	}
	x.del(0)
	if x.len() != 40 {
		t.Fatalf("del(0) changed len to %d", x.len())
	}
	if err := x.check(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if r := recover(); r == nil || !strings.HasPrefix(r.(string), "book:") {
			t.Fatalf("put(0) recovered %v, want a book: panic", r)
		}
	}()
	x.put(0, o)
}

func TestOrderIndexPutTwicePanics(t *testing.T) {
	x := newOrderIndex()
	x.put(7, &orderNode{})
	defer func() {
		if r := recover(); r == nil || !strings.HasPrefix(r.(string), "book:") {
			t.Fatalf("second put(7) recovered %v, want a book: panic", r)
		}
	}()
	x.put(7, &orderNode{})
}

func TestBookOrderZero(t *testing.T) {
	b := New()
	if _, ok := b.Order(0); ok {
		t.Fatal("Order(0) on an empty book reports an order")
	}
	for id := OrderID(1); id <= 40; id++ {
		b.Apply(limit(id, Buy, Price(id), 1), nil)
	}
	if _, ok := b.Order(0); ok {
		t.Fatal("Order(0) reports an order")
	}
}

// TestOrderIndexCheckCatchesCorruption makes sure check is not vacuous:
// each corruption of a valid index, some of them in the middle of growth,
// must be reported with the right complaint.
func TestOrderIndexCheckCatchesCorruption(t *testing.T) {
	// fresh returns an index holding IDs 1 to n; with n = 33 it has just
	// started growing out of its first table.
	fresh := func(n OrderID) *orderIndex {
		x := newOrderIndex()
		for id := OrderID(1); id <= n; id++ {
			x.put(id, &orderNode{})
		}
		if err := x.check(); err != nil {
			t.Fatalf("fresh index: %v", err)
		}
		return &x
	}
	// emptyAfter returns the first empty slot of t after slot i.
	emptyAfter := func(t *idTable, i int) int {
		for {
			i = (i + 1) & (len(t.slots) - 1)
			if t.slots[i].id == 0 {
				return i
			}
		}
	}
	cases := []struct {
		want    string
		corrupt func(x *orderIndex)
	}{
		{"counts 11 entries, holds 10", func(x *orderIndex) { x.n++ }},
		{"which a lookup of its ID does not find", func(x *orderIndex) {
			// Move 5 past an empty slot, out of reach of its probe run.
			i, _ := x.cur.find(5)
			j := emptyAfter(&x.cur, emptyAfter(&x.cur, i))
			x.cur.slots[j], x.cur.slots[i] = x.cur.slots[i], idSlot{}
		}},
		{"which a lookup of its ID does not find", func(x *orderIndex) {
			// A second entry for 5, with another node, after the first.
			i, _ := x.cur.find(5)
			x.cur.slots[emptyAfter(&x.cur, i)] = idSlot{5, &orderNode{}}
			x.n++
		}},
		{"holds a tombstone for 5 in the current table", func(x *orderIndex) {
			i, _ := x.cur.find(5)
			x.cur.slots[i].node = nil
			x.n--
		}},
		{"not growing, but next is not 0", func(x *orderIndex) { x.next = 3 }},
	}
	growing := []struct {
		want    string
		corrupt func(x *orderIndex)
	}{
		{"is in both tables", func(x *orderIndex) {
			// An ID still live in the old table, copied to the new one.
			for _, s := range x.old.slots[x.next:] {
				if s.node != nil {
					x.cur.put(s.id, s.node)
					x.n++
					return
				}
			}
			t.Fatal("no live old slot")
		}},
		{"but slots before", func(x *orderIndex) {
			// Skip past a live old slot that was never moved.
			for i, s := range x.old.slots {
				if i >= x.next && s.node != nil {
					x.next = i + 1
					return
				}
			}
			t.Fatal("no live old slot")
		}},
		{"old slot", func(x *orderIndex) {
			// Unreachable in the old table: move a live old entry past an
			// empty slot.
			for i, s := range x.old.slots {
				if i >= x.next && s.node != nil {
					j := emptyAfter(&x.old, emptyAfter(&x.old, i))
					x.old.slots[j], x.old.slots[i] = s, idSlot{}
					return
				}
			}
			t.Fatal("no live old slot")
		}},
	}
	for _, c := range cases {
		x := fresh(10)
		c.corrupt(x)
		if err := x.check(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("corruption %q: check returned %v", c.want, err)
		}
	}
	for _, c := range growing {
		x := fresh(33)
		if x.old.slots == nil {
			t.Fatal("index of 33 entries is not growing")
		}
		c.corrupt(x)
		if err := x.check(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("corruption %q while growing: check returned %v", c.want, err)
		}
	}
}
