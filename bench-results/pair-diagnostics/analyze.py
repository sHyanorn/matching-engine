#!/usr/bin/env python3
"""Tables and verdict of the wake-meter A/B (see README.md).

Usage: analyze.py [DIR]    DIR defaults to the directory of this script.

Reads DIR/runs/NN-with.json and DIR/runs/NN-without.json. Each holds, from
loadgen's report of the run, valid, problems, achieved_rate and the percentile
fields of the latency_us rows reply, hiccup and send_lag; the exit code of
loadgen; and, for a run with the meter, its rows (n, p99, max, placement)
under "meter".
"""
import glob, json, os, statistics, sys

DIR = sys.argv[1] if len(sys.argv) > 1 else os.path.dirname(os.path.abspath(__file__))
rows = []
for path in sorted(glob.glob(os.path.join(DIR, "runs", "[0-9][0-9]-*.json"))):
    num, mode = os.path.basename(path)[:-len(".json")].split("-")
    try:
        r = json.load(open(path))
        reply = r["latency_us"]["reply"]
    except Exception as e:
        rows.append(dict(run=int(num), mode=mode, error=str(e)))
        continue
    meter = None
    m = r.get("meter")
    if m:
        ns = [n for n in m["n"] if n]
        p99s = sorted(p for n, p in zip(m["n"], m["p99"]) if n)
        meter = dict(seconds=len(m["n"]), wakes=sum(m["n"]), per_s=sum(m["n"]) / max(1, len(ns)),
                     p99_median=p99s[len(p99s) // 2] if p99s else None, max=max(m["max"]) if m["max"] else None,
                     pinned=m["placement"]["pinned"], cpu=m["placement"]["cpu"])
    rows.append(dict(run=int(num), mode=mode, valid=r.get("valid"), problems=r.get("problems"),
                     p50=reply["p50"], p99=reply["p99"], p999=reply["p99_9"], p9999=reply["p99_99"], max=reply["max"],
                     hiccup_p99=r["latency_us"].get("hiccup", {}).get("p99"), send_lag_p999=r["latency_us"].get("send_lag", {}).get("p99_9"),
                     achieved=r.get("achieved_rate"), exit=r.get("exit"), meter=meter))

print("reply row of each run, in us (loadgen's report, 10000/s, 60 s after a 10 s warmup)")
print("%-4s %-8s %-5s %10s %10s %10s %10s %12s %s" % ("run", "meter", "valid", "p50", "p99", "p99.9", "p99.99", "max", "loadgen hiccup p99 / send lag p99.9 / achieved rate"))
for r in rows:
    if "error" in r:
        print(r["run"], r["mode"], "ERROR", r["error"]); continue
    print("%-4d %-8s %-5s %10.1f %10.1f %10.1f %10.1f %12.1f %.1f / %.1f / %.1f" % (r["run"], r["mode"], r["valid"], r["p50"], r["p99"], r["p999"], r["p9999"], r["max"], r["hiccup_p99"] or 0, r["send_lag_p999"] or 0, r["achieved"] or 0))

def med(v):
    return statistics.median(v)

ok = [r for r in rows if "error" not in r]
groups = {g: [r for r in ok if r["mode"] == g] for g in ("without", "with")}
print()
print("groups (all runs, valid or not): n without %d, n with %d" % (len(groups["without"]), len(groups["with"])))
verdict_drop = False
print("%-6s | %-34s | %-34s | %-10s | %s" % ("stat", "without: median (spread = max-min)", "with: median (spread)", "|diff|", "drop?"))
for key, label in (("p50", "p50"), ("p99", "p99"), ("p999", "p99.9")):
    a = [r[key] for r in groups["without"]]
    b = [r[key] for r in groups["with"]]
    if not a or not b:
        continue
    ma, mb = med(a), med(b)
    sa, sb = max(a) - min(a), max(b) - min(b)
    diff = abs(mb - ma)
    drop = diff > sa
    verdict_drop |= drop
    print("%-6s | %12.1f (%10.1f)             | %12.1f (%10.1f)             | %10.1f | %s (diff %s without's spread %.1f)" % (
        label, ma, sa, mb, sb, diff, "DROP" if drop else "keep", ">" if drop else "<=", sa))
print()
print("verdict by the rule (drop if, for any of p50, p99, p99.9, the difference of the two medians exceeds the without-meter group's spread):",
      "DROP THE METER" if verdict_drop else "KEEP THE METER (no statistic's difference exceeds the without group's spread)")
print()
print("what the meter saw in the runs with it (its rows over the whole run, warmup and the seconds around it included):")
for r in groups["with"]:
    m = r["meter"]
    print("run %d: %s" % (r["run"], "no meter rows" if not m else "%d seconds, %d wakes, %.0f wakes/s, median of per-second p99 %.0f us, worst max %.0f us, pinned %s on cpu %s" % (
        m["seconds"], m["wakes"], m["per_s"], m["p99_median"] or 0, m["max"] or 0, m["pinned"], m["cpu"])))
