# Benchmarks

Every number here comes from one machine and can be measured again with the commands at the end. The raw results are in [`bench-results/`](../bench-results).

**Machine**: a cloud VM with an Intel Xeon at 2.10 GHz, 4 vCPUs and 16 GB, Linux 6.18 on ext4, Go 1.24.7. In the end-to-end runs the load generator and the engine share the 4 vCPUs.

## Matching core

`BenchmarkApply` replays 65,536 commands of the SPEC §9 order flow ([`internal/flow`](../internal/flow)): GTC limit orders priced around the mid with a normal spread of 5 or 50 ticks, and about 30% cancels of earlier orders, some of them already filled. Each pass starts from an empty book. `BenchmarkApplyDeep` runs the same flow on a book that also holds 10k or 1M resting orders outside the flow's prices, as a book does after hours of trading. The figures are medians of 10 runs ([`bench-results/m5-core/baseline.txt`](../bench-results/m5-core/baseline.txt)).

| Book | Spread | ns per command | Commands per second | Allocations per command |
|---|---|---|---|---|
| empty | 5 ticks | 224 ± 7% | 4.47M | 0.59 |
| empty | 50 ticks | 242 ± 7% | 4.13M | 0.68 |
| 10k resting | 5 ticks | 237 ± 4% | 4.22M | 0.59 |
| 10k resting | 50 ticks | 245 ± 9% | 4.09M | 0.68 |
| 1M resting | 5 ticks | 280 ± 8% | 3.57M | 0.59 |
| 1M resting | 50 ticks | 334 ± 14% | 3.00M | 0.68 |

The SPEC target of 1M operations per second on one symbol holds with 3 to 4.5 times to spare. Go's `allocs/op` prints 0 for these runs because it rounds down; `allocs/cmd` keeps the fraction. An order that rests allocates its node, a new price level its tree node, and the ID map grows now and then.

**Memory**: in the matching core alone, 1,000,000 resting orders on 1,000 price levels per side hold 127.7 MiB of heap, 134 bytes per order (`BenchmarkRestingOrders`). The engine process holds more, among it the status of every resting order and of the last 100,000 finished ones per symbol (D022).

## End to end

Method ([D030](DECISIONS.md)):

- [`cmd/loadgen`](../cmd/loadgen) offers the same flow at a fixed rate of requests, 70% of them new orders and 30% cancels. Request i is due at t0 + i/rate whatever the replies, and goes out over one of 64 keep-alive connections. Latency counts from the due time, so a stall is charged to every request it delayed.
- The latency reported here runs from the due time of a new order to the first trade push for it on the WebSocket. The JSON reports also hold the reply latency of every request, the service time (from the actual send) and the send lag.
- Each run starts a fresh engine on an empty data directory (fsync every 10 ms, one symbol), discards a 10 s warmup and measures 30 s. Each rate runs three times, and the run with the median p99 stands for it. A run is valid only if 99% of the target rate went out, no request failed, and every trade was pushed once and in order ([D036](DECISIONS.md)): the push numbers run without gaps, and the pushes, the trades in the replies and the engine's trade counter agree in number and, by a hash of each trade, in content.
- A 1 ms sleep loop in loadgen records pauses of the generator and the machine (hiccups, after jHiccup).
- More connections than the load needs only add work on both sides: at 15k/s, 512 connections gave a p99 of 11.2 ms against 7.26 ms with 64 ([`conns-64`](../bench-results/m5-e2e/conns-64), [`conns-512`](../bench-results/m5-e2e/conns-512)).

Latency from order to trade push, in ms, after the optimizations below ([`bench-results/m5-e2e/after-fsync`](../bench-results/m5-e2e/after-fsync)):

| Requests/s | Valid runs | p50 | p90 | p99 | p99.9 | max | p99 of each run | Engine CPU per request | Worst hiccup |
|---|---|---|---|---|---|---|---|---|---|
| 10k/s | 3 of 3 | 0.30 | 0.57 | 3.82 | 11.2 | 18.0 | 3.81, 3.82, 7.68 | 131 µs | 6 ms |
| 15k/s | 3 of 3 | 0.35 | 1.10 | 7.41 | 12.5 | 16.6 | 7.24, 7.41, 7.81 | 105 µs | 10 ms |
| 20k/s | 3 of 3 | 0.39 | 2.42 | 11.6 | 19.2 | 29.5 | 11.5, 11.6, 11.7 | 84 µs | 11 ms |
| 25k/s | 3 of 3 | 0.56 | 6.28 | 19.8 | 28.2 | 37.3 | 19.6, 19.8, 28.7 | 71 µs | 12 ms |
| 30k/s | 3 of 3 | 0.98 | 15.8 | 42.3 | 92.7 | 103.0 | 40.2, 42.3, 47.7 | 62 µs | 50 ms |
| 40k/s | 0 of 1 | | | | | | sent 39.2k/s | | |

![p50, p99 and p99.9 by load](img/latency-by-load.svg)

- **Capacity.** Near 40k requests/s the two processes use up the 4 vCPUs; the engine alone takes about 1.8 cores at 25k/s. So the SPEC target, a p99 under 5 ms at 50,000 orders/s (about 71k requests/s in this flow), is out of reach on this machine. The p99 is under 5 ms at 10k requests/s.
- **Engine CPU per request** is the process's CPU time over the run divided by the requests it answered. It falls as the rate rises because the fixed costs, from idle wakeups to GC, are shared by more requests.
- **The tail** above p99 depends on single pauses. The hiccup meter runs no engine code, yet it saw pauses of up to 50 ms; the 10k/s run with a 7.68 ms p99 had one of 128 ms. The spread between runs at one rate shows how far to trust a single run.

## Where the time goes

A CPU profile of the engine at 25k/s before the optimizations ([`bench-results/m5-opt/cpu-before.pprof`](../bench-results/m5-opt/cpu-before.pprof)):

![Flame graph before the optimizations](img/flamegraph-before.png)

Reading and writing HTTP takes the largest share, about 19% in system calls alone. Decoding request bodies took 14%, 9% of it in the check of their keys. GC took about 10%, and the matching goroutine, book included, about 10%.

## Optimizations

Each change was measured with the full sweep above; [`bench-results/m5-e2e`](../bench-results/m5-e2e) holds the three sweeps.

| Requests/s | p99 before | Keys scanned | And fsync aside | Engine CPU per request |
|---|---|---|---|---|
| 10k/s | 5.65 | 5.32 | 3.82 | 138 → 131 → 131 µs |
| 15k/s | 9.64 | 8.25 | 7.41 | 107 → 101 → 105 µs |
| 20k/s | 19.8 | 12.9 | 11.6 | 88 → 82 → 84 µs |
| 25k/s | 26.7 | 36.4 | 19.8 | 75 → 69 → 71 µs |
| 30k/s | sent 29.4k/s | 37.7 | 42.3 | 61 → 62 µs |
| 40k/s | | sent 32.8k/s | sent 39.2k/s | |

![Latency by percentile at 20k/s for each step](img/latency-percentiles.svg)

**Checking request keys with one scan.** A request body must be one JSON object whose field names match exactly, with no unknown or repeated fields ([API](API.md#post-v1orders)). The check decoded every value with a `json.Decoder`, which builds a syntax error after each key and scalar value and throws it away. Now `json.Valid` checks the syntax and one scan finds the keys; a fuzz test holds the new check to the old one's answers.

- The check takes 0.51 µs instead of 5.79 µs, with 0 allocations instead of 79 ([`checkkeys.txt`](../bench-results/m5-opt/checkkeys.txt)).
- It fell from 9.2% to 1.1% of the engine's CPU profile, and the engine's CPU per request fell 5% to 8%.
- 30k/s became sustainable, and 40k/s sent 32.8k/s.

**Running the fsync beside the matching goroutine** ([D031](DECISIONS.md)). Every 10 ms the symbol's goroutine stopped for its fsync, 0.8 ms on average and tens of milliseconds at times, and every command waited. A helper goroutine now runs it, as Redis, Kafka and PostgreSQL do with their logs.

- At 10k/s, p90 fell from 0.91 to 0.57 ms and p99 from 5.32 to 3.82 ms.
- At 25k/s the runs' p99 went from 20.0, 36.4 and 215 ms to 19.6, 19.8 and 28.7 ms.
- 40k/s sent 39.2k/s. The engine's CPU per request rose by up to 3%, with no change at 10k/s.

The profile after both changes ([`cpu-after.pprof`](../bench-results/m5-opt/cpu-after.pprof)); the fsync runs on its own stack (`syncLoop`):

![Flame graph after the optimizations](img/flamegraph-after.png)

The book allocates 0.6 times per command, but `Book.Apply` is 1.4% to 2% of the engine's CPU, so pooling its nodes would not show end to end; it is not done.

## Startup and replay

The engine starts by replaying each symbol's whole command log; there are no snapshots and no log compaction (SPEC §3 lists snapshot recovery as optional). `BenchmarkReplay` starts an engine on a log of 1M commands of the flow and closes it ([`bench-results/replay`](../bench-results/replay/replay.txt), median of 10 runs):

- **Speed.** 1.87M commands/s, 534 ns each, about 3.5 times the cost of `Book.Apply` alone: each record is also read, checksummed and decoded, and the order history is rebuilt. A log takes 45 bytes per command without a `client_id`.
- **Startup time.** At 10k requests/s, an hour of load is about 36M commands, a 1.6 GB log and 19 s of replay; a day is 864M commands, 39 GB and about 8 minutes. The benchmark reads a log the page cache holds; a cold start also waits for the disk to read the log.
- **Known limit.** The log grows without bound, and so does the startup time with it. Snapshots with log truncation would bound both; they are not built.

## On Windows

All the figures above are from Linux. On GitHub's Windows runners the cost of an fsync depends on the disk ([`disks.txt`](../bench-results/m6-windows/disks.txt)). On the runner's system disk the median fsync after a write ranged from 3 to 31 ms across runs and write sizes, a write-through write from 4 to 30 ms, and writes to the file being flushed wait meanwhile; there, requests wait tens of milliseconds. On its local disk both take under 1 ms, as on Linux; a laptop's local SSD should behave alike, though that was not measured. Writes to another file do not wait for a flush on either disk.

## Measuring again

```bash
go test -run='^$' -bench='^(BenchmarkApply|BenchmarkApplyDeep|BenchmarkRestingOrders)$' -benchmem -count=10 ./internal/book
```

```bash
go test -run='^$' -bench='^BenchmarkReplay$' -benchmem -count=10 ./internal/engine
```

```bash
make e2e OUT=bench-results/e2e
```

```bash
python3 scripts/plot.py sweep docs/img/latency-by-load.svg bench-results/e2e
```

The engine serves profiles with `-pprof`, for example `go run ./cmd/engine -pprof 127.0.0.1:6060`, then `go tool pprof -http=: 'http://127.0.0.1:6060/debug/pprof/profile?seconds=10'` while loadgen runs.
