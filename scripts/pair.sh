#!/usr/bin/env bash
# Measures the round trip of OE1 Pings between two rented Pods, the floor
# of docs/TASKS.md M8: the engine's OE1 listener on one, loadgen -oe on
# the other.
#
#   scripts/pair.sh boot       print the start command of a Pod
#   scripts/pair.sh engine     serve OE1 on 0.0.0.0:9000
#   scripts/pair.sh client     measure against ENGINE_ADDR, publish the results
#   scripts/pair.sh selftest   print a line every minute
#
# A Pod runs `bash -c` with the text that boot prints, in the golang image
# the Dockerfile pins, with MODE, COMMIT, TREE and DEADLINE in its
# environment. It first starts a timer that deletes the Pod at DEADLINE,
# in Unix seconds, through Runpod's API with the Pod's own key, and
# retries every 30 s; a restarted container keeps the same DEADLINE. It
# then fetches the tarball of COMMIT, refuses it unless git
# get-tar-commit-id names COMMIT and its files hash to the tree TREE, and
# runs this script from it in mode MODE. The start command does not end
# on its own, so a Pod keeps its log until it is deleted.
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
set -euo pipefail

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

boot() {
	cat <<-'EOF'
		(sleep "$((DEADLINE - $(date +%s)))"
		until printf 'Authorization: Bearer %s\n' "$RUNPOD_API_KEY" |
		    curl -fsS -o /dev/null -X DELETE -H @- "https://api.runpod.io/v2/pods/$RUNPOD_POD_ID"; do
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
		    bash src/scripts/pair.sh "$MODE"
		echo "pair: stopped with status $?"
		wait
	EOF
}

build() {
	cd "$repo"
	CGO_ENABLED=0 GOTOOLCHAIN=local go build -trimpath -o bin/ ./cmd/engine ./cmd/loadgen
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
	env | grep '^RUNPOD_' | grep -Eiv '^[^=]*(KEY|SECRET|TOKEN|PASS)' | sort || true
	lscpu
}

engine() {
	envtxt
	exec bin/engine -addr 127.0.0.1:8080 -oe-addr 0.0.0.0:9000 -data "$(mktemp -d)"
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
	tar -czf results.tgz results
	echo "pair: results.tgz $(wc -c < results.tgz) bytes, sha256 $(sha256sum results.tgz | cut -d ' ' -f 1)"
	echo "-- begin results.tgz base64"
	base64 results.tgz
	echo "-- end results.tgz base64"
	exec bin/pair serve :8000 "$RESULT_PATH" results.tgz
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
selftest) selftest ;;
*)
	sed -n '6,9p' "$0" >&2
	exit 2
	;;
esac
