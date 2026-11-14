#!/usr/bin/env bash
# run.sh OUT FROM TO VARIANT... runs rounds FROM to TO of the alternating
# benchmark: each round runs the test binary book.VARIANT.test in BIN
# (default .) once per variant, one variant after another, starting one
# variant later each round, and appends to OUT/VARIANT.txt and
# OUT/VARIANT-latency.txt. Rounds can be run in several calls.
# Build a binary with: go test -c -o book.VARIANT.test ./internal/book
set -euo pipefail
OUT=$1; FROM=$2; TO=$3; shift 3
BIN=${BIN:-.}
mkdir -p "$OUT"
vs=("$@")
for ((r = FROM; r <= TO; r++)); do
  for ((k = 0; k < ${#vs[@]}; k++)); do
    v=${vs[$(((k + r) % ${#vs[@]}))]}
    "$BIN/book.$v.test" -test.run='^$' -test.bench='^(BenchmarkApply|BenchmarkApplyDeep|BenchmarkRestingOrders)$' -test.benchmem -test.count=1 >> "$OUT/$v.txt"
    "$BIN/book.$v.test" -test.run='^$' -test.bench='^BenchmarkApplyLatency$' -test.count=1 >> "$OUT/$v-latency.txt"
  done
  echo "round $r done $(date -u +%FT%TZ)"
done
