package oe

import (
	"bytes"
	"encoding/binary"
	"strconv"

	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/engine"
)

var le = binary.LittleEndian

// Version is the protocol version a Login names.
const Version = 1

// SymbolNameSize is the space for a symbol's name in a Symbol frame, the
// longest name the engine accepts.
const SymbolNameSize = 32

// Status is how a command ended, one to one with the REST answer
// (docs/API.md). The numbers are part of the protocol: append, never
// renumber.
type Status uint8

// The statuses, with the REST answer each stands for.
const (
	StatusOK               Status = 1  // 200
	StatusInvalid          Status = 2  // 400, with the validation reason
	StatusUnknownSymbol    Status = 3  // 404 unknown_symbol
	StatusUnknownOrder     Status = 4  // 404 unknown_order
	StatusOrderFinished    Status = 5  // 409 order_finished
	StatusInternal         Status = 6  // 500 internal
	StatusLogFailed        Status = 7  // 500 log_failed without events: not applied
	StatusLogFailedApplied Status = 8  // 500 log_failed with events: applied
	StatusBusy             Status = 9  // 503 busy
	StatusShuttingDown     Status = 10 // 503 shutting_down
)

var statusNames = [...]string{
	StatusOK:               "ok",
	StatusInvalid:          "invalid",
	StatusUnknownSymbol:    "unknown_symbol",
	StatusUnknownOrder:     "unknown_order",
	StatusOrderFinished:    "order_finished",
	StatusInternal:         "internal",
	StatusLogFailed:        "log_failed",
	StatusLogFailedApplied: "log_failed_applied",
	StatusBusy:             "busy",
	StatusShuttingDown:     "shutting_down",
}

func (s Status) String() string { return name(statusNames[:], s, "Status") }

// RejectReason says why a Login was refused. The numbers are part of the
// protocol: append, never renumber.
type RejectReason uint8

// The reasons a Login is refused.
const (
	RejectBadVersion      RejectReason = 1 // the version is not Version
	RejectUnknownSession  RejectReason = 2 // no such session is held
	RejectReplayLost      RejectReason = 3 // the output the client missed is no longer held
	RejectTooManySessions RejectReason = 4
)

var rejectNames = [...]string{
	RejectBadVersion:      "bad_version",
	RejectUnknownSession:  "unknown_session",
	RejectReplayLost:      "replay_lost",
	RejectTooManySessions: "too_many_sessions",
}

func (r RejectReason) String() string { return name(rejectNames[:], r, "RejectReason") }

func name[E ~uint8](names []string, v E, typeName string) string {
	if int(v) < len(names) && names[v] != "" {
		return names[v]
	}
	return typeName + "(" + strconv.Itoa(int(v)) + ")"
}

// Login opens a session: Session 0 asks for a new one.
type Login struct {
	Version       uint16
	Session       uint64
	NextServerSeq uint64 // the server_seq the client expects next
}

func (m Login) Append(dst []byte) []byte {
	dst = header(dst, TypeLogin)
	dst = le.AppendUint16(dst, m.Version)
	dst = le.AppendUint64(dst, m.Session)
	return le.AppendUint64(dst, m.NextServerSeq)
}

// DecodeLogin reads the body of a Login frame, as [Next] returns it; the
// other Decode functions take their type's body likewise.
func DecodeLogin(b []byte) Login {
	return Login{Version: le.Uint16(b), Session: le.Uint64(b[2:]), NextServerSeq: le.Uint64(b[10:])}
}

// NewOrder is POST /v1/orders without client_id. Symbol is the index a
// Symbol frame gave.
type NewOrder struct {
	ClientSeq uint64
	Symbol    uint8
	Side      book.Side
	Type      book.OrderType
	TIF       book.TIF
	Price     book.Price
	Qty       book.Qty
}

func (m NewOrder) Append(dst []byte) []byte {
	dst = header(dst, TypeNewOrder)
	dst = le.AppendUint64(dst, m.ClientSeq)
	dst = append(dst, m.Symbol, byte(m.Side), byte(m.Type), byte(m.TIF))
	dst = le.AppendUint64(dst, uint64(m.Price))
	return le.AppendUint64(dst, uint64(m.Qty))
}

func DecodeNewOrder(b []byte) NewOrder {
	return NewOrder{
		ClientSeq: le.Uint64(b),
		Symbol:    b[8], Side: book.Side(b[9]), Type: book.OrderType(b[10]), TIF: book.TIF(b[11]),
		Price: book.Price(le.Uint64(b[12:])),
		Qty:   book.Qty(le.Uint64(b[20:])),
	}
}

// Cancel is DELETE /v1/orders/{id}.
type Cancel struct {
	ClientSeq uint64
	OrderID   book.OrderID
}

func (m Cancel) Append(dst []byte) []byte {
	dst = header(dst, TypeCancel)
	dst = le.AppendUint64(dst, m.ClientSeq)
	return le.AppendUint64(dst, uint64(m.OrderID))
}

func DecodeCancel(b []byte) Cancel {
	return Cancel{ClientSeq: le.Uint64(b), OrderID: book.OrderID(le.Uint64(b[8:]))}
}

// Ping asks for a Pong. It is the size of a NewOrder, its body padded with
// zeros.
type Ping struct{ ClientSeq uint64 }

func (m Ping) Append(dst []byte) []byte {
	dst = header(dst, TypePing)
	dst = le.AppendUint64(dst, m.ClientSeq)
	return append(dst, make([]byte, 20)...)
}

func DecodePing(b []byte) Ping { return Ping{ClientSeq: le.Uint64(b)} }

// Heartbeat is sent by either side after a second without sending.
type Heartbeat struct{}

func (Heartbeat) Append(dst []byte) []byte { return header(dst, TypeHeartbeat) }

// Logout ends the session.
type Logout struct{}

func (Logout) Append(dst []byte) []byte { return header(dst, TypeLogout) }

// LoginAccepted answers a Login. LastClientSeq is the last client_seq the
// server took.
type LoginAccepted struct {
	Session       uint64
	NextServerSeq uint64
	LastClientSeq uint64
}

func (m LoginAccepted) Append(dst []byte) []byte {
	dst = header(dst, TypeLoginAccepted)
	dst = le.AppendUint64(dst, m.Session)
	dst = le.AppendUint64(dst, m.NextServerSeq)
	return le.AppendUint64(dst, m.LastClientSeq)
}

func DecodeLoginAccepted(b []byte) LoginAccepted {
	return LoginAccepted{Session: le.Uint64(b), NextServerSeq: le.Uint64(b[8:]), LastClientSeq: le.Uint64(b[16:])}
}

// LoginRejected refuses a Login; the server then closes the connection.
type LoginRejected struct{ Reason RejectReason }

func (m LoginRejected) Append(dst []byte) []byte {
	return append(header(dst, TypeLoginRejected), byte(m.Reason))
}

func DecodeLoginRejected(b []byte) LoginRejected { return LoginRejected{Reason: RejectReason(b[0])} }

// Symbol names the symbol at Index, the low byte of each of its order IDs
// (D019). The name is padded with zeros to SymbolNameSize bytes.
type Symbol struct {
	Index uint8
	Name  string
}

// Append encodes m; a name longer than SymbolNameSize panics.
func (m Symbol) Append(dst []byte) []byte {
	if len(m.Name) > SymbolNameSize {
		panic("oe: symbol name longer than " + strconv.Itoa(SymbolNameSize) + " bytes")
	}
	dst = append(header(dst, TypeSymbol), m.Index)
	dst = append(dst, m.Name...)
	return append(dst, make([]byte, SymbolNameSize-len(m.Name))...)
}

func DecodeSymbol(b []byte) Symbol {
	return Symbol{Index: b[0], Name: string(bytes.TrimRight(b[1:], "\x00"))}
}

// Event is one event of the client's own command, its fields in the order
// of book.Event.
type Event struct {
	ServerSeq uint64
	ClientSeq uint64
	book.Event
}

func (m Event) Append(dst []byte) []byte {
	dst = header(dst, TypeEvent)
	dst = le.AppendUint64(dst, m.ServerSeq)
	dst = le.AppendUint64(dst, m.ClientSeq)
	e := m.Event
	dst = le.AppendUint64(dst, e.Seq)
	dst = le.AppendUint64(dst, uint64(e.Timestamp))
	dst = append(dst, byte(e.Kind), byte(e.Side), byte(e.Reason), byte(e.LevelSide))
	dst = le.AppendUint32(dst, e.LevelOrders)
	dst = le.AppendUint64(dst, uint64(e.OrderID))
	dst = le.AppendUint64(dst, uint64(e.MakerID))
	dst = le.AppendUint64(dst, uint64(e.Price))
	dst = le.AppendUint64(dst, uint64(e.Qty))
	return le.AppendUint64(dst, uint64(e.LevelQty))
}

func DecodeEvent(b []byte) Event {
	e := b[16:]
	return Event{
		ServerSeq: le.Uint64(b),
		ClientSeq: le.Uint64(b[8:]),
		Event: book.Event{
			Seq:       le.Uint64(e),
			Timestamp: int64(le.Uint64(e[8:])),
			Kind:      book.EventKind(e[16]), Side: book.Side(e[17]), Reason: book.Reason(e[18]), LevelSide: book.Side(e[19]),
			LevelOrders: le.Uint32(e[20:]),
			OrderID:     book.OrderID(le.Uint64(e[24:])),
			MakerID:     book.OrderID(le.Uint64(e[32:])),
			Price:       book.Price(le.Uint64(e[40:])),
			Qty:         book.Qty(le.Uint64(e[48:])),
			LevelQty:    book.Qty(le.Uint64(e[56:])),
		},
	}
}

// Done ends the reply to a command, after its events. Reason is the
// validation reason when Status is StatusInvalid, and otherwise the reason
// of the order's status, if any. OrderID is the order the command was
// about, 0 if it got no ID, and State the order's state after the command,
// 0 if there is no order status.
type Done struct {
	ServerSeq uint64
	ClientSeq uint64
	Status    Status
	Reason    book.Reason
	OrderID   book.OrderID
	State     engine.OrderState
}

func (m Done) Append(dst []byte) []byte {
	dst = header(dst, TypeDone)
	dst = le.AppendUint64(dst, m.ServerSeq)
	dst = le.AppendUint64(dst, m.ClientSeq)
	dst = append(dst, byte(m.Status), byte(m.Reason))
	dst = le.AppendUint64(dst, uint64(m.OrderID))
	return append(dst, byte(m.State))
}

func DecodeDone(b []byte) Done {
	return Done{
		ServerSeq: le.Uint64(b),
		ClientSeq: le.Uint64(b[8:]),
		Status:    Status(b[16]), Reason: book.Reason(b[17]),
		OrderID: book.OrderID(le.Uint64(b[18:])),
		State:   engine.OrderState(b[26]),
	}
}

// Pong answers the Ping with the same ClientSeq. It is the size of a
// Done, its body padded with zeros, and carries no server_seq.
type Pong struct{ ClientSeq uint64 }

func (m Pong) Append(dst []byte) []byte {
	dst = header(dst, TypePong)
	dst = le.AppendUint64(dst, m.ClientSeq)
	return append(dst, make([]byte, 19)...)
}

func DecodePong(b []byte) Pong { return Pong{ClientSeq: le.Uint64(b)} }

// EndOfSession comes before the server ends a session itself.
type EndOfSession struct{}

func (EndOfSession) Append(dst []byte) []byte { return header(dst, TypeEndOfSession) }
