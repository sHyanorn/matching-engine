package book

import (
	"slices"
	"testing"
)

// queueIDs returns the IDs in l's queue from head to tail, and checks on the
// way that prev links mirror next links, that every node points at l, and
// that count and totalQty match the queue.
func queueIDs(t *testing.T, l *levelNode) []OrderID {
	t.Helper()
	var ids []OrderID
	var prev *orderNode
	var total Qty
	for o := l.head; o != nil; o = o.next {
		if o.prev != prev {
			t.Fatalf("order %d: prev link does not point at the previous order", o.ID)
		}
		if o.level != l {
			t.Fatalf("order %d: level link does not point at its level", o.ID)
		}
		ids = append(ids, o.ID)
		total += o.Remaining
		prev = o
	}
	if l.tail != prev {
		t.Fatal("tail is not the last order")
	}
	if int(l.count) != len(ids) || l.totalQty != total {
		t.Fatalf("count, totalQty = %d, %d; queue holds %d orders totalling %d", l.count, l.totalQty, len(ids), total)
	}
	return ids
}

func newTestOrder(id OrderID, qty Qty) *orderNode {
	return &orderNode{Order: Order{ID: id, Side: Sell, Type: Limit, TIF: GTC, Price: 100, Qty: qty, Remaining: qty}}
}

func TestQueuePushBackKeepsArrivalOrder(t *testing.T) {
	l := &levelNode{priceLevel: priceLevel{price: 100}}
	if got := queueIDs(t, l); got != nil {
		t.Fatalf("empty level holds %v", got)
	}
	for id := OrderID(1); id <= 4; id++ {
		l.pushBack(newTestOrder(id, Qty(id)*10))
	}
	if got, want := queueIDs(t, l), []OrderID{1, 2, 3, 4}; !slices.Equal(got, want) {
		t.Fatalf("queue = %v, want %v", got, want)
	}
	if l.totalQty != 100 || l.count != 4 {
		t.Fatalf("totalQty, count = %d, %d; want 100, 4", l.totalQty, l.count)
	}
}

func TestQueueUnlinkOrder(t *testing.T) {
	tests := []struct {
		name   string
		remove []OrderID
		want   []OrderID
	}{
		{"head", []OrderID{1}, []OrderID{2, 3, 4}},
		{"middle", []OrderID{3}, []OrderID{1, 2, 4}},
		{"tail", []OrderID{4}, []OrderID{1, 2, 3}},
		{"head twice", []OrderID{1, 2}, []OrderID{3, 4}},
		{"tail then head", []OrderID{4, 1}, []OrderID{2, 3}},
		{"all, tail first", []OrderID{4, 3, 2, 1}, nil},
		{"all, middle first", []OrderID{2, 3, 1, 4}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := &levelNode{priceLevel: priceLevel{price: 100}}
			nodes := map[OrderID]*orderNode{}
			for id := OrderID(1); id <= 4; id++ {
				nodes[id] = newTestOrder(id, Qty(id))
				l.pushBack(nodes[id])
			}
			for _, id := range tt.remove {
				o := nodes[id]
				l.unlinkOrder(o)
				if o.prev != nil || o.next != nil || o.level != nil {
					t.Fatalf("order %d keeps links after unlinkOrder", id)
				}
			}
			if got := queueIDs(t, l); !slices.Equal(got, tt.want) {
				t.Fatalf("queue = %v, want %v", got, tt.want)
			}
			if len(tt.want) == 0 && (l.head != nil || l.tail != nil || l.count != 0 || l.totalQty != 0) {
				t.Fatal("emptied level is not empty")
			}
		})
	}
}

// After an order is unlinked, a new order joins behind the survivors: a
// cancel never gives anyone a better place than arrival order.
func TestQueuePushBackAfterUnlink(t *testing.T) {
	l := &levelNode{priceLevel: priceLevel{price: 100}}
	a, b := newTestOrder(1, 5), newTestOrder(2, 5)
	l.pushBack(a)
	l.pushBack(b)
	l.unlinkOrder(a)
	l.pushBack(a) // the same node may rejoin, as a fresh arrival
	if got, want := queueIDs(t, l), []OrderID{2, 1}; !slices.Equal(got, want) {
		t.Fatalf("queue = %v, want %v", got, want)
	}
}
