package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sHyanorn/matching-engine/internal/book"
)

// Errors returned by the Engine. Callers compare with errors.Is.
var (
	ErrUnknownSymbol = errors.New("engine: unknown symbol")
	ErrUnknownOrder  = errors.New("engine: unknown order")
	ErrClosed        = errors.New("engine: closed")
	// ErrBusy is returned at once when a symbol's command queue is full
	// (SPEC §4: backpressure); the caller may retry later.
	ErrBusy        = errors.New("engine: busy, command queue full")
	ErrClientIDLen = fmt.Errorf("engine: client_id longer than %d bytes", MaxClientIDLen)
	// ErrLogFailed reports that a symbol's command log could not be
	// written or synced. The symbol accepts no more commands. A command
	// answered with it and no events was not applied and never will be;
	// one answered with it and its events was applied, but whether it is
	// durable is unknown (see Config.SyncInterval).
	ErrLogFailed = errors.New("engine: command log failed")
)

// MaxClientIDLen is the longest client_id the engine accepts.
const MaxClientIDLen = 64

// InvalidError reports a command that failed [book.Command.Validate]. The
// engine answers it without logging or applying the command (D009), so it
// consumes no Seq.
type InvalidError struct {
	Reason book.Reason
}

func (e *InvalidError) Error() string { return "engine: invalid command: " + e.Reason.String() }

// Config configures an Engine. Symbols and DataDir are required.
type Config struct {
	// Symbols lists the symbols the engine serves, one book each. A
	// symbol's index in this list is part of every order ID issued for
	// it and is recorded in its command log, so the order must not change
	// between runs.
	Symbols []string

	// DataDir holds one command log per symbol, <symbol>.log. It is
	// created if missing; existing logs are replayed by New.
	DataDir string

	// SyncInterval is how often a symbol's goroutine fsyncs its log
	// after writing to it (default 10ms). Each batch is written to the
	// operating system before it is applied, so a process crash loses
	// nothing; a machine crash may lose up to SyncInterval of commands.
	// A negative value fsyncs after every batch (D017).
	SyncInterval time.Duration

	// QueueSize is the capacity of each symbol's request queue
	// (default 4096).
	QueueSize int

	// MaxBatch is how many queued requests a symbol's goroutine takes at
	// once (default 256).
	MaxBatch int

	// Clock returns the time stamped on each command, in Unix nanoseconds
	// (default the system clock). Tests replace it for fixed values.
	Clock func() int64

	// History is how many finished orders each symbol remembers for
	// Order (default 100,000; negative: none). Open orders are always
	// known (D022).
	History int

	// onReplay, if set, receives the events of every command replayed
	// from the logs by New. Tests use it to compare replays.
	onReplay func(symbol string, evs []book.Event)
}

const (
	defaultQueueSize    = 4096
	defaultMaxBatch     = 256
	defaultSyncInterval = 10 * time.Millisecond
)

var symbolPattern = regexp.MustCompile(`^[A-Z0-9][A-Z0-9._-]{0,31}$`)

func (c *Config) setDefaults() error {
	if len(c.Symbols) == 0 {
		return errors.New("engine: no symbols configured")
	}
	if len(c.Symbols) > MaxSymbols {
		return fmt.Errorf("engine: %d symbols, at most %d", len(c.Symbols), MaxSymbols)
	}
	seen := make(map[string]bool, len(c.Symbols))
	for _, s := range c.Symbols {
		// IsLocal rejects a log name that this Windows maps to a device,
		// such as CON.log or NUL.log.
		if !symbolPattern.MatchString(s) || !filepath.IsLocal(s+".log") {
			return fmt.Errorf("engine: invalid symbol %q", s)
		}
		if seen[s] {
			return fmt.Errorf("engine: duplicate symbol %q", s)
		}
		seen[s] = true
	}
	if c.DataDir == "" {
		return errors.New("engine: no data directory configured")
	}
	if c.QueueSize <= 0 {
		c.QueueSize = defaultQueueSize
	}
	if c.SyncInterval == 0 {
		c.SyncInterval = defaultSyncInterval
	}
	switch {
	case c.History == 0:
		c.History = defaultHistory
	case c.History < 0:
		c.History = 0
	}
	if c.MaxBatch <= 0 {
		c.MaxBatch = defaultMaxBatch
	}
	if c.Clock == nil {
		c.Clock = func() int64 { return time.Now().UnixNano() }
	}
	return nil
}

// Engine runs one order book per symbol, each on its own goroutine that
// owns the book and processes requests one at a time (D003), so books need
// no locks and every book's result is deterministic. All Engine methods are
// safe for concurrent use.
type Engine struct {
	markets  []*market
	bySymbol map[string]*market
	nextSeq  atomic.Uint64 // last counter value issued for an order ID
	reserved atomic.Uint64 // highest counter value durably reserved (D021)
	reserve  sync.Mutex    // serializes extending the reservation
	ids      idStore

	// mu guards closed and the sends to market queues, so Close never
	// closes a queue while a send to it is in progress.
	mu     sync.RWMutex
	closed bool
	wg     sync.WaitGroup
}

// New replays each symbol's command log from cfg.DataDir into a fresh
// book, then starts one goroutine per symbol. Order IDs continue after the
// highest one found in the logs or reserved before (D021).
func New(cfg Config) (*Engine, error) {
	if err := cfg.setDefaults(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, err
	}
	// DataDir's own entry must survive a machine crash too.
	if err := syncDir(filepath.Dir(filepath.Clean(cfg.DataDir))); err != nil {
		return nil, err
	}
	e := &Engine{
		bySymbol: make(map[string]*market, len(cfg.Symbols)),
		ids:      idStore{path: filepath.Join(cfg.DataDir, "ids")},
	}
	// A failed start removes the logs it created, which hold only a
	// header: one left behind would bind its symbol to this attempt's
	// index and reject the corrected configuration.
	var created []string
	fail := func(err error) (*Engine, error) {
		for _, m := range e.markets {
			_ = m.file.Close() // err is the error to report
		}
		for _, path := range created {
			_ = os.Remove(path)
		}
		if len(created) > 0 {
			_ = syncDir(cfg.DataDir)
		}
		return nil, err
	}
	var maxSeq uint64
	for i, s := range cfg.Symbols {
		m, seq, isNew, err := openMarket(s, i, cfg)
		if isNew {
			created = append(created, filepath.Join(cfg.DataDir, s+".log"))
		}
		if err != nil {
			return fail(err)
		}
		e.markets = append(e.markets, m)
		e.bySymbol[s] = m
		maxSeq = max(maxSeq, seq)
	}
	start, err := e.ids.load()
	if err == nil {
		start = max(start, maxSeq)
		e.nextSeq.Store(start)
		e.reserved.Store(start)
		err = e.extendReservation(start)
	}
	if err != nil {
		return fail(err)
	}
	for _, m := range e.markets {
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			m.run()
		}()
	}
	return e, nil
}

// Close stops accepting requests, lets every symbol's goroutine finish the
// requests already queued, fsync and close its log, and waits for them to
// stop. It returns any log error. Every call, concurrent or later, waits
// for the shutdown and returns the same error.
func (e *Engine) Close() error {
	e.mu.Lock()
	if !e.closed {
		e.closed = true
		for _, m := range e.markets {
			close(m.in)
		}
	}
	e.mu.Unlock()
	e.wg.Wait()
	var errs []error
	for _, m := range e.markets {
		errs = append(errs, m.closeErr)
	}
	return errors.Join(errs...)
}

// NewOrder is a client's new order. The engine assigns its ID and
// timestamp.
type NewOrder struct {
	Symbol   string
	Side     book.Side
	Type     book.OrderType
	TIF      book.TIF
	Price    book.Price
	Qty      book.Qty
	ClientID string
}

// Result is the outcome of a command that reached its book: the order ID
// it concerns, the events the book emitted for it, in order, and that
// order's status afterwards (the zero OrderStatus if the engine does not
// know the order). Events is shared with event subscribers and must not
// be modified.
type Result struct {
	OrderID book.OrderID
	Events  []book.Event
	Status  OrderStatus
}

// PlaceOrder assigns the next order ID, validates the order and submits it
// to its symbol's book, then waits until it has been applied. A command
// that fails validation returns an *InvalidError and is not applied; if the
// symbol's queue is full, PlaceOrder returns ErrBusy at once. ctx bounds
// only the submission: if it is already done PlaceOrder submits nothing,
// and once the order is queued PlaceOrder waits for its result (D020). See
// ErrLogFailed for what a log failure means.
func (e *Engine) PlaceOrder(ctx context.Context, o NewOrder) (Result, error) {
	m, ok := e.bySymbol[o.Symbol]
	if !ok {
		return Result{}, ErrUnknownSymbol
	}
	if len(o.ClientID) > MaxClientIDLen {
		return Result{}, ErrClientIDLen
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err // nothing submitted, so no ID is taken
	}
	seq, err := e.nextID()
	if err != nil {
		return Result{}, err
	}
	id := makeOrderID(seq, m.index)
	cmd := book.Command{
		Kind: book.CommandNewOrder, OrderID: id,
		Side: o.Side, Type: o.Type, TIF: o.TIF, Price: o.Price, Qty: o.Qty,
	}
	if r := cmd.Validate(); r != book.ReasonNone {
		return Result{OrderID: id}, &InvalidError{Reason: r}
	}
	resp, err := e.send(ctx, m, &request{kind: reqCommand, cmd: cmd, clientID: o.ClientID}, orBusy)
	return Result{OrderID: id, Events: resp.events, Status: resp.status}, err
}

// CancelOrder submits a cancel of order id to the book of the symbol the
// ID was issued for and waits until it has been applied, with ctx and
// errors as for PlaceOrder. An ID this engine cannot have issued (counter 0
// or not yet reached, or no such symbol) returns ErrUnknownOrder without
// reaching a book; an issued ID that is not resting gives a Rejected event
// with ReasonUnknownOrder.
func (e *Engine) CancelOrder(ctx context.Context, id book.OrderID) (Result, error) {
	cmd := book.Command{Kind: book.CommandCancel, OrderID: id}
	if r := cmd.Validate(); r != book.ReasonNone {
		return Result{OrderID: id}, &InvalidError{Reason: r}
	}
	m, ok := e.marketOf(id)
	if !ok {
		return Result{OrderID: id}, ErrUnknownOrder
	}
	resp, err := e.send(ctx, m, &request{kind: reqCommand, cmd: cmd}, orBusy)
	return Result{OrderID: id, Events: resp.events, Status: resp.status}, err
}

// Depth is an L2 snapshot of one book: up to the requested number of
// levels per side, best first, as of the event with Seq LastSeq.
type Depth struct {
	Bids, Asks []book.Level
	LastSeq    uint64
}

// Depth returns up to n levels of each side of symbol's book, taken by the
// book's own goroutine between two commands.
func (e *Engine) Depth(ctx context.Context, symbol string, n int) (Depth, error) {
	m, ok := e.bySymbol[symbol]
	if !ok {
		return Depth{}, ErrUnknownSymbol
	}
	resp, err := e.send(ctx, m, &request{kind: reqDepth, n: n}, orWait)
	return resp.depth, err
}

// Order returns the status of order id: always for a resting order, and
// for one of the most recently finished orders of its symbol (see
// Config.History). It returns false for any other ID.
func (e *Engine) Order(ctx context.Context, id book.OrderID) (OrderStatus, bool, error) {
	m, ok := e.marketOf(id)
	if !ok {
		return OrderStatus{}, false, nil
	}
	resp, err := e.send(ctx, m, &request{kind: reqOrder, id: id}, orWait)
	return resp.status, resp.found, err
}

// SymbolOf returns the symbol an order ID was issued for, if the engine
// can have issued it.
func (e *Engine) SymbolOf(id book.OrderID) (string, bool) {
	m, ok := e.marketOf(id)
	if !ok {
		return "", false
	}
	return m.symbol, true
}

// Symbols returns the configured symbols, in order.
func (e *Engine) Symbols() []string {
	s := make([]string, len(e.markets))
	for i, m := range e.markets {
		s[i] = m.symbol
	}
	return s
}

// QueueLen returns how many requests wait in symbol's queue.
func (e *Engine) QueueLen(symbol string) (int, bool) {
	m, ok := e.bySymbol[symbol]
	if !ok {
		return 0, false
	}
	return len(m.in), true
}

// marketOf returns the market an order ID was issued for, if the engine
// can have issued it.
func (e *Engine) marketOf(id book.OrderID) (*market, bool) {
	i := symbolIndexOf(id)
	if seq := seqOf(id); seq == 0 || seq > e.nextSeq.Load() || i >= len(e.markets) {
		return nil, false
	}
	return e.markets[i], true
}

// nextID issues the next order-ID counter value, first extending the
// durable reservation if the value lies beyond it (D021).
func (e *Engine) nextID() (uint64, error) {
	seq := e.nextSeq.Add(1)
	if seq <= e.reserved.Load() {
		return seq, nil
	}
	e.reserve.Lock()
	defer e.reserve.Unlock()
	if seq > e.reserved.Load() {
		if err := e.extendReservation(seq); err != nil {
			return 0, err
		}
	}
	return seq, nil
}

// extendReservation durably reserves the block after from. The caller
// holds e.reserve, or no other goroutine runs yet.
func (e *Engine) extendReservation(from uint64) error {
	high := max(from, e.reserved.Load()) + idBlock
	if err := e.ids.store(high); err != nil {
		return fmt.Errorf("engine: reserving order IDs: %w", err)
	}
	e.reserved.Store(high)
	return nil
}

// queueFull says what send does when m's queue is full.
type queueFull bool

const (
	orBusy queueFull = true  // return ErrBusy: commands (backpressure)
	orWait queueFull = false // wait for room until ctx ends: queries
)

// send queues r on m and waits for its response. It holds the read lock
// while sending, so Close cannot close the queue under it.
func (e *Engine) send(ctx context.Context, m *market, r *request, full queueFull) (response, error) {
	if err := ctx.Err(); err != nil {
		// Checked first: a select with a ready queue and a done context
		// would pick either case at random.
		return response{}, err
	}
	r.reply = make(chan response, 1)
	e.mu.RLock()
	if e.closed {
		e.mu.RUnlock()
		return response{}, ErrClosed
	}
	if full == orBusy {
		select {
		case m.in <- r:
		default:
			e.mu.RUnlock()
			return response{}, ErrBusy
		}
		e.mu.RUnlock()
		// A queued command is always applied (Close drains the queue), so
		// its result is waited for rather than lost to ctx (D020).
		resp := <-r.reply
		return resp, resp.err
	}
	select {
	case m.in <- r:
	case <-ctx.Done():
		e.mu.RUnlock()
		return response{}, ctx.Err()
	}
	e.mu.RUnlock()
	select {
	case resp := <-r.reply:
		return resp, resp.err
	case <-ctx.Done():
		return response{}, ctx.Err()
	}
}
