package engine_test

import (
	"testing"

	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/engine"
	"github.com/sHyanorn/matching-engine/internal/flow"
)

// windowSink counts a reply by taking a token back.
type windowSink struct{ tokens chan struct{} }

func (s *windowSink) Reply(uint64, engine.Reply) { <-s.tokens }
func (s *windowSink) Flush()                     {}

// BenchmarkSendOrder measures SendOrder and SendCancel on the SPEC §9 flow
// with up to 256 commands sent and not yet answered, as a session keeps
// them. ns/op is one command through the whole path, and the time to
// answer the last one is included.
func BenchmarkSendOrder(b *testing.B) {
	const window = 256
	e := benchEngine(b)
	s := &windowSink{tokens: make(chan struct{}, window)}
	g := flow.New(1, 5)
	ids := make([]book.OrderID, 1, b.N+1) // by the flow's order number
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		op := g.Next()
		s.tokens <- struct{}{}
		var err error
		if op.Cancel {
			err = e.SendCancel(s, 0, ids[op.Order])
		} else {
			var id book.OrderID
			id, err = e.SendOrder(s, 0, flowOrder(op))
			ids = append(ids, id)
		}
		if err != nil {
			b.Fatal(err)
		}
	}
	for range window {
		s.tokens <- struct{}{}
	}
}
