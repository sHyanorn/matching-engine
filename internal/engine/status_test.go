package engine

import (
	"context"
	"testing"

	"github.com/sHyanorn/matching-engine/internal/book"
)

// TestOrderStatusLifecycle follows orders through every way they can end
// and checks the status the engine reports, both in each command's Result
// and from Order afterwards.
func TestOrderStatusLifecycle(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t, Config{Symbols: []string{"A"}})
	place := func(o NewOrder) Result {
		t.Helper()
		r, err := e.PlaceOrder(ctx, o)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	check := func(name string, id book.OrderID, state OrderState, reason book.Reason, filled, remaining book.Qty) {
		t.Helper()
		st, found, err := e.Order(ctx, id)
		if err != nil || !found || st.State != state || st.Reason != reason || st.Filled != filled || st.Remaining != remaining || st.Symbol != "A" {
			t.Fatalf("%s: status %+v, found %v, err %v; want %v %v filled %d remaining %d", name, st, found, err, state, reason, filled, remaining)
		}
	}

	maker := place(NewOrder{Symbol: "A", Side: book.Sell, Type: book.Limit, TIF: book.GTC, Price: 100, Qty: 10, ClientID: "m"})
	check("resting maker", maker.OrderID, StateOpen, book.ReasonNone, 0, 10)
	if maker.Status.State != StateOpen || maker.Status.ClientID != "m" || maker.Status.RestedSeq != 2 {
		t.Fatalf("maker result status = %+v", maker.Status)
	}

	full := place(limitOrder("A", book.Buy, 100, 4)) // fills against the maker
	check("taker filled", full.OrderID, StateFilled, book.ReasonNone, 4, 0)
	check("maker partly filled", maker.OrderID, StateOpen, book.ReasonNone, 4, 6)

	ioc := place(NewOrder{Symbol: "A", Side: book.Buy, Type: book.Limit, TIF: book.IOC, Price: 100, Qty: 8})
	check("IOC remainder", ioc.OrderID, StateCanceled, book.ReasonIOCRemainder, 6, 0)
	check("maker filled", maker.OrderID, StateFilled, book.ReasonNone, 10, 0)

	fok := place(NewOrder{Symbol: "A", Side: book.Buy, Type: book.Limit, TIF: book.FOK, Price: 100, Qty: 1})
	check("FOK rejected", fok.OrderID, StateRejected, book.ReasonFOKUnfillable, 0, 0)

	mkt := place(NewOrder{Symbol: "A", Side: book.Sell, Type: book.Market, TIF: book.IOC, Qty: 3})
	check("market into an empty side", mkt.OrderID, StateCanceled, book.ReasonIOCRemainder, 0, 0)

	// A cancel of a finished order is rejected by the book, but its Result
	// still carries the order's final status.
	c, err := e.CancelOrder(ctx, full.OrderID)
	if err != nil || c.Events[0].Reason != book.ReasonUnknownOrder || c.Status.State != StateFilled {
		t.Fatalf("cancel of a filled order: %+v, %v", c, err)
	}
}

// TestOrderHistoryIsBounded keeps only the most recently finished orders.
func TestOrderHistoryIsBounded(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t, Config{Symbols: []string{"A"}, History: 3})
	var ids []book.OrderID
	for range 5 { // IOC orders into an empty book finish at once
		r, err := e.PlaceOrder(ctx, NewOrder{Symbol: "A", Side: book.Buy, Type: book.Limit, TIF: book.IOC, Price: 100, Qty: 1})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.OrderID)
	}
	for i, id := range ids {
		_, found, _ := e.Order(ctx, id)
		if want := i >= 2; found != want {
			t.Errorf("order %d of 5: found %v, want %v", i+1, found, want)
		}
	}
}
