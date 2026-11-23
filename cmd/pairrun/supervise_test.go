//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	fakeMemLimit = 8 << 30 // the faked cgroup's limit: GOMEMLIMIT is half of it
	secretName   = "PAIRRUN_TEST_SECRET"
	secretValue  = "hunter2-do-not-leak"
)

// fakeHostFiles are the files of a host with 8 CPUs, a cgroup limit of
// fakeMemLimit and 16 GiB of memory.
func fakeHostFiles(cpus string) map[string]string {
	return map[string]string{
		"cpuinfo":    "processor\t: 0\nmodel name\t: Test CPU @ 3GHz\n",
		"loadavg":    "0.50 1.25 2.00 2/300 99\n",
		"boot_id":    "boot-1\n",
		"status":     "Name:\tpairrun\nCpus_allowed_list:\t" + cpus + "\n",
		"meminfo":    meminfo16G,
		"memory.max": fmt.Sprint(fakeMemLimit),
		"stat":       statText, "cpu.stat": cpuStatV2Text, "cpu.max": "max 100000\n", "cpu.pressure": pressureText,
	}
}

// supervisorTest is a supervisor with a stub engine and loadgen, served by an
// httptest server, with the files of an 8-CPU host.
type supervisorTest struct {
	*supervisor
	srv    *httptest.Server
	out    *syncBuffer
	cancel context.CancelFunc // ends the supervisor's context
}

func newSupervisorTest(t *testing.T, mutate ...func(*superviseOptions)) *supervisorTest {
	t.Helper()
	t.Setenv(secretName, secretValue)
	addrs := freeAddrs(t, 2)
	o := superviseOptions{
		Engine:   fakeBinary(t, "fake-engine"),
		Loadgen:  fakeBinary(t, "fake-loadgen"),
		Work:     filepath.Join(t.TempDir(), "work"),
		Params:   defaultParams,
		httpAddr: addrs[0],
		oeAddr:   addrs[1],
		src:      fakeSources(fakeProc(t, fakeHostFiles("0-7"))),
		// A measurement takes a tenth of a second or more.
		clockRead: func() float64 { return 25 },
	}
	for _, m := range mutate {
		m(&o)
	}
	ctx, cancel := context.WithCancel(context.Background())
	out := &syncBuffer{}
	s, err := newSupervisor(ctx, o, out)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	// The tests that serve must not pin the test binary's threads: one test of
	// wake_supervise_test.go does, with a meter of its own.
	s.wake.pin = func(int) (func() error, error) { return func() error { return nil }, nil }
	st := &supervisorTest{supervisor: s, srv: httptest.NewServer(s), out: out, cancel: cancel}
	t.Cleanup(func() {
		st.srv.Close()
		cancel()
		if err := s.stopEngine(); err != nil {
			t.Error(err)
		}
	})
	return st
}

// request sends a request to the control server and returns the status and
// body.
func (st *supervisorTest) request(t *testing.T, method, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, st.srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

// start asks for an engine in a configuration and returns the host record.
func (st *supervisorTest) start(t *testing.T, config string) hostRecord {
	t.Helper()
	code, body := st.request(t, http.MethodPost, "/start?config="+config)
	if code != http.StatusOK {
		t.Fatalf("/start?config=%s answered %d: %s", config, code, body)
	}
	var h hostRecord
	if err := json.Unmarshal([]byte(body), &h); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return h
}

// stub reads what the running engine wrote about its start, and its data
// directory.
func (st *supervisorTest) stub(t *testing.T) (rep stubReport, dir string) {
	t.Helper()
	eventually(t, "the engine to write its report", func() bool {
		st.mu.Lock()
		dir = st.dataDir
		st.mu.Unlock()
		data, err := os.ReadFile(filepath.Join(dir, "stub.json"))
		return dir != "" && err == nil && json.Unmarshal(data, &rep) == nil
	})
	return rep, dir
}

// alive reports whether a process with pid exists.
func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func TestSupervisorEngineAddressesAreFixed(t *testing.T) {
	if engineHTTPAddr != "0.0.0.0:8080" || engineOEAddr != "0.0.0.0:9000" {
		t.Errorf("engine listens on %s and %s, want 0.0.0.0:8080 and 0.0.0.0:9000", engineHTTPAddr, engineOEAddr)
	}
	s, err := newSupervisor(context.Background(), superviseOptions{
		Engine: "e", Loadgen: "l", Work: t.TempDir(), src: fakeSources(fakeProc(t, fakeHostFiles("0-7"))),
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if s.httpAddr != "0.0.0.0:8080" || s.oeAddr != "0.0.0.0:9000" {
		t.Errorf("supervisor starts the engine on %s and %s", s.httpAddr, s.oeAddr)
	}
}

func TestSupervisorConfigs(t *testing.T) {
	st := newSupervisorTest(t)
	st.ready = true
	for i, c := range []struct {
		config string
		env    []string
		extra  []string
	}{
		{"default", []string{}, nil},
		{"gogc-off", []string{"GOGC=off", fmt.Sprintf("GOMEMLIMIT=%d", fakeMemLimit/2)}, nil},
		{"spin-pin", []string{}, []string{"-oe-spin"}},
	} {
		h := st.start(t, c.config)
		rep, dir := st.stub(t)
		wantArgs := append([]string{"-addr", st.httpAddr, "-oe-addr", st.oeAddr, "-data", dir}, c.extra...)
		if !reflect.DeepEqual(rep.Args, wantArgs) {
			t.Errorf("%s: engine args %v, want %v", c.config, rep.Args, wantArgs)
		}
		slices.Sort(rep.Env)
		if !reflect.DeepEqual(rep.Env, c.env) {
			t.Errorf("%s: engine environment %v, want exactly %v", c.config, rep.Env, c.env)
		}
		if filepath.Dir(dir) != st.work {
			t.Errorf("%s: data directory %s is not under %s", c.config, dir, st.work)
		}
		if !h.Ready || h.Starts != i+1 || h.Config != c.config || !alive(h.EnginePID) {
			t.Errorf("%s: host record %+v", c.config, h)
		}
		if h.MemLimitBytes != fakeMemLimit || h.GoMemLimitBytes != fakeMemLimit/2 {
			t.Errorf("%s: memory limit %d and GOMEMLIMIT %d, want %d and %d", c.config, h.MemLimitBytes, h.GoMemLimitBytes, int64(fakeMemLimit), int64(fakeMemLimit/2))
		}
	}
}

// A /start whose client has gone while it waited for the lock does not start
// an engine; the next one does.
func TestSupervisorStartOfAGoneClientStartsNothing(t *testing.T) {
	st := newSupervisorTest(t)
	st.ready = true
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/start?config=default", nil).WithContext(ctx)
	st.op.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		st.ServeHTTP(httptest.NewRecorder(), req)
	}()
	cancel()
	st.op.Unlock()
	<-done
	if entries, _ := filepath.Glob(filepath.Join(st.work, "*")); st.starts != 0 || len(entries) != 0 || st.cmd != nil {
		t.Errorf("a request whose client had gone started an engine: starts %d, %v", st.starts, entries)
	}
	if h := st.start(t, "default"); h.Starts != 1 || !alive(h.EnginePID) {
		t.Errorf("the next /start gave %+v, want the first start", h)
	}
}

func TestSupervisorSecondStartGivesANewEmptyDirectory(t *testing.T) {
	st := newSupervisorTest(t)
	st.ready = true
	first := st.start(t, "default")
	rep1, dir1 := st.stub(t)
	if len(rep1.EntriesAtStart) != 0 {
		t.Errorf("the first data directory held %v at the start", rep1.EntriesAtStart)
	}
	if err := os.WriteFile(filepath.Join(dir1, "old-command-log"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	second := st.start(t, "gogc-off")
	rep2, dir2 := st.stub(t)
	if dir2 == dir1 {
		t.Fatalf("both engines used %s", dir1)
	}
	if _, err := os.Stat(dir1); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the old data directory is still there: %v", err)
	}
	if len(rep2.EntriesAtStart) != 0 {
		t.Errorf("the new data directory held %v at the start", rep2.EntriesAtStart)
	}
	if second.Starts != first.Starts+1 {
		t.Errorf("starts went from %d to %d, want one more", first.Starts, second.Starts)
	}
	if second.EnginePID == first.EnginePID || alive(first.EnginePID) {
		t.Errorf("the first engine (pid %d) is alive, or the second has its pid %d", first.EnginePID, second.EnginePID)
	}

	code, body := st.request(t, http.MethodPost, "/stop")
	var stopped hostRecord
	if err := json.Unmarshal([]byte(body), &stopped); err != nil || code != http.StatusOK {
		t.Fatalf("/stop answered %d %q: %v", code, body, err)
	}
	if stopped.EnginePID != 0 || stopped.Config != "" || stopped.Starts != second.Starts || alive(second.EnginePID) {
		t.Errorf("after /stop: %+v", stopped)
	}
	if _, err := os.Stat(dir2); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("/stop left the data directory: %v", err)
	}
}

func TestSupervisorKillsAnEngineThatIgnoresSIGTERM(t *testing.T) {
	st := newSupervisorTest(t, func(o *superviseOptions) { o.Engine = fakeBinary(t, "fake-engine-deaf") })
	st.ready = true
	st.stopWait = 300 * time.Millisecond
	first := st.start(t, "default")
	began := time.Now()
	st.start(t, "default")
	if took := time.Since(began); took < st.stopWait {
		t.Errorf("the second start took %v: the engine was not given %v to stop", took, st.stopWait)
	}
	if alive(first.EnginePID) {
		t.Errorf("the engine that ignores SIGTERM (pid %d) is still alive", first.EnginePID)
	}
}

func TestSupervisorEngineThatNeverListens(t *testing.T) {
	st := newSupervisorTest(t, func(o *superviseOptions) { o.Engine = fakeBinary(t, "fake-engine-mute") })
	st.ready = true
	st.readyWait = 300 * time.Millisecond
	code, body := st.request(t, http.MethodPost, "/start?config=default")
	if code != http.StatusInternalServerError || !strings.Contains(body, "did not listen on "+dialAddr(st.httpAddr)) {
		t.Errorf("/start answered %d %q, want 500 that says the engine did not listen", code, body)
	}
	st.mu.Lock()
	cmd, dir := st.cmd, st.dataDir
	st.mu.Unlock()
	if cmd != nil || dir != "" {
		t.Error("the supervisor still holds the engine that never listened")
	}
	var h hostRecord
	_, hostBody := st.request(t, http.MethodGet, "/host")
	if err := json.Unmarshal([]byte(hostBody), &h); err != nil || h.EnginePID != 0 || h.Config != "" || h.Starts != 1 {
		t.Errorf("host record after the failed start: %+v, %v", h, err)
	}
	if entries, _ := filepath.Glob(filepath.Join(st.work, "data-*")); len(entries) != 0 {
		t.Errorf("data directories left behind: %v", entries)
	}
}

func TestSupervisorEngineThatCannotBeStarted(t *testing.T) {
	st := newSupervisorTest(t, func(o *superviseOptions) { o.Engine = "/nonexistent/engine" })
	st.ready = true
	if code, body := st.request(t, http.MethodPost, "/start?config=default"); code != http.StatusInternalServerError || !strings.Contains(body, "starting the engine") {
		t.Errorf("/start answered %d %q", code, body)
	}
}

func TestSupervisorRoutes(t *testing.T) {
	st := newSupervisorTest(t)
	st.ready = true
	for _, c := range []struct{ method, path string }{
		{"GET", "/"}, {"GET", "/env"}, {"GET", "/environ"}, {"GET", "/debug/pprof/"}, {"GET", "/metrics"},
		{"POST", "/host"}, {"HEAD", "/host"}, {"DELETE", "/host"}, {"GET", "/host/"}, {"GET", "/host/x"}, {"GET", "//host"},
		{"GET", "/start"}, {"PUT", "/start"}, {"POST", "/start/"}, {"GET", "/stop"}, {"POST", "/stop/x"},
		{"GET", "/loopback"}, {"GET", "/loopback.json/"}, {"POST", "/loopback.json"}, {"HEAD", "/loopback.txt"}, {"GET", "/Host"},
		{"GET", "/engine-env"}, {"GET", "/engine-env.txt/"}, {"POST", "/engine-env.txt"}, {"HEAD", "/engine-env.txt"},
		{"POST", "/engine-log?start=1"}, {"HEAD", "/engine-log?start=1"}, {"GET", "/engine-log/"}, {"GET", "/engine-logs"}, {"GET", "/engine-log.txt"},
		{"POST", "/counters"}, {"HEAD", "/counters"}, {"GET", "/counters/"}, {"GET", "/Counters"}, {"GET", "/counters.json"},
		{"POST", "/wake?from=0&to=1"}, {"HEAD", "/wake?from=0&to=1"}, {"GET", "/wake/"}, {"GET", "/Wake"}, {"GET", "/wake.json"}, {"PUT", "/wake"},
		{"POST", "/exec?cmd=id"}, {"OPTIONS", "/host"},
	} {
		if code, body := st.request(t, c.method, c.path); code != http.StatusNotFound {
			t.Errorf("%s %s answered %d %q, want 404", c.method, c.path, code, body)
		}
	}
	for _, path := range []string{"/start", "/start?config=", "/start?config=bogus", "/start?config=GOGC=off", "/start?config=default%20-x"} {
		if code, body := st.request(t, http.MethodPost, path); code != http.StatusBadRequest || !strings.Contains(body, "unknown config") {
			t.Errorf("POST %s answered %d %q, want 400", path, code, body)
		}
	}
	if code, _ := st.request(t, http.MethodGet, "/host"); code != http.StatusOK {
		t.Errorf("GET /host answered %d", code)
	}
	if code, _ := st.request(t, http.MethodGet, "/counters"); code != http.StatusOK {
		t.Errorf("GET /counters answered %d", code)
	}
	if code, _ := st.request(t, http.MethodGet, "/wake?from=0&to=1"); code != http.StatusOK {
		t.Errorf("GET /wake answered %d", code)
	}
	if code, _ := st.request(t, http.MethodPost, "/stop"); code != http.StatusOK {
		t.Errorf("POST /stop answered %d", code)
	}
}

func TestSupervisorBeforeReady(t *testing.T) {
	st := newSupervisorTest(t)
	if code, body := st.request(t, http.MethodPost, "/start?config=default"); code != http.StatusServiceUnavailable {
		t.Errorf("/start before ready answered %d %q, want 503", code, body)
	}
	if code, _ := st.request(t, http.MethodPost, "/stop"); code != http.StatusServiceUnavailable {
		t.Errorf("/stop before ready answered %d, want 503", code)
	}
	if code, _ := st.request(t, http.MethodPost, "/start?config=bogus"); code != http.StatusBadRequest {
		t.Errorf("an unknown config before ready answered %d, want 400", code)
	}
	var h hostRecord
	_, body := st.request(t, http.MethodGet, "/host")
	if err := json.Unmarshal([]byte(body), &h); err != nil || h.Ready {
		t.Errorf("/host before ready: %+v, %v", h, err)
	}
	if entries, _ := filepath.Glob(filepath.Join(st.work, "*")); st.starts != 0 || len(entries) != 0 {
		t.Errorf("a refused /start left traces: starts %d, %v", st.starts, entries)
	}
}

func TestSupervisorHostRecord(t *testing.T) {
	st := newSupervisorTest(t)
	st.ready = true
	st.now = func() time.Time { return time.Date(2026, 11, 22, 9, 30, 0, 0, time.FixedZone("x", 3600)) }
	_, body := st.request(t, http.MethodGet, "/host")
	var h hostRecord
	if err := json.Unmarshal([]byte(body), &h); err != nil {
		t.Fatal(err)
	}
	want := hostRecord{
		Ready: true, MemLimitBytes: fakeMemLimit, GoMemLimitBytes: fakeMemLimit / 2, ClockReadNS: 25,
		machine: machine{CPUModel: "Test CPU @ 3GHz", NProc: 8, Loadavg: [3]float64{0.5, 1.25, 2}, BootID: "boot-1", Time: "2026-11-22T08:30:00Z"},
	}
	if !reflect.DeepEqual(h, want) {
		t.Errorf("host record %+v, want %+v", h, want)
	}

	if err := os.Remove(st.src.status); err != nil {
		t.Fatal(err)
	}
	if code, _ := st.request(t, http.MethodGet, "/host"); code != http.StatusInternalServerError {
		t.Errorf("/host without the host's files answered %d, want 500", code)
	}
}

// countersRoute reads /counters.
func (st *supervisorTest) countersRoute(t *testing.T) counters {
	t.Helper()
	code, body := st.request(t, http.MethodGet, "/counters")
	var c counters
	if err := json.Unmarshal([]byte(body), &c); code != http.StatusOK || err != nil {
		t.Fatalf("/counters answered %d %q, %v", code, body, err)
	}
	return c
}

// hasProcessCounters reports whether c holds any counter of a process, as a
// value or as unreadable.
func hasProcessCounters(c counters) bool {
	for _, name := range processNames {
		_, read := c.Values[name]
		_, not := c.Unreadable[name]
		if read || not {
			return true
		}
	}
	return false
}

func TestSupervisorServesCounters(t *testing.T) {
	st := newSupervisorTest(t)
	st.ready = true
	dir := filepath.Dir(st.src.procDir) // where fakeSources has the files, and where the threads go

	// No engine runs: the machine's counters and none of a process.
	c := st.countersRoute(t)
	values(t, c, map[string]int64{
		cpuNrPeriods: 100, cpuNrThrottled: 7, cpuThrottledUS: 4500, cpuQuotaUS: -1, cpuPeriodUS: 100000,
		psiCPUSomeUS: 210309781, stealTicks: 3306,
	})
	if hasProcessCounters(c) {
		t.Errorf("with no engine, the answer has counters of a process: %+v", c)
	}

	// The engine that /host reports has its threads read, and no other process.
	h := st.start(t, "default")
	fakeThreads(t, dir, h.EnginePID, map[string]string{"1": "1000 100 3\n", "2": "2000 200 5\n"})
	c = st.countersRoute(t)
	for name, want := range map[string]int64{schedRunNS: 3000, schedDelayNS: 300, schedSlices: 8, schedTasks: 2, stealTicks: 3306} {
		if c.Values[name] != want {
			t.Errorf("%s is %d, want %d (unreadable: %v)", name, c.Values[name], want, c.Unreadable)
		}
	}

	// A stopped engine, or one that has exited, is not read.
	if code, body := st.request(t, http.MethodPost, "/stop"); code != http.StatusOK {
		t.Fatalf("/stop answered %d %q", code, body)
	}
	if c = st.countersRoute(t); hasProcessCounters(c) {
		t.Errorf("after /stop, the answer has counters of a process: %+v", c)
	}
	h = st.start(t, "default")
	fakeThreads(t, dir, h.EnginePID, map[string]string{"1": "1000 100 3\n"})
	if err := syscall.Kill(h.EnginePID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the supervisor to see that the engine has exited", func() bool {
		_, body := st.request(t, http.MethodGet, "/host")
		var rec hostRecord
		return json.Unmarshal([]byte(body), &rec) == nil && rec.EnginePID == 0
	})
	if c = st.countersRoute(t); hasProcessCounters(c) {
		t.Errorf("after the engine exited, the answer has counters of a process: %+v", c)
	}
	if c.Values[stealTicks] != 3306 {
		t.Errorf("the machine's counters are gone with the engine: %+v", c)
	}
}

// The files of a machine that has nothing of what is read say so for each
// name, and the route still answers.
func TestSupervisorCountersOfAMachineWithoutTheFiles(t *testing.T) {
	st := newSupervisorTest(t, func(o *superviseOptions) {
		o.src = fakeSources(fakeProc(t, map[string]string{"meminfo": meminfo16G}))
	})
	c := st.countersRoute(t)
	eachNameOnce(t, c, machineNames...)
	if len(c.Values) != 0 {
		t.Errorf("values %v of files that do not exist", c.Values)
	}
}

func TestSupervisorNeverReturnsTheEnvironment(t *testing.T) {
	st := newSupervisorTest(t)
	st.ready = true
	var everything strings.Builder
	record := func(method, path string) {
		req, _ := http.NewRequest(method, st.srv.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		fmt.Fprintf(&everything, "%s %s\n%v\n%s\n", method, path, resp.Header, body)
	}
	if err := os.WriteFile(filepath.Join(st.work, "loopback.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][2]string{
		{"GET", "/host"}, {"POST", "/start?config=gogc-off"}, {"GET", "/host"}, {"POST", "/start?config=bogus"}, {"GET", "/env"},
		{"GET", "/loopback.json"}, {"GET", "/loopback.txt"}, {"POST", "/stop"},
	} {
		record(c[0], c[1])
	}
	st.engineBin = "/nonexistent/engine"
	record("POST", "/start?config=default")
	if strings.Contains(everything.String(), secretValue) || strings.Contains(everything.String(), secretName) {
		t.Errorf("a response holds the environment:\n%s", everything.String())
	}

	st.engineBin = fakeBinary(t, "fake-engine")
	st.start(t, "default")
	rep, _ := st.stub(t)
	for _, kv := range rep.Env {
		t.Errorf("the engine got %q of the supervisor's environment", kv)
	}
}

func TestSupervisorServesLoopbackFiles(t *testing.T) {
	st := newSupervisorTest(t)
	st.ready = true
	if code, _ := st.request(t, http.MethodGet, "/loopback.json"); code != http.StatusNotFound {
		t.Errorf("/loopback.json without a file answered %d, want 404", code)
	}
	for name, content := range map[string]string{"loopback.json": `{"valid": true}`, "loopback.txt": "table\n"} {
		if err := os.WriteFile(filepath.Join(st.work, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if code, body := st.request(t, http.MethodGet, "/"+name); code != http.StatusOK || body != content {
			t.Errorf("/%s answered %d %q, want %q", name, code, body, content)
		}
	}
}

func TestSupervisorServesTheEnvText(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "envtxt")
	st := newSupervisorTest(t, func(o *superviseOptions) { o.EnvFile = envFile })
	st.ready = true
	if code, _ := st.request(t, http.MethodGet, "/engine-env.txt"); code != http.StatusNotFound {
		t.Errorf("/engine-env.txt before its file exists answered %d, want 404", code)
	}
	for _, text := range []string{"clock source: tsc\nthread siblings: cpu0=0,4\n", "clock source: hpet\n"} {
		if err := os.WriteFile(envFile, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		if code, body := st.request(t, http.MethodGet, "/engine-env.txt"); code != http.StatusOK || body != text {
			t.Errorf("/engine-env.txt answered %d %q, want the file's text %q, read at the request", code, body, text)
		}
	}
}

func TestSupervisorWithoutAnEnvFileServesNoEnvText(t *testing.T) {
	st := newSupervisorTest(t)
	st.ready = true
	if code, body := st.request(t, http.MethodGet, "/engine-env.txt"); code != http.StatusNotFound {
		t.Errorf("/engine-env.txt without -env answered %d %q, want 404", code, body)
	}
}

// engineLogBytes is n bytes that differ from place to place, so that one range
// of them cannot pass for another. The first n of more bytes are the n.
func engineLogBytes(n int) []byte {
	b := make([]byte, n)
	x := uint32(1)
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = 'a' + byte(x>>24)%26
	}
	return b
}

// setStarts makes the supervisor say that it has made n starts.
func (st *supervisorTest) setStarts(n int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.starts = n
}

func (st *supervisorTest) writeEngineLog(t *testing.T, n int, data []byte) {
	t.Helper()
	if err := os.WriteFile(engineLogName(st.work, n), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The tests spell out the design's numbers (a limit of 64 KiB, a head of 16 KiB
// and a tail of 48 KiB) instead of reading the constants back.
func TestSupervisorServesAnEngineLogWholeUpToTheLimit(t *testing.T) {
	st := newSupervisorTest(t)
	st.ready = true
	st.setStarts(1)
	for _, size := range []int{0, 1, 16 << 10, 64<<10 - 1, 64 << 10} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			data := engineLogBytes(size)
			st.writeEngineLog(t, 1, data)
			code, body := st.request(t, http.MethodGet, "/engine-log?start=1")
			if code != http.StatusOK || body != string(data) {
				t.Errorf("a log of %d bytes: answered %d with %d bytes, want 200 and the file as it is", size, code, len(body))
			}
		})
	}
}

func TestSupervisorCutsAnEngineLogOverTheLimit(t *testing.T) {
	const head, tail = 16 << 10, 48 << 10
	st := newSupervisorTest(t)
	st.ready = true
	st.setStarts(1)
	for _, size := range []int{64<<10 + 1, 64<<10 + 2, 100_000, 8<<20 + 123} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			data := engineLogBytes(size)
			st.writeEngineLog(t, 1, data)
			code, body := st.request(t, http.MethodGet, "/engine-log?start=1")
			if code != http.StatusOK {
				t.Fatalf("a log of %d bytes: answered %d: %.200s", size, code, body)
			}
			note := fmt.Sprintf("\n[engine log cut: %d of %d bytes omitted]\n", size-64<<10, size)
			if want := head + len(note) + tail; len(body) != want {
				t.Fatalf("the body is %d bytes, want %d: the head, %q and the tail", len(body), want, note)
			}
			if body[:head] != string(data[:head]) {
				t.Error("the body does not start with the file's first 16 KiB")
			}
			if got := body[head : len(body)-tail]; got != note {
				t.Errorf("between the head and the tail: %q, want %q", got, note)
			}
			if body[len(body)-tail:] != string(data[size-tail:]) {
				t.Error("the body does not end with the file's last 48 KiB")
			}
		})
	}
}

// The engine may append while its log is read: the size is read once, so the
// note and the two ranges describe the same file.
func TestEngineLogOfAGrowingFileIsReadAtOneSize(t *testing.T) {
	name := filepath.Join(t.TempDir(), "engine-001.log")
	all := engineLogBytes(8 << 20)
	f, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	written := 100 << 10
	if _, err := f.Write(all[:written]); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for written+4096 <= len(all) {
			select {
			case <-stop:
				return
			case <-time.After(time.Microsecond):
			}
			if _, err := f.Write(all[written : written+4096]); err != nil {
				return
			}
			written += 4096
		}
	}()
	defer func() { <-done }()
	defer close(stop)

	grew := false
	for reads, began := 0, time.Now(); reads < 300 || !grew; reads++ {
		if time.Since(began) > 10*time.Second {
			t.Fatal("the file did not grow while it was read")
		}
		body, err := readEngineLog(name)
		if err != nil {
			t.Fatal(err)
		}
		var omitted, size int
		if _, err := fmt.Sscanf(string(body[16<<10:]), "\n[engine log cut: %d of %d bytes omitted]\n", &omitted, &size); err != nil {
			t.Fatalf("no note after the head: %v", err)
		}
		grew = grew || size > 100<<10
		if omitted != size-64<<10 || string(body[:16<<10]) != string(all[:16<<10]) || string(body[len(body)-48<<10:]) != string(all[size-48<<10:size]) {
			t.Fatalf("a log read at %d bytes, with %d omitted, is not that file's head and tail", size, omitted)
		}
	}
}

// The served text of a log is redacted; the file stays as the engine wrote it
// and the note counts the file's bytes.
func TestSupervisorRedactsTheEngineLogItServes(t *testing.T) {
	st := newSupervisorTest(t)
	st.ready = true
	st.setStarts(1)
	const (
		remote   = `{"time":"2026-11-23T05:46:09.28696149Z","level":"INFO","msg":"oe connection closed","session":0,"remote":"10.1.2.3:46376","reason":"EOF"}` + "\n"
		netErr   = "oe: read tcp 10.1.2.3:9000->4.5.6.7:8899: connection reset by peer\n"
		kept     = "listening on 0.0.0.0:8080 from 127.0.0.1:46376, 1.2.3.4.5, 2026-11-23T05:46:09.271175058Z, data-3568997236\n"
		redacted = `{"time":"2026-11-23T05:46:09.28696149Z","level":"INFO","msg":"oe connection closed","session":0,"remote":"x.x.x.x","reason":"EOF"}` + "\n" +
			"oe: read tcp x.x.x.x->x.x.x.x: connection reset by peer\n" + kept
	)
	t.Run("a whole log", func(t *testing.T) {
		st.writeEngineLog(t, 1, []byte(remote+netErr+kept))
		if code, body := st.request(t, http.MethodGet, "/engine-log?start=1"); code != http.StatusOK || body != redacted {
			t.Errorf("answered %d %q, want %q", code, body, redacted)
		}
	})
	t.Run("a cut log", func(t *testing.T) {
		const head, tail = 16 << 10, 48 << 10
		data := engineLogBytes(100_000)
		copy(data, remote)
		copy(data[head-len(netErr):], netErr)      // the last bytes of the head
		copy(data[len(data)-tail:], remote)        // the first bytes of the tail
		copy(data[len(data)-len(netErr):], netErr) // the end of the file
		st.writeEngineLog(t, 1, data)
		code, body := st.request(t, http.MethodGet, "/engine-log?start=1")
		if code != http.StatusOK {
			t.Fatalf("answered %d", code)
		}
		note := fmt.Sprintf("\n[engine log cut: %d of %d bytes omitted]\n", len(data)-64<<10, len(data))
		redactedRemote, redactedErr := strings.Split(redacted, "\n")[0]+"\n", strings.Split(redacted, "\n")[1]+"\n"
		wantHead := redactedRemote + string(data[len(remote):head-len(netErr)]) + redactedErr
		wantTail := redactedRemote + string(data[len(data)-tail+len(remote):len(data)-len(netErr)]) + redactedErr
		if want := wantHead + note + wantTail; body != want {
			t.Errorf("the body is %d bytes, want %d: the redacted head, the note for the file's %d bytes and the redacted tail", len(body), len(want), len(data))
		}
		if file, err := os.ReadFile(engineLogName(st.work, 1)); err != nil || !strings.Contains(string(file), "10.1.2.3:46376") || len(file) != len(data) {
			t.Errorf("the file on disk was changed: %d bytes, %v", len(file), err)
		}
	})
}

// Nothing but a start of the supervisor selects a file: a number that is no
// number is a 400, a start that does not exist a 404, and neither serves any
// of the files that are there.
func TestSupervisorEngineLogRefusals(t *testing.T) {
	st := newSupervisorTest(t)
	st.ready = true
	st.setStarts(2)
	planted := map[string]string{
		engineLogName(st.work, 1):         "log of start one",
		engineLogName(st.work, 2):         "log of start two",
		engineLogName(st.work, 3):         "log of a start that has not been made",
		filepath.Join(st.work, "2"):       "file named 2 in the work directory",
		filepath.Join(st.work, "..", "x"): "file next to the work directory",
	}
	for name, text := range planted {
		if err := os.WriteFile(name, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		query string
		want  int
	}{
		{"", http.StatusBadRequest}, {"start=", http.StatusBadRequest}, {"start=abc", http.StatusBadRequest},
		{"start=../x", http.StatusBadRequest}, {"start=..%2Fx", http.StatusBadRequest},
		{"start=1/../2", http.StatusBadRequest}, {"start=1%2F..%2F2", http.StatusBadRequest},
		{"start=engine-001.log", http.StatusBadRequest}, {"start=-1", http.StatusBadRequest}, {"start=%2B1", http.StatusBadRequest},
		{"start=1.5", http.StatusBadRequest}, {"start=0x1", http.StatusBadRequest}, {"start=%201", http.StatusBadRequest},
		{"start=1%20", http.StatusBadRequest}, {"start=1,2", http.StatusBadRequest}, {"start=%D9%A1", http.StatusBadRequest},
		{"start=0", http.StatusNotFound}, {"start=00", http.StatusNotFound}, {"start=3", http.StatusNotFound},
		{"start=99999999999999999999", http.StatusNotFound},
	} {
		code, body := st.request(t, http.MethodGet, "/engine-log?"+c.query)
		if code != c.want {
			t.Errorf("GET /engine-log?%s answered %d, want %d", c.query, code, c.want)
		}
		for _, text := range planted {
			if strings.Contains(body, text) {
				t.Errorf("GET /engine-log?%s served %q", c.query, text)
			}
		}
	}
	// The route works for the starts that exist, so the refusals above are the
	// route's judgement of the number.
	for n, want := range map[int]string{1: "log of start one", 2: "log of start two"} {
		if code, body := st.request(t, http.MethodGet, fmt.Sprintf("/engine-log?start=%d", n)); code != http.StatusOK || body != want {
			t.Errorf("start %d answered %d %q, want %q", n, code, body, want)
		}
	}
	// A start that was made but left no file is as missing as one that was not.
	st.setStarts(4)
	if code, body := st.request(t, http.MethodGet, "/engine-log?start=4"); code != http.StatusNotFound {
		t.Errorf("a start with no log file answered %d %q, want 404", code, body)
	}
	if code, body := st.request(t, http.MethodGet, "/engine-log?start=3"); code != http.StatusOK || body != planted[engineLogName(st.work, 3)] {
		t.Errorf("start 3 answered %d %q once it was made", code, body)
	}
}

// The route reads the file that startEngine made, whatever the start's number.
func TestSupervisorServesTheOutputOfEachEnginesStart(t *testing.T) {
	st := newSupervisorTest(t)
	st.ready = true
	if code, _ := st.request(t, http.MethodGet, "/engine-log?start=1"); code != http.StatusNotFound {
		t.Errorf("before any start the route answered %d, want 404", code)
	}
	st.start(t, "default")
	_, dir1 := st.stub(t)
	st.start(t, "spin-pin")
	_, dir2 := st.stub(t)
	for n, c := range []struct{ has, hasNot string }{{dir1, dir2}, {dir2, dir1}} {
		code, body := st.request(t, http.MethodGet, fmt.Sprintf("/engine-log?start=%d", n+1))
		if code != http.StatusOK || !strings.Contains(body, "fake engine stdout, data "+c.has+"\n") ||
			!strings.Contains(body, "fake engine stderr, data "+c.has+"\n") || strings.Contains(body, c.hasNot) {
			t.Errorf("start %d answered %d %q, want the stdout and stderr of that engine alone", n+1, code, body)
		}
	}
	if code, _ := st.request(t, http.MethodGet, "/engine-log?start=3"); code != http.StatusNotFound {
		t.Errorf("start 3 answered %d, want 404", code)
	}
}

func TestSupervisorReportsItsOverrides(t *testing.T) {
	want := map[string]string{"PILOT_DURATION": "5s", "PILOT_ROUNDS": "2"}
	st := newSupervisorTest(t, func(o *superviseOptions) { o.Overrides = want })
	st.ready = true
	_, body := st.request(t, http.MethodGet, "/host")
	var h hostRecord
	if err := json.Unmarshal([]byte(body), &h); err != nil || !reflect.DeepEqual(h.Overrides, want) {
		t.Errorf("/host reports overrides %v, %v, want %v", h.Overrides, err, want)
	}
	if got := st.start(t, "default"); !reflect.DeepEqual(got.Overrides, want) {
		t.Errorf("/start reports overrides %v, want %v", got.Overrides, want)
	}
}

func TestSupervisorWithoutOverridesReportsNone(t *testing.T) {
	st := newSupervisorTest(t)
	st.ready = true
	if _, body := st.request(t, http.MethodGet, "/host"); strings.Contains(body, "overrides") {
		t.Errorf("/host of a supervisor with no overrides: %s", body)
	}
}

// The supervisor reads its PILOT_* variables where it reads its flags.
func TestSuperviseOptionsHoldTheEnvironmentsOverrides(t *testing.T) {
	flags := []string{"-engine", "e", "-loadgen", "l", "-work", "w", "-env", "envtxt"}
	t.Setenv("PILOT_DURATION", "5s")
	t.Setenv("PILOT_ROUNDS", "5") // the default: no override
	o, err := superviseOptionsFromArgs(flags)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"PILOT_DURATION": "5s"}; !reflect.DeepEqual(o.Overrides, want) || o.Params.Duration != 5*time.Second {
		t.Errorf("overrides %v and duration %v, want %v and 5s", o.Overrides, o.Params.Duration, want)
	}
	if o.EnvFile != "envtxt" || o.Engine != "e" || o.Loadgen != "l" || o.Work != "w" || o.Listen != ":9100" {
		t.Errorf("options %+v", o)
	}
	t.Setenv("PILOT_DURATION", "")
	if o, err := superviseOptionsFromArgs(flags); err != nil || len(o.Overrides) != 0 {
		t.Errorf("with no variable set: overrides %v, %v", o.Overrides, err)
	}
	t.Setenv("PILOT_DURATION", "soon")
	if _, err := superviseOptionsFromArgs(flags); err == nil || !strings.Contains(err.Error(), "PILOT_DURATION") {
		t.Errorf("a bad override: %v", err)
	}
}

func TestSupervisorMeasuresTheClockAtStart(t *testing.T) {
	read := func(t *testing.T, mutate func(*superviseOptions)) float64 {
		st := newSupervisorTest(t, mutate)
		st.ready = true
		_, body := st.request(t, http.MethodGet, "/host")
		var h hostRecord
		if err := json.Unmarshal([]byte(body), &h); err != nil {
			t.Fatal(err)
		}
		return h.ClockReadNS
	}
	if got := read(t, func(o *superviseOptions) { o.clockRead = func() float64 { return 42.5 } }); got != 42.5 {
		t.Errorf("the host record gives a clock cost of %v ns, want the 42.5 that was measured", got)
	}
	if got := read(t, func(o *superviseOptions) { o.clockRead = nil }); !(got > 0) {
		t.Errorf("a supervisor that measures its own clock reports %v ns, want a positive cost", got)
	}
}

// fakeTaskset writes a taskset that logs its arguments to a file and then
// runs the command it was given, as taskset does, and returns it and the log.
// The shell adds variables of its own to the environment, so env clears it
// again.
func fakeTaskset(t *testing.T) (script, log string) {
	t.Helper()
	dir := t.TempDir()
	script, log = filepath.Join(dir, "taskset"), filepath.Join(dir, "args")
	text := fmt.Sprintf("#!/bin/sh\necho \"$@\" > %s\nshift 2\nexec /usr/bin/env -i \"$@\"\n", log)
	if err := os.WriteFile(script, []byte(text), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, log
}

// writeScript tells the fake loadgen what each run does, and returns the path
// of the log of its arguments.
func writeScript(t *testing.T, runs map[string]fakeRun) string {
	t.Helper()
	return writeFakeScript(t, fakeScript{Runs: runs})
}

// writeFakeScript saves script where the fake loadgen finds it: in a new
// directory, which becomes the working directory of the test and so of the
// loadgens that its code starts. It returns the path of the log of their
// arguments.
func writeFakeScript(t *testing.T, script fakeScript) string {
	t.Helper()
	dir := t.TempDir()
	data, err := json.Marshal(script)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fakeScriptFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	return filepath.Join(dir, fakeLogFile)
}

// serving is a supervisor serving in the background: done is closed when
// serve has returned err.
type serving struct {
	done chan struct{}
	err  error
	url  string // of the control port
}

// serveAsync runs the supervisor, with its loopback run, until the test ends.
func (st *supervisorTest) serveAsync(t *testing.T) *serving {
	t.Helper()
	sv := &serving{done: make(chan struct{})}
	go func() {
		sv.err = st.serve("127.0.0.1:0")
		close(sv.done)
	}()
	t.Cleanup(func() {
		st.cancel()
		select {
		case <-sv.done:
		case <-time.After(30 * time.Second):
			t.Error("the supervisor did not stop")
		}
	})
	eventually(t, "the supervisor to listen", func() bool {
		line, _, _ := strings.Cut(st.out.String(), "\n")
		addr, ok := strings.CutPrefix(line, "listening on ")
		sv.url = "http://" + addr
		return ok
	})
	return sv
}

// host reads the host record of a supervisor that serves; it ends the test
// if the supervisor has stopped.
func (sv *serving) host(t *testing.T) hostRecord {
	t.Helper()
	select {
	case <-sv.done:
		t.Fatalf("the supervisor stopped: %v", sv.err)
	default:
	}
	_, body := get(t, sv.url+"/host")
	var h hostRecord
	if err := json.Unmarshal([]byte(body), &h); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return h
}

func (sv *serving) awaitReady(t *testing.T) hostRecord {
	t.Helper()
	var h hostRecord
	eventually(t, "the supervisor to be ready", func() bool {
		h = sv.host(t)
		return h.Ready
	})
	return h
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestSuperviseNeedsItsFlags(t *testing.T) {
	for _, args := range [][]string{
		{"supervise"},
		{"supervise", "-engine", "e", "-loadgen", "l"},
		{"supervise", "-engine", "e", "-work", "w"},
		{"supervise", "-loadgen", "l", "-work", "w"},
		{"supervise", "-engine", "e", "-loadgen", "l", "-work", "w", "extra"},
		{"nonsense"},
		{},
	} {
		if err := run(context.Background(), args, io.Discard); err == nil {
			t.Errorf("run(%v) succeeded", args)
		}
	}
}
