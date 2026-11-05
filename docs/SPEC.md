# matching-engine Specification

## 1. One-line summary

A high-performance limit order book matching engine written in Go. It matches orders by price-time priority, streams market data in real time over WebSocket, and comes with a complete load-test report.

## 2. Capabilities to demonstrate

| Capability | Where it shows |
|---|---|
| Algorithms and data structures | Hand-written ordered price-level structure, O(1) cancels, complexity analysis |
| Backend development | REST and WebSocket APIs, concurrency model, graceful shutdown, observability |
| Engineering quality | Invariant tests, fuzzing, deterministic replay, benchmarks |
| Financial domain knowledge | Order types, market depth, matching rules |

## 3. Scope

### Required

- One or more symbols, each with its own independent order book
- Order types: limit and market
- Time in force: GTC (good till canceled), IOC (immediate or cancel: fill immediately, cancel the remainder), FOK (fill or kill: fill completely or cancel)
- Order cancellation
- Partial fills
- Queries for market depth (L2 aggregated) and individual order status
- Real-time streaming: trades and incremental book updates
- Command log and deterministic replay: replaying the log yields exactly the same order book state

### Optional

- Order amendment (reducing quantity keeps priority; increasing quantity or changing price re-queues the order)
- Self-trade prevention (buy and sell orders from the same account do not trade with each other)
- Snapshot-plus-delta recovery
- A simple web order book display (reusable on the portfolio website)

### Out of scope

- User accounts, settlement, risk management
- Distributed deployment

## 4. Architecture

```
           REST / WebSocket (internal/api)
                       │
             command channel (chan)
                       │
    ┌──────────────────▼──────────────────┐
    │ engine: one goroutine per symbol    │  single-threaded matching, lock-free
    │ 1. write the command log            │
    │ 2. call book to match               │
    │ 3. publish events                   │
    └──────────────────┬──────────────────┘
                       │
              event bus (fan-out)
                 │            │
          WebSocket push   metrics
```

Design notes:

- Matching uses a single-threaded event loop (modeled on the LMAX architecture), so no locks are needed within a symbol and results are naturally deterministic
- `internal/book` is a pure logic layer: commands in, events out, which makes it easy to test and replay
- The network layer and the matching layer are connected by a bounded channel; when it is full, a busy error is returned, which provides backpressure

## 5. Core data structures

```
OrderBook
├── bids: ordered price-level structure (high to low)
├── asks: ordered price-level structure (low to high)
└── orders: map[OrderID]*OrderNode     // O(1) lookup, used for cancels

PriceLevel
├── price    int64
├── totalQty int64
└── queue    doubly linked list (FIFO) of *OrderNode
```

Ordered price-level structure: a hand-written red-black tree or skip list; choose one and explain the choice in DECISIONS.md. Also cache pointers to the best bid and best ask.

Complexity targets:

| Operation | Complexity |
|---|---|
| Add order (new price level) | O(log P), where P is the number of price levels |
| Add order (existing price level) | O(1) lookup, then enqueue |
| Cancel order | O(1); O(log P) when the level becomes empty |
| Execute one trade | O(1) |
| Get best price | O(1) |
| Top N levels of the book | O(N) |

## 6. Matching rules

1. A new buy order is compared with the best ask; it trades if the bid is greater than or equal to the ask, at the price of the resting (passive) order
2. Orders at the same price fill in order of arrival
3. A market order trades until the opposite side is exhausted or its quantity is used up; any remainder is canceled and never rests on the book
4. The remainder of an IOC order is canceled; FOK checks the fillable quantity first and, if it is not enough, rejects the whole order without producing any trades
5. Each fill produces one Trade event containing both order IDs, the price, the quantity, and a sequence number

## 7. Invariants (for property tests)

- The book is never crossed: the best bid is below the best ask
- Quantity conservation: submitted quantity equals filled plus remaining plus canceled
- Each price level's totalQty equals the sum of the remaining quantities of the orders in its queue
- Orders in the orders map correspond one-to-one with orders in the tree
- Replaying the same command log twice produces exactly the same event sequence

## 8. API

### REST

| Method | Path | Description |
|---|---|---|
| POST | `/v1/orders` | Place an order |
| DELETE | `/v1/orders/{id}` | Cancel an order |
| GET | `/v1/orders/{id}` | Get an order |
| GET | `/v1/books/{symbol}?depth=20` | Market depth |
| GET | `/healthz` | Health check |
| GET | `/metrics` | Prometheus metrics |

Example order request:

```json
{ "symbol": "BTC-USD", "side": "buy", "type": "limit", "tif": "gtc",
  "price": 6500000, "qty": 150, "client_id": "c-001" }
```

### WebSocket `/v1/stream`

Subscribe message: `{"op":"subscribe","channels":["trades:BTC-USD","book:BTC-USD"]}`

Pushed message types: `trade`, `book_delta`, `book_snapshot`. Every message carries an increasing sequence number; a client that detects a gap in the sequence can request a snapshot.

## 9. Performance targets and load testing

On an ordinary laptop (with the CPU model stated in the report):

- Matching core benchmark: more than 1 million operations per second on a single symbol
- End to end (from a REST order to receiving the trade push): p99 latency under 5 ms at a test load of 50,000 orders per second
- Memory: memory usage with 1 million resting orders

`cmd/loadgen` generates a reproducible random order flow (fixed random seed) that simulates a realistic distribution: prices normally distributed around the mid price, and about 30% cancels.

The load-test report `docs/BENCHMARKS.md` covers the test environment, methodology, throughput, latency distribution charts (p50/p90/p99/p999), pprof flame graph screenshots, and before/after optimization comparisons.

## 10. Testing strategy

- Unit tests: every order type and edge case (empty book, an order that fills exactly, a level becoming empty)
- Table-driven tests: matching scenarios described as inputs and expected outputs in YAML or Go tables
- Property tests: generate large numbers of random commands and check the section 7 invariants after every step
- Fuzz tests: native Go fuzzing, ensuring that arbitrary input never crashes and the invariants hold
- Race detection: all tests run under `-race`

## 11. Deliverables

- A runnable service and Docker image
- README: one-line summary, architecture diagram, performance table, demo GIF, quick start
- `docs/BENCHMARKS.md` load-test report
- `docs/DECISIONS.md` technical decision log
