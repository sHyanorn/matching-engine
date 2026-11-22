package oe

import (
	"net"
	"testing"

	"github.com/sHyanorn/matching-engine/internal/spin"
	"github.com/sHyanorn/matching-engine/internal/spin/spintest"
)

// benchConn is a client for benchmarks: it sends a batch of frames in one
// write and reads until it has seen the answers, without allocating.
type benchConn struct {
	b    *testing.B
	c    net.Conn
	out  []byte
	in   []byte
	have int
}

// benchLogin serves opts on a loopback port, connects and opens a session.
func benchLogin(b *testing.B, opts ServerOptions) *benchConn {
	b.Helper()
	if opts.Symbols == nil {
		opts.Symbols = testSymbols
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	s := NewServer(opts)
	go func() { _ = s.Serve(ln) }()
	b.Cleanup(func() { _ = s.Close() })
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = c.Close() })
	bc := &benchConn{b: b, c: c, in: make([]byte, 64<<10)}
	bc.out = Login{Version: Version}.Append(bc.out)
	bc.flush()
	bc.read(1+len(opts.Symbols), func(t Type, _ []byte) bool { return true }) // LoginAccepted and the Symbols
	return bc
}

func (bc *benchConn) flush() {
	if _, err := bc.c.Write(bc.out); err != nil {
		bc.b.Fatal(err)
	}
	bc.out = bc.out[:0]
}

// read reads frames until n of them satisfy count.
func (bc *benchConn) read(n int, count func(Type, []byte) bool) {
	for {
		for {
			t, body, size, err := Next(bc.in[:bc.have])
			if err != nil {
				bc.b.Fatal(err)
			}
			if size == 0 {
				break
			}
			if count(t, body) {
				n--
			}
			bc.have = copy(bc.in, bc.in[size:bc.have])
		}
		if n == 0 {
			return
		}
		k, err := bc.c.Read(bc.in[bc.have:])
		if err != nil {
			bc.b.Fatal(err)
		}
		bc.have += k
	}
}

// BenchmarkPing measures Pings over one session on loopback, 256 sent in a
// write and their Pongs read before the next write. ns/op is one Ping, and
// the allocations are the server's.
func BenchmarkPing(b *testing.B) { benchPing(b, nil) }

// BenchmarkPingSpin is BenchmarkPing with the session's reader and writer
// spinning.
func BenchmarkPingSpin(b *testing.B) {
	pool, _ := spintest.New(b, 2)
	benchPing(b, pool)
}

func benchPing(b *testing.B, pool *spin.Pool) {
	const batch = 256
	bc := benchLogin(b, ServerOptions{Spin: pool})
	isPong := func(t Type, _ []byte) bool { return t == TypePong }
	var seq uint64
	b.ReportAllocs()
	b.ResetTimer()
	for done := 0; done < b.N; done += batch {
		n := min(batch, b.N-done)
		for range n {
			seq++
			bc.out = Ping{ClientSeq: seq}.Append(bc.out)
		}
		bc.flush()
		bc.read(n, isPong)
	}
}
