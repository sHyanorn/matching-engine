package book

import (
	"math/rand/v2"
	"slices"
	"testing"
)

// fuzzCommandSize is how many input bytes decodeCommands turns into one
// command; maxFuzzCommands caps the commands taken from one input.
const (
	fuzzCommandSize = 5
	maxFuzzCommands = 512
)

// decodeCommands turns fuzz input into commands, fuzzCommandSize bytes each
// (a trailing partial command is ignored). The mapping is biased toward the
// states where bugs hide: IDs come from a space of 16, so duplicates, reuse
// and cancels of known and unknown orders are frequent, and prices from a
// 16-tick band, so orders cross, sweep several levels and empty them. Two
// flag bits produce extreme values and invalid commands of every kind.
//
//	byte 0: bits 0-1 operation (cancel when 2, new order otherwise),
//	        bits 2-4 order type and TIF, bit 5 side,
//	        bit 6 extreme values, bit 7 invalid command
//	byte 1: order ID, 1 + b%16
//	byte 2: price, 92 + b%16
//	byte 3: quantity, 1 + b%16
//	byte 4: which field an invalid command breaks
func decodeCommands(data []byte) []Command {
	n := min(len(data)/fuzzCommandSize, maxFuzzCommands)
	cmds := make([]Command, 0, n)
	for i := range n {
		b := data[i*fuzzCommandSize : (i+1)*fuzzCommandSize]
		ctl := b[0]
		id := OrderID(1 + b[1]%16)
		price := Price(92 + b[2]%16)
		qty := Qty(1 + b[3]%16)
		side := Buy
		if ctl&0x20 != 0 {
			side = Sell
		}
		if ctl&0x40 != 0 { // extreme but valid values
			if b[2]&1 == 0 {
				price = 1
			} else {
				price = MaxPrice
			}
			qty = MaxQty - Qty(b[3]%2)
		}

		var c Command
		if ctl&0x03 == 2 {
			c = cancel(id)
		} else {
			switch (ctl >> 2) & 0x07 {
			case 0, 1, 2:
				c = limit(id, side, price, qty)
			case 3:
				c = ioc(id, side, price, qty)
			case 4:
				c = fok(id, side, price, qty)
			case 5:
				c = market(id, side, qty)
			case 6:
				c = marketFOK(id, side, qty)
			default: // 7: a limit order, made invalid below if bit 7 says so
				c = limit(id, side, price, qty)
			}
		}

		if ctl&0x80 != 0 {
			switch b[4] % 12 {
			case 0:
				c.Kind = CommandKind(3 + b[4]/12)
			case 1:
				c.OrderID = 0
			case 2:
				c.Side = Side(3 + b[4]/12)
			case 3:
				c.Type = OrderType(3 + b[4]/12)
			case 4:
				c.TIF = TIF(4 + b[4]/12)
			case 5:
				c.Type, c.TIF, c.Price = Market, GTC, 0
			case 6:
				c.Price = -Price(b[4])
			case 7:
				c.Price = MaxPrice + 1 + Price(b[4])
			case 8:
				c.Qty = -Qty(b[4])
			case 9:
				c.Qty = MaxQty + 1 + Qty(b[4])
			case 10: // a cancel with a field only new orders use
				c = cancel(id)
				c.Price = price
			case 11: // a market order with a price
				c = market(id, side, qty)
				c.Price = price
			}
		}
		cmds = append(cmds, at(int64(i+1), c))
	}
	return cmds
}

// FuzzBook applies the commands decoded from arbitrary input to a fresh
// book and requires that nothing panics and that, after every command,
// CheckInvariants passes, Seq is gap-free and the events match an
// independent derivation from the resting orders before the command
// (checkCommandEvents). At the end it checks quantity conservation per
// order and that replaying the commands gives the identical event stream
// and book.
func FuzzBook(f *testing.F) {
	// Seeds: an empty input, a short hand-written session (rest, cross,
	// partial fill, cancel, FOK, market) and a few seeded random inputs.
	f.Add([]byte{})
	f.Add([]byte{
		0x20, 1, 8, 4, 0, // sell 2 @ 100 x5 GTC
		0x20, 2, 9, 2, 0, // sell 3 @ 101 x3 GTC
		0x00, 3, 9, 6, 0, // buy 4 @ 101 x7 GTC: sweeps both, rests 0
		0x02, 2, 0, 0, 0, // cancel 3
		0x10, 4, 15, 15, 0, // buy FOK 5 @ 107 x16: rejected
		0x34, 5, 0, 3, 0, // sell market 6 x4
		0x80, 6, 8, 1, 6, // invalid: negative price
	})
	r := rand.New(rand.NewPCG(7, 7))
	for range 4 {
		seed := make([]byte, 600)
		for i := range seed {
			seed[i] = byte(r.Uint32())
		}
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		cmds := decodeCommands(data)
		b := New()
		lg := ledger{}
		var stream []Event
		before := make(map[OrderID]Order)
		for i, c := range cmds {
			clear(before)
			for id, o := range b.orders {
				before[id] = o.Order
			}
			evs := apply(t, b, c) // Seq, Timestamp and CheckInvariants
			if err := checkCommandEvents(c, evs, before); err != nil {
				t.Fatalf("command %d %+v: %v\nevents:%s", i, c, err, formatEvents(evs))
			}
			lg.record(evs)
			stream = append(stream, evs...)
		}
		if err := lg.check(b); err != nil {
			t.Fatal(err)
		}
		replay := New()
		var again []Event
		for _, c := range cmds {
			again = replay.Apply(c, again)
		}
		if !slices.Equal(stream, again) {
			t.Fatal("replay produced a different event stream")
		}
	})
}

// TestDecodeCommands pins the decoder's mapping, so a change to it is a
// deliberate change to what the fuzz corpus means.
func TestDecodeCommands(t *testing.T) {
	got := decodeCommands([]byte{
		0x20, 1, 8, 4, 0, // sell limit
		0x0c, 17, 0, 15, 0, // buy IOC, ID wraps to 2, price 92, qty 16
		0x02, 3, 0, 0, 0, // cancel 4
		0x54, 4, 1, 1, 0, // 0x54: extreme values, market, buy
		0x80, 5, 8, 4, 6, // invalid: price -6
		0x20, 1, // trailing partial command, ignored
	})
	want := []Command{
		at(1, limit(2, Sell, 100, 5)),
		at(2, ioc(2, Buy, 92, 16)),
		at(3, cancel(4)),
		at(4, market(5, Buy, MaxQty-1)),
		at(5, Command{Kind: CommandNewOrder, OrderID: 6, Side: Buy, Type: Limit, TIF: GTC, Price: -6, Qty: 5}),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("decodeCommands =\n%+v\nwant\n%+v", got, want)
	}
	if n := len(decodeCommands(make([]byte, fuzzCommandSize*(maxFuzzCommands+10)))); n != maxFuzzCommands {
		t.Fatalf("decoded %d commands, want the cap %d", n, maxFuzzCommands)
	}
}
