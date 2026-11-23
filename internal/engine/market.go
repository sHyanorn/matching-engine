package engine

import (
	"fmt"
	"slices"
	"time"

	"github.com/sHyanorn/matching-engine/internal/book"
	"github.com/sHyanorn/matching-engine/internal/spin"
)

type reqKind uint8

const (
	reqCommand     reqKind = iota + 1 // log cmd, then apply it to the book
	reqDepth                          // read n levels per side
	reqOrder                          // look up a resting order
	reqSubscribe                      // add sub to the subscribers
	reqUnsubscribe                    // remove sub
)

// request is one unit of work for a market's goroutine, passed by value. A
// command is answered through sink, with tag, if it has one, and through
// reply otherwise. reply is buffered (capacity 1), so the goroutine never
// blocks on a caller that stopped waiting.
type request struct {
	kind     reqKind
	cmd      book.Command
	clientID string
	n        int
	id       book.OrderID
	sub      *Subscription
	reply    chan response
	sink     Sink
	tag      uint64
}

type response struct {
	events []book.Event
	depth  Depth
	status OrderStatus
	found  bool
	err    error
}

// market is one symbol: its book, its command log and the goroutine that
// owns both. Every field below in is touched only by that goroutine once
// run has started.
type market struct {
	symbol string
	index  int
	in     chan request

	maxBatch int
	clock    func() int64
	book     *book.Book
	log      *cmdLog
	batch    []request
	touched  []Sink // sinks that got a reply in this batch, in order, without repeats of the last
	logBuf   []byte
	recEnds  []int        // end offset in logBuf of each command's record
	out      []book.Event // reused output buffer for book.Apply
	subs     []*Subscription
	orders   *orderTracker

	closeErr error // set by shutdown, read by Engine.Close after run returns
}

// openMarket opens symbol's command log at path and replays it into a
// fresh book. It returns the market and the highest order-ID counter value
// of a logged new order.
func openMarket(path, symbol string, index int, cfg Config) (*market, uint64, error) {
	m := &market{
		symbol:   symbol,
		index:    index,
		in:       make(chan request, cfg.QueueSize),
		maxBatch: cfg.MaxBatch,
		clock:    cfg.Clock,
		book:     book.New(),
		orders:   newOrderTracker(symbol, cfg.History),
	}
	var maxCounter uint64
	log, err := openLog(path, symbol, index, cfg, func(rec logRecord) error {
		c := rec.cmd
		if r := c.Validate(); r != book.ReasonNone {
			return fmt.Errorf("logged command is invalid: %v", r) // the engine never logs one
		}
		if c.Kind == book.CommandNewOrder {
			// Only new orders consume IDs; a cancel may name any ID.
			if symbolIndexOf(c.OrderID) != index {
				return fmt.Errorf("order %d was issued for another symbol: index %d, this log is %q at index %d", c.OrderID, symbolIndexOf(c.OrderID), symbol, index)
			}
			maxCounter = max(maxCounter, counterOf(c.OrderID))
		}
		m.out = m.book.Apply(c, m.out[:0])
		m.orders.track(c, rec.clientID, m.out)
		if cfg.onReplay != nil {
			cfg.onReplay(symbol, slices.Clone(m.out))
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	m.log = log
	return m, maxCounter, nil
}

// run is the market's event loop. It takes the next request, then up to
// maxBatch-1 more that are already queued, and processes the batch in
// arrival order. While idle it lets the log fsync pending writes when they
// are due. It returns once the queue is closed and drained. If it can pin
// itself to core it spins, polling for requests instead of blocking, and it
// gives the core back when it returns.
func (m *market) run(core *spin.Lease) {
	defer core.Release()
	defer m.shutdown()
	syncDue := m.log.start()
	spinning := core.Pin()
	for {
		r, open, tick := m.next(syncDue, spinning)
		if tick {
			m.log.tick()
			continue
		}
		if !open {
			return
		}
		m.batch = append(m.batch[:0], r)
		open = m.drain()
		m.process(m.batch)
		clear(m.batch) // drop references to finished requests
		if !open {
			return
		}
	}
}

// next waits for the next request. It reports a tick instead when syncDue
// fires first, and open false when the queue is closed and empty. A spinning
// market polls for both instead of blocking.
func (m *market) next(syncDue <-chan time.Time, spinning bool) (r request, open, tick bool) {
	if !spinning {
		select {
		case r, open = <-m.in:
			return r, open, false
		case <-syncDue:
			return r, true, true
		}
	}
	for {
		select {
		case r, open = <-m.in:
			return r, open, false
		default:
		}
		select {
		case <-syncDue:
			return r, true, true
		default:
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

// process handles one batch. The batch's commands are stamped and written
// to the log with one write before any of them is applied (write-ahead);
// then every request is handled in arrival order, so a query sees the book
// as the commands before it left it.
//
// The book applies exactly the commands whose records are wholly in the
// log, so it always equals what replaying the log gives. After a failed or
// short write, the commands that did not reach the log, and every command
// after them, fail with ErrLogFailed and are never applied. A failed fsync
// leaves the batch in the file, so the batch is applied. When every batch
// is fsynced, the commands applied after a failed write or fsync get the
// error too, with their events: durability was promised and is unknown.
func (m *market) process(batch []request) {
	m.logBuf, m.recEnds = m.logBuf[:0], m.recEnds[:0]
	for i := range batch {
		if r := &batch[i]; r.kind == reqCommand {
			r.cmd.Timestamp = m.clock()
			m.logBuf = appendRecord(m.logBuf, logRecord{cmd: r.cmd, clientID: r.clientID})
			m.recEnds = append(m.recEnds, len(m.logBuf))
		}
	}
	logged, unsynced := m.log.write(m.logBuf, m.recEnds)

	cmd := 0
	for i := range batch {
		r := &batch[i]
		var resp response
		switch r.kind {
		case reqCommand:
			cmd++
			if cmd > logged {
				resp.err = m.log.failed
				break
			}
			m.out = m.book.Apply(r.cmd, m.out[:0])
			resp.events = m.out
			if r.sink == nil || len(m.subs) > 0 {
				// One copy, since m.out is overwritten by the next Apply,
				// shared read-only by the caller and every subscriber. A
				// sink uses the events before it returns.
				resp.events = slices.Clone(m.out)
			}
			resp.status, resp.found = m.orders.track(r.cmd, r.clientID, resp.events)
			resp.err = unsynced
			m.publish(resp.events)
		case reqDepth:
			resp.depth = Depth{
				Bids:    m.book.Depth(book.Buy, r.n, nil),
				Asks:    m.book.Depth(book.Sell, r.n, nil),
				LastSeq: m.book.LastSeq(),
			}
		case reqOrder:
			resp.status, resp.found = m.orders.lookup(r.id)
		case reqSubscribe:
			resp = m.subscribe(r.sub, r.n)
		case reqUnsubscribe:
			m.unsubscribe(r.sub)
		}
		if r.sink != nil {
			r.sink.Reply(r.tag, Reply{Kind: r.cmd.Kind, OrderID: r.cmd.OrderID, Events: resp.events, Status: resp.status, Err: resp.err})
			if n := len(m.touched); n == 0 || m.touched[n-1] != r.sink {
				m.touched = append(m.touched, r.sink)
			}
			continue
		}
		r.reply <- resp
	}
	for i, s := range m.touched {
		s.Flush()
		m.touched[i] = nil
	}
	m.touched = m.touched[:0]
}

// shutdown runs when the loop ends: it ends every subscription, then has
// the log flushed and closed.
func (m *market) shutdown() {
	for _, s := range m.subs {
		m.end(s, ErrClosed)
	}
	m.subs = nil
	m.closeErr = m.log.close()
}
