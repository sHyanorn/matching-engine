# Second pilot between two rented Pods

The pilot matrix of D050, with the diagnostics of D051, on one pair of
rented Pods (D043), 8 vCPUs each, in EU-RO-1, at commit
cdb6a0f0ef84b653be7481996466ec566b97991e, from 2026-11-23 23:40:57 to
2026-11-24 00:02:19 UTC. The gate G2 stopped it after 2 of its 5 rounds, on
condition 1: 8 of the 18 runs were invalid, both F runs among them, so the
pair has no floor. Every invalid run failed the send lag rule, with a p99.9
of 13.8 to 40.5 µs against the bound of 10 µs. The engine machine's own
loopback run, before any run between the machines, already had a reply p99
of 2,587.3 µs. The CPU counters show the engine's threads waiting for a CPU
and CPU pressure on both machines, in valid and invalid runs alike; the
files do not show what raised the send lag.

## Results

G2 (`g2.json`):

| Condition | Status | Figures |
|---|---|---|
| 1. At least 4 of 5 runs valid in every configuration | fail | F 0 of 2, R3-71k 0 of 2 and T-71k 0 of 2: with 2 invalid runs none can reach 4 of 5 |
| 2. R1 reply p99, largest over smallest, at most 2 | open | 78.946 and 86.142 µs, 1.09, over 2 valid runs |
| 3. R1 median reply p50 at most 100 µs over the floor's | fail | R1's median p50 is 60.3 µs; with no valid F run there is no floor, and the gate counts a missing side as a fail once the matrix stops |

The matrix stops after a round once F can no longer reach 4 valid runs
(D050), so rounds 3 to 5 and the 15-minute R1 run did not run. No variant of
R1 was judged: R1 had 2 valid runs, R1-gogc-off 1 and R1-spin-pin 2, and the
rule wants 4 of each.

Reply row, in µs, minimum/median/maximum over the valid runs
(`summary.txt`), with no floor:

| Configuration | Valid | p50 | p99 | p99.9 |
|---|---|---|---|---|
| F | 0 of 2 | | | |
| R1 | 2 of 2 | 58.2/60.3/62.4 | 78.9/82.5/86.1 | 769.5/2,735.5/4,701.6 |
| R1-gogc-off | 1 of 2 | 63.0 | 83.9 | 2,372.4 |
| R1-spin-pin | 2 of 2 | 57.7/58.1/58.4 | 73.6/3,707.3/7,341.1 | 1,075.3/20,959.8/40,844.4 |
| R2 | 2 of 2 | 55.6/59.3/63.0 | 69.9/78.6/87.2 | 1,942.5/3,787.3/5,632.0 |
| R3-10k | 1 of 2 | 62.4 | 3,228.9 | 27,661.2 |
| R3-71k | 0 of 2 | | | |
| T-71k | 0 of 2 | | | |
| REST-10k | 2 of 2 | 163.6/167.4/171.1 | 968.9/8,474.0/15,979.1 | 6,684.6/45,458.7/84,232.9 |

The median of two runs is their mean, so the medians of R1, R1-spin-pin, R2
and REST-10k may be the figure of no run. The validity rules do not bound
latency, so a valid run can have a tail of milliseconds: the second
R1-spin-pin run had a reply p99 of 7,341.1 µs.

The R1 run over the engine machine's own loopback, whose measured part began
at 23:39:56 UTC, was valid, with a reply p50 of 25.8 µs, p99 of 2,587.3 µs
and p99.9 of 22,077.9 µs. In the first pilot the same run had a p99 of 55.3
µs, on an engine machine with another CPU (an AMD Ryzen Threadripper 7960X;
here an AMD EPYC 9575F).

### Which rule the invalid runs failed

Every invalid run failed the send lag rule, and loadgen marked each invalid
for the same reason. Send lag is the time from a command's due time to the
moment loadgen on the client machine is about to write it
(`cmd/loadgen/oe.go`). Run 17 (T-71k) also failed loadgen's push checks:
536,522 orders that traded got no trade push, and the trade stream was
closed as a slow consumer. Every run met the rate, failure, cancel skip and
engine stability rules.

### What the diagnostics of D051 show

For each invalid run, the table gives: the send lag p99.9; the second of the
measured part with the largest reply p99 (pong for F), as seconds after the
measured part began, against the median second's p99; the largest lateness
of the wake meter in that second; the run-queue delay of the engine's
threads as a share of their run time; and the CPU pressure of each machine,
the time in which some task waited for a CPU. The wake meter is a thread on
the engine machine that sleeps 1 ms in a loop and records how late each wake
is; outside the spin-pin configuration it may share a CPU with the engine's
threads, and the worst second may overlap two of its seconds (D051). The
counters cover the whole run, warmup included.

| Run | Configuration | Send lag p99.9, µs | Worst second, p99 in µs (median second) | Wake max, ms | Engine run-queue delay | CPU pressure, s: engine, client |
|---|---|---|---|---|---|---|
| 01 | F, round 1 | 13.8 | +33 s: 10,000 (69) | 1.2 | 6.5% | 17.32, 0.57 |
| 03 | R1-gogc-off, round 1 | 14.3 | +6 s: 1,700 (72) | 2.9 | 4.8% | 1.30, 1.96 |
| 06 | R3-10k, round 1 | 15.0 | +48 s: 77,000 (80) | 0.4 | 3.6% | 9.23, 0.72 |
| 07 | R3-71k, round 1 | 37.5 | +3 s: 4,000 (130) | 0.2 | 14.1% | 19.09, 1.83 |
| 08 | T-71k, round 1 | 39.5 | +21 s: 22,000 (165) | 0.2 | 31.3% | 18.03, 3.21 |
| 10 | F, round 2 | 19.8 | +48 s: 140 (63) | 1.5 | 16.5% | 13.45, 1.23 |
| 16 | R3-71k, round 2 | 37.3 | +53 s: 78,000 (135) | 0.1 | 12.5% | 12.08, 2.10 |
| 17 | T-71k, round 2 | 40.5 | +26 s: 81,000 (285) | 2.7 | 25.0% | 12.86, 2.56 |

Over all 18 runs:

- The engine's threads waited on a run queue for 3.6% to 31.3% of their run
  time in the invalid runs. In the valid runs the share was 0.5% in the two
  spin-pin runs (04 and 13) and 3.4% (run 15) to 20.8% in the others. The
  share does not compare across configurations, as the engine's threads ran
  for 10.4 to 19.5 s in most runs, 39.7 and 40.0 s in the REST runs, 86.1 to
  97.4 s in the runs at 71,429/s and 141.5 and 143.8 s in the spin-pin runs.
- CPU pressure was read from `/proc/pressure/cpu`, which counts every task
  on the machine, the Pod's own included; the files do not show whose tasks
  waited. It was 1.30 to 19.09 s per run on the engine machine and 0.49 to
  3.21 s on the client.
- Neither Pod had a CPU quota (`cpu_quota_us` -1, read from cgroup v1), so
  no period was counted and none was throttled.
- Steal was 0 on both machines in every run.

The ranges of the valid and the invalid runs overlap, and their medians
differ: the run-queue share is 13.3% in the invalid runs against 5.4% in the
valid ones, and the engine machine's pressure 13.16 s against 9.13 s. All
four runs at 71,429/s were invalid.

## The two hosts

| | Engine | Client |
|---|---|---|
| CPU | AMD EPYC 9575F 64-Core | AMD Ryzen Threadripper 7960X 24-Cores |
| Kernel | 6.8.0-147-generic | 6.8.0-138-generic |
| Kernel boot ID | 6efc30d2-9790-4f4b-8094-2e0c550f42f6 | 585c718f-07e1-4141-8981-6335b61ccfbd |
| CPUs allowed | 8 | 8 |
| Clock source | tsc | tsc |
| 1-minute load average before each run | 24.93 to 61.99 | 3.44 to 6.35 |

The two CPU models are those of the first pilot, with the roles swapped. The
load average is the machine's, from `/proc/loadavg`, as `runs.jsonl` records
it before each run. The engine machine's memory limit was 16,000,000,000
bytes. `env.txt` and `engine-env.txt` read only the cgroup v2 files, which
were unreadable on these Pods; the runner read the CPU quota from cgroup v1
(`cpu_limit_source` in `runs.jsonl`) and takes the memory limit from cgroup
v2, then cgroup v1, then the machine's total memory (`cmd/pairrun/host.go`).

## Method

As in [the first pilot](../pair-pilot-1/README.md): `scripts/pair.sh
supervise` on the engine Pod and `scripts/pair.sh matrix` on the client Pod,
the same nine configurations per round, 10 s warmup and 60 s measured, a
fresh engine for each run, and the validity rules of D050. The plan has no
override, so the results are publishable; reading the clock cost 37.2 ns on
the engine machine and 36.5 ns on the client. Each counter that D051 records
for a machine was readable on that machine.

## Files

The files of the first pilot (see [its README](../pair-pilot-1/README.md)),
and for each run whose engine started:

- `runs/NN-CONFIG-rR.engine.log`: the engine's output during the run.
- `runs/NN-CONFIG-rR.wake.json`: the wake meter's seconds around the
  measured part, with the clock offset between the machines. The REST runs
  have none, as they have no series.

`runs.jsonl` and `summary.json` also hold the counters and the worst second
of each run. `scripts/pair.sh` on the client Pod masked the files before it
served them: every IPv4 address other than 0.0.0.0 and 127.0.0.1 is written
`x.x.x.x`.

## Limits

This is one pair at one time of day, and 2 of the 5 rounds ran. With no
valid F run, no latency here can be set against a floor. The counters cover
each whole run and cannot be placed in time inside it; without a CPU quota
the throttling counters cannot rise; a steal of 0 does not show that the
host took nothing; and the wake meter sees one CPU of the engine machine,
nothing of the client, where send lag is measured. G2 did not pass, so these
runs are not a figure of the engine's latency between two machines (D052).
