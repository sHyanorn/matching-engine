package book

// EventKind says what an Event reports and which of its fields are used. The
// zero value is invalid.
type EventKind uint8

// The event kinds. The numbers are part of the output contract: append new
// kinds, never renumber.
const (
	// EventAccepted: a new order passed every check and entered matching.
	EventAccepted EventKind = 1
	// EventRejected: a command was refused and changed nothing.
	EventRejected EventKind = 2
	// EventTrade: the incoming (taker) order traded with one resting (maker)
	// order.
	EventTrade EventKind = 3
	// EventRested: the remainder of a GTC limit order joined the book.
	EventRested EventKind = 4
	// EventCanceled: an order's remaining quantity left without trading,
	// either from the book or before it could rest.
	EventCanceled EventKind = 5
)

var eventKindNames = [...]string{
	EventAccepted: "accepted",
	EventRejected: "rejected",
	EventTrade:    "trade",
	EventRested:   "rested",
	EventCanceled: "canceled",
}

// String returns the snake_case name, such as "trade" (also the WebSocket
// message type), or "EventKind(n)" for an invalid value.
func (k EventKind) String() string { return enumString(eventKindNames[:], k, "EventKind") }

// Event is one fact emitted by the book; the package documentation gives the
// order in which events appear. Every kind shares this one flat struct of
// integers (64 bytes). It is comparable with ==, so tests and the replay
// check compare whole event slices with slices.Equal, and the book appends it
// to a caller-owned slice without allocating, which one struct per kind boxed
// in an interface could not do. LevelOrders fills the padding after the four
// one-byte fields, so it costs no space.
//
// Fields used by each Kind; every other field is zero:
//
//	Kind      OrderID  MakerID  Side        Price             Qty        Reason  LevelSide, LevelQty, LevelOrders
//	Accepted  order    -        order side  limit; market: 0  submitted  -       -
//	Rejected  command  -        -           -                 -          why     -
//	Trade     taker    maker    taker side  maker's price     traded     -       maker's level
//	Rested    order    -        order side  limit             resting    -       order's level
//	Canceled  order    -        order side  limit; market: 0  removed    why     order's level, if it rested
//
// Seq and Timestamp are set on every event. A Rejected event echoes the
// command's OrderID, which may be 0 and says nothing about any resting order
// with that ID. A Canceled event's Reason is [ReasonUserCancel] for a resting
// order and [ReasonIOCRemainder] for an IOC or market remainder, which never
// rested.
//
// LevelSide, LevelQty and LevelOrders put L2 changes into the stream. An
// event that changed the resting book sets LevelSide to the side of the one
// level it changed, and LevelQty and LevelOrders to that level's total
// resting quantity and number of orders after the event (0 means the level
// is now empty); the level's price is always Price.
// Totals are absolute, not differences, so a consumer such as the engine
// building book_delta keeps the last value per level and needs no copy of
// the book. LevelSide 0 means the event did not change the book.
// [Event.LevelUpdate] returns these fields as one value.
type Event struct {
	// Seq numbers the events of one book 1, 2, 3, ... with no gaps,
	// rejections included. A trade's Seq is its trade number.
	Seq uint64
	// Timestamp is Command.Timestamp of the command that caused the event.
	Timestamp int64

	Kind        EventKind
	Side        Side
	Reason      Reason
	LevelSide   Side
	LevelOrders uint32

	OrderID  OrderID
	MakerID  OrderID
	Price    Price
	Qty      Qty
	LevelQty Qty
}

// Trade is one execution between the incoming (taker) order and one resting
// (maker) order. It is the typed view of an EventTrade returned by
// [Event.Trade], for consumers that only care about fills: the WebSocket
// trade channel, trade history and tests that assert on fills.
type Trade struct {
	Seq       uint64 // the event's Seq, which is also the trade number
	Timestamp int64
	TakerID   OrderID
	MakerID   OrderID
	Price     Price // always the maker's limit price
	Qty       Qty
	TakerSide Side // the aggressor; the maker is on TakerSide.Opposite()
}

// Trade returns the trade e reports, and false if e is not an EventTrade.
func (e Event) Trade() (Trade, bool) {
	if e.Kind != EventTrade {
		return Trade{}, false
	}
	return Trade{
		Seq:       e.Seq,
		Timestamp: e.Timestamp,
		TakerID:   e.OrderID,
		MakerID:   e.MakerID,
		Price:     e.Price,
		Qty:       e.Qty,
		TakerSide: e.Side,
	}, true
}

// LevelUpdate is the state of one price level right after an event changed
// it: the total resting quantity at Price on Side and the number of orders
// making it up, where Qty 0 (and so Orders 0) means the level is now empty.
type LevelUpdate struct {
	Side   Side
	Price  Price
	Qty    Qty
	Orders uint32
}

// LevelUpdate returns the price level e changed, and false if e did not
// change the resting book (its LevelSide is not a valid side).
func (e Event) LevelUpdate() (LevelUpdate, bool) {
	if !e.LevelSide.Valid() {
		return LevelUpdate{}, false
	}
	return LevelUpdate{Side: e.LevelSide, Price: e.Price, Qty: e.LevelQty, Orders: e.LevelOrders}, true
}
