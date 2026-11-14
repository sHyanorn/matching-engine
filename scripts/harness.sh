#!/usr/bin/env bash
# Runs the flash1-dev matching engine benchmark against the matching core
# (cmd/harness-adapter) and three reference engines on one machine.
#
#   scripts/harness.sh setup DIR            clone and build everything in DIR
#   scripts/harness.sh cases DIR OUT        conformance cases, into OUT
#   scripts/harness.sh perf DIR OUT FROM TO perf rounds FROM to TO, into OUT
#   scripts/harness.sh summary OUT          medians and worst cases of OUT
#
# setup clones the benchmark at a pinned commit and builds its harness
# with clang; Liquibook, CppTrader (with CppCommon) and geseq/orderbook
# with the benchmark's own build scripts, which pin their commits; and this
# repository's adapter. It needs git, clang, cmake, g++, Go, python3, the
# Boost headers and the {fmt} library, which CppTrader's CppCommon includes
# (libboost-dev and libfmt-dev on Ubuntu). It also writes the 34 cases of
# the benchmark's scripts/conformance_check.py as workload files.
#
# cases runs every case with every engine and writes each canonical report
# stream to DIR/canonical/ENGINE/CASE.txt, then runs the state audit, which
# compares the queried book with Liquibook's at every message, for every
# engine but Liquibook (its own audit needs QuantCup, which is not built).
# OUT/conformance.txt lists, per case, whether the references agree, whether
# this engine matches them, and each audit's verdict. The cases have no
# published hash, so the harness calls their runs INVALID; the canonical
# output and the audit verdict are what count. Last, it runs the audit on
# the five scenarios, into OUT/audit-ENGINE-SCENARIO.txt.
#
# perf runs each engine on each of the five scenarios once per round, at
# the canonical seed 23, starting each round one engine later, and keeps
# the harness output in OUT/runs/ENGINE/SCENARIO/ROUND.txt, and what the
# builds were made from in OUT/versions.txt. Rounds can be run in several
# calls. summary prints, per engine and scenario, the median
# throughput over the rounds, its range, how many runs' report streams had
# the published hash and how many the harness called VALID (it also gates
# a run on, among others, how long its audit queries took); then each
# engine's worst scenario; this engine against CppTrader round by round;
# and, per engine, the largest over the smallest run of each scenario.
set -euo pipefail

bench_url=https://github.com/flash1-dev/matching-engine-benchmark.git
bench_ref=424e4e5649652febb5b0c4c80859d74c4654131e
engines=(matching-engine liquibook cpptrader geseq)
scenarios=(static normal swing-25 swing-40 flash-crash)
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

usage() {
	sed -n '4,8p' "$0" >&2
	exit 2
}

setup() {
	local dir=$1
	mkdir -p "$dir"
	dir=$(cd "$dir" && pwd)
	if [ ! -d "$dir/bench/.git" ]; then
		git clone --quiet "$bench_url" "$dir/bench"
	fi
	git -C "$dir/bench" reset --hard --quiet "$bench_ref"
	cd "$dir/bench"
	CC=clang CXX=clang++ cmake -S . -B build -DCMAKE_BUILD_TYPE=Release > /dev/null
	cmake --build build -j > /dev/null
	bash scripts/build_baselines.sh liquibook
	bash additional_references/cpptrader_adapter/build.sh
	bash additional_references/geseq_adapter/build.sh
	(cd "$repo" && go build -tags harness -buildmode=c-shared -o "$dir/bench/matching-engine_adapter.so" ./cmd/harness-adapter)
	rm -f matching-engine_adapter.h
	# The cases as the benchmark defines them, one workload file each, and
	# their names and lengths in cases.txt.
	python3 -I - "$dir/bench" > cases.txt <<-'EOF'
		import importlib.util, os, sys
		bench = sys.argv[1]
		spec = importlib.util.spec_from_file_location("cc", os.path.join(bench, "scripts/conformance_check.py"))
		cc = importlib.util.module_from_spec(spec)
		spec.loader.exec_module(cc)
		for case, msgs in cc.EDGE_CASES.items():
		    scen = cc.scen_name(case)
		    cc.write_bin(os.path.join(bench, f"orders_{scen}_s0_n{len(msgs)}.bin"), msgs)
		    print(scen, len(msgs))
	EOF
	echo "setup done: $(wc -l < cases.txt) cases"
}

cases() {
	local dir=$1 out=$2 scen n e ref ours
	mkdir -p "$out"
	out=$(cd "$out" && pwd)
	dir=$(cd "$dir" && pwd)
	local can=$dir/canonical
	cd "$dir/bench"
	for e in "${engines[@]}"; do
		mkdir -p "$can/$e"
	done
	: > "$out/conformance.txt"
	while read -r scen n; do
		for e in "${engines[@]}"; do
			./harness --engine "./${e}_adapter.so" --scenario "$scen" --seed 0 --count "$n" \
				--mode perf --write-canonical-output "$can/$e/$scen.txt" > /dev/null || true
		done
		ref=differ
		if cmp -s "$can/liquibook/$scen.txt" "$can/cpptrader/$scen.txt" &&
			cmp -s "$can/liquibook/$scen.txt" "$can/geseq/$scen.txt"; then
			ref=agree
		fi
		ours=differs
		cmp -s "$can/liquibook/$scen.txt" "$can/matching-engine/$scen.txt" && ours=matches
		printf '%-50s references %-6s matching-engine %-8s' "$scen" "$ref" "$ours" >> "$out/conformance.txt"
		for e in matching-engine cpptrader geseq; do
			printf ' %s %s' "$e" "$(./harness --engine "./${e}_adapter.so" --scenario "$scen" --seed 0 --count "$n" \
				--mode audit | sed -n 's/^ *State audit: \([A-Z]*\).*/\1/p' || true)" >> "$out/conformance.txt"
		done
		echo >> "$out/conformance.txt"
	done < cases.txt
	for scen in "${scenarios[@]}"; do
		for e in matching-engine cpptrader geseq; do
			./harness --engine "./${e}_adapter.so" --scenario "$scen" --mode audit > "$out/audit-$e-$scen.txt" || true
		done
	done
	cat "$out/conformance.txt"
	grep -H -e 'Status:' -e 'State audit:' -e 'Verdict:' "$out"/audit-*.txt
}

perf() {
	local dir=$1 out=$2 from=$3 to=$4 r k s e
	mkdir -p "$out"
	out=$(cd "$out" && pwd)
	cd "$dir/bench"
	{
		echo "matching-engine: $(git -C "$repo" rev-parse --short HEAD)"
		echo "benchmark: $bench_ref"
		grep -h '^LIQUIBOOK_REF=' scripts/build_baselines.sh
		grep -h '^[A-Z_]*_REF=' additional_references/cpptrader_adapter/build.sh additional_references/geseq_adapter/build.sh
		echo "go: $(cd "$repo" && go version) for this repository's adapter, $(go version) for geseq's"
		echo "harness: $(clang --version | head -1); adapters: $(g++ --version | head -1)"
		dpkg-query -W -f='${Package} ${Version}\n' libboost-dev libfmt-dev
		echo "os: $(uname -srm)"
		echo "cpu: $(grep -m1 'model name' /proc/cpuinfo | cut -d: -f2- | sed 's/^ //'), $(nproc) logical CPUs"
	} > "$out/versions.txt"
	for ((r = from; r <= to; r++)); do
		for s in "${scenarios[@]}"; do
			for ((k = 0; k < ${#engines[@]}; k++)); do
				e=${engines[$(((k + r) % ${#engines[@]}))]}
				mkdir -p "$out/runs/$e/$s"
				./harness --engine "./${e}_adapter.so" --scenario "$s" --mode perf > "$out/runs/$e/$s/$r.txt" || true
			done
		done
		echo "round $r done $(date -u +%FT%TZ)"
	done
}

summary() {
	local out=$1
	python3 -I - "$out" <<-'EOF'
		import glob, os, re, statistics, sys
		out = sys.argv[1]
		scenarios = ["static", "normal", "swing-25", "swing-40", "flash-crash"]
		runs, worst = {}, {}
		print(f"{'engine':16} {'scenario':12} {'runs':>4} {'median M/s':>10} {'min':>6} {'max':>6}  hash  VALID")
		for e in sorted(os.listdir(os.path.join(out, "runs"))):
		    for s in scenarios:
		        files = glob.glob(os.path.join(out, "runs", e, s, "*.txt"))
		        if not files:
		            continue
		        mps, ok, valid = [], 0, 0
		        for f in files:
		            t = open(f).read()
		            mps.append(float(re.search(r"Throughput: +([0-9.]+) M msgs/s", t).group(1)))
		            ok += "Status: PASS" in t
		            valid += "Verdict: VALID" in t
		            n = int(re.search(r"Messages processed: +([0-9]+)", t).group(1))
		            secs = float(re.search(r"Wall time: +([0-9.]+) s", t).group(1))
		            runs[e, s, os.path.basename(f)] = n / secs
		        med = statistics.median(mps)
		        print(f"{e:16} {s:12} {len(mps):4} {med:10.2f} {min(mps):6.2f} {max(mps):6.2f}  {ok:2}/{len(mps)} {valid:2}/{len(mps)}")
		        if e not in worst or med < worst[e][1]:
		            worst[e] = (s, med)
		print()
		for e, (s, med) in sorted(worst.items(), key=lambda kv: -kv[1][1]):
		    print(f"worst case {e:16} {med:6.2f} M msgs/s ({s})")
		# The harness prints throughput to 0.01M msgs/s, too coarse for a
		# slow engine, so the ratios below divide messages by wall time.
		print()
		print("matching-engine against cpptrader, run by run (same round, same scenario):")
		for s in scenarios:
		    rounds = [r for (e, sc, r) in runs if e == "matching-engine" and sc == s and ("cpptrader", s, r) in runs]
		    if not rounds:
		        continue
		    ratio = [runs["matching-engine", s, r] / runs["cpptrader", s, r] for r in rounds]
		    faster = sum(x > 1 for x in ratio)
		    print(f"  {s:12} faster in {faster}/{len(ratio)} rounds; ratio {min(ratio):.2f} to {max(ratio):.2f}, median {statistics.median(ratio):.2f}")
		print("largest over smallest of the runs of one scenario:")
		for e in ["matching-engine", "cpptrader", "liquibook", "geseq"]:
		    spread = []
		    for s in scenarios:
		        x = [v for (en, sc, _), v in runs.items() if en == e and sc == s]
		        if x:
		            spread.append(max(x) / min(x))
		    if spread:
		        print(f"  {e:16} {min(spread):.2f} to {max(spread):.2f}")
	EOF
}

case ${1:-} in
setup) [ $# -eq 2 ] || usage; setup "$2" ;;
cases) [ $# -eq 3 ] || usage; cases "$2" "$3" ;;
perf) [ $# -eq 5 ] || usage; perf "$2" "$3" "$4" "$5" ;;
summary) [ $# -eq 2 ] || usage; summary "$2" ;;
*) usage ;;
esac
