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
// and cancels of known and unknown orders are frequent; prices come from a
// narrow band, by default with buys 6 ticks low and sells 6 ticks high, so
// both sides build several levels that overlap, cross and empty. Extreme
// values and invalid commands are rare (a byte value of 0xF0 or more) and
// chosen independently, so most of an input builds real books.
//
//	byte 0: bits 0-1 operation (cancel when 2, new order otherwise),
//	        bits 2-4 order type and TIF, bit 5 side, bits 6-7 unused
//	byte 1: order ID, 1 + b%16
//	byte 2: b >= 0xF0: extreme price, 1 for even b, MaxPrice for odd;
//	        otherwise 92 + b%16, moved 6 ticks away from the other side
//	        (down for a buy, up for a sell) unless bit 4 is set
//	byte 3: b >= 0xF0: extreme quantity, MaxQty - b%2; otherwise 1 + b%16
//	byte 4: b >= 0xF0: an invalid command, b%12 says which field it breaks
func decodeCommands(data []byte) []Command {
	n := min(len(data)/fuzzCommandSize, maxFuzzCommands)
	cmds := make([]Command, 0, n)
	for i := range n {
		b := data[i*fuzzCommandSize : (i+1)*fuzzCommandSize]
		ctl := b[0]
		id := OrderID(1 + b[1]%16)
		side := Buy
		if ctl&0x20 != 0 {
			side = Sell
		}

		var price Price
		switch {
		case b[2] >= 0xF0 && b[2]&1 == 0:
			price = 1
		case b[2] >= 0xF0:
			price = MaxPrice
		default:
			price = Price(92 + b[2]%16)
			if b[2]&0x10 == 0 {
				if side == Buy {
					price -= 6
				} else {
					price += 6
				}
			}
		}
		qty := Qty(1 + b[3]%16)
		if b[3] >= 0xF0 {
			qty = MaxQty - Qty(b[3]%2)
		}

		var c Command
		if ctl&0x03 == 2 {
			c = cancel(id)
		} else {
			switch (ctl >> 2) & 0x07 {
			case 3:
				c = ioc(id, side, price, qty)
			case 4:
				c = fok(id, side, price, qty)
			case 5:
				c = market(id, side, qty)
			case 6:
				c = marketFOK(id, side, qty)
			default: // 0, 1, 2 and 7: a GTC limit order
				c = limit(id, side, price, qty)
			}
		}

		if b[4] >= 0xF0 {
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
// CheckInvariants passes, Seq is gap-free, the events match checkCommandEvents
// fed with the resting book as the event stream alone describes it
// (restingModel), the book's resting orders equal that model, and any order
// whose ID is reused conserved its quantity. At the end it checks
// conservation for every order and that replaying the commands gives the
// identical event stream and book state.
func FuzzBook(f *testing.F) {
	// Seeds: an empty input; a deep session with several orders on each of
	// three levels per side, then a FOK that fills across levels, a FOK
	// rejected as unfillable, a market sweep and an invalid command; and a
	// few seeded random inputs. Plain go test runs only these.
	f.Add([]byte{})
	f.Add([]byte{
		// Bids at 92, 93 and 94, two orders each (byte 2 has bit 4 set: no
		// side offset, so the price is 92 + b%16).
		0x00, 0, 0x10, 2, 0, // buy 1 @ 92 x3
		0x00, 1, 0x10, 1, 0, // buy 2 @ 92 x2
		0x00, 2, 0x11, 3, 0, // buy 3 @ 93 x4
		0x00, 3, 0x11, 0, 0, // buy 4 @ 93 x1
		0x00, 4, 0x12, 1, 0, // buy 5 @ 94 x2
		0x00, 5, 0x12, 4, 0, // buy 6 @ 94 x5
		// Asks at 102, 103 and 104, two orders each.
		0x20, 6, 0x1a, 2, 0, // sell 7 @ 102 x3
		0x20, 7, 0x1a, 1, 0, // sell 8 @ 102 x2
		0x20, 8, 0x1b, 3, 0, // sell 9 @ 103 x4
		0x20, 9, 0x1b, 0, 0, // sell 10 @ 103 x1
		0x20, 10, 0x1c, 1, 0, // sell 11 @ 104 x2
		0x20, 11, 0x1c, 4, 0, // sell 12 @ 104 x5
		0x10, 12, 0x1b, 8, 0, // buy FOK 13 @ 103 x9: fills 102 and most of 103
		0x30, 13, 0x12, 15, 0, // sell FOK 14 @ 94 x16: only 7 crossable, rejected
		0x14, 14, 0x10, 5, 0, // buy market 15 x6: takes 103's last lot and 5 of 104's 7
		0x00, 15, 0x10, 0, 0xf6, // invalid: price -246
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
		model := newRestingModel()
		lg := ledger{}
		var stream []Event
		for i, c := range cmds {
			evs := apply(t, b, c) // Seq, Timestamp and CheckInvariants
			if err := checkCommandEvents(c, evs, model.orders); err != nil {
				t.Fatalf("command %d %+v: %v\nevents:%s", i, c, err, formatEvents(evs))
			}
			if err := model.record(evs); err != nil {
				t.Fatalf("command %d %+v: %v", i, c, err)
			}
			if err := model.check(b); err != nil {
				t.Fatalf("command %d %+v: %v", i, c, err)
			}
			if err := lg.record(evs); err != nil {
				t.Fatalf("command %d %+v: %v", i, c, err)
			}
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
		if err := sameBook(b, replay); err != nil {
			t.Fatalf("replay: %v", err)
		}
	})
}

// TestDecodeCommands pins the decoder's mapping branch by branch, so a
// change to it is a deliberate change to what the fuzz corpus means, and the
// decoder cannot silently stop producing some kind of command.
func TestDecodeCommands(t *testing.T) {
	base := limit(1, Buy, 92, 1) // byte 2 = 0x10: price 92 with no side offset
	with := func(edit func(*Command)) Command {
		c := base
		edit(&c)
		return c
	}
	tests := []struct {
		name string
		in   [fuzzCommandSize]byte
		want Command
	}{
		{"limit, type 0", [5]byte{0x00, 0, 0x10, 0, 0}, base},
		{"limit, type 1, op 1", [5]byte{0x05, 0, 0x11, 1, 0}, limit(1, Buy, 93, 2)},
		{"limit, type 2, op 3", [5]byte{0x0b, 0, 0x11, 1, 0}, limit(1, Buy, 93, 2)},
		{"limit, type 7", [5]byte{0x1c, 0, 0x10, 0, 0}, base},
		{"bits 6-7 ignored", [5]byte{0xc0, 0, 0x10, 0, 0}, base},
		{"IOC", [5]byte{0x0c, 0, 0x10, 0, 0}, ioc(1, Buy, 92, 1)},
		{"FOK", [5]byte{0x10, 0, 0x10, 0, 0}, fok(1, Buy, 92, 1)},
		{"market", [5]byte{0x14, 0, 0x10, 0, 0}, market(1, Buy, 1)},
		{"market FOK, sell", [5]byte{0x38, 0, 0x10, 0, 0}, marketFOK(1, Sell, 1)},
		{"cancel, ID wraps", [5]byte{0x02, 19, 0x10, 0, 0}, cancel(4)},
		{"buy offset down", [5]byte{0x00, 0, 0x00, 0, 0}, limit(1, Buy, 86, 1)},
		{"sell offset up", [5]byte{0x20, 0, 0x0f, 0, 0}, limit(1, Sell, 113, 1)},
		{"extreme price 1", [5]byte{0x00, 0, 0xf0, 0, 0}, limit(1, Buy, 1, 1)},
		{"extreme price max", [5]byte{0x00, 0, 0xf1, 0, 0}, limit(1, Buy, MaxPrice, 1)},
		{"extreme qty max", [5]byte{0x00, 0, 0x10, 0xf0, 0}, limit(1, Buy, 92, MaxQty)},
		{"extreme qty max-1", [5]byte{0x00, 0, 0x10, 0xf1, 0}, limit(1, Buy, 92, MaxQty-1)},
		{"valid just below the invalid range", [5]byte{0x00, 0, 0x10, 0, 0xef}, base},
		{"invalid 0: kind", [5]byte{0x00, 0, 0x10, 0, 0xf0}, with(func(c *Command) { c.Kind = 23 })},
		{"invalid 1: order ID", [5]byte{0x00, 0, 0x10, 0, 0xf1}, with(func(c *Command) { c.OrderID = 0 })},
		{"invalid 2: side", [5]byte{0x00, 0, 0x10, 0, 0xf2}, with(func(c *Command) { c.Side = 23 })},
		{"invalid 3: type", [5]byte{0x00, 0, 0x10, 0, 0xf3}, with(func(c *Command) { c.Type = 23 })},
		{"invalid 4: TIF", [5]byte{0x00, 0, 0x10, 0, 0xf4}, with(func(c *Command) { c.TIF = 24 })},
		{"invalid 5: market GTC", [5]byte{0x00, 0, 0x10, 0, 0xf5}, with(func(c *Command) { c.Type, c.TIF, c.Price = Market, GTC, 0 })},
		{"invalid 6: negative price", [5]byte{0x00, 0, 0x10, 0, 0xf6}, with(func(c *Command) { c.Price = -246 })},
		{"invalid 7: price above max", [5]byte{0x00, 0, 0x10, 0, 0xf7}, with(func(c *Command) { c.Price = MaxPrice + 248 })},
		{"invalid 8: negative qty", [5]byte{0x00, 0, 0x10, 0, 0xf8}, with(func(c *Command) { c.Qty = -248 })},
		{"invalid 9: qty above max", [5]byte{0x00, 0, 0x10, 0, 0xf9}, with(func(c *Command) { c.Qty = MaxQty + 250 })},
		{"invalid 10: cancel with a price", [5]byte{0x00, 0, 0x10, 0, 0xfa}, Command{Kind: CommandCancel, OrderID: 1, Price: 92}},
		{"invalid 11: market with a price", [5]byte{0x00, 0, 0x10, 0, 0xfb}, Command{Kind: CommandNewOrder, OrderID: 1, Side: Buy, Type: Market, TIF: IOC, Price: 92, Qty: 1}},
	}
	for _, tt := range tests {
		got := decodeCommands(tt.in[:])
		if want := []Command{at(1, tt.want)}; !slices.Equal(got, want) {
			t.Errorf("%s: decodeCommands(%x) = %+v, want %+v", tt.name, tt.in, got, want)
		}
	}

	// Several commands: timestamps count up, a trailing partial command is
	// ignored, and the number of commands is capped.
	got := decodeCommands([]byte{0x00, 0, 0x10, 0, 0, 0x02, 3, 0, 0, 0, 0x20, 1})
	if want := []Command{at(1, base), at(2, cancel(4))}; !slices.Equal(got, want) {
		t.Fatalf("decodeCommands = %+v, want %+v", got, want)
	}
	if n := len(decodeCommands(make([]byte, fuzzCommandSize*(maxFuzzCommands+10)))); n != maxFuzzCommands {
		t.Fatalf("decoded %d commands, want the cap %d", n, maxFuzzCommands)
	}
}
