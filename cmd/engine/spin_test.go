package main

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// -oe-spin spins the OE1 path, which is off without -oe-addr: the engine
// refuses to start, and leaves the data directory alone, rather than run
// without the mode it was asked for.
func TestOESpinNeedsOEAddr(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := run(ctx, []string{"-addr", "127.0.0.1:0", "-data", dir, "-oe-spin"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "-oe-spin needs -oe-addr") {
		t.Errorf("run -oe-spin without -oe-addr = %v, want an error that says it needs -oe-addr", err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("the data directory holds %v, %v after a refused start, want it empty", entries, err)
	}
}
