package spin

import (
	"errors"
	"io"
	"math"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// noDeadline is the deadline of a connection that has none.
const noDeadline = math.MaxInt64

// conn is a connection whose Read polls the socket once Spin is called.
type conn struct {
	net.Conn
	rc       syscall.RawConn
	spinning atomic.Bool
	closed   atomic.Bool
	start    time.Time    // the deadline is kept as an offset from here, on the monotonic clock
	deadline atomic.Int64 // the read deadline, in nanoseconds after start

	// The read in progress, kept here so that a read allocates nothing.
	poll func(fd uintptr) bool
	buf  []byte
	n    int
	err  error
}

func wrapConn(nc net.Conn) net.Conn {
	sc, ok := nc.(syscall.Conn)
	if !ok {
		return nc
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return nc
	}
	c := &conn{Conn: nc, rc: rc, start: time.Now()}
	c.deadline.Store(noDeadline)
	c.poll = c.pollFD // made once: a method value made for each read would allocate
	return c
}

// Spin clears the socket's own read deadline, which a polling Read would
// still be held to, and from here on does not set it.
func (c *conn) Spin() {
	_ = c.Conn.SetReadDeadline(time.Time{}) // fails only if the connection is closed, which Read finds out
	c.spinning.Store(true)
}

// Close makes a polling Read return, which closing the socket alone would
// not: the socket stays open until the Read that holds it is done.
func (c *conn) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

func (c *conn) SetDeadline(t time.Time) error {
	c.keep(t)
	return c.Conn.SetDeadline(t)
}

// SetReadDeadline does not set the socket's own deadline once Spin has been
// called: a polling Read never waits on it, and setting it costs time on
// every read.
func (c *conn) SetReadDeadline(t time.Time) error {
	c.keep(t)
	if c.spinning.Load() {
		return nil
	}
	return c.Conn.SetReadDeadline(t)
}

func (c *conn) keep(t time.Time) {
	if t.IsZero() {
		c.deadline.Store(noDeadline)
		return
	}
	c.deadline.Store(int64(t.Sub(c.start)))
}

// Read is the connection's own Read until Spin is called, and then it polls.
// Data that is there is returned even if the deadline has passed.
func (c *conn) Read(b []byte) (int, error) {
	if !c.spinning.Load() {
		return c.Conn.Read(b)
	}
	if len(b) == 0 {
		return 0, nil
	}
	c.buf = b
	if err := c.rc.Read(c.poll); err != nil {
		return 0, err // closed before the read began
	}
	switch {
	case c.err == nil && c.n == 0:
		return 0, io.EOF
	case c.err == nil:
		return c.n, nil
	case errors.Is(c.err, net.ErrClosed), errors.Is(c.err, os.ErrDeadlineExceeded):
		return 0, c.opError(c.err)
	default:
		return 0, c.opError(os.NewSyscallError("read", c.err))
	}
}

// pollFD reads from fd until there is data, an error, the end of the stream,
// the deadline or Close. It never asks the runtime to wait for the socket,
// so the goroutine never leaves its thread.
func (c *conn) pollFD(fd uintptr) bool {
	for {
		if c.closed.Load() {
			c.n, c.err = 0, net.ErrClosed
			return true
		}
		c.n, c.err = unix.Read(int(fd), c.buf)
		switch {
		case errors.Is(c.err, unix.EINTR):
		case !errors.Is(c.err, unix.EAGAIN):
			return true
		case int64(time.Since(c.start)) >= c.deadline.Load():
			c.n, c.err = 0, os.ErrDeadlineExceeded
			return true
		}
	}
}

func (c *conn) opError(err error) error {
	return &net.OpError{Op: "read", Net: c.LocalAddr().Network(), Source: c.LocalAddr(), Addr: c.RemoteAddr(), Err: err}
}
