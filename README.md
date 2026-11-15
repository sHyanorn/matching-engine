# matching-engine

[![CI](https://github.com/sHyanorn/matching-engine/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/sHyanorn/matching-engine/actions/workflows/ci.yml?query=branch%3Amain)

A limit order book matching engine in Go: price-time priority matching, REST and WebSocket APIs, a write-ahead command log with deterministic replay, and a load-test report. The matching core runs 6.25M to 8.88M commands/s on one symbol, and the end-to-end p99 from order to trade push is 1.8 ms at 10k requests/s. The figures come from 4 vCPU cloud VMs, and the throughput VM was noisy.

<p align="center"><img src="docs/img/demo.gif" width="800" alt="The order book page following BTC-USD while cmd/loadgen sends 2,000 requests per second"></p>

## Results

Measured on two 4 vCPU cloud VMs; [docs/BENCHMARKS.md](docs/BENCHMARKS.md) says which figure comes from which.

| Metric | Value |
|---|---|
| Core throughput, one symbol | 6.25M to 8.88M commands/s; 1.3 to 2.3 times the book before its own order index and node reuse, on the same VM |
| Matching core against Liquibook, CppTrader and geseq on one VM ([flash1-dev benchmark](docs/BENCHMARKS.md#against-other-engines), every run hash-checked) | Faster than CppTrader, the fastest of the three there, in 46 of 50 paired runs; median ratio 1.28 to 1.60 per scenario |
| End-to-end p99, order to trade push | 1.8 ms at 10k requests/s (70% orders, 30% cancels); 41 ms at 50k ([why](docs/BENCHMARKS.md#50k-requests-per-second-on-another-machine)) |
| Matching core heap, 1M resting orders (`BenchmarkRestingOrders`) | 124 MiB |
| Test coverage | 93% of statements |

## How it works

```mermaid
flowchart LR
    client(["clients"])
    subgraph process["engine"]
        api["HTTP and WebSocket API<br>internal/api"]
        subgraph symbol["each symbol"]
            queue[["bounded queue"]]
            market["market goroutine<br>internal/engine"]
            book["order book<br>internal/book"]
            log[("command log")]
        end
    end
    client -- "orders, cancels, queries" --> api
    api --> queue --> market
    market -- "apply" --> book
    market -- "write ahead" --> log
    market -- "events" --> api
    api -- "snapshots, deltas, trades" --> client
```

- **One goroutine per symbol** owns its book and log, so books need no locks and each book's results are deterministic: replaying its command log reproduces them ([D003](docs/DECISIONS.md)). Requests reach it through a bounded queue; when it is full, orders and cancels get 503 at once and queries wait for room (D020, D024).
- **The book** keeps price levels in a hand-written red-black tree with a FIFO queue per level (D004), and an index by order ID makes a cancel O(1). Prices and quantities are integers (D002).
- **The command log** gets each batch written to the operating system before the batch is applied and answered, so a crash of the process loses no acknowledged command: a test kills the engine under load and checks every order after the restart. A background goroutine fsyncs the log every 10 ms by default (D017, D031). Replaying it rebuilds the same book (D016). One engine at a time owns the data directory (D028).
- **The stream** sends each subscriber a full snapshot, then the levels each command changed, numbered without gaps; a client that falls behind is disconnected and resubscribes (D018, D026).

## Run it

With Docker:

```bash
docker compose up -d --wait
docker compose run --rm loadgen -rate 2000 -warmup 0s -duration 60s
```

Open http://localhost:8080 for the live book. With Go 1.27.4 or later, from the repository root, `go run ./cmd/engine` starts the engine and `go run ./cmd/loadgen -rate 2000 -warmup 0s -duration 60s` sends it order flow. To place an order by hand:

```bash
curl -s localhost:8080/v1/orders -d '{"symbol":"BTC-USD","side":"buy","type":"limit","price":10000,"qty":5}'
```

`make help` lists the test, benchmark, fuzz and load-test targets.

## Limits

- The throughput VM is noisy: the 10 runs of the old or the new book on one benchmark differ by up to 1.96 times in time per command, and the book's speedup over its earlier version is measured on that VM only.
- In the comparison with other engines, one engine's runs vary by up to 2.09 times, and no geseq run passes its probe-time rule.
- 50k requests/s is near saturation, and its p99 depends on the machine.
- A crash of the machine or a power loss can lose acknowledged commands: those of the last fsync interval, plus those taken while an fsync was still running (D017, D031).

## Layout

| Path | What it holds |
|---|---|
| [`internal/book`](internal/book) | The matching core: price levels, matching, events; no I/O, clock or randomness |
| [`internal/engine`](internal/engine) | Per-symbol goroutines, the command log and replay, order IDs, subscriptions |
| [`internal/api`](internal/api) | REST, WebSocket, metrics and the order book page |
| [`cmd/engine`](cmd/engine) | The service |
| [`cmd/loadgen`](cmd/loadgen) | An open-loop load generator that measures latency |
| [`internal/flow`](internal/flow) | The reproducible order flow both benchmarks use |

Docs: [API](docs/API.md) · [Benchmarks](docs/BENCHMARKS.md) · [Design decisions](docs/DECISIONS.md) · [Specification](docs/SPEC.md) and [tasks](docs/TASKS.md). Licence: [MIT](LICENSE).
