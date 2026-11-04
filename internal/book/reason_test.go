package book

import (
	"regexp"
	"strconv"
	"testing"
)

// The reason codes are API error strings and appear in logs, so they are
// pinned here one by one.
func TestReasonString(t *testing.T) {
	want := map[Reason]string{
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
	snakeCase := regexp.MustCompile(`^[a-z]+(_[a-z]+)*$`)
	seen := map[string]Reason{}
	for i := range 256 {
		r := Reason(i)
		name, ok := want[r]
		if !ok {
			name = "Reason(" + strconv.Itoa(i) + ")"
		}
		got := r.String()
		if got != name {
			t.Errorf("Reason(%d).String() = %q, want %q", i, got, name)
		}
		if !ok {
			continue
		}
		if !snakeCase.MatchString(got) {
			t.Errorf("Reason(%d).String() = %q, want snake_case", i, got)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("Reason(%d) and Reason(%d) both print %q", uint8(prev), i, got)
		}
		seen[got] = r
	}
}
