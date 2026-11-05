package api

import (
	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/engine"
)

// The JSON shapes of docs/API.md. Prices and quantities are int64, so a
// number with a fraction, or a string, fails to decode.

type orderRequest struct {
	Symbol   string `json:"symbol"`
	Side     string `json:"side"`
	Type     string `json:"type"`
	TIF      string `json:"tif"`
	Price    int64  `json:"price"`
	Qty      int64  `json:"qty"`
	ClientID string `json:"client_id"`
}

type orderJSON struct {
	OrderID      book.OrderID `json:"order_id"`
	Symbol       string       `json:"symbol"`
	ClientID     string       `json:"client_id"`
	Side         string       `json:"side"`
	Type         string       `json:"type"`
	TIF          string       `json:"tif"`
	Price        int64        `json:"price"`
	Qty          int64        `json:"qty"`
	FilledQty    int64        `json:"filled_qty"`
	RemainingQty int64        `json:"remaining_qty"`
	Status       string       `json:"status"`
	Reason       string       `json:"reason"`
	TS           int64        `json:"ts"`
}

type eventJSON struct {
	Seq       uint64       `json:"seq"`
	TS        int64        `json:"ts"`
	Type      string       `json:"type"`
	OrderID   book.OrderID `json:"order_id"`
	MakerID   book.OrderID `json:"maker_id,omitempty"`
	Side      string       `json:"side,omitempty"`
	Price     int64        `json:"price,omitempty"`
	Qty       int64        `json:"qty,omitempty"`
	Reason    string       `json:"reason,omitempty"`
	LevelSide string       `json:"level_side,omitempty"`
	LevelQty  *int64       `json:"level_qty,omitempty"` // present whenever LevelSide is
}

type levelJSON struct {
	Price  int64 `json:"price"`
	Qty    int64 `json:"qty"`
	Orders int   `json:"orders"`
}

type orderResponse struct {
	Order  orderJSON   `json:"order"`
	Events []eventJSON `json:"events"`
}

// appliedFailure answers a command that took effect although writing or
// syncing its log record failed: the error, then the order (if any) and
// the events, so the client learns what happened to its order.
type appliedFailure struct {
	errorBody
	Order  *orderJSON  `json:"order,omitempty"`
	Events []eventJSON `json:"events"`
}

type bookResponse struct {
	Symbol string      `json:"symbol"`
	Seq    uint64      `json:"seq"`
	Bids   []levelJSON `json:"bids"`
	Asks   []levelJSON `json:"asks"`
}

// reasonName is r's code, or "" for ReasonNone.
func reasonName(r book.Reason) string {
	if r == book.ReasonNone {
		return ""
	}
	return r.String()
}

// sideName is s's wire name, or "" for no side.
func sideName(s book.Side) string {
	if !s.Valid() {
		return ""
	}
	return s.String()
}

func toOrderJSON(s engine.OrderStatus) orderJSON {
	return orderJSON{
		OrderID: s.ID, Symbol: s.Symbol, ClientID: s.ClientID,
		Side: s.Side.String(), Type: s.Type.String(), TIF: s.TIF.String(),
		Price: int64(s.Price), Qty: int64(s.Qty),
		FilledQty: int64(s.Filled), RemainingQty: int64(s.Remaining),
		Status: s.State.String(), Reason: reasonName(s.Reason), TS: s.Timestamp,
	}
}

func toEventsJSON(evs []book.Event) []eventJSON {
	out := make([]eventJSON, len(evs))
	for i, e := range evs {
		out[i] = eventJSON{
			Seq: e.Seq, TS: e.Timestamp, Type: e.Kind.String(), OrderID: e.OrderID, MakerID: e.MakerID,
			Side: sideName(e.Side), Price: int64(e.Price), Qty: int64(e.Qty), Reason: reasonName(e.Reason),
		}
		if e.LevelSide.Valid() {
			q := int64(e.LevelQty)
			out[i].LevelSide, out[i].LevelQty = e.LevelSide.String(), &q
		}
	}
	return out
}

func toLevelsJSON(lv []book.Level) []levelJSON {
	out := make([]levelJSON, len(lv)) // [] rather than null for an empty side
	for i, l := range lv {
		out[i] = levelJSON{Price: int64(l.Price), Qty: int64(l.Qty), Orders: l.Orders}
	}
	return out
}
