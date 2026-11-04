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
| ChurnNearTop | 126.5 / 183.1 / 249.5 ns | 159.2 / 240.6 / 404.7 ns | grows to ~3.6 µs |
| SweepBest | 85.6 / 119.0 / 143.9 ns | 171.8 / 190.4 / 227.0 ns | grows to ~3.5 µs |
| Walk20 | ~104 ns | ~89 ns (13-16% faster; not significant in the interleaved rerun below) | ~72 ns |
| Memory per level (Build/10000 B/op and allocs/op ÷ 10,000) | 48 B, 1 alloc | 58.7 B, 2 allocs | |

The prototype `Level` holds only a price and a total. The production
`priceLevel` also holds the FIFO queue (head, tail, count), so a level node
in `internal/book` takes 80 B and 1 allocation
(`BenchmarkLevelTreeBuild/10000`: 800,006 B/op, 10,000 allocs/op).

## Interleaved rerun: red-black tree vs skip list

`results/interleaved-rb.txt` and `results/interleaved-sl.txt` come from the
two test binaries run alternately, 6 times each with `-test.benchtime=300ms`,
to rule out drift between the separate runs above.
`results/interleaved-rb-vs-sl.txt` is the benchstat table. Every write-heavy
gap holds (the skip list is 22% to 328% slower on Find, ChurnNearTop and
SweepBest); the skip list's Walk20 lead is not significant (p >= 0.065).

## Comparator: bit flip vs if

`levels.go` orders the bid side with `a^flip < b^flip` instead of an `if` on
the side. The `if` variant replaces `before` in `prototypes/rb/rbtree.go`
with:

```go
func (t *rbTree) before(a, b Price) bool {
	if t.flip != 0 {
		return a > b
	}
	return a < b
}
```

`results/flip.txt` and `results/if.txt` are the two binaries run alternately,
10 times each (`-test.bench='Find|SweepBest|ChurnNearTop' -test.benchmem`),
on a later VM of the same type that reported a 2.80GHz Xeon.
`results/flip-vs-if.txt` is the benchstat table: with the `if`, Find is 93%
slower at P = 100 and 11% to 15% slower at 1,000 and 10,000; SweepBest and
ChurnNearTop are 6% to 16% slower or within noise; the geometric mean is 17%
slower.
