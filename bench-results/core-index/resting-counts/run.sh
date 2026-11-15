#!/usr/bin/env bash
# run.sh OUT, from the repository root, builds the book's test binary at
# ea8d5fe (map) and 06dfad0 (index) for each count of resting orders, with
# the nCOUNT.diff beside this script setting BenchmarkRestingOrders' count
# (1,000,000 as committed), and appends two runs of it to OUT/VARIANT.txt
# after a "resting: COUNT" line.
set -euo pipefail
OUT=$1
dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$OUT"
export GOTOOLCHAIN=go1.27.4
for v in map:ea8d5fe index:06dfad0; do
  for n in 850000 1000000 1200000; do
    rm -rf "$tmp/src" && mkdir "$tmp/src"
    git archive "${v#*:}" | tar -x -C "$tmp/src"
    if [ -f "$dir/n$n.diff" ]; then
      (cd "$tmp/src" && git apply "$dir/n$n.diff")
    fi
    (cd "$tmp/src" && go test -c -o "$tmp/book.test" ./internal/book)
    echo "resting: $n" >> "$OUT/${v%%:*}.txt"
    "$tmp/book.test" -test.run='^$' -test.bench='^BenchmarkRestingOrders$' -test.benchmem -test.count=2 >> "$OUT/${v%%:*}.txt"
  done
done
