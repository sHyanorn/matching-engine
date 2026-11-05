"""Draws the docs/BENCHMARKS.md charts from the reports of scripts/e2e.sh.

    python3 scripts/plot.py percentiles OUT.svg RATE LABEL=DIR ...
    python3 scripts/plot.py sweep OUT.svg DIR

percentiles plots the order-to-trade-push latency at RATE against its
percentile, on HdrHistogram's 1/(1-p) axis, one line per DIR. sweep plots
p50, p99 and p99.9 of the same latency against the target rate. A rate's
valid run with the median p99 stands for it. Requires matplotlib.
"""

import glob
import json
import os
import re
import statistics
import sys

import matplotlib

matplotlib.use("svg")
import matplotlib.pyplot as plt  # noqa: E402
from matplotlib.ticker import FuncFormatter, NullLocator  # noqa: E402

# Validated categorical slots 1-3 (light surface), and neutral ink.
SERIES = ["#2a78d6", "#eb6834", "#1baf7a"]
SURFACE, INK, INK_2, GRID = "#fcfcfb", "#0b0b0b", "#52514e", "#e4e3df"
LATENCY = "trade_push"

plt.rcParams.update({
    "font.family": ["Inter", "DejaVu Sans", "sans-serif"],
    "font.size": 10,
    "svg.fonttype": "none",
    "figure.facecolor": SURFACE,
    "axes.facecolor": SURFACE,
    "axes.edgecolor": GRID,
    "axes.labelcolor": INK_2,
    "axes.linewidth": 0.8,
    "axes.grid": True,
    "grid.color": GRID,
    "grid.linewidth": 0.8,
    "xtick.color": INK_2,
    "ytick.color": INK_2,
    "legend.frameon": False,
    "legend.labelcolor": INK,
})


def load(path):
    with open(path) as f:
        return json.load(f)


def millis(us):
    return us / 1000


def median_runs(directory):
    """The valid run with the median p99 at each rate in directory."""
    runs = {}
    for path in glob.glob(os.path.join(directory, "rate-*-*.json")):
        report = load(path)
        if report["valid"]:
            rate = int(re.search(r"rate-(\d+)-\d+\.json$", path).group(1))
            runs.setdefault(rate, []).append(report)
    median = {}
    for rate, reports in runs.items():
        reports.sort(key=lambda r: r["latency_us"][LATENCY]["p99"])
        median[rate] = reports[len(reports) // 2]
    return median


def finish(ax, fig, out, title):
    ax.set_title(title, loc="left", color=INK, fontsize=11, pad=12)
    for side in ("top", "right"):
        ax.spines[side].set_visible(False)
    ax.tick_params(length=0)
    fig.tight_layout()
    fig.savefig(out, metadata={"Date": None})


def percentiles(out, rate, specs):
    fig, ax = plt.subplots(figsize=(7.2, 4))
    top = 1
    for i, spec in enumerate(specs):
        label, directory = spec.split("=", 1)
        report = median_runs(directory)[rate]
        curve = [(p, v) for p, v in report["latency_us"][LATENCY]["curve"] if p < 100]
        xs = [1 / (1 - p / 100) for p, _ in curve]
        ys = [millis(v) for _, v in curve]
        top = max(top, xs[-1])
        ax.plot(xs, ys, color=SERIES[i], linewidth=2, solid_capstyle="round", label=label)
        ax.annotate(f"{ys[-1]:.1f} ms", (xs[-1], ys[-1]), xytext=(4, 0), textcoords="offset points",
                    va="center", color=INK_2, fontsize=9)
    ax.set_xscale("log")
    ticks = [t for t in (2, 10, 100, 1000, 10_000, 100_000) if t <= top]
    ax.set_xticks(ticks)
    ax.xaxis.set_major_formatter(FuncFormatter(lambda x, _: f"{100 - 100 / x:g}%"))
    ax.xaxis.set_minor_locator(NullLocator())
    ax.set_xlim(1, top * 1.6)
    ax.set_ylim(bottom=0)
    ax.set_xlabel("percentile")
    ax.set_ylabel("order to trade push (ms)")
    if len(specs) > 1:
        ax.legend(loc="upper left")
    finish(ax, fig, out, f"Latency by percentile at {rate // 1000}k requests/s")


def sweep(out, directory):
    runs = median_runs(directory)
    rates = sorted(runs)
    fig, ax = plt.subplots(figsize=(7.2, 4))
    for i, (key, name) in enumerate((("p50", "p50"), ("p99", "p99"), ("p99_9", "p99.9"))):
        ys = [millis(runs[rate]["latency_us"][LATENCY][key]) for rate in rates]
        ax.plot(rates, ys, color=SERIES[i], linewidth=2, marker="o", markersize=6,
                markeredgecolor=SURFACE, markeredgewidth=2, label=name)
        ax.annotate(f"{name} {ys[-1]:.1f} ms", (rates[-1], ys[-1]), xytext=(6, 0), textcoords="offset points",
                    va="center", color=INK_2, fontsize=9)
    ax.set_xticks(rates)
    ax.xaxis.set_major_formatter(FuncFormatter(lambda x, _: f"{x / 1000:g}k"))
    ax.set_xlim(rates[0] * 0.8, rates[-1] * 1.25)
    ax.set_ylim(bottom=0)
    ax.set_xlabel("target rate (requests per second)")
    ax.set_ylabel("order to trade push (ms)")
    ax.legend(loc="upper left")
    finish(ax, fig, out, "Latency by load")


if __name__ == "__main__":
    if len(sys.argv) < 4 or sys.argv[1] not in ("percentiles", "sweep"):
        sys.exit(__doc__)
    if sys.argv[1] == "percentiles":
        percentiles(sys.argv[2], int(sys.argv[3]), sys.argv[4:])
    else:
        sweep(sys.argv[2], sys.argv[3])
