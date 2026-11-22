package oe

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/engine"
)

// attached returns a connection with no socket and no goroutines that is
// the connection of a new session on srv: tests drive its reader side and
// read what it queues.
func attached(t testing.TB, srv *Server) *conn {
	t.Helper()
	ss, err := srv.newSession()
	if err != nil {
		t.Fatal(err)
	}
	c := &conn{srv: srv, nc: nullConn{}, wake: make(chan struct{}, 1), readerDone: make(chan struct{}), ss: ss}
	ss.cur = c
	return c
}

// waitFor polls cond until it holds or the test times out.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// fakeCall is a command the fake engine took.
type fakeCall struct {
	sink   engine.Sink
	tag    uint64
	cancel bool
	order  engine.NewOrder
	id     book.OrderID // the new order's, or the one the cancel named
}

// fakeEngine takes commands and answers them as the test says: at once, or,
// with hold set, when answer is called, as the engine's goroutine would
// answer a batch. A new order rests, and a cancel cancels.
type fakeEngine struct {
	mu      sync.Mutex
	calls   []fakeCall // every command taken, in order
	pending []fakeCall // taken and not answered
	nextID  uint64
	hold    bool
	refuse  func(c fakeCall) error // not nil: may refuse a command
}

func (f *fakeEngine) take(c fakeCall) (book.OrderID, error) {
	f.mu.Lock()
	if !c.cancel {
		f.nextID++
		c.id = book.OrderID(f.nextID)
	}
	if f.refuse != nil {
		if err := f.refuse(c); err != nil {
			f.mu.Unlock()
			return c.id, err
		}
	}
	f.calls = append(f.calls, c)
	hold := f.hold
	if hold {
		f.pending = append(f.pending, c)
	}
	f.mu.Unlock()
	if !hold {
		c.sink.Reply(c.tag, fakeReply(c))
		c.sink.Flush()
	}
	return c.id, nil
}

func (f *fakeEngine) SendOrder(s engine.Sink, tag uint64, o engine.NewOrder) (book.OrderID, error) {
	return f.take(fakeCall{sink: s, tag: tag, order: o})
}

func (f *fakeEngine) SendCancel(s engine.Sink, tag uint64, id book.OrderID) error {
	_, err := f.take(fakeCall{sink: s, tag: tag, cancel: true, id: id})
	return err
}

// answer answers the first n commands taken and not yet answered, then
// flushes each sink that got a reply.
func (f *fakeEngine) answer(n int) {
	f.mu.Lock()
	batch := append([]fakeCall(nil), f.pending[:n]...)
	f.pending = f.pending[n:]
	f.mu.Unlock()
	var sinks []engine.Sink
	for _, c := range batch {
		c.sink.Reply(c.tag, fakeReply(c))
		if !containsSink(sinks, c.sink) {
			sinks = append(sinks, c.sink)
		}
	}
	for _, s := range sinks {
		s.Flush()
	}
}

func containsSink(sinks []engine.Sink, s engine.Sink) bool {
	for _, x := range sinks {
		if x == s {
			return true
		}
	}
	return false
}

func (f *fakeEngine) taken() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeReply is the reply to c: two events and an open order, or one event
// and a canceled one.
func fakeReply(c fakeCall) engine.Reply {
	if c.cancel {
		return engine.Reply{
			Kind: book.CommandCancel, OrderID: c.id,
			Events: []book.Event{{Seq: uint64(c.id), Kind: book.EventCanceled, OrderID: c.id, Reason: book.ReasonUserCancel}},
			Status: engine.OrderStatus{Order: book.Order{ID: c.id}, State: engine.StateCanceled, Reason: book.ReasonUserCancel},
		}
	}
	return engine.Reply{
		Kind: book.CommandNewOrder, OrderID: c.id,
		Events: []book.Event{
			{Seq: 2 * uint64(c.id), Kind: book.EventAccepted, OrderID: c.id},
			{Seq: 2*uint64(c.id) + 1, Kind: book.EventRested, OrderID: c.id, Qty: 5},
		},
		Status: engine.OrderStatus{Order: book.Order{ID: c.id}, State: engine.StateOpen},
	}
}

// wantFrames is what a session sends for reply r to the command with
// client_seq tag, from server_seq next on.
func wantFrames(tag, next uint64, r engine.Reply) []frame {
	d, withEvents := replyDone(r)
	var fs []frame
	if withEvents {
		for _, e := range r.Events {
			fs = append(fs, Event{ServerSeq: next, ClientSeq: tag, Event: e})
			next++
		}
	}
	d.ServerSeq, d.ClientSeq = next, tag
	return append(fs, d)
}

func buyOrder(seq uint64) NewOrder {
	return NewOrder{ClientSeq: seq, Symbol: 1, Side: book.Buy, Type: book.Limit, TIF: book.GTC, Price: 100, Qty: 5}
}

func TestOrdersAndCancelsAreAnswered(t *testing.T) {
	fe := &fakeEngine{}
	_, addr := startServer(t, ServerOptions{Engine: fe})
	c := dial(t, addr)
	c.login()
	c.send(buyOrder(1))
	c.expect(wantFrames(1, 1, fakeReply(fakeCall{id: 1}))...)
	c.send(Cancel{ClientSeq: 2, OrderID: 1})
	c.expect(wantFrames(2, 4, fakeReply(fakeCall{cancel: true, id: 1}))...)

	fe.mu.Lock()
	defer fe.mu.Unlock()
	want := engine.NewOrder{Symbol: "ETH-USD", Side: book.Buy, Type: book.Limit, TIF: book.GTC, Price: 100, Qty: 5}
	if len(fe.calls) != 2 || fe.calls[0].order != want || fe.calls[0].tag != 1 || !fe.calls[1].cancel || fe.calls[1].id != 1 || fe.calls[1].tag != 2 {
		t.Errorf("the engine took %+v", fe.calls)
	}
}

// TestDoneMapping holds Done to the table of docs/API.md: each status, with
// what the REST answer would have been.
func TestDoneMapping(t *testing.T) {
	ev := func(k book.EventKind, r book.Reason) []book.Event { return []book.Event{{Kind: k, Reason: r}} }
	st := func(s engine.OrderState, r book.Reason) engine.OrderStatus {
		return engine.OrderStatus{Order: book.Order{ID: 7}, State: s, Reason: r}
	}
	logFailed := errors.Join(engine.ErrLogFailed, errors.New("disk"))
	for _, tc := range []struct {
		name   string
		reply  engine.Reply
		want   Done
		events bool
	}{
		{"new order rests", engine.Reply{Kind: book.CommandNewOrder, OrderID: 7, Events: ev(book.EventRested, 0), Status: st(engine.StateOpen, 0)},
			Done{Status: StatusOK, OrderID: 7, State: engine.StateOpen}, true},
		{"IOC remainder", engine.Reply{Kind: book.CommandNewOrder, OrderID: 7, Events: ev(book.EventCanceled, book.ReasonIOCRemainder), Status: st(engine.StateCanceled, book.ReasonIOCRemainder)},
			Done{Status: StatusOK, Reason: book.ReasonIOCRemainder, OrderID: 7, State: engine.StateCanceled}, true},
		{"FOK that cannot fill is 200 with a rejected order", engine.Reply{Kind: book.CommandNewOrder, OrderID: 7, Events: ev(book.EventRejected, book.ReasonFOKUnfillable), Status: st(engine.StateRejected, book.ReasonFOKUnfillable)},
			Done{Status: StatusOK, Reason: book.ReasonFOKUnfillable, OrderID: 7, State: engine.StateRejected}, true},
		{"cancel", engine.Reply{Kind: book.CommandCancel, OrderID: 7, Events: ev(book.EventCanceled, book.ReasonUserCancel), Status: st(engine.StateCanceled, book.ReasonUserCancel)},
			Done{Status: StatusOK, Reason: book.ReasonUserCancel, OrderID: 7, State: engine.StateCanceled}, true},
		{"cancel of a finished order is 409", engine.Reply{Kind: book.CommandCancel, OrderID: 7, Events: ev(book.EventRejected, book.ReasonUnknownOrder), Status: st(engine.StateFilled, 0)},
			Done{Status: StatusOrderFinished, OrderID: 7}, false},
		{"cancel of an order the engine does not know is 404", engine.Reply{Kind: book.CommandCancel, OrderID: 7, Events: ev(book.EventRejected, book.ReasonUnknownOrder)},
			Done{Status: StatusUnknownOrder, OrderID: 7}, false},
		{"log failed, applied", engine.Reply{Kind: book.CommandNewOrder, OrderID: 7, Events: ev(book.EventRested, 0), Status: st(engine.StateOpen, 0), Err: logFailed},
			Done{Status: StatusLogFailedApplied, OrderID: 7, State: engine.StateOpen}, true},
		{"log failed, applied, a cancel of an unknown order", engine.Reply{Kind: book.CommandCancel, OrderID: 7, Events: ev(book.EventRejected, book.ReasonUnknownOrder), Err: logFailed},
			Done{Status: StatusLogFailedApplied, OrderID: 7}, true},
		{"log failed, not applied", engine.Reply{Kind: book.CommandNewOrder, OrderID: 7, Err: logFailed},
			Done{Status: StatusLogFailed, OrderID: 7}, false},
		{"an error that is none of the engine's", engine.Reply{Kind: book.CommandNewOrder, OrderID: 7, Err: errors.New("x")},
			Done{Status: StatusInternal, OrderID: 7}, false},
		{"no events and no error", engine.Reply{Kind: book.CommandNewOrder, OrderID: 7},
			Done{Status: StatusInternal, OrderID: 7}, false},
	} {
		d, events := replyDone(tc.reply)
		if d != tc.want || events != tc.events {
			t.Errorf("%s: got %+v with events %v, want %+v with events %v", tc.name, d, events, tc.want, tc.events)
		}
	}

	for _, tc := range []struct {
		name string
		kind book.CommandKind
		id   book.OrderID
		err  error
		want Done
	}{
		{"invalid", book.CommandNewOrder, 7, &engine.InvalidError{Reason: book.ReasonInvalidQty}, Done{Status: StatusInvalid, Reason: book.ReasonInvalidQty, OrderID: 7}},
		{"a cancel of ID 0 is 400 invalid_order_id", book.CommandCancel, 0, &engine.InvalidError{Reason: book.ReasonInvalidOrderID}, Done{Status: StatusInvalid, Reason: book.ReasonInvalidOrderID}},
		{"a new order with ID 0 is a server bug", book.CommandNewOrder, 7, &engine.InvalidError{Reason: book.ReasonInvalidOrderID}, Done{Status: StatusInternal, OrderID: 7}},
		{"an invalid command is a server bug", book.CommandCancel, 7, &engine.InvalidError{Reason: book.ReasonInvalidCommand}, Done{Status: StatusInternal, OrderID: 7}},
		{"unknown symbol", book.CommandNewOrder, 0, engine.ErrUnknownSymbol, Done{Status: StatusUnknownSymbol}},
		{"unknown order", book.CommandCancel, 7, engine.ErrUnknownOrder, Done{Status: StatusUnknownOrder, OrderID: 7}},
		{"busy", book.CommandNewOrder, 7, engine.ErrBusy, Done{Status: StatusBusy, OrderID: 7}},
		{"closed", book.CommandNewOrder, 7, engine.ErrClosed, Done{Status: StatusShuttingDown, OrderID: 7}},
		{"canceled", book.CommandNewOrder, 7, context.Canceled, Done{Status: StatusShuttingDown, OrderID: 7}},
		{"the ID reservation failed", book.CommandNewOrder, 0, errors.New("engine: reserving order IDs: disk full"), Done{Status: StatusInternal}},
		{"a client_id too long", book.CommandNewOrder, 0, engine.ErrClientIDLen, Done{Status: StatusInternal}},
	} {
		if d := refusalDone(tc.kind, tc.id, tc.err); d != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.name, d, tc.want)
		}
	}
}

func TestResendsAndGapsOfCommands(t *testing.T) {
	fe := &fakeEngine{}
	_, addr := startServer(t, ServerOptions{Engine: fe})
	c := dial(t, addr)
	c.login()
	// Pings and commands share one client_seq. A number not above the last
	// one taken is dropped, whatever the frame; one that skips a number
	// closes the connection.
	c.send(buyOrder(1), Ping{ClientSeq: 2}, buyOrder(2), Cancel{ClientSeq: 1, OrderID: 1}, buyOrder(3), Ping{ClientSeq: 3})
	c.expect(wantFrames(1, 1, fakeReply(fakeCall{id: 1}))...)
	c.expect(Pong{ClientSeq: 2})
	c.expect(wantFrames(3, 4, fakeReply(fakeCall{id: 2}))...)
	c.send(buyOrder(5))
	c.expectClosed()
	if n := fe.taken(); n != 2 {
		t.Errorf("the engine took %d commands, want 2", n)
	}
}

// TestAtMostMaxTakenCommandsAreOutstanding: a session whose commands are
// not answered stops being read at maxTaken, and one answer lets one more
// in.
func TestAtMostMaxTakenCommandsAreOutstanding(t *testing.T) {
	fe := &fakeEngine{hold: true}
	_, addr := startServer(t, ServerOptions{Engine: fe})
	c := dial(t, addr)
	c.login()
	const sent = maxTaken + 76
	var b []byte
	for seq := uint64(1); seq <= sent; seq++ {
		b = buyOrder(seq).Append(b)
	}
	if _, err := c.c.Write(b); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first maxTaken commands", func() bool { return fe.taken() == maxTaken })
	time.Sleep(50 * time.Millisecond)
	if n := fe.taken(); n != maxTaken {
		t.Fatalf("the engine took %d commands with none answered, want %d", n, maxTaken)
	}
	fe.answer(1)
	waitFor(t, "one more command", func() bool { return fe.taken() == maxTaken+1 })
	time.Sleep(50 * time.Millisecond)
	if n := fe.taken(); n != maxTaken+1 {
		t.Fatalf("the engine took %d commands after one answer, want %d", n, maxTaken+1)
	}
	for {
		fe.mu.Lock()
		n, total := len(fe.pending), len(fe.calls)
		fe.mu.Unlock()
		if n == 0 && total == sent {
			break
		}
		fe.answer(n)
		time.Sleep(time.Millisecond)
	}
	// Every command is answered, in the order the engine answered them.
	next := uint64(1)
	for seq := uint64(1); seq <= sent; seq++ {
		c.expect(wantFrames(seq, next, fakeReply(fakeCall{id: book.OrderID(seq)}))...)
		next += 3
	}
}

// TestCommandsRefusedAtOnce: a command the engine refuses, or the session
// cannot route, is answered at once, takes no place, and its answer can
// come before the answers to commands sent earlier.
func TestCommandsRefusedAtOnce(t *testing.T) {
	fe := &fakeEngine{hold: true}
	_, addr := startServer(t, ServerOptions{Engine: fe})
	c := dial(t, addr)
	c.login()
	bad := buyOrder(2)
	bad.Symbol = 9
	c.send(buyOrder(1), bad)
	c.expect(Done{ServerSeq: 1, ClientSeq: 2, Status: StatusUnknownSymbol})
	fe.answer(1)
	c.expect(wantFrames(1, 2, fakeReply(fakeCall{id: 1}))...)

	// More refused commands than there are places: none of them keeps one.
	fe.mu.Lock()
	fe.refuse = func(fakeCall) error { return engine.ErrBusy }
	fe.mu.Unlock()
	var b []byte
	for seq := uint64(3); seq < 3+2*maxTaken; seq++ {
		b = buyOrder(seq).Append(b)
	}
	if _, err := c.c.Write(b); err != nil {
		t.Fatal(err)
	}
	for seq := uint64(3); seq < 3+2*maxTaken; seq++ {
		c.expect(Done{ServerSeq: seq + 2, ClientSeq: seq, Status: StatusBusy, OrderID: book.OrderID(seq - 1)})
	}
}

// expectQuiet checks that the server sends nothing and keeps the
// connection open for a while.
func (c *client) expectQuiet() {
	c.t.Helper()
	_ = c.c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	var b [64]byte
	n, err := c.c.Read(b[:])
	var ne net.Error
	if n > 0 || !errors.As(err, &ne) || !ne.Timeout() {
		c.t.Fatalf("read %d bytes, %v; want the connection open and quiet", n, err)
	}
}

// countConn counts the writes made to a connection.
type countConn struct {
	net.Conn
	writes *atomic.Int64
}

func (c countConn) Write(b []byte) (int, error) {
	c.writes.Add(1)
	return c.Conn.Write(b)
}

type countListener struct {
	net.Listener
	writes *atomic.Int64
}

func (l countListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return countConn{c, l.writes}, nil
}

// TestAPassIsWrittenOnce: the replies the engine gives in one batch go out
// with one write.
func TestAPassIsWrittenOnce(t *testing.T) {
	fe := &fakeEngine{hold: true}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var writes atomic.Int64
	s := NewServer(ServerOptions{Engine: fe, Symbols: testSymbols, Heartbeat: time.Hour})
	go func() { _ = s.Serve(countListener{ln, &writes}) }()
	t.Cleanup(func() { _ = s.Close() })

	c := dial(t, ln.Addr().String())
	c.login()
	const n = 50
	var b []byte
	for seq := uint64(1); seq <= n; seq++ {
		b = buyOrder(seq).Append(b)
	}
	if _, err := c.c.Write(b); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the commands", func() bool { return fe.taken() == n })
	before := writes.Load()
	fe.answer(n)
	next := uint64(1)
	for seq := uint64(1); seq <= n; seq++ {
		c.expect(wantFrames(seq, next, fakeReply(fakeCall{id: book.OrderID(seq)}))...)
		next += 3
	}
	if got := writes.Load() - before; got != 1 {
		t.Errorf("%d writes for the replies of one batch, want 1", got)
	}
}

// TestRepliesDoNotWaitForAClientThatDoesNotRead: the engine's goroutine
// hands replies over without waiting, and a session that passes its limit
// of unsent output is closed.
func TestRepliesDoNotWaitForAClientThatDoesNotRead(t *testing.T) {
	fe := &fakeEngine{hold: true}
	ln := servePipes(t, ServerOptions{Engine: fe, MaxUnsent: 1000})
	c := ln.dial()
	defer func() { _ = c.Close() }()
	// The login is a write that waits for the client to read, so replies
	// pile up behind it.
	var b []byte
	b = Login{Version: Version}.Append(b)
	for seq := uint64(1); seq <= 20; seq++ {
		b = buyOrder(seq).Append(b)
	}
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write(b); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the commands", func() bool { return fe.taken() == 20 })
	answered := make(chan struct{})
	go func() {
		fe.answer(20)
		close(answered)
	}()
	select {
	case <-answered:
	case <-time.After(5 * time.Second):
		t.Fatal("the engine's goroutine waited for a client that does not read")
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var err error
	for err == nil {
		_, err = c.Read(make([]byte, 4096))
	}
	if !errors.Is(err, io.EOF) {
		t.Errorf("the session ended with %v on the client's side, want EOF", err)
	}
}

// TestLogoutWaitsForAnswers: after Logout the session sends the answers to
// the commands it took, then closes, without EndOfSession.
func TestLogoutWaitsForAnswers(t *testing.T) {
	fe := &fakeEngine{hold: true}
	_, addr := startServer(t, ServerOptions{Engine: fe})
	c := dial(t, addr)
	c.login()
	c.send(buyOrder(1), buyOrder(2), Logout{}, buyOrder(3))
	waitFor(t, "the commands", func() bool { return fe.taken() == 2 })
	c.expectQuiet()
	fe.answer(2)
	c.expect(wantFrames(1, 1, fakeReply(fakeCall{id: 1}))...)
	c.expect(wantFrames(2, 4, fakeReply(fakeCall{id: 2}))...)
	c.expectClosed()
	if n := fe.taken(); n != 2 {
		t.Errorf("the engine took %d commands, want 2: the one after Logout is not read", n)
	}
}

// TestShutdownWaitsForAnswers: a server that is shutting down answers the
// commands it took, answers new ones with shutting_down, an order for an
// unknown symbol too, as REST answers it 503, then sends EndOfSession and
// closes.
func TestShutdownWaitsForAnswers(t *testing.T) {
	fe := &fakeEngine{hold: true}
	s, addr := startServer(t, ServerOptions{Engine: fe})
	c := dial(t, addr)
	c.login()
	c.send(buyOrder(1), buyOrder(2))
	waitFor(t, "the commands", func() bool { return fe.taken() == 2 })
	s.BeginShutdown()
	unrouted := buyOrder(5)
	unrouted.Symbol = 9
	c.send(buyOrder(3), Cancel{ClientSeq: 4, OrderID: 1}, unrouted)
	c.expect(Done{ServerSeq: 1, ClientSeq: 3, Status: StatusShuttingDown})
	c.expect(Done{ServerSeq: 2, ClientSeq: 4, Status: StatusShuttingDown, OrderID: 1})
	c.expect(Done{ServerSeq: 3, ClientSeq: 5, Status: StatusShuttingDown})
	c.expectQuiet()
	fe.answer(2)
	c.expect(wantFrames(1, 4, fakeReply(fakeCall{id: 1}))...)
	c.expect(wantFrames(2, 7, fakeReply(fakeCall{id: 2}))...)
	c.expect(EndOfSession{})
	c.expectClosed()
	if n := fe.taken(); n != 2 {
		t.Errorf("the engine took %d commands, want 2", n)
	}
}

// echoEngine answers every command at once from the reader's goroutine,
// without allocating.
type echoEngine struct {
	id     uint64
	events [2]book.Event
}

func (e *echoEngine) SendOrder(s engine.Sink, tag uint64, o engine.NewOrder) (book.OrderID, error) {
	e.id++
	id := book.OrderID(e.id)
	s.Reply(tag, engine.Reply{Kind: book.CommandNewOrder, OrderID: id, Events: e.events[:], Status: engine.OrderStatus{State: engine.StateOpen}})
	s.Flush()
	return id, nil
}

func (e *echoEngine) SendCancel(s engine.Sink, tag uint64, id book.OrderID) error {
	s.Reply(tag, engine.Reply{Kind: book.CommandCancel, OrderID: id, Events: e.events[:1], Status: engine.OrderStatus{State: engine.StateCanceled}})
	s.Flush()
	return nil
}

// TestCommandsAllocateNothing: taking a NewOrder or a Cancel, passing it to
// the engine and queueing its reply allocates nothing.
func TestCommandsAllocateNothing(t *testing.T) {
	srv := NewServer(ServerOptions{Engine: &echoEngine{}, Symbols: testSymbols})
	c := attached(t, srv)
	c.out = make([]byte, 0, 1024)
	buf := make([]byte, 0, 64)
	for _, cancel := range []bool{false, true} {
		allocs := testing.AllocsPerRun(1000, func() {
			c.out = c.out[:0]
			seq := c.ss.lastClientSeq + 1
			if cancel {
				buf = Cancel{ClientSeq: seq, OrderID: 1}.Append(buf[:0])
			} else {
				buf = buf[:0]
				buf = NewOrder{ClientSeq: seq, Side: book.Buy, Type: book.Limit, TIF: book.GTC, Price: 100, Qty: 1}.Append(buf)
			}
			typ, body, _, _ := Next(buf)
			if err := c.handle(typ, body); err != nil {
				t.Fatal(err)
			}
		})
		if allocs != 0 {
			t.Errorf("%v allocations to take a command (cancel %v) and queue its reply, want 0", allocs, cancel)
		}
	}
	if c.ss.taken != 0 || c.ss.lastClientSeq < 2000 {
		t.Fatalf("%d places in use, %d commands taken", c.ss.taken, c.ss.lastClientSeq)
	}
}
