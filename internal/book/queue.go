package book

// orderNode is a resting order in its price level's queue. The queue is a
// doubly linked list in time priority: the head is the oldest order and
// trades first, new orders join at the tail. Holding prev and next in the
// node lets a cancel unlink an order in O(1) once the book's order index
// has found it, and holding level lets it update the level's totals and remove
// the level when it empties, without searching the tree.
type orderNode struct {
	Order
	prev, next *orderNode
	level      *levelNode
}

// pushBack appends o to the tail of l's queue, giving it the lowest time
// priority at this price, and adds its Remaining to the level's total. o must
// not be in any queue.
func (l *levelNode) pushBack(o *orderNode) {
	o.level = l
	o.prev = l.tail
	o.next = nil
	if l.tail == nil {
		l.head = o
	} else {
		l.tail.next = o
	}
	l.tail = o
	l.count++
	l.totalQty += o.Remaining
}

// unlinkOrder removes o from its level's queue in O(1) and subtracts its
// Remaining from the level's total. The level itself stays in the tree even
// if it is now empty; the caller removes it, because only the caller knows
// which side's tree it is in.
func (l *levelNode) unlinkOrder(o *orderNode) {
	if o.prev == nil {
		l.head = o.next
	} else {
		o.prev.next = o.next
	}
	if o.next == nil {
		l.tail = o.prev
	} else {
		o.next.prev = o.prev
	}
	l.count--
	l.totalQty -= o.Remaining
	// Clear the links so a stale pointer to o cannot reach the queue.
	o.prev, o.next, o.level = nil, nil, nil
}
