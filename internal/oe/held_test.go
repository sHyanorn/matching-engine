package oe

import (
	"bytes"
	"math/rand/v2"
	"testing"
)

// TestHeldKeepsTheLastFramesThatFit adds frames of both sizes and checks, at
// every step, that what is held is the longest run of the latest frames
// within the limit, and that from gives the frames asked for.
func TestHeldKeepsTheLastFramesThatFit(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for _, limit := range []int{83, 200, 1000, 5000} {
		h := held{limit: limit}
		var frames [][]byte // every frame added, by server_seq - 1
		for seq := uint64(1); seq <= 5000; seq++ {
			var fr []byte
			if r.IntN(3) == 0 {
				fr = Done{ServerSeq: seq, ClientSeq: seq * 7}.Append(nil)
			} else {
				fr = Event{ServerSeq: seq, ClientSeq: seq * 7}.Append(nil)
			}
			frames = append(frames, fr)
			h.add(fr, seq)

			// The longest suffix of frames within the limit.
			first, size := seq+1, 0
			for first > 1 && size+len(frames[first-2]) <= limit {
				first--
				size += len(frames[first-1])
			}
			if h.empty() != (first == seq+1) || (!h.empty() && h.first != first) {
				t.Fatalf("limit %d, seq %d: held from %d, want from %d", limit, seq, h.first, first)
			}
			if seq%97 == 0 || seq < 10 {
				for k := first; k <= seq+1; k += 1 + uint64(r.IntN(5)) {
					want := bytes.Join(frames[k-1:], nil)
					if got := h.from(k); !bytes.Equal(got, want) {
						t.Fatalf("limit %d, seq %d: from(%d) is %d bytes, want %d", limit, seq, k, len(got), len(want))
					}
				}
			}
			if cap(h.b) > max(4096, limit+limit/2+2*83) {
				t.Fatalf("limit %d: the buffer grew to %d bytes", limit, cap(h.b))
			}
		}
	}
}

func TestHeldAddAllocatesNothing(t *testing.T) {
	h := held{limit: 1000}
	fr := Event{}.Append(nil)
	for seq := uint64(1); seq < 100; seq++ {
		h.add(fr, seq)
	}
	seq := uint64(100)
	if n := testing.AllocsPerRun(1000, func() { h.add(fr, seq); seq++ }); n != 0 {
		t.Errorf("%v allocations to hold a frame, want 0", n)
	}
}

// BenchmarkResume measures what a Login that resumes a session costs on the
// server before the first byte is written: finding the first frame the
// client missed in a full 1 MB of held output and queueing LoginAccepted and
// every frame from there on. From the oldest frame there is the most to
// queue.
func BenchmarkResume(b *testing.B) {
	srv := NewServer(ServerOptions{})
	ss, err := srv.newSession()
	if err != nil {
		b.Fatal(err)
	}
	for seq := uint64(1); seq <= 20_000; seq++ {
		var fr []byte
		if seq%3 == 0 {
			fr = Done{ServerSeq: seq}.Append(ss.scratch[:0])
		} else {
			fr = Event{ServerSeq: seq}.Append(ss.scratch[:0])
		}
		ss.emitLocked(fr)
	}
	first := ss.heldFirstLocked()
	c := &conn{srv: srv}
	b.ReportAllocs()
	b.SetBytes(int64(len(ss.held.b) - ss.held.start))
	b.ResetTimer()
	for range b.N {
		c.out = c.out[:0]
		if reason, detail := c.resumeLocked(ss, first); reason != 0 {
			b.Fatal(reason, detail)
		}
		ss.cur = nil
	}
}
