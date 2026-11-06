package engine

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/sHyanorn/matching-engine/internal/book"
)

// randomTraffic sends n random requests for symbol from one client: orders
// of every type around a mid price, cancels of the client's own orders and
// a few invalid orders. It returns the results of the commands that
// reached the book and the IDs of every order it placed.
func randomTraffic(t *testing.T, e *Engine, symbol string, seed uint64, n int) ([]Result, []book.OrderID) {
	r := rand.New(rand.NewPCG(seed, 0xe9))
	var results []Result
	var ids []book.OrderID
	for range n {
		var res Result
		var err error
		if len(ids) > 0 && r.IntN(4) == 0 {
			res, err = e.CancelOrder(context.Background(), ids[r.IntN(len(ids))])
		} else {
			o := NewOrder{Symbol: symbol, Side: book.Buy, Type: book.Limit, TIF: book.GTC, Price: book.Price(1_000 + r.IntN(11) - 5), Qty: book.Qty(1 + r.IntN(9))}
			if r.IntN(2) == 0 {
				o.Side = book.Sell
			}
			switch r.IntN(10) {
			case 0:
				o.TIF = book.IOC
			case 1:
				o.TIF = book.FOK
			case 2:
				o.Type, o.TIF, o.Price = book.Market, book.IOC, 0
			case 3:
				o.Qty = 0 // invalid: answered without reaching the book
			}
			res, err = e.PlaceOrder(context.Background(), o)
			ids = append(ids, res.OrderID)
		}
		var inv *InvalidError
		switch {
		case errors.As(err, &inv):
			continue
		case err != nil:
			t.Error(err)
			return results, ids
		}
		results = append(results, res)
	}
	return results, ids
}

// stream orders the results of one symbol by Seq and returns all their
// events, which must then number 1, 2, 3, ... with no gaps.
func stream(t *testing.T, results []Result) []book.Event {
	t.Helper()
	slices.SortFunc(results, func(a, b Result) int { return cmp.Compare(a.Events[0].Seq, b.Events[0].Seq) })
	var evs []book.Event
	for _, r := range results {
		evs = append(evs, r.Events...)
	}
	for i, e := range evs {
		if e.Seq != uint64(i+1) {
			t.Fatalf("event %d has Seq %d", i, e.Seq)
		}
	}
	return evs
}

// TestReplayDeterminism is SPEC §7's replay property at the engine level:
// after concurrent traffic on several symbols, replaying the command logs
// into fresh books reproduces every symbol's event stream, depth and
// resting orders exactly, and replaying again gives the same result while
// leaving the logs untouched.
func TestReplayDeterminism(t *testing.T) {
	ctx := context.Background()
	syms := []string{"A", "B", "C"}
	cfg := Config{Symbols: syms, DataDir: t.TempDir(), QueueSize: 32, MaxBatch: 8}
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	const clientsPerSymbol, perClient = 3, 400
	var mu sync.Mutex
	results := map[string][]Result{}
	var ids []book.OrderID
	var wg sync.WaitGroup
	for si, s := range syms {
		for c := range clientsPerSymbol {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rs, placed := randomTraffic(t, e, s, uint64(si*10+c+1), perClient)
				mu.Lock()
				results[s] = append(results[s], rs...)
				ids = append(ids, placed...)
				mu.Unlock()
			}()
		}
	}
	wg.Wait()

	want := map[string][]book.Event{}
	depth := map[string]Depth{}
	for _, s := range syms {
		want[s] = stream(t, results[s])
		depth[s], _ = e.Depth(ctx, s, 1<<20)
	}
	orders := map[book.OrderID]OrderStatus{}
	for _, id := range ids {
		if o, found, _ := e.Order(ctx, id); found {
			orders[id] = o
		}
	}
	mustClose(t, e)
	logs := map[string][]byte{}
	for _, s := range syms {
		logs[s], _ = os.ReadFile(filepath.Join(cfg.DataDir, s+".log"))
	}

	for run := 1; run <= 2; run++ {
		replayed := map[string][]book.Event{}
		cfg.onReplay = func(symbol string, evs []book.Event) { replayed[symbol] = append(replayed[symbol], evs...) }
		r, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range syms {
			if !slices.Equal(replayed[s], want[s]) {
				t.Fatalf("run %d, %s: replay gave %d events, the original run %d, or they differ", run, s, len(replayed[s]), len(want[s]))
			}
			d, _ := r.Depth(ctx, s, 1<<20)
			if !slices.Equal(d.Bids, depth[s].Bids) || !slices.Equal(d.Asks, depth[s].Asks) || d.LastSeq != depth[s].LastSeq {
				t.Fatalf("run %d, %s: depth %+v, want %+v", run, s, d, depth[s])
			}
		}
		for _, id := range ids {
			o, found, _ := r.Order(ctx, id)
			if w, ok := orders[id]; found != ok || o != w {
				t.Fatalf("run %d: order %d = %+v, %v; want %+v, %v", run, id, o, found, w, ok)
			}
		}
		mustClose(t, r)
		for _, s := range syms {
			if got, _ := os.ReadFile(filepath.Join(cfg.DataDir, s+".log")); !bytes.Equal(got, logs[s]) {
				t.Fatalf("run %d, %s: replay changed the log", run, s)
			}
		}
	}
	t.Logf("%d events across %d symbols replayed identically", len(want["A"])+len(want["B"])+len(want["C"]), len(syms))
}
