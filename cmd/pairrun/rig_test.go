//go:build linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSupervisor is a supervisor's control port that starts no engine: it
// counts the starts, remembers the configurations asked for and answers with
// host records.
type fakeSupervisor struct {
	mu         sync.Mutex
	srv        *httptest.Server
	readyAfter int // /host answers not ready this many times first
	polls      int
	starts     int // as the host record counts them
	config     string
	started    []string     // the configuration of each /start, in order
	bump       map[int]bool // after the nth /start, another start happens before /host is read again
	bumped     map[int]bool
	failStarts map[int]bool   // the nth /start answers 500
	wrongStart map[int]string // the nth /start answers with a record of this other configuration
	hostFails  map[int]bool   // after the nth /start, /host answers 500
	onStart    func(n int)    // called at each /start, with its number
	loopback   map[string]string
	envText    string            // served as /engine-env.txt, if not empty
	clockNS    float64           // the record's clock_read_ns
	overrides  map[string]string // the record's overrides
}

func newFakeSupervisor(t *testing.T) *fakeSupervisor {
	t.Helper()
	f := &fakeSupervisor{
		bump: map[int]bool{}, bumped: map[int]bool{}, hostFails: map[int]bool{}, failStarts: map[int]bool{}, wrongStart: map[int]string{},
		loopback: map[string]string{"loopback.json": `{"valid": true, "mode": "oe_ioc"}`, "loopback.txt": "loopback table\n"},
		envText:  "clock source: tsc\nthread siblings: cpu0=0,4\n", clockNS: 30,
	}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSupervisor) addr() string { return strings.TrimPrefix(f.srv.URL, "http://") }

func (f *fakeSupervisor) record(ready bool) hostRecord {
	return hostRecord{
		Ready: ready, Starts: f.starts, Config: f.config, EnginePID: 4242,
		machine:       machine{CPUModel: "Engine CPU", NProc: 8, Loadavg: [3]float64{0.1, 0.2, 0.3}, BootID: "engine-boot", Time: "2026-11-22T09:00:00Z"},
		MemLimitBytes: 8 << 30, GoMemLimitBytes: 4 << 30, ClockReadNS: f.clockNS, Overrides: f.overrides,
	}
}

func (f *fakeSupervisor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method + " " + r.URL.Path {
	case "GET /host":
		f.polls++
		n := len(f.started)
		if f.hostFails[n] {
			http.Error(w, "host unreadable", http.StatusInternalServerError)
			return
		}
		if f.bump[n] && !f.bumped[n] {
			f.bumped[n] = true
			f.starts++
		}
		_ = json.NewEncoder(w).Encode(f.record(f.polls > f.readyAfter))
	case "POST /start":
		f.starts++
		f.config = r.URL.Query().Get("config")
		f.started = append(f.started, f.config)
		n := len(f.started)
		if f.onStart != nil {
			f.onStart(n)
		}
		if f.failStarts[n] {
			http.Error(w, "engine did not listen", http.StatusInternalServerError)
			return
		}
		h := f.record(true)
		if other, ok := f.wrongStart[n]; ok {
			h.Config = other
		}
		_ = json.NewEncoder(w).Encode(h)
	case "GET /loopback.json", "GET /loopback.txt":
		content, ok := f.loopback[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprint(w, content)
	case "GET /engine-env.txt":
		if f.envText == "" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprint(w, f.envText)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeSupervisor) startedConfigs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.started...)
}

// metricsServer serves the engine's /metrics page, or answers status.
func metricsServer(t *testing.T, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" || status != http.StatusOK {
			http.Error(w, "no metrics", status)
			return
		}
		_, _ = fmt.Fprint(w, `# TYPE go_gc_duration_seconds summary
go_gc_duration_seconds{quantile="0"} 3.84e-05
go_gc_duration_seconds_sum 0.5
go_gc_duration_seconds_count 12
process_resident_memory_bytes 2.1008384e+08
`)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// testClock is a clock that a test moves: step on every reading, and by hand.
type testClock struct {
	mu   sync.Mutex
	t    time.Time
	step time.Duration
}

var clockStart = time.Date(2026, 11, 22, 8, 0, 0, 0, time.UTC)

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.t
	c.t = c.t.Add(c.step)
	return t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// baseRun is what the fake loadgen does for run seq of cfg unless a test
// says otherwise: a valid report whose numbers differ from run to run.
func baseRun(cfg config, seq int) fakeRun {
	p50, p99 := 60+float64(seq)*0.123456789, 100+float64(seq)*0.1
	if cfg.Flow == flowF {
		p50 = 20 + float64(seq)*0.01
	}
	return fakeRun{Report: validReport(cfg.Flow, p50, p99).JSON(), Text: fmt.Sprintf("table of run %d\n", seq)}
}

func scriptKey(config string, round int) string { return config + "-r" + strconv.Itoa(round) }

// writeMatrixScript tells the fake loadgen what each run of the matrix of p
// does: baseRun, or what mod makes of it. It returns the path of the log of
// the arguments loadgen was started with.
func writeMatrixScript(t *testing.T, p params, mod func(cfg config, round int, r fakeRun) fakeRun) string {
	t.Helper()
	runs := map[string]fakeRun{}
	seq := 0
	add := func(cfg config, round int) {
		seq++
		r := baseRun(cfg, seq)
		if mod != nil {
			r = mod(cfg, round, r)
		}
		runs[scriptKey(cfg.Name, round)] = r
	}
	for round := 1; round <= p.Rounds; round++ {
		for _, cfg := range planConfigs(p, "oe", "http") {
			add(cfg, round)
		}
	}
	add(longRun(p, "oe", "http"), 0)
	return writeFakeScript(t, fakeScript{Runs: runs, NoDial: true})
}

// editReport changes the report of a run.
func editReport(r fakeRun, edit func(*cannedReport)) fakeRun {
	var base loadgenReport
	if err := json.Unmarshal(r.Report, &base); err != nil {
		panic(err)
	}
	rep := cannedReport{
		Valid: base.Valid, Problems: base.Problems, Target: base.TargetRate, Achieved: base.AchievedRate,
		Outcomes: base.Outcomes, Trades: base.Trades, Latency: base.Latency,
	}
	edit(&rep)
	r.Report = rep.JSON()
	return r
}

// rig is a client with a fake supervisor, a fake loadgen and a metrics
// page.
type rig struct {
	t      *testing.T
	sup    *fakeSupervisor
	c      *client
	out    string
	stdout *syncBuffer
	clock  *testClock
	http   string // the metrics server's address
}

func newRig(t *testing.T, p params, overrides map[string]string) *rig {
	t.Helper()
	r := &rig{t: t, sup: newFakeSupervisor(t), stdout: &syncBuffer{}, clock: &testClock{t: clockStart, step: time.Second}}
	r.http = metricsServer(t, http.StatusOK)
	r.out = filepath.Join(t.TempDir(), "results")
	r.c = newClient(clientOptions{
		Loadgen: fakeBinary(t, "fake-loadgen"), OE: "engine.test:9000", HTTP: r.http, Ctl: r.sup.addr(), Out: r.out,
		Params: p, Overrides: overrides,
	}, r.stdout)
	r.c.now = r.clock.Now
	r.c.pollEvery = time.Millisecond
	r.c.readyTimeout = 10 * time.Second
	r.c.clockRead = func() float64 { return 25 }
	r.c.src = fakeSources(fakeProc(t, fakeHostFiles("0-3")))
	return r
}

func (r *rig) run() error { return r.c.run(context.Background()) }

func (r *rig) file(name string) []byte {
	r.t.Helper()
	data, err := os.ReadFile(filepath.Join(r.out, name))
	if err != nil {
		r.t.Fatal(err)
	}
	return data
}

// records reads runs.jsonl as the runner wrote it.
func (r *rig) records() []runRecord {
	r.t.Helper()
	var out []runRecord
	for _, line := range bytes.Split(bytes.TrimSpace(r.file("runs.jsonl")), []byte("\n")) {
		var rec runRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			r.t.Fatalf("%v: %s", err, line)
		}
		out = append(out, rec)
	}
	return out
}

func (r *rig) summary() summaryFile {
	r.t.Helper()
	var s summaryFile
	if err := json.Unmarshal(r.file("summary.json"), &s); err != nil {
		r.t.Fatal(err)
	}
	return s
}

func (r *rig) g2() g2Result {
	r.t.Helper()
	var g g2Result
	if err := json.Unmarshal(r.file("g2.json"), &g); err != nil {
		r.t.Fatal(err)
	}
	return g
}

// loggedArgs reads the argument lists of the loadgens that were started, in
// order.
func loggedArgs(t *testing.T, log string) [][]string {
	t.Helper()
	f, err := os.Open(log)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out [][]string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var args []string
		if err := json.Unmarshal(sc.Bytes(), &args); err != nil {
			t.Fatal(err)
		}
		out = append(out, args)
	}
	return out
}

// rounds makes parameters of n rounds with the defaults otherwise.
func rounds(n int) params {
	p := defaultParams
	p.Rounds = n
	return p
}
