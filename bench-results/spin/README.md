# Spin benchmarks
`go test -run '^$' -bench PATTERN -benchmem -count=1`, ten runs each, one process per run: `BenchmarkOrders` and `BenchmarkPing` against their `Spin` twins and `BenchmarkAnswerAtLimit` (before: `session.go`, `server.go` and `conn_linux.go` of 6f7b666) in `./internal/oe`, `BenchmarkTake` in `./cmd/loadgen`; `benchstat.txt` compares the raw files.
`BenchmarkOrders` and `BenchmarkPing` send 256 commands per write and read all replies before the next write, in one process over loopback; ns/op is one command. `BenchmarkAnswerAtLimit` times what the symbol's side takes to give back a place of a session that has 1,024 commands unanswered while its reader waits; with Spin=on the reader polls a counter and is not pinned here. Its p50, p99 and max are per answer, taken per run; the table gives the median of ten runs. `BenchmarkTake` times the loadgen reader's take of one reply, with and without a trade Event; it is not a comparison of the mode.
Intel Xeon 2.30 GHz (KVM), 4 vCPUs, Linux 6.18.44-fc-v726, Go 1.27.4, runs started 2026-11-23T00:32:24Z. These are loopback microbenchmarks on a shared cloud container, with client and server in one process sharing the four vCPUs, and the comparison on the pilot pair decides.

| median of ten, ns/op (allocs/op) | off or before | on or after | ratio |
|---|---|---|---|
| Orders | 3923 (0) | 3886 (0) | 0.99 |
| Ping | 289 (0) | 335 (0) | 1.16 |
| TakeTrade | n/a | 50.9 (0) | n/a |
| TakeNoTrade | n/a | 32.1 (0) | n/a |
| AnswerAtLimit Spin=on, p50 / p99 / max ns per answer | 1159 / 95269 / 2273951 | 329 / 854 / 344607 | 0.28 / 0.0090 / 0.15 |
| AnswerAtLimit Spin=off, p50 / p99 / max ns per answer | 7735 / 18823 / 182986 | 7797 / 18427 / 126621 | 1.01 / 0.98 / 0.69 |

Ratio is on or after divided by off or before; Ping with the mode on took 16% longer.
