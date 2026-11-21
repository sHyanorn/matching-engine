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

	"github.com/sHyanorn/matching-engine/internal/oe"
)

// The OE1 mode (-oe) sends Pings over one session and times each from its
// due time to its Pong (D042): the round trip of the network, both
// kernels and the session's reader and writer, without the engine. It is
// the floor that the engine's own latency is set against.
//
// The session is pipelined: requests go out when they are due, whatever
// has been answered. With -spin a pacing thread spins on the clock to send
// them and, on Linux, a reader spins on the socket to stamp each Pong as
// it arrives, each locked to its thread and pinned to a CPU of its own,
// so that neither adds a wake-up to the round trip. Replies are matched by
// client_seq, an index into arrays made before the run.

// maxBatch bounds the Pings put into one write when several are due.
const maxBatch = 1024

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

	// By request: send time minus due time, Pong arrival minus due time,
	// whether the Pong arrived, and, at the first request of each write,
	// the longest gap between two passes of the pacing loop since the last
	// write, the write itself left out: how long the pacer could not run.
	lag, rtt, gap []int64
	got           []bool
	out           []byte // the pacer's buffer
	in            []byte // the reader's buffer
	loggedOut     atomic.Bool
	spinRead      bool   // the reader spun on the socket
	pinned        [2]int // CPUs the pacer and the reader were pinned to; -1: not pinned
	err           error  // that ended the run early
}

// since is the time from t0, on the monotonic clock.
func (r *oeRun) since() int64 { return int64(time.Since(r.t0)) }

// due is when request i is due, from t0.
func (r *oeRun) due(i int) int64 { return int64(float64(i) * r.period) }

// measureOE runs cfg's OE1 measurement.
func measureOE(ctx context.Context, cfg config) (report, error) {
	r, err := runOE(ctx, cfg)
	if err != nil {
		return report{}, err
	}
	return r.report(), nil
}

// runOE sends cfg's Pings and collects their round trips.
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
	r := &oeRun{cfg: cfg, conn: conn, period: float64(time.Second) / cfg.rate}
	r.total = int(cfg.rate * (cfg.warmup + cfg.duration).Seconds())
	r.lag, r.rtt, r.gap = make([]int64, r.total), make([]int64, r.total), make([]int64, r.total)
	r.got = make([]bool, r.total)
	r.out = make([]byte, 0, maxBatch*31)
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

	// The Pongs still on their way get a few seconds: the server sends
	// them before it closes the session on the Logout.
	var readErr error
	select {
	case readErr = <-read:
	case <-time.After(5 * time.Second):
		r.end()
		readErr = <-read
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.err = errors.Join(paced, readErr)
	return r, nil
}

// login opens a session and leaves what follows LoginAccepted in r.in.
func (r *oeRun) login() error {
	if err := r.conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	defer func() { _ = r.conn.SetDeadline(time.Time{}) }()
	if _, err := r.conn.Write(oe.Login{Version: oe.Version}.Append(nil)); err != nil {
		return err
	}
	r.in = make([]byte, 0, 64<<10)
	for {
		t, body, n, err := oe.Next(r.in)
		switch {
		case err != nil:
			return err
		case n > 0 && t == oe.TypeLoginAccepted:
			r.in = r.in[:copy(r.in, r.in[n:])]
			return nil
		case n > 0 && t == oe.TypeLoginRejected:
			return fmt.Errorf("login refused: %v", oe.DecodeLoginRejected(body).Reason)
		case n > 0:
			return fmt.Errorf("got a %q frame before LoginAccepted", byte(t))
		}
		k, err := r.conn.Read(r.in[len(r.in):cap(r.in)])
		r.in = r.in[:len(r.in)+k]
		if err != nil {
			return err
		}
	}
}

// end stops the run and closes the connection, which ends the reader and
// the pacer.
func (r *oeRun) end() {
	r.stop.Store(true)
	_ = r.conn.Close()
}

// pace sends every request when it is due, then a Logout. With -spin it
// spins on the clock, locked to its thread and pinned to cpu; otherwise it
// sleeps until each request is due.
func (r *oeRun) pace(cpu int) error {
	if r.cfg.spin {
		runtime.LockOSThread() // for good: see runOE
		if pin(cpu) {
			r.pinned[0] = cpu
		}
	}
	const heartbeat = int64(time.Second)
	var prev, longest, wrote int64 // wrote: when a frame last went out
	for i := 0; i < r.total; {
		if r.stop.Load() {
			return nil
		}
		now := r.since()
		if r.cfg.spin && prev != 0 {
			longest = max(longest, now-prev)
		}
		prev = now
		if now < r.due(i) {
			if now-wrote >= heartbeat { // docs/API.md: after 1 s without sending
				if _, err := r.conn.Write(oe.Heartbeat{}.Append(r.out[:0])); err != nil {
					r.end()
					return err
				}
				prev, wrote = r.since(), now
			}
			if !r.cfg.spin {
				time.Sleep(time.Duration(min(r.due(i), wrote+heartbeat) - now))
			}
			continue
		}
		if r.cfg.beforeSend != nil {
			r.cfg.beforeSend(i)
		}
		r.gap[i] = longest
		longest = 0
		var err error
		if i, err = r.send(i, now); err != nil {
			r.end()
			return err
		}
		prev = r.since() // the write is not a gap: it is what the loop is for
		wrote = prev
	}
	r.loggedOut.Store(true)
	_, err := r.conn.Write(oe.Logout{}.Append(r.out[:0]))
	return err
}

// send writes request i, due by now, and those after it that are due by
// now too, at most maxBatch, with one write, and returns the next request
// to send.
func (r *oeRun) send(i int, now int64) (int, error) {
	n := i + 1
	for n < r.total && n < i+maxBatch && r.due(n) <= now {
		n++
	}
	r.out = r.out[:0]
	for j := i; j < n; j++ {
		r.out = oe.Ping{ClientSeq: uint64(j + 1)}.Append(r.out)
	}
	sent := r.since()
	for j := i; j < n; j++ {
		r.lag[j] = sent - r.due(j)
	}
	_, err := r.conn.Write(r.out)
	return n, err
}

// read takes Pongs until the server closes the session after the Logout.
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
			return nil // the run is over or canceled; missing Pongs show
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
		switch t {
		case oe.TypePong:
			seq := oe.DecodePong(body).ClientSeq
			if seq == 0 || seq > uint64(r.total) || r.got[seq-1] {
				return fmt.Errorf("got Pong with client_seq %d, want each of 1 to %d once", seq, r.total)
			}
			r.rtt[seq-1] = at - r.due(int(seq-1))
			r.got[seq-1] = true
		case oe.TypeSymbol, oe.TypeHeartbeat:
		default:
			return fmt.Errorf("%q frame, want Pong, Symbol or Heartbeat", byte(t))
		}
		b = b[n:]
	}
	r.in = r.in[:copy(r.in, b)]
	return nil
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
	var rtt, lag, gap []int64
	sent, missing := 0, 0
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
		if r.gap[j] != 0 {
			gap = append(gap, r.gap[j])
		}
	}
	rep.Outcomes["pong"] = len(rtt)
	rep.Achieved = float64(sent) / cfg.duration.Seconds()
	if rep.Achieved < 0.99*cfg.rate {
		rep.Problems = append(rep.Problems, fmt.Sprintf("achieved %.0f/s, below 99%% of the target", rep.Achieved))
	}
	if missing > 0 {
		rep.Problems = append(rep.Problems, fmt.Sprintf("Pings with no Pong: %d", missing))
	}
	if r.err != nil {
		rep.Problems = append(rep.Problems, r.err.Error())
	}
	rep.Latency["pong"] = summarize(rtt)
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
