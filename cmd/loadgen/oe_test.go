package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/engine"
	"github.com/sHyanorn/matching-engine/internal/flow"
	"github.com/sHyanorn/matching-engine/internal/oe"
)

// fakeOE serves one OE1 session: it accepts the Login, answers each Ping
// with its Pong, and each NewOrder and Cancel with a Done, delay after the
// frame arrived, and closes on the Logout. After the frame numbered stallAt
// it reads nothing for stall, the frame numbered drop it never answers, and
// after the frame numbered garbleAt it sends a malformed frame. With no
// delay it writes all the answers that are ready with one write, as the
// engine's server does.
//
// It names two symbols, fakeSymbol second. The nth NewOrder gets the ID
// fakeOrderID(n), and, as the engine does, the Events accepted and rested
// before its Done, and every fourth the Events accepted and trade, unless it
// is an order of -ioc or -ack-cancel: a buy at their prices meets no ask. An
// IOC order is answered with the Events accepted and canceled and a Done in
// state canceled, any other order with a Done in state open, and a Cancel
// with a Done in state canceled. The command
// numbered busyAt is answered busy, the one numbered twiceAt twice, and the
// Done of the one numbered gapAt skips a server_seq. reply, if set, may
// change the answer to each NewOrder and Cancel, given the command.
type fakeOE struct {
	delay       time.Duration
	cancelDelay time.Duration // if not 0, the delay of a Cancel's Done
	stallAt     uint64
	stall       time.Duration
	drop        uint64
	garbleAt    uint64
	heartbeats  *atomic.Int32 // counts the client's Heartbeats, if set

	busyAt, twiceAt, gapAt uint64
	commands               chan<- any                   // gets each NewOrder and Cancel that arrives, if set
	trade                  func(book.Event)             // gets each trade Event, when its order arrives, if set
	sessions               *atomic.Int32                // counts the sessions open, if set
	reply                  func(cmd any, a *fakeAnswer) // changes an answer before it is queued, if set
}

const fakeSymbol = "BTC-USD"

func fakeOrderID(n int) book.OrderID { return book.OrderID(n)<<8 | 1 }

var (
	fakeEvents     = []book.EventKind{book.EventAccepted, book.EventTrade}
	fakeIOCEvents  = []book.EventKind{book.EventAccepted, book.EventCanceled}
	fakeRestEvents = []book.EventKind{book.EventAccepted, book.EventRested} // what the engine sends for an order that rests
)

// fakeAnswer is a frame to answer: a Ping, or a command, the Done it gets
// and the kinds of its Events, of which the trade one is trade.
type fakeAnswer struct {
	seq    uint64
	at     time.Time
	delay  time.Duration
	ping   bool
	events []book.EventKind
	trade  book.Event
	done   oe.Done // but for its server_seq
}

func (f fakeOE) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	if f.sessions != nil { // counted down before the connection closes
		f.sessions.Add(1)
		defer f.sessions.Add(-1)
	}
	answers := make(chan fakeAnswer, 1<<20)
	written := make(chan struct{})
	go func() {
		defer close(written)
		var serverSeq uint64
		add := func(b []byte, a fakeAnswer) []byte {
			if a.ping {
				return oe.Pong{ClientSeq: a.seq}.Append(b)
			}
			for _, kind := range a.events {
				serverSeq++
				e := book.Event{Kind: kind}
				if kind == book.EventTrade {
					e = a.trade
				}
				b = oe.Event{ServerSeq: serverSeq, ClientSeq: a.seq, Event: e}.Append(b)
			}
			d := a.done
			if a.seq == f.busyAt {
				d.Status = oe.StatusBusy
			}
			if a.seq == f.gapAt {
				serverSeq++
			}
			for range 1 + b2i(a.seq == f.twiceAt) {
				serverSeq++
				d.ServerSeq = serverSeq
				b = d.Append(b)
			}
			return b
		}
		var b []byte
		for a := range answers {
			time.Sleep(time.Until(a.at.Add(a.delay)))
			b = add(b[:0], a)
		more:
			for f.delay == 0 && f.cancelDelay == 0 { // with a delay, each answer waits for its own time
				select {
				case a, ok := <-answers:
					if !ok {
						break more
					}
					b = add(b, a)
				default:
					break more
				}
			}
			if _, err := c.Write(b); err != nil {
				return
			}
		}
	}()
	defer func() {
		close(answers)
		<-written
	}()
	var buf []byte
	tmp := make([]byte, 4096)
	orders, bookSeq := 0, uint64(0)
	for {
		n, err := c.Read(tmp)
		at := time.Now()
		buf = append(buf, tmp[:n]...)
		for {
			t, body, size, ferr := oe.Next(buf)
			if ferr != nil || (size == 0 && err != nil) {
				return
			}
			if size == 0 {
				break
			}
			buf = buf[size:]
			a := fakeAnswer{at: at, delay: f.delay}
			var cmd any
			switch t {
			case oe.TypeLogin:
				b := oe.LoginAccepted{Session: 1, NextServerSeq: 1}.Append(nil)
				b = oe.Symbol{Index: 0, Name: "ETH-USD"}.Append(b)
				if _, err := c.Write(oe.Symbol{Index: 1, Name: fakeSymbol}.Append(b)); err != nil {
					return
				}
				continue
			case oe.TypePing:
				a.seq, a.ping = oe.DecodePing(body).ClientSeq, true
			case oe.TypeNewOrder:
				m := oe.DecodeNewOrder(body)
				orders++
				a.seq, a.events = m.ClientSeq, fakeRestEvents
				a.done = oe.Done{ClientSeq: m.ClientSeq, Status: oe.StatusOK, OrderID: fakeOrderID(orders), State: engine.StateOpen}
				switch {
				case m.TIF == book.IOC:
					a.events, a.done.State, a.done.Reason = fakeIOCEvents, engine.StateCanceled, book.ReasonIOCRemainder
				case orders%4 == 0 && m.Price < ackPrice:
					bookSeq++
					a.events = fakeEvents
					a.trade = book.Event{Seq: bookSeq, Kind: book.EventTrade, OrderID: a.done.OrderID, MakerID: fakeOrderID(1000),
						Side: m.Side, Price: m.Price, Qty: m.Qty}
					if f.trade != nil {
						f.trade(a.trade)
					}
				}
				cmd = m
			case oe.TypeCancel:
				m := oe.DecodeCancel(body)
				a.seq = m.ClientSeq
				a.done = oe.Done{ClientSeq: m.ClientSeq, Status: oe.StatusOK, OrderID: m.OrderID, State: engine.StateCanceled, Reason: book.ReasonUserCancel}
				if f.cancelDelay != 0 {
					a.delay = f.cancelDelay
				}
				cmd = m
			case oe.TypeHeartbeat:
				if f.heartbeats != nil {
					f.heartbeats.Add(1)
				}
				continue
			case oe.TypeLogout:
				return
			default:
				continue
			}
			if cmd != nil {
				if f.commands != nil {
					f.commands <- cmd
				}
				if f.reply != nil {
					f.reply(cmd, &a)
				}
			}
			if a.seq != f.drop {
				answers <- a
			}
			if a.seq == f.stallAt {
				time.Sleep(f.stall)
			}
			if a.seq == f.garbleAt {
				if _, err := c.Write([]byte{0xff, 0xff, 'x'}); err != nil {
					return
				}
			}
		}
	}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// listenOE serves f on a loopback port and returns a dialer for it.
func listenOE(t *testing.T, f fakeOE) func() (net.Conn, error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return func() (net.Conn, error) { return net.Dial("tcp", ln.Addr().String()) }
}

func oeConfig(dial func() (net.Conn, error)) config {
	return config{rate: 2000, warmup: 100 * time.Millisecond, duration: 500 * time.Millisecond,
		spin: true, maxSendLag: time.Hour, oeDial: dial}
}

// measured returns the round trips of the requests due in the window.
func measured(r *oeRun) []int64 {
	var rtt []int64
	for j := range r.total {
		if d := r.due(j); d >= int64(r.cfg.warmup) && d < int64(r.cfg.warmup+r.cfg.duration) {
			rtt = append(rtt, r.rtt[j])
		}
	}
	return rtt
}

// A server that answers every Ping d after it arrived gives round trips
// of at least d, and a Ping sent late by s, at least s + d: the time
// counts from when the Ping was due, not from when it went out. Go's
// clock on Windows advances once per tick, too coarse for these bounds.
func TestOEDelayIsMeasured(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	if runtime.GOOS == "windows" {
		t.Skip("Go's clock on Windows advances once per tick (bench-results/m6-windows)")
	}
	const d, s = 2 * time.Millisecond, 3 * time.Millisecond
	for _, spin := range []bool{true, false} {
		cfg := oeConfig(listenOE(t, fakeOE{delay: d}))
		cfg.spin = spin
		// The hook runs before each write, with the first Ping it holds;
		// about every 100 Pings it holds the write back by s.
		var slowed []int
		cfg.beforeSend = func(i int) {
			if len(slowed) == 0 || i >= slowed[len(slowed)-1]+100 {
				time.Sleep(s)
				slowed = append(slowed, i)
			}
		}
		r, err := runOE(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		if r.err != nil {
			t.Fatalf("spin %v: %v", spin, r.err)
		}
		for i := range r.total {
			switch {
			case !r.got[i]:
				t.Fatalf("spin %v: no Pong for Ping %d", spin, i+1)
			case r.rtt[i] < int64(d):
				t.Errorf("spin %v: Ping %d round trip %v, below the server's delay %v", spin, i+1, time.Duration(r.rtt[i]), d)
			}
		}
		if len(slowed) < 10 {
			t.Fatalf("spin %v: %d Pings held back, want one in about every 100", spin, len(slowed))
		}
		for _, i := range slowed {
			if r.rtt[i] < int64(s+d) {
				t.Errorf("spin %v: Ping %d, sent %v late, round trip %v, below %v", spin, i+1, s, time.Duration(r.rtt[i]), s+d)
			}
		}
	}
}

// A server that stops reading for 200 ms at 10,000 requests a second
// delays the 2,000 requests due meanwhile, not only those already sent:
// the client cannot send them, and each counts from its due time. A
// net.Pipe holds no bytes in flight, so the client's write waits from the
// first request after the stall begins.
func TestOEStallIsChargedToEveryDelayedRequest(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	const stall = 200 * time.Millisecond
	cfg := oeConfig(func() (net.Conn, error) {
		c, s := net.Pipe()
		go fakeOE{stallAt: 1000, stall: stall}.serve(s)
		return c, nil
	})
	cfg.rate, cfg.warmup, cfg.duration = 10_000, 0, 400*time.Millisecond
	r, err := runOE(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if r.err != nil {
		t.Fatal(r.err)
	}
	// The stall begins once Ping 1000, due at 99.9 ms, has arrived, so
	// the 2,000 Pings due from 100 ms wait for it; those due in its last
	// 1 ms wait less than 1 ms.
	late := 0
	for _, v := range r.rtt[1000:3000] {
		if v > int64(time.Millisecond) {
			late++
		}
	}
	if late < 1800 {
		t.Errorf("%d of the 2,000 round trips due during the stall over 1 ms, want about 1,990", late)
	}
	if longest := slices.Max(measured(r)); longest < int64(stall-10*time.Millisecond) {
		t.Errorf("longest round trip %v, want about %v", time.Duration(longest), stall)
	}
}

// A pacer slowed on purpose makes the run invalid by its send lag.
func TestOESendLagBound(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	cfg := oeConfig(listenOE(t, fakeOE{}))
	cfg.maxSendLag = time.Millisecond
	cfg.beforeSend = func(i int) {
		if i%100 == 0 {
			time.Sleep(5 * time.Millisecond) // 10 requests fall due meanwhile
		}
	}
	r, err := measureOE(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if r.Valid || !slices.ContainsFunc(r.Problems, func(p string) bool { return strings.HasPrefix(p, "send lag p99.9") }) {
		t.Fatalf("valid %v, problems %q; want invalid by its send lag", r.Valid, r.Problems)
	}
}

// A Ping of the warmup that gets no Pong makes the run invalid, as a
// failed request of the warmup does in the REST mode (D030).
func TestOEMissingPongInvalidatesRun(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	r, err := measureOE(context.Background(), oeConfig(listenOE(t, fakeOE{drop: 1})))
	if err != nil {
		t.Fatal(err)
	}
	if r.Valid || !slices.Contains(r.Problems, "Pings with no Pong: 1") {
		t.Fatalf("valid %v, problems %q; want invalid by one missing Pong", r.Valid, r.Problems)
	}
}

// A reader that fails ends the run at once, the pacer included.
func TestOEReaderErrorStopsRun(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	cfg := oeConfig(listenOE(t, fakeOE{garbleAt: 100}))
	cfg.duration = 5 * time.Second
	start := time.Now()
	r, err := runOE(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if r.err == nil {
		t.Fatal("a malformed frame from the server went unreported")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("the run went on for %v after its reader failed, want it to stop", d)
	}
}

// A client that has nothing to send for a second sends a Heartbeat.
func TestOEHeartbeats(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	for _, spin := range []bool{true, false} {
		var beats atomic.Int32
		cfg := oeConfig(listenOE(t, fakeOE{heartbeats: &beats}))
		// Two Pings, 2.5 s apart.
		cfg.spin, cfg.rate, cfg.warmup, cfg.duration = spin, 0.4, 0, 5*time.Second
		if _, err := measureOE(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
		if n := beats.Load(); n != 2 {
			t.Errorf("spin %v: %d Heartbeats in 2.5 s without a Ping, want 2", spin, n)
		}
	}
}

// ordersConfig is oeConfig for the order flow, without a warmup, so that
// the report's outcomes cover the whole run.
func ordersConfig(dial func() (net.Conn, error)) config {
	cfg := oeConfig(dial)
	cfg.orders, cfg.symbol, cfg.seed, cfg.sd, cfg.warmup = true, fakeSymbol, 7, 5, 0
	return cfg
}

// runOrders runs cfg's flow against f and returns the run, its report and
// the commands f got.
func runOrders(t *testing.T, f fakeOE, cfg func(*config)) (*oeRun, report, []any) {
	t.Helper()
	got := make(chan any, 1<<16)
	f.commands = got
	c := ordersConfig(listenOE(t, f))
	if cfg != nil {
		cfg(&c)
	}
	r, err := runOE(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	// The run ended when f closed the session, after its last command.
	frames := make([]any, len(got))
	for i := range frames {
		frames[i] = <-got
	}
	return r, r.report(), frames
}

// sentFlow checks that frames are the first total ops of cfg's flow in
// order, numbered from 1 and naming fakeSymbol by its index, with only
// cancels left out. It returns how many are left out.
func sentFlow(t *testing.T, cfg config, total int, frames []any) (skipped int) {
	t.Helper()
	gen := flow.New(cfg.seed, cfg.sd)
	k := 0
	for i := range total {
		op := gen.Next()
		var want any = oe.Cancel{ClientSeq: uint64(k + 1), OrderID: fakeOrderID(op.Order)}
		if !op.Cancel {
			side := book.Sell
			if op.Buy {
				side = book.Buy
			}
			want = oe.NewOrder{ClientSeq: uint64(k + 1), Symbol: 1, Side: side, Type: book.Limit, TIF: book.GTC,
				Price: book.Price(op.Price), Qty: book.Qty(op.Qty)}
		}
		switch {
		case k < len(frames) && frames[k] == want:
			k++
		case op.Cancel:
			skipped++
		default:
			t.Fatalf("frame %d of %d is not op %d of the flow, %+v; the frames from there: %+v", k+1, len(frames), i+1, want, frames[k:min(k+3, len(frames))])
		}
	}
	if k != len(frames) {
		t.Fatalf("%d frames after the flow's %d ops, the first: %+v", len(frames)-k, total, frames[k])
	}
	return skipped
}

// rateAside fails the test for every problem of a run but a missed rate:
// whether the target rate went out depends on how busy the machine is, and
// any other problem is the tool's or the server's.
func rateAside(t *testing.T, r report) {
	t.Helper()
	for _, p := range r.Problems {
		if strings.HasPrefix(p, "achieved ") {
			t.Log(p)
		} else {
			t.Errorf("invalid run: %s", p)
		}
	}
}

// The commands that go out are the flow of the seed, as the REST mode
// sends it, and each Done is counted as the REST answer it stands for.
func TestOEOrdersSendTheFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	r, rep, frames := runOrders(t, fakeOE{}, nil)
	rateAside(t, rep)
	skipped := sentFlow(t, r.cfg, r.total, frames)
	orders := 0
	for _, f := range frames {
		if _, ok := f.(oe.NewOrder); ok {
			orders++
		}
	}
	// The fake answers every command ok, and every fourth order with a
	// trade after its accepted Event.
	want := map[string]int{"order_traded": orders / 4, "order_rested": orders - orders/4, "cancel_canceled": len(frames) - orders}
	if skipped > 0 {
		want["cancel_skipped"] = skipped
	}
	if !maps.Equal(rep.Outcomes, want) || rep.Trades.Replies != orders/4 {
		t.Errorf("outcomes %v and %d trades for %d orders and %d cancels sent, want %v and %d trades",
			rep.Outcomes, rep.Trades.Replies, orders, len(frames)-orders, want, orders/4)
	}
	for _, k := range []string{"reply", "service", "send_lag"} {
		if n := rep.Latency[k].Count; n != len(frames) {
			t.Errorf("%d %s samples, want one per command, %d", n, k, len(frames))
		}
	}
	if rep.Mode != "oe_orders" || orders < r.total/2 || len(frames) == orders {
		t.Errorf("mode %q, %d orders and %d cancels in %d requests", rep.Mode, orders, len(frames)-orders, r.total)
	}
}

// A cancel whose order has no Done yet when it is due is not sent, and
// takes no client_seq: with every Done 2 ms late, the cancels of the last
// orders are skipped, no command waits for a reply, and none is answered
// in under 2 ms.
func TestOEOrdersSkipACancelWhoseOrderIsUnanswered(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	if runtime.GOOS == "windows" {
		t.Skip("Go's clock on Windows advances once per tick (bench-results/m6-windows)")
	}
	const d = 2 * time.Millisecond
	r, rep, frames := runOrders(t, fakeOE{delay: d}, nil)
	rateAside(t, rep)
	skipped := sentFlow(t, r.cfg, r.total, frames)
	if skipped == 0 || rep.Outcomes["cancel_skipped"] != skipped {
		t.Errorf("%d cancels not sent, %d counted as cancel_skipped; want some, all counted", skipped, rep.Outcomes["cancel_skipped"])
	}
	if least := slices.Min(r.rtt[:r.nsent]); least < int64(d) {
		t.Errorf("a command answered in %v, want none under %v", time.Duration(least), d)
	}
	// Open loop: a request due every 500 µs went out then, with 2 ms to
	// wait for each reply.
	if p50 := rep.Latency["send_lag"].P50; p50 > 400 {
		t.Errorf("send lag p50 %.0f µs, want far below the 500 µs between requests", p50)
	}
}

// A run is invalid if a command got no Done or more than one, if a
// server_seq is missing, or if a command failed, in the warmup too.
func TestOEOrdersInvalidRuns(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	for _, c := range []struct {
		name string
		f    fakeOE
		want string
	}{
		{"a command with no Done", fakeOE{drop: 3}, "commands with no Done: 1"},
		{"a server_seq skipped", fakeOE{gapAt: 3}, "got server_seq "},
		{"a second Done for a command", fakeOE{twiceAt: 3}, "got Done with client_seq 3, want each of the "},
		{"a command of the warmup answered busy", fakeOE{busyAt: 3}, "1 commands failed, 1 of them in the warmup; the first: client_seq 3 answered busy"},
	} {
		_, rep, _ := runOrders(t, c.f, func(cfg *config) { cfg.warmup = 100 * time.Millisecond })
		if rep.Valid || !slices.ContainsFunc(rep.Problems, func(p string) bool { return strings.HasPrefix(p, c.want) }) {
			t.Errorf("%s: valid %v, problems %q; want invalid by %q", c.name, rep.Valid, rep.Problems, c.want)
		}
	}
}

// discardConn takes every write.
type discardConn struct{ net.Conn }

func (discardConn) Write(b []byte) (int, error) { return len(b), nil }

// offlineRun is a run of 10,000 requests, 100 µs apart, whose writes go
// nowhere: a test makes its sends and hands it its replies.
func offlineRun(orders bool) *oeRun {
	r := &oeRun{cfg: config{rate: 10_000, orders: orders}, conn: discardConn{}, period: 1e5, total: 10_000}
	r.lag, r.rtt, r.gap, r.got = make([]int64, r.total), make([]int64, r.total), make([]int64, r.total), make([]bool, r.total)
	r.out, r.in = make([]byte, 0, maxBatch*31), make([]byte, 0, 4096)
	if orders {
		r.gen = flow.New(1, 5)
		r.ids, r.cmds, r.res = make([]atomic.Uint64, r.total+1), make([]command, r.total), make([]outcome, r.total)
	}
	return r
}

// A command is timed from the due time of its own request, also once
// skipped cancels have left the client_seq behind the requests.
func TestOEOrdersAreTimedFromTheirDueTimes(t *testing.T) {
	r := offlineRun(true)
	// No order is answered, so every cancel is skipped.
	i := 0
	for r.nsent == i || r.cmds[r.nsent-1].request != int32(i-1) {
		var err error
		if i, err = r.send(i, r.due(i)); err != nil {
			t.Fatal(err)
		}
	}
	seq := uint64(r.nsent) // of the order that request i-1 was
	r.in = oe.Done{ServerSeq: 1, ClientSeq: seq, Status: oe.StatusOK, OrderID: fakeOrderID(1)}.Append(r.in)
	if err := r.take(r.due(i-1) + 1234); err != nil {
		t.Fatal(err)
	}
	if got := r.rtt[seq-1]; got != 1234 || seq == uint64(i) {
		t.Errorf("request %d, sent as client_seq %d and answered 1,234 ns after it was due, is timed at %d ns", i, seq, got)
	}
}

// A command's Events come together, before its Done: a Done after the
// Events of another command ends the run.
func TestOEOrdersEventsBelongToTheNextDone(t *testing.T) {
	for _, of := range []uint64{0, 2} {
		r := offlineRun(true)
		if _, err := r.send(0, r.due(0)); err != nil { // the first op is an order
			t.Fatal(err)
		}
		r.in = oe.Event{ServerSeq: 1, ClientSeq: of, Event: book.Event{Kind: book.EventAccepted}}.Append(r.in)
		r.in = oe.Done{ServerSeq: 2, ClientSeq: 1, Status: oe.StatusOK, OrderID: fakeOrderID(1)}.Append(r.in)
		if err := r.take(r.due(0)); err == nil {
			t.Errorf("the Done of client_seq 1 after an Event of client_seq %d was taken", of)
		}
	}
}

func TestOELoopsAllocateNothing(t *testing.T) {
	r := offlineRun(false)
	i, seq := 0, uint64(1)
	allocs := testing.AllocsPerRun(1000, func() {
		var err error
		if i, err = r.send(i, r.due(i)); err != nil {
			t.Fatal(err)
		}
		r.in = oe.Pong{ClientSeq: seq}.Append(r.in)
		seq++
		if err := r.take(r.due(i) + 1000); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Errorf("%v allocations to send a Ping and take its Pong, want 0", allocs)
	}

	// The order flow: each command is answered at once, so that cancels go
	// out too, and every other order with Events first.
	r = offlineRun(true)
	i, cancels := 0, 0
	var serverSeq uint64
	allocs = testing.AllocsPerRun(1000, func() {
		was := r.nsent
		var err error
		if i, err = r.send(i, r.due(i)); err != nil {
			t.Fatal(err)
		}
		if r.nsent == was {
			return // a skipped cancel: none here, where every order is answered before the next request
		}
		seq := uint64(r.nsent)
		if r.cmds[was].order == 0 {
			cancels++
		} else if seq%2 == 0 {
			for _, kind := range fakeEvents {
				serverSeq++
				r.in = oe.Event{ServerSeq: serverSeq, ClientSeq: seq, Event: book.Event{Kind: kind}}.Append(r.in)
			}
		}
		serverSeq++
		r.in = oe.Done{ServerSeq: serverSeq, ClientSeq: seq, Status: oe.StatusOK, OrderID: fakeOrderID(int(seq))}.Append(r.in)
		if err := r.take(r.due(i) + 1000); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 || r.nsent != i || cancels < 200 || r.trades == 0 {
		t.Errorf("%v allocations to send a command and take its reply, want 0; %d of %d requests sent, %d of them cancels, %d trades",
			allocs, r.nsent, i, cancels, r.trades)
	}

	// R1: each order is answered at once with its two Events.
	r = newOERun(config{rate: 10_000, duration: time.Second, ioc: true}, discardConn{})
	r.in = make([]byte, 0, 4096)
	i, serverSeq = 0, 0
	allocs = testing.AllocsPerRun(1000, func() {
		var err error
		if i, err = r.send(i, r.due(i)); err != nil {
			t.Fatal(err)
		}
		seq := uint64(r.nsent)
		for _, kind := range fakeIOCEvents {
			serverSeq++
			r.in = oe.Event{ServerSeq: serverSeq, ClientSeq: seq, Event: book.Event{Kind: kind}}.Append(r.in)
		}
		serverSeq++
		r.in = oe.Done{ServerSeq: serverSeq, ClientSeq: seq, Status: oe.StatusOK, OrderID: fakeOrderID(int(seq)),
			State: engine.StateCanceled, Reason: book.ReasonIOCRemainder}.Append(r.in)
		if err := r.take(r.due(i) + 1000); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 || r.nsent != i || slices.ContainsFunc(r.res[:i], func(o outcome) bool { return o != orderCanceled }) {
		t.Errorf("%v allocations to send an IOC order and take its reply, want 0; %d of %d requests sent, outcomes %v",
			allocs, r.nsent, i, r.res[:min(i, 5)])
	}

	// R2: the Done of each order queues its Cancel, which goes out with a
	// later order, and every command is answered at once.
	r = newOERun(config{rate: 10_000, duration: time.Second, ackCancel: true}, discardConn{})
	r.in = make([]byte, 0, 4096)
	i, serverSeq, cancels = 0, 0, 0
	allocs = testing.AllocsPerRun(1000, func() {
		was := r.nsent
		var err error
		if i, err = r.send(i, r.due(i)); err != nil {
			t.Fatal(err)
		}
		for seq := was + 1; seq <= r.nsent; seq++ {
			c := r.cmds[seq-1]
			d := oe.Done{ClientSeq: uint64(seq), Status: oe.StatusOK, OrderID: fakeOrderID(int(c.request) + 1), State: engine.StateOpen}
			if c.order == 0 {
				cancels++
				d.State, d.Reason = engine.StateCanceled, book.ReasonUserCancel
			} else {
				for _, kind := range fakeRestEvents {
					serverSeq++
					r.in = oe.Event{ServerSeq: serverSeq, ClientSeq: uint64(seq), Event: book.Event{Kind: kind}}.Append(r.in)
				}
			}
			serverSeq++
			d.ServerSeq = serverSeq
			r.in = d.Append(r.in)
		}
		if err := r.take(r.due(i) + 1000); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 || cancels < 900 || r.answered.Load() != int64(i) {
		t.Errorf("%v allocations to send an order and a Cancel and take their replies, want 0; %d Cancels sent for %d orders, %d answered",
			allocs, cancels, i, r.answered.Load())
	}
	for seq, out := range r.res[:r.nsent] {
		want := orderRested
		if r.cmds[seq].order == 0 {
			want = cancelCanceled
		}
		if out != want {
			t.Fatalf("client_seq %d, order %d: outcome %v, want %v", seq+1, r.cmds[seq].order, outcomeNames[out], outcomeNames[want])
		}
	}
}

// Every write the pacer makes carries a Ping, at a rate whose period is
// not a whole number of nanoseconds too.
func TestOESendTakesWhatIsDue(t *testing.T) {
	r := &oeRun{cfg: config{rate: 71_400}, conn: discardConn{}, period: 1e9 / 71_400.0, total: 100_000}
	r.lag = make([]int64, r.total)
	r.out = make([]byte, 0, maxBatch*31)
	for i := 0; i < r.total; {
		n, err := r.send(i, r.due(i))
		if err != nil {
			t.Fatal(err)
		}
		if n != i+1 {
			t.Fatalf("send(%d) at its due time sent Pings %d to %d, want only Ping %d", i, i+1, n, i+1)
		}
		i = n
	}
	n, _ := r.send(0, r.due(10)) // late: Pings 1 to 11 are due
	if n != 11 {
		t.Fatalf("send(0) at Ping 11's due time sent up to %d, want 11", n)
	}
}

// runOEReport runs loadgen with -oe against a fakeOE and the flags given,
// and returns what it printed and the report it wrote.
func runOEReport(t *testing.T, flags ...string) (string, report) {
	t.Helper()
	return runOEReportWith(t, fakeOE{}, flags...)
}

// runOEReportWith is runOEReport against f.
func runOEReportWith(t *testing.T, f fakeOE, flags ...string) (string, report) {
	t.Helper()
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		if c, err := ln.Accept(); err == nil {
			f.serve(c)
		}
	}()
	out := filepath.Join(t.TempDir(), "r.json")
	var stdout bytes.Buffer
	// The send lag bound is far above this machine's noise: the run is
	// valid unless the tool fails.
	args := append([]string{"-oe", ln.Addr().String(), "-rate", "1000", "-warmup", "0s",
		"-duration", "500ms", "-max-send-lag", "1h", "-out", out}, flags...)
	runErr := run(context.Background(), args, &stdout)
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("%v: %v\n%s", runErr, err, stdout.String())
	}
	var r report
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	// Whether the target rate went out depends on how busy the machine is,
	// as rateAside says: the caller sees a miss in the report, and any other
	// reason for run to fail stops the test here.
	if runErr != nil && (len(r.Problems) == 0 || slices.ContainsFunc(r.Problems, func(p string) bool { return !strings.HasPrefix(p, "achieved ") })) {
		t.Fatalf("%v\n%s", runErr, stdout.String())
	}
	return stdout.String(), r
}

func TestRunOEWritesReport(t *testing.T) {
	stdout, r := runOEReport(t)
	if !strings.Contains(stdout, "request to pong") || !strings.Contains(stdout, "\nvalid\n") {
		t.Errorf("report:\n%s", stdout)
	}
	if r.Mode != "oe" || r.Outcomes["ping"] != 500 || r.Latency["pong"].Count != 500 || r.Latency["send_lag"].Count != 500 {
		t.Errorf("report %+v", r)
	}
}

// With -orders the same flags send the flow of -seed and -sd on -symbol,
// and the report has the REST mode's rows.
func TestRunOEOrdersWritesReport(t *testing.T) {
	stdout, r := runOEReport(t, "-orders", "-symbol", fakeSymbol, "-seed", "7", "-sd", "3")
	for _, want := range []string{"OE1 orders on " + fakeSymbol, "\ntrades: ", " in replies\n", "request to reply", "service time", "send lag", "\nvalid\n"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("no %q in the report:\n%s", want, stdout)
		}
	}
	offered := 0
	for _, n := range r.Outcomes {
		offered += n
	}
	if r.Mode != "oe_orders" || r.Symbol != fakeSymbol || r.Seed != 7 || r.SD != 3 || offered != 500 ||
		r.Trades.Replies == 0 || r.Latency["reply"].Count == 0 || r.Latency["reply"].Count != r.Latency["service"].Count {
		t.Errorf("report %+v", r)
	}
	if _, ok := r.Latency["pong"]; ok || r.Outcomes["ping"] != 0 {
		t.Errorf("a run of the order flow reports Pings: %+v", r)
	}
}

// -orders chooses what an OE1 session carries, so it needs -oe.
func TestRunOrdersNeedsOE(t *testing.T) {
	err := run(context.Background(), []string{"-orders"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "-orders without -oe") {
		t.Errorf("run -orders = %v, want an error asking for -oe", err)
	}
}
