package oe

import (
	"fmt"
	"math/rand/v2"
	"net"
	"testing"
	"time"

	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/engine"
)

// opSource gives the choices of a run of the model: the bytes of a fuzz
// input, or a seeded generator.
type opSource interface {
	more() bool
	intn(n int) int
}

type byteSource struct{ b []byte }

func (s *byteSource) more() bool { return len(s.b) > 0 }
func (s *byteSource) intn(n int) int {
	if len(s.b) == 0 {
		return 0
	}
	v := int(s.b[0]) % n
	s.b = s.b[1:]
	return v
}

type randSource struct {
	r    *rand.Rand
	left int
}

func (s *randSource) more() bool { s.left--; return s.left >= 0 }
func (s *randSource) intn(n int) int {
	return s.r.IntN(n)
}

// nullConn is a connection to nowhere: the model reads the session's
// output from the session.
type nullConn struct{ net.Conn }

func (nullConn) Close() error { return nil }

// model drives a session without a connection, as a client would, and
// checks what the session does against what the protocol says: replies are
// numbered without gaps, every command is passed to the engine at most
// once and answered exactly once, a resend is dropped, and the places in use
// are the commands the engine has not answered.
type model struct {
	t        *testing.T
	srv      *Server
	ss       *session
	c        *conn // nil between two connections
	lost     bool  // a resume was refused for replay_lost: replies were lost
	fe       *fakeEngine
	src      opSource
	buf      []byte
	last     uint64            // the last client_seq sent and taken
	kind     map[uint64]byte   // 'N', 'C' or 'P', by client_seq
	answered map[uint64]int    // Done frames seen, by client_seq
	seen     map[uint64]bool   // client_seqs the engine took
	issued   []book.OrderID    // IDs the fake engine gave
	nextSeq  uint64            // server_seq expected next
	open     uint64            // client_seq of the reply being read, 0 if none
	refuse   map[uint64]string // client_seq the engine refuses, by what it returns
	step     int
}

func (m *model) fatalf(format string, args ...any) {
	m.t.Helper()
	m.t.Fatalf("step %d: %s", m.step, fmt.Sprintf(format, args...))
}

func (m *model) send(f frame) error {
	m.buf = f.Append(m.buf[:0])
	typ, body, _, err := Next(m.buf)
	if err != nil {
		m.fatalf("%v", err)
	}
	return m.c.handle(typ, body)
}

// output returns the frames the session queued since the last call.
func (m *model) output() []frame {
	if m.c == nil {
		return nil
	}
	m.ss.mu.Lock()
	out := append([]byte(nil), m.c.out...)
	m.c.out = m.c.out[:0]
	m.ss.mu.Unlock()
	var fs []frame
	for len(out) > 0 {
		typ, body, n, err := Next(out)
		if err != nil || n == 0 {
			m.fatalf("the session queued a bad frame: %v", err)
		}
		fs = append(fs, decode(typ, body))
		out = out[n:]
	}
	return fs
}

// check reads what the session queued and holds it to the protocol.
func (m *model) check() []frame {
	fs := m.output()
	for _, f := range fs {
		switch f := f.(type) {
		case Event:
			if f.ServerSeq != m.nextSeq {
				m.fatalf("Event with server_seq %d, want %d", f.ServerSeq, m.nextSeq)
			}
			m.nextSeq++
			if m.kind[f.ClientSeq] != 'N' && m.kind[f.ClientSeq] != 'C' || m.answered[f.ClientSeq] != 0 || (m.open != 0 && m.open != f.ClientSeq) {
				m.fatalf("Event %+v for a command that is not open", f)
			}
			m.open = f.ClientSeq
		case Done:
			if f.ServerSeq != m.nextSeq {
				m.fatalf("Done with server_seq %d, want %d", f.ServerSeq, m.nextSeq)
			}
			m.nextSeq++
			if m.kind[f.ClientSeq] != 'N' && m.kind[f.ClientSeq] != 'C' || (m.open != 0 && m.open != f.ClientSeq) {
				m.fatalf("Done %+v for a command that is not open", f)
			}
			m.answered[f.ClientSeq]++
			if m.answered[f.ClientSeq] != 1 {
				m.fatalf("Done %+v: the command was answered twice", f)
			}
			m.open = 0
		case LoginAccepted:
			if f.Session != m.ss.id || f.NextServerSeq != m.nextSeq || f.LastClientSeq != m.last {
				m.fatalf("LoginAccepted %+v, want session %d, next_server_seq %d, last_client_seq %d", f, m.ss.id, m.nextSeq, m.last)
			}
		case Pong:
			if m.kind[f.ClientSeq] != 'P' {
				m.fatalf("Pong %+v for no Ping", f)
			}
		default:
			m.fatalf("unexpected frame %T", f)
		}
	}
	m.fe.mu.Lock()
	pending := len(m.fe.pending)
	m.fe.mu.Unlock()
	m.ss.mu.Lock()
	taken := m.ss.taken
	m.ss.mu.Unlock()
	if taken != pending {
		m.fatalf("%d places in use, the engine has %d commands unanswered", taken, pending)
	}
	if m.ss.lastClientSeq != m.last {
		m.fatalf("lastClientSeq %d, want %d", m.ss.lastClientSeq, m.last)
	}
	return fs
}

// command sends a NewOrder or Cancel with the next client_seq and checks
// how it was taken.
func (m *model) command(cancel bool, symbol uint8, id book.OrderID) {
	seq := m.last + 1
	before := m.fe.taken()
	var f frame = NewOrder{ClientSeq: seq, Symbol: symbol, Side: book.Buy, Type: book.Limit, TIF: book.GTC, Price: 100, Qty: 1}
	m.kind[seq] = 'N'
	if cancel {
		f, m.kind[seq] = Cancel{ClientSeq: seq, OrderID: id}, 'C'
	}
	_, refused := m.refuse[seq]
	if err := m.send(f); err != nil {
		m.fatalf("command %d: %v", seq, err)
	}
	m.last = seq
	fs := m.check()
	switch {
	case !cancel && int(symbol) >= len(testSymbols):
		if len(fs) != 1 || fs[0] != (Done{ServerSeq: m.nextSeq - 1, ClientSeq: seq, Status: StatusUnknownSymbol}) || m.fe.taken() != before {
			m.fatalf("unknown symbol %d: frames %+v, %d commands taken", symbol, fs, m.fe.taken()-before)
		}
	case refused:
		if len(fs) != 1 || fs[0].(Done).Status != StatusBusy || m.fe.taken() != before {
			m.fatalf("refused command: frames %+v, %d commands taken", fs, m.fe.taken()-before)
		}
	default:
		if len(fs) != 0 || m.fe.taken() != before+1 {
			m.fatalf("command %d: frames %+v, %d commands taken, want none and 1", seq, fs, m.fe.taken()-before)
		}
		m.seen[seq] = true
		if !cancel {
			m.issued = append(m.issued, book.OrderID(m.fe.nextID))
		}
	}
}

// room says whether a command can be sent without waiting for an answer.
func (m *model) room() bool {
	m.ss.mu.Lock()
	defer m.ss.mu.Unlock()
	return m.ss.taken < maxTaken
}

// runModel runs the model on the choices src gives.
func runModel(t *testing.T, src opSource) {
	t.Helper()
	m := &model{t: t, src: src, fe: &fakeEngine{hold: true}, kind: map[uint64]byte{}, answered: map[uint64]int{},
		seen: map[uint64]bool{}, refuse: map[uint64]string{}, nextSeq: 1}
	srv := NewServer(ServerOptions{Engine: m.fe, Symbols: testSymbols, Held: 700,
		afterFunc: func(time.Duration, func()) func() bool { return func() bool { return true } }})
	m.srv = srv
	m.c = attached(t, srv)
	m.ss = m.c.ss
	m.fe.refuse = func(c fakeCall) error {
		if _, ok := m.refuse[c.tag]; ok {
			return engine.ErrBusy
		}
		return nil
	}

	for m.step = 1; src.more(); m.step++ {
		switch op := src.intn(12); {
		case op < 3 && m.c != nil && m.room():
			sym := uint8(src.intn(len(testSymbols) + 1)) // the last is no symbol
			if src.intn(8) == 0 {
				m.refuse[m.last+1] = "busy"
			}
			m.command(false, sym, 0)
		case op == 3 && m.c != nil && m.room():
			id := book.OrderID(src.intn(3)) // 0 is never an ID; the rest may be unknown
			if len(m.issued) > 0 && src.intn(2) == 0 {
				id = m.issued[src.intn(len(m.issued))]
			}
			m.command(true, 0, id)
		case op == 4 && m.c != nil:
			m.last++
			m.kind[m.last] = 'P'
			if err := m.send(Ping{ClientSeq: m.last}); err != nil {
				m.fatalf("Ping: %v", err)
			}
			if fs := m.check(); len(fs) != 1 || fs[0] != (Pong{ClientSeq: m.last}) {
				m.fatalf("Ping %d answered with %+v", m.last, fs)
			}
		case op == 5 || op == 6:
			m.fe.mu.Lock()
			n := len(m.fe.pending)
			m.fe.mu.Unlock()
			if n > 0 {
				if op == 6 {
					n = 1 + src.intn(n)
				}
				m.fe.answer(n)
			}
			m.check()
		case op == 7 && m.c != nil && m.last > 0: // a resend, whatever the frame
			seq := 1 + uint64(src.intn(int(m.last)))
			before := m.fe.taken()
			var f frame = NewOrder{ClientSeq: seq, Symbol: 0, Side: book.Buy, Type: book.Limit, TIF: book.GTC, Price: 100, Qty: 1}
			switch src.intn(3) {
			case 1:
				f = Cancel{ClientSeq: seq, OrderID: 1}
			case 2:
				f = Ping{ClientSeq: seq}
			}
			if err := m.send(f); err != nil {
				m.fatalf("resend of %d: %v", seq, err)
			}
			if fs := m.check(); len(fs) != 0 || m.fe.taken() != before {
				m.fatalf("resend of %d: frames %+v, %d commands taken, want none", seq, fs, m.fe.taken()-before)
			}
		case op == 10 && m.c != nil: // the connection breaks, with or without what is queued read
			if src.intn(2) == 0 {
				m.check()
			}
			m.ss.mu.Lock()
			m.c.out, m.c.state = nil, aborted
			m.ss.mu.Unlock()
			m.ss.detach(m.c)
			m.c = nil
		case op == 11: // also over a connection still attached
			m.resume()
		case op == 8 && m.c != nil: // fill every place
			for m.room() {
				m.command(false, 0, 0)
			}
		case op == 9 && m.c != nil && src.intn(16) == 0: // a number that skips one ends the connection
			skip := NewOrder{ClientSeq: m.last + 2, Symbol: 0, Side: book.Buy, Type: book.Limit, TIF: book.GTC, Price: 100, Qty: 1}
			if err := m.send(skip); err == nil {
				m.fatalf("client_seq %d after %d was taken", skip.ClientSeq, m.last)
			}
			if fs := m.check(); len(fs) != 0 {
				m.fatalf("frames after a gap: %+v", fs)
			}
			m.finish()
			return
		}
	}
	m.finish()
}

// finish answers what is left and checks that every command was answered
// once.
func (m *model) finish() {
	m.fe.mu.Lock()
	n := len(m.fe.pending)
	m.fe.mu.Unlock()
	m.fe.answer(n)
	if m.c == nil {
		m.resume()
	}
	m.check()
	for seq := uint64(1); seq <= m.last && m.c != nil && !m.lost; seq++ {
		want := 1
		if m.kind[seq] == 'P' {
			want = 0
		}
		if m.answered[seq] != want {
			m.fatalf("client_seq %d (%c) answered %d times, want %d", seq, m.kind[seq], m.answered[seq], want)
		}
	}
	calls := map[uint64]bool{}
	for _, c := range m.fe.calls {
		if calls[c.tag] {
			m.fatalf("the engine took client_seq %d twice", c.tag)
		}
		calls[c.tag] = true
	}
}

// resume logs in again, from the server_seq the client expects, or now and
// then from one that no session can have sent, and checks the outcome
// against what the session holds. A refused resume leaves the connection
// the session has, if any, as it was.
func (m *model) resume() {
	k := m.nextSeq
	if m.src.intn(8) == 0 {
		m.ss.mu.Lock()
		next := m.ss.nextServerSeq
		m.ss.mu.Unlock()
		k = []uint64{0, next + 1 + uint64(m.src.intn(3))}[m.src.intn(2)]
	}
	c := &conn{srv: m.srv, nc: nullConn{}, wake: make(chan struct{}, 1), readerDone: make(chan struct{})}
	m.ss.mu.Lock()
	first, next := m.ss.heldFirstLocked(), m.ss.nextServerSeq
	reason, _ := c.resumeLocked(m.ss, k)
	m.ss.mu.Unlock()
	want := RejectReason(0)
	switch {
	case k == 0 || k > next:
		want = RejectUnknownSession
	case k < first:
		want = RejectReplayLost
		m.lost = true
	}
	if reason != want {
		m.fatalf("resume at %d (next %d, held from %d): refused with %v, want %v", k, next, first, reason, want)
	}
	if reason != 0 {
		if old := m.c; old != nil && (m.ss.cur != old || old.state != running || len(old.out) != 0) {
			m.fatalf("a refused resume at %d touched the attached connection: state %d, %d bytes queued", k, old.state, len(old.out))
		}
		return
	}
	m.c = c
	m.check()
	if m.nextSeq != next {
		m.fatalf("the replay ended at server_seq %d, want %d", m.nextSeq, next)
	}
}

func TestSessionModel(t *testing.T) {
	seeds, steps := 50, 2_000
	if testing.Short() {
		seeds = 5
	}
	for seed := range seeds {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			runModel(t, &randSource{r: rand.New(rand.NewPCG(uint64(seed), 0x5e55)), left: steps})
		})
	}
}

func FuzzSession(f *testing.F) {
	f.Add([]byte{0, 0, 5, 3, 6, 4, 7, 5})
	f.Add([]byte{8, 6, 6, 5, 8, 5})
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 9, 9})
	f.Fuzz(func(t *testing.T, data []byte) {
		runModel(t, &byteSource{data})
	})
}
