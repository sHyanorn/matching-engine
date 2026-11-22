package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/flow"
	"github.com/sHyanorn/matching-engine/internal/oe"
)

// The OE1 mode (-oe) sends Pings over one session and times each from its
// due time to its Pong (D042): the round trip of the network, both
// kernels and the session's reader and writer, without the engine. It is
// the floor that the engine's own latency is set against. With -orders it
// sends the SPEC §9 flow instead, as the REST mode does, and times each
// NewOrder and Cancel from its due time to its Done (D046). With -ioc it
// sends IOC orders that cancel at once, and with -ack-cancel orders that
// are each canceled once their Done is in (oe_flows.go).
//
// The session is pipelined: requests go out when they are due, whatever
// has been answered. With -spin a pacing thread spins on the clock to send
// them and, on Linux, a reader spins on the socket to stamp each reply as
// it arrives, each locked to its thread and pinned to a CPU of its own,
// so that neither adds a wake-up to the round trip. Replies are matched by
// client_seq, an index into arrays made before the run.

// maxBatch bounds the frames put into one write when several are due.
const maxBatch = 1024

// outcome is how a command of the flow ended, named as classify names the
// REST answer its Done stands for (worker.go).
type outcome uint8

const (
	noDone outcome = iota // sent and not answered
	orderRested
	orderTraded
	orderCanceled // an IOC order that was canceled at once
	cancelCanceled
	cancelFinished
	cancelUnknown
	commandBusy
	commandError
)

var outcomeNames = [...]string{
	orderRested: "order_rested", orderTraded: "order_traded", orderCanceled: "order_canceled",
	cancelCanceled: "cancel_canceled", cancelFinished: "cancel_finished", cancelUnknown: "cancel_unknown",
	commandBusy: "busy", commandError: "error",
}

// failed reports whether o fails the run, as a REST request answered busy
// or with an error does.
func (o outcome) failed() bool { return o == commandBusy || o == commandError }

// command is a NewOrder or Cancel that went out: the request it was, and
// for a new order its number in the flow, 0 for a cancel.
type command struct{ request, order int32 }

// errStopped ends a spinning read that the run no longer waits for.
var errStopped = errors.New("loadgen: read stopped")

// cpus are CPUs to pin the pacer and the reader to.
type cpus []int

// next hands out the next CPU, or -1 when there is none.
func (c *cpus) next() int {
	if len(*c) == 0 {
		return -1
	}
	cpu := (*c)[0]
	*c = (*c)[1:]
	return cpu
}

// oeRun is the state of one OE1 measurement.
type oeRun struct {
	cfg    config
	conn   net.Conn
	t0     time.Time
	period float64 // ns between requests
	total  int
	stop   atomic.Bool // set when the run is over or canceled

	// By request: send time minus due time and, at the first request of
	// each call of send, the longest gap between two passes of the pacing
	// loop since the call before, its write left out: how long the pacer
	// could not run. By client_seq: reply arrival minus due time, and
	// whether the Pong arrived. Pings are numbered as their requests are.
	// With -ack-cancel rtt of a Cancel is its reply arrival minus its send
	// time.
	lag, gap, rtt []int64
	got           []bool
	out           []byte // the pacer's buffer
	in            []byte // the reader's buffer
	loggedOut     atomic.Bool
	spinRead      bool   // the reader spun on the socket
	pinned        [2]int // CPUs the pacer and the reader were pinned to; -1: not pinned
	err           error  // that ended the run early

	// The order flows. A cancel of the -orders flow whose order has no ID
	// yet is not sent and takes no client_seq, so the pacer notes each
	// command it sends in cmds and publishes their number in sent before
	// the write that carries them: the reader looks at cmds only below
	// sent.
	gen     *flow.Generator
	symbol  uint8           // index of cfg.symbol, from its Symbol frame
	ids     []atomic.Uint64 // order ID by flow order number, once its Done is in
	cmds    []command       // by client_seq
	sent    atomic.Int64
	nsent   int // the pacer's own count of cmds
	skipped int // cancels of the window not sent

	// With -ack-cancel: when the Cancel of each order went out, by request
	// (0: not sent), and the queue of the order numbers whose Done said
	// that they rest, from the reader, the only producer, to the pacer, the
	// only consumer. The reader writes cq[n] and then publishes n+1 in
	// queued; the pacer has taken the first taken. A Done of an order is
	// counted in answered after its order is queued.
	cancelAt []int64
	cq       []int32
	queued   atomic.Int64
	taken    int
	answered atomic.Int64

	// The reader's: each command's outcome by client_seq, the last
	// server_seq, the Events since the last Done (how many, their
	// client_seq, the trades among them and the kinds of the first two),
	// the trade Events of the whole run, counted and as the sum of their
	// hashes (check.go), and the first command that failed.
	res                 []outcome
	serverSeq           uint64
	events, eventTrades int
	eventsOf            uint64
	kinds               [2]book.EventKind
	trades              int
	tradeSum            uint64
	firstFailed         oe.Done
	firstFound          string // what the first failed command of -ioc or -ack-cancel answered
}

// since is the time from t0, on the monotonic clock.
func (r *oeRun) since() int64 { return int64(time.Since(r.t0)) }

func (r *oeRun) due(i int) int64 { return int64(float64(i) * r.period) }

// newOERun makes the run of cfg on conn with every array the run fills, so
// that nothing allocates once it has begun. Requests are numbered from 0 and
// client_seqs from 1; with -ack-cancel there are two client_seqs for each
// order, its own and its Cancel's.
func newOERun(cfg config, conn net.Conn) *oeRun {
	r := &oeRun{cfg: cfg, conn: conn, period: float64(time.Second) / cfg.rate}
	r.total = int(cfg.rate * (cfg.warmup + cfg.duration).Seconds())
	seqs := r.total
	if cfg.ackCancel {
		seqs = 2 * r.total
	}
	r.lag, r.rtt, r.gap = make([]int64, r.total), make([]int64, seqs), make([]int64, r.total)
	r.got = make([]bool, r.total)
	r.out = make([]byte, 0, maxBatch*31)
	if cfg.orderFlow() {
		r.cmds, r.res = make([]command, seqs), make([]outcome, seqs)
	}
	if cfg.orders {
		r.gen = flow.New(cfg.seed, cfg.sd)
	}
	if cfg.orders || cfg.ackCancel {
		r.ids = make([]atomic.Uint64, r.total+1)
	}
	if cfg.ackCancel {
		r.cancelAt, r.cq = make([]int64, r.total), make([]int32, r.total)
	}
	return r
}

// measureOE runs cfg and describes it. With -http it also checks the run's
// trades against the engine (oe_check.go).
func measureOE(ctx context.Context, cfg config) (report, error) {
	if cfg.httpAddr != "" {
		return measureChecked(ctx, cfg)
	}
	r, err := runOE(ctx, cfg)
	if err != nil {
		return report{}, err
	}
	return r.report(), nil
}

// runOE sends cfg's Pings, or its order flow, and collects the round trips.
func runOE(ctx context.Context, cfg config) (*oeRun, error) {
	dial := cfg.oeDial
	if dial == nil {
		dial = func() (net.Conn, error) { return net.Dial("tcp", cfg.oeAddr) }
	}
	conn, err := dial()
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	r := newOERun(cfg, conn)
	if err := r.login(); err != nil {
		return nil, fmt.Errorf("logging in to %s: %w", cfg.oeAddr, err)
	}

	pacerCPU, readerCPU := -1, -1
	if cfg.spin {
		spare := spareCPUs()
		pacerCPU, readerCPU = spare.next(), spare.next()
	}
	r.pinned = [2]int{-1, -1}
	r.t0 = time.Now()
	// A server that stops reading for good fails the run instead of
	// holding it.
	if err := conn.SetWriteDeadline(r.t0.Add(cfg.warmup + cfg.duration + 10*time.Second)); err != nil {
		return nil, err
	}
	read := make(chan error, 1)
	go func() {
		err := r.read(readerCPU)
		if err != nil {
			r.end() // the pacer stops too
		}
		read <- err
	}()
	stopped := context.AfterFunc(ctx, r.end)
	defer stopped()
	// Each loop runs on a goroutine of its own, which a pinned thread
	// does not outlive: a goroutine that ends locked to its thread ends
	// the thread.
	pace := make(chan error, 1)
	go func() { pace <- r.pace(pacerCPU) }()
	paced := <-pace

	// The replies still on their way get a few seconds: the server sends
	// them before it closes the session on the Logout.
	var readErr error
	select {
	case readErr = <-read:
	case <-time.After(cfg.replyTimeout()):
		r.end()
		readErr = <-read
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.err = errors.Join(paced, readErr)
	return r, nil
}

// login opens a session and leaves in r.in what follows LoginAccepted or,
// for an order flow, what follows cfg.symbol's Symbol frame, whose index
// the orders name: the server sends the Symbol frames before anything
// else, so another frame first means that it has no such symbol.
func (r *oeRun) login() error {
	if err := r.conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	defer func() { _ = r.conn.SetDeadline(time.Time{}) }()
	if _, err := r.conn.Write(oe.Login{Version: oe.Version}.Append(nil)); err != nil {
		return err
	}
	r.in = make([]byte, 0, 64<<10)
	accepted := false
	for {
		t, body, n, err := oe.Next(r.in)
		if err != nil {
			return err
		}
		if n == 0 {
			k, err := r.conn.Read(r.in[len(r.in):cap(r.in)])
			r.in = r.in[:len(r.in)+k]
			if err != nil {
				return err
			}
			continue
		}
		found := false
		switch {
		case !accepted && t == oe.TypeLoginAccepted:
			accepted = true
			found = !r.cfg.orderFlow()
		case !accepted && t == oe.TypeLoginRejected:
			return fmt.Errorf("login refused: %v", oe.DecodeLoginRejected(body).Reason)
		case !accepted:
			return fmt.Errorf("got a %q frame before LoginAccepted", byte(t))
		case t != oe.TypeSymbol:
			return fmt.Errorf("the engine has no symbol %q", r.cfg.symbol)
		default:
			s := oe.DecodeSymbol(body)
			r.symbol, found = s.Index, s.Name == r.cfg.symbol
		}
		r.in = r.in[:copy(r.in, r.in[n:])]
		if found {
			return nil
		}
	}
}

// end stops the run and closes the connection, which ends the reader and
// the pacer.
func (r *oeRun) end() {
	r.stop.Store(true)
	_ = r.conn.Close()
}

// cancelPoll is the longest a sleeping pacer of -ack-cancel waits before it
// looks for a Cancel to send: the reader queues one when an order's Done is
// in, and does not wake the pacer.
const cancelPoll = 50 * time.Microsecond

// pace sends every request when it is due, but for the cancels that
// command skips, then a Logout. With -ack-cancel it also sends each Cancel
// the reader has queued, with the orders that are due or alone, and after
// the last order it waits for the Dones still to come, up to
// cfg.replyTimeout, so that every order that rests is canceled. With -spin
// it spins on the clock, locked to its thread and pinned to cpu; otherwise
// it sleeps until each request is due.
func (r *oeRun) pace(cpu int) error {
	if r.cfg.spin {
		runtime.LockOSThread() // for good: see runOE
		if pin(cpu) {
			r.pinned[0] = cpu
		}
	}
	const heartbeat = int64(time.Second)
	var prev, longest, wrote int64 // wrote: when a frame last went out
	var giveUp int64               // when to stop waiting for the last Dones
	for i := 0; ; {
		if r.stop.Load() {
			return nil
		}
		now := r.since()
		if r.cfg.spin && prev != 0 {
			longest = max(longest, now-prev)
		}
		prev = now
		// Counted before the queue is looked at: a Done queues its Cancel
		// first, so the queue holds every Cancel of the Dones counted.
		answered := r.answered.Load()
		orderDue := i < r.total && now >= r.due(i)
		if !orderDue && r.taken == int(r.queued.Load()) {
			if i == r.total {
				if giveUp == 0 {
					giveUp = now + int64(r.cfg.replyTimeout())
				}
				if !r.cfg.ackCancel || answered == int64(r.total) || now >= giveUp {
					break
				}
			}
			if now-wrote >= heartbeat { // docs/API.md: after 1 s without sending
				if _, err := r.conn.Write(oe.Heartbeat{}.Append(r.out[:0])); err != nil {
					r.end()
					return err
				}
				prev, wrote = r.since(), now
			}
			if !r.cfg.spin {
				wake := wrote + heartbeat
				if i < r.total {
					wake = min(wake, r.due(i))
				}
				if r.cfg.ackCancel {
					wake = min(wake, now+int64(cancelPoll))
				}
				time.Sleep(time.Duration(wake - now))
			}
			continue
		}
		if orderDue {
			if r.cfg.beforeSend != nil {
				r.cfg.beforeSend(i)
			}
			r.gap[i] = longest
			longest = 0
		}
		var err error
		if i, err = r.send(i, now); err != nil {
			r.end()
			return err
		}
		prev = r.since() // the write is not a gap: it is what the loop is for
		// Requests that were all skipped cancels wrote nothing.
		if len(r.out) > 0 {
			wrote = prev
		}
	}
	r.loggedOut.Store(true)
	_, err := r.conn.Write(oe.Logout{}.Append(r.out[:0]))
	return err
}

// send writes the requests from i that are due by now, at most maxBatch,
// with one write, and with -ack-cancel the Cancels that are queued, in the
// room that is left in the write. It returns the next request to send.
func (r *oeRun) send(i int, now int64) (int, error) {
	n := i
	for n < r.total && n < i+maxBatch && r.due(n) <= now {
		n++
	}
	r.out = r.out[:0]
	var cancels int // where the Cancels of this write begin in cmds
	if r.cfg.orderFlow() {
		for j := i; j < n; j++ {
			r.command(j)
		}
		cancels = r.nsent
		if r.cfg.ackCancel {
			r.cancels(maxBatch - (n - i))
		}
	} else {
		for j := i; j < n; j++ {
			r.out = oe.Ping{ClientSeq: uint64(j + 1)}.Append(r.out)
		}
	}
	sent := r.since()
	for j := i; j < n; j++ {
		r.lag[j] = sent - r.due(j)
	}
	for _, c := range r.cmds[cancels:r.nsent] {
		r.cancelAt[c.request] = sent
	}
	if r.cfg.orderFlow() {
		r.sent.Store(int64(r.nsent)) // after the times, which the reader reads
	}
	if len(r.out) == 0 { // every request due was a cancel that is skipped
		return n, nil
	}
	_, err := r.conn.Write(r.out)
	return n, err
}

// command appends the frame of request j, and notes it, as a command of the
// flow chosen. For -orders it is the next op of the flow, and a cancel
// needs its order's ID, which the order's Done carries: one whose order has
// no Done yet, or failed, is skipped, as the REST mode skips it (worker.go),
// so that no send waits for a reply.
func (r *oeRun) command(j int) {
	if r.cfg.ioc {
		r.fixedOrder(j, book.IOC, iocPrice)
		return
	}
	if r.cfg.ackCancel {
		r.fixedOrder(j, book.GTC, ackPrice)
		return
	}
	op := r.gen.Next()
	c, seq := command{request: int32(j)}, uint64(r.nsent+1)
	if op.Cancel {
		id := r.ids[op.Order].Load()
		if id == 0 {
			if r.due(j) >= int64(r.cfg.warmup) {
				r.skipped++
			}
			return
		}
		r.out = oe.Cancel{ClientSeq: seq, OrderID: book.OrderID(id)}.Append(r.out)
	} else {
		c.order = int32(op.Order)
		side := book.Sell
		if op.Buy {
			side = book.Buy
		}
		r.out = oe.NewOrder{ClientSeq: seq, Symbol: r.symbol, Side: side, Type: book.Limit, TIF: book.GTC,
			Price: book.Price(op.Price), Qty: book.Qty(op.Qty)}.Append(r.out)
	}
	r.cmds[r.nsent] = c
	r.nsent++
}

// read takes replies until the server closes the session after the Logout.
// With -spin, where the socket allows it, it spins on the socket, locked
// to its thread and pinned to cpu.
func (r *oeRun) read(cpu int) error {
	readFn := r.conn.Read
	if r.cfg.spin {
		if fn, ok := spinRead(r.conn, &r.stop); ok {
			runtime.LockOSThread() // for good: see runOE
			if pin(cpu) {
				r.pinned[1] = cpu
			}
			readFn, r.spinRead = fn, true
		}
	}
	if err := r.take(r.since()); err != nil { // what came with LoginAccepted
		return err
	}
	for {
		k, err := readFn(r.in[len(r.in):cap(r.in)])
		at := r.since()
		r.in = r.in[:len(r.in)+k]
		if k > 0 {
			if err := r.take(at); err != nil {
				return err
			}
		}
		switch {
		case err == nil:
		case errors.Is(err, io.EOF) && r.loggedOut.Load():
			return nil
		case r.stop.Load():
			return nil // the run is over or canceled; missing replies show
		default:
			return err
		}
	}
}

// take records the whole frames in r.in, which arrived at at.
func (r *oeRun) take(at int64) error {
	b := r.in
	for {
		t, body, n, err := oe.Next(b)
		if err != nil {
			return err
		}
		if n == 0 {
			break
		}
		switch {
		case t == oe.TypeSymbol || t == oe.TypeHeartbeat:
		case t == oe.TypePong && !r.cfg.orderFlow():
			seq := oe.DecodePong(body).ClientSeq
			if seq == 0 || seq > uint64(r.total) || r.got[seq-1] {
				return fmt.Errorf("got Pong with client_seq %d, want each of 1 to %d once", seq, r.total)
			}
			r.rtt[seq-1] = at - r.due(int(seq-1))
			r.got[seq-1] = true
		case t == oe.TypeEvent && r.cfg.orderFlow():
			e := oe.DecodeEvent(body)
			if err := r.numbered(e.ServerSeq, e.ClientSeq); err != nil {
				return err
			}
			if r.events < len(r.kinds) {
				r.kinds[r.events] = e.Kind
			}
			r.events, r.eventsOf = r.events+1, e.ClientSeq
			if e.Kind == book.EventTrade {
				r.eventTrades++
				r.trades++
				r.tradeSum += trade{bookSeq: e.Seq, taker: uint64(e.OrderID), maker: uint64(e.MakerID),
					price: int64(e.Price), qty: int64(e.Qty)}.hash()
			}
		case t == oe.TypeDone && r.cfg.orderFlow():
			if err := r.done(oe.DecodeDone(body), at); err != nil {
				return err
			}
		case r.cfg.orderFlow():
			return fmt.Errorf("%q frame, want Event, Done, Symbol or Heartbeat", byte(t))
		default:
			return fmt.Errorf("%q frame, want Pong, Symbol or Heartbeat", byte(t))
		}
		b = b[n:]
	}
	r.in = r.in[:copy(r.in, b)]
	return nil
}

// numbered checks the numbers of an Event or Done: server_seq rises by one
// from 1, and a command's Events come together, before its Done.
func (r *oeRun) numbered(serverSeq, clientSeq uint64) error {
	if serverSeq != r.serverSeq+1 {
		return fmt.Errorf("got server_seq %d after %d", serverSeq, r.serverSeq)
	}
	r.serverSeq = serverSeq
	if r.events > 0 && clientSeq != r.eventsOf {
		return fmt.Errorf("got a frame of client_seq %d after Events of %d without their Done", clientSeq, r.eventsOf)
	}
	return nil
}

// done records the answer to a command, which arrived at at. A Cancel of
// -ack-cancel is timed from its own send, every other command from the due
// time of its request.
func (r *oeRun) done(d oe.Done, at int64) error {
	if err := r.numbered(d.ServerSeq, d.ClientSeq); err != nil {
		return err
	}
	sent := uint64(r.sent.Load())
	if d.ClientSeq == 0 || d.ClientSeq > sent || r.res[d.ClientSeq-1] != noDone {
		return fmt.Errorf("got Done with client_seq %d, want each of the %d sent once", d.ClientSeq, sent)
	}
	c := r.cmds[d.ClientSeq-1]
	cancel := c.order == 0
	out := r.outcome(c, d)
	from := r.due(int(c.request))
	if cancel && r.cfg.ackCancel {
		from = r.cancelAt[c.request]
	}
	switch {
	case out == orderRested || out == orderTraded:
		r.ids[c.order].Store(uint64(d.OrderID))
		if r.cfg.ackCancel {
			r.queue(c.order)
		}
	case out.failed() && r.firstFailed.ClientSeq == 0:
		r.firstFailed, r.firstFound = d, r.found(c, d)
	}
	if r.cfg.ackCancel && !cancel {
		r.answered.Add(1)
	}
	r.rtt[d.ClientSeq-1] = at - from
	r.res[d.ClientSeq-1] = out
	r.events, r.eventTrades, r.kinds = 0, 0, [2]book.EventKind{}
	return nil
}

// outcome names the Done of a command of the flow chosen.
func (r *oeRun) outcome(c command, d oe.Done) outcome {
	switch {
	case r.cfg.ioc:
		return r.iocOutcome(d)
	case r.cfg.ackCancel:
		return r.ackOutcome(c, d)
	}
	return doneOutcome(c.order == 0, d, r.eventTrades > 0)
}

// doneOutcome names the Done of a new order or a cancel as classify names
// the REST answer with that status (docs/API.md): both kinds of 503 are
// busy, and an answer the flow cannot bring about is an error.
func doneOutcome(cancel bool, d oe.Done, traded bool) outcome {
	switch {
	case d.Status == oe.StatusBusy || d.Status == oe.StatusShuttingDown:
		return commandBusy
	case cancel && d.Status == oe.StatusOK:
		return cancelCanceled
	case cancel && d.Status == oe.StatusOrderFinished:
		return cancelFinished
	case cancel && d.Status == oe.StatusUnknownOrder:
		return cancelUnknown
	case cancel || d.Status != oe.StatusOK || d.OrderID == 0:
		return commandError
	case traded:
		return orderTraded
	}
	return orderRested
}

// report describes the requests due in the measurement window.
func (r *oeRun) report() report {
	cfg := r.cfg
	rep := report{
		Mode: "oe", TargetRate: cfg.rate, Warmup: cfg.warmup.Seconds(), Duration: cfg.duration.Seconds(),
		Conns: 1, Spin: cfg.spin, SpinRead: r.spinRead, Go: runtime.Version(), CPUs: runtime.NumCPU(),
		Outcomes: map[string]int{}, Latency: map[string]summary{},
	}
	if r.pinned != [2]int{-1, -1} {
		rep.Pinned = r.pinned[:]
	}
	start, end := int64(cfg.warmup), int64(cfg.warmup+cfg.duration)
	fill := r.pings
	switch {
	case cfg.ackCancel:
		fill = r.ackCancels
	case cfg.orderFlow():
		fill = r.orders
	}
	sent, lag, problems := fill(&rep)
	var gap []int64
	for j := range r.total {
		if d := r.due(j); d >= start && d < end && r.gap[j] != 0 {
			gap = append(gap, r.gap[j])
		}
	}
	rep.Achieved = float64(sent) / cfg.duration.Seconds()
	if rep.Achieved < 0.99*cfg.rate {
		rep.Problems = append(rep.Problems, fmt.Sprintf("achieved %.0f/s, below 99%% of the target", rep.Achieved))
	}
	rep.Problems = append(rep.Problems, problems...)
	if r.err != nil {
		rep.Problems = append(rep.Problems, r.err.Error())
	}
	rep.Latency["send_lag"] = summarize(lag)
	if cfg.spin {
		rep.Latency["hiccup"] = summarize(gap)
	}
	if p := rep.Latency["send_lag"].P999; p > micros(int64(cfg.maxSendLag)) {
		rep.Problems = append(rep.Problems, fmt.Sprintf("send lag p99.9 %.1f µs, over %v", p, cfg.maxSendLag))
	}
	rep.Valid = len(rep.Problems) == 0
	return rep
}

// pings fills rep for a run of Pings and returns how many of the window's
// answered Pings went out in it, their send lags, and what makes the run
// invalid.
func (r *oeRun) pings(rep *report) (sent int, lag []int64, problems []string) {
	start, end := int64(r.cfg.warmup), int64(r.cfg.warmup+r.cfg.duration)
	var rtt []int64
	missing := 0
	for j := range r.total {
		if !r.got[j] { // the warmup's Pings too, as in D030
			missing++
		}
		if d := r.due(j); d < start || d >= end {
			continue
		}
		rep.Outcomes["ping"]++
		if !r.got[j] {
			continue
		}
		if r.due(j)+r.lag[j] < end {
			sent++
		}
		rtt, lag = append(rtt, r.rtt[j]), append(lag, r.lag[j])
	}
	rep.Outcomes["pong"] = len(rtt)
	if missing > 0 {
		problems = append(problems, fmt.Sprintf("Pings with no Pong: %d", missing))
	}
	rep.Latency["pong"] = summarize(rtt)
	return sent, lag, problems
}

// orders fills rep for a run of the order flow or of R1, as the REST mode
// reports one (worker.go): a command that was answered busy or with an
// error fails the run, the warmup's too, and is left out of the latencies.
func (r *oeRun) orders(rep *report) (sent int, lag []int64, problems []string) {
	cfg := r.cfg
	rep.Symbol = cfg.symbol
	if cfg.ioc {
		rep.Mode = "oe_ioc"
	} else {
		rep.Mode, rep.Seed, rep.SD = "oe_orders", cfg.seed, cfg.sd
	}
	rep.Trades.Replies = r.trades
	start, end := int64(cfg.warmup), int64(cfg.warmup+cfg.duration)
	var reply, service []int64
	missing, failed, warmupFailed := 0, 0, 0
	for seq, c := range r.cmds[:r.nsent] {
		j, out := int(c.request), r.res[seq]
		if out == noDone {
			missing++
		}
		if r.due(j) < start {
			if out.failed() {
				warmupFailed++
			}
			continue
		}
		if r.due(j)+r.lag[j] < end {
			sent++
		}
		if out == noDone {
			continue
		}
		rep.Outcomes[outcomeNames[out]]++
		if out.failed() {
			failed++
			continue
		}
		reply, service, lag = append(reply, r.rtt[seq]), append(service, r.rtt[seq]-r.lag[j]), append(lag, r.lag[j])
	}
	if r.skipped > 0 {
		rep.Outcomes["cancel_skipped"] = r.skipped
	}
	rep.Latency["reply"], rep.Latency["service"] = summarize(reply), summarize(service)
	return sent, lag, r.commandProblems(failed, warmupFailed, missing)
}

// commandProblems says what the commands of an order flow that failed or
// got no Done make wrong with the run.
func (r *oeRun) commandProblems(failed, warmupFailed, missing int) (problems []string) {
	if n := failed + warmupFailed; n > 0 {
		f := r.firstFailed
		problems = append(problems, fmt.Sprintf("%d commands failed, %d of them in the warmup; the first: client_seq %d answered %v%s",
			n, warmupFailed, f.ClientSeq, f.Status, r.firstFound))
	}
	if missing > 0 {
		problems = append(problems, fmt.Sprintf("commands with no Done: %d", missing))
	}
	return problems
}
