package engine

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sHyanorn/matching-engine/internal/book"
)

// receive returns the next update on s, failing t if none arrives soon.
func receive(t *testing.T, s *Subscription) (Update, bool) {
	t.Helper()
	select {
	case u, ok := <-s.C:
		return u, ok
	case <-time.After(5 * time.Second):
		t.Fatal("no update within 5s")
		return Update{}, false
	}
}

func TestSubscribersReceiveEveryUpdateInOrder(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t, Config{Symbols: []string{"A", "B"}})
	// Events before the subscriptions are not delivered; FromSeq says
	// where the stream starts.
	if _, err := e.PlaceOrder(ctx, limitOrder("A", book.Sell, 100, 5)); err != nil {
		t.Fatal(err)
	}
	s1, err := e.Subscribe(ctx, "A", 16)
	if err != nil {
		t.Fatal(err)
	}
	s2, _ := e.Subscribe(ctx, "A", 16)
	other, _ := e.Subscribe(ctx, "B", 16)
	if s1.FromSeq != 2 || s2.FromSeq != 2 || other.FromSeq != 0 {
		t.Fatalf("FromSeq = %d, %d, %d", s1.FromSeq, s2.FromSeq, other.FromSeq)
	}

	var results []Result
	for _, o := range []NewOrder{limitOrder("A", book.Buy, 100, 2), limitOrder("A", book.Buy, 99, 1), limitOrder("A", book.Buy, 100, 0)} {
		r, err := e.PlaceOrder(ctx, o)
		var inv *InvalidError
		if errors.As(err, &inv) {
			continue // an invalid order reaches no book and publishes nothing
		}
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, r)
	}
	for _, s := range []*Subscription{s1, s2} {
		next := s.FromSeq + 1
		for _, r := range results {
			u, ok := receive(t, s)
			if !ok || u.Symbol != "A" || !slices.Equal(u.Events, r.Events) || u.Events[0].Seq != next {
				t.Fatalf("update %+v, want the events of %+v from Seq %d", u, r, next)
			}
			next += uint64(len(u.Events))
		}
		select {
		case u := <-s.C:
			t.Fatalf("unexpected update %+v", u)
		default:
		}
	}
	select {
	case u := <-other.C:
		t.Fatalf("B's subscriber got A's update %+v", u)
	default:
	}
}

// TestDeliveredEventsAreNotOverwritten keeps an update and checks that the
// book's later commands, which reuse its output buffer, do not change it.
func TestDeliveredEventsAreNotOverwritten(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t, Config{Symbols: []string{"A"}})
	s, _ := e.Subscribe(ctx, "A", 64)
	r, _ := e.PlaceOrder(ctx, limitOrder("A", book.Sell, 100, 5))
	u, _ := receive(t, s)
	keep := slices.Clone(u.Events)
	for range 20 {
		if _, err := e.PlaceOrder(ctx, limitOrder("A", book.Buy, 100, 1)); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(u.Events, keep) || !slices.Equal(r.Events, keep) {
		t.Fatal("a delivered update changed after later commands")
	}
}

// TestSlowSubscriberIsDropped fills a subscriber that never reads: it is
// dropped with ErrSlowConsumer while matching and the other subscriber
// carry on.
func TestSlowSubscriberIsDropped(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t, Config{Symbols: []string{"A"}})
	slow, _ := e.Subscribe(ctx, "A", 2)
	fast, _ := e.Subscribe(ctx, "A", 64)
	const n = 10
	for range n {
		if _, err := e.PlaceOrder(ctx, limitOrder("A", book.Buy, 100, 1)); err != nil {
			t.Fatal(err)
		}
	}
	got := 0
	for {
		_, ok := receive(t, slow)
		if !ok {
			break
		}
		got++
	}
	if got != 2 || !errors.Is(slow.Err(), ErrSlowConsumer) {
		t.Fatalf("slow subscriber got %d updates, then %v", got, slow.Err())
	}
	for range n {
		if _, ok := receive(t, fast); !ok {
			t.Fatalf("fast subscriber ended: %v", fast.Err())
		}
	}
}

func TestSubscriptionClose(t *testing.T) {
	ctx := context.Background()
	e, err := New(Config{Symbols: []string{"A"}, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := e.Subscribe(ctx, "A", 4)
	s.Close()
	if _, ok := receive(t, s); ok || s.Err() != nil {
		t.Fatalf("after Close: channel open or Err %v", s.Err())
	}
	s.Close() // twice is fine
	for range 10 {
		if _, err := e.PlaceOrder(ctx, limitOrder("A", book.Buy, 100, 1)); err != nil {
			t.Fatal(err) // a closed subscription must not fill up or block
		}
	}

	live, _ := e.Subscribe(ctx, "A", 4)
	mustClose(t, e)
	if _, ok := receive(t, live); ok || !errors.Is(live.Err(), ErrClosed) {
		t.Fatalf("after engine Close: channel open or Err %v", live.Err())
	}
	live.Close() // after the engine closed is fine too
	if _, err := e.Subscribe(ctx, "A", 4); !errors.Is(err, ErrClosed) {
		t.Fatalf("Subscribe after Close: %v", err)
	}
	if _, err := e.Subscribe(ctx, "Z", 4); !errors.Is(err, ErrUnknownSymbol) {
		t.Fatalf("Subscribe to an unknown symbol: %v", err)
	}
}
