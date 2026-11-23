//go:build !linux

package main

import (
	"errors"
	"fmt"
	"runtime"
)

// pinThread is unsupported: pinning a thread to a CPU is done on Linux only.
func pinThread(int) (func() error, error) {
	return nil, fmt.Errorf("pinning a thread needs Linux, this is %s: %w", runtime.GOOS, errors.ErrUnsupported)
}
