package testclock

import "testing"

func TestStall(t *testing.T) {
	c := New(func(call int64) int64 { return 100 + call })
	if got := c.Now(); got != 101 {
		t.Fatalf("first Now = %d, want 101", got)
	}
	for _, tc := range []struct {
		name  string
		stall func() (<-chan struct{}, func())
		free  int // calls that pass before the stalled one
	}{
		{"next call", c.Stall, 0},
		{"fifth call", func() (<-chan struct{}, func()) { return c.StallAt(5) }, 2},
	} {
		entered, release := tc.stall()
		for range tc.free {
			c.Now()
		}
		done := make(chan int64)
		go func() { done <- c.Now() }()
		<-entered
		select {
		case got := <-done:
			t.Fatalf("%s: Now returned %d while stalled", tc.name, got)
		default:
		}
		release()
		<-done
	}
	// A stall that ReleaseAll ended does not block.
	c.Stall()
	c.ReleaseAll()
	if got := c.Now(); got != 106 {
		t.Errorf("Now after ReleaseAll = %d, want 106", got)
	}
}
