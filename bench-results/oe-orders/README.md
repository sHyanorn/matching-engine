# Orders and cancels over an OE1 session

Benchmarks behind D044. All on one cloud VM (4 vCPUs of an Intel Xeon at
2.10 GHz, KVM, 15.7 GiB, Linux 6.18.44, Go 1.27.4), otherwise idle. Compare
only within this directory. The engine runs on the default configuration,
with its data directory on the VM's disk.

`run.sh` alternates the variants round by round, as in `core-reuse/`; each
round runs a benchmark of 300,000 commands (200,000 for `internal/oe`) once
per variant. `benchstat` is golang.org/x/perf v0.0.0-20261009192801-be2c69fb417e;
ten rounds each.

## Variants

- `before`: bfa2f5d plus the PlaceOrder benchmark.
- `queue`: requests passed by value on the symbol's queue, and replies to a
  sink (`Engine.SendOrder`, `SendCancel`).
- `free`: `queue` with the statuses of finished orders kept for new ones. This
  is the commit.
- oe `before`: bfa2f5d plus `BenchmarkPing`. oe `orders`: the commit.

## Results

`BenchmarkPlaceOrder` (`internal/engine`, the REST path: one command sent,
answered, then the next), `benchstat-engine-place.txt`:

| | before | queue | free |
|---|---|---|---|
| time per command | 5.24 µs | 5.06 µs (p=1.0) | 5.39 µs (p=0.39) |
| bytes per command | 691 | 595 | 542 |
| allocations per command | 5 | 4 | 3 |

The REST path is not slower. `BenchmarkSendOrder` (256 commands in flight,
answered to a sink), `benchstat-engine-send.txt`:

| | queue | free |
|---|---|---|
| time per command | 1.557 µs | 1.468 µs (-5.75%, p=0.037) |
| bytes per command | 155 | 102 (-34%) |
| allocations per command | 1 | 0 |

`benchstat-engine-place-vs-send.txt`: SendOrder takes 1.468 µs against
5.386 µs for PlaceOrder (-72.7%), 102 against 542 bytes, 0 against 3
allocations. 0 allocations is the integer part of the mean: the 102 bytes are
map growth, and `TestSendAllocatesNothing` finds none once the book, the
order history and the tracker have grown to the size of the flow.

`BenchmarkPing` (`internal/oe`, 256 Pings in a write over loopback, their
Pongs read before the next write), `benchstat-oe-ping.txt`: 399.5 ns before,
428.2 ns after (p=0.35), 0 allocations in both. The Ping path of D042 is not
slower.

`BenchmarkOrders` (`internal/oe`, a real engine, loopback, 256 commands of the
SPEC §9 flow in a write and their replies read before the next write),
`summary-oe-orders.txt`: 5.25 µs per command ± 7%, 140 bytes, 0 allocations
(integer part). The time includes the client, which shares the four CPUs with
the server's reader and writer, the symbol's goroutine and the log's fsync,
and the fill and drain of each batch.
