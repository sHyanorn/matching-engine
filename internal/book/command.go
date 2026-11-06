package book

// CommandKind says what a Command asks the book to do. The zero value is
// invalid.
type CommandKind uint8

// The command kinds. The numbers are stored in the command log: append new
// kinds, never renumber.
const (
	// CommandNewOrder submits a new order.
	CommandNewOrder CommandKind = 1
	// CommandCancel removes a resting order and its remaining quantity.
	CommandCancel CommandKind = 2
)

var commandKindNames = [...]string{CommandNewOrder: "new_order", CommandCancel: "cancel"}

// String returns "new_order" or "cancel", or "CommandKind(n)" for an invalid
// value.
func (k CommandKind) String() string {
	return enumString(commandKindNames[:], k, "CommandKind")
}

// Command is the only input to the book. The engine logs each Command it
// applies verbatim, with OrderID and Timestamp already filled in, next to
// what the engine itself needs to route it, such as the symbol. It is a
// fixed-size value made only of integers (no pointers, strings or
// interfaces), so it goes through a channel and into the log without
// allocating, compares with ==, and is easy to build field by field from fuzz
// input.
//
// Fields used by each Kind:
//
//	Kind             OrderID  Side  Type  TIF                 Price                       Qty
//	CommandNewOrder  new ID   set   set   limit: any          limit: [1, MaxPrice]        [1, MaxQty]
//	                                      market: IOC or FOK  market: 0
//	CommandCancel    target   0     0     0                   0                           0
//
// Both kinds use Timestamp. Every field a Kind does not use must be zero, so
// each valid command has exactly one representation, and a later feature
// (amend reusing Price and Qty, say) can give such a field a meaning without
// changing how old logs replay.
//
// Symbol and client_id are deliberately absent: routing (the symbol) belongs
// to the engine, which records it alongside the Command, and client data to
// the engine and API, which keeps Command free of strings.
type Command struct {
	Kind    CommandKind
	Side    Side
	Type    OrderType
	TIF     TIF
	OrderID OrderID
	Price   Price
	Qty     Qty

	// Timestamp is supplied by the caller (the engine uses Unix nanoseconds)
	// and copied verbatim into the resulting events and Order. The book never
	// reads a clock and never compares timestamps: time priority is the
	// order in which commands are applied, so any value, even a negative or
	// repeated one, is valid.
	Timestamp int64
}

// Validate returns the first stateless problem with c, or [ReasonNone] if c
// is well formed. Checks run in a fixed order, so a command with several
// problems always gets the same reason:
//
//	new order:  kind, order ID, side, type, TIF, market with GTC, price, qty
//	cancel:     kind, order ID, unexpected field
//
// Validate knows nothing about book state: a valid new order can still be
// rejected with [ReasonDuplicateOrderID] or [ReasonFOKUnfillable], and a
// valid cancel with [ReasonUnknownOrder]. The API calls Validate after the
// engine has assigned the OrderID (see [OrderID]) and answers 400 without
// logging or applying the command. The book calls it again for every
// command, so the outcome never depends on who called it and arbitrary
// (fuzz) input is safe.
func (c Command) Validate() Reason {
	switch c.Kind {
	case CommandNewOrder:
		return c.validateNewOrder()
	case CommandCancel:
		return c.validateCancel()
	default:
		return ReasonInvalidCommand
	}
}

func (c Command) validateNewOrder() Reason {
	switch {
	case c.OrderID == 0:
		return ReasonInvalidOrderID
	case !c.Side.Valid():
		return ReasonInvalidSide
	case !c.Type.Valid():
		return ReasonInvalidType
	case !c.TIF.Valid():
		return ReasonInvalidTIF
	case c.Type == Market && c.TIF == GTC:
		return ReasonMarketGTC
	case c.Type == Limit && (c.Price < 1 || c.Price > MaxPrice),
		c.Type == Market && c.Price != 0:
		return ReasonInvalidPrice
	case c.Qty < 1 || c.Qty > MaxQty:
		return ReasonInvalidQty
	default:
		return ReasonNone
	}
}

func (c Command) validateCancel() Reason {
	switch {
	case c.OrderID == 0:
		return ReasonInvalidOrderID
	case c.Side != 0 || c.Type != 0 || c.TIF != 0 || c.Price != 0 || c.Qty != 0:
		return ReasonUnexpectedField
	default:
		return ReasonNone
	}
}
