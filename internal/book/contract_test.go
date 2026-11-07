package book

import (
	"math"
	"reflect"
	"testing"
	"unsafe"
)

// TestWireValuesArePinned guards the command log format and the event
// contract: these numbers are stored on disk and compared across replays.
// Changing one must be a deliberate, versioned format change, never a side
// effect of reordering constants.
func TestWireValuesArePinned(t *testing.T) {
	pins := []struct {
		name      string
		got, want uint8
	}{
		{"CommandNewOrder", uint8(CommandNewOrder), 1},
		{"CommandCancel", uint8(CommandCancel), 2},
		{"Buy", uint8(Buy), 1},
		{"Sell", uint8(Sell), 2},
		{"Limit", uint8(Limit), 1},
		{"Market", uint8(Market), 2},
		{"GTC", uint8(GTC), 1},
		{"IOC", uint8(IOC), 2},
		{"FOK", uint8(FOK), 3},
		{"EventAccepted", uint8(EventAccepted), 1},
		{"EventRejected", uint8(EventRejected), 2},
		{"EventTrade", uint8(EventTrade), 3},
		{"EventRested", uint8(EventRested), 4},
		{"EventCanceled", uint8(EventCanceled), 5},
		{"ReasonNone", uint8(ReasonNone), 0},
		{"ReasonInvalidCommand", uint8(ReasonInvalidCommand), 1},
		{"ReasonInvalidOrderID", uint8(ReasonInvalidOrderID), 2},
		{"ReasonInvalidSide", uint8(ReasonInvalidSide), 3},
		{"ReasonInvalidType", uint8(ReasonInvalidType), 4},
		{"ReasonInvalidTIF", uint8(ReasonInvalidTIF), 5},
		{"ReasonMarketGTC", uint8(ReasonMarketGTC), 6},
		{"ReasonInvalidPrice", uint8(ReasonInvalidPrice), 7},
		{"ReasonInvalidQty", uint8(ReasonInvalidQty), 8},
		{"ReasonUnexpectedField", uint8(ReasonUnexpectedField), 9},
		{"ReasonDuplicateOrderID", uint8(ReasonDuplicateOrderID), 10},
		{"ReasonUnknownOrder", uint8(ReasonUnknownOrder), 11},
		{"ReasonFOKUnfillable", uint8(ReasonFOKUnfillable), 12},
		{"ReasonUserCancel", uint8(ReasonUserCancel), 13},
		{"ReasonIOCRemainder", uint8(ReasonIOCRemainder), 14},
	}
	for _, p := range pins {
		if p.got != p.want {
			t.Errorf("%s = %d, want %d (pinned wire value)", p.name, p.got, p.want)
		}
	}
}

// TestLayout pins the sizes the hot path relies on: Command travels by value
// through the engine channel, and Event is appended to reused buffers. If a
// new field changes a size, update the number on purpose, rerun the
// benchmarks and record it in DECISIONS.md.
func TestLayout(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("sizes are pinned for 64-bit targets, where int64 is 8-byte aligned")
	}
	tests := []struct {
		name      string
		got, want uintptr
	}{
		{"Command", unsafe.Sizeof(Command{}), 40},
		{"Event", unsafe.Sizeof(Event{}), 64},
		{"Order", unsafe.Sizeof(Order{}), 64},
		{"Trade", unsafe.Sizeof(Trade{}), 56},
		{"LevelUpdate", unsafe.Sizeof(LevelUpdate{}), 32},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("unsafe.Sizeof(%s{}) = %d, want %d", tt.name, tt.got, tt.want)
		}
	}
}

// TestPlainData guards what the rest of the system relies on: these types
// hold only fixed-size integers (no pointers, strings, slices, maps or
// interfaces), so copying them never allocates, the GC never scans a buffer
// of them, a fuzzer can build them from raw values, and == compares them
// completely (the replay test compares event slices).
func TestPlainData(t *testing.T) {
	for _, v := range []any{Command{}, Event{}, Order{}, Trade{}, LevelUpdate{}} {
		typ := reflect.TypeOf(v)
		assertPlainData(t, typ, typ.Name())
	}
	requireComparable[Command]()
	requireComparable[Event]()
	requireComparable[Order]()
	requireComparable[Trade]()
	requireComparable[LevelUpdate]()
}

// requireComparable fails to compile if T stops being comparable.
func requireComparable[T comparable]() {}

func assertPlainData(t *testing.T, typ reflect.Type, path string) {
	t.Helper()
	switch typ.Kind() {
	case reflect.Struct:
		for i := range typ.NumField() {
			f := typ.Field(i)
			assertPlainData(t, f.Type, path+"."+f.Name)
		}
	case reflect.Uint8, reflect.Uint32, reflect.Uint64, reflect.Int64:
	default:
		t.Errorf("%s has kind %s; only fixed-size integers are allowed", path, typ.Kind())
	}
}

// TestBoundsCannotOverflow restates the argument in the MaxPrice and MaxQty
// documentation, so raising a bound past it fails here, and so does lowering
// a bound, which would break replay of old logs.
func TestBoundsCannotOverflow(t *testing.T) {
	maxPrice, maxQty := int64(MaxPrice), int64(MaxQty)
	const (
		jsonExact     = 1 << 53   // integers up to 2^53 are exact in a float64 (JavaScript Number)
		restingOrders = 9_000_000 // nine times the 1M resting-order memory target
	)
	if maxPrice > math.MaxInt64/maxQty {
		t.Errorf("MaxPrice*MaxQty overflows int64: a fill's notional must fit")
	}
	if maxQty > jsonExact/restingOrders {
		t.Errorf("%d resting orders of MaxQty total more than 2^53", restingOrders)
	}
	if maxPrice >= jsonExact {
		t.Errorf("MaxPrice is not exact as a JSON number")
	}
	// Lowering a bound makes commands in old logs invalid, so replay would
	// reject them (D012). Raising is safe up to the limits checked above.
	if maxPrice < 1_000_000_000 {
		t.Errorf("MaxPrice = %d was lowered below 1e9: old command logs would no longer replay (D012)", maxPrice)
	}
	if maxQty < 1_000_000_000 {
		t.Errorf("MaxQty = %d was lowered below 1e9: old command logs would no longer replay (D012)", maxQty)
	}
}
