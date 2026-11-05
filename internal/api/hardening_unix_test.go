//go:build unix

package api_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// smallBuffers returns a socket option setter that shrinks the buffer opt
// (SO_SNDBUF or SO_RCVBUF), so a peer that does not read stalls writes
// after a few kilobytes.
func smallBuffers(opt int) func(network, address string, c syscall.RawConn) error {
	return func(_, _ string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) { serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, opt, 4096) })
		return errors.Join(err, serr)
	}
}

// slowEnv serves from a listener whose connections have a small send
// buffer.
func slowEnv(t *testing.T, cfg envConfig) *testEnv {
	t.Helper()
	lc := net.ListenConfig{Control: smallBuffers(syscall.SO_SNDBUF)}
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.listener = ln
	return newEnv(t, cfg)
}

// dialNoRead opens a WebSocket connection with a small receive buffer. The
// test reads from it only when it chooses to.
func dialNoRead(t *testing.T, env *testEnv) *websocket.Conn {
	t.Helper()
	d := &net.Dialer{Control: smallBuffers(syscall.SO_RCVBUF)}
	client := &http.Client{Transport: &http.Transport{DialContext: d.DialContext}}
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, env.streamURL(), &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	conn.SetReadLimit(64 << 20)
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

// Snapshots queued for a client that does not read are bounded in bytes:
// past the limit the client is disconnected as a slow consumer, long
// before the write timeout.
func TestStreamBacklogBoundedInBytes(t *testing.T) {
	env := slowEnv(t, envConfig{writeTimeout: time.Minute})
	restBids(t, env, 2000) // a snapshot of about 80 KB
	conn := dialNoRead(t, env)
	for range 30 {
		writeMsg(t, conn, `{"op":"subscribe","channels":["book:BTC-USD"]}`)
		writeMsg(t, conn, `{"op":"unsubscribe","channels":["book:BTC-USD"]}`)
	}
	requireClose(t, newStream(conn).closeError(t), websocket.StatusPolicyViolation, "slow consumer")
}

// A message whose write stalls past the write timeout ends the connection
// with a close frame once the client reads again.
func TestStalledWriteEndsWithCloseFrame(t *testing.T) {
	env := slowEnv(t, envConfig{writeTimeout: 200 * time.Millisecond})
	restBids(t, env, 2000)
	conn := dialNoRead(t, env)
	writeMsg(t, conn, `{"op":"subscribe","channels":["book:BTC-USD"]}`)
	time.Sleep(time.Second) // the snapshot's write stalls meanwhile
	requireClose(t, newStream(conn).closeError(t), websocket.StatusPolicyViolation, "slow consumer")
}
