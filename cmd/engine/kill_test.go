package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// buildEngine compiles this command into a temporary directory.
func buildEngine(t *testing.T) string {
	t.Helper()
	gotool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command to build the engine with")
	}
	bin := filepath.Join(t.TempDir(), "engine")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command(gotool, "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// process is the engine running as a child process.
type process struct {
	cmd    *exec.Cmd
	base   string // http://host:port
	stderr bytes.Buffer
}

// startProcess runs the engine binary on dir and waits for its listening
// line. An engine that finds dir still locked is started again: Windows
// releases the lock of a killed process some time after it exits. The
// process is killed when the test ends, if it still runs, and its stderr
// logged if the test failed.
func startProcess(t *testing.T, bin, dir string) *process {
	t.Helper()
	for deadline := time.Now().Add(waitFor); ; time.Sleep(50 * time.Millisecond) {
		p, line := tryStart(t, bin, dir)
		if addr, ok := strings.CutPrefix(strings.TrimSpace(line), "listening on "); ok {
			p.base = "http://" + addr
			return p
		}
		p.kill(t) // it has exited, or it hangs before listening
		if !strings.Contains(p.stderr.String(), "in use by another engine") || time.Now().After(deadline) {
			t.Fatalf("stdout %q, want \"listening on HOST:PORT\"", line)
		}
	}
}

// tryStart starts the engine and returns its first line of stdout, or what
// it wrote before it exited or waitFor passed.
func tryStart(t *testing.T, bin, dir string) (*process, string) {
	t.Helper()
	p := &process{cmd: exec.Command(bin, "-addr", "127.0.0.1:0", "-data", dir, "-symbols", symbol)}
	p.cmd.Stderr = &p.stderr
	stdout, err := p.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p.kill(t)
		if t.Failed() {
			t.Logf("stderr of engine %d:\n%s", p.cmd.Process.Pid, &p.stderr)
		}
	})
	lines := make(chan string, 1)
	go func() {
		br := bufio.NewReader(stdout)
		line, _ := br.ReadString('\n')
		lines <- line
		_, _ = br.WriteTo(io.Discard) // until the process exits
	}()
	select {
	case line := <-lines:
		return p, line
	case <-time.After(waitFor):
		return p, ""
	}
}

// kill ends the process at once, as a crash would: SIGKILL on Unix,
// TerminateProcess on Windows. Nothing runs in it afterwards, so no
// buffer is flushed and no log is synced or closed.
func (p *process) kill(t *testing.T) {
	t.Helper()
	if p.cmd.ProcessState != nil {
		return
	}
	if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("kill: %v", err)
	}
	_ = p.cmd.Wait() // reports the kill
}

func deleteOrder(c *http.Client, base string, id uint64) outcome {
	req, err := http.NewRequest(http.MethodDelete, base+"/v1/orders/"+strconv.FormatUint(id, 10), nil)
	if err != nil {
		return outcome{err: err}
	}
	resp, err := c.Do(req)
	if err != nil {
		return outcome{err: err}
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close() // the read error, if any, is the one to report
	if err != nil {
		return outcome{err: err}
	}
	return outcome{status: resp.StatusCode, body: b}
}

// killWorker is one client of TestKillUnderLoad. It sends one request at
// a time, new orders on its own price level and cancels of its own open
// orders, so it alone changes that level, and only its last request,
// which got no reply, has an unknown outcome.
type killWorker struct {
	side  string
	price int64

	acked    []orderReply          // new orders answered 200, in order
	canceled map[uint64]orderReply // cancels answered 200, by order ID
	events   int                   // in all replies answered 200

	// The request in flight when the engine was killed, if any: a new
	// order of pendingQty, or the cancel of pendingCancel.
	pending       bool
	pendingQty    int64
	pendingCancel uint64

	unexpected string // the first reply that was neither 200 nor a transport error
}

func (w *killWorker) run(c *http.Client, base string, n int, acks *atomic.Int64) {
	var open []uint64
	for i := range n {
		if i%4 == 3 && len(open) > 0 {
			id := open[0]
			o := deleteOrder(c, base, id)
			if o.err != nil {
				w.pending, w.pendingCancel = true, id
				return
			}
			var r orderReply
			if o.status != http.StatusOK || json.Unmarshal(o.body, &r) != nil {
				w.unexpected = fmt.Sprintf("DELETE %d: %d %s", id, o.status, o.body)
				return
			}
			w.canceled[id], w.events = r, w.events+len(r.Events)
			open = open[1:]
			continue
		}
		qty := int64(1 + i%7)
		o := post(c, base, orderJSON(w.side, w.price, qty, ""))
		if o.err != nil {
			w.pending, w.pendingQty = true, qty
			return
		}
		var r orderReply
		if o.status != http.StatusOK || json.Unmarshal(o.body, &r) != nil {
			w.unexpected = fmt.Sprintf("POST: %d %s", o.status, o.body)
			return
		}
		w.acked, w.events = append(w.acked, r), w.events+len(r.Events)
		open = append(open, r.Order.OrderID)
		acks.Add(1)
	}
}

// TestKillUnderLoad kills the engine process while orders and cancels are
// in flight, restarts it on the same data directory and checks that every
// acknowledged command survived (D017, D031): each order has the status
// its last reply gave, and the book is exactly what the acknowledged
// commands build, plus whatever each worker's one unanswered request did.
// It does not cover a machine crash, which can lose the last unsynced
// interval.
func TestKillUnderLoad(t *testing.T) {
	const (
		workers   = 16
		perWorker = 100_000 // a bound only; the kill stops the workers
		ackTarget = 2000    // acknowledged orders before the kill
	)
	bin := buildEngine(t)
	dir := t.TempDir()
	first := startProcess(t, bin, dir)
	client := newClient(t, true)

	// A partial fill and a user cancel, so finished statuses are checked
	// too.
	maker := place(t, client, first.base, orderJSON("sell", 5000, 10, "maker"))
	taker := place(t, client, first.base, orderJSON("buy", 5000, 4, "taker"))
	gone := place(t, client, first.base, orderJSON("buy", 2000, 3, "gone"))
	var goneCanceled orderReply
	decode(t, deleteOrder(client, first.base, gone.Order.OrderID), &goneCanceled)
	var makerStatus orderStatus
	decode(t, getOrder(t, client, first.base, maker.Order.OrderID), &makerStatus)
	setupEvents := len(maker.Events) + len(taker.Events) + len(gone.Events) + len(goneCanceled.Events)

	// Bids at 1000+w and asks at 9000+w never trade with each other or
	// with the maker at 5000.
	ws := make([]*killWorker, workers)
	var acks atomic.Int64
	var wg sync.WaitGroup
	for i := range ws {
		w := &killWorker{side: "buy", price: 1000 + int64(i), canceled: map[uint64]orderReply{}}
		if i%2 == 1 {
			w.side, w.price = "sell", 9000+int64(i)
		}
		ws[i] = w
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.run(newClient(t, true), first.base, perWorker, &acks)
		}()
	}
	for deadline := time.Now().Add(waitFor); acks.Load() < ackTarget; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("only %d orders acknowledged within %v", acks.Load(), waitFor)
		}
	}
	first.kill(t)
	waitGroup(t, &wg, "workers")
	inFlight := 0
	for _, w := range ws {
		if w.unexpected != "" {
			t.Fatalf("worker on %s %d: %s", w.side, w.price, w.unexpected)
		}
		if w.pending {
			inFlight++
		}
	}

	second := startProcess(t, bin, dir)
	check := func(want orderStatus) orderStatus {
		t.Helper()
		var got orderStatus
		decode(t, getOrder(t, client, second.base, want.OrderID), &got)
		if got != want {
			t.Errorf("order after restart\n got %+v\nwant %+v", got, want)
		}
		return got
	}
	check(makerStatus)
	check(taker.Order)
	check(goneCanceled.Order)

	bk := getBook(t, client, second.base)
	levels := map[string]level{}
	for _, l := range append(bk.Bids, bk.Asks...) {
		levels[strconv.FormatInt(l.Price, 10)] = l
	}
	wantSeq := uint64(setupEvents)
	wantLevels := map[string]level{"5000": {Price: 5000, Qty: 6, Orders: 1}}
	applied := 0 // unanswered requests that took effect
	for _, w := range ws {
		wantSeq += uint64(w.events)
		var l level
		for _, r := range w.acked {
			id := r.Order.OrderID
			if c, ok := w.canceled[id]; ok {
				check(c.Order)
				continue
			}
			if w.pending && w.pendingCancel == id {
				// The unanswered cancel took effect or it did not; the
				// order says which, and must be one or the other.
				var got orderStatus
				decode(t, getOrder(t, client, second.base, id), &got)
				if got.Status == "canceled" {
					applied++
					wantSeq++
					canceled := r.Order
					canceled.Status, canceled.Reason, canceled.RemainingQty = "canceled", "user_cancel", 0
					check(canceled)
					continue
				}
			}
			check(r.Order)
			l.Qty += r.Order.Qty
			l.Orders++
		}
		// An unanswered new order rests at the worker's level if it took
		// effect; the level says which.
		key := strconv.FormatInt(w.price, 10)
		if got := levels[key]; w.pending && w.pendingQty > 0 && got.Qty == l.Qty+w.pendingQty && got.Orders == l.Orders+1 {
			applied++
			wantSeq += 2 // accepted, rested
			l.Qty, l.Orders = got.Qty, got.Orders
		}
		if l.Orders > 0 {
			l.Price = w.price
			wantLevels[key] = l
		}
	}
	for k, l := range wantLevels {
		if levels[k] != l {
			t.Errorf("level %s after restart: %+v, want %+v", k, levels[k], l)
		}
	}
	for k, l := range levels {
		if _, ok := wantLevels[k]; !ok {
			t.Errorf("unexpected level %+v after restart", l)
		}
	}
	if bk.Seq != wantSeq {
		t.Errorf("book seq %d after restart, want %d", bk.Seq, wantSeq)
	}
	t.Logf("%d orders acknowledged, %d requests in flight at the kill, %d of them took effect", acks.Load(), inFlight, applied)

	// No ID issued before the kill is issued again.
	fresh := place(t, client, second.base, orderJSON("buy", 1500, 1, "fresh"))
	issued := []orderReply{maker, taker, gone}
	for _, w := range ws {
		issued = append(issued, w.acked...)
	}
	if slices.ContainsFunc(issued, func(r orderReply) bool { return r.Order.OrderID == fresh.Order.OrderID }) {
		t.Errorf("new order after restart reuses ID %d", fresh.Order.OrderID)
	}
}
