package book

import (
	"slices"
	"testing"
)

func TestDepth(t *testing.T) {
	b := New()
	ts := int64(0)
	add := func(id OrderID, s Side, p Price, q Qty) {
		ts++
		apply(t, b, gtc(id, s, p, q, ts))
	}
	add(1, Buy, 99, 5)
	add(2, Buy, 101, 3)
	add(3, Buy, 99, 7)
	add(4, Buy, 100, 1)
	add(5, Sell, 105, 4)
	add(6, Sell, 103, 2)
	add(7, Sell, 103, 6)
	add(8, Sell, 110, 9)

	bids := []Level{{101, 3, 1}, {100, 1, 1}, {99, 12, 2}}
	asks := []Level{{103, 8, 2}, {105, 4, 1}, {110, 9, 1}}
	tests := []struct {
		name string
		side Side
		n    int
		want []Level
	}{
		{"all bids, highest first", Buy, 10, bids},
		{"all asks, lowest first", Sell, 10, asks},
		{"exactly the level count", Buy, 3, bids},
		{"top 2 bids", Buy, 2, bids[:2]},
		{"best ask only", Sell, 1, asks[:1]},
		{"n = 0", Sell, 0, nil},
		{"negative n", Buy, -1, nil},
		{"invalid side", 0, 10, nil},
		{"unknown side", 3, 10, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := b.Depth(tt.side, tt.n, nil); !slices.Equal(got, tt.want) {
				t.Fatalf("Depth(%v, %d) = %v, want %v", tt.side, tt.n, got, tt.want)
			}
		})
	}

	// Depth appends to dst and keeps what was there.
	dst := []Level{{1, 1, 1}}
	if got := b.Depth(Sell, 1, dst); !slices.Equal(got, []Level{{1, 1, 1}, {103, 8, 2}}) {
		t.Fatalf("Depth appended to dst as %v", got)
	}

	// A trade and a cancel are reflected at once: buy 5 @ 103 takes order 6
	// (2) and 3 of order 7; then cancelling 3 leaves bid 99 with one order.
	ts++
	apply(t, b, gtc(9, Buy, 103, 5, ts))
	ts++
	apply(t, b, cancelCmd(3, ts))
	if got, want := b.Depth(Sell, 1, nil), []Level{{103, 3, 1}}; !slices.Equal(got, want) {
		t.Fatalf("asks after trade = %v, want %v", got, want)
	}
	if got, want := b.Depth(Buy, 10, nil), []Level{{101, 3, 1}, {100, 1, 1}, {99, 5, 1}}; !slices.Equal(got, want) {
		t.Fatalf("bids after cancel = %v, want %v", got, want)
	}
}

func TestDepthEmptyBookAndAllocations(t *testing.T) {
	b := New()
	if got := b.Depth(Buy, 20, nil); got != nil {
		t.Fatalf("empty book depth = %v", got)
	}
	for id := OrderID(1); id <= 50; id++ {
		b.Apply(gtc(id, Sell, 100+Price(id), 1, 0), nil)
	}
	dst := make([]Level, 0, 20)
	if a := testing.AllocsPerRun(100, func() { dst = b.Depth(Sell, 20, dst[:0]) }); a != 0 {
		t.Fatalf("Depth into a buffer with room allocates %.1f times", a)
	}
	if len(dst) != 20 || dst[0].Price != 101 || dst[19].Price != 120 {
		t.Fatalf("top 20 asks = %v", dst)
	}
}
