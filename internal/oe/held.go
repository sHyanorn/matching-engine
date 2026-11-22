package oe

// held is the last frames of a session's output that carry a server_seq,
// kept so that a client that lost its connection can be sent what it
// missed. Frames are appended whole and dropped whole from the front, so
// that what is kept is never more than limit bytes, in a buffer of at most
// one and a half times that, and a frame or two.
type held struct {
	b     []byte
	start int    // offset of the oldest frame in b
	limit int    // most bytes kept
	first uint64 // server_seq of the frame at start, when there is one
}

func (h *held) empty() bool { return h.start == len(h.b) }

// frameSize is the length on the wire of the frame at the start of b.
func frameSize(b []byte) int { return int(le.Uint16(b)) + 2 }

// add keeps frame fr, whose server_seq is seq, and drops the oldest frames
// beyond the limit.
func (h *held) add(fr []byte, seq uint64) {
	if h.empty() {
		h.first = seq
	}
	if len(h.b)+len(fr) > cap(h.b) {
		// Grow by doubling, but no further than the buffer ever needs: the
		// frames within the limit, and the dropped part before it passes
		// half the limit and is closed up.
		nb := make([]byte, len(h.b), min(max(2*cap(h.b), 4096), h.limit+h.limit/2+2*len(fr)))
		h.b = nb[:copy(nb, h.b)]
	}
	h.b = append(h.b, fr...)
	for len(h.b)-h.start > h.limit {
		h.start += frameSize(h.b[h.start:])
		h.first++
	}
	if h.start > h.limit/2 { // the dropped part is over half the space: close up
		h.b = h.b[:copy(h.b, h.b[h.start:])]
		h.start = 0
	}
}

// from returns the frames with a server_seq of k or more. k must be no
// lower than the oldest frame kept and no higher than the next server_seq.
func (h *held) from(k uint64) []byte {
	off := h.start
	for seq := h.first; seq < k && off < len(h.b); seq++ {
		off += frameSize(h.b[off:])
	}
	return h.b[off:]
}
