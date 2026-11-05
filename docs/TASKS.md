# Task List

Check off each item when it is done. All tests must pass at the end of every milestone.

## M0 Project setup
- [x] Confirm the go.mod module path; add staticcheck and golangci-lint configuration
- [x] Define the core types: Order, Side, OrderType, TIF, Trade, Event, Command
- [x] Write a Makefile: test, bench, fuzz, lint, run

## M1 Order book core
- [x] Implement the ordered price-level structure (red-black tree or skip list), with its own unit tests
- [x] Implement the FIFO doubly linked list for PriceLevel
- [x] Implement resting and matching of limit GTC orders
- [x] Implement cancels (O(1) lookup)
- [x] Implement L2 market depth queries
- [x] Table-driven matching scenario tests, at least 20 scenarios

## M2 Full order types and correctness
- [x] Market orders
- [x] IOC and FOK
- [x] Invariant check function `book.CheckInvariants()`
- [x] Property test: 100,000 random commands, checking invariants after every step
- [x] Fuzz test `FuzzBook`

## M3 Engine layer
- [x] Event loop with one goroutine per symbol
- [x] Command log (append-only file) and replay
- [x] Replay determinism tests
- [x] Event bus with fan-out to subscribers
- [x] Backpressure via bounded channels

## M4 Network API
- [x] REST API with input validation
- [x] WebSocket subscriptions and streaming, with message sequence numbers
- [x] Graceful shutdown (stop accepting, drain the queue, flush the log)
- [x] Prometheus metrics: order count, trade count, queue length, latency histogram
- [x] Integration tests

## M5 Performance
- [x] Matching core benchmarks
- [x] `cmd/loadgen` reproducible order flow generator
- [x] End-to-end latency measurement
- [x] pprof analysis and at least one data-backed optimization (e.g. object pooling to reduce allocations)
- [x] Write `docs/BENCHMARKS.md`, with charts

## M6 Presentation
- [x] Dockerfile and docker compose
- [ ] README (English version only): introduction, architecture diagram, performance table, GIF, quick start
- [x] Optional: simple web order book visualization
- [ ] Tidy up DECISIONS.md so that every decision has a rationale
