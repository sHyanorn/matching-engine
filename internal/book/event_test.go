package book

import "testing"

func TestEventKindString(t *testing.T) {
	tests := []struct {
		kind EventKind
		want string
	}{
		{EventAccepted, "accepted"},
		{EventRejected, "rejected"},
		{EventTrade, "trade"},
		{EventRested, "rested"},
		{EventCanceled, "canceled"},
		{0, "EventKind(0)"},
		{6, "EventKind(6)"},
		{255, "EventKind(255)"},
	}
	for _, tt := range tests {
		if got := tt.kind.String(); got != tt.want {
			t.Errorf("EventKind(%d).String() = %q, want %q", uint8(tt.kind), got, tt.want)
		}
	}
}

// sampleEvents has one event of each shape in the Event field table, taken
// from one scenario (events 3, 6, 7 and 8 repeat shapes already shown and
// are left out):
//
//	1-2   sell 1: 10 @ 100 GTC rests
//	3-4   buy 2: 4 @ 100 GTC trades 4 with order 1
//	5     cancel 9 fails: no such order
//	6-7   buy 3: 5 @ 99 GTC rests
//	8-10  market buy 4: 8 IOC takes order 1's last 6, 2 are canceled
//	11    cancel 3 removes it, emptying the bid level at 99
func sampleEvents() map[string]Event {
	return map[string]Event{
		"accepted":           {Seq: 1, Timestamp: 10, Kind: EventAccepted, OrderID: 1, Side: Sell, Price: 100, Qty: 10},
		"rested":             {Seq: 2, Timestamp: 10, Kind: EventRested, OrderID: 1, Side: Sell, Price: 100, Qty: 10, LevelSide: Sell, LevelQty: 10},
		"trade":              {Seq: 4, Timestamp: 11, Kind: EventTrade, OrderID: 2, MakerID: 1, Side: Buy, Price: 100, Qty: 4, LevelSide: Sell, LevelQty: 6},
		"rejected":           {Seq: 5, Timestamp: 12, Kind: EventRejected, OrderID: 9, Reason: ReasonUnknownOrder},
		"trade emptying":     {Seq: 9, Timestamp: 14, Kind: EventTrade, OrderID: 4, MakerID: 1, Side: Buy, Price: 100, Qty: 6, LevelSide: Sell, LevelQty: 0},
		"canceled remainder": {Seq: 10, Timestamp: 14, Kind: EventCanceled, OrderID: 4, Side: Buy, Qty: 2, Reason: ReasonIOCRemainder},
		"canceled resting":   {Seq: 11, Timestamp: 15, Kind: EventCanceled, OrderID: 3, Side: Buy, Price: 99, Qty: 5, Reason: ReasonUserCancel, LevelSide: Buy, LevelQty: 0},
	}
}

func TestEventTrade(t *testing.T) {
	events := sampleEvents()

	got, ok := events["trade"].Trade()
	want := Trade{Seq: 4, Timestamp: 11, TakerID: 2, MakerID: 1, Price: 100, Qty: 4, TakerSide: Buy}
	if !ok || got != want {
		t.Errorf("Trade() = %+v, %v; want %+v, true", got, ok, want)
	}
	if want.TakerSide.Opposite() != events["trade"].LevelSide {
		t.Errorf("maker side %v != trade LevelSide %v", want.TakerSide.Opposite(), events["trade"].LevelSide)
	}

	for name, e := range events {
		got, ok := e.Trade()
		if ok != (e.Kind == EventTrade) {
			t.Errorf("%s: Trade() ok = %v, want %v", name, ok, e.Kind == EventTrade)
		}
		if !ok && got != (Trade{}) {
			t.Errorf("%s: Trade() = %+v, want the zero Trade", name, got)
		}
	}
	// Kind alone decides, whatever the other fields hold.
	if _, ok := (Event{Kind: EventTrade}).Trade(); !ok {
		t.Error("zero-field EventTrade: Trade() ok = false, want true")
	}
	if _, ok := (Event{Kind: 0, OrderID: 1, MakerID: 2, Price: 3, Qty: 4}).Trade(); ok {
		t.Error("Kind 0: Trade() ok = true, want false")
	}
}

func TestEventLevelUpdate(t *testing.T) {
	events := sampleEvents()
	tests := []struct {
		name string
		want LevelUpdate
		ok   bool
	}{
		{"accepted", LevelUpdate{}, false},
		{"rested", LevelUpdate{Side: Sell, Price: 100, Qty: 10}, true},
		{"trade", LevelUpdate{Side: Sell, Price: 100, Qty: 6}, true},
		{"rejected", LevelUpdate{}, false},
		{"trade emptying", LevelUpdate{Side: Sell, Price: 100, Qty: 0}, true},
		{"canceled remainder", LevelUpdate{}, false},
		{"canceled resting", LevelUpdate{Side: Buy, Price: 99, Qty: 0}, true},
	}
	if len(tests) != len(events) {
		t.Fatalf("%d cases for %d sample events", len(tests), len(events))
	}
	for _, tt := range tests {
		got, ok := events[tt.name].LevelUpdate()
		if ok != tt.ok || got != tt.want {
			t.Errorf("%s: LevelUpdate() = %+v, %v; want %+v, %v", tt.name, got, ok, tt.want, tt.ok)
		}
	}
	// An invalid LevelSide never reports a level, whatever LevelQty holds.
	if got, ok := (Event{Kind: EventTrade, Price: 100, LevelSide: 3, LevelQty: 5}).LevelUpdate(); ok {
		t.Errorf("LevelSide 3: LevelUpdate() = %+v, true; want false", got)
	}
}
