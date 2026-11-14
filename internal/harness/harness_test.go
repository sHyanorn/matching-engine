package harness

import (
	"math"
	"slices"
	"testing"

	"github.com/sHyanorn/matching-engine/internal/book"
)

const (
	buy  uint8 = 0
	sell uint8 = 1
)

// Message constructors in the order of conformance_check.py's NEW, CANCEL
// and MODIFY; seq is set by run.
func newOrder(id uint64, s uint8, qty uint32, price int64) Msg {
	return Msg{Type: MsgNew, OrderID: id, Side: s, Qty: qty, Price: price}
}

func newIOC(id uint64, s uint8, qty uint32, price int64) Msg {
	m := newOrder(id, s, qty, price)
	m.IOC = true
	return m
}

func cancelMsg(id uint64) Msg { return Msg{Type: MsgCancel, OrderID: id} }

func modify(id uint64, s uint8, qty uint32, price int64) Msg {
	return Msg{Type: MsgModify, OrderID: id, Side: s, Qty: qty, Price: price}
}

// step is one message and exactly the reports it must produce.
type step struct {
	msg  Msg
	want []Report
}

// run applies the steps to a new engine, numbering them from 0 as the
// benchmark does, and checks each one's reports and the book.
func run(t *testing.T, steps []step) *Engine {
	t.Helper()
	e := New()
	var out []Report
	for i, s := range steps {
		s.msg.Seq = uint64(i)
		var err error
		out, err = e.Apply(s.msg, out[:0])
		if err != nil {
			t.Fatalf("message %d %+v: %v", i, s.msg, err)
		}
		if !slices.Equal(out, s.want) {
			t.Fatalf("message %d %+v:\n got %+v\nwant %+v", i, s.msg, out, s.want)
		}
		if err := e.book.CheckInvariants(); err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
	}
	return e
}

func ack(seq, id uint64, s uint8, price int64, qty uint32) Report {
	return Report{Type: OrderAck, Seq: seq, OrderID: id, Side: s, Price: price, Qty: qty}
}

func trade(seq uint64, price int64, qty uint32, maker, taker uint64) Report {
	return Report{Type: Trade, Seq: seq, Price: price, Qty: qty, MakerID: maker, TakerID: taker}
}

func canceled(seq, id uint64, s uint8, price int64, qty uint32) Report {
	return Report{Type: CancelAck, Seq: seq, OrderID: id, Side: s, Price: price, Qty: qty}
}

func modified(seq, id uint64, s uint8, price int64, qty uint32) Report {
	return Report{Type: ModifyAck, Seq: seq, OrderID: id, Side: s, Price: price, Qty: qty}
}

func TestNewOrders(t *testing.T) {
	run(t, []step{
		{newOrder(1, sell, 10, 100), []Report{ack(0, 1, sell, 100, 10)}},
		{newOrder(2, sell, 10, 101), []Report{ack(1, 2, sell, 101, 10)}},
		// A GTC buy sweeps both asks at the makers' prices and rests 5.
		{newOrder(3, buy, 25, 105), []Report{
			ack(2, 3, buy, 105, 25), trade(2, 100, 10, 1, 3), trade(2, 101, 10, 2, 3),
		}},
		// An IOC sell takes the resting 5 and cancels the other 5.
		{newIOC(4, sell, 10, 90), []Report{
			ack(3, 4, sell, 90, 10), trade(3, 105, 5, 3, 4), canceled(3, 4, sell, 90, 5),
		}},
		// An IOC that trades nothing is canceled in full.
		{newIOC(5, buy, 7, 50), []Report{ack(4, 5, buy, 50, 7), canceled(4, 5, buy, 50, 7)}},
	})
}

func TestCancels(t *testing.T) {
	run(t, []step{
		{newOrder(1, buy, 10, 100), []Report{ack(0, 1, buy, 100, 10)}},
		{newOrder(2, sell, 4, 100), []Report{ack(1, 2, sell, 100, 4), trade(1, 100, 4, 1, 2)}},
		// The remaining 6 are canceled.
		{cancelMsg(1), []Report{canceled(2, 1, buy, 100, 6)}},
		// Not resting: canceled, filled, never seen, and ID 0.
		{cancelMsg(1), []Report{{Type: CancelReject, Seq: 3, OrderID: 1}}},
		{cancelMsg(2), []Report{{Type: CancelReject, Seq: 4, OrderID: 2}}},
		{cancelMsg(99), []Report{{Type: CancelReject, Seq: 5, OrderID: 99}}},
		{cancelMsg(0), []Report{{Type: CancelReject, Seq: 6, OrderID: 0}}},
		// The ID of a canceled order is free again.
		{newOrder(1, sell, 3, 200), []Report{ack(7, 1, sell, 200, 3)}},
	})
}

func TestModifies(t *testing.T) {
	e := run(t, []step{
		{newOrder(1, buy, 10, 99), []Report{ack(0, 1, buy, 99, 10)}},
		{newOrder(2, buy, 10, 99), []Report{ack(1, 2, buy, 99, 10)}},
		{newOrder(3, sell, 4, 100), []Report{ack(2, 3, sell, 100, 4)}},
		// Order 1 moves to the back of the queue at 99 with 11 lots.
		{modify(1, buy, 11, 99), []Report{modified(3, 1, buy, 99, 11)}},
		// Order 2 is repriced through the ask: it trades at the maker's
		// 100, not its own 101, and rests its other 8 at 101.
		{modify(2, buy, 12, 101), []Report{modified(4, 2, buy, 101, 12), trade(4, 100, 4, 3, 2)}},
		// A modify of an order that is not resting is rejected.
		{modify(3, sell, 5, 100), []Report{{Type: ModifyReject, Seq: 5, OrderID: 3}}},
		{modify(0, sell, 5, 100), []Report{{Type: ModifyReject, Seq: 6, OrderID: 0}}},
		// A sell at 99 meets order 2 at 101 first, then order 1.
		{newIOC(4, sell, 20, 99), []Report{
			ack(7, 4, sell, 99, 20), trade(7, 101, 8, 2, 4), trade(7, 99, 11, 1, 4), canceled(7, 4, sell, 99, 1),
		}},
	})
	if bid, ask := e.BestBid(), e.BestAsk(); bid != math.MinInt64 || ask != math.MaxInt64 {
		t.Fatalf("book not empty: best bid %d, best ask %d", bid, ask)
	}
}

func TestQueries(t *testing.T) {
	e := New()
	if bid, ask := e.BestBid(), e.BestAsk(); bid != math.MinInt64 || ask != math.MaxInt64 {
		t.Fatalf("empty book: best bid %d, best ask %d", bid, ask)
	}
	for i, m := range []Msg{
		newOrder(1, buy, 10, 98), newOrder(2, buy, 5, 99), newOrder(3, buy, 7, 99),
		newOrder(4, sell, 3, 101), newOrder(5, sell, 4, 103),
	} {
		m.Seq = uint64(i)
		if _, err := e.Apply(m, nil); err != nil {
			t.Fatal(err)
		}
	}
	if bid, ask := e.BestBid(), e.BestAsk(); bid != 99 || ask != 101 {
		t.Fatalf("best bid %d, best ask %d, want 99 and 101", bid, ask)
	}
	for _, c := range []struct {
		price int64
		side  uint8
		want  uint64
	}{
		{99, buy, 12}, {98, buy, 10}, {97, buy, 0}, {101, buy, 0},
		{101, sell, 3}, {103, sell, 4}, {102, sell, 0}, {99, sell, 0}, {99, 2, 0},
	} {
		if got := e.DepthAt(c.price, c.side); got != c.want {
			t.Errorf("DepthAt(%d, %d) = %d, want %d", c.price, c.side, got, c.want)
		}
	}
}

// TestErrors checks that a message the book cannot hold is an error with
// no report, and that a modify fails after its cancel half.
func TestErrors(t *testing.T) {
	for _, m := range []Msg{
		newOrder(1, 2, 10, 100),
		newOrder(1, buy, 10, 0),
		newOrder(1, buy, 10, -5),
		newOrder(1, buy, 10, int64(book.MaxPrice)+1),
		newOrder(1, buy, 0, 100),
		newOrder(1, buy, uint32(book.MaxQty)+1, 100),
		newOrder(0, buy, 10, 100),
		{Type: 3, OrderID: 1},
	} {
		if out, err := New().Apply(m, nil); err == nil || len(out) != 0 {
			t.Errorf("%+v: reports %+v, error %v; want an error alone", m, out, err)
		}
	}

	e := New()
	if _, err := e.Apply(newOrder(1, buy, 10, 100), nil); err != nil {
		t.Fatal(err)
	}
	if out, err := e.Apply(newOrder(1, sell, 10, 200), nil); err == nil || len(out) != 0 {
		t.Errorf("new order with a resting order's ID: reports %+v, error %v", out, err)
	}
	if out, err := e.Apply(modify(1, buy, 10, 0), nil); err == nil || len(out) != 0 {
		t.Errorf("modify to price 0: reports %+v, error %v", out, err)
	}
	if bid := e.BestBid(); bid != math.MinInt64 {
		t.Errorf("after the failed modify the best bid is %d, want order 1 gone", bid)
	}
}
