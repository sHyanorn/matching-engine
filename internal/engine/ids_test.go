package engine

import (
	"testing"

	"github.com/sHyanorn/matching-engine/internal/book"
)

func TestOrderIDLayout(t *testing.T) {
	for _, tc := range []struct {
		counter uint64
		sym     int
	}{{1, 0}, {1, 255}, {2, 7}, {1<<45 - 1, 3}} {
		id := makeOrderID(tc.counter, tc.sym)
		if id == 0 || counterOf(id) != tc.counter || symbolIndexOf(id) != tc.sym {
			t.Errorf("makeOrderID(%d, %d) = %d: counter %d, symbol %d", tc.counter, tc.sym, id, counterOf(id), symbolIndexOf(id))
		}
	}
	// The first 2^45 - 1 orders stay exact as a JSON number; the next
	// one would not.
	if id := makeOrderID(1<<45-1, MaxSymbols-1); uint64(id) >= 1<<53 {
		t.Errorf("ID %d is not exact in a float64", id)
	}
	if id := makeOrderID(1<<45, 0); uint64(id) < 1<<53 {
		t.Errorf("ID %d: the bound in ids.go is not tight", id)
	}
	if symbolIndexOf(book.OrderID(0x1_00)) != 0 || counterOf(book.OrderID(0x1_00)) != 1 {
		t.Error("ID 256 is counter 1 of symbol 0")
	}
}
