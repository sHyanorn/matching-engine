# First pilot between two rented Pods

The pilot matrix of D050 on one pair of rented Pods (D043), 8 vCPUs each, in
EU-RO-1, at commit cd0548b234d8d904b15388223e646b29a390b5e9, on 2026-11-23
from 03:32:47 to 04:04:25 UTC. The gate G2 stopped it after 3 of its 5
rounds: the three R1 runs had a reply p99 of 86.956, 87.951 and 317.442 µs,
3.65 times apart against a limit of 2 (condition 2). 26 of the 27 runs were
valid. The commit does not have the diagnostics of D051, so the files cannot
say when inside a run the tail rose or on which machine.

## Results

G2 (`g2.json`):

| Condition | Status | Figures |
|---|---|---|
| 1. At least 4 of 5 runs valid in every configuration | open | 3 rounds ran; every configuration 3 of 3 valid except T-71k, 2 of 3 |
| 2. R1 reply p99, largest over smallest, at most 2 | fail | 86.956 and 317.442 µs, 3.65 |
| 3. R1 median reply p50 at most 100 µs over the floor's | pass | 68.305 against 59.035 µs, 9.27 µs over |

The matrix stops after a round once condition 2 has failed (D050), so rounds
4 and 5 and the 15-minute R1 run did not run. No variant of R1 was judged:
the adoption rule wants 4 valid runs of each, and each had 3.

Reply row (pong for F), in µs, minimum/median/maximum over the valid runs
(`summary.txt`). F's pong row is the pair's floor.

| Configuration | Valid | p50 | p99 | p99.9 |
|---|---|---|---|---|
| F (floor) | 3 of 3 | 57.5/59.0/60.0 | 70.6/71.6/76.2 | 83.5/91.1/213.4 |
| R1 | 3 of 3 | 67.7/68.3/71.0 | 87.0/88.0/317.4 | 127.5/137.0/2,433.7 |
| R1-gogc-off | 3 of 3 | 68.5/70.1/70.8 | 83.9/89.5/886.1 | 111.4/144.0/3,163.0 |
| R1-spin-pin | 3 of 3 | 55.8/58.0/58.9 | 68.7/69.7/71.5 | 97.4/100.2/120.4 |
| R2 | 3 of 3 | 69.2/69.8/70.3 | 85.7/85.9/136.5 | 124.0/124.4/921.0 |
| R3-10k | 3 of 3 | 69.2/70.0/70.5 | 85.7/86.3/87.6 | 116.7/118.6/562.9 |
| R3-71k | 3 of 3 | 82.6/83.7/86.5 | 134.9/135.5/144.0 | 1,422.6/1,692.6/2,300.3 |
| T-71k | 2 of 3 | 88.7/91.6/94.5 | 285.2/291.0/296.8 | 5,799.3/9,072.6/12,346.0 |
| REST-10k | 3 of 3 | 173.3/174.5/176.3 | 307.4/315.8/328.7 | 1,511.9/1,607.3/1,680.5 |

The median of two runs is their mean, so the median of T-71k may be the
figure of no run. Where they apply, `summary.txt` also has the rows of
service time, send lag, hiccup, cancel, order to cancel and trade push.

The raised tail of condition 2 is in round 3. R1 had a p99 of 87.951 µs in
round 1 and 86.956 µs in round 2, and 317.442 µs in round 3, from 03:55:02
to 03:56:12 UTC; R1-gogc-off, which ran next, had 83.919 and 89.539 µs and
then 886.053 µs. These two runs are the only ones whose p99 is more than
twice their configuration's median, and they ran back to back, from 03:55:02
to 03:57:22. R1-spin-pin, which ran after them, had a p99 of 71.548 µs.

The one invalid run is T-71k of round 3 (run 26): its send lag p99.9 was
10.363 µs against the bound of 10 µs, and loadgen marked it invalid for the
same reason. It met the rate, failure, cancel skip and engine stability
rules.

Before the matrix, an R1 run over the engine machine's own loopback was
valid, with a reply p50 of 40.7 µs, p99 of 55.3 µs and p99.9 of 97.0 µs.

## The two hosts

| | Engine | Client |
|---|---|---|
| CPU | AMD Ryzen Threadripper 7960X 24-Cores | AMD EPYC 9575F 64-Core |
| Kernel | 6.8.0-138-generic | 6.8.0-147-generic |
| Kernel boot ID | 9385754a-09a7-459a-8a1b-f7e4837d58b7 | 17447eb8-cbd5-4665-aa52-f84f6f0af5dd |
| CPUs allowed | 8 | 8 |
| Clock source | tsc | tsc |
| 1-minute load average before each run | 1.19 to 5.83 | 15.69 to 62.31 |

The load average is the machine's, from `/proc/loadavg`, as `runs.jsonl`
records it before each run. The engine machine's memory limit was
16,000,000,000 bytes. `env.txt` and `engine-env.txt` read only the cgroup v2
files, which were unreadable on these Pods; the runner takes the memory
limit from cgroup v2, then cgroup v1, then the machine's total memory
(`cmd/pairrun/host.go`).

## Method

`scripts/pair.sh supervise` ran on the engine Pod and `scripts/pair.sh
matrix` on the client Pod (D050). A round runs, in this order: F, R1,
R1-gogc-off, R1-spin-pin, R2 at 5,000/s, R3-10k, R3-71k at 71,429/s, T-71k
and REST-10k, each at 10,000/s unless named, with a 10 s warmup and 60 s
measured, each on a fresh engine with an empty data directory. A run is
valid if at least 99% of its rate was sent, nothing failed or was busy,
fewer than 0.1% of the cancels were skipped (R3, T and REST), send lag p99.9
was at most 10 µs (OE1 flows), loadgen found it valid, and no engine was
started during it. The plan has no override, so the results are publishable;
reading the clock cost 36.7 ns on the engine machine and 36.4 ns on the
client.

## Files

- `summary.txt`, `summary.json`: the runner's summary: G2, the tuning rule,
  and every configuration's rows with the floor.
- `g2.json`: the gate's conditions and the figures they used.
- `plan.json`: the matrix as run.
- `runs.jsonl`: one record per run: its arguments, start and end, each
  validity rule with its value and verdict, the readings of both machines
  before and after, and loadgen's figures.
- `runs/NN-CONFIG-rR.json`, `runs/NN-CONFIG-rR.txt`: loadgen's report and
  text table of run NN, configuration CONFIG, round R.
- `loopback.json`, `loopback.txt`: the R1 run over the engine machine's
  loopback.
- `env.txt`, `engine-env.txt`: the environment report of the client and of
  the engine machine.

The runner of commit cd0548b does not mask addresses, so these files were
masked after they were fetched from the client Pod, with `pairrun redact` of
commit cdb6a0f: every IPv4 address other than 0.0.0.0 and 127.0.0.1 is
written `x.x.x.x`.

## Limits

This is one pair at one time of day, and 3 of the 5 rounds ran. The commit
has no per-second series, no CPU counters, no wake meter and no engine
output (D051 added them), so the raised tail of round 3 cannot be placed
inside its runs or on either machine. The two hosts have different CPUs. G2
did not pass, so these runs are not a figure of the engine's latency between
two machines (D052).
