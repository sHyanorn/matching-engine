package oe

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

var testSymbols = []string{"BTC-USD", "ETH-USD"}

// startServer serves opts on a loopback port until the test ends.
func startServer(t *testing.T, opts ServerOptions) (*Server, string) {
	t.Helper()
	if opts.Symbols == nil {
		opts.Symbols = testSymbols
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(opts)
	served := make(chan error, 1)
	go func() { served <- s.Serve(ln) }()
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return s, ln.Addr().String()
}

// client is the test's end of one connection.
type client struct {
	t   *testing.T
	c   net.Conn
	buf []byte
}

func dial(t *testing.T, addr string) *client {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &client{t: t, c: c}
}

// send writes frames with one write.
func (c *client) send(frames ...frame) {
	c.t.Helper()
	var b []byte
	for _, f := range frames {
		b = f.Append(b)
	}
	if _, err := c.c.Write(b); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

// next returns the next frame, or the error that ended the connection.
func (c *client) next() (frame, error) {
	c.t.Helper()
	if err := c.c.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return nil, err
	}
	for {
		t, body, n, err := Next(c.buf)
		if err != nil {
			c.t.Fatalf("from the server: %v", err)
		}
		if n > 0 {
			f := decode(t, body)
			c.buf = c.buf[n:]
			return f, nil
		}
		var tmp [512]byte
		k, err := c.c.Read(tmp[:])
		c.buf = append(c.buf, tmp[:k]...)
		if err != nil && k == 0 {
			return nil, err
		}
	}
}

// expect reads the next frames and checks they are want.
func (c *client) expect(want ...frame) {
	c.t.Helper()
	for _, w := range want {
		f, err := c.next()
		if err != nil {
			c.t.Fatalf("got %v, want %+v", err, w)
		}
		if f != w {
			c.t.Fatalf("got %T %+v, want %T %+v", f, f, w, w)
		}
	}
}

// expectClosed checks that the server closes the connection with no
// further frames.
func (c *client) expectClosed() {
	c.t.Helper()
	f, err := c.next()
	if err == nil {
		c.t.Fatalf("got %T %+v, want the connection closed", f, f)
	}
	if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && !isReset(err) {
		c.t.Fatalf("got %v, want the connection closed", err)
	}
}

// isReset reports a connection the server closed with input unread.
func isReset(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && !op.Timeout()
}

// login opens a session and reads the server's answer.
func (c *client) login() uint64 {
	c.t.Helper()
	c.send(Login{Version: Version})
	f, err := c.next()
	acc, ok := f.(LoginAccepted)
	if err != nil || !ok || acc.Session == 0 || acc.NextServerSeq != 1 || acc.LastClientSeq != 0 {
		c.t.Fatalf("Login: got %+v, %v; want LoginAccepted with a session, next_server_seq 1, last_client_seq 0", f, err)
	}
	for i, name := range testSymbols {
		c.expect(Symbol{Index: uint8(i), Name: name})
	}
	return acc.Session
}

func TestLoginPingLogout(t *testing.T) {
	_, addr := startServer(t, ServerOptions{})
	a, b := dial(t, addr), dial(t, addr)
	if sa, sb := a.login(), b.login(); sa == sb {
		t.Fatalf("both sessions are %d", sa)
	}
	a.send(Ping{ClientSeq: 1}, Ping{ClientSeq: 2}, Heartbeat{}, Ping{ClientSeq: 3})
	a.expect(Pong{ClientSeq: 1}, Pong{ClientSeq: 2}, Pong{ClientSeq: 3})
	b.send(Ping{ClientSeq: 1}) // each session counts from 1
	b.expect(Pong{ClientSeq: 1})
	a.send(Ping{ClientSeq: 4}, Logout{})
	a.expect(Pong{ClientSeq: 4})
	a.expectClosed()
}

func TestLoginRefused(t *testing.T) {
	_, addr := startServer(t, ServerOptions{})
	for _, tc := range []struct {
		login Login
		want  RejectReason
	}{
		{Login{Version: 2}, RejectBadVersion},
		{Login{Version: Version, Session: 1}, RejectUnknownSession},
	} {
		c := dial(t, addr)
		c.send(tc.login)
		c.expect(LoginRejected{Reason: tc.want})
		c.expectClosed()
	}
	c := dial(t, addr)
	c.send(Ping{ClientSeq: 1})
	c.expectClosed()
}

func TestClientSeq(t *testing.T) {
	_, addr := startServer(t, ServerOptions{})
	c := dial(t, addr)
	c.login()
	// A number not above the last one taken is a resend; one that skips
	// a number ends the connection.
	c.send(Ping{ClientSeq: 1}, Ping{ClientSeq: 2}, Ping{ClientSeq: 2}, Ping{ClientSeq: 1}, Ping{ClientSeq: 3}, Ping{ClientSeq: 5})
	c.expect(Pong{ClientSeq: 1}, Pong{ClientSeq: 2}, Pong{ClientSeq: 3})
	c.expectClosed()
}

func TestProtocolErrorsClose(t *testing.T) {
	_, addr := startServer(t, ServerOptions{})
	for _, tc := range []struct {
		name string
		f    frame
	}{
		{"NewOrder, not served yet", NewOrder{ClientSeq: 1}},
		{"Cancel, not served yet", Cancel{ClientSeq: 1, OrderID: 1}},
		{"a second Login", Login{Version: Version}},
		{"a server frame", Pong{ClientSeq: 1}},
		{"a wrong length", rawFrame{0x02, 0x00, byte(TypePing), 0}},
		{"an unknown type", rawFrame{0x01, 0x00, 'x'}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := dial(t, addr)
			c.login()
			c.send(tc.f, Ping{ClientSeq: 1})
			c.expectClosed()
		})
	}
}

// rawFrame is bytes sent as they are.
type rawFrame []byte

func (r rawFrame) Append(dst []byte) []byte { return append(dst, r...) }

func TestHeartbeatAndIdle(t *testing.T) {
	_, addr := startServer(t, ServerOptions{Heartbeat: 20 * time.Millisecond, Idle: 200 * time.Millisecond})

	// A client that sends heartbeats keeps its session and gets them.
	c := dial(t, addr)
	c.login()
	beats := 0
	for end := time.Now().Add(400 * time.Millisecond); time.Now().Before(end); {
		c.send(Heartbeat{})
		c.expect(Heartbeat{})
		beats++
	}
	// A silent one is closed once nothing has arrived for Idle, counted
	// from its last frame.
	start := time.Now()
	c.send(Ping{ClientSeq: 1})
	c.expect(Pong{ClientSeq: 1})
	for time.Since(start) < 3*time.Second {
		f, err := c.next()
		if err != nil {
			if d := time.Since(start); d < 150*time.Millisecond {
				t.Errorf("silent connection closed after %v, want about 200ms", d)
			}
			return
		}
		if f != (Heartbeat{}) {
			t.Fatalf("got %+v while silent, want Heartbeat", f)
		}
	}
	t.Error("silent connection still open after 3s, want it closed after about 200ms")
}

// pipeListener hands out the server's ends of net.Pipe connections, which
// hold no bytes in flight: a write waits until the other end reads.
type pipeListener struct {
	conns chan net.Conn
	once  sync.Once
	done  chan struct{}
}

func newPipeListener() *pipeListener {
	return &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
}

func (l *pipeListener) dial() net.Conn {
	c, s := net.Pipe()
	l.conns <- s
	return c
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *pipeListener) Addr() net.Addr { return &net.TCPAddr{} }

func servePipes(t *testing.T, opts ServerOptions) *pipeListener {
	t.Helper()
	opts.Symbols = testSymbols
	s, ln := NewServer(opts), newPipeListener()
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(func() { _ = s.Close() })
	return ln
}

func TestSlowConsumer(t *testing.T) {
	t.Run("unsent output", func(t *testing.T) {
		ln := servePipes(t, ServerOptions{MaxUnsent: 1000})
		c := ln.dial()
		defer func() { _ = c.Close() }()
		// The client never reads, so the server's first write, its 99-byte
		// answer to the Login, waits, and the unsent output passes 1000
		// bytes with the 31st Pong of 30 bytes.
		if _, err := c.Write(Login{Version: Version}.Append(nil)); err != nil {
			t.Fatal(err)
		}
		_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		taken := 0
		for seq := uint64(1); seq <= 1000; seq++ {
			if _, err := c.Write(Ping{ClientSeq: seq}.Append(nil)); err != nil {
				break
			}
			taken++
		}
		if taken != 31 {
			t.Errorf("the server took %d Pings from a client that reads nothing, want it to close after 31", taken)
		}
	})
	t.Run("write timeout", func(t *testing.T) {
		ln := servePipes(t, ServerOptions{WriteTimeout: 100 * time.Millisecond})
		c := ln.dial()
		defer func() { _ = c.Close() }()
		start := time.Now()
		if _, err := c.Write(Login{Version: Version}.Append(nil)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond) // reads nothing meanwhile
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if n, err := c.Read(make([]byte, 64)); !errors.Is(err, io.EOF) {
			t.Fatalf("Read = %d, %v after the write timed out; want EOF", n, err)
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("closed after %v, want about 100ms", d)
		}
	})
}

func TestCloseEndsSessions(t *testing.T) {
	s, addr := startServer(t, ServerOptions{})
	const clients, pings = 4, 2000
	var wg sync.WaitGroup
	errs := make(chan error, clients)
	for range clients {
		c := dial(t, addr)
		c.login()
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- pingPong(c, pings)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	idle := dial(t, addr) // connected, not logged in
	logged := dial(t, addr)
	logged.login()
	busy := dial(t, addr) // pings while the server closes
	busy.login()
	stop := make(chan struct{})
	go func() {
		var b []byte
		for seq := uint64(1); ; seq++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := busy.c.Write(Ping{ClientSeq: seq}.Append(b[:0])); err != nil {
				return
			}
		}
	}()
	defer close(stop)
	time.Sleep(10 * time.Millisecond)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	logged.expect(EndOfSession{})
	logged.expectClosed()
	idle.expectClosed()
	// Nothing follows EndOfSession. A client still sending when the
	// connection closes may get a reset instead, and lose what it had not
	// yet read, EndOfSession included.
	for ended := false; ; {
		f, err := busy.next()
		if err != nil {
			break
		}
		if ended {
			t.Fatalf("got %+v after EndOfSession", f)
		}
		ended = f == EndOfSession{}
	}
}

// pingPong sends n Pings in uneven batches while reading their Pongs.
func pingPong(c *client, n int) error {
	sent := make(chan error, 1)
	go func() {
		var b []byte
		for seq := 1; seq <= n; {
			b = b[:0]
			for k := 0; k < seq%7+1 && seq <= n; k++ {
				b = Ping{ClientSeq: uint64(seq)}.Append(b)
				seq++
			}
			if _, err := c.c.Write(b); err != nil {
				sent <- err
				return
			}
		}
		sent <- nil
	}()
	for seq := 1; seq <= n; seq++ {
		f, err := c.next()
		if err != nil {
			return err
		}
		if f != (Pong{ClientSeq: uint64(seq)}) {
			return fmt.Errorf("got %+v, want Pong %d", f, seq)
		}
	}
	return <-sent
}

func TestPingAllocatesNothing(t *testing.T) {
	ss := &session{srv: NewServer(ServerOptions{}), id: 1, wake: make(chan struct{}, 1)}
	ss.out = make([]byte, 0, 64)
	buf := make([]byte, 0, 64)
	allocs := testing.AllocsPerRun(1000, func() {
		ss.out = ss.out[:0]
		buf = Ping{ClientSeq: ss.lastClientSeq + 1}.Append(buf[:0])
		typ, body, _, _ := Next(buf)
		if err := ss.handle(typ, body); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Errorf("%v allocations to take a Ping and queue its Pong, want 0", allocs)
	}
	if ss.lastClientSeq < 1000 {
		t.Fatalf("took %d Pings", ss.lastClientSeq)
	}
}
