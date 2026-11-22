package api_test

import (
	"strings"
	"testing"

	"github.com/sHyanorn/matching-engine/internal/api"
	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/engine"
)

// commandCounts are the samples of the orders, cancels and trades counters.
func commandCounts(m metricSet) map[string]float64 {
	counts := map[string]float64{}
	for k, v := range m.values {
		for _, name := range []string{"matching_engine_orders_total", "matching_engine_cancels_total", "matching_engine_trades_total"} {
			if strings.HasPrefix(k, name+"{") {
				counts[k] = v
			}
		}
	}
	return counts
}

// TestCountOrderAndCountCancel: a command is counted by its events and its
// error, whatever protocol it came over, and one that names no configured
// symbol or an ID the engine has not issued is not counted.
func TestCountOrderAndCountCancel(t *testing.T) {
	env := newEnv(t, envConfig{})
	id := book.OrderID(env.place(t, limitOrder("sell", 100, 5)).Order.OrderID)
	notIssued := id + 1<<40
	before := commandCounts(env.metrics(t))

	trade := book.Event{Kind: book.EventTrade}
	accepted := []book.Event{{Kind: book.EventAccepted}, trade, trade}
	rejected := []book.Event{{Kind: book.EventRejected}}
	canceled := []book.Event{{Kind: book.EventCanceled}}
	none := engine.OrderStatus{}
	filled := engine.OrderStatus{Order: book.Order{ID: id}, State: engine.StateFilled}
	s := env.srv
	s.CountOrder(btc, accepted, nil)
	s.CountOrder(btc, accepted, engine.ErrLogFailed) // it took effect
	s.CountOrder(eth, rejected, nil)
	s.CountOrder(btc, nil, &engine.InvalidError{Reason: book.ReasonInvalidQty})
	s.CountOrder(btc, nil, engine.ErrBusy)
	s.CountOrder(btc, nil, engine.ErrClosed)
	s.CountOrder(btc, nil, engine.ErrLogFailed)
	s.CountOrder("NOPE", accepted, nil)
	s.CountOrder("NOPE", nil, engine.ErrClosed)
	s.CountCancel(id, canceled, none, nil)
	s.CountCancel(id, rejected, none, nil)
	s.CountCancel(id, rejected, filled, nil)
	s.CountCancel(id, nil, none, engine.ErrBusy)
	s.CountCancel(id, nil, none, engine.ErrClosed)
	s.CountCancel(notIssued, nil, none, engine.ErrUnknownOrder)
	s.CountCancel(notIssued, nil, none, engine.ErrClosed)
	s.CountCancel(0, nil, none, &engine.InvalidError{Reason: book.ReasonInvalidOrderID})

	want := map[string]float64{
		sampleKey("matching_engine_orders_total", "symbol", btc, "result", "accepted"):  2,
		sampleKey("matching_engine_orders_total", "symbol", eth, "result", "rejected"):  1,
		sampleKey("matching_engine_orders_total", "symbol", btc, "result", "invalid"):   1,
		sampleKey("matching_engine_orders_total", "symbol", btc, "result", "busy"):      1,
		sampleKey("matching_engine_orders_total", "symbol", btc, "result", "failed"):    2,
		sampleKey("matching_engine_trades_total", "symbol", btc):                        4,
		sampleKey("matching_engine_cancels_total", "symbol", btc, "result", "canceled"): 1,
		sampleKey("matching_engine_cancels_total", "symbol", btc, "result", "unknown"):  1,
		sampleKey("matching_engine_cancels_total", "symbol", btc, "result", "finished"): 1,
		sampleKey("matching_engine_cancels_total", "symbol", btc, "result", "busy"):     1,
		sampleKey("matching_engine_cancels_total", "symbol", btc, "result", "failed"):   1,
	}
	after := commandCounts(env.metrics(t))
	if len(after) != len(before) {
		t.Errorf("%d series of the three counters, %d before: one was made for a symbol that is not configured", len(after), len(before))
	}
	for k, v := range after {
		if got := v - before[k]; got != want[k] {
			t.Errorf("%s rose by %v, want %v", k, got, want[k])
		}
	}
}

// BenchmarkCountOrder measures counting one order that traded twice, which
// an OE1 session does on the goroutine of the order's symbol.
func BenchmarkCountOrder(b *testing.B) {
	eng, err := engine.New(engine.Config{Symbols: []string{btc}, DataDir: b.TempDir()})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = eng.Close() })
	s := api.New(eng, api.Options{})
	events := []book.Event{{Kind: book.EventAccepted}, {Kind: book.EventTrade}, {Kind: book.EventTrade}}
	b.ReportAllocs()
	for b.Loop() {
		s.CountOrder(btc, events, nil)
	}
}
