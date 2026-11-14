// Package harness puts a [book.Book] behind the messages and reports of the
// flash1-dev matching engine benchmark
// (github.com/flash1-dev/matching-engine-benchmark), so that the benchmark
// can drive the matching core through cmd/harness-adapter. It is plain Go;
// the C ABI is the adapter's.
//
// The benchmark sends three messages: a new limit order, GTC or IOC; a
// cancel; and a modify, which it defines as a cancel followed by a new GTC
// order with the same ID at the new price and quantity, so the order loses
// its time priority. The book has no modify, and the mapping does exactly
// that. Each message yields the reports the benchmark asks for:
//
//	message  book events                         reports
//	new      Accepted, Trade..., then Rested     OrderAck, Trade...
//	         or Canceled (IOC remainder)         and a CancelAck for it
//	cancel   Canceled, or Rejected               CancelAck, or CancelReject
//	modify   Canceled, then as a new order       ModifyAck, Trade...
//	         Rejected (cancel half)              ModifyReject
//
// Every report carries the message's sequence number, and a trade the
// maker's price, as in the book. A cancel or modify of an order that is not
// resting is rejected whatever the reason: never seen, filled, canceled,
// or ID 0, which no order can have.
//
// The benchmark's prices are signed ticks and its quantities uint32; the
// book takes prices in [1, book.MaxPrice] and quantities in
// [1, book.MaxQty]. A new order or modify outside that domain is an error,
// not a report: the benchmark has no report for a rejected new order, and
// its workloads stay far inside the domain. Such a modify fails after its
// cancel half has removed the order; the adapter ends the run on any error.
package harness

import (
	"fmt"
	"math"

	"github.com/sHyanorn/matching-engine/internal/book"
)

// MsgType is the kind of a benchmark message, numbered as in the
// benchmark's ABI.
type MsgType uint8

// The message types.
const (
	MsgNew    MsgType = 0
	MsgCancel MsgType = 1
	MsgModify MsgType = 2
)

// Msg is one benchmark message. A cancel uses only Seq and OrderID. A
// modify carries the new price and quantity and the order's side; IOC is
// for new orders only.
type Msg struct {
	Type    MsgType
	Seq     uint64 // sequence number, copied into every report
	OrderID uint64
	Price   int64 // ticks
	Qty     uint32
	Side    uint8 // 0 buy, 1 sell
	IOC     bool
}

// ReportType is the kind of a report, numbered as in the benchmark's ABI.
type ReportType uint8

// The report types.
const (
	OrderAck     ReportType = 0
	Trade        ReportType = 1
	CancelAck    ReportType = 2
	ModifyAck    ReportType = 3
	CancelReject ReportType = 4
	ModifyReject ReportType = 5
)

// Report is one report, with the fields of the benchmark's me_report_t. A
// field a type does not use is zero: a Trade sets Seq, Price, Qty, MakerID
// and TakerID; a reject sets Seq and OrderID; the others set Side, Seq,
// OrderID, Price and Qty, the price and quantity of the order or, for a
// CancelAck, of what was canceled.
type Report struct {
	Type    ReportType
	Side    uint8 // 0 buy, 1 sell
	Seq     uint64
	OrderID uint64
	Price   int64
	Qty     uint32
	MakerID uint64
	TakerID uint64
}

// Engine is one book behind the benchmark's messages, with scratch space
// for the book's events. It is not safe for concurrent use; the benchmark
// calls it from one thread.
type Engine struct {
	book   *book.Book
	events []book.Event
	levels []book.Level
}

// New returns an engine with an empty book.
func New() *Engine {
	return &Engine{book: book.New(), events: make([]book.Event, 0, 64)}
}

// Apply applies m to the book and appends its reports to out, which it
// returns. It returns an error, and appends nothing, for a message of an
// unknown type and for a new order or modify the book cannot hold.
func (e *Engine) Apply(m Msg, out []Report) ([]Report, error) {
	switch m.Type {
	case MsgNew:
		return e.order(m, OrderAck, out)
	case MsgCancel:
		if !e.cancel(m.OrderID) {
			return append(out, Report{Type: CancelReject, Seq: m.Seq, OrderID: m.OrderID}), nil
		}
		ev := e.events[0]
		return append(out, Report{
			Type: CancelAck, Side: side(ev.Side), Seq: m.Seq, OrderID: m.OrderID,
			Price: int64(ev.Price), Qty: uint32(ev.Qty),
		}), nil
	case MsgModify:
		if !e.cancel(m.OrderID) {
			return append(out, Report{Type: ModifyReject, Seq: m.Seq, OrderID: m.OrderID}), nil
		}
		m.IOC = false
		return e.order(m, ModifyAck, out)
	}
	return out, fmt.Errorf("message %d: unknown type %d", m.Seq, m.Type)
}

// cancel cancels order id and reports whether it was resting. Its
// Canceled event is then e.events[0].
func (e *Engine) cancel(id uint64) bool {
	e.events = e.book.Apply(book.Command{Kind: book.CommandCancel, OrderID: book.OrderID(id)}, e.events[:0])
	return e.events[0].Kind == book.EventCanceled
}

// order applies the limit order m describes and appends its reports, ack
// for its Accepted event.
func (e *Engine) order(m Msg, ack ReportType, out []Report) ([]Report, error) {
	tif := book.GTC
	if m.IOC {
		tif = book.IOC
	}
	e.events = e.book.Apply(book.Command{
		Kind: book.CommandNewOrder, OrderID: book.OrderID(m.OrderID), Side: bookSide(m.Side),
		Type: book.Limit, TIF: tif, Price: book.Price(m.Price), Qty: book.Qty(m.Qty),
	}, e.events[:0])
	for _, ev := range e.events {
		switch ev.Kind {
		case book.EventAccepted:
			out = append(out, Report{
				Type: ack, Side: side(ev.Side), Seq: m.Seq, OrderID: m.OrderID,
				Price: int64(ev.Price), Qty: uint32(ev.Qty),
			})
		case book.EventTrade:
			out = append(out, Report{
				Type: Trade, Seq: m.Seq, Price: int64(ev.Price), Qty: uint32(ev.Qty),
				MakerID: uint64(ev.MakerID), TakerID: uint64(ev.OrderID),
			})
		case book.EventCanceled: // the remainder of an IOC order
			out = append(out, Report{
				Type: CancelAck, Side: side(ev.Side), Seq: m.Seq, OrderID: m.OrderID,
				Price: int64(ev.Price), Qty: uint32(ev.Qty),
			})
		case book.EventRejected: // the only event of a rejected command
			return out, fmt.Errorf("message %d: order %d side %d price %d qty %d: %v",
				m.Seq, m.OrderID, m.Side, m.Price, m.Qty, ev.Reason)
		default: // Rested: the benchmark has no report for it
		}
	}
	return out, nil
}

// BestBid returns the best bid price, or math.MinInt64 if no bid rests.
func (e *Engine) BestBid() int64 {
	if e.levels = e.book.Depth(book.Buy, 1, e.levels[:0]); len(e.levels) > 0 {
		return int64(e.levels[0].Price)
	}
	return math.MinInt64
}

// BestAsk returns the best ask price, or math.MaxInt64 if no ask rests.
func (e *Engine) BestAsk() int64 {
	if e.levels = e.book.Depth(book.Sell, 1, e.levels[:0]); len(e.levels) > 0 {
		return int64(e.levels[0].Price)
	}
	return math.MaxInt64
}

// DepthAt returns the quantity resting at price on side, 0 buy and 1 sell,
// or 0 if there is none. It walks the side's levels from the best price,
// O(P) for P levels; the benchmark calls it only at its audit points,
// outside the timed work.
func (e *Engine) DepthAt(price int64, s uint8) uint64 {
	e.levels = e.book.Depth(bookSide(s), math.MaxInt, e.levels[:0])
	for _, l := range e.levels {
		if int64(l.Price) == price {
			return uint64(l.Qty)
		}
	}
	return 0
}

// bookSide returns the book's side for the benchmark's, 0 buy and 1 sell,
// and the invalid zero Side, which the book rejects, for any other value.
func bookSide(s uint8) book.Side {
	switch s {
	case 0:
		return book.Buy
	case 1:
		return book.Sell
	}
	return 0
}

// side returns the benchmark's side for the book's valid side s.
func side(s book.Side) uint8 {
	if s == book.Sell {
		return 1
	}
	return 0
}
