package book

// Reason explains an EventRejected or an EventCanceled, and is what
// [Command.Validate] returns. It is [ReasonNone] on every other event.
//
// String gives a stable snake_case code, which the API returns as its error
// string. The numbers are part of the event contract: append new reasons,
// never renumber.
type Reason uint8

// The reasons, grouped by where they come from and which event reports them.
const (
	// ReasonNone means no problem: a valid command, or an event that takes
	// no reason.
	ReasonNone Reason = 0

	// Stateless rejections: returned by Command.Validate, in this order of
	// precedence, and reported by EventRejected. An API answers them with
	// 400 without logging or applying the command. For a new order the API
	// itself sets Kind and OrderID, so invalid_command or invalid_order_id
	// there means an API bug (500); for a cancel, invalid_order_id is the
	// client's error (400).
	ReasonInvalidCommand  Reason = 1 // Kind is not a known CommandKind
	ReasonInvalidOrderID  Reason = 2 // OrderID is 0
	ReasonInvalidSide     Reason = 3 // Side is not Buy or Sell
	ReasonInvalidType     Reason = 4 // Type is not Limit or Market
	ReasonInvalidTIF      Reason = 5 // TIF is not GTC, IOC or FOK
	ReasonMarketGTC       Reason = 6 // a market order cannot rest, so it needs IOC or FOK
	ReasonInvalidPrice    Reason = 7 // limit price outside [1, MaxPrice], or market price not 0
	ReasonInvalidQty      Reason = 8 // quantity outside [1, MaxQty]
	ReasonUnexpectedField Reason = 9 // a cancel sets a field that only new orders use

	// Stateful rejections: found by the book against its current state and
	// reported by EventRejected. The command was valid and changed nothing.
	ReasonDuplicateOrderID Reason = 10 // a new order reuses the ID of a resting order
	ReasonUnknownOrder     Reason = 11 // a cancel names an ID that is not resting (never existed, filled or canceled)
	ReasonFOKUnfillable    Reason = 12 // a FOK order cannot fill in full right now, so nothing traded

	// Cancellations: reported by EventCanceled, whose Qty left the order
	// without trading.
	ReasonUserCancel   Reason = 13 // a CommandCancel removed a resting order
	ReasonIOCRemainder Reason = 14 // the untraded part of an IOC or market order; it never rested
)

var reasonNames = [...]string{
	ReasonNone:             "none",
	ReasonInvalidCommand:   "invalid_command",
	ReasonInvalidOrderID:   "invalid_order_id",
	ReasonInvalidSide:      "invalid_side",
	ReasonInvalidType:      "invalid_type",
	ReasonInvalidTIF:       "invalid_tif",
	ReasonMarketGTC:        "market_gtc",
	ReasonInvalidPrice:     "invalid_price",
	ReasonInvalidQty:       "invalid_qty",
	ReasonUnexpectedField:  "unexpected_field",
	ReasonDuplicateOrderID: "duplicate_order_id",
	ReasonUnknownOrder:     "unknown_order",
	ReasonFOKUnfillable:    "fok_unfillable",
	ReasonUserCancel:       "user_cancel",
	ReasonIOCRemainder:     "ioc_remainder",
}

// String returns the snake_case code, such as "invalid_price", or "Reason(n)"
// for an unknown value.
func (r Reason) String() string { return enumString(reasonNames[:], r, "Reason") }
