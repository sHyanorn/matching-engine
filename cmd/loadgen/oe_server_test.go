package main

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/sHyanorn/matching-engine/internal/oe"
)

// TestOEAgainstServer runs the OE1 mode against the engine's session
// server, as the loopback runs do.
func TestOEAgainstServer(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := oe.NewServer(oe.ServerOptions{Symbols: []string{"BTC-USD"}})
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	cfg := config{oeAddr: ln.Addr().String(), rate: 1000, warmup: 200 * time.Millisecond, duration: time.Second,
		spin: true, maxSendLag: time.Hour}
	r, err := measureOE(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Whether the target rate went out depends on how busy the machine
	// is; any other problem is the tool's or the server's.
	for _, p := range r.Problems {
		if strings.HasPrefix(p, "achieved ") {
			t.Log(p)
		} else {
			t.Errorf("invalid run: %s", p)
		}
	}
	if r.Outcomes["ping"] != 1000 || r.Outcomes["pong"] != 1000 || r.Latency["pong"].Count != 1000 {
		t.Errorf("outcomes %v, %d round trips; want 1,000 Pings answered", r.Outcomes, r.Latency["pong"].Count)
	}
}
