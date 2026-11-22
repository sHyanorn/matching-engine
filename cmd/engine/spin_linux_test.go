package main

import (
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/engine"
	"github.com/sHyanorn/matching-engine/internal/oe"
	"github.com/sHyanorn/matching-engine/internal/spin/spintest"
)

// pinnedThreads counts the threads of the process that may run on one CPU
// only, as /proc gives them. The main thread is left out: a goroutine that
// ends locked to it parks it for good instead of ending it, and a goroutine
// may well run there.
func pinnedThreads(t *testing.T) int {
	t.Helper()
	tasks, err := os.ReadDir("/proc/self/task")
	if err != nil {
		t.Skip(err)
	}
	n := 0
	for _, task := range tasks {
		if task.Name() == strconv.Itoa(os.Getpid()) {
			continue
		}
		status, err := os.ReadFile("/proc/self/task/" + task.Name() + "/status")
		if err != nil {
			continue // the thread has exited
		}
		for line := range strings.Lines(string(status)) {
			if list, ok := strings.CutPrefix(line, "Cpus_allowed_list:"); ok {
				if list = strings.TrimSpace(list); !strings.ContainsAny(list, "-,") {
					n++
				}
			}
		}
	}
	return n
}

// TestOESpinServed serves OE1 with -oe-spin: the symbol's goroutine and the
// session's reader and writer pin threads of their own, an order and a
// cancel are answered and reach the book, and shutdown ends the session and
// the threads. At most one of the three may have run on the main thread.
func TestOESpinServed(t *testing.T) {
	spintest.Require(t, 3)
	if n := pinnedThreads(t); n != 0 {
		t.Skipf("%d threads already run on one CPU only", n)
	}
	in := start(t, t.TempDir(), "-oe-addr", "127.0.0.1:0", "-oe-spin")
	conn, err := net.Dial("tcp", in.oe)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	var buf []byte
	first := oeDone(t, conn, &buf, 1, oe.Login{Version: oe.Version}.Append(nil),
		oe.NewOrder{ClientSeq: 1, Symbol: 0, Side: book.Buy, Type: book.Limit, TIF: book.GTC, Price: 1000, Qty: 5}.Append(nil))
	if first.Status != oe.StatusOK || first.State != engine.StateOpen || first.OrderID == 0 {
		t.Fatalf("NewOrder: %+v, want ok and open", first)
	}
	deadline := time.Now().Add(waitFor)
	for n := pinnedThreads(t); n < 2; n = pinnedThreads(t) {
		if time.Now().After(deadline) {
			t.Fatalf("%d threads run on one CPU only, want the 3 of the symbol, the reader and the writer, less one that may be the main thread", n)
		}
		time.Sleep(time.Millisecond)
	}
	d := oeDone(t, conn, &buf, 2, oe.Cancel{ClientSeq: 2, OrderID: first.OrderID}.Append(nil))
	if d.Status != oe.StatusOK || d.State != engine.StateCanceled {
		t.Fatalf("Cancel: %+v", d)
	}
	var st orderStatus
	decode(t, getOrder(t, newClient(t, true), in.base, uint64(first.OrderID)), &st)
	if st.Status != "canceled" {
		t.Fatalf("over REST after the cancel: %+v", st)
	}
	if err := in.stop(t); err != nil {
		t.Fatalf("run returned %v, want nil", err)
	}
	deadline = time.Now().Add(waitFor)
	for n := pinnedThreads(t); n != 0; n = pinnedThreads(t) {
		if time.Now().After(deadline) {
			t.Fatalf("%d threads still run on one CPU only after the engine stopped", n)
		}
		time.Sleep(time.Millisecond)
	}
}
