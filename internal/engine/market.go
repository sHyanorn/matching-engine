package engine

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/sHyanorn/matching-engine/internal/book"
)

type reqKind uint8

const (
	reqCommand     reqKind = iota + 1 // log cmd, then apply it to the book
	reqDepth                          // read n levels per side
	reqOrder                          // look up a resting order
	reqSubscribe                      // add sub to the subscribers
	reqUnsubscribe                    // remove sub
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
	sub      *Subscription
	reply    chan response
}

type response struct {
	events []book.Event
	depth  Depth
	order  book.Order
	found  bool
	err    error
}

// market is one symbol: its book, its command log and the goroutine that
// owns both. Every field below in is touched only by that goroutine once
// run has started.
type market struct {
	symbol string
	index  int
	in     chan *request

	maxBatch     int
	clock        func() int64
	syncInterval time.Duration // < 0: fsync after every batch
	book         *book.Book
	file         *os.File
	batch        []*request
	logBuf       []byte
	out          []book.Event // reused output buffer for book.Apply
	subs         []*Subscription

	dirty    bool      // written since the last fsync
	lastSync time.Time // of the last fsync
	failed   error     // a log write or fsync failed; no command is applied after it
	closeErr error     // set by shutdown, read by Engine.Close after run returns
}

// openMarket opens symbol's command log in cfg.DataDir, creating it if it
// does not exist, and replays it into a fresh book. A torn last record is
// truncated away. It returns the market and the highest order-ID counter
// value found in the log.
func openMarket(symbol string, index int, cfg Config) (*market, uint64, error) {
	m := &market{
		symbol:       symbol,
		index:        index,
		in:           make(chan *request, cfg.QueueSize),
		maxBatch:     cfg.MaxBatch,
		clock:        cfg.Clock,
		syncInterval: cfg.SyncInterval,
		book:         book.New(),
		lastSync:     time.Now(),
	}
	path := filepath.Join(cfg.DataDir, symbol+".log")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, 0, err
	}
	maxSeq, err := m.replay(f, cfg.onReplay)
	if err != nil {
		_ = f.Close() // the replay error is the one to report
		return nil, 0, fmt.Errorf("%s: %w", path, err)
	}
	m.file = f
	return m, maxSeq, nil
}

// replay writes the header to an empty log, or checks the header of an
// existing one and applies every record to the book.
func (m *market) replay(f *os.File, observe func(symbol string, evs []book.Event)) (uint64, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if info.Size() == 0 {
		if _, err := f.Write(appendHeader(nil, m.symbol, m.index)); err != nil {
			return 0, err
		}
		return 0, f.Sync()
	}
	start, err := readHeader(f, m.symbol, m.index)
	if err != nil {
		return 0, err
	}
	var maxSeq uint64
	good, torn, err := readRecords(f, start, info.Size(), func(rec logRecord) error {
		c := rec.cmd
		if r := c.Validate(); r != book.ReasonNone {
			return fmt.Errorf("logged command is invalid: %v", r) // the engine never logs one
		}
		if symbolIndexOf(c.OrderID) != m.index && c.Kind == book.CommandNewOrder {
			return fmt.Errorf("order %d was issued for another symbol", c.OrderID)
		}
		maxSeq = max(maxSeq, seqOf(c.OrderID))
		m.out = m.book.Apply(c, m.out[:0])
		if observe != nil {
			observe(m.symbol, slices.Clone(m.out))
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if torn {
		if err := f.Truncate(good); err != nil {
			return 0, err
		}
		if err := f.Sync(); err != nil {
			return 0, err
		}
	}
	// O_APPEND puts every write at the end; reading moved only the offset.
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return 0, err
	}
	return maxSeq, nil
}

// run is the market's event loop. It takes the next request, then up to
// maxBatch-1 more that are already queued, and processes the batch in
// arrival order. While idle it fsyncs pending log writes on schedule. It
// returns once the queue is closed and drained.
func (m *market) run() {
	defer m.shutdown()
	var tick <-chan time.Time
	if m.syncInterval > 0 {
		t := time.NewTicker(m.syncInterval)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case r, ok := <-m.in:
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
		case <-tick:
			if m.dirty && time.Since(m.lastSync) >= m.syncInterval {
				m.sync()
			}
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
func (m *market) process(batch []*request) {
	m.logBuf = m.logBuf[:0]
	for _, r := range batch {
		if r.kind == reqCommand {
			r.cmd.Timestamp = m.clock()
			m.logBuf = appendRecord(m.logBuf, logRecord{cmd: r.cmd, clientID: r.clientID})
		}
	}
	if len(m.logBuf) > 0 && m.failed == nil {
		if _, err := m.file.Write(m.logBuf); err != nil {
			m.failed = fmt.Errorf("engine: %s: writing command log: %w", m.symbol, err)
		} else {
			m.dirty = true
			if m.syncInterval < 0 || time.Since(m.lastSync) >= m.syncInterval {
				m.sync()
			}
		}
	}

	for _, r := range batch {
		var resp response
		switch r.kind {
		case reqCommand:
			if m.failed != nil {
				resp.err = m.failed
				break
			}
			m.out = m.book.Apply(r.cmd, m.out[:0])
			// One copy, since m.out is overwritten by the next Apply,
			// shared read-only by the caller and every subscriber.
			resp.events = slices.Clone(m.out)
			m.publish(resp.events)
		case reqDepth:
			resp.depth = Depth{
				Bids:    m.book.Depth(book.Buy, r.n, nil),
				Asks:    m.book.Depth(book.Sell, r.n, nil),
				LastSeq: m.book.LastSeq(),
			}
		case reqOrder:
			resp.order, resp.found = m.book.Order(r.id)
		case reqSubscribe:
			resp = m.subscribe(r.sub)
		case reqUnsubscribe:
			m.unsubscribe(r.sub)
		}
		r.reply <- resp
	}
}

// sync flushes the log to stable storage. A failure stops the market from
// applying further commands: their durability could not be promised.
func (m *market) sync() {
	if err := m.file.Sync(); err != nil && m.failed == nil {
		m.failed = fmt.Errorf("engine: %s: syncing command log: %w", m.symbol, err)
	}
	m.dirty = false
	m.lastSync = time.Now()
}

// shutdown runs when the loop ends: it ends every subscription, then
// syncs and closes the log.
func (m *market) shutdown() {
	for _, s := range m.subs {
		m.end(s, ErrClosed)
	}
	m.subs = nil
	if m.dirty {
		m.sync()
	}
	m.closeErr = errors.Join(m.failed, m.file.Close())
}
