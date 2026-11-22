//go:build !linux

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Off Linux the engine refuses -oe-spin, with the system's name in the
// message, and leaves the data directory alone.
func TestOESpinRefusedOffLinux(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := run(ctx, []string{"-addr", "127.0.0.1:0", "-data", dir, "-oe-addr", "127.0.0.1:0", "-oe-spin"}, io.Discard)
	if !errors.Is(err, errors.ErrUnsupported) || !strings.Contains(err.Error(), "needs Linux") || !strings.Contains(err.Error(), runtime.GOOS) {
		t.Errorf("run -oe-spin = %v, want an unsupported error that says it needs Linux and names %s", err, runtime.GOOS)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("the data directory holds %v, %v after a refused start, want it empty", entries, err)
	}
}
