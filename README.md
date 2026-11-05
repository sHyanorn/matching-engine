# matching-engine

A limit order book matching engine written in Go, with price-time priority matching, real-time WebSocket market data, deterministic replay and a published benchmark report.

> Status: in development. See [docs/TASKS.md](docs/TASKS.md).

## Highlights

Measured on a 4 vCPU cloud VM; see [docs/BENCHMARKS.md](docs/BENCHMARKS.md).

| Metric | Value |
|---|---|
| Core throughput, one symbol | 3.0M to 4.5M commands/s |
| End-to-end p99, order to trade push | 3.8 ms at 10k orders/s |
| Memory, 1M resting orders | 128 MiB |
| Test coverage | 92% of statements |

## Docs

- [Specification](docs/SPEC.md)
- [Tasks](docs/TASKS.md)
- [Design decisions](docs/DECISIONS.md)
- [Benchmarks](docs/BENCHMARKS.md)

## License

MIT
