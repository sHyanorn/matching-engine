package oe

import (
	"runtime"
	"testing"
	"time"

	"github.com/sHyanorn/matching-engine/internal/spin"
)

// atLimit returns the connection of a session that has maxTaken commands
// taken and unanswered, with the lease that makes its reader spin. The test
// stands in for the reader.
func atLimit(t *testing.T) *conn {
	t.Helper()
	c := attached(t, NewServer(ServerOptions{Symbols: testSymbols}))
	c.lease = spin.NewPool([]int{0}, nil).Reserve("reader")
	c.ss.taken = maxTaken
	t.Cleanup(func() { c.ss.gen.Add(1) }) // lets go of a reader that a failed test left spinning
	return c
}

// TestSpinningReaderWaitsForRoomWithoutTheLock: a reader that spins in
// waitRoom holds no lock while it waits, so the symbol's goroutine takes
// ss.mu at once to give a place back, and the reader takes ss.mu again and
// returns when a place is given back, a drain begins or the connection is
// aborted.
func TestSpinningReaderWaitsForRoomWithoutTheLock(t *testing.T) {
	for _, tc := range []struct {
		name string
		wake func(c *conn)
	}{
		{"a place is given back", func(c *conn) { c.ss.answeredLocked() }},
		{"a drain begins", func(c *conn) { c.ss.beginDrainLocked(drainShutdown) }},
		{"the connection is aborted", func(c *conn) { c.abortLocked(errSlowConsumer) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := atLimit(t)
			ss := c.ss
			ss.mu.Lock() // admit holds it when it calls waitRoom, and the reader unlocks it
			returned := make(chan struct{})
			go func() {
				c.waitRoom()
				close(returned)
			}()
			for deadline := time.Now().Add(10 * time.Second); !ss.mu.TryLock(); runtime.Gosched() {
				if time.Now().After(deadline) {
					t.Fatal("the reader did not release the session lock")
				}
			}
			ss.mu.Unlock()
			for i := range 1000 {
				if !ss.mu.TryLock() {
					t.Fatalf("the session lock was free %d times in a row, then taken again by the reader waiting for room", i)
				}
				ss.mu.Unlock()
			}
			select {
			case <-returned:
				t.Fatal("waitRoom returned before anything changed")
			default:
			}

			ss.mu.Lock()
			tc.wake(c)
			ss.mu.Unlock()
			select {
			case <-returned:
			case <-time.After(10 * time.Second):
				t.Fatal("the reader did not go on after the change")
			}
			if ss.mu.TryLock() {
				t.Fatal("waitRoom returned without ss.mu held")
			}
			ss.mu.Unlock()
		})
	}
}
