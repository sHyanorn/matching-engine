package book

import (
	"strconv"
	"testing"
)

// inputEnum is implemented by the enums that arrive as wire names.
type inputEnum interface {
	~uint8
	Valid() bool
	String() string
}

// checkInputEnum checks Valid, String and Parse for all 256 values of an
// enum: exactly the values in names are valid, print their name and parse
// back to themselves; every other value is invalid and prints as
// "typeName(n)", which does not parse. Valid and the name table can
// therefore never disagree.
func checkInputEnum[E inputEnum](t *testing.T, typeName string, names map[E]string, parse func(string) (E, bool)) {
	t.Helper()
	for i := range 256 {
		v := E(i)
		name, valid := names[v]
		if !valid {
			name = typeName + "(" + strconv.Itoa(i) + ")"
		}
		if got := v.Valid(); got != valid {
			t.Errorf("%s(%d).Valid() = %v, want %v", typeName, i, got, valid)
		}
		if got := v.String(); got != name {
			t.Errorf("%s(%d).String() = %q, want %q", typeName, i, got, name)
		}
		got, ok := parse(name)
		switch {
		case valid && (!ok || got != v):
			t.Errorf("parse(%q) = %v, %v; want %v, true", name, got, ok, v)
		case !valid && (ok || got != 0):
			t.Errorf("parse(%q) = %v, %v; want 0, false", name, got, ok)
		}
	}
	// Wire names match exactly: no case folding, trimming or numbers.
	for _, bad := range []string{"", " ", "BUY", "Buy", " buy", "sell ", "Limit", "GTC", "1", "none", "bid"} {
		if got, ok := parse(bad); ok || got != 0 {
			t.Errorf("parse(%q) = %v, %v; want 0, false", bad, got, ok)
		}
	}
}

func TestSideNames(t *testing.T) {
	checkInputEnum(t, "Side", map[Side]string{Buy: "buy", Sell: "sell"}, ParseSide)
}

func TestOrderTypeNames(t *testing.T) {
	checkInputEnum(t, "OrderType", map[OrderType]string{Limit: "limit", Market: "market"}, ParseOrderType)
}

func TestTIFNames(t *testing.T) {
	checkInputEnum(t, "TIF", map[TIF]string{GTC: "gtc", IOC: "ioc", FOK: "fok"}, ParseTIF)
}

func TestSideOpposite(t *testing.T) {
	tests := []struct {
		side, want Side
	}{
		{Buy, Sell},
		{Sell, Buy},
		{0, 0},
		{3, 0},
		{255, 0},
	}
	for _, tt := range tests {
		if got := tt.side.Opposite(); got != tt.want {
			t.Errorf("%v.Opposite() = %v, want %v", tt.side, got, tt.want)
		}
	}
}

func TestSideCrosses(t *testing.T) {
	tests := []struct {
		side     Side
		limit, p Price
		want     bool
	}{
		{Buy, 100, 99, true},
		{Buy, 100, 100, true}, // equal prices trade (SPEC §6.1: 大于等于)
		{Buy, 100, 101, false},
		{Sell, 100, 101, true},
		{Sell, 100, 100, true},
		{Sell, 100, 99, false},
		{Buy, MaxPrice, 1, true},
		{Sell, 1, MaxPrice, true},
		{0, 100, 100, false},
		{3, 100, 100, false},
	}
	for _, tt := range tests {
		if got := tt.side.Crosses(tt.limit, tt.p); got != tt.want {
			t.Errorf("%v.Crosses(limit=%d, p=%d) = %v, want %v", tt.side, tt.limit, tt.p, got, tt.want)
		}
	}
}

// The never-crossed invariant is phrased with Crosses: a book with best bid
// b and best ask a is crossed exactly when Buy.Crosses(b, a), which must
// also be when Sell.Crosses(a, b).
func TestSideCrossesIsSymmetric(t *testing.T) {
	for bid := Price(98); bid <= 102; bid++ {
		for ask := Price(98); ask <= 102; ask++ {
			buy, sell := Buy.Crosses(bid, ask), Sell.Crosses(ask, bid)
			if buy != sell || buy != (bid >= ask) {
				t.Errorf("bid %d, ask %d: Buy.Crosses = %v, Sell.Crosses = %v, want %v", bid, ask, buy, sell, bid >= ask)
			}
		}
	}
}
