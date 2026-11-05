package engine

import (
	"context"
	"errors"

	"github.com/sHyanorn/matching-engine/internal/book"
)

// ErrSlowConsumer ends a subscription whose channel was full when the book
// had events for it. Matching never waits for a subscriber (D018): the
// subscriber is dropped and, with the Seq it last saw, can resubscribe and
// fetch a fresh snapshot.
var ErrSlowConsumer = errors.New("engine: subscriber too slow")

const defaultSubscriberBuffer = 1024

// Update is the events one command produced on a symbol's book. Events is
// shared with the command's Result and every other subscriber and must not
// be modified.
type Update struct {
	Symbol string
	Events []book.Event
}

// Subscription delivers every Update of one symbol, in Seq order, starting
// with the first event after FromSeq.
type Subscription struct {
	// C receives the updates. The engine closes it when the subscription
	// ends; Err then says why.
	C <-chan Update
	// FromSeq is the book's LastSeq when the subscription started: a
	// snapshot taken at FromSeq plus the updates on C is the whole book.
	FromSeq uint64

	ch     chan Update
	err    error // set before ch is closed
	engine *Engine
	market *market
}

// Err returns why the subscription ended: nil after Close, ErrSlowConsumer
// or ErrClosed. It is meaningful only once C has been closed.
func (s *Subscription) Err() error { return s.err }

// Close ends the subscription; C is closed soon after. It is safe to call
// more than once and after the engine has closed.
func (s *Subscription) Close() {
	_, _ = s.engine.send(context.Background(), s.market, &request{kind: reqUnsubscribe, sub: s}, orWait)
}

// Subscribe starts a subscription to symbol's events. buffer is the
// capacity of its channel (default 1024 if not positive); a subscriber
// that falls that far behind is dropped with ErrSlowConsumer.
func (e *Engine) Subscribe(ctx context.Context, symbol string, buffer int) (*Subscription, error) {
	m, ok := e.bySymbol[symbol]
	if !ok {
		return nil, ErrUnknownSymbol
	}
	if buffer <= 0 {
		buffer = defaultSubscriberBuffer
	}
	ch := make(chan Update, buffer)
	s := &Subscription{C: ch, ch: ch, engine: e, market: m}
	resp, err := e.send(ctx, m, &request{kind: reqSubscribe, sub: s}, orWait)
	if err != nil {
		return nil, err
	}
	s.FromSeq = resp.depth.LastSeq
	return s, nil
}

// publish hands evs to every subscriber without waiting. A subscriber
// whose channel is full is dropped.
func (m *market) publish(evs []book.Event) {
	if len(m.subs) == 0 {
		return
	}
	u := Update{Symbol: m.symbol, Events: evs}
	kept := m.subs[:0]
	for _, s := range m.subs {
		select {
		case s.ch <- u:
			kept = append(kept, s)
		default:
			m.end(s, ErrSlowConsumer)
		}
	}
	clear(m.subs[len(kept):])
	m.subs = kept
}

// subscribe adds s; its FromSeq is the book's LastSeq now.
func (m *market) subscribe(s *Subscription) response {
	m.subs = append(m.subs, s)
	return response{depth: Depth{LastSeq: m.book.LastSeq()}}
}

// unsubscribe removes s if it is still subscribed.
func (m *market) unsubscribe(s *Subscription) {
	if i := indexOf(m.subs, s); i >= 0 {
		m.subs = append(m.subs[:i], m.subs[i+1:]...)
		m.end(s, nil)
	}
}

// end records why s ended and closes its channel.
func (m *market) end(s *Subscription, err error) {
	s.err = err
	close(s.ch)
}

func indexOf(subs []*Subscription, s *Subscription) int {
	for i, x := range subs {
		if x == s {
			return i
		}
	}
	return -1
}
