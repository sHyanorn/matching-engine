package book

// Book is the limit order book of one symbol: two trees of price levels, one
// per side, each level a FIFO queue of resting orders, plus a map from order
// ID to resting order for O(1) cancels. It is a single-threaded state
// machine: the engine owns one Book per symbol and calls Apply from one
// goroutine (D003), so nothing here takes a lock.
//
// The zero value is not usable; call [New].
type Book struct {
	bids   levelTree // best (highest) bid first
	asks   levelTree // best (lowest) ask first
	orders map[OrderID]*orderNode
	seq    uint64 // Seq of the last event emitted
}

// New returns an empty book.
func New() *Book {
	return &Book{
		bids:   newLevelTree(Buy),
		asks:   newLevelTree(Sell),
		orders: make(map[OrderID]*orderNode),
	}
}

// Apply validates cmd, applies it, and appends the events it produced to out,
// which it returns, in the style of strconv.AppendInt:
//
//	buf = b.Apply(cmd, buf[:0])
//
// The events follow the grammar in the package documentation. A command that
// fails [Command.Validate] produces one Rejected event and changes nothing
// else, so Apply is safe on arbitrary input. Once out has grown large enough,
// Apply allocates only when an order comes to rest: its node, a level if the
// price is new, and now and then room for a larger ID map.
//
// The returned slice aliases out: the next call overwrites it, so a caller
// that hands events to another goroutine must copy them first.
func (b *Book) Apply(cmd Command, out []Event) []Event {
	if r := cmd.Validate(); r != ReasonNone {
		return b.reject(out, cmd, r)
	}
	switch cmd.Kind {
	case CommandNewOrder:
		return b.newOrder(out, cmd)
	case CommandCancel:
		return b.cancel(out, cmd)
	}
	panic("book: Validate accepted an unknown command kind") // see D014
}

// Order returns a copy of the resting order with this ID, and false if no
// such order is resting. Filled and canceled orders are not kept (D013).
func (b *Book) Order(id OrderID) (Order, bool) {
	o, ok := b.orders[id]
	if !ok {
		return Order{}, false
	}
	return o.Order, true
}

// LastSeq returns the Seq of the last event the book emitted, or 0 for a new
// book. A snapshot taken now reflects every event up to and including it.
func (b *Book) LastSeq() uint64 { return b.seq }

// side returns the tree of resting orders on side s. s must be valid.
func (b *Book) side(s Side) *levelTree {
	if s == Buy {
		return &b.bids
	}
	return &b.asks
}

// emit numbers e, stamps it with the command's timestamp and appends it.
// Every event goes through here, which is what makes Seq gap-free.
func (b *Book) emit(out []Event, ts int64, e Event) []Event {
	b.seq++
	e.Seq = b.seq
	e.Timestamp = ts
	return append(out, e)
}

func (b *Book) reject(out []Event, cmd Command, r Reason) []Event {
	return b.emit(out, cmd.Timestamp, Event{Kind: EventRejected, OrderID: cmd.OrderID, Reason: r})
}

// newOrder handles a valid new-order command: stateful checks, then matching
// against the opposite side, then the remainder rests or is canceled.
func (b *Book) newOrder(out []Event, cmd Command) []Event {
	if _, dup := b.orders[cmd.OrderID]; dup {
		return b.reject(out, cmd, ReasonDuplicateOrderID)
	}
	if cmd.TIF == FOK && b.available(cmd) < cmd.Qty {
		// SPEC §6.4 and D010: all or nothing, decided before any trade.
		return b.reject(out, cmd, ReasonFOKUnfillable)
	}
	out = b.emit(out, cmd.Timestamp, Event{
		Kind: EventAccepted, OrderID: cmd.OrderID, Side: cmd.Side, Price: cmd.Price, Qty: cmd.Qty,
	})

	out, remaining := b.match(out, cmd)
	switch {
	case remaining == 0:
		// Fully filled: the order's last event was its last Trade.
		return out
	case cmd.Type == Limit && cmd.TIF == GTC:
		return b.rest(out, cmd, remaining)
	default:
		// IOC, or a market order (which is always IOC or FOK): what did not
		// trade at once is canceled and never rests.
		return b.emit(out, cmd.Timestamp, Event{
			Kind: EventCanceled, OrderID: cmd.OrderID, Side: cmd.Side, Price: cmd.Price,
			Qty: remaining, Reason: ReasonIOCRemainder,
		})
	}
}

// crosses reports whether an incoming order cmd may trade at price p: a
// market order trades at any price, a limit order at its limit or better.
func crosses(cmd Command, p Price) bool {
	return cmd.Type == Market || cmd.Side.Crosses(cmd.Price, p)
}

// match trades the incoming order cmd against the opposite side, best price
// first and in time priority within a price, until it is filled or the best
// opposite price no longer crosses. Every trade is at the resting (maker)
// order's price (SPEC §6.1). It returns the quantity left untraded.
func (b *Book) match(out []Event, cmd Command) ([]Event, Qty) {
	opposite := b.side(cmd.Side.Opposite())
	remaining := cmd.Qty
	for remaining > 0 {
		level := opposite.best
		if level == nil || !crosses(cmd, level.price) {
			break
		}
		for remaining > 0 && level.head != nil {
			maker := level.head
			q := min(remaining, maker.Remaining)
			remaining -= q
			maker.Filled += q
			maker.Remaining -= q
			level.totalQty -= q
			if maker.Remaining == 0 {
				level.unlinkOrder(maker) // subtracts the maker's Remaining, now 0
				delete(b.orders, maker.ID)
			}
			out = b.emit(out, cmd.Timestamp, Event{
				Kind: EventTrade, OrderID: cmd.OrderID, MakerID: maker.ID, Side: cmd.Side,
				Price: level.price, Qty: q, LevelSide: cmd.Side.Opposite(), LevelQty: level.totalQty,
				LevelOrders: level.count,
			})
		}
		if level.head == nil {
			opposite.remove(level)
		}
	}
	return out, remaining
}

// available returns the quantity resting on the opposite side at prices cmd
// crosses, adding whole levels from the best price outward until the total
// reaches at least cmd.Qty, so the result may exceed cmd.Qty. It is the FOK
// pre-check, and it walks only the levels a fill would consume.
func (b *Book) available(cmd Command) Qty {
	var total Qty
	for level := b.side(cmd.Side.Opposite()).best; level != nil && total < cmd.Qty; level = level.next() {
		if !crosses(cmd, level.price) {
			break
		}
		total += level.totalQty
	}
	return total
}

// rest puts the untraded remainder of a GTC limit order at the tail of its
// price level, creating the level if the price is new. match stopped because
// the opposite best no longer crosses, so the book stays uncrossed.
func (b *Book) rest(out []Event, cmd Command, remaining Qty) []Event {
	own := b.side(cmd.Side)
	level := own.find(cmd.Price)
	if level == nil {
		level = own.insert(cmd.Price)
	}
	o := &orderNode{Order: Order{
		ID: cmd.OrderID, Side: cmd.Side, Type: cmd.Type, TIF: cmd.TIF, Price: cmd.Price,
		Qty: cmd.Qty, Filled: cmd.Qty - remaining, Remaining: remaining, Timestamp: cmd.Timestamp,
	}}
	level.pushBack(o)
	b.orders[o.ID] = o
	out = b.emit(out, cmd.Timestamp, Event{
		Kind: EventRested, OrderID: o.ID, Side: o.Side, Price: o.Price, Qty: remaining,
		LevelSide: o.Side, LevelQty: level.totalQty, LevelOrders: level.count,
	})
	o.RestedSeq = b.seq // the Seq of the Rested event just emitted
	return out
}

// cancel removes a resting order: an O(1) map lookup and queue unlink, plus
// O(log P) to remove its level if the order was the last one there.
func (b *Book) cancel(out []Event, cmd Command) []Event {
	o, ok := b.orders[cmd.OrderID]
	if !ok {
		return b.reject(out, cmd, ReasonUnknownOrder)
	}
	level := o.level
	level.unlinkOrder(o)
	delete(b.orders, o.ID)
	if level.head == nil {
		b.side(o.Side).remove(level)
	}
	return b.emit(out, cmd.Timestamp, Event{
		Kind: EventCanceled, OrderID: o.ID, Side: o.Side, Price: o.Price, Qty: o.Remaining,
		Reason: ReasonUserCancel, LevelSide: o.Side, LevelQty: level.totalQty, LevelOrders: level.count,
	})
}
