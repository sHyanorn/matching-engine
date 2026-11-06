#!/usr/bin/env bash
# Measures end-to-end latency as docs/BENCHMARKS.md describes. For each
# rate it starts a fresh engine on an empty data directory, offers the
# order flow with loadgen, and stops the engine. The sweep stops at the
# first rate whose run is invalid.
#
#   scripts/e2e.sh OUT [RATE...]
#
# The default rates are 10000 15000 20000 25000 30000 40000 50000. OUT gets
# env.txt and, per run, loadgen's report (rate-R-N.json and .txt) and the
# engine's /metrics at the end (rate-R-N.metrics.txt). Settings: REPEATS
# (runs per rate, default 3), WARMUP (10s), DURATION (30s), PORT (18080),
# ENGINE_FLAGS, LOADGEN_FLAGS.
set -euo pipefail

out=${1:?usage: scripts/e2e.sh OUT [RATE...]}
shift
[ $# -gt 0 ] || set -- 10000 15000 20000 25000 30000 40000 50000
repeats=${REPEATS:-3} warmup=${WARMUP:-10s} duration=${DURATION:-30s} port=${PORT:-18080}
addr=127.0.0.1:$port

mkdir -p "$out"
bin=$(mktemp -d)
pid=
trap 'rm -rf "$bin"; [ -z "$pid" ] || kill "$pid" 2>/dev/null || true' EXIT
go build -o "$bin/engine" ./cmd/engine
go build -o "$bin/loadgen" ./cmd/loadgen

data=$(mktemp -d)
{
	echo "commit: $(git rev-parse --short HEAD)$([ -z "$(git status --porcelain -- . ':!bench-results')" ] || echo ' (modified)')"
	echo "go: $(go version)"
	echo "os: $(uname -srm)"
	if [ -r /proc/cpuinfo ]; then
		echo "cpu: $(grep -m1 'model name' /proc/cpuinfo | cut -d: -f2- | sed 's/^ //'), $(nproc) logical CPUs"
	else
		echo "cpu: $(sysctl -n machdep.cpu.brand_string 2>/dev/null), $(sysctl -n hw.ncpu 2>/dev/null) logical CPUs"
	fi
	echo "data directory filesystem: $(df -PT "$data" 2>/dev/null | awk 'NR==2 {print $2}')"
	echo "engine flags: -symbols BTC-USD ${ENGINE_FLAGS:-}"
	echo "loadgen flags: -warmup $warmup -duration $duration ${LOADGEN_FLAGS:-}"
} > "$out/env.txt"
rm -rf "$data"

valid=0
for rate in "$@"; do
	failed=0
	for n in $(seq "$repeats"); do
		data=$(mktemp -d)
		# shellcheck disable=SC2086 # the flags are meant to split
		"$bin/engine" -addr "$addr" -data "$data" -symbols BTC-USD ${ENGINE_FLAGS:-} 2> "$out/rate-$rate-$n.engine.log" > /dev/null &
		pid=$!
		for _ in $(seq 100); do
			curl -fs "http://$addr/healthz" > /dev/null && break
			sleep 0.1
		done
		echo "== $rate/s, run $n"
		# shellcheck disable=SC2086
		"$bin/loadgen" -addr "$addr" -rate "$rate" -warmup "$warmup" -duration "$duration" \
			-out "$out/rate-$rate-$n.json" ${LOADGEN_FLAGS:-} | tee "$out/rate-$rate-$n.txt" && valid=$((valid + 1)) || failed=1
		curl -fs "http://$addr/metrics" > "$out/rate-$rate-$n.metrics.txt" || true
		kill -TERM "$pid"
		wait "$pid" || true
		pid=
		rm -rf "$data"
		[ "$failed" = 0 ] || break 2
	done
done
[ "$valid" -gt 0 ] || { echo "e2e: no valid run" >&2; exit 1; }
