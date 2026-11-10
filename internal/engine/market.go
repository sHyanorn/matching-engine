package engine

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
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
	status OrderStatus
	found  bool
	err    error
}

// logFile is what a market needs from its open log once replay is done.
// Tests substitute failing implementations.
type logFile interface {
	io.Writer
	Sync() error
	Close() error
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
	logger       *slog.Logger
	onSync       func(symbol string, took time.Duration)
	syncInterval time.Duration // < 0: fsync after every batch
	book         *book.Book
	file         logFile
	batch        []*request
	logBuf       []byte
	recEnds      []int        // end offset in logBuf of each command's record
	out          []book.Event // reused output buffer for book.Apply
	subs         []*Subscription
	orders       *orderTracker

	dirty    bool      // written since the last fsync was started or asked for
	lastSync time.Time // when it was
	failed   error     // a log write or fsync failed (wraps ErrLogFailed); no command is logged after it
	closeErr error     // set by shutdown, read by Engine.Close after run returns

	logErr atomic.Pointer[error] // failed, for other goroutines

	// With syncInterval > 0 the fsyncs run on a helper goroutine, so
	// commands do not wait for them (D031): syncs wakes it, it calls
	// file.Sync and onSync, and it reports a failure in syncErr, which the
	// market takes up. So file and onSync must not change while run runs.
	syncs   chan struct{}
	synced  chan struct{} // closed when the helper has stopped
	syncErr atomic.Pointer[error]
}

// openMarket opens symbol's command log in cfg.DataDir, creating it if it
// does not exist, and replays it into a fresh book. A torn last record is
// truncated away. It returns the market, the highest order-ID counter
// value of a logged new order, and whether it created the log, which it
// reports even with an error.
func openMarket(symbol string, index int, cfg Config) (m *market, maxCounter uint64, created bool, err error) {
	m = &market{
		symbol:       symbol,
		index:        index,
		in:           make(chan *request, cfg.QueueSize),
		maxBatch:     cfg.MaxBatch,
		clock:        cfg.Clock,
		logger:       cfg.Logger,
		onSync:       cfg.OnSync,
		syncInterval: cfg.SyncInterval,
		book:         book.New(),
		orders:       newOrderTracker(symbol, cfg.History),
		lastSync:     time.Now(),
	}
	path := filepath.Join(cfg.DataDir, symbol+".log")
	if created, err = createLog(path, symbol, index); err != nil {
		return nil, 0, created, err
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, 0, created, err
	}
	if maxCounter, err = m.replay(f, cfg.onReplay); err != nil {
		_ = f.Close() // the replay error is the one to report
		return nil, 0, created, fmt.Errorf("%s: %w", path, err)
	}
	m.file = f
	return m, maxCounter, created, nil
}

// createLog creates a log holding only its header, unless path exists, and
// reports whether it did. The header goes to a temporary file that is
// synced and then renamed into place, so a crash never leaves a log with a
// partial header.
func createLog(path, symbol string, index int) (bool, error) {
	if _, err := os.Stat(path); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	tmp := path + ".tmp"
	err := writeFileSync(tmp, appendHeader(nil, symbol, index))
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp) // best effort; err is the one to report
		return false, err
	}
	return true, syncDir(filepath.Dir(path))
}

// writeFileSync writes data to a new or truncated file and fsyncs it.
func writeFileSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

// replay checks the log's header and applies every record to the book. An
// empty file, which only an older engine could have left, gets a header.
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
	var maxCounter uint64
	good, torn, err := readRecords(f, start, info.Size(), func(rec logRecord) error {
		c := rec.cmd
		if r := c.Validate(); r != book.ReasonNone {
			return fmt.Errorf("logged command is invalid: %v", r) // the engine never logs one
		}
		if c.Kind == book.CommandNewOrder {
			// Only new orders consume IDs; a cancel may name any ID.
			if symbolIndexOf(c.OrderID) != m.index {
				return fmt.Errorf("order %d was issued for another symbol", c.OrderID)
			}
			maxCounter = max(maxCounter, counterOf(c.OrderID))
		}
		m.out = m.book.Apply(c, m.out[:0])
		m.orders.track(c, rec.clientID, m.out)
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
		m.logger.Warn("dropped a torn tail from the command log", "symbol", m.symbol, "bytes", info.Size()-good)
	}
	// Writes go on at the end, after the last good record.
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return 0, err
	}
	return maxCounter, nil
}

// run is the market's event loop. It takes the next request, then up to
// maxBatch-1 more that are already queued, and processes the batch in
// arrival order. While idle it has pending log writes fsynced every
// syncInterval. It returns once the queue is closed and drained.
func (m *market) run() {
	defer m.shutdown()
	var tick <-chan time.Time
	if m.syncInterval > 0 {
		t := time.NewTicker(m.syncInterval)
		defer t.Stop()
		tick = t.C
		m.syncs, m.synced = make(chan struct{}, 1), make(chan struct{})
		go m.syncLoop(m.syncs)
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
			m.takeSyncErr()
			if m.dirty {
				_ = m.sync() // a failure is kept in m.failed
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
//
// The book applies exactly the commands whose records are wholly in the
// log, so it always equals what replaying the log gives. After a failed or
// short write, the commands that did not reach the log, and every command
// after them, fail with ErrLogFailed and are never applied. A failed fsync
// leaves the batch in the file, so the batch is applied. When every batch
// is fsynced (syncInterval < 0), the commands applied after a failed write
// or fsync get the error too, with their events: durability was promised
// and is unknown.
func (m *market) process(batch []*request) {
	m.takeSyncErr()
	m.logBuf, m.recEnds = m.logBuf[:0], m.recEnds[:0]
	for _, r := range batch {
		if r.kind == reqCommand {
			r.cmd.Timestamp = m.clock()
			m.logBuf = appendRecord(m.logBuf, logRecord{cmd: r.cmd, clientID: r.clientID})
			m.recEnds = append(m.recEnds, len(m.logBuf))
		}
	}
	logged := 0 // commands of this batch whose records are wholly in the log
	var syncErr error
	if len(m.logBuf) > 0 && m.failed == nil {
		n, err := m.file.Write(m.logBuf)
		for logged < len(m.recEnds) && m.recEnds[logged] <= n {
			logged++
		}
		if n > 0 {
			m.dirty = true
		}
		switch {
		case err != nil:
			m.fail("write", err)
			if m.syncInterval < 0 {
				syncErr = m.failed // the commands that were written are never fsynced
			}
		case m.syncInterval < 0 || time.Since(m.lastSync) >= m.syncInterval:
			if err := m.sync(); err != nil && m.syncInterval < 0 {
				syncErr = err
			}
		}
	}

	cmd := 0
	for _, r := range batch {
		var resp response
		switch r.kind {
		case reqCommand:
			cmd++
			if cmd > logged {
				resp.err = m.failed
				break
			}
			m.out = m.book.Apply(r.cmd, m.out[:0])
			// One copy, since m.out is overwritten by the next Apply,
			// shared read-only by the caller and every subscriber.
			resp.events = slices.Clone(m.out)
			resp.status, resp.found = m.orders.track(r.cmd, r.clientID, resp.events)
			resp.err = syncErr
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
		r.reply <- resp
	}
}

// sync has the log flushed to stable storage: by the helper if there is
// one, at once otherwise. A failure is kept in m.failed.
func (m *market) sync() error {
	m.dirty = false
	m.lastSync = time.Now()
	if m.syncs != nil {
		select {
		case m.syncs <- struct{}{}:
		default: // an fsync is already due to start, and covers these writes
		}
		return nil
	}
	if err := m.fsync(); err != nil {
		m.fail("fsync", err)
		return m.failed
	}
	return nil
}

// syncLoop is the helper: it runs an fsync for each wake on syncs, until
// syncs is closed.
func (m *market) syncLoop(syncs <-chan struct{}) {
	defer close(m.synced)
	for range syncs {
		if err := m.fsync(); err != nil {
			m.syncErr.CompareAndSwap(nil, &err)
		}
	}
}

// takeSyncErr takes up a failed fsync of the helper.
func (m *market) takeSyncErr() {
	if err := m.syncErr.Load(); err != nil {
		m.fail("fsync", *err)
	}
}

// fsync flushes the log and reports how long it took.
func (m *market) fsync() error {
	start := time.Now()
	err := m.file.Sync()
	if m.onSync != nil {
		m.onSync(m.symbol, time.Since(start))
	}
	return err
}

// fail records the first failed write or fsync of the log: nothing is
// logged after it, since its durability could not be promised (D017).
func (m *market) fail(op string, err error) {
	if m.failed != nil {
		return
	}
	m.failed = fmt.Errorf("%w: %s: %s: %w", ErrLogFailed, m.symbol, op, err)
	failed := m.failed
	m.logErr.Store(&failed)
	m.logger.Error("command log failed; the symbol accepts no more commands", "symbol", m.symbol, "err", err)
}

// shutdown runs when the loop ends: it ends every subscription, stops the
// helper once its fsyncs are done, then syncs and closes the log.
func (m *market) shutdown() {
	for _, s := range m.subs {
		m.end(s, ErrClosed)
	}
	m.subs = nil
	if m.syncs != nil {
		close(m.syncs)
		<-m.synced
		m.syncs = nil
		m.takeSyncErr()
	}
	if m.dirty {
		_ = m.sync() // a failure is kept in m.failed
	}
	m.closeErr = errors.Join(m.failed, m.file.Close())
}
