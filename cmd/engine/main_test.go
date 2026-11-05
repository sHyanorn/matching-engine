package main

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	symbol = "BTC-USD"

	// waitFor bounds every wait for something asynchronous.
	waitFor = 10 * time.Second
)

type orderStatus struct {
	OrderID      uint64 `json:"order_id"`
	Symbol       string `json:"symbol"`
	ClientID     string `json:"client_id"`
	Side         string `json:"side"`
	Type         string `json:"type"`
	TIF          string `json:"tif"`
	Price        int64  `json:"price"`
	Qty          int64  `json:"qty"`
	FilledQty    int64  `json:"filled_qty"`
	RemainingQty int64  `json:"remaining_qty"`
	Status       string `json:"status"`
	Reason       string `json:"reason"`
	TS           int64  `json:"ts"`
}

type event struct {
	Seq  uint64 `json:"seq"`
	Type string `json:"type"`
}

type orderReply struct {
	Order  orderStatus `json:"order"`
	Events []event     `json:"events"`
}

type level struct {
	Price  int64 `json:"price"`
	Qty    int64 `json:"qty"`
	Orders int   `json:"orders"`
}

type bookReply struct {
	Symbol string  `json:"symbol"`
	Seq    uint64  `json:"seq"`
	Bids   []level `json:"bids"`
	Asks   []level `json:"asks"`
}

// instance is one call of run, serving on a loopback port.
type instance struct {
	base   string // http://host:port
	cancel context.CancelFunc
	done   chan struct{}
	err    error // run's result; set before done is closed
}

// start calls run on dir, with any extra flags, in a goroutine and waits
// for its listening line. The instance is stopped when the test ends.
func start(t *testing.T, dir string, flags ...string) *instance {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	in := &instance{cancel: cancel, done: make(chan struct{})}
	pr, pw := io.Pipe()
	go func() {
		in.err = run(ctx, append([]string{"-addr", "127.0.0.1:0", "-data", dir, "-symbols", symbol}, flags...), pw)
		_ = pw.Close() // a pipe writer's Close never fails
		close(in.done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-in.done:
		case <-time.After(waitFor):
			t.Errorf("run did not return within %v of its context being canceled", waitFor)
		}
	})

	lines := make(chan string, 1)
	go func() {
		br := bufio.NewReader(pr)
		line, _ := br.ReadString('\n') // a short line is reported below
		lines <- line
		_, _ = io.Copy(io.Discard, br) // never block run on stdout
	}()
	var line string
	select {
	case line = <-lines:
	case <-time.After(waitFor):
		t.Fatalf("run wrote no line to stdout within %v", waitFor)
	}
	addr, ok := strings.CutPrefix(line, "listening on ")
	addr, nl := strings.CutSuffix(addr, "\n")
	if !nl {
		// The line ended at EOF: only run's return closes the pipe, and
		// done is closed right after, so its error is about to be known.
		select {
		case <-in.done:
			t.Fatalf("stdout %q, want \"listening on HOST:PORT\\n\"; run returned %v", line, in.err)
		case <-time.After(waitFor):
			t.Fatalf("stdout %q, want \"listening on HOST:PORT\\n\"", line)
		}
	}
	if !ok {
		t.Fatalf("stdout %q, want \"listening on HOST:PORT\\n\"", line)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host != "127.0.0.1" || port == "" || port == "0" {
		t.Fatalf("listening on %q, want 127.0.0.1 and the port actually bound", addr)
	}
	in.base = "http://" + addr
	return in
}

// stop cancels run's context and returns its result.
func (in *instance) stop(t *testing.T) error {
	t.Helper()
	in.cancel()
	select {
	case <-in.done:
		return in.err
	case <-time.After(waitFor):
		t.Fatalf("run did not return within %v of its context being canceled", waitFor)
		return nil
	}
}

// newClient returns a client that never uses a proxy and bounds every
// request by 3*waitFor. With keepAlive false every request has a
// connection of its own.
func newClient(t *testing.T, keepAlive bool) *http.Client {
	t.Helper()
	tr := &http.Transport{DisableKeepAlives: !keepAlive}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 3 * waitFor}
}

// outcome is what one request got: a status and body, or a transport
// error and no response.
type outcome struct {
	status int
	body   []byte
	err    error
}

// post never touches t, so any goroutine may call it.
func post(c *http.Client, base, body string) outcome {
	resp, err := c.Post(base+"/v1/orders", "application/json", strings.NewReader(body))
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

func get(t *testing.T, c *http.Client, url string) outcome {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close() // the read error, if any, is the one to report
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return outcome{status: resp.StatusCode, body: b}
}

func decode(t *testing.T, o outcome, v any) {
	t.Helper()
	if o.status != http.StatusOK {
		t.Fatalf("status %d, want 200; body %s", o.status, o.body)
	}
	if err := json.Unmarshal(o.body, v); err != nil {
		t.Fatalf("decoding %s: %v", o.body, err)
	}
}

// errorCode returns the code of an error reply, or "" if body is not one.
func errorCode(body []byte) string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		return ""
	}
	return e.Error.Code
}

func orderJSON(side string, price, qty int64, clientID string) string {
	return fmt.Sprintf(`{"symbol":%q,"side":%q,"type":"limit","price":%d,"qty":%d,"client_id":%q}`,
		symbol, side, price, qty, clientID)
}

func place(t *testing.T, c *http.Client, base, body string) orderReply {
	t.Helper()
	o := post(c, base, body)
	if o.err != nil {
		t.Fatalf("POST %s: %v", body, o.err)
	}
	var r orderReply
	decode(t, o, &r)
	return r
}

func getOrder(t *testing.T, c *http.Client, base string, id uint64) outcome {
	t.Helper()
	return get(t, c, base+"/v1/orders/"+strconv.FormatUint(id, 10))
}

func getBook(t *testing.T, c *http.Client, base string) bookReply {
	t.Helper()
	var b bookReply
	decode(t, get(t, c, base+"/v1/books/"+symbol+"?depth=1000"), &b)
	return b
}

// floodOrder is order i of worker w. Bids stay in [1000, 1099] and asks in
// [9000, 9099], apart from each other and from the maker at 5000, so no
// flood order trades and every acknowledged one rests in full.
func floodOrder(w, i int) string {
	side, price := "buy", int64(1000+(w*7+i)%100)
	if (w+i)%2 == 1 {
		side, price = "sell", int64(9000+(w*7+i)%100)
	}
	return orderJSON(side, price, int64(1+i%5), fmt.Sprintf("w%d-%d", w, i))
}

func waitGroup(t *testing.T, wg *sync.WaitGroup, what string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(waitFor):
		t.Fatalf("%s still running after %v", what, waitFor)
	}
}

// TestRunShutsDownGracefullyAndRestarts cancels run while orders are in
// flight, then starts it again on the same data directory: every
// acknowledged order must survive with its status, and nothing that was
// applied may have gone unacknowledged.
// A shutdown under load loses no acknowledged order and applies none whose
// reply was lost. Keep-alive connections are the hard case: Shutdown closes
// a connection it sees as idle, which can race a request the server has
// just read from it.
func TestRunShutsDownGracefullyAndRestarts(t *testing.T) {
	for _, keepAlive := range []bool{false, true} {
		t.Run(fmt.Sprintf("keepAlive=%v", keepAlive), func(t *testing.T) { testShutdownUnderLoad(t, keepAlive) })
	}
}

func testShutdownUnderLoad(t *testing.T, keepAlive bool) {
	const (
		workers = 16
		// perWorker is a bound only; workers stop at the first transport
		// error. Each flood request opens a connection, so it also keeps
		// the total well inside the ephemeral port range.
		perWorker = 1000
		ackTarget = 200 // acknowledged flood orders before ctx is canceled
	)
	dir := t.TempDir()
	first := start(t, dir)
	client := newClient(t, true)
	flood := newClient(t, keepAlive)

	// A partial fill, which the restarted book must reproduce.
	maker := place(t, client, first.base, orderJSON("sell", 5000, 10, "maker"))
	taker := place(t, client, first.base, orderJSON("buy", 5000, 4, "taker"))
	if taker.Order.Status != "filled" {
		t.Fatalf("taker %+v, want filled", taker.Order)
	}
	var makerStatus orderStatus
	decode(t, getOrder(t, client, first.base, maker.Order.OrderID), &makerStatus)
	if makerStatus.Status != "open" || makerStatus.FilledQty != 4 || makerStatus.RemainingQty != 6 {
		t.Fatalf("maker %+v, want open with 4 filled and 6 remaining", makerStatus)
	}

	// Flood the server, and cancel run's context while requests are in
	// flight.
	var (
		mu       sync.Mutex
		outcomes []outcome
		acks     atomic.Int64
		inFlight atomic.Int64
		enough   = make(chan struct{})
		once     sync.Once
		wg       sync.WaitGroup
	)
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perWorker {
				inFlight.Add(1)
				o := post(flood, first.base, floodOrder(w, i))
				inFlight.Add(-1)
				mu.Lock()
				outcomes = append(outcomes, o)
				mu.Unlock()
				switch {
				case o.err != nil:
					return // the server has stopped accepting connections
				case o.status == http.StatusOK:
					if acks.Add(1) == ackTarget {
						once.Do(func() { close(enough) })
					}
				case o.status != http.StatusServiceUnavailable:
					return // reported below
				}
			}
		}()
	}
	select {
	case <-enough:
	case <-time.After(waitFor):
		t.Fatalf("only %d orders acknowledged within %v", acks.Load(), waitFor)
	}
	t.Logf("canceling with %d requests in flight", inFlight.Load())
	if err := first.stop(t); err != nil {
		t.Fatalf("run returned %v after a clean shutdown, want nil", err)
	}
	waitGroup(t, &wg, "flood clients")

	// Every request got 200 or 503 shutting_down (the queue of 4096 cannot
	// fill with 16 clients, so never busy), or failed in transport, in which
	// case nothing was applied.
	var acked []orderReply
	transportErrors := 0
	for _, o := range outcomes {
		switch {
		case o.err != nil:
			transportErrors++
		case o.status == http.StatusOK:
			var r orderReply
			if err := json.Unmarshal(o.body, &r); err != nil {
				t.Fatalf("decoding %s: %v", o.body, err)
			}
			if r.Order.Status != "open" || r.Order.RemainingQty != r.Order.Qty {
				t.Errorf("flood order %+v, want open and unfilled", r.Order)
			}
			acked = append(acked, r)
		case o.status == http.StatusServiceUnavailable:
			if code := errorCode(o.body); code != "shutting_down" {
				t.Errorf("an order got 503 with code %q during shutdown, want shutting_down; body %s", code, o.body)
			}
		default:
			t.Errorf("an order got %d during shutdown, want 200, 503 or a transport error; body %s", o.status, o.body)
		}
	}
	t.Logf("%d requests: %d acknowledged, %d transport errors", len(outcomes), len(acked), transportErrors)

	// Every command the book applied was acknowledged: the events of all
	// 200 replies number the book's events 1..n without gaps.
	all := append([]orderReply{maker, taker}, acked...)
	var seqs []uint64
	ids := map[uint64]bool{}
	for _, r := range all {
		ids[r.Order.OrderID] = true
		for _, e := range r.Events {
			seqs = append(seqs, e.Seq)
		}
	}
	slices.Sort(seqs)
	for i, s := range seqs {
		if s != uint64(i+1) {
			t.Fatalf("acknowledged events: seq %d at position %d of %d, want 1..%d without gaps or repeats",
				s, i+1, len(seqs), len(seqs))
		}
	}
	if len(ids) != len(all) {
		t.Errorf("%d distinct order IDs among %d acknowledged orders", len(ids), len(all))
	}

	// The book the first run left: the maker's remainder plus every
	// acknowledged flood order.
	type levelKey struct {
		side  string
		price int64
	}
	want := map[levelKey]level{{"sell", 5000}: {Price: 5000, Qty: 6, Orders: 1}}
	for _, r := range acked {
		k := levelKey{r.Order.Side, r.Order.Price}
		l := want[k]
		l.Price, l.Qty, l.Orders = k.price, l.Qty+r.Order.Qty, l.Orders+1
		want[k] = l
	}
	var wantBids, wantAsks []level
	for k, l := range want {
		if k.side == "buy" {
			wantBids = append(wantBids, l)
		} else {
			wantAsks = append(wantAsks, l)
		}
	}
	slices.SortFunc(wantBids, func(a, b level) int { return cmp.Compare(b.Price, a.Price) })
	slices.SortFunc(wantAsks, func(a, b level) int { return cmp.Compare(a.Price, b.Price) })

	// A second run on the same directory serves the same book.
	second := start(t, dir)
	bk := getBook(t, client, second.base)
	if bk.Seq != uint64(len(seqs)) {
		t.Errorf("restarted book seq %d, want %d", bk.Seq, len(seqs))
	}
	if !slices.Equal(bk.Bids, wantBids) {
		t.Errorf("restarted bids\n got %+v\nwant %+v", bk.Bids, wantBids)
	}
	if !slices.Equal(bk.Asks, wantAsks) {
		t.Errorf("restarted asks\n got %+v\nwant %+v", bk.Asks, wantAsks)
	}

	// Resting orders keep their exact status.
	check := func(want orderStatus) {
		t.Helper()
		var got orderStatus
		decode(t, getOrder(t, client, second.base, want.OrderID), &got)
		if got != want {
			t.Errorf("order after restart\n got %+v\nwant %+v", got, want)
		}
	}
	check(makerStatus)
	for _, r := range acked {
		check(r.Order)
	}
	// A finished order may be forgotten (404 unknown_order), but if it is
	// remembered its status is unchanged.
	if o := getOrder(t, client, second.base, taker.Order.OrderID); o.status == http.StatusNotFound {
		if errorCode(o.body) != "unknown_order" {
			t.Errorf("filled order after restart: 404 %s, want code unknown_order", o.body)
		}
	} else {
		check(taker.Order)
	}

	// The restarted engine accepts orders and never reuses an ID.
	fresh := place(t, client, second.base, orderJSON("buy", 1500, 1, "fresh"))
	if fresh.Order.Status != "open" {
		t.Errorf("new order after restart %+v, want open", fresh.Order)
	}
	if ids[fresh.Order.OrderID] {
		t.Errorf("new order after restart reuses ID %d", fresh.Order.OrderID)
	}
	if err := second.stop(t); err != nil {
		t.Fatalf("second run returned %v, want nil", err)
	}
}

// A client that stalls in the middle of a request delays shutdown by at
// most the server's read timeout, and run still returns nil.
func TestShutdownWithStalledRequest(t *testing.T) {
	in := start(t, t.TempDir(), "-shutdown-timeout", "1m")
	conn, err := net.Dial("tcp", strings.TrimPrefix(in.base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := io.WriteString(conn, "POST /v1/orders HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\n{"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // the handler is now waiting for the body
	if err := in.stop(t); err != nil {
		t.Fatalf("run returned %v, want nil", err)
	}
}

func TestPprofServedApart(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String() // free for the engine, almost surely
	_ = l.Close()
	in := start(t, t.TempDir(), "-pprof", addr)
	for _, tc := range []struct {
		url  string
		want int
	}{
		{"http://" + addr + "/debug/pprof/cmdline", http.StatusOK},
		{in.base + "/debug/pprof/cmdline", http.StatusNotFound},
	} {
		resp, err := http.Get(tc.url)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("GET %s: %d, want %d", tc.url, resp.StatusCode, tc.want)
		}
	}
}

func TestHealthcheck(t *testing.T) {
	in := start(t, t.TempDir())
	addr := strings.TrimPrefix(in.base, "http://")
	_, port, _ := net.SplitHostPort(addr)
	for _, a := range []string{addr, ":" + port, "0.0.0.0:" + port} {
		if err := healthcheck([]string{"-addr", a}); err != nil {
			t.Errorf("healthcheck -addr %s against a running engine: %v", a, err)
		}
	}
	if err := in.stop(t); err != nil {
		t.Fatal(err)
	}
	if err := healthcheck([]string{"-addr", addr}); err == nil {
		t.Error("healthcheck succeeded against a stopped engine")
	}
	if err := healthcheck([]string{"-addr", "nonsense"}); err == nil {
		t.Error("healthcheck accepted an address without a port")
	}
	// A zoned IPv6 address must reach the dial, not fail to parse as a URL.
	if err := healthcheck([]string{"-addr", "[fe80::1%lo]:1"}); err == nil || strings.Contains(err.Error(), "invalid URL escape") {
		t.Errorf("healthcheck -addr [fe80::1%%lo]:1: %v, want a dial error", err)
	}
}
