package oe

import (
	"bytes"
	"encoding/hex"
	"errors"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/engine"
)

// frame is what every frame type has.
type frame interface{ Append([]byte) []byte }

// wireSize is each frame type's length on the wire, from the tables in
// docs/API.md, kept apart from bodySize so that each checks the other.
var wireSize = map[Type]int{
	TypeLogin: 21, TypeNewOrder: 31, TypeCancel: 19, TypePing: 31, TypeHeartbeat: 3, TypeLogout: 3,
	TypeLoginAccepted: 27, TypeLoginRejected: 4, TypeSymbol: 36, TypeEvent: 83, TypeDone: 30, TypePong: 30,
	TypeEndOfSession: 3,
}

// decode decodes a body that Next returned for type t.
func decode(t Type, body []byte) frame {
	switch t {
	case TypeLogin:
		return DecodeLogin(body)
	case TypeNewOrder:
		return DecodeNewOrder(body)
	case TypeCancel:
		return DecodeCancel(body)
	case TypePing:
		return DecodePing(body)
	case TypeHeartbeat:
		return Heartbeat{}
	case TypeLogout:
		return Logout{}
	case TypeLoginAccepted:
		return DecodeLoginAccepted(body)
	case TypeLoginRejected:
		return DecodeLoginRejected(body)
	case TypeSymbol:
		return DecodeSymbol(body)
	case TypeEvent:
		return DecodeEvent(body)
	case TypeDone:
		return DecodeDone(body)
	case TypePong:
		return DecodePong(body)
	case TypeEndOfSession:
		return EndOfSession{}
	}
	panic("decode: unknown type " + string(rune(t)))
}

// golden holds one frame of each type and its bytes, written out by hand
// from docs/API.md. Each field holds bytes no neighbouring field holds, so
// a field that moves, changes width or byte order, or an enumeration that
// is renumbered, changes the bytes.
var golden = []struct {
	f   frame
	hex string
}{
	{Login{Version: 1, Session: 0x0807060504030201, NextServerSeq: 0x1817161514131211},
		"1300 4c 0100 0102030405060708 1112131415161718"},
	{NewOrder{ClientSeq: 0x0807060504030201, Symbol: 0x21, Side: book.Buy, Type: book.Market, TIF: book.FOK,
		Price: 0x3837363534333231, Qty: 0x4847464544434241},
		"1d00 4e 0102030405060708 21 01 02 03 3132333435363738 4142434445464748"},
	{Cancel{ClientSeq: 0x0807060504030201, OrderID: 0x1817161514131211},
		"1100 43 0102030405060708 1112131415161718"},
	{Ping{ClientSeq: 0x0807060504030201},
		"1d00 50 0102030405060708 0000000000000000000000000000000000000000"},
	{Heartbeat{}, "0100 48"},
	{Logout{}, "0100 4f"},
	{LoginAccepted{Session: 0x0807060504030201, NextServerSeq: 0x1817161514131211, LastClientSeq: 0x2827262524232221},
		"1900 41 0102030405060708 1112131415161718 2122232425262728"},
	{LoginRejected{Reason: RejectReplayLost}, "0200 4a 03"},
	{Symbol{Index: 7, Name: "BTC-USD"},
		"2200 59 07 4254432d555344 00000000000000000000000000000000000000000000000000"},
	{Event{ServerSeq: 0x0807060504030201, ClientSeq: 0x1817161514131211, Event: book.Event{
		Seq: 0x2827262524232221, Timestamp: 0x3837363534333231,
		Kind: book.EventTrade, Side: book.Buy, Reason: book.ReasonUserCancel, LevelSide: book.Sell,
		LevelOrders: 0x44434241,
		OrderID:     0x5857565554535251, MakerID: 0x6867666564636261,
		Price: 0x7877767574737271, Qty: 0x0f0e0d0c0b0a0908, LevelQty: 0x4f4e4d4c4b4a4948,
	}},
		"5100 45 0102030405060708 1112131415161718 2122232425262728 3132333435363738 03 01 0d 02 41424344" +
			" 5152535455565758 6162636465666768 7172737475767778 08090a0b0c0d0e0f 48494a4b4c4d4e4f"},
	{Done{ServerSeq: 0x0807060504030201, ClientSeq: 0x1817161514131211, Status: StatusLogFailedApplied,
		Reason: book.ReasonFOKUnfillable, OrderID: 0x2827262524232221, State: engine.StateCanceled},
		"1c00 44 0102030405060708 1112131415161718 08 0c 2122232425262728 03"},
	{Pong{ClientSeq: 0x0807060504030201},
		"1c00 51 0102030405060708 00000000000000000000000000000000000000"},
	{EndOfSession{}, "0100 5a"},
}

func TestGolden(t *testing.T) {
	for _, g := range golden {
		want, err := hex.DecodeString(strings.ReplaceAll(g.hex, " ", ""))
		if err != nil {
			t.Fatal(err)
		}
		got := g.f.Append([]byte("prefix"))
		if !bytes.Equal(got[len("prefix"):], want) {
			t.Errorf("%T encodes as\n%x, want\n%x", g.f, got[len("prefix"):], want)
			continue
		}
		typ, body, n, err := Next(want)
		if err != nil || n != len(want) {
			t.Errorf("%T: Next = %q, %d, %v; want the whole %d bytes", g.f, byte(typ), n, err, len(want))
			continue
		}
		if n != wireSize[typ] {
			t.Errorf("%T is %d bytes, docs/API.md says %d", g.f, n, wireSize[typ])
		}
		if back := decode(typ, body); back != g.f {
			t.Errorf("%T decodes as %+v, want %+v", g.f, back, g.f)
		}
	}
	if len(golden) != len(wireSize) {
		t.Errorf("%d golden frames, %d frame types", len(golden), len(wireSize))
	}
}

// TestWireValuesArePinned guards the numbers OE1 sends that golden does
// not show one by one.
func TestWireValuesArePinned(t *testing.T) {
	pins := []struct {
		name      string
		got, want uint8
	}{
		{"StatusOK", uint8(StatusOK), 1},
		{"StatusInvalid", uint8(StatusInvalid), 2},
		{"StatusUnknownSymbol", uint8(StatusUnknownSymbol), 3},
		{"StatusUnknownOrder", uint8(StatusUnknownOrder), 4},
		{"StatusOrderFinished", uint8(StatusOrderFinished), 5},
		{"StatusInternal", uint8(StatusInternal), 6},
		{"StatusLogFailed", uint8(StatusLogFailed), 7},
		{"StatusLogFailedApplied", uint8(StatusLogFailedApplied), 8},
		{"StatusBusy", uint8(StatusBusy), 9},
		{"StatusShuttingDown", uint8(StatusShuttingDown), 10},
		{"RejectBadVersion", uint8(RejectBadVersion), 1},
		{"RejectUnknownSession", uint8(RejectUnknownSession), 2},
		{"RejectReplayLost", uint8(RejectReplayLost), 3},
		{"RejectTooManySessions", uint8(RejectTooManySessions), 4},
		{"StateOpen", uint8(engine.StateOpen), 1},
		{"StateFilled", uint8(engine.StateFilled), 2},
		{"StateCanceled", uint8(engine.StateCanceled), 3},
		{"StateRejected", uint8(engine.StateRejected), 4},
	}
	for _, p := range pins {
		if p.got != p.want {
			t.Errorf("%s = %d, want %d (pinned wire value)", p.name, p.got, p.want)
		}
	}
}

func TestNames(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{StatusOK.String(), "ok"},
		{StatusLogFailedApplied.String(), "log_failed_applied"},
		{StatusShuttingDown.String(), "shutting_down"},
		{Status(0).String(), "Status(0)"},
		{Status(11).String(), "Status(11)"},
		{RejectBadVersion.String(), "bad_version"},
		{RejectTooManySessions.String(), "too_many_sessions"},
		{RejectReason(5).String(), "RejectReason(5)"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

func TestNextRejectsMalformedHeaders(t *testing.T) {
	for typ, size := range wireSize {
		for _, length := range []int{size - HeaderSize, size - HeaderSize + 2} {
			b := make([]byte, size+1)
			le.PutUint16(b, uint16(length))
			b[2] = byte(typ)
			if _, _, _, err := Next(b[:HeaderSize]); !errors.Is(err, ErrFrame) {
				t.Errorf("type %q with length %d: Next = %v, want ErrFrame", byte(typ), length, err)
			}
		}
	}
	for c := range 256 {
		if _, known := wireSize[Type(c)]; known {
			continue
		}
		if _, _, _, err := Next([]byte{1, 0, byte(c)}); !errors.Is(err, ErrFrame) {
			t.Errorf("type %q: Next = %v, want ErrFrame", byte(c), err)
		}
	}
}

func TestNextWaitsForWholeFrame(t *testing.T) {
	for _, g := range golden {
		b := g.f.Append(nil)
		for i := range len(b) {
			if typ, body, n, err := Next(b[:i]); n != 0 || err != nil || typ != 0 || body != nil {
				t.Errorf("%T, %d of %d bytes: Next = %q, %x, %d, %v; want nothing yet", g.f, i, len(b), byte(typ), body, n, err)
			}
		}
	}
}

// randomFrame returns a frame of a random type with random fields.
func randomFrame(r *rand.Rand) frame {
	u := r.Uint64
	b := func() uint8 { return uint8(r.Uint32()) }
	switch r.IntN(len(wireSize)) {
	case 0:
		return Login{Version: uint16(r.Uint32()), Session: u(), NextServerSeq: u()}
	case 1:
		return NewOrder{ClientSeq: u(), Symbol: b(), Side: book.Side(b()), Type: book.OrderType(b()), TIF: book.TIF(b()),
			Price: book.Price(u()), Qty: book.Qty(u())}
	case 2:
		return Cancel{ClientSeq: u(), OrderID: book.OrderID(u())}
	case 3:
		return Ping{ClientSeq: u()}
	case 4:
		return Heartbeat{}
	case 5:
		return Logout{}
	case 6:
		return LoginAccepted{Session: u(), NextServerSeq: u(), LastClientSeq: u()}
	case 7:
		return LoginRejected{Reason: RejectReason(b())}
	case 8:
		const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-"
		name := make([]byte, r.IntN(SymbolNameSize+1))
		for i := range name {
			name[i] = chars[r.IntN(len(chars))]
		}
		return Symbol{Index: b(), Name: string(name)}
	case 9:
		return Event{ServerSeq: u(), ClientSeq: u(), Event: book.Event{
			Seq: u(), Timestamp: int64(u()),
			Kind: book.EventKind(b()), Side: book.Side(b()), Reason: book.Reason(b()), LevelSide: book.Side(b()),
			LevelOrders: r.Uint32(),
			OrderID:     book.OrderID(u()), MakerID: book.OrderID(u()),
			Price: book.Price(u()), Qty: book.Qty(u()), LevelQty: book.Qty(u()),
		}}
	case 10:
		return Done{ServerSeq: u(), ClientSeq: u(), Status: Status(b()), Reason: book.Reason(b()),
			OrderID: book.OrderID(u()), State: engine.OrderState(b())}
	case 11:
		return Pong{ClientSeq: u()}
	default:
		return EndOfSession{}
	}
}

// TestRoundTrip encodes random frames into one stream, reads it back in
// random pieces, as a socket delivers it, and checks that every frame
// comes back as it was sent.
func TestRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	n := 100_000
	if testing.Short() {
		n = 10_000
	}
	sent := make([]frame, n)
	var stream []byte
	for i := range sent {
		sent[i] = randomFrame(r)
		stream = sent[i].Append(stream)
	}
	var buf []byte
	got := 0
	for len(stream) > 0 || len(buf) > 0 {
		k := min(len(stream), r.IntN(200))
		buf, stream = append(buf, stream[:k]...), stream[k:]
		for {
			typ, body, size, err := Next(buf)
			if err != nil {
				t.Fatalf("frame %d: %v", got, err)
			}
			if size == 0 {
				break
			}
			if f := decode(typ, body); f != sent[got] {
				t.Fatalf("frame %d: got %+v, sent %+v", got, f, sent[got])
			}
			got++
			buf = buf[size:]
		}
		if len(stream) == 0 && len(buf) > 0 {
			t.Fatalf("%d bytes left over after %d frames", len(buf), got)
		}
	}
	if got != n {
		t.Fatalf("read %d frames, sent %d", got, n)
	}
}

func TestEncodeDecodeAllocateNothing(t *testing.T) {
	buf := make([]byte, 0, 256)
	body := func() []byte { _, b, _, _ := Next(buf); return b }
	var (
		login  Login
		order  NewOrder
		cancel Cancel
		ping   Ping
		acc    LoginAccepted
		rej    LoginRejected
		ev     Event
		done   Done
		pong   Pong
	)
	cases := []struct {
		name string
		fn   func()
	}{
		{"Login", func() { buf = login.Append(buf[:0]); login = DecodeLogin(body()) }},
		{"NewOrder", func() { buf = order.Append(buf[:0]); order = DecodeNewOrder(body()) }},
		{"Cancel", func() { buf = cancel.Append(buf[:0]); cancel = DecodeCancel(body()) }},
		{"Ping", func() { buf = ping.Append(buf[:0]); ping = DecodePing(body()) }},
		{"Heartbeat", func() { buf = Heartbeat{}.Append(buf[:0]); body() }},
		{"Logout", func() { buf = Logout{}.Append(buf[:0]); body() }},
		{"LoginAccepted", func() { buf = acc.Append(buf[:0]); acc = DecodeLoginAccepted(body()) }},
		{"LoginRejected", func() { buf = rej.Append(buf[:0]); rej = DecodeLoginRejected(body()) }},
		{"Symbol", func() { buf = Symbol{Name: "BTC-USD"}.Append(buf[:0]); body() }}, // decoding copies the name
		{"Event", func() { buf = ev.Append(buf[:0]); ev = DecodeEvent(body()) }},
		{"Done", func() { buf = done.Append(buf[:0]); done = DecodeDone(body()) }},
		{"Pong", func() { buf = pong.Append(buf[:0]); pong = DecodePong(body()) }},
		{"EndOfSession", func() { buf = EndOfSession{}.Append(buf[:0]); body() }},
	}
	for _, c := range cases {
		if allocs := testing.AllocsPerRun(100, c.fn); allocs != 0 {
			t.Errorf("%s: %v allocations per encode and decode, want 0", c.name, allocs)
		}
	}
	if len(cases) != len(wireSize) {
		t.Errorf("%d cases, %d frame types", len(cases), len(wireSize))
	}
}

// FuzzDecode feeds Next arbitrary bytes. It must never panic, must take
// exactly the frames whose header is well formed, and every frame it
// takes must decode to a value that encodes and decodes to itself.
func FuzzDecode(f *testing.F) {
	for _, g := range golden {
		f.Add(g.f.Append(nil))
	}
	f.Add([]byte{0x13, 0x00, 'L', 1, 0})
	f.Fuzz(func(t *testing.T, b []byte) {
		for len(b) > 0 {
			typ, body, n, err := Next(b)
			var size int
			var valid bool
			if len(b) >= HeaderSize {
				size, valid = wireSize[Type(b[2])]
				valid = valid && int(le.Uint16(b)) == size-HeaderSize+1
			}
			switch {
			case len(b) < HeaderSize:
				if n != 0 || err != nil {
					t.Fatalf("Next(%x) = %d, %v with no whole header", b, n, err)
				}
				return
			case !valid:
				if !errors.Is(err, ErrFrame) {
					t.Fatalf("Next(%x) = %d, %v; want ErrFrame", b, n, err)
				}
				return
			case len(b) < size:
				if n != 0 || err != nil {
					t.Fatalf("Next(%x) = %d, %v before the frame is whole", b, n, err)
				}
				return
			case err != nil || n != size || len(body) != size-HeaderSize || typ != Type(b[2]):
				t.Fatalf("Next(%x) = %q, %d-byte body, %d, %v; want the %d-byte frame", b, byte(typ), len(body), n, err, size)
			}
			m := decode(typ, body)
			again := m.Append(nil)
			typ2, body2, n2, err := Next(again)
			if err != nil || n2 != n || typ2 != typ || decode(typ2, body2) != m {
				t.Fatalf("%+v re-encodes as %x, which does not decode to itself", m, again)
			}
			b = b[n:]
		}
	})
}
