package book

// OrderID identifies an order. The engine assigns it before the command is
// logged, so replay sees the same IDs, and keeps IDs unique across symbols
// for the life of the log, so that DELETE and GET /v1/orders/{id}, which name
// no symbol, can be routed. Zero is never a valid ID.
//
// The book treats IDs as opaque: priority never depends on them, and it can
// only detect a duplicate among the orders it currently holds. Reusing the ID
// of a filled or canceled order is an engine bug the book cannot see.
type OrderID uint64

// Price is a price in ticks, the instrument's minimum price increment. A limit
// price is in [1, MaxPrice]; a market order has no limit and carries Price 0.
//
// Price is a defined type over int64 (D002): an integer, never a float, but
// the compiler stops a Price variable from being used as a Qty.
type Price int64

// Qty is a quantity in lots, the instrument's minimum size increment. An
// order quantity is in [1, MaxQty]; level and side totals are Qty too.
type Qty int64

// Bounds enforced by [Command.Validate]. They make int64 overflow impossible
// rather than something every sum has to check for:
//
//   - MaxPrice * MaxQty = 1e18 < math.MaxInt64 (about 9.2e18), so the
//     notional value of any single fill fits in an int64.
//   - A resting order holds at most MaxQty, so a level or side total over n
//     orders is at most n * 1e9. That stays below 2^53, and so is exact as a
//     JSON number in JavaScript clients, for up to 9 million resting orders
//     (nine times the 1M-order memory target), and cannot overflow int64
//     below 9.2e9 orders. The FOK pre-check sums level totals, so the same
//     bound covers it.
//
// Raising a bound later keeps old command logs replayable, because every
// command that was valid stays valid. Lowering one does not.
const (
	MaxPrice Price = 1_000_000_000
	MaxQty   Qty   = 1_000_000_000
)

// Side is the side of the book an order belongs to. The zero value is
// invalid, so a forgotten field or a missing JSON field is rejected instead
// of silently meaning "buy".
type Side uint8

// The sides. The numbers are stored in the command log: never renumber.
const (
	Buy  Side = 1
	Sell Side = 2
)

var sideNames = [...]string{Buy: "buy", Sell: "sell"}

// Valid reports whether s is Buy or Sell.
func (s Side) Valid() bool { return s == Buy || s == Sell }

// Opposite returns the side that s trades against: Sell for Buy and Buy for
// Sell. It returns the invalid zero Side for an invalid s.
func (s Side) Opposite() Side {
	switch s {
	case Buy:
		return Sell
	case Sell:
		return Buy
	default:
		return 0
	}
}

// Crosses reports whether a limit order on side s with limit price limit
// trades at price p: a buyer accepts any p <= limit, a seller any p >= limit.
// It returns false for an invalid side.
//
// It is the single definition of crossing, shared by matching, the FOK
// pre-check and the never-crossed invariant: the book is crossed exactly when
// Buy.Crosses(bestBid, bestAsk). A market order has no limit and crosses
// every price, so callers check for [Market] before calling Crosses.
func (s Side) Crosses(limit, p Price) bool {
	switch s {
	case Buy:
		return p <= limit
	case Sell:
		return p >= limit
	default:
		return false
	}
}

// String returns the wire name, "buy" or "sell", or "Side(n)" for an invalid
// value.
func (s Side) String() string { return enumString(sideNames[:], s, "Side") }

// ParseSide is the inverse of [Side.String] for valid sides. It matches
// exactly (case-sensitive) and returns (0, false) for any other input. The
// API can ignore ok and let [Command.Validate] report [ReasonInvalidSide], so
// a missing and a misspelled side produce the same error code.
func ParseSide(s string) (Side, bool) { return parseEnum[Side](sideNames[:], s) }

// OrderType says how an order's price is interpreted. The zero value is
// invalid.
type OrderType uint8

// The order types. The numbers are stored in the command log: never
// renumber.
const (
	// Limit orders trade at their limit price or better. Any TIF is valid.
	Limit OrderType = 1
	// Market orders trade at any price and never rest. They carry Price 0
	// and TIF IOC or FOK; GTC is rejected with ReasonMarketGTC.
	Market OrderType = 2
)

var orderTypeNames = [...]string{Limit: "limit", Market: "market"}

// Valid reports whether t is Limit or Market.
func (t OrderType) Valid() bool { return t == Limit || t == Market }

// String returns the wire name, "limit" or "market", or "OrderType(n)" for an
// invalid value.
func (t OrderType) String() string { return enumString(orderTypeNames[:], t, "OrderType") }

// ParseOrderType is the inverse of [OrderType.String] for valid types; see
// [ParseSide] for how failures are reported.
func ParseOrderType(s string) (OrderType, bool) {
	return parseEnum[OrderType](orderTypeNames[:], s)
}

// TIF (time in force) says what happens to the part of an order that does not
// trade immediately. The zero value is invalid.
//
// Valid combinations: a Limit order takes GTC, IOC or FOK; a Market order
// takes IOC or FOK.
type TIF uint8

// The time-in-force values. The numbers are stored in the command log: never
// renumber.
const (
	// GTC (good till canceled): the remainder rests in the book until it
	// trades or is canceled.
	GTC TIF = 1
	// IOC (immediate or cancel): the remainder is canceled right after
	// matching (Canceled with ReasonIOCRemainder).
	IOC TIF = 2
	// FOK (fill or kill): the order trades in full immediately, or is
	// Rejected with ReasonFOKUnfillable without trading at all.
	FOK TIF = 3
)

var tifNames = [...]string{GTC: "gtc", IOC: "ioc", FOK: "fok"}

// Valid reports whether t is GTC, IOC or FOK.
func (t TIF) Valid() bool { return t == GTC || t == IOC || t == FOK }

// String returns the wire name, "gtc", "ioc" or "fok", or "TIF(n)" for an
// invalid value.
func (t TIF) String() string { return enumString(tifNames[:], t, "TIF") }

// ParseTIF is the inverse of [TIF.String] for valid values; see [ParseSide]
// for how failures are reported.
func ParseTIF(s string) (TIF, bool) { return parseEnum[TIF](tifNames[:], s) }

// Order is the state of one order in the book. It is a plain value with no
// pointers: the book embeds it in its internal queue node and hands out
// copies, so callers can never alias book internals.
//
// The book holds only resting orders, which today are always Limit GTC. A
// taker exists only while its command is applied, and an order that is
// filled or canceled is forgotten; the engine keeps its history for
// GET /v1/orders/{id}. A snapshot of the book is its resting Orders, level by
// level in queue order, plus the last Event.Seq.
//
// Quantities obey Qty = Filled + Remaining + canceled, where canceled is what
// left the order without trading. It is 0 for every order the book holds
// today, since a cancel removes the whole order; reducing an order's quantity
// (a possible amend) would lower Remaining and leave Qty as submitted.
type Order struct {
	ID        OrderID
	Side      Side
	Type      OrderType
	TIF       TIF
	Price     Price // limit price in ticks; 0 for a market order
	Qty       Qty   // quantity originally submitted (Accepted.Qty)
	Filled    Qty   // cumulative traded quantity
	Remaining Qty   // quantity still open; > 0 while the order rests
	Timestamp int64 // Command.Timestamp of the order's new-order command

	// RestedSeq is the Seq of the EventRested that gave the order its current
	// queue position, or 0 if it has not rested. It strictly increases from
	// head to tail of every level, so CheckInvariants can verify time
	// priority, and a snapshot restore keeps it.
	RestedSeq uint64
}
