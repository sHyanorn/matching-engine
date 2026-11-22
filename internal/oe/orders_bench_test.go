package oe

import (
	"testing"

	"github.com/sHyanorn/matching-engine/internal/api"
	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/engine"
	"github.com/sHyanorn/matching-engine/internal/flow"
)

// BenchmarkOrders measures NewOrder and Cancel over one session on loopback
// against a real engine, on the SPEC §9 flow: 256 commands are sent in a
// write and all their replies read before the next write. A cancel names an
// order whose ID an earlier reply gave. The commands are counted in the
// counters of an api.Server, as cmd/engine wires them. ns/op is one
// command, and the allocations are those of the server and the engine.
func BenchmarkOrders(b *testing.B) {
	const batch = 256
	e, err := engine.New(engine.Config{Symbols: []string{"BTC-USD"}, DataDir: b.TempDir()})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = e.Close() }) // after the server, which benchLogin closes
	bc := benchLogin(b, ServerOptions{Engine: e, Counts: api.New(e, api.Options{}), Symbols: e.Symbols()})
	g := flow.New(1, 5)
	var known []book.OrderID // the IDs of orders answered, by the flow's order number minus 1; 0 if not an order
	var ids []book.OrderID   // IDs of the batch being answered
	isDone := func(t Type, body []byte) bool {
		if t != TypeDone {
			return false
		}
		d := DecodeDone(body)
		if d.OrderID != 0 && d.ServerSeq != 0 {
			ids = append(ids, d.OrderID)
		}
		return true
	}
	var seq uint64
	b.ReportAllocs()
	b.ResetTimer()
	for done := 0; done < b.N; done += batch {
		n := min(batch, b.N-done)
		sent := 0
		ids = ids[:0]
		for range n {
			op := g.Next()
			seq++
			switch {
			case op.Cancel && op.Order <= len(known):
				bc.out = Cancel{ClientSeq: seq, OrderID: known[op.Order-1]}.Append(bc.out)
			default:
				side := book.Sell
				if op.Buy {
					side = book.Buy
				}
				bc.out = NewOrder{ClientSeq: seq, Side: side, Type: book.Limit, TIF: book.GTC, Price: book.Price(op.Price), Qty: book.Qty(op.Qty)}.Append(bc.out)
				sent++
			}
		}
		bc.flush()
		bc.read(n, isDone)
		known = append(known, ids...)
	}
}
