# Cost of the wake meter

The wake meter of `pairrun supervise` (D051) sleeps 1 ms in a loop on one CPU
of the engine machine. This is its cost, measured on one shared cloud VM of
4 vCPUs (Intel Xeon at 2.30 GHz, Linux 6.18). It shows the cost on this
machine only, not on a Pod.

## Files

- `analyze.py [DIR]` prints the tables and the verdict below from the files in
  `DIR/runs/` (default: its own directory); `analysis.txt` is its output.
- `runs/NN-without.json`, `runs/NN-with.json`: what `analyze.py` reads of run
  NN. From loadgen's report: `valid`, `problems`, `achieved_rate` and the
  percentile fields (`p50` to `max`, in us) of the rows `reply`, `hiccup` and
  `send_lag`; then loadgen's exit code `exit`. A run with the meter also has
  its rows under `meter`: for each second the wakes `n` and the `p99` and
  `max` of their lateness in us, and the `placement` the meter reported.
- `runs/NN-without.txt`, `runs/NN-with.txt`: loadgen's text table of run NN.
- `hold-meter.go.txt`: two tests of `cmd/pairrun`, kept as text so that they
  are not part of the build. `TestB2MeterCost` measures the meter's own cost.
  `TestB2HoldMeter` runs a meter while another process runs, as in the runs
  with the meter below. To run them, copy the file to
  `cmd/pairrun/b2_meter_cost_test.go`.
- `cost-30s.txt`: the output of `B2_SECONDS=30 go test -run TestB2MeterCost
  -count=1 -v ./cmd/pairrun/`. `cost-30s-nanosleep.txt`: the same with
  `B2_SLEEP=nano`.

## Effect on the engine's latency

Engine in its default configuration on CPUs 0 and 1, `loadgen -oe -ioc` (R1)
at 10,000/s with a 10 s warmup and 60 s measured, on CPUs 2 and 3, a fresh
engine and an empty data directory for each run. Six runs with the meter and
six without, alternating. The meter ran in the process of `TestB2HoldMeter` on
CPUs 0 and 1, so the placement rule pinned it to CPU 1, one of the engine's two
CPUs. On a pair with more CPUs the meter takes a smaller share of the engine's
CPUs, and with `-oe-spin` the spin pool does not hand out the meter's CPU.
Where it does, the placement note of the wake file says "shares a CPU with the
spin pool". Reply row, in us, median of each group and, in brackets, its spread
(largest minus smallest):

| | without the meter | with the meter | difference of medians |
|---|---|---|---|
| p50 | 47.1 (12.2) | 49.6 (6.4) | 2.4 |
| p99 | 999.7 (876.1) | 920.8 (1094.4) | 78.9 |
| p99.9 | 4347.6 (5396.8) | 4855.6 (10205.4) | 508.1 |

The rule: drop the meter if, for any of the three percentiles, the difference
of the medians exceeds the spread of the group without it. None does. The
machine was too noisy for more: all twelve runs missed the send lag bound of
10 us (the p99.9 of send lag was 1.9 to 10.5 ms), so the comparison can show
only a large effect.

## Own cost

`cost-30s.txt`: over 30 s with nothing else running and the meter on CPU 3,
where the placement rule puts it on this machine, the meter's thread used
13.4 ms of CPU a second (1.34% of one CPU). It woke about 850 times a second at
rest: 859 by the kernel's count of its timeslices and 839 by its rows, whose
first second is partial. The six runs with the meter above have 882 to 896 wakes a second in its
rows, averaged over every second the meter recorded, the partial first one
included. The whole process used 62.4 ms of CPU a second
(6.2% of one CPU), against 0.04 ms a second in the 30 s before the meter
started. The rest ran in the process's other threads. With a raw `nanosleep`
in place of the runtime's timer (`cost-30s-nanosleep.txt`) the thread used
32.8 ms a second and the process 61.8 ms, so it saves nothing.
