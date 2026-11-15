package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sHyanorn/matching-engine/internal/oe"
)

// fakeOE serves one OE1 session: it accepts the Login, answers each Ping
// with its Pong delay after the Ping arrived, and closes on the Logout.
// After the Ping numbered stallAt it reads nothing for stall, the Ping
// numbered drop it never answers, and after the Ping numbered garbleAt it
// sends a malformed frame. With no delay it writes all the Pongs that are
// ready with one write, as the engine's server does.
type fakeOE struct {
	delay      time.Duration
	stallAt    uint64
	stall      time.Duration
	drop       uint64
	garbleAt   uint64
	heartbeats *atomic.Int32 // counts the client's Heartbeats, if set
}

func (f fakeOE) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	type pong struct {
		seq uint64
		at  time.Time
	}
	pongs := make(chan pong, 1<<20)
	written := make(chan struct{})
	go func() {
		defer close(written)
		var b []byte
		for p := range pongs {
			time.Sleep(time.Until(p.at.Add(f.delay)))
			b = oe.Pong{ClientSeq: p.seq}.Append(b[:0])
		more:
			for f.delay == 0 { // with a delay, each Pong waits for its own time
				select {
				case p, ok := <-pongs:
					if !ok {
						break more
					}
					b = oe.Pong{ClientSeq: p.seq}.Append(b)
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
		close(pongs)
		<-written
	}()
	var buf []byte
	tmp := make([]byte, 4096)
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
			switch t {
			case oe.TypeLogin:
				if _, err := c.Write(oe.LoginAccepted{Session: 1, NextServerSeq: 1}.Append(nil)); err != nil {
					return
				}
			case oe.TypePing:
				seq := oe.DecodePing(body).ClientSeq
				if seq != f.drop {
					pongs <- pong{seq, at}
				}
				if seq == f.stallAt {
					time.Sleep(f.stall)
				}
				if seq == f.garbleAt {
					if _, err := c.Write([]byte{0xff, 0xff, 'x'}); err != nil {
						return
					}
				}
			case oe.TypeHeartbeat:
				if f.heartbeats != nil {
					f.heartbeats.Add(1)
				}
			case oe.TypeLogout:
				return
			default:
			}
		}
	}
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

// discardConn takes every write.
type discardConn struct{ net.Conn }

func (discardConn) Write(b []byte) (int, error) { return len(b), nil }

func TestOELoopsAllocateNothing(t *testing.T) {
	r := &oeRun{cfg: config{rate: 10_000}, conn: discardConn{}, period: 1e5, total: 10_000}
	r.lag, r.rtt, r.gap, r.got = make([]int64, r.total), make([]int64, r.total), make([]int64, r.total), make([]bool, r.total)
	r.out, r.in = make([]byte, 0, maxBatch*31), make([]byte, 0, 4096)
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

func TestRunOEWritesReport(t *testing.T) {
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
			fakeOE{}.serve(c)
		}
	}()
	out := filepath.Join(t.TempDir(), "r.json")
	var stdout bytes.Buffer
	// The send lag bound is far above this machine's noise: the run is
	// valid unless the tool fails.
	err = run(context.Background(), []string{"-oe", ln.Addr().String(), "-rate", "1000", "-warmup", "0s",
		"-duration", "500ms", "-max-send-lag", "1h", "-out", out}, &stdout)
	if err != nil {
		t.Fatalf("%v\n%s", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), "request to pong") || !strings.Contains(stdout.String(), "\nvalid\n") {
		t.Errorf("report:\n%s", stdout.String())
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var r report
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	if r.Mode != "oe" || r.Outcomes["ping"] != 500 || r.Latency["pong"].Count != 500 || r.Latency["send_lag"].Count != 500 {
		t.Errorf("report %s", data)
	}
}
