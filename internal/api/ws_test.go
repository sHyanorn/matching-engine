package api

import "testing"

// A connection's queued messages, apart from the largest, are limited to
// maxQueuedBytes; the largest may be of any size.
func TestBacklogBound(t *testing.T) {
	const (
		limit = maxQueuedBytes
		large = limit + limit/2
	)
	tests := []struct {
		name    string
		queued  []int // added before next
		written int   // removed after queued; 0 for none
		next    int
		want    bool
	}{
		{"any size into an empty queue", nil, 0, 5 * limit, true},
		{"others up to the limit beside a large message", []int{large, limit - 100}, 0, 100, true},
		{"others past the limit beside a large message", []int{large, limit - 100}, 0, 101, false},
		{"a large message beside others at the limit", []int{limit}, 0, large, true},
		{"a second message over the limit", []int{large}, 0, limit + 1, false},
		{"a large message once the last one is written", []int{large, 100}, large, large, true},
		{"past the limit with no large message", []int{limit / 2, limit / 2, limit / 2}, 0, 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b backlog
			for _, n := range tt.queued {
				if !b.add(n) {
					t.Fatalf("add(%d) refused while queueing %v", n, tt.queued)
				}
			}
			if tt.written > 0 {
				b.remove(tt.written)
			}
			if got := b.add(tt.next); got != tt.want {
				t.Errorf("add(%d) = %v, want %v", tt.next, got, tt.want)
			}
		})
	}
}
