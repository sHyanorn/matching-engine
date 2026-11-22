package engine_test

import (
	"context"
	"testing"

	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/engine"
	"github.com/sHyanorn/matching-engine/internal/flow"
)

const benchSymbol = "BTC-USD"

// benchEngine returns an engine with one symbol on a fresh data directory.
func benchEngine(b *testing.B) *engine.Engine {
	b.Helper()
	e, err := engine.New(engine.Config{Symbols: []string{benchSymbol}, DataDir: b.TempDir()})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := e.Close(); err != nil {
			b.Error(err)
		}
	})
	return e
}

func flowOrder(op flow.Op) engine.NewOrder {
	o := engine.NewOrder{Symbol: benchSymbol, Side: book.Sell, Type: book.Limit, TIF: book.GTC, Price: book.Price(op.Price), Qty: book.Qty(op.Qty)}
	if op.Buy {
		o.Side = book.Buy
	}
	return o
}

// BenchmarkPlaceOrder measures PlaceOrder and CancelOrder on the SPEC §9
// flow. Each command is answered before the next is sent, so ns/op is one
// round trip through the symbol's goroutine.
func BenchmarkPlaceOrder(b *testing.B) {
	e := benchEngine(b)
	g := flow.New(1, 5)
	ids := make([]book.OrderID, 1, b.N+1) // by the flow's order number
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		op := g.Next()
		if op.Cancel {
			if _, err := e.CancelOrder(ctx, ids[op.Order]); err != nil {
				b.Fatal(err)
			}
			continue
		}
		r, err := e.PlaceOrder(ctx, flowOrder(op))
		if err != nil {
			b.Fatal(err)
		}
		ids = append(ids, r.OrderID)
	}
}
