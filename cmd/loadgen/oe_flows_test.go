package main

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/engine"
	"github.com/sHyanorn/matching-engine/internal/oe"
)

// iocConfig and ackConfig are oeConfig for R1 and R2 against dial, without a
// warmup so that the report covers the whole run. An order that gets no Done
// is waited for 300 ms.
func iocConfig(dial func() (net.Conn, error)) config {
	cfg := oeConfig(dial)
	cfg.ioc, cfg.symbol, cfg.warmup, cfg.replyWait = true, fakeSymbol, 0, 300*time.Millisecond
	return cfg
}

func ackConfig(dial func() (net.Conn, error)) config {
	cfg := iocConfig(dial)
	cfg.ioc, cfg.ackCancel = false, true
	return cfg
}

// arrival is a command the fake got, and when.
type arrival struct {
	cmd any
	at  time.Time
}

// runFlow runs the flow that flow configures against f and returns the run,
// its report and the commands f got in the order they arrived.
func runFlow(t *testing.T, f fakeOE, flow func(func() (net.Conn, error)) config, set func(*config)) (*oeRun, report, []arrival) {
	t.Helper()
	var mu sync.Mutex
	var got []arrival
	reply := f.reply
	f.reply = func(cmd any, a *fakeAnswer) {
		mu.Lock()
		got = append(got, arrival{cmd, a.at})
		mu.Unlock()
		if reply != nil {
			reply(cmd, a)
		}
	}
	cfg := flow(listenOE(t, f))
	if set != nil {
		set(&cfg)
	}
	r, err := runOE(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock() // the fake has answered its last command: the session has ended
	defer mu.Unlock()
	return r, r.report(), got
}

func problemWith(rep report, want string) bool {
	return slices.ContainsFunc(rep.Problems, func(p string) bool { return strings.Contains(p, want) })
}

// R1 sends IOC buys of one lot at a fixed price, with the numbering of the
// session and nothing else, and counts each Done as an order canceled.
func TestOEIOCSendsIOCOrders(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	r, rep, got := runFlow(t, fakeOE{}, iocConfig, nil)
	rateAside(t, rep)
	if len(got) != r.total {
		t.Fatalf("%d commands sent, want one for each of the %d requests", len(got), r.total)
	}
	for k, a := range got {
		want := oe.NewOrder{ClientSeq: uint64(k + 1), Symbol: 1, Side: book.Buy, Type: book.Limit, TIF: book.IOC, Price: 6_500_000, Qty: 1}
		if a.cmd != want {
			t.Fatalf("command %d is %+v, want %+v", k+1, a.cmd, want)
		}
	}
	if want := map[string]int{"order_canceled": r.total}; !maps.Equal(rep.Outcomes, want) {
		t.Errorf("outcomes %v, want %v", rep.Outcomes, want)
	}
	for _, k := range []string{"reply", "service", "send_lag"} {
		if n := rep.Latency[k].Count; n != r.total {
			t.Errorf("%d %s samples, want one per order, %d", n, k, r.total)
		}
	}
	if rep.Mode != "oe_ioc" || rep.Symbol != fakeSymbol || rep.Trades.Replies != 0 {
		t.Errorf("mode %q, symbol %q, %d trades in the replies", rep.Mode, rep.Symbol, rep.Trades.Replies)
	}
}

// A Done that is not that of an IOC order that rested nowhere and traded
// with nothing fails the run, and the report names the first command that
// failed.
func TestOEIOCInvalidDones(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	tradeEvent := book.Event{Seq: 1, Kind: book.EventTrade, OrderID: fakeOrderID(3), MakerID: fakeOrderID(9), Price: 1, Qty: 1}
	for _, c := range []struct {
		name   string
		answer func(a *fakeAnswer)
		want   string // in the problem
	}{
		{"state open", func(a *fakeAnswer) { a.done.State = engine.StateOpen }, "in state open"},
		{"state filled", func(a *fakeAnswer) { a.done.State = engine.StateFilled }, "in state filled"},
		{"reason user_cancel", func(a *fakeAnswer) { a.done.Reason = book.ReasonUserCancel }, "reason user_cancel"},
		{"no reason", func(a *fakeAnswer) { a.done.Reason = 0 }, "reason none"},
		{"three Events", func(a *fakeAnswer) {
			a.events = []book.EventKind{book.EventAccepted, book.EventRested, book.EventCanceled}
		}, "3 Events"},
		{"one Event", func(a *fakeAnswer) { a.events = a.events[:1] }, "1 Events"},
		{"Events in the wrong order", func(a *fakeAnswer) {
			a.events = []book.EventKind{book.EventCanceled, book.EventAccepted}
		}, "the first two canceled, accepted"},
		{"a trade Event", func(a *fakeAnswer) {
			a.events, a.trade = []book.EventKind{book.EventAccepted, book.EventTrade}, tradeEvent
		}, "1 trades"},
		{"order ID 0", func(a *fakeAnswer) { a.done.OrderID = 0 }, "with order 0"},
		{"status invalid", func(a *fakeAnswer) { a.done.Status = oe.StatusInvalid }, "answered invalid"},
		{"status busy", func(a *fakeAnswer) { a.done.Status = oe.StatusBusy }, "answered busy"},
	} {
		// The third and fifth orders both fail: the first is named.
		f := fakeOE{reply: func(cmd any, a *fakeAnswer) {
			if a.seq == 3 || a.seq == 5 {
				c.answer(a)
			}
		}}
		_, rep, _ := runFlow(t, f, iocConfig, nil)
		wantFirst := "2 commands failed, 0 of them in the warmup; the first: client_seq 3 answered "
		if rep.Valid || !problemWith(rep, wantFirst) || !problemWith(rep, c.want) {
			t.Errorf("%s: valid %v, problems %q; want invalid by %q naming %q", c.name, rep.Valid, rep.Problems, wantFirst, c.want)
		}
		if want := (map[string]int{"error": 2, "order_canceled": rep.Latency["reply"].Count}); !maps.Equal(rep.Outcomes, want) || want["order_canceled"] < 900 {
			t.Errorf("%s: outcomes %v, want 2 errors and the other orders canceled, %v", c.name, rep.Outcomes, want)
		}
		if c.name == "a trade Event" && rep.Trades.Replies != 2 {
			t.Errorf("%s: %d trades in the replies, want the 2 that were sent", c.name, rep.Trades.Replies)
		}
	}
}

// ackFrames checks the commands f got in a run of R2 and returns, for each
// order, the Cancel that named it. The commands are the orders and their
// Cancels, numbered from 1 in the order they arrived, and each Cancel comes
// no sooner than minGap after its order.
func ackFrames(t *testing.T, got []arrival, minGap time.Duration) (orders int, canceled map[book.OrderID]arrival) {
	t.Helper()
	arrived := map[book.OrderID]time.Time{}
	canceled = map[book.OrderID]arrival{}
	for k, a := range got {
		switch m := a.cmd.(type) {
		case oe.NewOrder:
			orders++
			want := oe.NewOrder{ClientSeq: uint64(k + 1), Symbol: 1, Side: book.Buy, Type: book.Limit, TIF: book.GTC, Price: 6_400_000, Qty: 1}
			if m != want {
				t.Fatalf("command %d is %+v, want %+v", k+1, m, want)
			}
			arrived[fakeOrderID(orders)] = a.at
		case oe.Cancel:
			at, ok := arrived[m.OrderID]
			switch {
			case m.ClientSeq != uint64(k+1):
				t.Fatalf("command %d is a Cancel with client_seq %d", k+1, m.ClientSeq)
			case !ok:
				t.Fatalf("command %d cancels order %d, which had not arrived", k+1, m.OrderID)
			case canceled[m.OrderID].cmd != nil:
				t.Fatalf("command %d cancels order %d a second time", k+1, m.OrderID)
			case a.at.Sub(at) < minGap:
				t.Fatalf("order %d arrived %v before its Cancel, want at least %v, the time its Done was held back", m.OrderID, a.at.Sub(at), minGap)
			}
			canceled[m.OrderID] = a
		default:
			t.Fatalf("command %d is %T", k+1, a.cmd)
		}
	}
	return orders, canceled
}

// R2 cancels each order once its Done is in, and not before: with every Done
// held back 2 ms, no Cancel arrives within 2 ms of its order. Every order
// is canceled once, and client_seq runs without a gap in the order the
// commands arrived, Cancels included.
func TestOEAckCancelFollowsTheDone(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	if runtime.GOOS == "windows" {
		t.Skip("Go's clock on Windows advances once per tick (bench-results/m6-windows)")
	}
	const hold = 2 * time.Millisecond
	for _, spin := range []bool{true, false} {
		r, rep, got := runFlow(t, fakeOE{delay: hold}, ackConfig, func(c *config) { c.spin = spin })
		rateAside(t, rep)
		orders, canceled := ackFrames(t, got, hold)
		if orders != r.total || len(canceled) != r.total || len(got) != 2*r.total {
			t.Errorf("spin %v: %d orders and %d Cancels of %d commands, want %d each", spin, orders, len(canceled), len(got), r.total)
		}
		want := map[string]int{"order_rested": r.total, "cancel_canceled": r.total}
		if !maps.Equal(rep.Outcomes, want) || rep.Cancels != r.total || rep.Mode != "oe_ack_cancel" {
			t.Errorf("spin %v: mode %q, outcomes %v and %d Cancels sent, want %v and %d", spin, rep.Mode, rep.Outcomes, rep.Cancels, want, r.total)
		}
		for _, k := range []string{"reply", "cancel", "order_to_cancel", "send_lag"} {
			if n := rep.Latency[k].Count; n != r.total {
				t.Errorf("spin %v: %d %s samples, want one per order, %d", spin, n, k, r.total)
			}
		}
		if _, ok := rep.Latency["service"]; ok {
			t.Errorf("spin %v: a run of R2 reports a service time", spin)
		}
		if h, ok := rep.Latency["hiccup"]; ok != spin || ok && h.Count == 0 {
			t.Errorf("spin %v: hiccup row %+v (present %v), want samples with -spin and none without", spin, h, ok)
		}
	}
}

// The report of R2 describes the orders due in the window and their Cancels:
// the warmup's orders are sent and canceled too, but not counted.
func TestOEAckCancelReportsTheWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	r, rep, got := runFlow(t, fakeOE{}, ackConfig, func(c *config) { c.warmup = 100 * time.Millisecond })
	rateAside(t, rep)
	window := int(r.cfg.rate * r.cfg.duration.Seconds())
	if r.total != window+int(r.cfg.rate*r.cfg.warmup.Seconds()) || len(got) != 2*r.total {
		t.Fatalf("%d commands for %d orders in all, want twice as many: a Cancel for each, the warmup's too", len(got), r.total)
	}
	want := map[string]int{"order_rested": window, "cancel_canceled": window}
	if !maps.Equal(rep.Outcomes, want) || rep.Cancels != window || rep.Latency["cancel"].Count != window {
		t.Errorf("outcomes %v, %d Cancels and %d cancel samples, want %v and %d of each", rep.Outcomes, rep.Cancels, rep.Latency["cancel"].Count, want, window)
	}
}

// The three latencies of R2 are timed from their own instants: with the Done
// of an order held back by d1 and that of a Cancel by d2, reply is at least
// d1, cancel at least d2, and order_to_cancel at least d1+d2, and cancel
// does not count the d1 that came before it was sent. The server answers in
// the order the commands arrived, so d2 is the longer hold: a Done never
// waits behind one due later.
func TestOEAckCancelTimes(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	if runtime.GOOS == "windows" {
		t.Skip("Go's clock on Windows advances once per tick (bench-results/m6-windows)")
	}
	const d1, d2 = 4 * time.Millisecond, 10 * time.Millisecond
	r, rep, _ := runFlow(t, fakeOE{delay: d1, cancelDelay: d2}, ackConfig, nil)
	rateAside(t, rep)
	// Each order is timed from its own due time, and each Cancel from its
	// own send, whatever came before.
	for seq, c := range r.cmds[:r.nsent] {
		j := int(c.request)
		switch {
		case r.res[seq] == noDone:
			t.Fatalf("client_seq %d has no Done", seq+1)
		case c.order != 0 && r.rtt[seq] < int64(d1):
			t.Errorf("order %d answered %v after it was due, want at least %v", c.order, time.Duration(r.rtt[seq]), d1)
		case c.order == 0 && r.rtt[seq] < int64(d2):
			t.Errorf("Cancel of order %d answered %v after it was sent, want at least %v", j+1, time.Duration(r.rtt[seq]), d2)
		case c.order == 0 && r.cancelAt[j]+r.rtt[seq]-r.due(j) < int64(d1+d2):
			t.Errorf("order %d canceled %v after it was due, want at least %v", j+1, time.Duration(r.cancelAt[j]+r.rtt[seq]-r.due(j)), d1+d2)
		}
	}
	us := func(d time.Duration) float64 { return float64(d) / float64(time.Microsecond) }
	if rep.Latency["reply"].P50 < us(d1) || rep.Latency["cancel"].P50 < us(d2) || rep.Latency["order_to_cancel"].P50 < us(d1+d2) {
		t.Errorf("p50 of reply %.0f, cancel %.0f, order_to_cancel %.0f µs, want at least %.0f, %.0f, %.0f",
			rep.Latency["reply"].P50, rep.Latency["cancel"].P50, rep.Latency["order_to_cancel"].P50, us(d1), us(d2), us(d1+d2))
	}
	// The Cancel's own wait is d2 and a little: counted from the order's due
	// time it would be d1+d2 or more.
	if p := rep.Latency["cancel"].P50; p > us(d2+d1/2) {
		t.Errorf("cancel p50 %.0f µs, want about %.0f: it must not count the %v before the Cancel was sent", p, us(d2), d1)
	}
}

// An order that gets no Done is not canceled, and fails the run; the other
// orders are canceled, and the run waits for the missing Done only for the
// time given.
func TestOEAckCancelUnansweredOrder(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	start := time.Now()
	r, rep, got := runFlow(t, fakeOE{drop: 1}, ackConfig, nil)
	if d := time.Since(start); d < r.cfg.duration+r.cfg.replyWait {
		t.Errorf("run took %v, want it to wait %v for the Done of order 1 after the last order, at %v", d, r.cfg.replyWait, r.cfg.duration)
	}
	orders, canceled := ackFrames(t, got, 0)
	if _, first := canceled[fakeOrderID(1)]; first || len(canceled) != orders-1 {
		t.Errorf("%d of %d orders canceled, order 1 among them: %v; want all but order 1", len(canceled), orders, first)
	}
	if rep.Valid || !problemWith(rep, "commands with no Done: 1") {
		t.Errorf("valid %v, problems %q; want invalid by one command with no Done", rep.Valid, rep.Problems)
	}
	if rep.Outcomes["order_rested"] != r.total-1 || rep.Outcomes["cancel_canceled"] != r.total-1 {
		t.Errorf("outcomes %v, want %d orders rested and canceled", rep.Outcomes, r.total-1)
	}
}

// An order whose Done comes after the pacer has stopped waiting is not
// canceled, and the run says so. Every Done is held back 210 ms and the run
// waits 150 ms after the last order, so the orders of the last 60 ms are
// answered after the pacer has ended and before the reader has, and those
// before them are canceled.
func TestOEAckCancelLateDone(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	_, rep, got := runFlow(t, fakeOE{delay: 210 * time.Millisecond}, ackConfig, func(c *config) {
		c.duration, c.replyWait = 200*time.Millisecond, 150*time.Millisecond
	})
	if _, canceled := ackFrames(t, got, 0); len(canceled) == 0 {
		t.Error("no order was canceled, though those of the first 140 ms were answered before the pacer stopped waiting")
	}
	if rep.Valid || !problemWith(rep, "orders that rested and were not canceled: ") {
		t.Errorf("valid %v, problems %q; want invalid by orders that rested and were not canceled", rep.Valid, rep.Problems)
	}
}

// A Done of an order or a Cancel that is not what R2 expects fails the run
// and is named, and an order that did not rest is not canceled.
func TestOEAckCancelInvalidDones(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	// check changes the answer to the first order, or to the first Cancel,
	// and expects one failed command, named, and no Cancel for an order that
	// did not rest. The orders are client_seq 1, 2 and so on until the first
	// Cancel goes out.
	check := func(name string, cancel bool, answer func(a *fakeAnswer), wants ...string) {
		t.Helper()
		failed := false
		f := fakeOE{reply: func(cmd any, a *fakeAnswer) {
			_, isCancel := cmd.(oe.Cancel)
			if isCancel == cancel && !failed {
				failed = true
				answer(a)
			}
		}}
		// Every order is answered, so the run does not wait for one.
		start := time.Now()
		r, rep, got := runFlow(t, f, ackConfig, func(c *config) { c.replyWait = 5 * time.Second })
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("%s: run took %v, want about %v: every order was answered", name, d, r.cfg.duration)
		}
		wants = append(wants, "1 commands failed, 0 of them in the warmup; the first: client_seq ")
		for _, want := range wants {
			if rep.Valid || !problemWith(rep, want) {
				t.Errorf("%s: valid %v, problems %q; want invalid by one failed command with %q", name, rep.Valid, rep.Problems, want)
			}
		}
		if rep.Outcomes["error"] != 1 {
			t.Errorf("%s: outcomes %v, want 1 error", name, rep.Outcomes)
		}
		cancels := len(got) - r.total
		if want := r.total - 1 + b2i(cancel); cancels != want {
			t.Errorf("%s: %d Cancels sent, want %d: one for each order that rested", name, cancels, want)
		}
	}
	tradeEvent := book.Event{Seq: 1, Kind: book.EventTrade, OrderID: fakeOrderID(3), MakerID: fakeOrderID(9), Price: 1, Qty: 1}
	for _, c := range []struct {
		name   string
		cancel bool // changes the answers to the Cancels, not to the orders
		answer func(a *fakeAnswer)
		want   string
	}{
		{"order not open", false, func(a *fakeAnswer) { a.done.State = engine.StateFilled }, "in state filled"},
		{"order without ID", false, func(a *fakeAnswer) { a.done.OrderID = 0 }, "with order 0"},
		{"order with a trade", false, func(a *fakeAnswer) {
			a.events, a.trade = []book.EventKind{book.EventAccepted, book.EventTrade}, tradeEvent
		}, "1 trades"},
		{"order invalid", false, func(a *fakeAnswer) { a.done.Status = oe.StatusInvalid }, "answered invalid"},
		// The engine sends accepted, then rested, for an order that rests:
		// any other Events fail the run.
		{"order with one Event", false, func(a *fakeAnswer) { a.events = a.events[:1] }, "1 Events"},
		{"order with three Events", false, func(a *fakeAnswer) {
			a.events = []book.EventKind{book.EventAccepted, book.EventRested, book.EventRested}
		}, "3 Events"},
		{"order with its Events the wrong way round", false, func(a *fakeAnswer) {
			a.events = []book.EventKind{book.EventRested, book.EventAccepted}
		}, "the first two rested, accepted"},
		{"order with the Events of one that is canceled", false, func(a *fakeAnswer) {
			a.events = []book.EventKind{book.EventAccepted, book.EventCanceled}
		}, "the first two accepted, canceled"},
		{"Cancel with a trade", true, func(a *fakeAnswer) {
			a.events, a.trade = []book.EventKind{book.EventTrade}, tradeEvent
		}, "1 trades"},
		{"Cancel not canceled", true, func(a *fakeAnswer) { a.done.State = engine.StateOpen }, "in state open"},
		{"Cancel with another reason", true, func(a *fakeAnswer) { a.done.Reason = book.ReasonIOCRemainder }, "reason ioc_remainder"},
		{"Cancel finished", true, func(a *fakeAnswer) { a.done.Status = oe.StatusOrderFinished }, "answered order_finished"},
	} {
		check(c.name, c.cancel, c.answer, c.want)
	}
	// A Cancel's Done names the order that was canceled: the first Cancel
	// is for the first order, which the fake numbers 1. The problem says
	// which order it named and which it should have.
	wantID := fmt.Sprintf("order %d and no trade Event", fakeOrderID(1))
	check("Cancel naming another order", true, func(a *fakeAnswer) { a.done.OrderID = fakeOrderID(2) },
		fmt.Sprintf("with order %d in state canceled", fakeOrderID(2)), wantID)
	check("Cancel naming no order", true, func(a *fakeAnswer) { a.done.OrderID = 0 }, "with order 0 in state canceled", wantID)
}

// loggedConn records, for each write, when it began and the client_seq of the
// NewOrders and Cancels in it, and holds back a write that carries a Cancel.
type loggedConn struct {
	net.Conn
	slow time.Duration

	mu  sync.Mutex
	log []loggedWrite
}

type loggedWrite struct {
	at              time.Time
	orders, cancels []uint64
}

func (c *loggedConn) Write(b []byte) (int, error) {
	w := loggedWrite{at: time.Now()}
	for rest := b; ; {
		typ, body, n, err := oe.Next(rest)
		if err != nil || n == 0 {
			break
		}
		switch typ {
		case oe.TypeNewOrder:
			w.orders = append(w.orders, oe.DecodeNewOrder(body).ClientSeq)
		case oe.TypeCancel:
			w.cancels = append(w.cancels, oe.DecodeCancel(body).ClientSeq)
		default: // a Login, a Heartbeat or the Logout
		}
		rest = rest[n:]
	}
	c.mu.Lock()
	c.log = append(c.log, w)
	c.mu.Unlock()
	if len(w.cancels) > 0 {
		time.Sleep(c.slow)
	}
	return c.Conn.Write(b)
}

// A pacer whose write is held up by the Cancels it carries delays the orders
// due meanwhile, and their send lag shows it, as it does for any write: it is
// the time from an order's due time to just before the write that carries
// it, and a Cancel's send time is taken just before its own write.
func TestOEAckCancelSlowWriteIsSendLag(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	const slow = 3 * time.Millisecond
	dial := listenOE(t, fakeOE{})
	var lc *loggedConn
	cfg := ackConfig(func() (net.Conn, error) {
		c, err := dial()
		lc = &loggedConn{Conn: c, slow: slow}
		return lc, err
	})
	cfg.maxSendLag = time.Millisecond
	r, err := runOE(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	rep := r.report()
	if rep.Valid || !problemWith(rep, "send lag p99.9") {
		t.Errorf("valid %v, problems %q; want invalid by send lag", rep.Valid, rep.Problems)
	}
	if n, max := rep.Latency["send_lag"].Count, rep.Latency["send_lag"].Max; n != r.total || max < 2000 {
		t.Errorf("%d send lag samples with a longest of %.0f µs; want one for each of the %d orders, and some due during a %v write sent about that late", n, max, r.total, slow)
	}
	lc.mu.Lock()
	defer lc.mu.Unlock()
	checked := 0
	for _, w := range lc.log {
		check := func(what string, seq uint64, sentAt int64) {
			t.Helper()
			if d := w.at.Sub(r.t0.Add(time.Duration(sentAt))); d < 0 || d > 3*time.Millisecond {
				t.Errorf("%s of client_seq %d was stamped %v before the write that carried it began, want just before", what, seq, d)
			}
			checked++
		}
		for _, seq := range w.orders {
			j := r.cmds[seq-1].request
			check("the send time", seq, r.due(int(j))+r.lag[j])
		}
		for _, seq := range w.cancels {
			check("the send time", seq, r.cancelAt[r.cmds[seq-1].request])
		}
	}
	if checked < 2*r.total {
		t.Errorf("%d commands checked against the writes, want %d", checked, 2*r.total)
	}
}

// With -spin=false the pacer sleeps, and still sends a Cancel soon after the
// Done that queued it, not when the next order falls due: at 100 orders a
// second that is 10 ms away.
func TestOEAckCancelIsSentSoonAfterItsDone(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	for _, spin := range []bool{true, false} {
		r, rep, _ := runFlow(t, fakeOE{}, ackConfig, func(c *config) {
			c.spin, c.rate, c.duration = spin, 100, time.Second
		})
		rateAside(t, rep)
		var waits []int64 // from the Done of each order to the send of its Cancel
		for seq, c := range r.cmds[:r.nsent] {
			if c.order != 0 {
				waits = append(waits, r.cancelAt[c.request]-(r.due(int(c.request))+r.rtt[seq]))
			}
		}
		slices.Sort(waits)
		if len(waits) != r.total || waits[len(waits)/2] > int64(2*time.Millisecond) {
			t.Errorf("spin %v: %d Cancels, the median %v after the Done; want %d, within 2 ms", spin, len(waits), time.Duration(waits[len(waits)/2]), r.total)
		}
	}
}

// Once every order is answered and canceled, the run ends: it does not wait
// out the time it allows for a Done that is missing.
func TestOEAckCancelEndsWhenEverythingIsAnswered(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	start := time.Now()
	r, rep, _ := runFlow(t, fakeOE{}, ackConfig, func(c *config) { c.replyWait = 0 })
	rateAside(t, rep)
	if d := time.Since(start); d > r.cfg.duration+2*time.Second {
		t.Errorf("run took %v, want about %v, not a wait of 5 s for Dones that were all in", d, r.cfg.duration)
	}
}

// An order that rested is reported as not canceled only if no Cancel command
// names it, whatever time its Cancel went out at: a coarse clock, as Go's on
// Windows, reads 0 for the sends of the first tick of a run.
func TestOEAckCancelRestingOrdersComeFromTheCommands(t *testing.T) {
	order := func(j int) command { return command{request: int32(j), order: int32(j + 1)} }
	cancel := func(j int) command { return command{request: int32(j)} }
	for _, c := range []struct {
		name     string
		cmds     []command
		res      []outcome
		cancelAt int64  // when the Cancel of the first order went out
		want     string // in the problem; "" for none
	}{
		{"a Cancel sent at time 0", []command{order(0), cancel(0)}, []outcome{orderRested, cancelCanceled}, 0, ""},
		{"a Cancel sent at time 0 for the second order", []command{order(0), order(1), cancel(0), cancel(1)},
			[]outcome{orderRested, orderRested, cancelCanceled, cancelCanceled}, 0, ""},
		{"a Cancel sent after time 0", []command{order(0), cancel(0)}, []outcome{orderRested, cancelCanceled}, 1234, ""},
		{"no Cancel", []command{order(0)}, []outcome{orderRested}, 0, "orders that rested and were not canceled: 1"},
		{"the Cancel of another order", []command{order(0), order(1), cancel(1)}, []outcome{orderRested, orderRested, cancelCanceled}, 0,
			"orders that rested and were not canceled: 1"},
	} {
		r := newOERun(config{rate: 10_000, duration: time.Second, ackCancel: true}, discardConn{})
		copy(r.cmds, c.cmds)
		copy(r.res, c.res)
		r.nsent = len(c.cmds)
		r.cancelAt[0] = c.cancelAt // every other send time is 0 too
		rep := r.report()
		if c.want == "" && problemWith(rep, "were not canceled") || c.want != "" && !problemWith(rep, c.want) {
			t.Errorf("%s: problems %q, want one containing %q (none if empty)", c.name, rep.Problems, c.want)
		}
	}
}

// A write holds at most maxBatch frames, the orders that are due first and
// the Cancels in the room that is left.
func TestOEAckCancelBatchIsBounded(t *testing.T) {
	r := newOERun(config{rate: 10_000, duration: time.Second, ackCancel: true}, discardConn{})
	const queued = 3000
	for order := 1; order <= queued; order++ {
		r.ids[order].Store(uint64(fakeOrderID(order)))
		r.queue(int32(order))
	}
	frames := func() (orders, cancels int) {
		for rest := r.out; ; {
			typ, _, n, err := oe.Next(rest)
			if err != nil || n == 0 {
				return orders, cancels
			}
			switch typ {
			case oe.TypeNewOrder:
				orders++
			case oe.TypeCancel:
				cancels++
			default:
			}
			rest = rest[n:]
		}
	}
	// Orders 1 to 10 are due, and 3,000 Cancels are queued.
	next, err := r.send(0, r.due(9))
	if orders, cancels := frames(); err != nil || next != 10 || orders != 10 || cancels != maxBatch-10 || r.taken != maxBatch-10 {
		t.Errorf("send(0): next %d, %d orders and %d Cancels in %d bytes, %d queued Cancels taken, error %v; want 10 orders and %d Cancels",
			next, orders, cancels, len(r.out), r.taken, err, maxBatch-10)
	}
	// Nothing is due: the Cancels alone fill a write, which does not move
	// the next order.
	next, err = r.send(10, r.due(10)-1)
	if orders, cancels := frames(); err != nil || next != 10 || orders != 0 || cancels != maxBatch || r.taken != 2*maxBatch-10 {
		t.Errorf("send(10): next %d, %d orders and %d Cancels, %d taken, error %v; want only %d Cancels", next, orders, cancels, r.taken, err, maxBatch)
	}
	// The commands are numbered in write order.
	for k, c := range r.cmds[:r.nsent] {
		if k >= 10 && c.order != 0 || k < 10 && c.order != int32(k+1) {
			t.Fatalf("command %d is %+v", k+1, c)
		}
	}
	if r.nsent != 2*maxBatch {
		t.Errorf("%d commands noted, want %d", r.nsent, 2*maxBatch)
	}
}

// The flags that choose a flow, and what is checked against the engine's
// HTTP side, are checked before anything is dialed.
func TestRunFlowFlagErrors(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"-oe", "x:1", "-orders", "-ioc"}, "-orders and -ioc together"},
		{[]string{"-oe", "x:1", "-ioc", "-ack-cancel"}, "-ioc and -ack-cancel together"},
		{[]string{"-oe", "x:1", "-orders", "-ack-cancel"}, "-orders and -ack-cancel together"},
		{[]string{"-oe", "x:1", "-orders", "-ioc", "-ack-cancel"}, "-orders and -ioc and -ack-cancel together"},
		{[]string{"-orders"}, "-orders without -oe"},
		{[]string{"-ioc"}, "-ioc without -oe"},
		{[]string{"-ack-cancel"}, "-ack-cancel without -oe"},
		{[]string{"-oe", "x:1", "-orders", "-push"}, "-push without -http"},
		{[]string{"-oe", "x:1", "-ioc", "-push", "-http", "x:2"}, "-push without -orders"},
		{[]string{"-oe", "x:1", "-ack-cancel", "-push", "-http", "x:2"}, "-push without -orders"},
		{[]string{"-oe", "x:1", "-push", "-http", "x:2"}, "-push without -orders"},
		{[]string{"-push"}, "-push without -orders"},
		{[]string{"-http", "x:2"}, "-http without -orders, -ioc or -ack-cancel"},
		{[]string{"-oe", "x:1", "-http", "x:2"}, "-http without -orders, -ioc or -ack-cancel"},
		// A flag given at its default value counts as given.
		{[]string{"-oe", "x:1", "-ioc", "-seed", "1"}, "-seed with -ioc"},
		{[]string{"-oe", "x:1", "-ioc", "-sd", "5"}, "-sd with -ioc"},
		{[]string{"-oe", "x:1", "-ack-cancel", "-seed", "1"}, "-seed with -ack-cancel"},
		{[]string{"-oe", "x:1", "-ack-cancel", "-sd", "5"}, "-sd with -ack-cancel"},
	} {
		err := run(context.Background(), c.args, io.Discard)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("run %v = %v, want an error containing %q", c.args, err, c.want)
		}
	}
}

// -seed and -sd shape the random flow of -orders, so -ioc and -ack-cancel,
// which have none, reject them when they are given; -orders takes both.
func TestCheckFlowsSeedAndSD(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  config
		set  map[string]bool
		want string // in the error; "" for none
	}{
		{"-ioc without them", config{oeAddr: "x:1", ioc: true}, nil, ""},
		{"-ack-cancel without them", config{oeAddr: "x:1", ackCancel: true}, nil, ""},
		{"-ioc with others", config{oeAddr: "x:1", ioc: true}, map[string]bool{"rate": true, "out": true}, ""},
		{"-orders with both", config{oeAddr: "x:1", orders: true}, map[string]bool{"seed": true, "sd": true}, ""},
		{"-ioc with -seed", config{oeAddr: "x:1", ioc: true}, map[string]bool{"seed": true}, "-seed with -ioc"},
		{"-ack-cancel with -sd", config{oeAddr: "x:1", ackCancel: true}, map[string]bool{"sd": true}, "-sd with -ack-cancel"},
	} {
		err := checkFlows(c.cfg, c.set)
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: checkFlows = %v, want an error containing %q", c.name, err, c.want)
		}
	}
}

// Each flow is reported under its own mode, with the rows it has, and the
// trades line shows the pushes when they were read and the counter when it
// was.
func TestPrintReportOEFlows(t *testing.T) {
	rows := func(keys ...string) map[string]summary {
		m := map[string]summary{}
		for _, k := range keys {
			m[k] = summary{Count: 7}
		}
		return m
	}
	for _, c := range []struct {
		name string
		rep  report
		want []string
		not  []string
	}{
		{"R1", report{Mode: "oe_ioc", Symbol: "BTC-USD", Valid: true, Outcomes: map[string]int{"order_canceled": 7},
			Latency: rows("reply", "service", "send_lag", "hiccup")},
			[]string{"OE1 IOC orders on BTC-USD: ", "order_canceled 7\n", "trades: 0 in replies\n", "request to reply ", "service time ", "loadgen hiccup ", "\nvalid\n"},
			[]string{"cancels sent", "pushed", "counter rose", "cancel to reply"}},
		{"R1 with -http", report{Mode: "oe_ioc", Symbol: "BTC-USD", Valid: true, CounterRead: true, Trades: tradeCounts{Metric: 3},
			Latency: rows("reply")},
			[]string{"trades: 0 in replies, counter rose 3\n"}, []string{"pushed"}},
		{"R1 with -http, no rise", report{Mode: "oe_ioc", Symbol: "BTC-USD", Valid: true, CounterRead: true,
			Latency: rows("reply")},
			[]string{"trades: 0 in replies, counter rose 0\n"}, []string{"pushed"}},
		{"R2", report{Mode: "oe_ack_cancel", Symbol: "BTC-USD", Valid: true, Cancels: 1234,
			Outcomes: map[string]int{"order_rested": 1234, "cancel_canceled": 1234},
			Latency:  rows("reply", "cancel", "order_to_cancel", "send_lag")},
			[]string{"OE1 orders and their cancels on BTC-USD: ", "cancel_canceled 1234, order_rested 1234\n", "cancels sent: 1234\n", "trades: 0 in replies\n",
				"request to reply ", "cancel to reply ", "order to canceled ", "send lag "},
			[]string{"service time", "pushed"}},
		{"T", report{Mode: "oe_orders", Symbol: "BTC-USD", CounterRead: true, Trades: tradeCounts{Replies: 10, Pushes: 10, Metric: 10},
			Problems: []string{"x"}, Latency: rows("trade_push", "reply", "service", "send_lag")},
			[]string{"OE1 orders on BTC-USD: ", "trades: 10 in replies, 10 pushed, counter rose 10\n", "order to trade push ", "\ninvalid: x\n"},
			[]string{"cancels sent"}},
		{"R3", report{Mode: "oe_orders", Symbol: "BTC-USD", Trades: tradeCounts{Replies: 10}, Valid: true,
			Latency: rows("reply", "service", "send_lag")},
			[]string{"trades: 10 in replies\n"}, []string{"pushed", "counter rose", "order to trade push"}},
	} {
		var b strings.Builder
		if err := printReport(&b, c.rep); err != nil {
			t.Fatal(err)
		}
		for _, want := range c.want {
			if !strings.Contains(b.String(), want) {
				t.Errorf("%s: no %q in the report:\n%s", c.name, want, b.String())
			}
		}
		for _, not := range c.not {
			if strings.Contains(b.String(), not) {
				t.Errorf("%s: %q in the report:\n%s", c.name, not, b.String())
			}
		}
	}
}

// loadgen -oe -ioc and -ack-cancel print and write what the flows report.
func TestRunOEFlowsWriteReport(t *testing.T) {
	stdout, r := runOEReport(t, "-ioc", "-symbol", fakeSymbol)
	if r.Mode != "oe_ioc" || r.Symbol != fakeSymbol || r.Outcomes["order_canceled"] != 500 || r.Latency["reply"].Count != 500 ||
		r.Latency["service"].Count != 500 || r.Latency["send_lag"].Count != 500 || r.Cancels != 0 {
		t.Errorf("report %+v", r)
	}
	for _, want := range []string{"OE1 IOC orders on " + fakeSymbol, "request to reply"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("no %q in the report:\n%s", want, stdout)
		}
	}
	checkVerdict(t, stdout, r)

	stdout, r = runOEReport(t, "-ack-cancel", "-symbol", fakeSymbol)
	if r.Mode != "oe_ack_cancel" || r.Outcomes["order_rested"] != 500 || r.Outcomes["cancel_canceled"] != 500 || r.Cancels != 500 {
		t.Errorf("report %+v", r)
	}
	for _, k := range []string{"reply", "cancel", "order_to_cancel", "send_lag"} {
		if n := r.Latency[k].Count; n != 500 {
			t.Errorf("%d %s samples, want 500", n, k)
		}
	}
	for _, want := range []string{"cancels sent: 500\n", "cancel to reply", "order to canceled"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("no %q in the report:\n%s", want, stdout)
		}
	}
	checkVerdict(t, stdout, r)
}

// With -http, a run of R1 or R2 prints that the counter rose, by 0 as nothing
// trades, so that the report shows the check ran; without it the report has no
// counter text.
func TestRunOEFlowsPrintTheCounterRead(t *testing.T) {
	for _, flowFlag := range []string{"-ioc", "-ack-cancel"} {
		s := newFakeStream(t)
		stdout, r := runOEReport(t, flowFlag, "-symbol", fakeSymbol, "-http", s.addr)
		if !r.CounterRead || r.Trades.Metric != 0 || !strings.Contains(stdout, "trades: 0 in replies, counter rose 0\n") {
			t.Errorf("%s -http: counter read %v, rise %d, report:\n%s", flowFlag, r.CounterRead, r.Trades.Metric, stdout)
		}
		checkVerdict(t, stdout, r)

		stdout, r = runOEReport(t, flowFlag, "-symbol", fakeSymbol)
		if r.CounterRead || strings.Contains(stdout, "counter") {
			t.Errorf("%s without -http: counter read %v, report:\n%s", flowFlag, r.CounterRead, stdout)
		}
	}
}

// loadgen -oe -orders -push -http reads the stream and the counter of the
// engine at -http and reports flow T beside the rows of R3.
func TestRunOEPushWritesReport(t *testing.T) {
	s := newFakeStream(t)
	stdout, r := runOEReportWith(t, fakeOE{trade: s.trade}, "-orders", "-symbol", fakeSymbol, "-push", "-http", s.addr)
	tc := r.Trades
	if r.Mode != "oe_orders" || tc.Replies == 0 || tc.Pushes != tc.Replies || tc.Metric != tc.Replies || r.Latency["trade_push"].Count != r.Outcomes["order_traded"] {
		t.Errorf("report %+v", r)
	}
	for _, want := range []string{fmt.Sprintf("trades: %d in replies, %d pushed, counter rose %d\n", tc.Replies, tc.Pushes, tc.Metric),
		"order to trade push", "request to reply"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("no %q in the report:\n%s", want, stdout)
		}
	}
	checkVerdict(t, stdout, r)
}

// checkVerdict checks that a printed report ends with the verdict of r, and
// that r is valid but for a missed rate.
func checkVerdict(t *testing.T, stdout string, r report) {
	t.Helper()
	rateAside(t, r)
	verdict := "valid\n"
	if !r.Valid {
		verdict = "invalid: " + strings.Join(r.Problems, "; ") + "\n"
	}
	if !strings.HasSuffix(stdout, verdict) {
		t.Errorf("report does not end with %q:\n%s", verdict, stdout)
	}
}
