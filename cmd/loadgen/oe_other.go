//go:build !linux

package main

import (
	"net"
	"sync/atomic"
)

// spareCPUs gives no CPUs to pin to: pinning is done on Linux only.
func spareCPUs() cpus { return nil }

func pin(int) bool { return false }

// spinRead returns false: spinning on a socket is done on Linux only.
func spinRead(net.Conn, *atomic.Bool) (func([]byte) (int, error), bool) { return nil, false }
