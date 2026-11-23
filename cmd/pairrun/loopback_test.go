//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestLoopbackRunsBeforeReady(t *testing.T) {
	st := newSupervisorTest(t, func(o *superviseOptions) {
		o.Params = params{Rate: 2000, RateHigh: 3000, Warmup: time.Second, Duration: 3 * time.Second, Long: 5 * time.Second, MaxSendLag: 20 * time.Microsecond, Rounds: 5}
	})
	release := filepath.Join(t.TempDir(), "release")
	report := validReport(flowR1, 60, 70).JSON()
	log := writeScript(t, map[string]fakeRun{"loopback": {Report: report, WaitFile: release, Text: "loopback table\n", Exit: 1}})
	taskset, tasksetLog := fakeTaskset(t)
	st.lookPath = func(name string) (string, error) {
		if name != "taskset" {
			t.Errorf("looked up %q, want taskset", name)
		}
		return taskset, nil
	}
	sv := st.serveAsync(t)

	// While loadgen waits, the engine runs, restricted, and the supervisor is not ready.
	rep, dir := st.stub(t)
	h := sv.host(t)
	if h.Ready || h.Config != "default" || h.Starts != 1 || h.EnginePID == 0 {
		t.Errorf("during the loopback run: %+v, want not ready, default, starts 1, an engine", h)
	}
	resp, err := http.Post(sv.url+"/start?config=default", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("/start during the loopback run answered %d, want 503", resp.StatusCode)
	}
	if want := []string{"-addr", st.httpAddr, "-oe-addr", st.oeAddr, "-data", dir}; !reflect.DeepEqual(rep.Args, want) {
		t.Errorf("loopback engine args %v, want %v", rep.Args, want)
	}
	if len(rep.Env) != 0 {
		t.Errorf("loopback engine environment %v, want empty", rep.Env)
	}
	tasksetArgs, err := os.ReadFile(tasksetLog)
	if err != nil {
		t.Fatal(err)
	}
	// 8 CPUs allowed: loadgen takes the two highest, the engine the other six.
	if want := fmt.Sprintf("-c 0-5 %s -addr %s -oe-addr %s -data %s\n", st.engineBin, st.httpAddr, st.oeAddr, dir); string(tasksetArgs) != want {
		t.Errorf("taskset got %q, want %q", tasksetArgs, want)
	}

	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	h = sv.awaitReady(t)
	// The loopback engine is gone, its start is counted, and its files are served.
	if h.EnginePID != 0 || h.Config != "" || h.Starts != 1 {
		t.Errorf("after the loopback run: %+v", h)
	}
	if entries, _ := filepath.Glob(filepath.Join(st.work, "data-*")); len(entries) != 0 {
		t.Errorf("the loopback run left %v", entries)
	}
	if code, body := get(t, sv.url+"/loopback.json"); code != http.StatusOK || body != string(report) {
		t.Errorf("/loopback.json answered %d %q, want the report loadgen wrote", code, body)
	}
	if code, body := get(t, sv.url+"/loopback.txt"); code != http.StatusOK || body != "loopback table\n" {
		t.Errorf("/loopback.txt answered %d %q", code, body)
	}
	// One R1 run as the matrix's R1, at the overridden rate and durations.
	logged, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	if err := json.Unmarshal(logged, &args); err != nil {
		t.Fatalf("%v: %s", err, logged)
	}
	wantArgs := []string{"-oe", dialAddr(st.oeAddr), "-ioc", "-http", dialAddr(st.httpAddr), "-max-send-lag", "20us",
		"-rate", "2000", "-warmup", "1s", "-duration", "3s", "-out", filepath.Join(st.work, "loopback.json")}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("loopback loadgen args %v, want %v", args, wantArgs)
	}

	// It serves on: an engine started now is the second start, and a signal stops it.
	resp, err = http.Post(sv.url+"/start?config=spin-pin", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/start after the loopback run answered %d", resp.StatusCode)
	}
	select {
	case <-sv.done:
		t.Fatalf("the supervisor stopped: %v", sv.err)
	default:
	}
	st.cancel()
	<-sv.done
	if sv.err != nil {
		t.Errorf("stopping the supervisor: %v", sv.err)
	}
	st.mu.Lock()
	running, starts := st.cmd != nil, st.starts
	st.mu.Unlock()
	if running || starts != 2 {
		t.Errorf("after the signal: engine running %v, starts %d, want the engine stopped and 2 starts", running, starts)
	}
	if entries, _ := filepath.Glob(filepath.Join(st.work, "data-*")); len(entries) != 0 {
		t.Errorf("stopping the supervisor left %v", entries)
	}
}

func TestLoopbackKeepsTheEngineOffLoadgensCPUs(t *testing.T) {
	if _, err := exec.LookPath("taskset"); err != nil {
		t.Skip("no taskset")
	}
	allowed, err := allowedCPUs("/proc/self/status")
	if err != nil || len(allowed) < 3 {
		t.Skipf("allowed CPUs %v, %v: the loopback run restricts the engine only with 3 or more", allowed, err)
	}
	release := filepath.Join(t.TempDir(), "release")
	writeScript(t, map[string]fakeRun{"loopback": {Report: validReport(flowR1, 60, 70).JSON(), WaitFile: release}})
	st := newSupervisorTest(t, func(o *superviseOptions) { o.src = procSources })
	sv := st.serveAsync(t)
	rep, _ := st.stub(t)
	got, err := parseCPUList(rep.CPUsAllowed)
	if err != nil {
		t.Fatal(err)
	}
	// Loadgen pins to the two highest CPUs it may use: the engine may use all the others.
	if want := allowed[:len(allowed)-2]; !slices.Equal(got, want) {
		t.Errorf("the engine was allowed CPUs %v, want %v of %v", got, want, allowed)
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sv.awaitReady(t)
}

func TestLoopbackWithFewCPUsIsNotRestricted(t *testing.T) {
	writeScript(t, map[string]fakeRun{"loopback": {Report: validReport(flowR1, 60, 70).JSON()}})
	st := newSupervisorTest(t, func(o *superviseOptions) { o.src = fakeSources(fakeProc(t, fakeHostFiles("0-1"))) })
	st.lookPath = func(string) (string, error) { return "", errors.New("taskset is not needed with 2 CPUs") }
	st.serveAsync(t).awaitReady(t)
}

func TestLoopbackThatFindsTheRunInvalidStillSetsReady(t *testing.T) {
	invalid := cannedReport{Valid: false, Problems: []string{"achieved 100/s, below 99% of the target"}, Target: 10000, Achieved: 100}.JSON()
	writeScript(t, map[string]fakeRun{"loopback": {Report: invalid, Exit: 1, Text: "invalid\n"}})
	st := newSupervisorTest(t)
	taskset, _ := fakeTaskset(t)
	st.lookPath = func(string) (string, error) { return taskset, nil }
	sv := st.serveAsync(t)
	sv.awaitReady(t)
	if code, body := get(t, sv.url+"/loopback.json"); code != http.StatusOK || body != string(invalid) {
		t.Errorf("/loopback.json answered %d %q", code, body)
	}
}

func TestLoopbackStartupFailures(t *testing.T) {
	for _, c := range []struct {
		name   string
		mutate func(*superviseOptions)
		after  func(*supervisorTest)
		want   string
	}{
		{"no taskset", nil, func(st *supervisorTest) {
			st.lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
		}, "taskset"},
		{"loadgen that cannot start", func(o *superviseOptions) { o.Loadgen = "/nonexistent/loadgen" }, nil, "starting loadgen"},
		{"engine that never listens", func(o *superviseOptions) { o.Engine = fakeBinary(t, "fake-engine-mute") }, func(st *supervisorTest) {
			st.readyWait = 200 * time.Millisecond
		}, "did not listen"},
		{"unreadable host", func(o *superviseOptions) {
			o.src = fakeSources(fakeProc(t, map[string]string{"meminfo": meminfo16G}))
		}, nil, "status"},
	} {
		t.Run(c.name, func(t *testing.T) {
			writeScript(t, map[string]fakeRun{"loopback": {Report: validReport(flowR1, 60, 70).JSON()}})
			var mutate []func(*superviseOptions)
			if c.mutate != nil {
				mutate = append(mutate, c.mutate)
			}
			st := newSupervisorTest(t, mutate...)
			taskset, _ := fakeTaskset(t)
			st.lookPath = func(string) (string, error) { return taskset, nil }
			if c.after != nil {
				c.after(st)
			}
			err := st.serve("127.0.0.1:0")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("serve returned %v, want an error with %q", err, c.want)
			}
			if st.isReady() {
				t.Error("the supervisor is ready after a startup failure")
			}
			st.mu.Lock()
			running := st.cmd != nil
			st.mu.Unlock()
			if running {
				t.Error("an engine is still running after the failure")
			}
		})
	}
}

// The Pod's API key is in the environment of the process that starts the
// supervisor unless the start command removes it; loadgen needs none of it.
func TestLoopbackStartsLoadgenWithoutTheSupervisorsEnvironment(t *testing.T) {
	t.Setenv("RUNPOD_API_KEY", "canary-supervisor")
	st := newSupervisorTest(t, func(o *superviseOptions) { o.Loadgen = fakeBinary(t, "fake-loadgen-env") })
	report := filepath.Join(st.work, "loopback.json")
	if err := st.runLoopbackLoadgen(report, filepath.Join(st.work, "loopback.txt")); err != nil {
		t.Fatal(err)
	}
	for _, name := range namesOfFakeLoadgenEnv(t, report) {
		t.Errorf("loadgen got %s of the supervisor's environment", name)
	}
}

func TestLoopbackIgnoresAStaleReport(t *testing.T) {
	// A report from an earlier supervisor in the same work directory must not pass for this run's.
	writeScript(t, map[string]fakeRun{"loopback": {NoReport: true, Exit: 1}})
	st := newSupervisorTest(t)
	taskset, _ := fakeTaskset(t)
	st.lookPath = func(string) (string, error) { return taskset, nil }
	if err := os.WriteFile(filepath.Join(st.work, "loopback.json"), []byte(`{"stale": true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	sv := st.serveAsync(t)
	sv.awaitReady(t)
	if code, body := get(t, sv.url+"/loopback.json"); code != http.StatusNotFound {
		t.Errorf("/loopback.json answered %d %q, want 404: loadgen wrote no report", code, body)
	}
}
