package book

import (
	"math"
	"testing"
)

func TestCommandKindString(t *testing.T) {
	tests := []struct {
		kind CommandKind
		want string
	}{
		{CommandNewOrder, "new_order"},
		{CommandCancel, "cancel"},
		{0, "CommandKind(0)"},
		{3, "CommandKind(3)"},
		{255, "CommandKind(255)"},
	}
	for _, tt := range tests {
		if got := tt.kind.String(); got != tt.want {
			t.Errorf("CommandKind(%d).String() = %q, want %q", uint8(tt.kind), got, tt.want)
		}
	}
}

func TestCommandValidate(t *testing.T) {
	// Well-formed commands; each failing case changes one thing.
	limit := Command{Kind: CommandNewOrder, OrderID: 1, Side: Buy, Type: Limit, TIF: GTC, Price: 100, Qty: 10, Timestamp: 42}
	market := Command{Kind: CommandNewOrder, OrderID: 2, Side: Sell, Type: Market, TIF: IOC, Qty: 5}
	cancel := Command{Kind: CommandCancel, OrderID: 1, Timestamp: 43}
	with := func(c Command, edit func(*Command)) Command {
		edit(&c)
		return c
	}

	tests := []struct {
		name string
		cmd  Command
		want Reason
	}{
		{"limit gtc", limit, ReasonNone},
		{"limit ioc", with(limit, func(c *Command) { c.TIF = IOC }), ReasonNone},
		{"limit fok", with(limit, func(c *Command) { c.TIF = FOK }), ReasonNone},
		{"limit sell", with(limit, func(c *Command) { c.Side = Sell }), ReasonNone},
		{"price and qty at min", with(limit, func(c *Command) { c.Price, c.Qty = 1, 1 }), ReasonNone},
		{"price and qty at max", with(limit, func(c *Command) { c.Price, c.Qty = MaxPrice, MaxQty }), ReasonNone},
		{"largest order id", with(limit, func(c *Command) { c.OrderID = math.MaxUint64 }), ReasonNone},
		{"timestamp is opaque", with(limit, func(c *Command) { c.Timestamp = math.MinInt64 }), ReasonNone},
		{"market ioc", market, ReasonNone},
		{"market fok", with(market, func(c *Command) { c.TIF = FOK }), ReasonNone},
		{"market buy at max qty", with(market, func(c *Command) { c.Side, c.Qty = Buy, MaxQty }), ReasonNone},
		{"cancel", cancel, ReasonNone},
		{"cancel with zero timestamp", with(cancel, func(c *Command) { c.Timestamp = 0 }), ReasonNone},

		{"zero command", Command{}, ReasonInvalidCommand},
		{"unknown kind", with(limit, func(c *Command) { c.Kind = 3 }), ReasonInvalidCommand},
		{"largest kind", with(limit, func(c *Command) { c.Kind = 255 }), ReasonInvalidCommand},

		{"order id zero", with(limit, func(c *Command) { c.OrderID = 0 }), ReasonInvalidOrderID},
		{"side zero", with(limit, func(c *Command) { c.Side = 0 }), ReasonInvalidSide},
		{"side unknown", with(limit, func(c *Command) { c.Side = 3 }), ReasonInvalidSide},
		{"type zero", with(limit, func(c *Command) { c.Type = 0 }), ReasonInvalidType},
		{"type unknown", with(limit, func(c *Command) { c.Type = 3 }), ReasonInvalidType},
		{"tif zero", with(limit, func(c *Command) { c.TIF = 0 }), ReasonInvalidTIF},
		{"tif unknown", with(limit, func(c *Command) { c.TIF = 4 }), ReasonInvalidTIF},
		{"market gtc", with(market, func(c *Command) { c.TIF = GTC }), ReasonMarketGTC},
		{"limit price zero", with(limit, func(c *Command) { c.Price = 0 }), ReasonInvalidPrice},
		{"limit price negative", with(limit, func(c *Command) { c.Price = -100 }), ReasonInvalidPrice},
		{"limit price above max", with(limit, func(c *Command) { c.Price = MaxPrice + 1 }), ReasonInvalidPrice},
		{"limit price min int64", with(limit, func(c *Command) { c.Price = math.MinInt64 }), ReasonInvalidPrice},
		{"limit price max int64", with(limit, func(c *Command) { c.Price = math.MaxInt64 }), ReasonInvalidPrice},
		{"market with price", with(market, func(c *Command) { c.Price = 100 }), ReasonInvalidPrice},
		{"market with negative price", with(market, func(c *Command) { c.Price = -1 }), ReasonInvalidPrice},
		{"qty zero", with(limit, func(c *Command) { c.Qty = 0 }), ReasonInvalidQty},
		{"qty negative", with(limit, func(c *Command) { c.Qty = -10 }), ReasonInvalidQty},
		{"qty above max", with(limit, func(c *Command) { c.Qty = MaxQty + 1 }), ReasonInvalidQty},
		{"qty max int64", with(limit, func(c *Command) { c.Qty = math.MaxInt64 }), ReasonInvalidQty},
		{"market qty zero", with(market, func(c *Command) { c.Qty = 0 }), ReasonInvalidQty},

		{"cancel id zero", with(cancel, func(c *Command) { c.OrderID = 0 }), ReasonInvalidOrderID},
		{"cancel with side", with(cancel, func(c *Command) { c.Side = Buy }), ReasonUnexpectedField},
		{"cancel with type", with(cancel, func(c *Command) { c.Type = Limit }), ReasonUnexpectedField},
		{"cancel with tif", with(cancel, func(c *Command) { c.TIF = GTC }), ReasonUnexpectedField},
		{"cancel with price", with(cancel, func(c *Command) { c.Price = 1 }), ReasonUnexpectedField},
		{"cancel with qty", with(cancel, func(c *Command) { c.Qty = -3 }), ReasonUnexpectedField},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cmd.Validate(); got != tt.want {
				t.Errorf("Validate(%+v) = %v, want %v", tt.cmd, got, tt.want)
			}
		})
	}
}

// When several fields are wrong, the first check in the documented order
// wins, so the reason (and with it the replayed event stream) is
// deterministic.
func TestCommandValidatePrecedence(t *testing.T) {
	tests := []struct {
		name string
		cmd  Command
		want Reason
	}{
		{"kind before everything", Command{Kind: 9, Side: 9, Type: 9, TIF: 9, Price: -1, Qty: -1}, ReasonInvalidCommand},
		{"order id before side", Command{Kind: CommandNewOrder, Side: 9, Type: 9, TIF: 9, Price: -1, Qty: -1}, ReasonInvalidOrderID},
		{"side before type", Command{Kind: CommandNewOrder, OrderID: 1, Type: 9, TIF: 9, Price: -1, Qty: -1}, ReasonInvalidSide},
		{"type before tif", Command{Kind: CommandNewOrder, OrderID: 1, Side: Buy, TIF: 9, Price: -1, Qty: -1}, ReasonInvalidType},
		{"tif before price", Command{Kind: CommandNewOrder, OrderID: 1, Side: Buy, Type: Limit, Price: -1, Qty: -1}, ReasonInvalidTIF},
		{"market gtc before price", Command{Kind: CommandNewOrder, OrderID: 1, Side: Buy, Type: Market, TIF: GTC, Price: 5, Qty: -1}, ReasonMarketGTC},
		{"price before qty", Command{Kind: CommandNewOrder, OrderID: 1, Side: Buy, Type: Limit, TIF: GTC, Qty: -1}, ReasonInvalidPrice},
		{"cancel id before stray field", Command{Kind: CommandCancel, Qty: 1}, ReasonInvalidOrderID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cmd.Validate(); got != tt.want {
				t.Errorf("Validate(%+v) = %v, want %v", tt.cmd, got, tt.want)
			}
		})
	}
}

// statelessReasons is everything Command.Validate may return.
var statelessReasons = map[Reason]bool{
	ReasonNone:            true,
	ReasonInvalidCommand:  true,
	ReasonInvalidOrderID:  true,
	ReasonInvalidSide:     true,
	ReasonInvalidType:     true,
	ReasonInvalidTIF:      true,
	ReasonMarketGTC:       true,
	ReasonInvalidPrice:    true,
	ReasonInvalidQty:      true,
	ReasonUnexpectedField: true,
}

// FuzzCommandValidate builds a Command straight from fuzz arguments, so every
// field combination is reachable, and checks that Validate is total and
// deterministic, returns only stateless reasons, and accepts only commands
// that meet the guarantees the book relies on (restated below independently
// of Validate's code).
func FuzzCommandValidate(f *testing.F) {
	for _, c := range []Command{
		{Kind: CommandNewOrder, OrderID: 1, Side: Buy, Type: Limit, TIF: GTC, Price: 100, Qty: 10, Timestamp: 1},
		{Kind: CommandNewOrder, OrderID: 2, Side: Sell, Type: Market, TIF: FOK, Qty: 5},
		{Kind: CommandNewOrder, OrderID: 3, Side: Buy, Type: Limit, TIF: IOC, Price: MaxPrice + 1, Qty: math.MaxInt64},
		{Kind: CommandCancel, OrderID: 1},
		{Kind: CommandCancel, OrderID: 1, Qty: 1},
		{},
	} {
		f.Add(uint8(c.Kind), uint8(c.Side), uint8(c.Type), uint8(c.TIF), uint64(c.OrderID), int64(c.Price), int64(c.Qty), c.Timestamp)
	}
	f.Fuzz(func(t *testing.T, kind, side, typ, tif uint8, id uint64, price, qty, ts int64) {
		c := Command{
			Kind: CommandKind(kind), Side: Side(side), Type: OrderType(typ), TIF: TIF(tif),
			OrderID: OrderID(id), Price: Price(price), Qty: Qty(qty), Timestamp: ts,
		}
		r := c.Validate()
		if again := c.Validate(); again != r {
			t.Fatalf("Validate(%+v) = %v, then %v", c, r, again)
		}
		if !statelessReasons[r] {
			t.Fatalf("Validate(%+v) = %v, which is not a stateless reason", c, r)
		}
		if r == ReasonNone {
			checkAccepted(t, c)
		}
	})
}

// checkAccepted fails t unless c meets every guarantee a valid command gives
// the book.
func checkAccepted(t *testing.T, c Command) {
	t.Helper()
	if c.OrderID == 0 {
		t.Fatalf("accepted %+v with OrderID 0", c)
	}
	switch c.Kind {
	case CommandNewOrder:
		if !c.Side.Valid() || !c.Type.Valid() || !c.TIF.Valid() {
			t.Fatalf("accepted %+v with an invalid enum", c)
		}
		if c.Qty < 1 || c.Qty > MaxQty {
			t.Fatalf("accepted %+v with qty out of range", c)
		}
		switch c.Type {
		case Limit:
			if c.Price < 1 || c.Price > MaxPrice {
				t.Fatalf("accepted limit %+v with price out of range", c)
			}
		case Market:
			if c.Price != 0 || c.TIF == GTC {
				t.Fatalf("accepted market %+v that has a price or could rest", c)
			}
		}
	case CommandCancel:
		if c != (Command{Kind: CommandCancel, OrderID: c.OrderID, Timestamp: c.Timestamp}) {
			t.Fatalf("accepted cancel %+v with stray fields", c)
		}
	default:
		t.Fatalf("accepted %+v with an unknown kind", c)
	}
}
