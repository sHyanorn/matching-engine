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
# from its environment, so that this script and everything it starts run
# without the key and only the timer holds it. The start command does not
# end on its own, so a Pod keeps its log until it is deleted.
#
# engine and client build engine and loadgen with the image's Go, as the
# Dockerfile does. client waits until ENGINE_ADDR (host:port) accepts a
# connection, then makes RUNS loadgen -oe runs (5) at RATE (10000) with
# WARMUP (10s) and DURATION (60s), each into run-N.json and run-N.txt,
# and writes env.txt. It packs them into results.tgz, prints the
# archive's SHA-256, each run's table and the archive in base64 to the
# log, and serves the archive at /RESULT_PATH/results.tgz on port 8000.
# env.txt holds the CPU, the clock source and the cost of reading it,
# whether SO_BUSY_POLL can be set, the kernel's boot ID (the same on two
# Pods only if they share a host), and the RUNPOD_* variables, but for
# those whose names suggest a secret.
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
# next to env.txt. matrix passes DEADLINE_MARGIN with DEADLINE: no run
# starts unless it would end that long before DEADLINE. It is a Go
# duration ("600s", not "600") and 10m when unset (pairrun's
# -deadline-margin); a value that is not one ends the client at flag
# parsing, before any run. PILOT_* variables override the plan's rates and
# durations for a rehearsal (cmd/pairrun).
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
	env | grep '^RUNPOD_' | grep -Eiv '^[^=]*(KEY|SECRET|TOKEN|PASS)' | sort || true
	lscpu
}

engine() {
	envtxt
	exec bin/engine -addr 127.0.0.1:8080 -oe-addr 0.0.0.0:9000 -data "$(mktemp -d)"
}

# publish packs the results, prints the archive and serves it.
publish() {
	tar -czf results.tgz results
	echo "pair: results.tgz $(wc -c < results.tgz) bytes, sha256 $(sha256sum results.tgz | cut -d ' ' -f 1)"
	echo "-- begin results.tgz base64"
	base64 results.tgz
	echo "-- end results.tgz base64"
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
