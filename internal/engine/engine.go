package engine

import (
	"context"
	"errors"
	"fmt"
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
	ErrClientIDLen   = fmt.Errorf("engine: client_id longer than %d bytes", MaxClientIDLen)
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

// Config configures an Engine. Only Symbols is required.
type Config struct {
	// Symbols lists the symbols the engine serves, one book each. A
	// symbol's index in this list is part of every order ID issued for
	// it, so the order must not change between runs.
	Symbols []string

	// QueueSize is the capacity of each symbol's request queue
	// (default 4096).
	QueueSize int

	// MaxBatch is how many queued requests a symbol's goroutine takes at
	// once (default 256).
	MaxBatch int

	// Clock returns the time stamped on each command, in Unix nanoseconds
	// (default the system clock). Tests replace it for fixed values.
	Clock func() int64
}

const (
	defaultQueueSize = 4096
	defaultMaxBatch  = 256
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
		if !symbolPattern.MatchString(s) {
			return fmt.Errorf("engine: invalid symbol %q", s)
		}
		if seen[s] {
			return fmt.Errorf("engine: duplicate symbol %q", s)
		}
		seen[s] = true
	}
	if c.QueueSize <= 0 {
		c.QueueSize = defaultQueueSize
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

	// mu guards closed and the sends to market queues, so Close never
	// closes a queue while a send to it is in progress.
	mu     sync.RWMutex
	closed bool
	wg     sync.WaitGroup
}

// New starts an engine with one goroutine per configured symbol.
func New(cfg Config) (*Engine, error) {
	if err := cfg.setDefaults(); err != nil {
		return nil, err
	}
	e := &Engine{bySymbol: make(map[string]*market, len(cfg.Symbols))}
	for i, s := range cfg.Symbols {
		m := newMarket(s, i, cfg)
		e.markets = append(e.markets, m)
		e.bySymbol[s] = m
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
// requests already queued, and waits for them to stop. It is safe to call
// more than once.
func (e *Engine) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	for _, m := range e.markets {
		close(m.in)
	}
	e.mu.Unlock()
	e.wg.Wait()
	return nil
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
// it concerns and the events the book emitted for it, in order. Events is
// shared with event subscribers and must not be modified.
type Result struct {
	OrderID book.OrderID
	Events  []book.Event
}

// PlaceOrder assigns the next order ID, validates the order and submits it
// to its symbol's book, then waits until it has been applied. A command
// that fails validation returns an *InvalidError and is not applied. If ctx
// ends while waiting, PlaceOrder returns ctx.Err() and the order may or may
// not have been applied; Order tells which.
func (e *Engine) PlaceOrder(ctx context.Context, o NewOrder) (Result, error) {
	m, ok := e.bySymbol[o.Symbol]
	if !ok {
		return Result{}, ErrUnknownSymbol
	}
	if len(o.ClientID) > MaxClientIDLen {
		return Result{}, ErrClientIDLen
	}
	id := makeOrderID(e.nextSeq.Add(1), m.index)
	cmd := book.Command{
		Kind: book.CommandNewOrder, OrderID: id,
		Side: o.Side, Type: o.Type, TIF: o.TIF, Price: o.Price, Qty: o.Qty,
	}
	if r := cmd.Validate(); r != book.ReasonNone {
		return Result{OrderID: id}, &InvalidError{Reason: r}
	}
	resp, err := e.send(ctx, m, &request{kind: reqCommand, cmd: cmd, clientID: o.ClientID})
	return Result{OrderID: id, Events: resp.events}, err
}

// CancelOrder submits a cancel of order id to the book of the symbol the
// ID was issued for and waits until it has been applied. An ID this engine
// cannot have issued returns ErrUnknownOrder; a valid ID that is not
// resting gives a Rejected event with ReasonUnknownOrder.
func (e *Engine) CancelOrder(ctx context.Context, id book.OrderID) (Result, error) {
	cmd := book.Command{Kind: book.CommandCancel, OrderID: id}
	if r := cmd.Validate(); r != book.ReasonNone {
		return Result{OrderID: id}, &InvalidError{Reason: r}
	}
	m, ok := e.marketOf(id)
	if !ok {
		return Result{OrderID: id}, ErrUnknownOrder
	}
	resp, err := e.send(ctx, m, &request{kind: reqCommand, cmd: cmd})
	return Result{OrderID: id, Events: resp.events}, err
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
	resp, err := e.send(ctx, m, &request{kind: reqDepth, n: n})
	return resp.depth, err
}

// Order returns the resting order with this ID, and false if it is not
// resting (unknown, filled or canceled).
func (e *Engine) Order(ctx context.Context, id book.OrderID) (book.Order, bool, error) {
	m, ok := e.marketOf(id)
	if !ok {
		return book.Order{}, false, nil
	}
	resp, err := e.send(ctx, m, &request{kind: reqOrder, id: id})
	return resp.order, resp.found, err
}

// marketOf returns the market an order ID was issued for, if any.
func (e *Engine) marketOf(id book.OrderID) (*market, bool) {
	i := symbolIndexOf(id)
	if id == 0 || i >= len(e.markets) {
		return nil, false
	}
	return e.markets[i], true
}

// send queues r on m and waits for its response. It holds the read lock
// while sending, so Close cannot close the queue under it.
func (e *Engine) send(ctx context.Context, m *market, r *request) (response, error) {
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
