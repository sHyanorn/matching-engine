package book_test

import (
	"fmt"
	"slices"

	"github.com/sHyanorn/matching-engine/internal/book"
)

// The API layer parses the wire names of a POST /v1/orders body, builds a
// Command and validates it before the engine sees it. Parse failures are
// ignored on purpose: the invalid zero value makes Validate report the
// error, so a missing and a misspelled field get the same code.
func ExampleCommand_Validate() {
	newOrder := func(side, typ, tif string, price book.Price, qty book.Qty) book.Command {
		s, _ := book.ParseSide(side)
		ty, _ := book.ParseOrderType(typ)
		tf, _ := book.ParseTIF(tif)
		return book.Command{
			Kind: book.CommandNewOrder, OrderID: 1,
			Side: s, Type: ty, TIF: tf, Price: price, Qty: qty,
		}
	}
	fmt.Println(newOrder("buy", "limit", "gtc", 6500000, 150).Validate())
	fmt.Println(newOrder("", "limit", "gtc", 6500000, 150).Validate())
	fmt.Println(newOrder("long", "limit", "gtc", 6500000, 150).Validate())
	fmt.Println(newOrder("sell", "market", "gtc", 0, 150).Validate())
	fmt.Println(newOrder("sell", "market", "ioc", 6500000, 150).Validate())
	fmt.Println(newOrder("sell", "limit", "fok", 6500000, 0).Validate())
	// Output:
	// none
	// invalid_side
	// invalid_side
	// market_gtc
	// invalid_price
	// invalid_qty
}

// The engine turns one command's events into WebSocket messages: a trade
// message per Trade, and one book_delta entry per changed level, keeping the
// last absolute total. Here a limit buy of 15 @ 101 meets asks of 10 @ 100
// (order 1) and 3 + 5 @ 101 (orders 2 and 3).
func ExampleEvent_LevelUpdate() {
	events := []book.Event{
		{Seq: 7, Kind: book.EventAccepted, OrderID: 9, Side: book.Buy, Price: 101, Qty: 15},
		{Seq: 8, Kind: book.EventTrade, OrderID: 9, MakerID: 1, Side: book.Buy, Price: 100, Qty: 10, LevelSide: book.Sell, LevelQty: 0},
		{Seq: 9, Kind: book.EventTrade, OrderID: 9, MakerID: 2, Side: book.Buy, Price: 101, Qty: 3, LevelSide: book.Sell, LevelQty: 5},
		{Seq: 10, Kind: book.EventTrade, OrderID: 9, MakerID: 3, Side: book.Buy, Price: 101, Qty: 2, LevelSide: book.Sell, LevelQty: 3},
	}

	var deltas []book.LevelUpdate
	for _, ev := range events {
		if tr, ok := ev.Trade(); ok {
			fmt.Printf("trade #%d: %v %d @ %d, taker %d, maker %d\n", tr.Seq, tr.TakerSide, tr.Qty, tr.Price, tr.TakerID, tr.MakerID)
		}
		if lu, ok := ev.LevelUpdate(); ok {
			i := slices.IndexFunc(deltas, func(d book.LevelUpdate) bool { return d.Side == lu.Side && d.Price == lu.Price })
			if i < 0 {
				deltas = append(deltas, lu)
			} else {
				deltas[i] = lu
			}
		}
	}
	for _, d := range deltas {
		fmt.Printf("book_delta: %v %d -> %d\n", d.Side, d.Price, d.Qty)
	}
	// Output:
	// trade #8: buy 10 @ 100, taker 9, maker 1
	// trade #9: buy 3 @ 101, taker 9, maker 2
	// trade #10: buy 2 @ 101, taker 9, maker 3
	// book_delta: sell 100 -> 0
	// book_delta: sell 101 -> 3
}
