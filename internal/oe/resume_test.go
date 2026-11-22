package oe

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/engine"
)

// answers is what a session sends for the n-th order of a fake engine that
// answers at once, which has ID n, starting at server_seq next.
func answers(n, next uint64) []frame {
	return wantFrames(n, next, fakeReply(fakeCall{id: book.OrderID(n)}))
}

// ordersAnswered returns the frames for orders 1 to n, three each.
func ordersAnswered(n uint64) []frame {
	var fs []frame
	for i := uint64(1); i <= n; i++ {
		fs = append(fs, answers(i, 3*i-2)...)
	}
	return fs
}

func (c *client) resume(session, next uint64) {
	c.send(Login{Version: Version, Session: session, NextServerSeq: next})
}

func TestResumeReplaysWhatTheClientMissed(t *testing.T) {
	fe := &fakeEngine{}
	_, addr := startServer(t, ServerOptions{Engine: fe})
	a := dial(t, addr)
	id := a.login()
	a.send(buyOrder(1), buyOrder(2), buyOrder(3))
	all := ordersAnswered(3)
	a.expect(all[:4]...) // the client has read four frames when the connection breaks
	_ = a.c.Close()

	b := dial(t, addr)
	b.resume(id, 5)
	b.expect(LoginAccepted{Session: id, NextServerSeq: 5, LastClientSeq: 3})
	b.expect(all[4:]...)
	// The client sends again what it sent last: 3 was taken, 4 was not.
	b.send(buyOrder(3), buyOrder(4))
	b.expect(answers(4, 10)...)
	if n := fe.taken(); n != 4 {
		t.Errorf("the engine took %d commands, want 4: each once", n)
	}
}

func TestResumeGetsRepliesProducedWhileDetached(t *testing.T) {
	fe := &fakeEngine{hold: true}
	_, addr := startServer(t, ServerOptions{Engine: fe})
	a := dial(t, addr)
	id := a.login()
	a.send(buyOrder(1), buyOrder(2))
	waitFor(t, "the commands", func() bool { return fe.taken() == 2 })
	_ = a.c.Close()
	fe.answer(2) // there may be no connection to take the replies

	b := dial(t, addr)
	b.resume(id, 1)
	b.expect(LoginAccepted{Session: id, NextServerSeq: 1, LastClientSeq: 2})
	b.expect(ordersAnswered(2)...)
}

func TestReplayLost(t *testing.T) {
	// 200 bytes hold the last two Events and one Done of an answer: frames
	// 7 to 9 of three orders.
	_, addr := startServer(t, ServerOptions{Engine: &fakeEngine{}, Held: 200})
	a := dial(t, addr)
	id := a.login()
	a.send(buyOrder(1), buyOrder(2), buyOrder(3))
	a.expect(ordersAnswered(3)...)
	_ = a.c.Close()

	b := dial(t, addr)
	b.resume(id, 7)
	b.expect(LoginAccepted{Session: id, NextServerSeq: 7, LastClientSeq: 3})
	b.expect(ordersAnswered(3)[6:]...)
	_ = b.c.Close()

	c := dial(t, addr)
	c.resume(id, 6)
	c.expect(LoginRejected{Reason: RejectReplayLost})
	c.expectClosed()
}

func TestResumeRefused(t *testing.T) {
	_, addr := startServer(t, ServerOptions{Engine: &fakeEngine{}})
	a := dial(t, addr)
	id := a.login()
	a.send(buyOrder(1))
	a.expect(ordersAnswered(1)...)
	_ = a.c.Close()

	for _, tc := range []struct {
		name          string
		session, next uint64
		want          RejectReason
	}{
		{"no such session", id + 1, 1, RejectUnknownSession},
		{"server_seq 0", id, 0, RejectUnknownSession},
		{"a server_seq the session has not sent", id, 5, RejectUnknownSession},
	} {
		c := dial(t, addr)
		c.resume(tc.session, tc.next)
		c.expect(LoginRejected{Reason: tc.want})
		c.expectClosed()
		if t.Failed() {
			t.Fatal(tc.name)
		}
	}
	// A refused Login leaves the session as it was. With nothing missed
	// there is nothing to replay.
	b := dial(t, addr)
	b.resume(id, 4)
	b.expect(LoginAccepted{Session: id, NextServerSeq: 4, LastClientSeq: 1})
	b.send(buyOrder(2))
	b.expect(answers(2, 4)...)
}

func TestResumeTakesOverTheAttachedConnection(t *testing.T) {
	fe := &fakeEngine{}
	_, addr := startServer(t, ServerOptions{Engine: fe})
	a := dial(t, addr)
	id := a.login()
	a.send(buyOrder(1))
	a.expect(ordersAnswered(1)...)

	b := dial(t, addr) // a is still connected: the server has not noticed anything
	b.resume(id, 4)
	b.expect(LoginAccepted{Session: id, NextServerSeq: 4, LastClientSeq: 1})
	a.expectClosed()
	b.send(buyOrder(2))
	b.expect(answers(2, 4)...)
}

func TestRefusedLoginLeavesTheAttachedConnection(t *testing.T) {
	// 200 bytes hold the last three frames, as in TestReplayLost.
	_, addr := startServer(t, ServerOptions{Engine: &fakeEngine{}, Held: 200})
	a := dial(t, addr)
	id := a.login()
	a.send(buyOrder(1), buyOrder(2), buyOrder(3))
	a.expect(ordersAnswered(3)...)

	orders := uint64(3) // answered so far, with three frames each
	for _, tc := range []struct {
		name string
		next func(sent uint64) uint64
		want RejectReason
	}{
		{"server_seq 0", func(uint64) uint64 { return 0 }, RejectUnknownSession},
		{"a server_seq the session has not sent", func(sent uint64) uint64 { return sent + 2 }, RejectUnknownSession},
		{"a server_seq no longer held", func(sent uint64) uint64 { return sent - 3 }, RejectReplayLost},
	} {
		b := dial(t, addr)
		b.resume(id, tc.next(3*orders))
		b.expect(LoginRejected{Reason: tc.want})
		b.expectClosed()
		// a is still the session's connection: its next order is answered.
		orders++
		a.send(buyOrder(orders))
		a.expect(answers(orders, 3*orders-2)...)
		if t.Failed() {
			t.Fatal(tc.name)
		}
	}
}

func TestSessionsEnd(t *testing.T) {
	var mu sync.Mutex
	var fire []func()
	stops := 0
	opts := ServerOptions{Engine: &fakeEngine{}, afterFunc: func(d time.Duration, f func()) func() bool {
		mu.Lock()
		defer mu.Unlock()
		fire = append(fire, f)
		return func() bool { mu.Lock(); stops++; mu.Unlock(); return true }
	}}
	s, addr := startServer(t, opts)
	timers := func() int { mu.Lock(); defer mu.Unlock(); return len(fire) }

	refused := func(session uint64) {
		t.Helper()
		c := dial(t, addr)
		c.resume(session, 1)
		c.expect(LoginRejected{Reason: RejectUnknownSession})
		c.expectClosed()
	}
	login := func() (*client, uint64) {
		c := dial(t, addr)
		return c, c.login()
	}

	// A connection that ends starts a timer, and a resume stops it.
	a, id := login()
	_ = a.c.Close()
	waitFor(t, "the timer", func() bool { return timers() == 1 })
	b := dial(t, addr)
	b.resume(id, 1)
	b.expect(LoginAccepted{Session: id, NextServerSeq: 1})
	mu.Lock()
	if stops != 1 {
		t.Errorf("%d timers stopped by the resume, want 1", stops)
	}
	mu.Unlock()
	// A timer that goes off after the resume does nothing.
	mu.Lock()
	late := fire[0]
	mu.Unlock()
	late()
	b.send(Ping{ClientSeq: 1})
	b.expect(Pong{ClientSeq: 1})

	// A session whose time is up cannot be resumed.
	_ = b.c.Close()
	waitFor(t, "the second timer", func() bool { return timers() == 2 })
	mu.Lock()
	expire := fire[1]
	mu.Unlock()
	expire()
	refused(id)

	// Logout ends the session.
	c, id2 := login()
	c.send(Logout{})
	c.expectClosed()
	refused(id2)

	// So does a restart.
	d, id3 := login()
	_ = d.c.Close()
	waitFor(t, "the third timer", func() bool { return timers() == 3 }) // the Logout's connection set none
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	_, addr2 := startServer(t, ServerOptions{Engine: &fakeEngine{}})
	e := dial(t, addr2)
	e.resume(id3, 1)
	e.expect(LoginRejected{Reason: RejectUnknownSession})
}

func TestEveryDisconnectKeepsTheSession(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(c *client)
	}{
		{"the client closes", func(c *client) { _ = c.c.Close() }},
		{"a gap in client_seq", func(c *client) { c.send(buyOrder(9)); c.expectClosed() }},
		{"a malformed frame", func(c *client) { c.send(rawFrame{0x01, 0x00, 'x'}); c.expectClosed() }},
		{"nothing arrives for the idle time", func(c *client) { c.expectClosed() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, addr := startServer(t, ServerOptions{Engine: &fakeEngine{}, Idle: 300 * time.Millisecond})
			a := dial(t, addr)
			id := a.login()
			a.send(buyOrder(1))
			a.expect(ordersAnswered(1)...)
			tc.break_(a)
			b := dial(t, addr)
			b.resume(id, 4)
			b.expect(LoginAccepted{Session: id, NextServerSeq: 4, LastClientSeq: 1})
		})
	}
}

// TestResumeAfterABrokenConnection sends orders to a real engine, breaks
// the connection with a reset while replies are on their way, resumes, and
// sends again what the server has not taken: every order is applied once
// and answered once.
func TestResumeAfterABrokenConnection(t *testing.T) {
	const n = 3000
	e, err := engine.New(engine.Config{Symbols: []string{"BTC-USD"}, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() }) // after the server closes
	_, addr := startServer(t, ServerOptions{Engine: e, Symbols: e.Symbols()})

	var session, lastSeen, lastClientSeq uint64 // lastSeen: the highest server_seq read
	dones := make(map[uint64]int)               // by client_seq
	// read reads frames until the connection ends, or until stop is true.
	read := func(c net.Conn, stop func() bool) {
		var buf []byte
		var tmp [4096]byte
		for !stop() {
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			k, err := c.Read(tmp[:])
			buf = append(buf, tmp[:k]...)
			for {
				typ, body, size, nerr := Next(buf)
				if nerr != nil || size == 0 {
					break
				}
				switch typ {
				case TypeLoginAccepted:
					acc := DecodeLoginAccepted(body)
					session, lastClientSeq = acc.Session, acc.LastClientSeq
				case TypeEvent:
					lastSeen = DecodeEvent(body).ServerSeq
				case TypeDone:
					d := DecodeDone(body)
					lastSeen = d.ServerSeq
					dones[d.ClientSeq]++
				default: // Symbols and Heartbeats
				}
				buf = buf[size:]
			}
			if err != nil {
				return
			}
		}
	}
	order := func(seq uint64) []byte {
		return NewOrder{ClientSeq: seq, Side: book.Buy, Type: book.Limit, TIF: book.GTC, Price: book.Price(100 + seq%50), Qty: 1}.Append(nil)
	}

	c1, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c1.Write(Login{Version: Version}.Append(nil)); err != nil {
		t.Fatal(err)
	}
	go func() { // writes until the connection breaks
		for seq := uint64(1); seq <= n; seq++ {
			if _, err := c1.Write(order(seq)); err != nil {
				return
			}
		}
	}()
	read(c1, func() bool { return len(dones) >= 300 })
	_ = c1.(*net.TCPConn).SetLinger(0)
	_ = c1.Close()

	c2, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c2.Close() })
	if _, err := c2.Write(Login{Version: Version, Session: session, NextServerSeq: lastSeen + 1}.Append(nil)); err != nil {
		t.Fatal(err)
	}
	gotAccepted := false
	read(c2, func() bool {
		if session != 0 && !gotAccepted && lastClientSeq != 0 {
			gotAccepted = true
			var b []byte
			for seq := lastClientSeq + 1; seq <= n; seq++ {
				b = append(b, order(seq)...)
			}
			if _, err := c2.Write(b); err != nil {
				t.Error(err)
			}
		}
		return len(dones) >= n
	})
	for seq := uint64(1); seq <= n; seq++ {
		if dones[seq] != 1 {
			t.Fatalf("order %d was answered %d times, want 1", seq, dones[seq])
		}
	}
	d, err := e.Depth(context.Background(), "BTC-USD", 1000)
	if err != nil {
		t.Fatal(err)
	}
	resting := 0
	for _, l := range d.Bids {
		resting += int(l.Orders)
	}
	if resting != n {
		t.Errorf("%d orders rest on the book, want %d: each applied once", resting, n)
	}
}

// takenOverUnread serves opts over pipes, which hold no bytes in flight,
// and returns a session at the point where a resume from the oldest frame
// held has taken it over and the new client, b, has read nothing: answered
// orders have been answered and read on the first connection, and inFlight
// more are with the engine, fe, which answers them when the test says. cur
// is b's connection, and first and next are the oldest server_seq held and
// the next the session sends.
func takenOverUnread(t *testing.T, opts ServerOptions, answered, inFlight uint64) (fe *fakeEngine, ss *session, b *client, cur *conn, first, next uint64) {
	t.Helper()
	fe = &fakeEngine{}
	opts.Engine, opts.Symbols = fe, testSymbols
	opts.Heartbeat = time.Hour // no Heartbeat among the frames and bytes the tests count
	s, ln := NewServer(opts), newPipeListener()
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(func() { _ = s.Close() })
	a := &client{t: t, c: ln.dial()}
	t.Cleanup(func() { _ = a.c.Close() }) // before the server closes: nothing reads its last frames
	id := a.login()
	s.mu.Lock()
	ss = s.sessions[id]
	s.mu.Unlock()

	for seq := uint64(1); seq <= answered; seq++ { // one at a time: a has little unread
		a.send(buyOrder(seq))
		a.expectSeqs(3*seq-2, 3*seq)
	}
	fe.mu.Lock()
	fe.hold = true
	fe.mu.Unlock()
	for seq := answered + 1; seq <= answered+inFlight; seq++ {
		a.send(buyOrder(seq))
	}
	waitFor(t, "the orders in flight", func() bool { return fe.taken() == int(answered+inFlight) })
	ss.mu.Lock()
	old := ss.cur
	first, next = ss.heldFirstLocked(), ss.nextServerSeq
	ss.mu.Unlock()
	if first == 1 {
		t.Fatalf("the session still holds its first frame after %d orders: the test needs it full", answered)
	}

	b = &client{t: t, c: ln.dial()}
	t.Cleanup(func() { _ = b.c.Close() })
	b.resume(id, first)
	waitFor(t, "the takeover", func() bool {
		ss.mu.Lock()
		defer ss.mu.Unlock()
		cur = ss.cur
		return cur != old
	})
	return fe, ss, b, cur, first, next
}

// expectSeqs reads the Events and Dones with server_seq from to to.
func (c *client) expectSeqs(from, to uint64) {
	c.t.Helper()
	for seq := from; seq <= to; seq++ {
		f, err := c.next()
		var got uint64
		switch f := f.(type) {
		case Event:
			got = f.ServerSeq
		case Done:
			got = f.ServerSeq
		}
		if err != nil || got != seq {
			c.t.Fatalf("got %+v, %v; want server_seq %d of %d to %d", f, err, seq, from, to)
		}
	}
}

// TestAFullReplayIsNotASlowConsumer resumes a session from the oldest frame
// it holds, with the sizes docs/API.md gives, and has the reply to an order
// in flight arrive before the client has read any of the replay. The client
// gets the whole replay and then the reply.
func TestAFullReplayIsNotASlowConsumer(t *testing.T) {
	// An answer is 196 bytes: 5,400 of them are more than the 1 MB held.
	const orders = 5400
	fe, ss, b, cur, first, next := takenOverUnread(t, ServerOptions{}, orders, 1)
	fe.answer(1)
	ss.mu.Lock()
	why := cur.abortErr
	ss.mu.Unlock()
	if why != nil {
		t.Fatalf("the server closed the resumed connection: %v", why)
	}
	b.expect(LoginAccepted{Session: ss.id, NextServerSeq: first, LastClientSeq: orders + 1})
	b.expectSeqs(first, next+2)
}

// TestReplayLeavesTheLimitOnOtherOutput queues, on a resumed connection
// that reads none of them, the answers that fit in MaxUnsent beyond its
// replay, which leave it open, and one more, which closes it: the same
// while the replay is unwritten and once it is written.
func TestReplayLeavesTheLimitOnOtherOutput(t *testing.T) {
	// 1000 bytes hold five answers of 196 bytes, and five more fit in
	// MaxUnsent; the sixth passes it.
	const answer, fit = 196, 5
	for _, read := range []bool{false, true} {
		name := "the replay unwritten"
		if read {
			name = "the replay written"
		}
		t.Run(name, func(t *testing.T) {
			fe, ss, b, cur, first, next := takenOverUnread(t, ServerOptions{Held: 1000, MaxUnsent: 1000}, 6, fit+1)
			ss.mu.Lock()
			replay := cur.replay
			ss.mu.Unlock()
			if want := len(LoginAccepted{}.Append(nil)) + fit*answer; replay != want {
				t.Fatalf("a replay of %d bytes, want %d", replay, want)
			}
			unwritten := replay
			if read {
				b.expect(LoginAccepted{Session: ss.id, NextServerSeq: first, LastClientSeq: 6 + fit + 1})
				b.expectSeqs(first, next-1)
				waitFor(t, "the write of the replay to return", func() bool {
					ss.mu.Lock()
					defer ss.mu.Unlock()
					return cur.replay == 0
				})
				unwritten = 0
			}

			fe.answer(fit) // b reads none of it
			ss.mu.Lock()
			st, unsent := cur.state, len(cur.out)+cur.inFlight
			ss.mu.Unlock()
			if st != running || unsent != unwritten+fit*answer {
				t.Fatalf("after %d answers: state %d with %d bytes unsent, want it running with %d", fit, st, unsent, unwritten+fit*answer)
			}
			fe.answer(1)
			ss.mu.Lock()
			st, why := cur.state, cur.abortErr
			ss.mu.Unlock()
			if st != aborted || !errors.Is(why, errSlowConsumer) {
				t.Fatalf("after one answer more: state %d, %v; want the connection closed as a slow consumer", st, why)
			}
		})
	}
}
