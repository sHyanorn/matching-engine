#!/usr/bin/env bash
# Runs the start command of scripts/pair.sh as five Pods on this machine,
# against a stand-in for Runpod's API and codeload.github.com, and checks
#
#   - that each Pod deletes itself once, with its key, at its DEADLINE and
#     not before, and that no Pod prints the key;
#   - engine and client: that the engine serves OE1 on port 9000, and
#     that the client's archive, fetched from its port 8000, holds every
#     run's report and has the SHA-256 that the log prints, as has the
#     base64 copy in the log;
#   - selftest: that it prints its heartbeat;
#   - a tarball of another commit, and one with an added file: that the
#     start command refuses them and never runs pair.sh.
#
#   scripts/pair-test.sh
#
# With the argument `pilot` it runs the pilot instead (scripts/pair.sh
# supervise and matrix, cmd/pairrun): two Pods on this machine, each on
# half of its CPUs, with the plan's durations and send lag bound
# shortened by PILOT_* variables, and checks that the results come out
# whole, can be fetched from port 8000 with no one logged in, and say that
# they are a rehearsal. It takes minutes; the summary of what it found is
# printed. Ports 8000, 8080, 9000 and 9100 must be free.
#
# The tarballs hold the working tree. curl reaches the stand-in through a
# .curlrc in CURL_HOME, so the start command runs as it is. Ports 8000,
# 8080 and 9000 must be free. Needs Go at go.mod's version, git, GNU tar,
# openssl and python3.
set -euo pipefail

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
t=$(mktemp -d)
pids=()
cleanup() {
	local p
	for p in "${pids[@]}"; do
		kill -- "-$p" 2> /dev/null || true
	done
	rm -rf "$t"
}
trap cleanup EXIT
mkdir "$t/www" "$t/tmp" "$t/curl" "$t/export" "$t/objects"

hex() { od -An -tx1 -N"$1" /dev/urandom | tr -d ' \n'; }
key=rpa_$(hex 20)
path=$(hex 16)
commit=$(hex 20) other=$(hex 20) added=$(hex 20)

# The working tree as a tree object, and tarballs of it as codeload serves
# them: a pax global header whose comment is the commit ID, then the files
# under one directory.
# New objects go to a store of the test's own.
export GIT_INDEX_FILE=$t/index GIT_OBJECT_DIRECTORY=$t/objects GIT_ALTERNATE_OBJECT_DIRECTORIES=$repo/.git/objects
tree=$(cd "$repo" && git read-tree HEAD && git add -A && git write-tree)
git -C "$repo" archive --prefix=src/ "$tree" | tar -x -C "$t/export"
unset GIT_INDEX_FILE GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES
tarball() { tar --format=pax --pax-option="comment=$1" -czf "$t/www/$2" -C "$t/export" src; }
tarball "$commit" "$commit"
tarball "$commit" "$other"
echo added > "$t/export/src/added"
tarball "$added" "$added"

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 1 -subj /CN=stand-in \
	-addext "subjectAltName=DNS:api.runpod.io,DNS:codeload.github.com" \
	-keyout "$t/key.pem" -out "$t/cert.pem" 2> /dev/null
KEY=$key setsid python3 -I - "$t" <<-'EOF' &
	import http.server, os, ssl, sys, time
	t = sys.argv[1]
	class Handler(http.server.BaseHTTPRequestHandler):
	    def do_GET(self):
	        prefix = "/sHyanorn/matching-engine/tar.gz/"
	        name = self.path[len(prefix):]
	        if not self.path.startswith(prefix) or not os.path.isfile(os.path.join(t, "www", name)):
	            return self.send_error(404)
	        data = open(os.path.join(t, "www", name), "rb").read()
	        self.send_response(200)
	        self.send_header("Content-Length", str(len(data)))
	        self.end_headers()
	        self.wfile.write(data)
	    def do_DELETE(self):
	        prefix = "/v2/pods/"
	        if not self.path.startswith(prefix):
	            return self.send_error(404)
	        auth = "key" if self.headers.get("Authorization") == "Bearer " + os.environ["KEY"] else "other"
	        with open(os.path.join(t, "deletes"), "a") as f:
	            f.write(f"{self.path[len(prefix):]} {time.time():.3f} {auth}\n")
	        self.send_response(204)
	        self.end_headers()
	    def log_message(self, *args):
	        pass
	srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
	ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
	ctx.load_cert_chain(os.path.join(t, "cert.pem"), os.path.join(t, "key.pem"))
	srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
	with open(os.path.join(t, "port.tmp"), "w") as f:
	    f.write(str(srv.server_address[1]))
	os.rename(os.path.join(t, "port.tmp"), os.path.join(t, "port"))
	srv.serve_forever()
EOF
pids+=($!)
for _ in $(seq 50); do [ -e "$t/port" ] && break; sleep 0.1; done
port=$(cat "$t/port")
cat > "$t/curl/.curlrc" <<-EOF
	connect-to = api.runpod.io:443:127.0.0.1:$port
	connect-to = codeload.github.com:443:127.0.0.1:$port
	cacert = $t/cert.pem
	noproxy = *
EOF

boot=$("$repo/scripts/pair.sh" boot)
go=$(cd "$repo" && go env GOROOT)/bin
now=$(date +%s)
deadline=()

# pod NAME MODE COMMIT SECONDS [VAR=VALUE...] starts a Pod that deletes
# itself SECONDS from now, with its output in NAME.log.
pod() {
	local name=$1 mode=$2 c=$3 at=$((now + $4)) pin=() a
	shift 4
	deadline+=("$name $at")
	local vars=()
	for a in "$@"; do
		case $a in
		CPUS=*) pin=(taskset -c "${a#CPUS=}") ;;
		*) vars+=("$a") ;;
		esac
	done
	env PATH="$go:$PATH" TMPDIR="$t/tmp" CURL_HOME="$t/curl" MODE="$mode" COMMIT="$c" TREE="$tree" \
		DEADLINE="$at" RUNPOD_POD_ID="$name" RUNPOD_API_KEY="$key" "${vars[@]}" \
		"${pin[@]}" setsid bash -c "$boot" > "$t/$name.log" 2>&1 &
	pids+=($!)
}
if [ "${1:-}" != pilot ]; then
	pod engine engine "$commit" 40
	pod client client "$commit" 45 ENGINE_ADDR=127.0.0.1:9000 RESULT_PATH="$path" RUNS=2 WARMUP=1s DURATION=2s
	pod selftest selftest "$commit" 10
	pod other selftest "$other" 10
	pod added selftest "$added" 10
fi

failed=0
check() {
	local what=$1
	shift
	if "$@"; then echo "ok: $what"; else echo "FAIL: $what"; failed=1; fi
}
not() { ! "$@"; }
# deleted NAME AT: the stand-in saw one delete of NAME, with the key, in
# the 5 s from AT.
deleted() {
	awk -v id="$1" -v at="$2" '$1 == id { n++; ok = $2 >= at && $2 < at + 5 && $3 == "key" }
		END { exit !(n == 1 && ok) }' "$t/deletes"
}
# summed FILE: FILE has the SHA-256 that the client printed.
summed() { [ -n "$sum" ] && [ "$(sha256sum < "$1" | cut -d ' ' -f 1)" = "$sum" ]; }
reports() {
	[ -s "$t/results/results/env.txt" ] &&
		[ "$(grep -l '"mode": "oe"' "$t"/results/results/run-{1,2}.json | wc -l)" = 2 ]
}
# refused NAME LINE: NAME printed LINE, stopped with status 1, and its
# heartbeat never came.
refused() {
	grep -qx "$2" "$t/$1.log" && grep -qx 'pair: stopped with status 1' "$t/$1.log" &&
		not grep -q '^pair: alive' "$t/$1.log"
}

# pilot rehearses the pilot's two modes. The plan's durations and send lag
# bound are shortened; nothing else differs from a rented pair but the
# CPUs, which the two Pods share half and half. The result files are kept
# in $PILOT_KEEP when it is set.
pilot() {
	local over=("PILOT_WARMUP=${PILOT_WARMUP:-1s}" "PILOT_DURATION=${PILOT_DURATION:-3s}" "PILOT_LONG=${PILOT_LONG:-5s}" PILOT_MAX_SEND_LAG=20ms DEADLINE_MARGIN=30s)
	pod supervise supervise "$commit" 2400 CPUS=0,1 "${over[@]}"
	pod matrix matrix "$commit" 2400 CPUS=2,3 ENGINE_OE=127.0.0.1:9000 ENGINE_HTTP=127.0.0.1:8080 ENGINE_CTL=127.0.0.1:9100 \
		RESULT_PATH="$path" "${over[@]}"
	for _ in $(seq 1800); do
		grep -q -- '-- end results.tgz base64' "$t/matrix.log" && break
		sleep 1
	done
	sum=$(sed -n 's/^pair: results.tgz [0-9]* bytes, sha256 //p' "$t/matrix.log")
	curl -fsS --noproxy '*' --retry 5 --retry-connrefused -o "$t/fetched.tgz" "http://127.0.0.1:8000/$path/results.tgz" || true
	sed -n '/^-- begin results.tgz base64$/,/^-- end results.tgz base64$/p' "$t/matrix.log" | sed '1d;$d' |
		base64 -d > "$t/logged.tgz" || true
	mkdir "$t/results" && tar -xzf "$t/fetched.tgz" -C "$t/results" || true
	local r=$t/results/results
	check "the archive served on port 8000 has the SHA-256 in the log" summed "$t/fetched.tgz"
	check "the base64 copy in the log has the same SHA-256" summed "$t/logged.tgz"
	check "the archive holds the plan, the runs, the loopback run, G2 and the summary" \
		bash -c 'cd "$0" && [ -s env.txt ] && [ -s plan.json ] && [ -s runs.jsonl ] && [ -s loopback.json ] && [ -s g2.json ] && [ -s summary.json ] && [ -s summary.txt ]' "$r"
	check "every line of runs.jsonl is a JSON object and every run has its two report files" \
		python3 -I -c '
import json, os, sys
r = sys.argv[1]
for line in open(os.path.join(r, "runs.jsonl")):
    run = json.loads(line)
    base = os.path.splitext(os.path.join(r, run["report"]))[0]
    for ext in (".json", ".txt"):
        assert os.path.exists(base + ext), run["report"]
' "$r"
	check "the summary says that this is a rehearsal and not publishable" \
		python3 -I -c '
import json, sys
s = json.load(open(sys.argv[1] + "/summary.json"))
assert s["rehearsal"] is True and s["publishable"] is False
' "$r"
	check "the control port serves nothing but its routes" \
		bash -c '[ "$(curl -s --noproxy "*" -o /dev/null -w "%{http_code}" http://127.0.0.1:9100/proc/self/environ)" = 404 ] &&
			[ "$(curl -s --noproxy "*" -o /dev/null -w "%{http_code}" -X POST "http://127.0.0.1:9100/start?config=nope")" = 400 ]'
	check "no response of the control port holds the environment" \
		bash -c '! curl -s --noproxy "*" http://127.0.0.1:9100/host | grep -qE "PATH=|RUNPOD|COMMIT"'
	check "no output holds the key" not grep -rqF "$key" "$t"/*.log "$r"
	if [ -n "${PILOT_KEEP:-}" ]; then
		mkdir -p "$PILOT_KEEP" && cp -r "$r"/. "$PILOT_KEEP"/
	fi
	[ -s "$r/summary.txt" ] && cat "$r/summary.txt"
	[ "$failed" = 0 ] || {
		tail -n 20 "$t"/*.log
		exit 1
	}
}
if [ "${1:-}" = pilot ]; then
	pilot
	exit
fi

touch "$t/deletes"
for _ in $(seq 240); do
	grep -q -- '-- end results.tgz base64' "$t/client.log" && [ "$(date +%s)" -gt $((now + 50)) ] && break
	sleep 1
done
for d in "${deadline[@]}"; do
	read -r name at <<< "$d"
	check "$name deleted itself once, with its key, at its deadline" deleted "$name" "$at"
done
sum=$(sed -n 's/^pair: results.tgz [0-9]* bytes, sha256 //p' "$t/client.log")
curl -fsS --noproxy '*' --retry 5 --retry-connrefused -o "$t/fetched.tgz" "http://127.0.0.1:8000/$path/results.tgz" || true
sed -n '/^-- begin results.tgz base64$/,/^-- end results.tgz base64$/p' "$t/client.log" | sed '1d;$d' |
	base64 -d > "$t/logged.tgz" || true
check "the engine serves OE1 on port 9000" grep -q '^oe listening on .*:9000$' "$t/engine.log"
check "the archive served on port 8000 has the SHA-256 in the log" summed "$t/fetched.tgz"
check "the base64 copy in the log has the same SHA-256" summed "$t/logged.tgz"
check "nothing else is served" [ "$(curl -s --noproxy '*' -o /dev/null -w '%{http_code}' http://127.0.0.1:8000/results.tgz)" = 404 ]
mkdir "$t/results" && tar -xzf "$t/fetched.tgz" -C "$t/results" || true
check "the archive holds env.txt and every run's report" reports
check "the selftest Pod prints its heartbeat" grep -q '^pair: alive at ' "$t/selftest.log"
check "a tarball of another commit is refused" refused other "pair: tarball of commit $commit"
check "a tarball with an added file is refused" refused added "pair: tree $(sed -n 's/^pair: tree //p' "$t/added.log" | grep -vx "$tree")"
check "no output holds the key" not grep -rqF "$key" "$t"/*.log "$t/results"

[ "$failed" = 0 ] || {
	tail -n 20 "$t"/*.log
	exit 1
}
