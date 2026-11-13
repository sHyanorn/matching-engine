package api_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/engine"
)

// restBids rests one buy order of qty 1 at each price 1..n of BTC-USD.
func restBids(t *testing.T, env *testEnv, n int) {
	t.Helper()
	for i := range n {
		o := engine.NewOrder{Symbol: btc, Side: book.Buy, Type: book.Limit, TIF: book.GTC, Price: book.Price(i + 1), Qty: 1}
		if _, err := env.eng.PlaceOrder(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMethodNotAllowedListsMethods(t *testing.T) {
	env := newEnv(t, envConfig{})
	res := env.do(t, http.MethodPut, "/v1/orders/5", "")
	expectError(t, res, http.StatusMethodNotAllowed, "method_not_allowed")
	if allow := res.header.Get("Allow"); !strings.Contains(allow, "GET") || !strings.Contains(allow, "DELETE") {
		t.Errorf("Allow %q, want GET and DELETE", allow)
	}
}

// A body over the limit is refused as soon as the limit is reached, and the
// connection is closed rather than read to the end of the declared length.
func TestOversizedBodyRefusedAtOnce(t *testing.T) {
	env := newEnv(t, envConfig{})
	conn, err := net.Dial("tcp", env.http.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	head := "POST /v1/orders HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 100000\r\n\r\n"
	if _, err := io.WriteString(conn, head+"{"+strings.Repeat(" ", 4999)); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(waitFor)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no reply while the rest of the body is unsent: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusRequestEntityTooLarge || !resp.Close {
		t.Errorf("status %d, close %v; want 413 and the connection closed", resp.StatusCode, resp.Close)
	}
}

// Once shutdown begins, orders and cancels are refused and nothing is
// applied, while reads still work.
func TestCommandsRefusedAfterBeginShutdown(t *testing.T) {
	env := newEnv(t, envConfig{})
	resting := env.place(t, limitOrder("buy", 100, 1))
	before := env.metrics(t)
	env.srv.BeginShutdown()

	expectError(t, env.do(t, http.MethodPost, "/v1/orders", limitOrder("sell", 100, 1).body()),
		http.StatusServiceUnavailable, "shutting_down")
	expectError(t, env.do(t, http.MethodDelete, orderPath(resting.Order.OrderID), ""),
		http.StatusServiceUnavailable, "shutting_down")

	bk := env.book(t, btc, "")
	if want := []level{{Price: 100, Qty: 1, Orders: 1}}; bk.Seq != 2 || !slices.Equal(bk.Bids, want) || len(bk.Asks) != 0 {
		t.Errorf("book %+v, want only the resting bid at seq 2", bk)
	}
	env.metricsAfter(t, before,
		rise{"matching_engine_orders_total", []string{"symbol", btc, "result", "failed"}, 1},
		rise{"matching_engine_cancels_total", []string{"symbol", btc, "result", "failed"}, 1})
}

// The snapshot holds every level, so snapshot plus deltas reproduces a
// book deeper than any REST depth.
func TestBookSnapshotHoldsEveryLevel(t *testing.T) {
	env := newEnv(t, envConfig{})
	const n = 1500
	restBids(t, env, n)

	ws := env.stream(t)
	ws.send(t, `{"op":"subscribe","channels":["book:BTC-USD"]}`)
	ws.control(t, "subscribed", bookBTC)
	snap := ws.data(t, bookBTC)
	if len(snap.Bids) != n || snap.Bids[0].Price != n || snap.Bids[n-1].Price != 1 {
		t.Fatalf("snapshot has %d bid levels, want %d from %d down to 1", len(snap.Bids), n, n)
	}

	env.place(t, marketOrder("sell", 1))
	delta := ws.data(t, bookBTC)
	if want := []change{{Side: "buy", Price: n, Qty: 0}}; !slices.Equal(delta.Changes, want) {
		t.Fatalf("delta %+v, want %+v", delta.Changes, want)
	}
	bids := snap.Bids[1:] // the snapshot with the delta applied
	if got := env.book(t, btc, "?depth=1000").Bids; !slices.Equal(got, bids[:1000]) {
		t.Errorf("REST top 1000 differs from the streamed book")
	}
}

// A message over maxQueuedBytes reaches a client that reads, while other
// messages wait to be written.
func TestLargeDeltaReachesReadingClient(t *testing.T) {
	const (
		levels = 25_000 // snapshot about 864 KB; the sweep's delta about 1.19 MB
		tape   = 20_000 // orders on one more level: their trades keep the trades pump busy
		top    = 30_000 // that level's price, above all others
	)
	ctx := context.Background()
	env := newEnv(t, envConfig{})
	restBids(t, env, levels)
	for range tape {
		o := engine.NewOrder{Symbol: btc, Side: book.Buy, Type: book.Limit, TIF: book.GTC, Price: top, Qty: 1}
		if _, err := env.eng.PlaceOrder(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	conn := env.dial(t)
	conn.SetReadLimit(64 << 20)
	writeMsg(t, conn, `{"op":"subscribe","channels":["trades:BTC-USD","book:BTC-USD"]}`)
	for {
		if m := readMsg(t, conn); m.Type == "book_snapshot" {
			if len(m.Bids) != levels+1 {
				t.Fatalf("snapshot has %d bid levels, want %d", len(m.Bids), levels+1)
			}
			break
		}
	}
	type result struct {
		changes int
		err     error
	}
	done := make(chan result, 1)
	go func() {
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				done <- result{err: err}
				return
			}
			if m := parseStreamMsg(data); m.Type == "book_delta" && len(m.Changes) > 1 {
				done <- result{changes: len(m.Changes)}
				return
			}
		}
	}()
	sell := func(qty book.Qty) {
		t.Helper()
		o := engine.NewOrder{Symbol: btc, Side: book.Sell, Type: book.Market, TIF: book.IOC, Qty: qty}
		if _, err := env.eng.PlaceOrder(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	sell(tape)   // 20,000 trade pushes and a one-change delta
	sell(levels) // sweeps 25,000 levels: one delta of about 1.19 MB
	select {
	case r := <-done:
		var ce websocket.CloseError
		switch {
		case errors.As(r.err, &ce):
			t.Fatalf("closed with %d %q before the sweep's book_delta arrived; slow_consumer_bytes = %v",
				ce.Code, ce.Reason, env.metrics(t).value("matching_engine_ws_disconnects_total", "reason", "slow_consumer_bytes"))
		case r.err != nil:
			t.Fatalf("read: %v", r.err)
		case r.changes != levels:
			t.Fatalf("book_delta has %d changes, want %d", r.changes, levels)
		}
	case <-time.After(3 * waitFor):
		t.Fatal("neither the book_delta nor a close arrived")
	}
}

func TestOrderBookPage(t *testing.T) {
	env := newEnv(t, envConfig{})
	res := env.do(t, http.MethodGet, "/", "")
	if res.status != http.StatusOK || !strings.HasPrefix(res.header.Get("Content-Type"), "text/html") || !strings.Contains(string(res.body), "/v1/stream") {
		t.Fatalf("GET / = %d %s, want the order book page", res.status, res.header.Get("Content-Type"))
	}
	expectError(t, env.do(t, http.MethodGet, "/nope", ""), http.StatusNotFound, "not_found")
}
