#!/usr/bin/env bash
# run.sh OUT FROM TO PATTERN VARIANT... runs rounds FROM to TO of the
# alternating benchmark: each round runs the test binary engine.VARIANT.test
# in BIN (default .) once per variant, one variant after another, starting
# one variant later each round, and appends to OUT/VARIANT.txt. Rounds can
# be run in several calls. Build a binary with:
#   go test -c -o engine.VARIANT.test ./internal/engine
# or ./internal/oe, with oe.VARIANT.test and PREFIX=oe.
set -euo pipefail
OUT=$1; FROM=$2; TO=$3; PATTERN=$4; shift 4
BIN=${BIN:-.}
PREFIX=${PREFIX:-engine.}
mkdir -p "$OUT"
vs=("$@")
for ((r = FROM; r <= TO; r++)); do
  for ((k = 0; k < ${#vs[@]}; k++)); do
    v=${vs[$(((k + r) % ${#vs[@]}))]}
    "$BIN/$PREFIX$v.test" -test.run='^$' -test.bench="$PATTERN" -test.benchtime="${BENCHTIME:-300000x}" -test.benchmem -test.count=1 >> "$OUT/$v.txt"
  done
  echo "round $r done $(date -u +%FT%TZ)"
done
