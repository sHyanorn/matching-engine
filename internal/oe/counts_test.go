package oe

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/engine"
)

// counted is one command a recorder was told of.
type counted struct {
	cancel bool
	symbol string             // of an order
	id     book.OrderID       // of a cancel
	events int                // how many
	status engine.OrderStatus // of a cancel
	err    error
	next   uint64 // the session's next server_seq when it was counted
}

// recorder is a Counts that keeps what it is told. next, if set, reads the
// next server_seq of the session under test.
type recorder struct {
	mu    sync.Mutex
	calls []counted
	next  func() uint64
}

func (r *recorder) add(c counted) {
	if r.next != nil {
		c.next = r.next()
	}
	r.mu.Lock()
	r.calls = append(r.calls, c)
	r.mu.Unlock()
}

func (r *recorder) CountOrder(symbol string, events []book.Event, err error) {
	r.add(counted{symbol: symbol, events: len(events), err: err})
}

func (r *recorder) CountCancel(id book.OrderID, events []book.Event, status engine.OrderStatus, err error) {
	r.add(counted{cancel: true, id: id, events: len(events), status: status, err: err})
}

// take returns what was counted since the last call.
func (r *recorder) take() []counted {
	r.mu.Lock()
	defer r.mu.Unlock()
	calls := r.calls
	r.calls = nil
	return calls
}

// countedReply is how the reply r to a command is counted; symbol is an
// order's.
func countedReply(symbol string, r engine.Reply) counted {
	if r.Kind == book.CommandCancel {
		return counted{cancel: true, id: r.OrderID, events: len(r.Events), status: r.Status, err: r.Err}
	}
	return counted{symbol: symbol, events: len(r.Events), err: r.Err}
}

// feed has c's reader take the frames, and returns what the session then
// queued on c.
func feed(t *testing.T, c *conn, frames ...frame) []frame {
	t.Helper()
	var buf []byte
	for _, f := range frames {
		buf = f.Append(buf[:0])
		typ, body, _, err := Next(buf)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.handle(typ, body); err != nil {
			t.Fatalf("%+v: %v", f, err)
		}
	}
	return queued(t, c)
}

// queued returns the frames queued on c and empties its queue.
func queued(t *testing.T, c *conn) []frame {
	t.Helper()
	c.ss.mu.Lock()
	out := append([]byte(nil), c.out...)
	c.out = c.out[:0]
	c.ss.mu.Unlock()
	var fs []frame
	for len(out) > 0 {
		typ, body, n, err := Next(out)
		if err != nil || n == 0 {
			t.Fatalf("a bad frame was queued: %v", err)
		}
		fs = append(fs, decode(typ, body))
		out = out[n:]
	}
	return fs
}

// TestEveryAnswerIsCountedOnce: a session tells Counts of each NewOrder and
// Cancel when it answers it, with what the engine gave, before any frame of
// the answer is numbered. A frame sent again, a replayed frame and an order
// for a symbol that does not exist are not counted.
func TestEveryAnswerIsCountedOnce(t *testing.T) {
	fe, rec := &fakeEngine{}, &recorder{}
	srv := NewServer(ServerOptions{Engine: fe, Counts: rec, Symbols: testSymbols})
	c := attached(t, srv)
	rec.next = func() uint64 { return c.ss.nextServerSeq } // the test has the only goroutine
	refuse := func(err error) {
		fe.mu.Lock()
		fe.refuse = func(fakeCall) error { return err }
		fe.mu.Unlock()
	}
	unknownSymbol := func(seq uint64) NewOrder {
		o := buyOrder(seq)
		o.Symbol = uint8(len(testSymbols))
		return o
	}
	// expect checks that the frames are answered as one command with status
	// and counted as want, at the server_seq of the answer's first frame.
	next := uint64(1)
	expect := func(what string, status Status, want []counted, frames ...frame) {
		t.Helper()
		fs := feed(t, c, frames...)
		for i := range want {
			want[i].next = next
		}
		if got := rec.take(); !slices.Equal(got, want) {
			t.Errorf("%s: counted %+v, want %+v", what, got, want)
		}
		if len(fs) == 0 {
			if status != 0 {
				t.Fatalf("%s: no answer, want status %v", what, status)
			}
			return
		}
		d, ok := fs[len(fs)-1].(Done)
		if !ok || d.Status != status || d.ServerSeq != next+uint64(len(fs))-1 {
			t.Fatalf("%s: answered %+v, want a Done with status %v", what, fs, status)
		}
		next += uint64(len(fs))
	}

	order := fakeReply(fakeCall{id: 1})
	expect("an order", StatusOK, []counted{countedReply("ETH-USD", order)}, buyOrder(1))
	expect("a cancel", StatusOK, []counted{countedReply("", fakeReply(fakeCall{cancel: true, id: 1}))}, Cancel{ClientSeq: 2, OrderID: 1})
	expect("frames sent again", 0, nil, buyOrder(1), Cancel{ClientSeq: 2, OrderID: 1})
	expect("an unknown symbol", StatusUnknownSymbol, nil, unknownSymbol(3))

	refuse(engine.ErrBusy)
	expect("a busy order", StatusBusy, []counted{{symbol: "ETH-USD", err: engine.ErrBusy}}, buyOrder(4))
	expect("a busy cancel", StatusBusy, []counted{{cancel: true, id: 1, err: engine.ErrBusy}}, Cancel{ClientSeq: 5, OrderID: 1})
	refuse(engine.ErrUnknownOrder)
	expect("a cancel of an ID not issued", StatusUnknownOrder, []counted{{cancel: true, id: 77, err: engine.ErrUnknownOrder}}, Cancel{ClientSeq: 6, OrderID: 77})
	refuse(engine.ErrClosed)
	expect("an order to a closed engine", StatusShuttingDown, []counted{{symbol: "ETH-USD", err: engine.ErrClosed}}, buyOrder(7))

	// A resume replays the frames from server_seq 1 without counting again.
	again := &conn{srv: srv, nc: nullConn{}, wake: make(chan struct{}, 1), readerDone: make(chan struct{})}
	c.ss.mu.Lock()
	reason, _ := again.resumeLocked(c.ss, 1)
	c.ss.mu.Unlock()
	if replayed := queued(t, again); reason != 0 || uint64(len(replayed)) != next || len(rec.take()) != 0 {
		t.Fatalf("resume refused with %v, or %d frames replayed for %d sent, or the replay was counted", reason, len(replayed)-1, next-1)
	}
	c = again

	// A session that is shutting down counts what it answers shutting_down
	// with the error REST counts at shutdown.
	refuse(nil)
	fe.mu.Lock()
	fe.hold = true
	fe.mu.Unlock()
	expect("an order the engine holds", 0, nil, buyOrder(8))
	c.ss.mu.Lock()
	c.ss.beginDrainLocked(drainShutdown)
	c.ss.mu.Unlock()
	expect("an order at shutdown", StatusShuttingDown, []counted{{symbol: "ETH-USD", err: engine.ErrClosed}}, buyOrder(9))
	expect("a cancel at shutdown", StatusShuttingDown, []counted{{cancel: true, id: 1, err: engine.ErrClosed}}, Cancel{ClientSeq: 10, OrderID: 1})
	expect("an unknown symbol at shutdown", StatusShuttingDown, nil, unknownSymbol(11))
}

// TestAnAnswerAfterTheSessionIsDroppedIsCounted: the engine answers a
// command whose session has expired. No Done is sent, and the command is
// counted, as the book has applied it.
func TestAnAnswerAfterTheSessionIsDroppedIsCounted(t *testing.T) {
	fe, rec := &fakeEngine{hold: true}, &recorder{}
	srv := NewServer(ServerOptions{Engine: fe, Counts: rec, Symbols: testSymbols,
		afterFunc: func(time.Duration, func()) func() bool { return func() bool { return true } }})
	c := attached(t, srv)
	ss := c.ss
	if fs := feed(t, c, buyOrder(1)); len(fs) != 0 || len(rec.take()) != 0 {
		t.Fatalf("an order the engine holds was answered with %+v or counted", fs)
	}
	ss.mu.Lock()
	c.state = aborted
	ss.mu.Unlock()
	ss.detach(c)
	srv.expire(ss)
	fe.answer(1)
	ss.mu.Lock()
	gone, sent := ss.gone, ss.nextServerSeq-1
	ss.mu.Unlock()
	want := []counted{countedReply("ETH-USD", fakeReply(fakeCall{id: 1}))}
	if got := rec.take(); !gone || sent != 0 || !slices.Equal(got, want) {
		t.Errorf("session dropped: %v, %d frames numbered, counted %+v; want true, 0 and %+v", gone, sent, got, want)
	}
}
