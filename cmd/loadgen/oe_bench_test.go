package main

import (
	"testing"

	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/engine"
	"github.com/sHyanorn/matching-engine/internal/oe"
)

// BenchmarkTakeTrade measures the reader's take of the reply to an order
// that traded: a trade Event, which it hashes into the run's trade sum, and
// the order's Done. ns/op is one reply.
func BenchmarkTakeTrade(b *testing.B) { benchTake(b, book.EventTrade, orderTraded) }

// BenchmarkTakeNoTrade is BenchmarkTakeTrade for an order that rests: an
// accepted Event, which is counted and not hashed, and the Done.
func BenchmarkTakeNoTrade(b *testing.B) { benchTake(b, book.EventAccepted, orderRested) }

// benchTake takes, over and over, the frames of a reply to the first order of
// the flow, an Event of kind and its Done, and checks that the Done was
// counted as want.
func benchTake(b *testing.B, kind book.EventKind, want outcome) {
	r := offlineRun(true)
	if _, err := r.send(0, r.due(0)); err != nil { // the first op of the flow is an order
		b.Fatal(err)
	}
	frames := oe.Event{ServerSeq: 1, ClientSeq: 1, Event: book.Event{Seq: 1, Kind: kind, OrderID: fakeOrderID(1),
		MakerID: fakeOrderID(2), Price: 6_400_000, Qty: 1}}.Append(nil)
	frames = oe.Done{ServerSeq: 2, ClientSeq: 1, Status: oe.StatusOK, OrderID: fakeOrderID(1), State: engine.StateOpen}.Append(frames)
	b.ReportAllocs()
	for b.Loop() {
		// The same frames each time: server_seq starts again and the command is unanswered.
		r.in, r.serverSeq, r.res[0] = frames, 0, noDone
		if err := r.take(0); err != nil {
			b.Fatal(err)
		}
	}
	if r.res[0] != want {
		b.Fatalf("the Done was counted as %v, want %v", outcomeNames[r.res[0]], outcomeNames[want])
	}
}
