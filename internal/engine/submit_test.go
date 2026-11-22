package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/flow"
	"github.com/sHyanorn/matching-engine/internal/spin"
	"github.com/sHyanorn/matching-engine/internal/testclock"
)

// call is one call of a recording sink.
type call struct {
	flush bool
	sink  int
	tag   uint64
	reply Reply // Events is a copy
}

// recorder keeps the calls of the sinks made from it, in order.
type recorder struct {
	mu    sync.Mutex
	calls []call
}

type recSink struct {
	rec *recorder
	id  int
}

func (s *recSink) Reply(tag uint64, r Reply) {
	r.Events = slices.Clone(r.Events)
	s.rec.mu.Lock()
	s.rec.calls = append(s.rec.calls, call{sink: s.id, tag: tag, reply: r})
	s.rec.mu.Unlock()
}

func (s *recSink) Flush() {
	s.rec.mu.Lock()
	s.rec.calls = append(s.rec.calls, call{flush: true, sink: s.id})
	s.rec.mu.Unlock()
}

func (rec *recorder) sink(id int) *recSink { return &recSink{rec: rec, id: id} }

// replies returns the calls once n of them are Replies.
func (rec *recorder) replies(t *testing.T, n int) []call {
	t.Helper()
	var got []call
	waitFor(t, "replies", func() bool {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		got = slices.Clone(rec.calls)
		k := 0
		for _, c := range got {
			if !c.flush {
				k++
			}
		}
		return k >= n
	})
	return got
}

// chanSink passes on each reply, with one command in flight at a time.
type chanSink struct{ ch chan call }

func (s *chanSink) Reply(tag uint64, r Reply) {
	r.Events = slices.Clone(r.Events)
	s.ch <- call{tag: tag, reply: r}
}

func (s *chanSink) Flush() {}

// TestSendMatchesPlace runs the SPEC §9 flow through PlaceOrder and
// CancelOrder on one engine and through SendOrder and SendCancel on another:
// every command gets the same result, and the two command logs are the same
// bytes.
func TestSendMatchesPlace(t *testing.T) {
	ctx := context.Background()
	n := 20_000
	if testing.Short() {
		n = 2_000
	}
	dirA, dirB := t.TempDir(), t.TempDir()
	cfg := func(dir string) Config {
		var now int64
		return Config{Symbols: []string{"BTC-USD"}, DataDir: dir, Clock: func() int64 { now++; return now }}
	}
	a, err := New(cfg(dirA))
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(cfg(dirB))
	if err != nil {
		t.Fatal(err)
	}
	s := &chanSink{ch: make(chan call, 1)}

	g := flow.New(1, 5)
	ids := make([]book.OrderID, 1, n+1) // by the flow's order number
	for i := 1; i <= n; i++ {
		op := g.Next()
		var want Result
		var wantErr error
		var sendErr error
		if op.Cancel {
			want, wantErr = a.CancelOrder(ctx, ids[op.Order])
			sendErr = b.SendCancel(s, uint64(i), ids[op.Order])
		} else {
			o := limitOrder("BTC-USD", book.Sell, book.Price(op.Price), book.Qty(op.Qty))
			if op.Buy {
				o.Side = book.Buy
			}
			want, wantErr = a.PlaceOrder(ctx, o)
			var id book.OrderID
			id, sendErr = b.SendOrder(s, uint64(i), o)
			if id != want.OrderID {
				t.Fatalf("op %d: SendOrder took ID %d, PlaceOrder %d", i, id, want.OrderID)
			}
			ids = append(ids, id)
		}
		if wantErr != nil || sendErr != nil {
			t.Fatalf("op %d: errors %v and %v", i, wantErr, sendErr)
		}
		got := <-s.ch
		if got.tag != uint64(i) || got.reply.Err != nil || got.reply.OrderID != want.OrderID ||
			!slices.Equal(got.reply.Events, want.Events) || got.reply.Status != want.Status {
			t.Fatalf("op %d: sink got %+v, PlaceOrder %+v", i, got, want)
		}
	}
	if err := errors.Join(a.Close(), b.Close()); err != nil {
		t.Fatal(err)
	}
	logA, errA := os.ReadFile(filepath.Join(dirA, "BTC-USD.log"))
	logB, errB := os.ReadFile(filepath.Join(dirB, "BTC-USD.log"))
	if errA != nil || errB != nil {
		t.Fatal(errA, errB)
	}
	if !slices.Equal(logA, logB) {
		t.Errorf("command logs differ: %d and %d bytes", len(logA), len(logB))
	}
}

// TestSendErrorsAreReturned: a command that fails before the queue returns
// its error and the ID taken, and its sink hears nothing.
func TestSendErrorsAreReturned(t *testing.T) {
	rec := &recorder{}
	s := rec.sink(0)
	e := newTestEngine(t, Config{Symbols: []string{"A"}})

	if id, err := e.SendOrder(s, 1, limitOrder("NOPE", book.Buy, 100, 1)); id != 0 || !errors.Is(err, ErrUnknownSymbol) {
		t.Errorf("unknown symbol: ID %d, %v", id, err)
	}
	var inv *InvalidError
	bad := limitOrder("A", book.Buy, 100, 0)
	if id, err := e.SendOrder(s, 2, bad); id == 0 || !errors.As(err, &inv) || inv.Reason != book.ReasonInvalidQty {
		t.Errorf("zero qty: ID %d, %v; want its ID and invalid_qty", id, err)
	}
	long := limitOrder("A", book.Buy, 100, 1)
	long.ClientID = string(make([]byte, MaxClientIDLen+1))
	if id, err := e.SendOrder(s, 3, long); id != 0 || !errors.Is(err, ErrClientIDLen) {
		t.Errorf("long client_id: ID %d, %v", id, err)
	}
	if err := e.SendCancel(s, 4, 0); !errors.As(err, &inv) || inv.Reason != book.ReasonInvalidOrderID {
		t.Errorf("cancel of ID 0: %v", err)
	}
	if err := e.SendCancel(s, 5, makeOrderID(1<<30, 0)); !errors.Is(err, ErrUnknownOrder) {
		t.Errorf("cancel of an ID never issued: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if id, err := e.SendOrder(s, 6, limitOrder("A", book.Buy, 100, 1)); !errors.Is(err, ErrClosed) || id == 0 {
		t.Errorf("after Close: ID %d, %v; want ErrClosed", id, err)
	}
	if err := e.SendCancel(s, 7, makeOrderID(1, 0)); !errors.Is(err, ErrClosed) {
		t.Errorf("cancel after Close: %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.calls) != 0 {
		t.Errorf("the sink was called %d times: %+v", len(rec.calls), rec.calls)
	}
}

// TestSendFullQueueReturnsBusy stalls the symbol's goroutine, fills its
// queue and checks that the next send returns ErrBusy at once, that the
// sink hears of no more than the commands queued, and that every one of
// them is answered, in order, once the goroutine resumes.
func TestSendFullQueueReturnsBusy(t *testing.T) {
	const queue = 4
	g := testclock.New(nil)
	e := newTestEngine(t, Config{Symbols: []string{"A"}, QueueSize: queue, MaxBatch: 1, Clock: g.Now})
	t.Cleanup(g.ReleaseAll)
	rec := &recorder{}
	s := rec.sink(0)

	stalled, release := g.Stall()
	if _, err := e.SendOrder(s, 1, limitOrder("A", book.Buy, 100, 1)); err != nil {
		t.Fatal(err)
	}
	<-stalled
	for i := uint64(2); i < 2+queue; i++ {
		if _, err := e.SendOrder(s, i, limitOrder("A", book.Buy, 100, 1)); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	if _, err := e.SendOrder(s, 9, limitOrder("A", book.Buy, 100, 1)); !errors.Is(err, ErrBusy) {
		t.Fatalf("send to a full queue: %v, want ErrBusy", err)
	}
	if err := e.SendCancel(s, 10, makeOrderID(1, 0)); !errors.Is(err, ErrBusy) {
		t.Fatalf("cancel to a full queue: %v, want ErrBusy", err)
	}
	release()
	var tags []uint64
	for _, c := range rec.replies(t, 1+queue) {
		if !c.flush {
			tags = append(tags, c.tag)
		}
	}
	if !slices.Equal(tags, []uint64{1, 2, 3, 4, 5}) {
		t.Errorf("replies for tags %v, want 1 to 5", tags)
	}
}

// TestFlushFollowsTheBatch: a sink gets one Flush for each run of its
// Replies in a batch, after the last of them and after every Reply of the
// batch.
func TestFlushFollowsTheBatch(t *testing.T) {
	g := testclock.New(nil)
	e := newTestEngine(t, Config{Symbols: []string{"A"}, Clock: g.Now})
	t.Cleanup(g.ReleaseAll)
	rec := &recorder{}
	x, y := rec.sink(1), rec.sink(2)

	// The first order is a batch of its own, held in the clock; the others
	// queue up behind it and make the second batch.
	stalled, release := g.Stall()
	send := func(s *recSink, tag uint64) {
		t.Helper()
		if _, err := e.SendOrder(s, tag, limitOrder("A", book.Buy, 100, 1)); err != nil {
			t.Fatal(err)
		}
	}
	send(x, 1)
	<-stalled
	for _, c := range []struct {
		s   *recSink
		tag uint64
	}{{x, 2}, {x, 3}, {y, 4}, {x, 5}} {
		send(c.s, c.tag)
	}
	release()
	waitFor(t, "the second batch", func() bool {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return len(rec.calls) >= 9
	})
	rec.mu.Lock()
	defer rec.mu.Unlock()
	type step struct {
		flush bool
		sink  int
		tag   uint64
	}
	var got []step
	for _, c := range rec.calls {
		got = append(got, step{c.flush, c.sink, c.tag})
	}
	want := []step{
		{false, 1, 1}, {true, 1, 0}, // the first batch
		{false, 1, 2}, {false, 1, 3}, {false, 2, 4}, {false, 1, 5},
		{true, 1, 0}, {true, 2, 0}, {true, 1, 0}, // once for each run, after the batch
	}
	if !slices.Equal(got, want) {
		t.Errorf("calls %v\nwant  %v", got, want)
	}
}

// TestSendEventsReachSubscribers: with a subscriber on the symbol, the sink
// still gets the events at once and the subscriber gets the same events,
// intact after the book has reused its buffer for later commands.
func TestSendEventsReachSubscribers(t *testing.T) {
	ctx := context.Background()
	const n = 300
	e := newTestEngine(t, Config{Symbols: []string{"A"}})
	sub, err := e.Subscribe(ctx, "A", 2*n, 0)
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	s := rec.sink(0)
	g := flow.New(7, 3)
	ids := make([]book.OrderID, 1, n+1)
	for i := 1; i <= n; i++ {
		op := g.Next()
		var err error
		if op.Cancel {
			err = e.SendCancel(s, uint64(i), ids[op.Order])
		} else {
			o := limitOrder("A", book.Sell, book.Price(op.Price), book.Qty(op.Qty))
			if op.Buy {
				o.Side = book.Buy
			}
			var id book.OrderID
			id, err = e.SendOrder(s, uint64(i), o)
			ids = append(ids, id)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	var fromSink [][]book.Event
	for _, c := range rec.replies(t, n) {
		if !c.flush {
			fromSink = append(fromSink, c.reply.Events)
		}
	}
	for i := range n {
		u := <-sub.C
		if !slices.Equal(u.Events, fromSink[i]) {
			t.Fatalf("command %d: subscriber got %+v, sink %+v", i+1, u.Events, fromSink[i])
		}
	}
}

// TestSendLogFailed: a failed fsync after the batch is reported with the
// events, and later commands fail without them, as PlaceOrder reports them.
func TestSendLogFailed(t *testing.T) {
	cfg := Config{Symbols: []string{"A"}, DataDir: t.TempDir(), SyncInterval: -1}
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	injectFault(e, &faultyLog{limit: -1, syncErr: errInjected})
	rec := &recorder{}
	s := rec.sink(0)
	if _, err := e.SendOrder(s, 1, limitOrder("A", book.Buy, 100, 1)); err != nil {
		t.Fatal(err)
	}
	rec.replies(t, 1)
	if _, err := e.SendOrder(s, 2, limitOrder("A", book.Buy, 99, 1)); err != nil {
		t.Fatal(err)
	}
	var replies []Reply
	for _, c := range rec.replies(t, 2) {
		if !c.flush {
			replies = append(replies, c.reply)
		}
	}
	if !errors.Is(replies[0].Err, ErrLogFailed) || len(replies[0].Events) != 2 || replies[0].Status.State != StateOpen {
		t.Errorf("first: %v, %d events, %v; want ErrLogFailed, the events and the order", replies[0].Err, len(replies[0].Events), replies[0].Status.State)
	}
	if !errors.Is(replies[1].Err, ErrLogFailed) || len(replies[1].Events) != 0 || replies[1].OrderID == 0 {
		t.Errorf("second: %v, %d events, ID %d; want ErrLogFailed, not applied", replies[1].Err, len(replies[1].Events), replies[1].OrderID)
	}
	if err := e.Close(); !errors.Is(err, ErrLogFailed) {
		t.Fatalf("Close = %v", err)
	}
}

// TestSendAllocatesNothing: once the book, the order history and the
// tracker have grown to the size of the flow, a command allocates nothing on
// its way through the queue, the book, the log and the sink.
func TestSendAllocatesNothing(t *testing.T) { sendAllocatesNothing(t, nil, testing.AllocsPerRun) }

// sendAllocatesNothing runs the symbol on a core of pool, if there is one,
// and counts allocations with allocsPerRun.
func sendAllocatesNothing(t *testing.T, pool *spin.Pool, allocsPerRun func(int, func()) float64) {
	e := newTestEngine(t, Config{Symbols: []string{"A"}, History: 1000, Spin: pool})
	const batch = 250
	tokens := make(chan struct{}, batch)
	s := &tokenSink{tokens: tokens}
	g := flow.New(3, 5)
	var ids []book.OrderID
	ids = append(ids, 0)
	queue := func() {
		op := g.Next()
		var err error
		if op.Cancel {
			err = e.SendCancel(s, 0, ids[op.Order])
		} else {
			o := limitOrder("A", book.Sell, book.Price(op.Price), book.Qty(op.Qty))
			if op.Buy {
				o.Side = book.Buy
			}
			var id book.OrderID
			id, err = e.SendOrder(s, 0, o)
			ids = append(ids, id)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	send := func() {
		queue()
		<-tokens
	}
	// The warm-up queues batches: the answer of a spinning symbol wakes the
	// test through the scheduler, which takes long if done for each command.
	for range 50_000 / batch {
		for range batch {
			queue()
		}
		for range batch {
			<-tokens
		}
	}
	ids = slices.Grow(ids, 10_000)
	if n := allocsPerRun(5_000, send); n != 0 {
		t.Errorf("%v allocations per command, want 0", n)
	}
}

type tokenSink struct{ tokens chan struct{} }

func (s *tokenSink) Reply(uint64, Reply) { s.tokens <- struct{}{} }
func (s *tokenSink) Flush()              {}
