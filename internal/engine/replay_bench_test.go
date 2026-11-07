package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/flow"
)

// flowCommand is op as the engine logs it, for the symbol at index 0.
func flowCommand(op flow.Op, ts int64) book.Command {
	c := book.Command{Kind: book.CommandCancel, OrderID: makeOrderID(uint64(op.Order), 0), Timestamp: ts}
	if !op.Cancel {
		c.Kind, c.Side, c.Type, c.TIF = book.CommandNewOrder, book.Sell, book.Limit, book.GTC
		if op.Buy {
			c.Side = book.Buy
		}
		c.Price, c.Qty = book.Price(op.Price), book.Qty(op.Qty)
	}
	return c
}

// writeFlowLog writes a command log for symbol (index 0) in dir holding n
// commands of the SPEC §9 flow, as the engine would have logged them, and
// returns its size in bytes.
func writeFlowLog(tb testing.TB, dir, symbol string, n int) int64 {
	tb.Helper()
	g := flow.New(1, 5)
	data := appendHeader(nil, symbol, 0)
	for i := range n {
		c := flowCommand(g.Next(), int64(i+1))
		data = appendRecord(data, logRecord{cmd: c})
	}
	if err := os.WriteFile(filepath.Join(dir, symbol+".log"), data, 0o644); err != nil {
		tb.Fatal(err)
	}
	return int64(len(data))
}

// BenchmarkReplay measures a start of the engine on a log of 1M commands:
// New locks the directory, reserves IDs and replays the log into the book
// and the order history; Close then closes it. ns/op is per start.
func BenchmarkReplay(b *testing.B) {
	const n = 1_000_000
	dir := b.TempDir()
	size := writeFlowLog(b, dir, "BTC-USD", n)
	cfg := Config{Symbols: []string{"BTC-USD"}, DataDir: dir, SyncInterval: time.Hour}
	b.ResetTimer()
	for range b.N {
		e, err := New(cfg)
		if err != nil {
			b.Fatal(err)
		}
		if err := e.Close(); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	perStart := b.Elapsed().Seconds() / float64(b.N)
	b.ReportMetric(n/perStart, "cmds/s")
	b.ReportMetric(perStart*1e9/n, "ns/cmd")
	b.ReportMetric(float64(size)/n, "bytes/cmd")
}

// TestReplayOfFlowLog checks that writeFlowLog makes a log the engine
// accepts, with the book the flow builds.
func TestReplayOfFlowLog(t *testing.T) {
	dir := t.TempDir()
	const n = 10_000
	writeFlowLog(t, dir, "BTC-USD", n)
	var events int
	e, err := New(Config{Symbols: []string{"BTC-USD"}, DataDir: dir, onReplay: func(_ string, evs []book.Event) {
		events += len(evs)
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := e.Close(); err != nil {
			t.Error(err)
		}
	}()
	want := book.New()
	var buf []book.Event
	g := flow.New(1, 5)
	var wantEvents int
	for i := range n {
		c := flowCommand(g.Next(), int64(i+1))
		buf = want.Apply(c, buf[:0])
		wantEvents += len(buf)
	}
	d, err := e.Depth(t.Context(), "BTC-USD", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if events != wantEvents || d.LastSeq != want.LastSeq() {
		t.Errorf("replay gave %d events up to seq %d, want %d up to %d", events, d.LastSeq, wantEvents, want.LastSeq())
	}
}
