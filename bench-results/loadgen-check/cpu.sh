#!/usr/bin/env bash
# Usage: cpu.sh OUTDIR RATE RUNS. Alternates loadgen-before and -after
# against a fresh engine each run and records loadgen's CPU time.
set -euo pipefail
dir=$(dirname "$0"); out=$1 rate=$2 runs=$3
mkdir -p "$out"
addr=127.0.0.1:18080
for n in $(seq "$runs"); do
  for v in before after; do
    data=$(mktemp -d)
    "$dir/engine" -addr $addr -data "$data" -symbols BTC-USD 2>/dev/null >/dev/null &
    pid=$!
    for _ in $(seq 100); do curl -fs http://$addr/healthz >/dev/null && break; sleep 0.1; done
    TIMEFORMAT='%U %S'
    { time "$dir/loadgen-$v" -addr $addr -rate "$rate" -out "$out/$v-$n.json" > "$out/$v-$n.txt" 2>&1 || true; } 2> "$out/$v-$n.cpu"
    kill -TERM $pid; wait $pid || true; rm -rf "$data"
    echo "$v $n: cpu $(cat "$out/$v-$n.cpu") $(tail -1 "$out/$v-$n.txt")"
  done
done
