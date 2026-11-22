package spin

import (
	"errors"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
	"time"
)

// tcpPair returns the two ends of a loopback connection.
func tcpPair(t *testing.T) (client, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server, err = ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	return client, server
}

// spinning returns the server end of a loopback connection, wrapped and
// polling.
func spinning(t *testing.T) (client net.Conn, server Poller) {
	t.Helper()
	c, s := tcpPair(t)
	p, ok := NewPool(nil, nil).Wrap(s).(Poller)
	if !ok {
		t.Fatalf("a TCP connection was wrapped as %T, want a Poller", s)
	}
	p.Spin()
	return c, p
}

func TestWrapNeedsASocket(t *testing.T) {
	p := NewPool(nil, nil)
	a, b := net.Pipe()
	defer func() { _ = a.Close(); _ = b.Close() }()
	if got := p.Wrap(a); got != a {
		t.Errorf("a pipe was wrapped as %T, want it left alone: it has no socket to poll", got)
	}
	_, s := tcpPair(t)
	if _, ok := p.Wrap(s).(Poller); !ok {
		t.Errorf("a TCP connection was wrapped as %T, want a Poller", p.Wrap(s))
	}
}

// Until Spin, Read is the connection's own, with its own deadline.
func TestReadBlocksUntilSpin(t *testing.T) {
	c, s := tcpPair(t)
	p := NewPool(nil, nil).Wrap(s)
	_ = p.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, err := p.Read(make([]byte, 8)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Read = %v, want a deadline error", err)
	}
	if _, err := c.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	_ = p.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 8)
	if n, err := p.Read(buf); err != nil || string(buf[:n]) != "abc" {
		t.Fatalf("Read = %q, %v, want abc", buf[:n], err)
	}
}

// The socket's own deadline, set while Read blocked, does not end a
// polling Read when it passes.
func TestSpinForgetsTheSocketsOwnDeadline(t *testing.T) {
	c, s := tcpPair(t)
	p := NewPool(nil, nil).Wrap(s).(Poller)
	_ = p.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	p.Spin()
	_ = p.SetReadDeadline(time.Now().Add(5 * time.Second))
	time.Sleep(60 * time.Millisecond)
	if _, err := c.Write([]byte("late")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8)
	if n, err := p.Read(buf); err != nil || string(buf[:n]) != "late" {
		t.Fatalf("Read = %q, %v, want the data sent after the socket's own deadline", buf[:n], err)
	}
}

func TestPollingReadGetsDataAndTheEnd(t *testing.T) {
	c, p := spinning(t)
	_ = p.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 8)
	if n, err := p.Read(buf[:0]); n != 0 || err != nil {
		t.Errorf("Read of no bytes = %d, %v, want 0, nil", n, err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond) // the read is polling by now
		_, _ = c.Write([]byte("hello"))
		time.Sleep(20 * time.Millisecond)
		_ = c.Close()
	}()
	if n, err := p.Read(buf); err != nil || string(buf[:n]) != "hello" {
		t.Fatalf("Read = %q, %v, want hello", buf[:n], err)
	}
	if n, err := p.Read(buf); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("Read after the peer closed = %d, %v, want 0, EOF", n, err)
	}
}

func TestPollingReadHonorsTheDeadline(t *testing.T) {
	c, p := spinning(t)
	start := time.Now()
	_ = p.SetReadDeadline(start.Add(60 * time.Millisecond))
	_, err := p.Read(make([]byte, 8))
	var ne net.Error
	if !errors.Is(err, os.ErrDeadlineExceeded) || !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("Read = %v, want a timeout that is os.ErrDeadlineExceeded", err)
	}
	if d := time.Since(start); d < 60*time.Millisecond || d > 2*time.Second {
		t.Errorf("Read gave up after %v, want about 60ms", d)
	}

	// A later deadline is the one that counts, and none means none.
	_ = p.SetReadDeadline(time.Now().Add(5 * time.Second))
	go func() { time.Sleep(100 * time.Millisecond); _, _ = c.Write([]byte("x")) }()
	if n, err := p.Read(make([]byte, 8)); err != nil || n != 1 {
		t.Fatalf("Read = %d, %v, want the byte sent after the old deadline", n, err)
	}
	_ = p.SetReadDeadline(time.Time{})
	go func() { time.Sleep(100 * time.Millisecond); _, _ = c.Write([]byte("y")) }()
	if n, err := p.Read(make([]byte, 8)); err != nil || n != 1 {
		t.Fatalf("Read = %d, %v, want the byte sent with no deadline", n, err)
	}

	// SetDeadline sets the read deadline too.
	_ = p.SetDeadline(time.Now().Add(40 * time.Millisecond))
	if _, err := p.Read(make([]byte, 8)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Read after SetDeadline = %v, want a deadline error", err)
	}
}

// Closing the connection ends a read that is polling, which the closed
// socket alone would not.
func TestCloseEndsAPollingRead(t *testing.T) {
	_, p := spinning(t)
	_ = p.SetReadDeadline(time.Now().Add(time.Minute))
	go func() { time.Sleep(50 * time.Millisecond); _ = p.Close() }()
	start := time.Now()
	_, err := p.Read(make([]byte, 8))
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Read = %v, want net.ErrClosed", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Read returned %v after the close", d)
	}
	if _, err := p.Read(make([]byte, 8)); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Read on a closed connection = %v, want net.ErrClosed", err)
	}
}

// A closed connection gives nothing more, whatever the peer still sends.
func TestPollingReadStopsAtOnceWhenClosed(t *testing.T) {
	c, p := spinning(t)
	if _, err := c.Write([]byte("more")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	_ = p.Close()
	if n, err := p.Read(make([]byte, 8)); n != 0 || !errors.Is(err, net.ErrClosed) {
		t.Errorf("Read = %d, %v after Close, want 0, net.ErrClosed", n, err)
	}
}

func TestPollingReadReportsAReset(t *testing.T) {
	c, p := spinning(t)
	_ = p.SetReadDeadline(time.Now().Add(5 * time.Second))
	_ = c.(*net.TCPConn).SetLinger(0)
	_ = c.Close() // a reset, not a FIN
	_, err := p.Read(make([]byte, 8))
	var op *net.OpError
	if !errors.Is(err, syscall.ECONNRESET) || !errors.As(err, &op) || op.Op != "read" {
		t.Errorf("Read = %v, want a read error that is ECONNRESET", err)
	}
}

func TestSpinReadAllocatesNothing(t *testing.T) {
	c, p := spinning(t)
	frame := []byte("0123456789abcdef")
	buf := make([]byte, 64)
	allocs := testing.AllocsPerRun(1000, func() {
		if _, err := c.Write(frame); err != nil {
			t.Fatal(err)
		}
		for got := 0; got < len(frame); {
			_ = p.SetReadDeadline(time.Now().Add(5 * time.Second))
			n, err := p.Read(buf)
			if err != nil {
				t.Fatal(err)
			}
			got += n
		}
	})
	if allocs != 0 {
		t.Errorf("%v allocations per spinning read, want 0", allocs)
	}
}
