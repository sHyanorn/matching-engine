#!/usr/bin/env bash
# Measures the round trip of OE1 Pings between two rented Pods, the floor
# of docs/TASKS.md M8: the engine's OE1 listener on one, loadgen -oe on
# the other.
#
#   scripts/pair.sh boot       print the start command of a Pod
#   scripts/pair.sh engine     serve OE1 on 0.0.0.0:9000
#   scripts/pair.sh client     measure against ENGINE_ADDR, publish the results
#   scripts/pair.sh supervise  run the engines of the pilot matrix on request
#   scripts/pair.sh matrix     run the pilot matrix against a supervisor, publish the results
#   scripts/pair.sh selftest   print a line every minute
#
# A Pod runs `bash -c` with the text that boot prints, in the golang image
# the Dockerfile pins, with MODE, COMMIT, TREE and DEADLINE in its
# environment. It first starts a timer that deletes the Pod at DEADLINE,
# in Unix seconds, through Runpod's API with the Pod's own key, and
# retries every 30 s until the API answers 204. A DEADLINE that is not a
# whole number deletes the Pod at once; a restarted container keeps the
# same DEADLINE. It then fetches the tarball of COMMIT, refuses it unless
# git get-tar-commit-id names COMMIT and its files hash to the tree TREE,
# and runs this script from it in mode MODE with RUNPOD_API_KEY removed
# from its environment, so that no process this script starts holds the
# key. The start shell does, as do the timer and the curl, git and tar that
# the start shell runs before this script. The start command does not end
# on its own, so a Pod keeps its log until it is deleted.
#
# engine and client build engine and loadgen with the image's Go, as the
# Dockerfile does. client waits until ENGINE_ADDR (host:port) accepts a
# connection, then makes RUNS loadgen -oe runs (5) at RATE (10000) with
# WARMUP (10s) and DURATION (60s), each into run-N.json and run-N.txt,
# and writes env.txt. It prints each run's table to the log and publishes
# the results. env.txt holds the CPU, the clock source and the cost of
# reading it, whether SO_BUSY_POLL can be set, the kernel's boot ID (the
# same on two Pods only if they share a host), and three variables of the
# Pod: RUNPOD_POD_ID, RUNPOD_DC_ID and RUNPOD_CPU_COUNT. It is published
# and the supervisor serves its copy unauthenticated, so no other variable
# goes in.
#
# To publish, client and matrix first copy the results with their network
# addresses masked (pairrun redact into pub/results): every IPv4 address but
# 0.0.0.0 and 127.0.0.1, with its port, becomes x.x.x.x, since on a rented Pod
# the client reaches the engine at a public address, which the results quote.
# The results on the Pod stay as they are. If pairrun redact fails, no archive
# is printed or served. Both archives are made from the masked copy. The log
# returns at most its last 5000 lines, so the log copy (results-log.tgz, made
# by pairrun slim) leaves out what the full archive still has and the log can
# do without: the percentile curves of the loadgen reports and loadgen's text
# tables. It adds log-copy.txt, which says so. The log gets, in this order,
#
#   pair: results.tgz N bytes, sha256 H          the full archive
#   pair: results-log.tgz M bytes, sha256 H2     the log copy
#   -- begin results-log.tgz base64
#   ...                                          the log copy, 76 characters a line
#   -- end results-log.tgz base64
#   pair: results-log.tgz M bytes, sha256 H2     the same line again
#
# so that a log cut at either end still shows the log copy's size and hash.
# The log copy is fetched from the log: the lines between the begin and end
# lines, decoded with base64 -d and checked against H2. The full archive,
# which holds every file of the masked copy, is fetched over HTTP from
# /RESULT_PATH/results.tgz on port 8000, which the Pod serves from then on,
# and checked against H. Both paths hold the masked results; only the full
# archive has the curves and the tables. If pairrun slim fails, a line says so
# and the log copy is a copy of the full archive: the results are printed all
# the same.
#
# supervise and matrix are the pilot (cmd/pairrun). supervise serves the
# control port 9100 and, with the engine's OE1 port 9000 and HTTP port
# 8080, is what the client reaches: it runs one loopback R1 run, then
# starts a fresh engine for each run the client asks for. It prints what
# env.txt holds, for its own machine, to the log and writes it to
# engine-env.txt in its work directory, which it serves to the client
# (-env). matrix, on the other machine, runs the plan against ENGINE_OE,
# ENGINE_HTTP and ENGINE_CTL (host:port of the three), with the Pod's own
# DEADLINE as the end of its time, and publishes its results as client
# does, with what the client fetched from the supervisor as engine-env.txt
# next to env.txt. The results also hold the output of the engine of each
# run as runs/*.engine.log, bounded in size, with its IPv4 addresses but
# 0.0.0.0 and 127.0.0.1 replaced by x.x.x.x, and each run's record in
# runs.jsonl has the change in the CPU scheduling counters of both machines
# over the run (engine_delta, client_delta). Once it is ready, the supervisor
# also times how late a thread that sleeps 1 ms is woken on its machine; the
# results hold those seconds for the measured part of each run that has a
# loadgen series, as runs/*.wake.json, and the run's record the offset between
# the two clocks (clock_offset). matrix passes DEADLINE_MARGIN
# with DEADLINE: no run starts unless it would end that long before
# DEADLINE. It is a Go duration ("600s", not "600") and 10m when unset
# (pairrun's -deadline-margin); a value that is not one ends the client at
# flag parsing, before any run. PILOT_* variables override the plan's rates
# and durations for a rehearsal (cmd/pairrun).
set -euo pipefail

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

boot() {
	cat <<-'EOF'
		( (sleep "$((DEADLINE - $(date +%s)))")
		until [ "$(printf 'Authorization: Bearer %s\n' "$RUNPOD_API_KEY" |
		    curl -fsS -m 30 -o /dev/null -w '%{http_code}' -X DELETE -H @- "https://api.runpod.io/v2/pods/$RUNPOD_POD_ID")" = 204 ]; do
		    sleep 30
		done) &
		echo "pair: $MODE; the Pod deletes itself at $(date -u -d "@$DEADLINE" +%FT%TZ)"
		cd "$(mktemp -d)" &&
		    curl -fsS -o src.tgz "https://codeload.github.com/sHyanorn/matching-engine/tar.gz/$COMMIT" &&
		    got=$(gzip -dc src.tgz | git get-tar-commit-id) && echo "pair: tarball of commit $got" &&
		    [ "$got" = "$COMMIT" ] &&
		    mkdir src && tar -xzf src.tgz -C src --strip-components=1 &&
		    got=$(git init -q --bare tree && git --git-dir=tree --work-tree=src add -Af && git --git-dir=tree write-tree) &&
		    echo "pair: tree $got" && [ "$got" = "$TREE" ] &&
		    env -u RUNPOD_API_KEY bash src/scripts/pair.sh "$MODE"
		echo "pair: stopped with status $?"
		wait
	EOF
}

build() {
	cd "$repo"
	CGO_ENABLED=0 GOTOOLCHAIN=local go build -trimpath -o bin/ ./cmd/engine ./cmd/loadgen ./cmd/pairrun
	GOTOOLCHAIN=local go build -o bin/pair scripts/pair.go
}

# show prints the first line of a file, or that it cannot be read.
show() {
	head -n 1 "$1" 2>/dev/null || echo "unreadable"
}

envtxt() {
	echo "commit: $COMMIT, tree $TREE"
	echo "go: $(go version)"
	echo "os: $(uname -srm), $(nproc) CPUs allowed, cgroup cpu.max: $(show /sys/fs/cgroup/cpu.max)"
	echo "kernel boot id: $(show /proc/sys/kernel/random/boot_id)"
	echo "clock source: $(show /sys/devices/system/clocksource/clocksource0/current_clocksource)"
	echo "net.core.busy_read: $(show /proc/sys/net/core/busy_read), net.core.busy_poll: $(show /proc/sys/net/core/busy_poll)"
	bin/pair probe
	echo "thread siblings: $(for c in /sys/devices/system/cpu/cpu[0-9]*; do echo -n "${c##*/}=$(show "$c/topology/thread_siblings_list") "; done)"
	echo "cgroup memory.max: $(show /sys/fs/cgroup/memory.max), taskset: $(taskset --version 2>&1 | head -n 1)"
	local v
	for v in RUNPOD_CPU_COUNT RUNPOD_DC_ID RUNPOD_POD_ID; do
		[ -z "${!v+x}" ] || echo "$v=${!v}"
	done
	lscpu
}

engine() {
	envtxt
	exec bin/engine -addr 127.0.0.1:8080 -oe-addr 0.0.0.0:9000 -data "$(mktemp -d)"
}

# publish masks the results, packs the masked copy, prints the log copy of the
# archive and serves the archive. The log copy is not allowed to stop the
# results from being printed: if it cannot be made, the whole archive stands in
# for it.
publish() {
	local marker
	mkdir -p pub slim
	# An unmasked publication cannot be taken back, so without the mask nothing is published.
	if ! bin/pairrun redact -in results -out pub/results; then
		echo "pair: pairrun redact failed, nothing is published"
		exit 1
	fi
	tar -czf results.tgz -C pub results
	if ! { bin/pairrun slim -in pub/results -out slim/results && tar -C slim -cf - results | gzip -9n > results-log.tgz; }; then
		echo "pair: pairrun slim failed, results-log.tgz is a copy of results.tgz"
		cp results.tgz results-log.tgz
	fi
	marker="pair: results-log.tgz $(wc -c < results-log.tgz) bytes, sha256 $(sha256sum results-log.tgz | cut -d ' ' -f 1)"
	echo "pair: results.tgz $(wc -c < results.tgz) bytes, sha256 $(sha256sum results.tgz | cut -d ' ' -f 1)"
	echo "$marker"
	echo "-- begin results-log.tgz base64"
	base64 results-log.tgz
	echo "-- end results-log.tgz base64"
	echo "$marker"
	exec bin/pair serve :8000 "$RESULT_PATH" results.tgz
}

client() {
	: "${ENGINE_ADDR:?}" "${RESULT_PATH:?}"
	local n
	mkdir -p results
	envtxt > results/env.txt
	echo "pair: waiting for $ENGINE_ADDR"
	# shellcheck disable=SC2016 # the inner shell expands them
	until timeout 5 bash -c ': > "/dev/tcp/${0%:*}/${0##*:}"' "$ENGINE_ADDR" 2>/dev/null; do
		sleep 2
	done
	for n in $(seq "${RUNS:-5}"); do
		echo "== run $n"
		bin/loadgen -oe "$ENGINE_ADDR" -rate "${RATE:-10000}" -warmup "${WARMUP:-10s}" -duration "${DURATION:-60s}" \
			-out "results/run-$n.json" > "results/run-$n.txt" 2>&1 || true
		cat "results/run-$n.txt"
	done
	publish
}

supervise() {
	local work
	work=$(mktemp -d)
	envtxt | tee "$work/engine-env.txt"
	exec bin/pairrun supervise -engine bin/engine -loadgen bin/loadgen -listen :9100 -work "$work" -env "$work/engine-env.txt"
}

matrix() {
	: "${ENGINE_OE:?}" "${ENGINE_HTTP:?}" "${ENGINE_CTL:?}" "${RESULT_PATH:?}"
	mkdir -p results
	envtxt > results/env.txt
	bin/pairrun client -loadgen bin/loadgen -oe "$ENGINE_OE" -http "$ENGINE_HTTP" -ctl "$ENGINE_CTL" -out results \
		${DEADLINE:+-deadline "$DEADLINE"} ${DEADLINE_MARGIN:+-deadline-margin "$DEADLINE_MARGIN"} || echo "pair: pairrun client failed with status $?"
	publish
}

selftest() {
	while :; do
		echo "pair: alive at $(date -u +%FT%TZ)"
		sleep 60
	done
}

case ${1:-} in
boot) boot ;;
engine)
	build
	engine
	;;
client)
	build
	client
	;;
supervise)
	build
	supervise
	;;
matrix)
	build
	matrix
	;;
selftest) selftest ;;
*)
	sed -n '6,11p' "$0" >&2
	exit 2
	;;
esac
