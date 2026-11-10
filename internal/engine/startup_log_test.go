package engine

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sHyanorn/matching-engine/internal/book"
)

// startupLogs starts an engine on cfg, closes it and returns the records
// its logger got at the given level.
func startupLogs(t *testing.T, cfg Config, level slog.Level) []map[string]any {
	t.Helper()
	var logs lockedBuffer
	cfg.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	mustClose(t, e)
	var recs []map[string]any
	for line := range strings.Lines(logs.String()) {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if rec["level"] == level.String() {
			recs = append(recs, rec)
		}
	}
	return recs
}

// TestFreshStartLogsEmptyBook checks that a first start on an empty data
// directory says, at info level, that the symbol's log was not found.
func TestFreshStartLogsEmptyBook(t *testing.T) {
	cfg := Config{Symbols: []string{"A"}, DataDir: t.TempDir()}
	infos := startupLogs(t, cfg, slog.LevelInfo)
	if len(infos) != 1 || infos[0]["symbol"] != "A" || !strings.Contains(infos[0]["msg"].(string), "empty book") {
		t.Fatalf("info logs = %v, want one naming symbol A and the empty book", infos)
	}
	if warns := startupLogs(t, cfg, slog.LevelWarn); len(warns) != 0 {
		t.Fatalf("restart with the log in place warned: %v", warns)
	}
}

// TestMissingLogWithEarlierStateWarned deletes a symbol's log and keeps the
// ids file, as a wrong volume or a deleted log would. The engine still
// starts, and warns that the directory holds earlier state.
func TestMissingLogWithEarlierStateWarned(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Symbols: []string{"A"}, DataDir: dir}
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	mustClose(t, e)
	if err := os.Remove(filepath.Join(dir, "A.log")); err != nil {
		t.Fatal(err)
	}
	warns := startupLogs(t, cfg, slog.LevelWarn)
	if len(warns) != 1 || warns[0]["symbol"] != "A" || !strings.Contains(warns[0]["msg"].(string), "earlier state") {
		t.Fatalf("warnings = %v, want one naming symbol A and the earlier state", warns)
	}
}

// TestTornTailTruncationWarned appends half a record to a log and checks
// that the start warns with the symbol and the number of bytes it dropped.
func TestTornTailTruncationWarned(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Symbols: []string{"A"}, DataDir: dir}
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlaceOrder(context.Background(), limitOrder("A", book.Sell, 100, 5)); err != nil {
		t.Fatal(err)
	}
	mustClose(t, e)
	rec := appendRecord(nil, logRecord{cmd: book.Command{Kind: book.CommandCancel, OrderID: makeOrderID(1, 0), Timestamp: 9}})
	torn := rec[:len(rec)/2]
	f, err := os.OpenFile(filepath.Join(dir, "A.log"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(torn); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	warns := startupLogs(t, cfg, slog.LevelWarn)
	if len(warns) != 1 || warns[0]["symbol"] != "A" || warns[0]["bytes"] != float64(len(torn)) {
		t.Fatalf("warnings = %v, want one with symbol A and bytes %d", warns, len(torn))
	}
}
