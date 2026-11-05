package flow

import (
	"math"
	"testing"
)

func TestFlowIsReproducible(t *testing.T) {
	a, b := New(7, 5), New(7, 5)
	for i := range 10_000 {
		if x, y := a.Next(), b.Next(); x != y {
			t.Fatalf("op %d: %+v and %+v from the same seed", i, x, y)
		}
	}
	a, b = New(7, 5), New(8, 5)
	same := true
	for range 10 {
		same = same && a.Next() == b.Next()
	}
	if same {
		t.Error("seeds 7 and 8 give the same first 10 ops")
	}
}

func TestFlowShape(t *testing.T) {
	const n, sd = 200_000, 50
	g := New(1, sd)
	var cancels, orders, buys int
	var sum, sumSq float64
	for range n {
		op := g.Next()
		if op.Cancel {
			cancels++
			if op.Order < 1 || op.Order > orders {
				t.Fatalf("cancel of order %d after %d orders", op.Order, orders)
			}
			continue
		}
		orders++
		if op.Order != orders {
			t.Fatalf("order numbered %d, want %d", op.Order, orders)
		}
		if op.Qty < 1 || op.Qty > 100 {
			t.Fatalf("qty %d, want 1 to 100", op.Qty)
		}
		off := float64(op.Price - Mid)
		if op.Buy {
			buys++
			off += 2
		} else {
			off -= 2
		}
		sum += off
		sumSq += off * off
	}
	if r := float64(cancels) / n; math.Abs(r-0.3) > 0.01 {
		t.Errorf("cancel share %.3f, want about 0.30", r)
	}
	if r := float64(buys) / float64(orders); math.Abs(r-0.5) > 0.01 {
		t.Errorf("buy share %.3f, want about 0.50", r)
	}
	mean := sum / float64(orders)
	if s := math.Sqrt(sumSq/float64(orders) - mean*mean); math.Abs(mean) > 1 || math.Abs(s-sd)/sd > 0.02 {
		t.Errorf("price offset mean %.2f sd %.2f, want about 0 and %d", mean, s, sd)
	}
}
