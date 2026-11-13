# Stream byte bound: end-to-end latency before and after

Order-to-trade-push latency before and after the WebSocket byte bound
stopped counting the largest queued message (D026, update of 2026-11-13).
`before` is bab0696, `after` is 9262418.

- Machine: a 4-CPU cloud container (Intel Xeon @ 2.10GHz, ext4), not the
  VM behind docs/BENCHMARKS.md; each sweep's `env.txt` has the details.
- Command, from a clean checkout of each commit:
  `scripts/e2e.sh OUT 10000 30000` (three runs per rate, 10 s warmup,
  30 s measured).
- Order: 1-before, 2-after, then 3-before, 4-after. The container
  restarted between the two pairs.

Trade push latency in µs, median of three runs, and each run's p99. Every
run was valid.

| Rate | Sweep | p50 | p99 | p99 of each run |
|---|---|---|---|---|
| 10k/s | 1-before | 204 | 1,682 | 1,682, 1,643, 1,755 |
| 10k/s | 2-after | 207 | 1,959 | 1,893, 1,959, 1,974 |
| 10k/s | 3-before | 249 | 2,959 | 2,959, 2,873, 3,083 |
| 10k/s | 4-after | 270 | 2,792 | 2,619, 3,004, 2,792 |
| 30k/s | 1-before | 241 | 10,956 | 10,412, 11,208, 10,956 |
| 30k/s | 2-after | 255 | 21,337 | 12,894, 25,187, 21,337 |
| 30k/s | 3-before | 471 | 19,927 | 19,927, 20,001, 19,856 |
| 30k/s | 4-after | 442 | 18,434 | 16,902, 18,434, 19,318 |

`after` is slower in the first pair and faster in the second, and the same
commit moved further between the pairs (p99 at 10k/s: 1,682 to 2,959 µs)
than the two commits differ within either pair. No difference between the
commits stands out from this machine's variation.
