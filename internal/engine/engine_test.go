package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/sHyanorn/matching-engine/internal/book"
)

// newTestEngine starts an engine with a fixed clock that counts up from 1,
// so every command's timestamp is predictable, and closes it at the end.
func newTestEngine(t *testing.T, cfg Config) *Engine {
	t.Helper()
	if cfg.DataDir == "" {
		cfg.DataDir = t.TempDir()
	}
	if cfg.Clock == nil {
		var mu sync.Mutex
		var now int64
		cfg.Clock = func() int64 {
			mu.Lock()
			defer mu.Unlock()
			now++
			return now
		}
	}
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.Close(); err != nil {
			t.Error(err)
		}
	})
	return e
}

func limitOrder(sym string, s book.Side, p book.Price, q book.Qty) NewOrder {
	return NewOrder{Symbol: sym, Side: s, Type: book.Limit, TIF: book.GTC, Price: p, Qty: q}
}

func TestNewValidatesConfig(t *testing.T) {
	tooMany := make([]string, MaxSymbols+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("S%d", i)
	}
	for _, tc := range []struct {
		name    string
		symbols []string
	}{
		{"no symbols", nil},
		{"too many symbols", tooMany},
		{"lower case", []string{"btc-usd"}},
		{"path separator", []string{"BTC/USD"}},
		{"dot dot", []string{"..X"}},
		{"too long", []string{"ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456"}},
		{"duplicate", []string{"BTC-USD", "BTC-USD"}},
	} {
		if _, err := New(Config{Symbols: tc.symbols, DataDir: t.TempDir()}); err == nil {
			t.Errorf("%s: New accepted %v", tc.name, tc.symbols)
		}
	}
	if _, err := New(Config{Symbols: []string{"A"}}); err == nil {
		t.Error("New accepted a config without DataDir")
	}
	e, err := New(Config{Symbols: []string{"BTC-USD", "ETH_USD", "X.1"}, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPlaceCancelAndQuery(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t, Config{Symbols: []string{"BTC-USD"}})

	sell, err := e.PlaceOrder(ctx, NewOrder{Symbol: "BTC-USD", Side: book.Sell, Type: book.Limit, TIF: book.GTC, Price: 101, Qty: 10, ClientID: "c-1"})
	if err != nil {
		t.Fatal(err)
	}
	want := []book.Event{
		{Seq: 1, Timestamp: 1, Kind: book.EventAccepted, OrderID: sell.OrderID, Side: book.Sell, Price: 101, Qty: 10},
		{Seq: 2, Timestamp: 1, Kind: book.EventRested, OrderID: sell.OrderID, Side: book.Sell, Price: 101, Qty: 10, LevelSide: book.Sell, LevelQty: 10},
	}
	if !slices.Equal(sell.Events, want) {
		t.Fatalf("sell events = %+v\nwant %+v", sell.Events, want)
	}

	buy, err := e.PlaceOrder(ctx, limitOrder("BTC-USD", book.Buy, 102, 4))
	if err != nil {
		t.Fatal(err)
	}
	want = []book.Event{
		{Seq: 3, Timestamp: 2, Kind: book.EventAccepted, OrderID: buy.OrderID, Side: book.Buy, Price: 102, Qty: 4},
		{Seq: 4, Timestamp: 2, Kind: book.EventTrade, OrderID: buy.OrderID, MakerID: sell.OrderID, Side: book.Buy, Price: 101, Qty: 4, LevelSide: book.Sell, LevelQty: 6},
	}
	if !slices.Equal(buy.Events, want) {
		t.Fatalf("buy events = %+v\nwant %+v", buy.Events, want)
	}

	d, err := e.Depth(ctx, "BTC-USD", 10)
	if err != nil {
		t.Fatal(err)
	}
	if d.LastSeq != 4 || d.Bids != nil || !slices.Equal(d.Asks, []book.Level{{Price: 101, Qty: 6, Orders: 1}}) {
		t.Fatalf("depth = %+v", d)
	}
	o, found, err := e.Order(ctx, sell.OrderID)
	if err != nil || !found || o.Remaining != 6 || o.Filled != 4 {
		t.Fatalf("Order = %+v, %v, %v", o, found, err)
	}

	c, err := e.CancelOrder(ctx, sell.OrderID)
	if err != nil {
		t.Fatal(err)
	}
	want = []book.Event{{Seq: 5, Timestamp: 3, Kind: book.EventCanceled, OrderID: sell.OrderID, Side: book.Sell, Price: 101, Qty: 6, Reason: book.ReasonUserCancel, LevelSide: book.Sell}}
	if !slices.Equal(c.Events, want) {
		t.Fatalf("cancel events = %+v\nwant %+v", c.Events, want)
	}
	st, found, _ := e.Order(ctx, sell.OrderID)
	if !found || st.State != StateCanceled || st.Reason != book.ReasonUserCancel || st.Filled != 4 || st.Remaining != 0 || st.ClientID != "c-1" {
		t.Fatalf("canceled order status = %+v, %v", st, found)
	}
	if c.Status.State != StateCanceled || c.Status.ID != sell.OrderID {
		t.Fatalf("cancel result status = %+v", c.Status)
	}
}

func TestOrderIDsCarryTheirSymbol(t *testing.T) {
	ctx := context.Background()
	syms := []string{"A", "B", "C"}
	e := newTestEngine(t, Config{Symbols: syms})
	seen := map[book.OrderID]bool{}
	for round := range 3 {
		for i, s := range syms {
			r, err := e.PlaceOrder(ctx, limitOrder(s, book.Buy, 100, 1))
			if err != nil {
				t.Fatal(err)
			}
			if symbolIndexOf(r.OrderID) != i || seen[r.OrderID] {
				t.Fatalf("round %d, %s: ID %d has symbol index %d (or repeats)", round, s, r.OrderID, symbolIndexOf(r.OrderID))
			}
			seen[r.OrderID] = true
			if want := uint64(round*len(syms) + i + 1); seqOf(r.OrderID) != want {
				t.Fatalf("ID %d has counter %d, want %d", r.OrderID, seqOf(r.OrderID), want)
			}
		}
	}
	// A cancel of B's order goes to B's book.
	b, _ := e.PlaceOrder(ctx, limitOrder("B", book.Sell, 200, 1))
	c, err := e.CancelOrder(ctx, b.OrderID)
	if err != nil || len(c.Events) != 1 || c.Events[0].Kind != book.EventCanceled {
		t.Fatalf("cancel of B's order: %+v, %v", c, err)
	}
}

func TestInvalidAndUnknownRequests(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t, Config{Symbols: []string{"A", "B"}})

	_, err := e.PlaceOrder(ctx, limitOrder("A", book.Buy, 100, 0))
	var inv *InvalidError
	if !errors.As(err, &inv) || inv.Reason != book.ReasonInvalidQty {
		t.Fatalf("qty 0: err = %v", err)
	}
	if _, err := e.PlaceOrder(ctx, limitOrder("Z", book.Buy, 100, 1)); !errors.Is(err, ErrUnknownSymbol) {
		t.Fatalf("unknown symbol: err = %v", err)
	}
	long := NewOrder{Symbol: "A", Side: book.Buy, Type: book.Limit, TIF: book.GTC, Price: 1, Qty: 1, ClientID: string(make([]byte, MaxClientIDLen+1))}
	if _, err := e.PlaceOrder(ctx, long); !errors.Is(err, ErrClientIDLen) {
		t.Fatalf("long client_id: err = %v", err)
	}
	// Nothing above reached a book: Seq did not move.
	if d, _ := e.Depth(ctx, "A", 1); d.LastSeq != 0 {
		t.Fatalf("LastSeq = %d after rejected requests", d.LastSeq)
	}

	if _, err := e.CancelOrder(ctx, 0); !errors.As(err, &inv) || inv.Reason != book.ReasonInvalidOrderID {
		t.Fatalf("cancel 0: err = %v", err)
	}
	if _, err := e.CancelOrder(ctx, makeOrderID(1, 7)); !errors.Is(err, ErrUnknownOrder) {
		t.Fatalf("cancel with no such symbol index: err = %v", err)
	}
	// IDs the engine cannot have issued never reach a book: counter 0, a
	// counter not reached yet, and a symbol index at or past the last one.
	for _, id := range []book.OrderID{makeOrderID(0, 1), makeOrderID(99, 1), makeOrderID(1, 2)} {
		if _, err := e.CancelOrder(ctx, id); !errors.Is(err, ErrUnknownOrder) {
			t.Fatalf("cancel of never-issued ID %d: err = %v", id, err)
		}
		if _, found, err := e.Order(ctx, id); found || err != nil {
			t.Fatalf("Order of never-issued ID %d: %v, %v", id, found, err)
		}
	}
	if d, _ := e.Depth(ctx, "B", 1); d.LastSeq != 0 {
		t.Fatalf("B's LastSeq = %d after cancels of never-issued IDs", d.LastSeq)
	}
	// An issued ID that is not resting reaches its book and is rejected.
	placed, _ := e.PlaceOrder(ctx, NewOrder{Symbol: "B", Side: book.Buy, Type: book.Limit, TIF: book.IOC, Price: 1, Qty: 1})
	r, err := e.CancelOrder(ctx, placed.OrderID)
	if err != nil || len(r.Events) != 1 || r.Events[0].Reason != book.ReasonUnknownOrder {
		t.Fatalf("cancel of an issued but not resting ID: %+v, %v", r, err)
	}
	if _, err := e.Depth(ctx, "Z", 1); !errors.Is(err, ErrUnknownSymbol) {
		t.Fatalf("depth of unknown symbol: err = %v", err)
	}
}

func TestContextAlreadyDone(t *testing.T) {
	e := newTestEngine(t, Config{Symbols: []string{"A"}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 100 { // the result must not depend on select's random choice
		if _, err := e.PlaceOrder(ctx, limitOrder("A", book.Buy, 100, 1)); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	}
	if d, _ := e.Depth(context.Background(), "A", 1); d.LastSeq != 0 {
		t.Fatalf("a command with a done context was applied (LastSeq %d)", d.LastSeq)
	}
}

// TestConcurrentClients places orders on several symbols from many
// goroutines at once. Each book still sees a gap-free Seq, every ID is
// unique, and each symbol's events are exactly what applying its commands
// in Seq order to a fresh book gives.
func TestConcurrentClients(t *testing.T) {
	syms := []string{"A", "B", "C", "D"}
	e := newTestEngine(t, Config{Symbols: syms, QueueSize: 16, MaxBatch: 4})
	const clients, perClient = 8, 200
	var mu sync.Mutex
	results := map[string][]Result{}
	var wg sync.WaitGroup
	for c := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perClient {
				s := syms[(c+i)%len(syms)]
				side := book.Buy
				if i%2 == 0 {
					side = book.Sell
				}
				r, err := e.PlaceOrder(context.Background(), limitOrder(s, side, book.Price(100+i%5), book.Qty(1+i%3)))
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				results[s] = append(results[s], r)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	ids := map[book.OrderID]bool{}
	total := 0
	for _, s := range syms {
		rs := results[s]
		slices.SortFunc(rs, func(a, b Result) int { return int(a.Events[0].Seq) - int(b.Events[0].Seq) })
		var seq uint64
		for _, r := range rs {
			if ids[r.OrderID] {
				t.Fatalf("ID %d issued twice", r.OrderID)
			}
			ids[r.OrderID] = true
			for _, ev := range r.Events {
				seq++
				if ev.Seq != seq {
					t.Fatalf("%s: Seq %d, want %d", s, ev.Seq, seq)
				}
			}
		}
		total += len(rs)
	}
	if total != clients*perClient {
		t.Fatalf("%d results, want %d", total, clients*perClient)
	}
}

// TestCloseDrainsAndRefuses races Close against 50 concurrent orders, 20
// times: every order is either applied or refused with ErrClosed.
func TestCloseDrainsAndRefuses(t *testing.T) {
	for range 20 {
		closeDuringTraffic(t)
	}
}

func closeDuringTraffic(t *testing.T) {
	e, err := New(Config{Symbols: []string{"A"}, QueueSize: 64, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var applied, refused int
	var mu sync.Mutex
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.PlaceOrder(context.Background(), limitOrder("A", book.Buy, 100, 1))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				applied++
			case errors.Is(err, ErrClosed):
				refused++
			default:
				t.Error(err)
			}
		}()
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if applied+refused != 50 {
		t.Fatalf("applied %d + refused %d != 50", applied, refused)
	}
	if _, err := e.PlaceOrder(context.Background(), limitOrder("A", book.Buy, 100, 1)); !errors.Is(err, ErrClosed) {
		t.Fatalf("after Close: err = %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// A start that fails removes the logs it created: one left behind would
// bind its symbol to the failed attempt's index and reject the corrected
// configuration.
func TestFailedStartRemovesNewLogs(t *testing.T) {
	dir := t.TempDir()
	e, err := New(Config{Symbols: []string{"A", "B"}, DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	mustClose(t, e)
	if _, err := New(Config{Symbols: []string{"A", "X", "B"}, DataDir: dir}); err == nil {
		t.Fatal("New with B moved to index 2 succeeded")
	}
	if _, err := os.Stat(filepath.Join(dir, "X.log")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("X.log after the failed start: %v, want it removed", err)
	}
	e, err = New(Config{Symbols: []string{"A", "B", "X"}, DataDir: dir})
	if err != nil {
		t.Fatalf("New with X appended: %v", err)
	}
	mustClose(t, e)
}
