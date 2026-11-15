package engine_test

import (
	"bytes"
	"testing"

	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/engine"
	"github.com/sHyanorn/matching-engine/internal/oe"
)

// TestOEMatchesLog holds the command fields of OE1's NewOrder and Cancel
// to the bytes the command log writes for them (D041), so the two
// encodings cannot drift apart. Every field has bytes of its own.
func TestOEMatchesLog(t *testing.T) {
	order := book.Command{
		Kind: book.CommandNewOrder, OrderID: 0x0807060504030201,
		Side: book.Sell, Type: book.Limit, TIF: book.FOK,
		Price: 0x3837363534333231, Qty: 0x4847464544434241, Timestamp: 0x5857565554535251,
	}
	log := engine.LogPayload(order)
	wire := oe.NewOrder{ClientSeq: 0x6867666564636261, Symbol: 0x71,
		Side: order.Side, Type: order.Type, TIF: order.TIF, Price: order.Price, Qty: order.Qty}.Append(nil)
	// Log payload: kind 0, side 1, type 2, tif 3, order ID 4:12, price
	// 12:20, qty 20:28, timestamp 28:36. NewOrder: header 0:3, client_seq
	// 3:11, symbol 11, side 12, type 13, tif 14, price 15:23, qty 23:31.
	if !bytes.Equal(wire[12:15], log[1:4]) || !bytes.Equal(wire[15:31], log[12:28]) {
		t.Errorf("NewOrder side, type, tif, price, qty = %x %x; the log writes %x %x", wire[12:15], wire[15:31], log[1:4], log[12:28])
	}

	cancel := book.Command{Kind: book.CommandCancel, OrderID: 0x0807060504030201, Timestamp: 0x5857565554535251}
	log = engine.LogPayload(cancel)
	wire = oe.Cancel{ClientSeq: 0x6867666564636261, OrderID: cancel.OrderID}.Append(nil)
	// Cancel: header 0:3, client_seq 3:11, order_id 11:19.
	if !bytes.Equal(wire[11:19], log[4:12]) {
		t.Errorf("Cancel order_id = %x; the log writes %x", wire[11:19], log[4:12])
	}
}
