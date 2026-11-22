//go:build !linux

package spin

import (
	"errors"
	"fmt"
	"net"
	"runtime"
)

// errUnsupported is why a pool cannot be made here: pinning a thread to a
// CPU and polling a socket are done on Linux only.
var errUnsupported = fmt.Errorf("spin mode needs Linux, this is %s: %w", runtime.GOOS, errors.ErrUnsupported)

func allowedCPUs() ([]int, error) { return nil, errUnsupported }

func pinThread(int) error { return errUnsupported }

func wrapConn(nc net.Conn) net.Conn { return nc }
