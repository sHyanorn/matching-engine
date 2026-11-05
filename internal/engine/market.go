package engine

import (
	"slices"

	"github.com/sHyanorn/matching-engine/internal/book"
)

type reqKind uint8

const (
	reqCommand reqKind = iota + 1 // apply cmd to the book
	reqDepth                      // read n levels per side
	reqOrder                      // look up a resting order
)

// request is one unit of work for a market's goroutine. reply is buffered
// (capacity 1), so the goroutine never blocks on a caller that stopped
// waiting.
type request struct {
	kind     reqKind
	cmd      book.Command
	clientID string
	n        int
	id       book.OrderID
	reply    chan response
}

type response struct {
	events []book.Event
	depth  Depth
	order  book.Order
	found  bool
	err    error
}

// market is one symbol: its book and the goroutine that owns it. Every
// field below in is touched only by that goroutine.
type market struct {
	symbol string
	index  int
	in     chan *request

	maxBatch int
	clock    func() int64
	book     *book.Book
	batch    []*request
	out      []book.Event // reused output buffer for book.Apply
}

func newMarket(symbol string, index int, cfg Config) *market {
	return &market{
		symbol:   symbol,
		index:    index,
		in:       make(chan *request, cfg.QueueSize),
		maxBatch: cfg.MaxBatch,
		clock:    cfg.Clock,
		book:     book.New(),
	}
}

// run is the market's event loop. It takes the next request, then up to
// maxBatch-1 more that are already queued, and processes the batch in
// arrival order. It returns once the queue is closed and drained.
func (m *market) run() {
	for {
		r, ok := <-m.in
		if !ok {
			return
		}
		m.batch = append(m.batch[:0], r)
		open := m.drain()
		m.process(m.batch)
		clear(m.batch) // drop references to finished requests
		if !open {
			return
		}
	}
}

// drain appends requests that are already queued to m.batch without
// waiting, up to maxBatch, and reports whether the queue is still open.
func (m *market) drain() bool {
	for len(m.batch) < m.maxBatch {
		select {
		case r, ok := <-m.in:
			if !ok {
				return false
			}
			m.batch = append(m.batch, r)
		default:
			return true
		}
	}
	return true
}

// process handles one batch in order: commands are stamped and applied,
// queries see the book as the commands before them left it.
func (m *market) process(batch []*request) {
	for _, r := range batch {
		var resp response
		switch r.kind {
		case reqCommand:
			r.cmd.Timestamp = m.clock()
			m.out = m.book.Apply(r.cmd, m.out[:0])
			resp.events = slices.Clone(m.out) // m.out is overwritten by the next Apply
		case reqDepth:
			resp.depth = Depth{
				Bids:    m.book.Depth(book.Buy, r.n, nil),
				Asks:    m.book.Depth(book.Sell, r.n, nil),
				LastSeq: m.book.LastSeq(),
			}
		case reqOrder:
			resp.order, resp.found = m.book.Order(r.id)
		}
		r.reply <- resp
	}
}
