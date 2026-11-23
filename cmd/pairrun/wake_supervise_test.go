//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A meter that has no CPU is not started, and one that is stops with its
// context: nothing keeps measuring.
func TestWakeMeterStartAndStop(t *testing.T) {
	var sleeps atomic.Int64
	count := func(d time.Duration) { sleeps.Add(1); time.Sleep(d) }

	none := newWakeMeter(placeWake(fakeTopology(t, nil), []int{0}))
	none.sleep = count
	select {
	case <-none.start(context.Background()):
	case <-time.After(5 * time.Second):
		t.Fatal("a meter with no CPU did not report that it had ended")
	}
	if sleeps.Load() != 0 {
		t.Errorf("a meter with no CPU slept %d times", sleeps.Load())
	}

	var pinned atomic.Int64
	m := newWakeMeter(wakePlacement{CPU: 6, Siblings: []int{6}})
	m.sleep = count
	m.pin = func(cpu int) (func() error, error) {
		pinned.Store(int64(cpu))
		return func() error { return nil }, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := m.start(ctx)
	eventually(t, "the meter to sleep", func() bool { return sleeps.Load() > 3 })
	if p := m.placement(); !p.Pinned || pinned.Load() != 6 || p.Note != "" {
		t.Errorf("placement %+v after a pin of CPU %d, want it pinned and without a note", p, pinned.Load())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the meter did not stop with its context")
	}
	n := sleeps.Load()
	time.Sleep(20 * time.Millisecond)
	if sleeps.Load() != n {
		t.Errorf("the meter went on sleeping after its context ended: %d, then %d", n, sleeps.Load())
	}
}

// A pin that the system refuses is recorded and the meter measures unpinned.
func TestWakeMeterPinRefusedIsRecordedAndMeasuresOn(t *testing.T) {
	var sleeps atomic.Int64
	m := newWakeMeter(wakePlacement{CPU: 1, Siblings: []int{1}, Note: "earlier note"})
	m.sleep = func(d time.Duration) { sleeps.Add(1); time.Sleep(d) }
	m.pin = func(int) (func() error, error) { return nil, errors.New("operation not permitted") }
	ctx, cancel := context.WithCancel(context.Background())
	done := m.start(ctx)
	eventually(t, "the unpinned meter to sleep", func() bool { return sleeps.Load() > 3 })
	p := m.placement()
	cancel()
	<-done
	if p.Pinned || !strings.Contains(p.Note, "operation not permitted") || !strings.HasPrefix(p.Note, "earlier note; ") || p.CPU != 1 {
		t.Errorf("placement %+v, want it unpinned with the refusal added to the note", p)
	}
}

// The meter gives its thread its CPUs back as the last thing it does, on the
// thread that it pinned. The runtime parks the main thread of the process for
// good, rather than ends it, when the goroutine locked to it ends.
func TestWakeMeterPutsItsThreadsCPUsBackLast(t *testing.T) {
	var sleeps, restoredAfter, restores, pinTID, restoreTID atomic.Int64
	m := newWakeMeter(wakePlacement{CPU: 6, Siblings: []int{6}})
	m.sleep = func(d time.Duration) { sleeps.Add(1); time.Sleep(d) }
	m.pin = func(int) (func() error, error) {
		pinTID.Store(int64(unix.Gettid()))
		return func() error {
			restores.Add(1)
			restoredAfter.Store(sleeps.Load())
			restoreTID.Store(int64(unix.Gettid()))
			return nil
		}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := m.start(ctx)
	eventually(t, "the meter to sleep", func() bool { return sleeps.Load() > 3 })
	if restores.Load() != 0 {
		t.Errorf("the CPUs were put back %d times while the meter ran", restores.Load())
	}
	cancel()
	<-done
	if restores.Load() != 1 || restoreTID.Load() != pinTID.Load() {
		t.Errorf("the CPUs were put back %d times, on thread %d, want once on the pinned thread %d", restores.Load(), restoreTID.Load(), pinTID.Load())
	} else if restoredAfter.Load() != sleeps.Load() {
		t.Errorf("the meter slept %d times after it put its CPUs back", sleeps.Load()-restoredAfter.Load())
	}
	if p := m.placement(); !p.Pinned || p.Note != "" {
		t.Errorf("placement %+v, want it pinned and without a note", p)
	}
}

// A thread whose CPUs cannot be put back keeps the pin, and the placement says so.
func TestWakeMeterRestoreRefusedIsRecorded(t *testing.T) {
	m := newWakeMeter(wakePlacement{CPU: 1, Siblings: []int{1}, Note: "earlier note"})
	m.pin = func(int) (func() error, error) {
		return func() error { return errors.New("operation not permitted") }, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := m.start(ctx)
	eventually(t, "the meter to pin its thread", func() bool { return m.placement().Pinned })
	cancel()
	<-done
	p := m.placement()
	if !p.Pinned || !strings.HasPrefix(p.Note, "earlier note; ") || !strings.Contains(p.Note, "keeps the pin") || !strings.Contains(p.Note, "operation not permitted") {
		t.Errorf("placement %+v, want it pinned with the refusal to put the CPUs back added to the note", p)
	}
}

// pinThread puts the calling thread on one CPU and its restore gives the thread
// the CPUs it had.
func TestPinThreadRestoreGivesTheCPUsBack(t *testing.T) {
	allowed := cpusAllowed(t, "/proc/self/status")
	if len(allowed) < 2 {
		t.Skip("one CPU: a pinned thread cannot be told from an unpinned one")
	}
	cpu := allowed[len(allowed)-1]
	threadCPUs := func() []int {
		var set unix.CPUSet
		if err := unix.SchedGetaffinity(0, &set); err != nil {
			t.Fatal(err)
		}
		var cpus []int
		for c := range 64 * len(set) {
			if set.IsSet(c) {
				cpus = append(cpus, c)
			}
		}
		return cpus
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	before := threadCPUs()
	restore, err := pinThread(cpu)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restore() }() // a thread must not go back to the pool pinned
	if got := threadCPUs(); !slices.Equal(got, []int{cpu}) {
		t.Errorf("the thread may use CPUs %v after the pin, want CPU %d alone", got, cpu)
	}
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	if got := threadCPUs(); !slices.Equal(got, before) {
		t.Errorf("the thread may use CPUs %v after the restore, want %v as before the pin", got, before)
	}
}

// cpusAllowed reads the Cpus_allowed_list of /proc/<pid>/status, or of a
// thread's status file if given one.
func cpusAllowed(t *testing.T, status string) []int {
	t.Helper()
	cpus, err := allowedCPUs(status)
	if err != nil {
		t.Fatal(err)
	}
	return cpus
}

// pinnedThreads counts the threads of this process that may run on cpu alone.
func pinnedThreads(t *testing.T, cpu int) int {
	t.Helper()
	files, err := filepath.Glob("/proc/self/task/*/status")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, f := range files {
		// A thread may end between the listing and the read.
		if cpus, err := allowedCPUs(f); err == nil && slices.Equal(cpus, []int{cpu}) {
			n++
		}
	}
	return n
}

// The meter's pin is on its own thread: the processes that the supervisor
// starts while it runs, and the threads that the runtime starts, may use every
// CPU the supervisor may, and the pinned thread ends with the meter.
func TestWakeMeterPinDoesNotReachChildrenOrOtherThreads(t *testing.T) {
	allowed := cpusAllowed(t, "/proc/self/status")
	if len(allowed) < 2 {
		t.Skip("one CPU: a pinned thread cannot be told from an unpinned one")
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep to start as a child")
	}
	supervisor := cpusAllowed(t, "/proc/"+strconv.Itoa(os.Getpid())+"/status")
	cpu := allowed[len(allowed)-1]
	m := newWakeMeter(wakePlacement{CPU: cpu, Siblings: []int{cpu}})
	ctx, cancel := context.WithCancel(context.Background())
	done := m.start(ctx)
	defer func() { cancel(); <-done }()
	eventually(t, "the meter to pin its thread", func() bool { return m.placement().Pinned })
	if n := pinnedThreads(t, cpu); n != 1 {
		t.Fatalf("%d threads are pinned to CPU %d, want the meter's alone", n, cpu)
	}

	// Many children at once, started from many goroutines, so that the runtime
	// starts threads and some child is forked from every thread there is.
	const children = 48
	cmds := make([]*exec.Cmd, children)
	var wg sync.WaitGroup
	for i := range cmds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmds[i] = exec.Command(sleep, "30")
			if err := cmds[i].Start(); err != nil {
				t.Error(err)
				cmds[i] = nil
			}
		}()
	}
	wg.Wait()
	for _, cmd := range cmds {
		if cmd == nil {
			continue
		}
		if got := cpusAllowed(t, "/proc/"+strconv.Itoa(cmd.Process.Pid)+"/status"); !reflect.DeepEqual(got, supervisor) {
			t.Errorf("a child of the supervisor may use CPUs %v, want those of the supervisor, %v", got, supervisor)
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	if n := pinnedThreads(t, cpu); n != 1 {
		t.Errorf("%d threads are pinned to CPU %d after the children, want the meter's alone", n, cpu)
	}
	cancel()
	<-done
	eventually(t, "the meter's thread to end", func() bool { return pinnedThreads(t, cpu) == 0 })
	// The status file of the process gives the CPUs of its main thread, which
	// the runtime parks rather than ends if the meter ran on it.
	if got := cpusAllowed(t, "/proc/self/status"); !reflect.DeepEqual(got, supervisor) {
		t.Errorf("after the meter, the process may use CPUs %v by its status file, want %v", got, supervisor)
	}
}

// The meter does not run on the main thread: the CPUs of the process, as
// /proc/<pid>/status gives them, are those of the main thread, and are not the
// meter's one CPU while it runs. The main goroutine keeps that thread (main.go).
func TestWakeMeterDoesNotRunOnTheMainThread(t *testing.T) {
	allowed := cpusAllowed(t, "/proc/self/status")
	if len(allowed) < 2 {
		t.Skip("one CPU: a pinned thread cannot be told from an unpinned one")
	}
	process := "/proc/" + strconv.Itoa(os.Getpid()) + "/status"
	before := cpusAllowed(t, process)
	cpu := allowed[len(allowed)-1]
	var tid atomic.Int64
	m := newWakeMeter(wakePlacement{CPU: cpu, Siblings: []int{cpu}})
	m.pin = func(cpu int) (func() error, error) {
		tid.Store(int64(unix.Gettid()))
		return pinThread(cpu)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := m.start(ctx)
	defer func() { cancel(); <-done }()
	eventually(t, "the meter to pin its thread", func() bool { return m.placement().Pinned })
	if int(tid.Load()) == os.Getpid() {
		t.Errorf("the meter's thread, %d, is the main thread of the process", tid.Load())
	}
	if got := cpusAllowed(t, process); !reflect.DeepEqual(got, before) {
		t.Errorf("the process may use CPUs %v while the meter runs, want %v as before it started", got, before)
	}
}

func (st *supervisorTest) wakeRoute(t *testing.T, query string) (int, wakeAnswer, string) {
	t.Helper()
	code, body := st.request(t, http.MethodGet, "/wake"+query)
	var a wakeAnswer
	if code == http.StatusOK {
		if err := json.Unmarshal([]byte(body), &a); err != nil {
			t.Fatalf("%v: %s", err, body)
		}
	}
	return code, a, body
}

// /wake serves the finished seconds as columns, between from and to, with the
// supervisor's clock, the tick and the placement.
func TestSupervisorServesWake(t *testing.T) {
	st := newSupervisorTest(t)
	st.now = func() time.Time { return time.Unix(1_700_000_005, 123_456_789) }
	for _, sec := range []int64{100, 101, 104} {
		st.wake.record(unixSecond(sec, 0), time.Duration(sec-99)*10*time.Microsecond)
		st.wake.record(unixSecond(sec, 500*time.Millisecond), time.Duration(sec-99)*20*time.Microsecond)
	}
	st.wake.record(unixSecond(105, 0), 0) // finishes 104

	code, a, body := st.wakeRoute(t, "?from=100000&to=104000")
	if code != http.StatusOK {
		t.Fatalf("/wake answered %d %q", code, body)
	}
	if a.NowUnixNS != 1_700_000_005_123_456_789 || a.TickUS != 1000 {
		t.Errorf("now_unix_ns %d, tick_us %d, want the supervisor's clock and 1000", a.NowUnixNS, a.TickUS)
	}
	if a.T0 != 100 || !reflect.DeepEqual(a.N, []int{2, 2, 0, 0, 2}) ||
		!reflect.DeepEqual(a.P99, []float64{20, 40, 0, 0, 100}) || !reflect.DeepEqual(a.Max, []float64{20, 40, 0, 0, 100}) {
		t.Errorf("columns t0 %d, n %v, p99 %v, max %v, want the three seconds with the two gaps filled", a.T0, a.N, a.P99, a.Max)
	}
	// The fake host has 8 CPUs and no topology files.
	if p := a.Placement; p.CPU != 7 || !reflect.DeepEqual(p.Siblings, []int{7}) || p.Cores != 8 || p.TopologyReadable || p.Pinned || !strings.Contains(p.Note, "unreadable") {
		t.Errorf("placement %+v", p)
	}
	for _, want := range []string{`"t0":100,"n":[2,2,0,0,2],"p99":[20,40,0,0,100],"max":[20,40,0,0,100]`, `"placement":{"cpu":7,"siblings":[7],"cores":8,"topology_readable":false,"pinned":false,"note":`} {
		if !strings.Contains(body, want) {
			t.Errorf("the answer %s does not hold %s", body, want)
		}
	}

	// The bounds are the seconds' starts, both ends in; the second that is not finished is not served.
	for _, c := range []struct {
		query string
		t0    int64
		n     []int
	}{
		{"?from=100000&to=105000", 100, []int{2, 2, 0, 0, 2}},
		{"?from=100001&to=104000", 101, []int{2, 0, 0, 2}},
		{"?from=100000&to=103999", 100, []int{2, 2}},
		{"?from=101000&to=101000", 101, []int{2}},
		{"?from=102000&to=103999", 0, []int{}},
		{"?from=105000&to=900000", 0, []int{}},
		{"?to=104000&from=100000", 100, []int{2, 2, 0, 0, 2}},
	} {
		code, a, body := st.wakeRoute(t, c.query)
		if code != http.StatusOK || a.T0 != c.t0 || !reflect.DeepEqual(a.N, c.n) || len(a.P99) != len(c.n) || len(a.Max) != len(c.n) {
			t.Errorf("%s: %d, t0 %d, n %v, want t0 %d, n %v (%s)", c.query, code, a.T0, a.N, c.t0, c.n, body)
		}
	}
}

func TestSupervisorWakeRefusesABadRange(t *testing.T) {
	st := newSupervisorTest(t)
	for _, query := range []string{
		"", "?from=100", "?to=100", "?from=&to=100", "?from=100&to=", "?from=a&to=100", "?from=100&to=1.5", "?from=0x10&to=100",
		"?from=-1&to=100", "?from=100&to=-1", "?from=200&to=100", "?from=99999999999999999999&to=100", "?FROM=1&TO=2",
	} {
		if code, body := st.request(t, http.MethodGet, "/wake"+query); code != http.StatusBadRequest || body == "" {
			t.Errorf("/wake%s answered %d %q, want 400 and a reason", query, code, body)
		}
	}
}

// The supervisor's meter does not run during the loopback run, which spins
// loadgen on the machine's highest CPUs, starts when the supervisor is ready
// and has stopped, with nothing left running, when serve has returned.
func TestSupervisorStartsTheMeterWhenReadyAndStopsItWithItsContext(t *testing.T) {
	release := filepath.Join(t.TempDir(), "release")
	writeScript(t, map[string]fakeRun{"loopback": {Report: validReport(flowR1, 60, 70).JSON(), WaitFile: release}})
	st := newSupervisorTest(t)
	taskset, _ := fakeTaskset(t)
	st.lookPath = func(string) (string, error) { return taskset, nil }
	var sleeps atomic.Int64
	st.wake.sleep = func(d time.Duration) { sleeps.Add(1); time.Sleep(d) }
	sv := st.serveAsync(t)

	st.stub(t) // the loopback run is under way
	time.Sleep(100 * time.Millisecond)
	if n := sleeps.Load(); n != 0 {
		t.Errorf("the meter slept %d times during the loopback run", n)
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sv.awaitReady(t)
	eventually(t, "the meter to sleep once the supervisor is ready", func() bool { return sleeps.Load() > 3 })
	code, a, body := st.wakeRoute(t, "?from=0&to=0")
	if code != http.StatusOK || !a.Placement.Pinned || a.Placement.CPU != 7 {
		t.Errorf("/wake answered %d %s, want the placement of a meter that has pinned itself on CPU 7", code, body)
	}

	st.cancel()
	select {
	case <-sv.done:
	case <-time.After(30 * time.Second):
		t.Fatal("the supervisor did not stop")
	}
	n := sleeps.Load()
	time.Sleep(20 * time.Millisecond)
	if sleeps.Load() != n {
		t.Errorf("the meter was still sleeping after serve returned: %d, then %d", n, sleeps.Load())
	}
}

// serve does not return while the meter still runs, so that no meter outlives
// the supervisor that started it.
func TestSupervisorWaitsForTheMeterToEnd(t *testing.T) {
	writeScript(t, map[string]fakeRun{"loopback": {Report: validReport(flowR1, 60, 70).JSON()}})
	st := newSupervisorTest(t)
	taskset, _ := fakeTaskset(t)
	st.lookPath = func(string) (string, error) { return taskset, nil }
	var hold, held atomic.Bool
	release := make(chan struct{})
	st.wake.sleep = func(d time.Duration) {
		if hold.Load() {
			held.Store(true)
			<-release
		}
		time.Sleep(d)
	}
	sv := st.serveAsync(t)
	sv.awaitReady(t)
	hold.Store(true)
	eventually(t, "the meter to be held in a sleep", held.Load)
	st.cancel()
	select {
	case <-sv.done:
		t.Error("serve returned while the meter was still running")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case <-sv.done:
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not return once the meter ended")
	}
}

// With one CPU there is no meter: the placement says so and nothing sleeps.
func TestSupervisorWithOneCPUHasNoMeter(t *testing.T) {
	writeScript(t, map[string]fakeRun{"loopback": {Report: validReport(flowR1, 60, 70).JSON()}})
	st := newSupervisorTest(t, func(o *superviseOptions) { o.src = fakeSources(fakeProc(t, fakeHostFiles("3"))) })
	var sleeps atomic.Int64
	st.wake.sleep = func(d time.Duration) { sleeps.Add(1); time.Sleep(d) }
	st.serveAsync(t).awaitReady(t)
	time.Sleep(50 * time.Millisecond)
	code, a, body := st.wakeRoute(t, "?from=0&to=9999999999999")
	if code != http.StatusOK || a.Placement.CPU != -1 || a.Placement.Pinned || len(a.N) != 0 || !strings.Contains(a.Placement.Note, "1 CPU") {
		t.Errorf("/wake answered %d %s, want a placement with no CPU that says why, and no seconds", code, body)
	}
	if sleeps.Load() != 0 {
		t.Errorf("a supervisor with one CPU ran a meter: %d sleeps", sleeps.Load())
	}
}

// A host whose CPUs cannot be listed gets no meter either.
func TestSupervisorWithUnreadableCPUsHasNoMeter(t *testing.T) {
	files := fakeHostFiles("0-3")
	delete(files, "status")
	s, err := newSupervisor(context.Background(), superviseOptions{
		Engine: "e", Loadgen: "l", Work: t.TempDir(), src: fakeSources(fakeProc(t, files)), clockRead: func() float64 { return 25 },
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if p := s.wake.placement(); p.CPU != -1 || !strings.Contains(p.Note, "unreadable") || p.Siblings == nil {
		t.Errorf("placement %+v, want no CPU and a note that the CPUs are unreadable", p)
	}
}
