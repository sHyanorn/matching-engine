// Package testclock is the engine clock that tests use to hold a book
// still.
package testclock

import (
	"slices"
	"sync"
	"sync/atomic"
)

// Clock is an engine.Config.Clock for tests: pass its Now method. The
// engine calls its clock once per command, on the goroutine of the
// command's symbol, so a call that blocks holds that book in the middle of
// a batch while its queue fills up.
type Clock struct {
	time  func(call int64) int64
	calls atomic.Int64

	mu       sync.Mutex
	next     bool  // stall the next call
	at       int64 // stall this call; 0: none
	entered  chan struct{}
	release  chan struct{}
	releases []func()
}

// New returns a Clock whose timestamps come from time, which gets the
// number of the call, from 1. A nil time returns that number.
func New(time func(call int64) int64) *Clock {
	if time == nil {
		time = func(call int64) int64 { return call }
	}
	return &Clock{time: time}
}

// Now returns the next timestamp, after waiting out a stall set for this
// call.
func (c *Clock) Now() int64 {
	n := c.calls.Add(1)
	c.mu.Lock()
	var entered, release chan struct{}
	if c.next || n == c.at {
		entered, release = c.entered, c.release
		c.next, c.at = false, 0
	}
	c.mu.Unlock()
	if entered != nil {
		close(entered)
		<-release
	}
	return c.time(n)
}

// Stall makes the next call of Now block until release is called. entered
// is closed once a book's goroutine is blocked there; from then on nothing
// leaves that symbol's queue. One stall can be set at a time.
func (c *Clock) Stall() (entered <-chan struct{}, release func()) {
	return c.stall(true, 0)
}

// StallAt is Stall for call number n of Now, counting from 1.
func (c *Clock) StallAt(n int64) (entered <-chan struct{}, release func()) {
	return c.stall(false, n)
}

func (c *Clock) stall(next bool, at int64) (<-chan struct{}, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next, c.at = next, at
	c.entered = make(chan struct{})
	ch := make(chan struct{})
	c.release = ch
	release := sync.OnceFunc(func() { close(ch) })
	c.releases = append(c.releases, release)
	return c.entered, release
}

// ReleaseAll ends every stall, set or entered, so that a cleanup never
// waits on a stalled book.
func (c *Clock) ReleaseAll() {
	c.mu.Lock()
	c.next, c.at = false, 0
	rs := slices.Clone(c.releases)
	c.mu.Unlock()
	for _, r := range rs {
		r()
	}
}
