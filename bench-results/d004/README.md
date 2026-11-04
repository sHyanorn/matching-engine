# D004 benchmark: red-black tree vs skip list

Data behind decision D004 in `docs/DECISIONS.md`.

- Machine: Intel Xeon @ 2.10GHz cloud VM (4 vCPU), Go 1.24.7, linux/amd64
- Command, run in each prototype directory one at a time:
  `go test -run='^$' -bench=. -benchmem -count=10 .`
- Comparison: `benchstat naive.txt rb.txt sl.txt` (see `results/threeway.txt`)

## Layout

- `prototypes/naive`, `prototypes/rb`, `prototypes/sl`: three implementations of
  the same `levelSet` interface (`set.go`), sharing model-based tests
  (`set_test.go`) and benchmarks (`bench_test.go`). Each is its own Go module,
  so the main module's `./...` does not build them.
- `results/`: raw `go test -bench` output for each, plus the benchstat table.

## Workloads (one book side, P = 100, 1,000, 10,000 levels)

| Benchmark | What it models |
|---|---|
| Find | look up an existing level, prices drawn near the top |
| ChurnNearTop | a level near the top empties and another appears |
| SweepBest | an aggressive order empties the best level |
| Walk20 | read the top 20 levels (L2 depth) |
| Build | insert P levels in random order (startup, restore) |

## Medians (P = 100 / 1,000 / 10,000)

| Benchmark | red-black tree | skip list | sorted slice |
|---|---|---|---|
| Find | 6.0 / 27.1 / 48.1 ns | 26.8 / 66.8 / 100.7 ns | see results |
| ChurnNearTop | 126.5 / 183.1 / 249.5 ns | 159.2 / 240.6 / 404.7 ns | grows to ~3.4 µs |
| SweepBest | 85.6 / 119.0 / 143.9 ns | 171.8 / 190.4 / 227.0 ns | grows to ~3.5 µs |
| Walk20 | ~104 ns | ~89 ns (not significant in an interleaved rerun) | ~77 ns |
| Memory per level (1M levels) | 48 B, 1 alloc | 58.7 B, 2 allocs | |

The prototype `Level` holds only a price and a total. The production
`priceLevel` also holds the FIFO queue, so its per-level size is re-measured
in `internal/book`.
