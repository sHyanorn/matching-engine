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
	o := superviseOptions{
		Engine:   fakeBinary(t, "fake-engine"),
		Loadgen:  fakeBinary(t, "fake-loadgen"),
		Work:     filepath.Join(t.TempDir(), "work"),
		Params:   defaultParams,
		httpAddr: freeAddr(t),
		oeAddr:   freeAddr(t),
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

// writeScript tells the fake loadgen what each run does and where to log its
// arguments, and returns the log's path.
func writeScript(t *testing.T, runs map[string]fakeRun) string {
	t.Helper()
	dir := t.TempDir()
	data, err := json.Marshal(fakeScript{Runs: runs})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "script.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "loadgen.log")
	t.Setenv("FAKE_LOADGEN_SCRIPT", path)
	t.Setenv("FAKE_LOADGEN_LOG", log)
	return log
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
