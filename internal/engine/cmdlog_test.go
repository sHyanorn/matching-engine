package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sHyanorn/matching-engine/internal/book"
)

func sampleRecords() []logRecord {
	return []logRecord{
		{cmd: book.Command{Kind: book.CommandNewOrder, OrderID: makeOrderID(1, 0), Side: book.Buy, Type: book.Limit, TIF: book.GTC, Price: 100, Qty: 5, Timestamp: 1}, clientID: "c-1"},
		{cmd: book.Command{Kind: book.CommandNewOrder, OrderID: makeOrderID(2, 0), Side: book.Sell, Type: book.Market, TIF: book.FOK, Qty: book.MaxQty, Timestamp: math.MaxInt64}},
		{cmd: book.Command{Kind: book.CommandCancel, OrderID: makeOrderID(1, 0), Timestamp: math.MinInt64}, clientID: strings.Repeat("x", MaxClientIDLen)},
		{cmd: book.Command{Kind: book.CommandNewOrder, OrderID: makeOrderID(1<<45-1, 0), Side: book.Buy, Type: book.Limit, TIF: book.IOC, Price: book.MaxPrice, Qty: 1, Timestamp: -7}},
	}
}

// encodeLog returns a log file for symbol "A" at index 0 holding recs, and
// the offset where each record starts.
func encodeLog(recs []logRecord) ([]byte, []int64) {
	buf := appendHeader(nil, "A", 0)
	var starts []int64
	for _, r := range recs {
		starts = append(starts, int64(len(buf)))
		buf = appendRecord(buf, r)
	}
	return buf, starts
}

// decodeLog reads a log produced by encodeLog.
func decodeLog(t *testing.T, data []byte) (recs []logRecord, good int64, torn bool, err error) {
	t.Helper()
	r := bytes.NewReader(data)
	start, err := readHeader(r, "A", 0)
	if err != nil {
		return nil, 0, false, err
	}
	good, torn, err = readRecords(r, start, int64(len(data)), func(rec logRecord) error {
		recs = append(recs, rec)
		return nil
	})
	return recs, good, torn, err
}

func TestLogRoundTrip(t *testing.T) {
	want := sampleRecords()
	data, _ := encodeLog(want)
	got, good, torn, err := decodeLog(t, data)
	if err != nil || torn || good != int64(len(data)) {
		t.Fatalf("good %d of %d, torn %v, err %v", good, len(data), torn, err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("decoded\n%+v\nwant\n%+v", got, want)
	}
	// An empty log is just a header.
	if got, _, torn, err := decodeLog(t, appendHeader(nil, "A", 0)); err != nil || torn || len(got) != 0 {
		t.Fatalf("empty log: %v, %v, %v", got, torn, err)
	}
}

func TestLogHeaderMismatch(t *testing.T) {
	data := appendHeader(nil, "A", 0)
	for _, tc := range []struct {
		name   string
		data   []byte
		symbol string
		index  int
		want   string
	}{
		{"other symbol", data, "B", 0, `for symbol "A" at index 0`},
		{"other index", data, "A", 1, `for symbol "A" at index 0`},
		{"not a log", []byte("hello, world!!!"), "A", 0, "not a command log"},
		{"version", append([]byte("MECMDLOG\x02\x00"), data[10:]...), "A", 0, "version 2"},
		{"short", data[:5], "A", 0, "header"},
	} {
		_, err := readHeader(bytes.NewReader(tc.data), tc.symbol, tc.index)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
}

// TestLogTornTail cuts the log at every byte inside its last record: each
// cut is a torn tail that ends the log after the record before it.
func TestLogTornTail(t *testing.T) {
	recs := sampleRecords()
	data, starts := encodeLog(recs)
	last := starts[len(starts)-1]
	for cut := last + 1; cut < int64(len(data)); cut++ {
		got, good, torn, err := decodeLog(t, data[:cut])
		if err != nil || !torn || good != last || !slices.Equal(got, recs[:len(recs)-1]) {
			t.Fatalf("cut at %d: %d records, good %d (want %d), torn %v, err %v", cut, len(got), good, last, torn, err)
		}
	}
	// A cut exactly between records is a clean end.
	if got, _, torn, err := decodeLog(t, data[:last]); err != nil || torn || len(got) != len(recs)-1 {
		t.Fatalf("cut between records: %d records, torn %v, err %v", len(got), torn, err)
	}
}

func TestLogDamage(t *testing.T) {
	recs := sampleRecords()
	data, starts := encodeLog(recs)
	flip := func(at int64) []byte {
		d := slices.Clone(data)
		d[at] ^= 0x40
		return d
	}
	zero := func(from, to int64) []byte { // zero bytes [from, to)
		d := slices.Clone(data)
		clear(d[from:to])
		return d
	}
	// badIDLen makes the first record's client_id length byte disagree
	// with its payload length, under a valid checksum.
	badIDLen := func() []byte {
		d := slices.Clone(data)
		p := d[starts[0]+4 : starts[1]-4]
		p[payloadFixed-1]--
		binary.LittleEndian.PutUint32(d[starts[1]-4:], crc32.Checksum(p, castagnoli))
		return d
	}
	end := int64(len(data))
	for _, tc := range []struct {
		name string
		data []byte
		torn bool // otherwise ErrCorruptLog
		good int64
	}{
		{"payload of the first record", flip(starts[0] + 10), false, starts[0]},
		{"checksum of a middle record", flip(starts[2] - 1), false, starts[1]},
		{"implausible length", flip(starts[1] + 2), false, starts[1]},
		{"payload of the last record", flip(int64(len(data)) - 6), true, starts[3]},
		{"zero-filled tail", append(slices.Clone(data), make([]byte, 300)...), true, int64(len(data))},
		{"zeros then data", append(append(slices.Clone(data), make([]byte, 8)...), 1), false, int64(len(data))},
		{"zero tail from inside a record", zero(starts[1]+10, end), true, starts[1]},
		{"zero tail from inside a length", zero(starts[2]+1, end), true, starts[2]},
		{"zeros inside, data after", zero(starts[1]+10, starts[3]), false, starts[1]},
		{"client_id length", badIDLen(), false, starts[0]},
	} {
		_, good, torn, err := decodeLog(t, tc.data)
		switch {
		case tc.torn && (err != nil || !torn):
			t.Errorf("%s: torn %v, err %v; want a torn tail", tc.name, torn, err)
		case !tc.torn && !errors.Is(err, ErrCorruptLog):
			t.Errorf("%s: torn %v, err %v; want ErrCorruptLog", tc.name, torn, err)
		case good != tc.good:
			t.Errorf("%s: good %d, want %d", tc.name, good, tc.good)
		}
	}
}

// TestEngineRestart closes an engine and opens a new one on the same data
// directory: books and sequence numbers survive, and new IDs are above
// every ID issued before.
func TestEngineRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := Config{Symbols: []string{"A", "B"}, DataDir: dir}
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var last book.OrderID
	for i := range 20 {
		sym := []string{"A", "B"}[i%2]
		side := book.Buy
		if i%3 == 0 {
			side = book.Sell
		}
		r, err := e.PlaceOrder(ctx, NewOrder{Symbol: sym, Side: side, Type: book.Limit, TIF: book.GTC, Price: book.Price(100 + i%4), Qty: 3, ClientID: "c"})
		if err != nil {
			t.Fatal(err)
		}
		last = r.OrderID
	}
	before := map[string]Depth{}
	for _, s := range cfg.Symbols {
		before[s], _ = e.Depth(ctx, s, 100)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	e2, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer mustClose(t, e2)
	for _, s := range cfg.Symbols {
		after, _ := e2.Depth(ctx, s, 100)
		if !slices.Equal(after.Bids, before[s].Bids) || !slices.Equal(after.Asks, before[s].Asks) || after.LastSeq != before[s].LastSeq {
			t.Fatalf("%s after restart: %+v, before %+v", s, after, before[s])
		}
	}
	r, err := e2.PlaceOrder(ctx, limitOrder("A", book.Buy, 1, 1))
	if err != nil || seqOf(r.OrderID) <= seqOf(last) {
		t.Fatalf("first ID after restart = %d (counter %d), last before %d (counter %d), err %v", r.OrderID, seqOf(r.OrderID), last, seqOf(last), err)
	}
}

// TestEngineRecoversTornLog appends half a record to a log, as a crash in
// the middle of a write would, and checks that the engine starts, drops
// the partial record from the file and keeps logging after it.
func TestEngineRecoversTornLog(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := Config{Symbols: []string{"A"}, DataDir: dir}
	e, _ := New(cfg)
	if _, err := e.PlaceOrder(ctx, limitOrder("A", book.Sell, 100, 5)); err != nil {
		t.Fatal(err)
	}
	mustClose(t, e)
	path := filepath.Join(dir, "A.log")
	intact, _ := os.ReadFile(path)
	half := appendRecord(nil, logRecord{cmd: book.Command{Kind: book.CommandCancel, OrderID: makeOrderID(1, 0), Timestamp: 9}})
	if err := os.WriteFile(path, append(slices.Clone(intact), half[:len(half)/2]...), 0o644); err != nil {
		t.Fatal(err)
	}

	e2, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, intact) {
		t.Fatalf("log is %d bytes after recovery, want the intact %d", len(got), len(intact))
	}
	if _, found, _ := e2.Order(ctx, makeOrderID(1, 0)); !found {
		t.Fatal("the order before the torn record was lost")
	}
	if _, err := e2.PlaceOrder(ctx, limitOrder("A", book.Buy, 90, 1)); err != nil {
		t.Fatal(err)
	}
	mustClose(t, e2)
	e3, err := New(cfg) // the record written after recovery replays too
	if err != nil {
		t.Fatal(err)
	}
	defer mustClose(t, e3)
	if d, _ := e3.Depth(ctx, "A", 10); len(d.Bids) != 1 || len(d.Asks) != 1 {
		t.Fatalf("depth after second restart = %+v", d)
	}
}

// TestEngineRefusesBadLogs checks that New fails, rather than starting with
// a wrong book, when a log holds something the engine never writes.
func TestEngineRefusesBadLogs(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  logRecord
		want string
	}{
		{"invalid command", logRecord{cmd: book.Command{Kind: book.CommandNewOrder, OrderID: makeOrderID(1, 0), Side: book.Buy, Type: book.Limit, TIF: book.GTC, Price: 100}}, "invalid"},
		{"order of another symbol", logRecord{cmd: book.Command{Kind: book.CommandNewOrder, OrderID: makeOrderID(1, 3), Side: book.Buy, Type: book.Limit, TIF: book.GTC, Price: 100, Qty: 1}}, "another symbol"},
	} {
		dir := t.TempDir()
		data := appendRecord(appendHeader(nil, "A", 0), tc.rec)
		if err := os.WriteFile(filepath.Join(dir, "A.log"), data, 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := New(Config{Symbols: []string{"A"}, DataDir: dir})
		if !errors.Is(err, ErrCorruptLog) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: New err = %v", tc.name, err)
		}
	}
	// A log written for another symbol order.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "A.log"), appendHeader(nil, "A", 1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Symbols: []string{"A"}, DataDir: dir}); err == nil || !strings.Contains(err.Error(), "index 1") {
		t.Errorf("reordered symbols: New err = %v", err)
	}
}

// mustClose closes e and fails t if Close reports an error.
func mustClose(t *testing.T, e *Engine) {
	t.Helper()
	if err := e.Close(); err != nil {
		t.Error(err)
	}
}

// TestLogFormatGolden pins the on-disk format byte for byte. The expected
// bytes were produced by an independent implementation of the layout
// documented in cmdlog.go. Any change here needs a new logVersion.
func TestLogFormatGolden(t *testing.T) {
	recs := []logRecord{
		{cmd: book.Command{Kind: book.CommandNewOrder, Side: book.Sell, Type: book.Limit, TIF: book.IOC, OrderID: makeOrderID(7, 3), Price: 100, Qty: 5, Timestamp: -2}, clientID: "c-1"},
		{cmd: book.Command{Kind: book.CommandNewOrder, Side: book.Buy, Type: book.Market, TIF: book.FOK, OrderID: makeOrderID(8, 3), Qty: 9, Timestamp: 1730000000000000000}},
	}
	buf := appendHeader(nil, "A", 3)
	for _, r := range recs {
		buf = appendRecord(buf, r)
	}
	const want = "4d45434d444c4f470100030001412800000001020102030700000000000064000000000000000500000000000000feffffffffffffff03632d31f6d76ffc250000000101020303080000000000000000000000000000090000000000000000006d85d531021800afdb0d44"
	if got := hex.EncodeToString(buf); got != want {
		t.Fatalf("log bytes changed:\n got %s\nwant %s", got, want)
	}
}

// writeLog writes a log for symbol "A" at index 0 holding recs to dir.
func writeLog(t *testing.T, dir string, recs ...logRecord) {
	t.Helper()
	data := appendHeader(nil, "A", 0)
	for _, r := range recs {
		data = appendRecord(data, r)
	}
	if err := os.WriteFile(filepath.Join(dir, "A.log"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRestartCounterFromNewOrdersOnly restores the ID counter from a log
// with no ID reservation: it is the highest counter of a logged new order,
// even when the last record is a cancel of a lower ID or names an ID far
// above anything issued.
func TestRestartCounterFromNewOrdersOnly(t *testing.T) {
	ctx := context.Background()
	order := func(seq uint64) logRecord {
		return logRecord{cmd: book.Command{Kind: book.CommandNewOrder, OrderID: makeOrderID(seq, 0), Side: book.Buy, Type: book.Limit, TIF: book.GTC, Price: 100, Qty: 1, Timestamp: 1}}
	}
	cancelOf := func(id book.OrderID) logRecord {
		return logRecord{cmd: book.Command{Kind: book.CommandCancel, OrderID: id, Timestamp: 2}}
	}
	for _, tc := range []struct {
		name string
		recs []logRecord
		want uint64 // counter of the next ID
	}{
		{"cancel of a lower ID last", []logRecord{order(1), order(2), cancelOf(makeOrderID(1, 0))}, 3},
		{"cancel of a huge ID", []logRecord{order(1), cancelOf(makeOrderID(1<<50, 0)), cancelOf(0xFFFFFFFFFFFFFF00)}, 2},
	} {
		dir := t.TempDir()
		writeLog(t, dir, tc.recs...)
		e, err := New(Config{Symbols: []string{"A"}, DataDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		r, err := e.PlaceOrder(ctx, limitOrder("A", book.Buy, 90, 1))
		if err != nil || seqOf(r.OrderID) != tc.want {
			t.Errorf("%s: next ID has counter %d, err %v; want %d", tc.name, seqOf(r.OrderID), err, tc.want)
		}
		mustClose(t, e)
	}
}

// TestIDsNotReusedAfterLostTail loses the end of a log, as a machine crash
// before fsync would, and checks that IDs issued before the crash are not
// issued again: the reservation in DataDir/ids outlives the log tail.
func TestIDsNotReusedAfterLostTail(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := Config{Symbols: []string{"A"}, DataDir: dir}
	e, _ := New(cfg)
	path := filepath.Join(dir, "A.log")
	issued := map[book.OrderID]bool{}
	var keep int64
	for i := range 10 {
		if i == 3 {
			info, _ := os.Stat(path)
			keep = info.Size()
		}
		r, err := e.PlaceOrder(ctx, limitOrder("A", book.Buy, 100, 1))
		if err != nil {
			t.Fatal(err)
		}
		issued[r.OrderID] = true
	}
	mustClose(t, e)
	if err := os.Truncate(path, keep); err != nil { // the last 7 orders are lost
		t.Fatal(err)
	}
	e2, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer mustClose(t, e2)
	for range 20 {
		r, err := e2.PlaceOrder(ctx, limitOrder("A", book.Buy, 100, 1))
		if err != nil || issued[r.OrderID] {
			t.Fatalf("ID %d reissued after a lost tail (err %v)", r.OrderID, err)
		}
	}
}

func TestIDStoreRefusesCorruption(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ids"), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Symbols: []string{"A"}, DataDir: dir}); !errors.Is(err, ErrCorruptLog) {
		t.Fatalf("New with a corrupt ids file: err = %v", err)
	}
}

// TestLeftoverTempLog starts from a directory where a crash interrupted
// the creation of a log: only the temporary file exists. New replaces it.
func TestLeftoverTempLog(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "A.log.tmp"), make([]byte, 7), 0o644); err != nil {
		t.Fatal(err)
	}
	e, err := New(Config{Symbols: []string{"A"}, DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	mustClose(t, e)
	data, _ := os.ReadFile(filepath.Join(dir, "A.log"))
	if !bytes.Equal(data, appendHeader(nil, "A", 0)) {
		t.Fatalf("A.log = %x, want just a header", data)
	}
}
