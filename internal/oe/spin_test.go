package oe

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/engine"
	"github.com/sHyanorn/matching-engine/internal/flow"
	"github.com/sHyanorn/matching-engine/internal/spin"
	"github.com/sHyanorn/matching-engine/internal/spin/spintest"
)

// fixedClockEngine returns an engine whose clock counts commands, so that two
// engines given the same commands in the same order stamp them alike. It
// spins on pool, if there is one, and closes when the test ends, after the
// servers.
func fixedClockEngine(t testing.TB, symbols []string, pool *spin.Pool) *engine.Engine {
	t.Helper()
	var now atomic.Int64
	e, err := engine.New(engine.Config{Symbols: symbols, DataDir: t.TempDir(), Clock: func() int64 { return now.Add(1) }, Spin: pool})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

// rawClient reads the frames of a session as the server sent them.
type rawClient struct {
	t   *testing.T
	c   net.Conn
	buf []byte
}

func rawDial(t *testing.T, addr string) *rawClient {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &rawClient{t: t, c: c}
}

func (r *rawClient) send(frames ...frame) {
	r.t.Helper()
	var b []byte
	for _, f := range frames {
		b = f.Append(b)
	}
	if _, err := r.c.Write(b); err != nil {
		r.t.Fatalf("write: %v", err)
	}
}

// next returns the next frame, whole.
func (r *rawClient) next() (Type, []byte) {
	r.t.Helper()
	_ = r.c.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		typ, _, n, err := Next(r.buf)
		if err != nil {
			r.t.Fatalf("from the server: %v", err)
		}
		if n > 0 {
			raw := bytes.Clone(r.buf[:n])
			r.buf = r.buf[n:]
			return typ, raw
		}
		var tmp [4096]byte
		k, err := r.c.Read(tmp[:])
		r.buf = append(r.buf, tmp[:k]...)
		if err != nil && k == 0 {
			r.t.Fatalf("read: %v", err)
		}
	}
}

// login opens a session and reads the frames that answer the Login: the
// LoginAccepted and a Symbol for each of symbols.
func (r *rawClient) login(symbols int) {
	r.t.Helper()
	r.send(Login{Version: Version})
	for range 1 + symbols {
		r.next()
	}
}

// done is the Done in the frame raw.
func done(raw []byte) Done { return DecodeDone(raw[HeaderSize:]) }

// sessionFrames runs cmds over one session, one at a time, against a new
// engine with a fixed clock, and returns every Event and Done frame the
// session received, bytes as sent.
func sessionFrames(t *testing.T, pool *spin.Pool, cmds []parityCommand) [][]byte {
	t.Helper()
	e := fixedClockEngine(t, testSymbols, pool)
	_, addr := startServer(t, ServerOptions{Engine: e, Symbols: e.Symbols(), Heartbeat: time.Hour, Spin: pool})
	c := rawDial(t, addr)
	c.login(len(testSymbols))
	var frames [][]byte
	var ids []book.OrderID // the order each command made, 0 for none
	for i, cmd := range cmds {
		seq := uint64(i + 1)
		id := cmd.id
		if cmd.target >= 0 {
			id = ids[cmd.target]
		}
		if cmd.cancel {
			c.send(Cancel{ClientSeq: seq, OrderID: id})
		} else {
			c.send(NewOrder{ClientSeq: seq, Symbol: cmd.symIdx, Side: book.Side(cmd.sideN), Type: book.OrderType(cmd.typN), TIF: book.TIF(cmd.tifN), Price: book.Price(cmd.price), Qty: book.Qty(cmd.qty)})
		}
		for {
			typ, raw := c.next()
			if typ != TypeEvent && typ != TypeDone {
				t.Fatalf("command %d: got a %q frame, want Events and a Done", i, byte(typ))
			}
			frames = append(frames, raw)
			if typ == TypeDone {
				ids = append(ids, 0)
				if !cmd.cancel {
					ids[i] = done(raw).OrderID
				}
				break
			}
		}
	}
	return frames
}

// TestSpinSessionIsTheSameBytes: the Event and Done frames of a session
// that runs with the mode on are the bytes a session without it gets, for
// every kind of answer.
func TestSpinSessionIsTheSameBytes(t *testing.T) {
	n := 4_000
	if testing.Short() {
		n = 400
	}
	cmds := parityCommands(n)
	want := sessionFrames(t, nil, cmds)
	pool, log := spintest.Some(t, 4)
	got := sessionFrames(t, pool, cmds)
	if len(log.Pinned()) == 0 {
		t.Skip("no goroutine could pin itself")
	}
	if len(want) < 2*len(cmds) {
		t.Errorf("%d frames for %d commands: the commands did not reach the book", len(want), len(cmds))
	}
	if len(got) != len(want) {
		t.Fatalf("%d frames with the mode on, %d without", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("frame %d with the mode on:\n%x\nand without it:\n%x", i, got[i], want[i])
		}
	}
}

// sessionResult is how a session of orders and cancels left the engine.
type sessionResult struct {
	depth    engine.Depth
	statuses []engine.OrderStatus // of the orders placed, in order
}

// flowSession sends n commands of the SPEC §9 flow over one session in
// batches of 250, as BenchmarkOrders does, to a new engine with a fixed
// clock, and returns what is left on its book and the status of every
// order. A cancel whose order is not yet answered is left out of the flow.
func flowSession(t *testing.T, pool *spin.Pool, n int) sessionResult {
	t.Helper()
	e := fixedClockEngine(t, []string{"BTC-USD"}, pool)
	_, addr := startServer(t, ServerOptions{Engine: e, Symbols: e.Symbols(), Heartbeat: time.Hour, Spin: pool})
	c := rawDial(t, addr)
	c.login(1)
	g := flow.New(7, 5)
	var orders []book.OrderID // the ID of each order answered, by the flow's order number minus 1
	isOrder := map[uint64]bool{}
	var seq uint64
	for sent := 0; sent < n; {
		var frames []frame
		for len(frames) < min(250, n-sent) {
			op := g.Next()
			if op.Cancel && op.Order > len(orders) {
				continue
			}
			seq++
			if op.Cancel {
				frames = append(frames, Cancel{ClientSeq: seq, OrderID: orders[op.Order-1]})
				continue
			}
			side := book.Sell
			if op.Buy {
				side = book.Buy
			}
			isOrder[seq] = true
			frames = append(frames, NewOrder{ClientSeq: seq, Side: side, Type: book.Limit, TIF: book.GTC, Price: book.Price(op.Price), Qty: book.Qty(op.Qty)})
		}
		c.send(frames...)
		var answered []book.OrderID
		for left := len(frames); left > 0; {
			if typ, raw := c.next(); typ == TypeDone {
				left--
				if d := done(raw); d.Status != StatusOK && d.Status != StatusOrderFinished {
					t.Fatalf("command %d answered %v", d.ClientSeq, d.Status)
				} else if isOrder[d.ClientSeq] {
					answered = append(answered, d.OrderID)
				}
			}
		}
		orders = append(orders, answered...)
		sent += len(frames)
	}
	res := sessionResult{}
	var err error
	if res.depth, err = e.Depth(context.Background(), "BTC-USD", 1000); err != nil {
		t.Fatal(err)
	}
	for _, id := range orders {
		st, ok, err := e.Order(context.Background(), id)
		if err != nil || !ok {
			t.Fatalf("order %d: %+v, %v, %v", id, st, ok, err)
		}
		res.statuses = append(res.statuses, st)
	}
	return res
}

// TestSpinSessionOfOrdersAndCancels: a session of 3,000 orders and cancels
// against the real engine, both running in spin mode, ends with the book and
// the order statuses it ends with otherwise.
func TestSpinSessionOfOrdersAndCancels(t *testing.T) {
	const n = 3000
	want := flowSession(t, nil, n)
	pool, log := spintest.Some(t, 3)
	got := flowSession(t, pool, n)
	if len(log.Pinned()) == 0 {
		t.Skip("no goroutine could pin itself")
	}
	if !reflect.DeepEqual(got.depth, want.depth) {
		t.Errorf("book with the mode on:\n%+v\nand without it:\n%+v", got.depth, want.depth)
	}
	if len(got.statuses) != len(want.statuses) {
		t.Fatalf("%d orders with the mode on, %d without", len(got.statuses), len(want.statuses))
	}
	for i := range want.statuses {
		if !reflect.DeepEqual(got.statuses[i], want.statuses[i]) {
			t.Fatalf("order %d with the mode on: %+v, and without it: %+v", i, got.statuses[i], want.statuses[i])
		}
	}
	states := map[engine.OrderState]int{}
	for _, st := range want.statuses {
		states[st.State]++
	}
	if len(want.depth.Bids)+len(want.depth.Asks) == 0 || states[engine.StateOpen] == 0 || states[engine.StateFilled] == 0 || states[engine.StateCanceled] == 0 {
		t.Errorf("the flow left %d bid and %d ask levels and the states %v: it must leave a book and open, filled and canceled orders", len(want.depth.Bids), len(want.depth.Asks), states)
	}
}

// connGoroutines counts the readers and writers of connections that are
// running.
func connGoroutines() int {
	buf := make([]byte, 1<<20)
	stacks := string(buf[:runtime.Stack(buf, true)])
	return strings.Count(stacks, "oe.(*conn).serve(") + strings.Count(stacks, "oe.(*conn).write(")
}

// pinnedCount is how many goroutines have pinned themselves.
func pinnedCount(log *spintest.Log) int {
	n := 0
	for _, r := range log.Records() {
		if r.Msg == "spin: pinned" {
			n++
		}
	}
	return n
}

// TestSpinCoresFollowTheConnection: a connection's reader and writer take
// two cores when it logs in, give them back when the connection ends, for
// whatever reason, and the next one gets them: a resume, one that takes
// over a connection that is still attached, a Logout and a shutdown.
func TestSpinCoresFollowTheConnection(t *testing.T) {
	pool, log := spintest.New(t, 2)
	s, addr := startServer(t, ServerOptions{Spin: pool, Heartbeat: time.Hour})
	pins, ended := 0, func() {
		t.Helper()
		waitFor(t, "the cores to come back", func() bool { return pool.Free() == 2 })
		waitFor(t, "the readers and writers to end", func() bool { return connGoroutines() == 0 })
	}
	started := func() {
		t.Helper()
		pins += 2
		waitFor(t, "the reader and the writer to pin themselves", func() bool { return pool.Free() == 0 && pinnedCount(log) == pins })
	}

	a := dial(t, addr)
	id := a.login()
	started()
	if err := pingPong(a, 200); err != nil {
		t.Fatal(err)
	}
	pinned := log.Pinned()
	if len(pinned) != 2 || pinned[fmt.Sprintf("session %d reader", id)] != pool.CPUs()[0] || pinned[fmt.Sprintf("session %d writer", id)] != pool.CPUs()[1] {
		t.Errorf("pinned %v, want the reader on CPU %d and the writer on %d, in the order they were asked", pinned, pool.CPUs()[0], pool.CPUs()[1])
	}

	// The client goes away: its goroutines end, the session stays.
	_ = a.c.Close()
	ended()

	b := dial(t, addr)
	b.resume(id, 1)
	b.expect(LoginAccepted{Session: id, NextServerSeq: 1, LastClientSeq: 200})
	started()

	// A Login that takes the session over gets the cores of the connection
	// it closes.
	c := dial(t, addr)
	c.resume(id, 1)
	c.expect(LoginAccepted{Session: id, NextServerSeq: 1, LastClientSeq: 200})
	b.expectClosed()
	started()

	c.send(Logout{})
	c.expectClosed()
	ended()

	d := dial(t, addr)
	d.login()
	started()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if n := pool.Free(); n != 2 {
		t.Errorf("%d cores free after Close, want 2: it waits for the goroutines of every connection", n)
	}
	d.expect(EndOfSession{})
	d.expectClosed()
	ended()
	if u := log.Unserved(); len(u) != 0 {
		t.Errorf("goroutines found no core free: %v", u)
	}
}

// A goroutine for which the pool has no core runs as it does without one,
// blocking and unlocked, and the session works.
func TestSpinSessionWithoutEnoughCores(t *testing.T) {
	for _, cores := range []int{0, 1} {
		t.Run(fmt.Sprintf("%d cores", cores), func(t *testing.T) {
			pool, log := spintest.New(t, cores)
			_, addr := startServer(t, ServerOptions{Spin: pool, Heartbeat: time.Hour})
			c := dial(t, addr)
			id := c.login()
			reader, writer := fmt.Sprintf("session %d reader", id), fmt.Sprintf("session %d writer", id)
			want := []string{reader, writer}[cores:] // the reader is asked for first
			if got := log.Unserved(); !reflect.DeepEqual(got, want) {
				t.Errorf("goroutines without a core: %v, want %v", got, want)
			}
			waitFor(t, "the goroutines to pin themselves", func() bool { return pinnedCount(log) == cores })
			if err := pingPong(c, 500); err != nil {
				t.Fatal(err)
			}
			if cores == 1 && log.Pinned()[reader] != pool.CPUs()[0] {
				t.Errorf("pinned %v, want the reader on CPU %d", log.Pinned(), pool.CPUs()[0])
			}
			_ = c.c.Close()
			waitFor(t, "the cores to come back", func() bool { return pool.Free() == cores })
		})
	}
}

// A connection that has no socket to poll, as a pipe has not, keeps its
// reader as it is and gives the reader's core back.
func TestSpinReaderNeedsASocket(t *testing.T) {
	pool, log := spintest.New(t, 2)
	ln := servePipes(t, ServerOptions{Spin: pool, Heartbeat: time.Hour})
	c := &client{t: t, c: ln.dial()}
	t.Cleanup(func() { _ = c.c.Close() })
	id := c.login()
	waitFor(t, "the writer to pin itself", func() bool { return pinnedCount(log) == 1 })
	if pool.Free() != 1 {
		t.Errorf("%d cores free with the session logged in, want 1: the writer's", pool.Free())
	}
	if _, ok := log.Pinned()[fmt.Sprintf("session %d writer", id)]; !ok {
		t.Errorf("pinned %v, want the writer", log.Pinned())
	}
	c.send(Ping{ClientSeq: 1})
	c.expect(Pong{ClientSeq: 1})
}

// The three tests below run the tests of the mode without cores, on cores:
// idle and heartbeat times, the limit of commands taken and not answered,
// and the end of sessions.

func TestSpinHeartbeatAndIdle(t *testing.T) {
	pool, log := spintest.New(t, 2)
	heartbeatAndIdle(t, pool)
	if n := pinnedCount(log); n == 0 {
		t.Error("no goroutine pinned itself")
	}
}

// A spinning reader waits for room without blocking, and a place given back
// lets it go on.
func TestSpinAtMostMaxTakenCommandsAreOutstanding(t *testing.T) {
	pool, log := spintest.New(t, 2)
	atMostMaxTaken(t, pool)
	if n := pinnedCount(log); n != 2 {
		t.Errorf("%d goroutines pinned themselves, want the reader and the writer", n)
	}
}

func TestSpinCloseEndsSessions(t *testing.T) {
	pool, log := spintest.Some(t, 3)
	closeEndsSessions(t, pool)
	if n := pinnedCount(log); n == 0 {
		t.Error("no goroutine pinned itself")
	}
	if n, all := pool.Free(), len(pool.CPUs()); n != all {
		t.Errorf("%d cores free after Close, want all %d", n, all)
	}
}

// TestSpinPathAllocatesNothing: with both loops of a session spinning, a
// Ping and its Pong over a real socket allocate nothing anywhere in the
// process, but for a few allocations that other goroutines make now and then.
func TestSpinPathAllocatesNothing(t *testing.T) {
	pool, log := spintest.New(t, 2)
	_, addr := startServer(t, ServerOptions{Spin: pool, Heartbeat: time.Hour})
	c := dial(t, addr)
	c.login()
	waitFor(t, "the reader and the writer to pin themselves", func() bool { return pinnedCount(log) == 2 })
	out, in := make([]byte, 0, 64), make([]byte, 64)
	pong := len(Pong{}.Append(nil))
	var seq uint64
	allocs := spintest.AllocsPerRun(2000, func() {
		seq++
		out = Ping{ClientSeq: seq}.Append(out[:0])
		if _, err := c.c.Write(out); err != nil {
			t.Fatal(err)
		}
		for got := 0; got < pong; {
			n, err := c.c.Read(in[got:pong])
			if err != nil {
				t.Fatal(err)
			}
			got += n
		}
	})
	if allocs != 0 {
		t.Errorf("%v allocations per Ping and Pong, want 0", allocs)
	}
}
