#!/usr/bin/env bash
# run.sh OUT ROUNDS VARIANT... alternates the test binaries book.VARIANT.test
# in BIN (default .), one benchmark run each per round, starting each round
# one variant later, and appends to OUT/VARIANT.txt and OUT/VARIANT-latency.txt.
# Build a binary with: go test -c -o book.VARIANT.test ./internal/book
set -euo pipefail
OUT=$1; ROUNDS=$2; shift 2
BIN=${BIN:-.}
mkdir -p "$OUT"
vs=("$@")
for ((r = 1; r <= ROUNDS; r++)); do
  for ((k = 0; k < ${#vs[@]}; k++)); do
    v=${vs[$(((k + r) % ${#vs[@]}))]}
    "$BIN/book.$v.test" -test.run='^$' -test.bench='^(BenchmarkApply|BenchmarkApplyDeep|BenchmarkRestingOrders)$' -test.benchmem -test.count=1 >> "$OUT/$v.txt"
    "$BIN/book.$v.test" -test.run='^$' -test.bench='^BenchmarkApplyLatency$' -test.count=1 >> "$OUT/$v-latency.txt"
  done
done
