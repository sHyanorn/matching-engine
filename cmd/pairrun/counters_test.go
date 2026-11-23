package main

import (
	"encoding/json"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// The files as the kernel writes them.
const (
	statText = "cpu  254117 0 127248 2051319 5261 0 21364 3306 0 0\n" +
		"cpu0 63000 0 31000 512000 1300 0 5300 800 0 0\n" +
		"intr 1234567 0 0\nctxt 987654\nbtime 1769000000\nprocesses 4242\nprocs_running 1\nprocs_blocked 0\n"
	cpuStatV2Text = "usage_usec 1234567\nuser_usec 1000000\nsystem_usec 234567\n" +
		"nr_periods 100\nnr_throttled 7\nthrottled_usec 4500\nnr_bursts 0\nburst_usec 0\n"
	// v1 counts the time throttled in ns.
	cpuStatV1Text = "nr_periods 200\nnr_throttled 9\nthrottled_time 8765432100\nnr_bursts 0\nburst_time 0\n"
	pressureText  = "some avg10=0.19 avg60=0.70 avg300=2.65 total=210309781\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n"
)

// fakeThreads writes the threads of process pid into the proc directory of
// the sources that fakeSources makes of dir: one directory for each thread,
// with its schedstat text. A name in ended has a directory and no file, as a
// thread has that ends after the directory was listed.
func fakeThreads(t *testing.T, dir string, pid int, threads map[string]string, ended ...string) {
	t.Helper()
	task := filepath.Join(dir, "proc", strconv.Itoa(pid), "task")
	for tid, text := range threads {
		if err := os.MkdirAll(filepath.Join(task, tid), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(task, tid, "schedstat"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, tid := range ended {
		if err := os.MkdirAll(filepath.Join(task, tid), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// machineNames are the names of a reading without a process, in the order of
// the spec.
var machineNames = []string{cpuNrPeriods, cpuNrThrottled, cpuThrottledUS, cpuQuotaUS, cpuPeriodUS, psiCPUSomeUS, stealTicks}

var processNames = []string{schedRunNS, schedDelayNS, schedSlices, schedTasks}

// eachNameOnce fails the test unless every name is in exactly one of the
// reading's Values and Unreadable.
func eachNameOnce(t *testing.T, c counters, names ...string) {
	t.Helper()
	for _, name := range names {
		_, read := c.Values[name]
		_, not := c.Unreadable[name]
		if read == not {
			t.Errorf("%s: readable %v and unreadable %v, want exactly one", name, read, not)
		}
	}
}

// values fails the test unless the reading's Values are exactly want.
func values(t *testing.T, c counters, want map[string]int64) {
	t.Helper()
	if !maps.Equal(c.Values, want) {
		t.Errorf("values %v, want %v", c.Values, want)
	}
}

// unreadable fails the test unless each name is unreadable in c with a reason
// that holds part, and holds no value.
func unreadable(t *testing.T, c counters, part string, names ...string) {
	t.Helper()
	for _, name := range names {
		why, ok := c.Unreadable[name]
		if _, read := c.Values[name]; read || !ok || !strings.Contains(why, part) {
			t.Errorf("%s: value %v, unreadable %v with reason %q, want unreadable with a reason that holds %q", name, read, ok, why, part)
		}
	}
}

func source(t *testing.T, c counters, want map[string]string) {
	t.Helper()
	if !maps.Equal(c.Source, want) {
		t.Errorf("sources %v, want %v", c.Source, want)
	}
}

func TestReadCountersFromCgroupV2(t *testing.T) {
	dir := fakeProc(t, map[string]string{
		"stat": statText, "cpu.stat": cpuStatV2Text, "cpu.max": "200000 100000\n", "cpu.pressure": pressureText,
	})
	fakeThreads(t, dir, 4242, map[string]string{"4242": "1000 200 3\n", "4243": "2000 300 5\n", "4250": "500 0 1\n"})
	c := readCounters(fakeSources(dir), 4242)
	values(t, c, map[string]int64{
		schedRunNS: 3500, schedDelayNS: 500, schedSlices: 9, schedTasks: 3,
		cpuNrPeriods: 100, cpuNrThrottled: 7, cpuThrottledUS: 4500, cpuQuotaUS: 200000, cpuPeriodUS: 100000,
		psiCPUSomeUS: 210309781, stealTicks: 3306,
	})
	source(t, c, map[string]string{psiSource: "cgroup", cpuStatSource: cgroupV2, cpuLimitSource: cgroupV2})
	eachNameOnce(t, c, append(slices.Clone(processNames), machineNames...)...)
}

func TestReadCountersFromCgroupV1(t *testing.T) {
	dir := fakeProc(t, map[string]string{
		"stat": statText, "cpu.stat.v1": cpuStatV1Text, "cpu.cfs_quota_us": "-1\n", "cpu.cfs_period_us": "100000\n",
		"pressure.cpu": pressureText,
	})
	c := readCounters(fakeSources(dir), 0)
	values(t, c, map[string]int64{
		cpuNrPeriods: 200, cpuNrThrottled: 9, cpuThrottledUS: 8765432, // ns to us
		cpuQuotaUS: -1, cpuPeriodUS: 100000, psiCPUSomeUS: 210309781, stealTicks: 3306,
	})
	source(t, c, map[string]string{psiSource: "proc", cpuStatSource: cgroupV1, cpuLimitSource: cgroupV1})
	eachNameOnce(t, c, machineNames...)
	for _, name := range processNames {
		if _, ok := c.Values[name]; ok {
			t.Errorf("a reading without a process holds %s", name)
		}
		if _, ok := c.Unreadable[name]; ok {
			t.Errorf("a reading without a process lists %s as unreadable", name)
		}
	}
}

func TestReadCountersQuotaOfV1IsPositive(t *testing.T) {
	dir := fakeProc(t, map[string]string{"cpu.cfs_quota_us": "150000\n", "cpu.cfs_period_us": "50000\n"})
	values(t, readCounters(fakeSources(dir), 0), map[string]int64{cpuQuotaUS: 150000, cpuPeriodUS: 50000})
}

func TestReadCountersV2WinsOverV1(t *testing.T) {
	dir := fakeProc(t, map[string]string{
		"cpu.stat": cpuStatV2Text, "cpu.max": "max 100000\n",
		"cpu.stat.v1": cpuStatV1Text, "cpu.cfs_quota_us": "50000\n", "cpu.cfs_period_us": "10000\n",
		"cpu.pressure": "some avg10=0.00 avg60=0.00 avg300=0.00 total=11\n", "pressure.cpu": pressureText,
	})
	c := readCounters(fakeSources(dir), 0)
	if c.Values[cpuNrPeriods] != 100 || c.Values[cpuQuotaUS] != -1 || c.Values[cpuPeriodUS] != 100000 || c.Values[psiCPUSomeUS] != 11 {
		t.Errorf("values %v, want v2's, the quota max as -1 and the cgroup's pressure", c.Values)
	}
	source(t, c, map[string]string{psiSource: "cgroup", cpuStatSource: cgroupV2, cpuLimitSource: cgroupV2})
}

// A cgroup v2 whose cpu controller is off has a cpu.stat without the
// throttling lines, and no cpu.max: its names fall back to v1 one by one.
func TestReadCountersTakesEachNameFromTheVersionThatHasIt(t *testing.T) {
	dir := fakeProc(t, map[string]string{
		"cpu.stat": "usage_usec 5\nuser_usec 3\nsystem_usec 2\n", "cpu.stat.v1": cpuStatV1Text,
		"cpu.max": "max 100000\n", "cpu.cfs_quota_us": "-1\n", "cpu.cfs_period_us": "100000\n",
	})
	c := readCounters(fakeSources(dir), 0)
	values(t, c, map[string]int64{cpuNrPeriods: 200, cpuNrThrottled: 9, cpuThrottledUS: 8765432, cpuQuotaUS: -1, cpuPeriodUS: 100000})
	source(t, c, map[string]string{cpuStatSource: cgroupV1, cpuLimitSource: cgroupV2})

	dir = fakeProc(t, map[string]string{
		"cpu.stat": "usage_usec 5\nnr_periods 1\nnr_throttled 0\n", "cpu.stat.v1": "throttled_time 3000\n",
	})
	c = readCounters(fakeSources(dir), 0)
	if c.Source[cpuStatSource] != cgroupV1+"+"+cgroupV2 {
		t.Errorf("sources %v, want the stat names to say that both versions were used", c.Source)
	}
}

func TestReadCountersMissingFiles(t *testing.T) {
	c := readCounters(fakeSources(fakeProc(t, nil)), 4242)
	eachNameOnce(t, c, append(slices.Clone(processNames), machineNames...)...)
	if len(c.Values) != 0 || len(c.Source) != 0 {
		t.Errorf("nothing exists, yet values %v and sources %v", c.Values, c.Source)
	}
	unreadable(t, c, "4242", processNames...)
	unreadable(t, c, "cpu.stat", cpuNrPeriods, cpuNrThrottled, cpuThrottledUS)
	unreadable(t, c, "cpu.stat.v1", cpuNrPeriods, cpuNrThrottled, cpuThrottledUS)
	unreadable(t, c, "cpu.max", cpuQuotaUS, cpuPeriodUS)
	unreadable(t, c, "cpu.cfs_quota_us", cpuQuotaUS)
	unreadable(t, c, "cpu.cfs_period_us", cpuPeriodUS)
	unreadable(t, c, "cpu.pressure", psiCPUSomeUS)
	unreadable(t, c, "pressure.cpu", psiCPUSomeUS)
	unreadable(t, c, "stat", stealTicks)
}

func TestReadCountersFilesThatAreEmptyOrMalformed(t *testing.T) {
	cpuNames := []string{cpuNrPeriods, cpuNrThrottled, cpuThrottledUS}
	for _, c := range []struct {
		name   string
		files  map[string]string
		reason string // part of the reason of each of names
		names  []string
		read   map[string]int64 // what else is read
	}{
		{"empty cpu.stat", map[string]string{"cpu.stat": "", "cpu.stat.v1": "\n"}, ": empty", cpuNames, nil},
		{"cpu.stat without the lines", map[string]string{"cpu.stat": "usage_usec 5\n"}, "no nr_periods line", cpuNames[:1], nil},
		{"a number that is not one", map[string]string{"cpu.stat": "nr_periods abc\nnr_throttled 3\nthrottled_usec 9\n"},
			`nr_periods is "abc"`, cpuNames[:1], map[string]int64{cpuNrThrottled: 3, cpuThrottledUS: 9}},
		{"a negative count", map[string]string{"cpu.stat": "nr_periods -4\nnr_throttled 3\nthrottled_usec 9\n"},
			`nr_periods is "-4"`, cpuNames[:1], map[string]int64{cpuNrThrottled: 3, cpuThrottledUS: 9}},
		{"a line with a third field", map[string]string{"cpu.stat": "nr_periods 4 5\n"}, `"nr_periods 4 5"`, cpuNames[:1], nil},
		{"empty cpu.max", map[string]string{"cpu.max": " \n"}, ": empty", []string{cpuQuotaUS, cpuPeriodUS}, nil},
		{"cpu.max with one number", map[string]string{"cpu.max": "max\n"}, `found "max"`, []string{cpuQuotaUS, cpuPeriodUS}, nil},
		{"cpu.max with a quota that is not one", map[string]string{"cpu.max": "lots 100000\n"}, `the quota is "lots"`, []string{cpuQuotaUS, cpuPeriodUS}, nil},
		{"cpu.max with no period", map[string]string{"cpu.max": "max 0\n"}, `the period is "0"`, []string{cpuQuotaUS, cpuPeriodUS}, nil},
		{"v1 quota that is not -1 or positive", map[string]string{"cpu.cfs_quota_us": "-5\n", "cpu.cfs_period_us": "100000\n"},
			`the quota is "-5"`, []string{cpuQuotaUS}, map[string]int64{cpuPeriodUS: 100000}},
		{"v1 period that is not a number", map[string]string{"cpu.cfs_quota_us": "-1\n", "cpu.cfs_period_us": "x\n"},
			`the period is "x"`, []string{cpuPeriodUS}, map[string]int64{cpuQuotaUS: -1}},
		{"empty pressure file", map[string]string{"pressure.cpu": ""}, ": empty", []string{psiCPUSomeUS}, nil},
		{"pressure without a some line", map[string]string{"pressure.cpu": "full avg10=0.00 avg60=0.00 avg300=0.00 total=5\n"},
			"no some line", []string{psiCPUSomeUS}, nil},
		{"pressure without a total", map[string]string{"pressure.cpu": "some avg10=0.00 avg60=0.00 avg300=0.00\n"},
			"no total in the some line", []string{psiCPUSomeUS}, nil},
		{"pressure with a total that is not a number", map[string]string{"pressure.cpu": "some avg10=0.00 total=many\n"},
			`total is "many"`, []string{psiCPUSomeUS}, nil},
		{"empty /proc/stat", map[string]string{"stat": ""}, ": empty", []string{stealTicks}, nil},
		{"no cpu line", map[string]string{"stat": "cpu0 1 2 3 4 5 6 7 8\nintr 5\n"}, "no cpu line", []string{stealTicks}, nil},
		{"a cpu line that ends before steal", map[string]string{"stat": "cpu  1 2 3 4 5 6 7\n"}, `"cpu 1 2 3 4 5 6 7"`, []string{stealTicks}, nil},
		{"a steal that is not a number", map[string]string{"stat": "cpu  1 2 3 4 5 6 7 x 0 0\n"}, `steal is "x"`, []string{stealTicks}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := readCounters(fakeSources(fakeProc(t, c.files)), 0)
			unreadable(t, got, c.reason, c.names...)
			for name, want := range c.read {
				if v, ok := got.Values[name]; !ok || v != want {
					t.Errorf("%s: %d, %v, want %d: the other lines of the file are fine", name, v, ok, want)
				}
			}
		})
	}
}

// A steal of 0 is a reading, not a failure to read.
func TestReadCountersZeroStealIsAValue(t *testing.T) {
	c := readCounters(fakeSources(fakeProc(t, map[string]string{"stat": "cpu  1 2 3 4 5 6 7 0 9 10\n"})), 0)
	if v, ok := c.Values[stealTicks]; !ok || v != 0 {
		t.Errorf("steal %d, %v, want a value of 0", v, ok)
	}
}

func TestReadCountersPressureFallsBackToTheMachine(t *testing.T) {
	proc := "some avg10=0.00 avg60=0.00 avg300=0.00 total=77\n"
	for _, c := range []struct {
		name  string
		files map[string]string
		want  int64
		src   string
	}{
		{"the cgroup's file is missing", map[string]string{"pressure.cpu": proc}, 77, "proc"},
		{"the cgroup's file is empty", map[string]string{"cpu.pressure": "", "pressure.cpu": proc}, 77, "proc"},
		{"the cgroup's file has no some line", map[string]string{"cpu.pressure": "full avg10=0 total=1\n", "pressure.cpu": proc}, 77, "proc"},
		{"both are there", map[string]string{"cpu.pressure": pressureText, "pressure.cpu": proc}, 210309781, "cgroup"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := readCounters(fakeSources(fakeProc(t, c.files)), 0)
			if got.Values[psiCPUSomeUS] != c.want || got.Source[psiSource] != c.src {
				t.Errorf("psi %d from %q, want %d from %q", got.Values[psiCPUSomeUS], got.Source[psiSource], c.want, c.src)
			}
		})
	}
	got := readCounters(fakeSources(fakeProc(t, map[string]string{"cpu.pressure": "some avg10=0\n"})), 0)
	unreadable(t, got, "cgroup: ", psiCPUSomeUS)
	unreadable(t, got, "proc: ", psiCPUSomeUS)
	if _, ok := got.Source[psiSource]; ok {
		t.Errorf("a pressure that was not read has the source %q", got.Source[psiSource])
	}
}

func TestReadCountersSumsTheThreadsSchedstat(t *testing.T) {
	dir := fakeProc(t, nil)
	fakeThreads(t, dir, 7, map[string]string{"7": "10 1 2\n", "8": "20 2 4\n", "9": "30 3 8"}) // the last has no newline
	c := readCounters(fakeSources(dir), 7)
	for name, want := range map[string]int64{schedRunNS: 60, schedDelayNS: 6, schedSlices: 14, schedTasks: 3} {
		if c.Values[name] != want {
			t.Errorf("%s is %d, want %d", name, c.Values[name], want)
		}
	}
}

// Without scheduler statistics the kernel shows "0 0 1" for a thread.
func TestReadCountersKernelWithoutSchedulerStatistics(t *testing.T) {
	dir := fakeProc(t, nil)
	fakeThreads(t, dir, 7, map[string]string{"7": "0 0 1\n", "8": "0 0 1\n", "9": "0 0 1\n"})
	c := readCounters(fakeSources(dir), 7)
	unreadable(t, c, "no scheduler statistics", schedRunNS, schedDelayNS, schedSlices)
	if c.Values[schedTasks] != 3 {
		t.Errorf("the threads read: %d, want 3 even if the statistics are missing", c.Values[schedTasks])
	}
	if _, ok := c.Values[schedSlices]; ok {
		t.Error("the timeslices of a kernel without statistics are a value")
	}

	// One thread that ran is enough: the kernel keeps statistics.
	fakeThreads(t, dir, 7, map[string]string{"10": "1500 20 4\n"})
	c = readCounters(fakeSources(dir), 7)
	values(t, c, map[string]int64{schedRunNS: 1500, schedDelayNS: 20, schedSlices: 7, schedTasks: 4})
}

// A thread that ends after the directory was listed has a directory and no
// file; it is not an error and is not counted.
func TestReadCountersSkipsAThreadThatEnded(t *testing.T) {
	dir := fakeProc(t, nil)
	fakeThreads(t, dir, 7, map[string]string{"7": "10 1 2\n", "8": "20 2 4\n"}, "9", "10")
	c := readCounters(fakeSources(dir), 7)
	values(t, c, map[string]int64{schedRunNS: 30, schedDelayNS: 3, schedSlices: 6, schedTasks: 2})

	only := fakeProc(t, nil)
	fakeThreads(t, only, 7, nil, "7", "8")
	unreadable(t, readCounters(fakeSources(only), 7), "no thread", processNames...)
}

func TestGoneIsTheEndOfAThread(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{&fs.PathError{Op: "open", Path: "/proc/1/task/2/schedstat", Err: fs.ErrNotExist}, true},
		{&fs.PathError{Op: "read", Path: "/proc/1/task/2/schedstat", Err: syscall.ESRCH}, true},
		{&fs.PathError{Op: "open", Path: "/proc/1/task/2/schedstat", Err: fs.ErrPermission}, false},
		{nil, false},
	} {
		if got := gone(c.err); got != c.want {
			t.Errorf("gone(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestReadCountersThreadFileThatIsMalformed(t *testing.T) {
	for _, c := range []struct{ name, text, found string }{
		{"two numbers", "1 2\n", `"1 2"`},
		{"a word", "1 two 3\n", `"1 two 3"`},
		{"a negative number", "-1 0 3\n", `"-1 0 3"`},
		{"nothing", "\n", `""`},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := fakeProc(t, nil)
			fakeThreads(t, dir, 7, map[string]string{"7": "10 1 2\n", "8": c.text})
			got := readCounters(fakeSources(dir), 7)
			unreadable(t, got, c.found, processNames...)
			unreadable(t, got, filepath.Join("8", "schedstat"), processNames...)
		})
	}
}

func TestReadCountersWithoutAnEngine(t *testing.T) {
	dir := fakeProc(t, nil)
	fakeThreads(t, dir, 7, map[string]string{"7": "10 1 2\n"})
	c := readCounters(fakeSources(dir), 0)
	for _, name := range processNames {
		_, read := c.Values[name]
		_, not := c.Unreadable[name]
		if read || not {
			t.Errorf("%s is in a reading of no process", name)
		}
	}
}

// The real files of this machine: whatever they hold, or whether they exist,
// every name is read or says why not.
func TestReadCountersOfThisMachineLeavesNoNameOut(t *testing.T) {
	c := readCounters(procSources, os.Getpid())
	eachNameOnce(t, c, append(slices.Clone(processNames), machineNames...)...)
	for name, v := range c.Values {
		if v < 0 && name != cpuQuotaUS {
			t.Errorf("%s is %d", name, v)
		}
	}
}

func TestDelta(t *testing.T) {
	before := counters{
		Values: map[string]int64{
			schedRunNS: 1000, schedDelayNS: 100, schedSlices: 10, schedTasks: 12, cpuNrPeriods: 50, cpuNrThrottled: 2, cpuThrottledUS: 700,
			cpuQuotaUS: 200000, cpuPeriodUS: 100000, psiCPUSomeUS: 5000, stealTicks: 8,
		},
		Source: map[string]string{psiSource: "cgroup", cpuStatSource: cgroupV2, cpuLimitSource: cgroupV2},
	}
	after := counters{
		Values: map[string]int64{
			schedRunNS: 4000, schedDelayNS: 150, schedSlices: 16, schedTasks: 12, cpuNrPeriods: 80, cpuNrThrottled: 2, cpuThrottledUS: 700,
			cpuQuotaUS: 150000, cpuPeriodUS: 100000, psiCPUSomeUS: 5600, stealTicks: 9,
		},
		Source: map[string]string{psiSource: "cgroup", cpuStatSource: cgroupV2, cpuLimitSource: cgroupV2},
	}
	d := delta(before, after)
	want := map[string]int64{
		schedRunNS: 3000, schedDelayNS: 50, schedSlices: 6, schedTasks: 0, cpuNrPeriods: 30, cpuNrThrottled: 0, cpuThrottledUS: 0,
		psiCPUSomeUS: 600, stealTicks: 1,
	}
	if !maps.Equal(d.Values, want) || len(d.Unreadable) != 0 || len(d.Partial) != 0 {
		t.Errorf("delta %+v, want values %v and nothing else", d, want)
	}
	// The quota and period are settings: the later reading's, not a difference.
	if !maps.Equal(d.Settings.Values, map[string]int64{cpuQuotaUS: 150000, cpuPeriodUS: 100000}) ||
		!maps.Equal(d.Settings.Source, after.Source) {
		t.Errorf("settings %+v, want the quota and period of the later reading and its sources", d.Settings)
	}
}

func TestDeltaOfACounterUnreadableOnOneSide(t *testing.T) {
	read := counters{Values: map[string]int64{stealTicks: 5, psiCPUSomeUS: 10}}
	not := counters{
		Values:     map[string]int64{psiCPUSomeUS: 12},
		Unreadable: map[string]string{stealTicks: "open /proc/stat: no such file"},
	}
	for _, c := range []struct {
		name          string
		before, after counters
		reason        string
	}{
		{"after", read, not, "after: open /proc/stat: no such file"},
		{"before", not, read, "before: open /proc/stat: no such file"},
		{"both, the same reason", not, not, "open /proc/stat: no such file"},
		{"both, two reasons", not, counters{Unreadable: map[string]string{stealTicks: "empty"}}, "before: open /proc/stat: no such file; after: empty"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := delta(c.before, c.after)
			if d.Unreadable[stealTicks] != c.reason {
				t.Errorf("steal unreadable as %q, want %q", d.Unreadable[stealTicks], c.reason)
			}
			if _, ok := d.Values[stealTicks]; ok {
				t.Error("an unreadable counter has a difference")
			}
		})
	}
	if d := delta(read, not); d.Values[psiCPUSomeUS] != 2 {
		t.Errorf("the counter that both could read is %d, want 2", d.Values[psiCPUSomeUS])
	}
}

// A name that one reading lacks altogether, as the engine's when it has
// exited since the first reading, is not read.
func TestDeltaOfACounterThatOneReadingLacks(t *testing.T) {
	d := delta(counters{Values: map[string]int64{schedRunNS: 5, stealTicks: 1}}, counters{Values: map[string]int64{stealTicks: 2}})
	if d.Unreadable[schedRunNS] != "after: not read" || d.Values[stealTicks] != 1 {
		t.Errorf("delta %+v", d)
	}
	if d.Values[schedRunNS] != 0 {
		t.Error("a counter that was not read has a difference")
	}
}

func TestDeltaOfACounterThatWentDown(t *testing.T) {
	d := delta(
		counters{Values: map[string]int64{cpuNrPeriods: 90, stealTicks: 4}},
		counters{Values: map[string]int64{cpuNrPeriods: 10, stealTicks: 6}})
	if d.Unreadable[cpuNrPeriods] != "went down" {
		t.Errorf("a counter that went down is %q", d.Unreadable[cpuNrPeriods])
	}
	if _, ok := d.Values[cpuNrPeriods]; ok {
		t.Error("a counter that went down has a difference")
	}
	if d.Values[stealTicks] != 2 {
		t.Errorf("another counter is %d, want 2", d.Values[stealTicks])
	}
}

// A counter that stays where it was has a difference of 0, which is a reading.
func TestDeltaOfACounterThatStoodStill(t *testing.T) {
	d := delta(counters{Values: map[string]int64{cpuNrThrottled: 3}}, counters{Values: map[string]int64{cpuNrThrottled: 3}})
	if v, ok := d.Values[cpuNrThrottled]; !ok || v != 0 {
		t.Errorf("difference %d, %v, want 0", v, ok)
	}
}

// Readings of two different sources cannot be subtracted.
func TestDeltaOfPressureFromTwoSources(t *testing.T) {
	d := delta(
		counters{Values: map[string]int64{psiCPUSomeUS: 100}, Source: map[string]string{psiSource: "cgroup"}},
		counters{Values: map[string]int64{psiCPUSomeUS: 900}, Source: map[string]string{psiSource: "proc"}})
	if d.Unreadable[psiCPUSomeUS] != "source changed from cgroup to proc" {
		t.Errorf("pressure %+v", d)
	}
}

// The threads of a process that end take their counts with them: the sums fall
// short, and the difference says so.
func TestDeltaOfThreadsThatEndedIsPartial(t *testing.T) {
	read := func(threads map[string]string) counters {
		dir := fakeProc(t, nil)
		fakeThreads(t, dir, 7, threads)
		return readCounters(fakeSources(dir), 7)
	}
	before := read(map[string]string{"7": "1000 10 5\n", "8": "1000 10 5\n", "9": "100 1 1\n"})
	after := read(map[string]string{"7": "3000 30 9\n", "8": "2000 20 8\n"}) // thread 9 ended with 100 ns of run time
	d := delta(before, after)
	if want := []string{schedDelayNS, schedRunNS, schedSlices}; !slices.Equal(d.Partial, want) {
		t.Errorf("partial %v, want %v", d.Partial, want)
	}
	if d.Values[schedRunNS] != 2900 || d.Values[schedTasks] != -1 {
		t.Errorf("run %d and threads %d, want 2900 and -1: fewer threads is a difference and not a counter that went down",
			d.Values[schedRunNS], d.Values[schedTasks])
	}

	// More threads, or as many, make the sums whole.
	for _, threads := range []map[string]string{
		{"7": "3000 30 9\n", "8": "2000 20 8\n", "9": "200 2 2\n"},
		{"7": "3000 30 9\n", "8": "2000 20 8\n", "9": "200 2 2\n", "10": "50 0 1\n"},
	} {
		if d := delta(before, read(threads)); len(d.Partial) != 0 {
			t.Errorf("%d threads after 3 gave partial %v", len(threads), d.Partial)
		}
	}
}

// A lower bound may even be below zero, when the threads that ended had run
// longer than the others did meanwhile: that is not a difference to report.
func TestDeltaOfEndedThreadsThatLeavesANegativeSum(t *testing.T) {
	d := delta(
		counters{Values: map[string]int64{schedRunNS: 9000, schedDelayNS: 10, schedSlices: 5, schedTasks: 4}},
		counters{Values: map[string]int64{schedRunNS: 1000, schedDelayNS: 20, schedSlices: 6, schedTasks: 2}})
	if d.Unreadable[schedRunNS] != "went down" || d.Values[schedDelayNS] != 10 {
		t.Errorf("delta %+v", d)
	}
	if want := []string{schedDelayNS, schedSlices}; !slices.Equal(d.Partial, want) {
		t.Errorf("partial %v, want %v: the names that have a difference", d.Partial, want)
	}
}

func TestDeltaSerialises(t *testing.T) {
	d := delta(
		counters{Values: map[string]int64{schedRunNS: 1, schedTasks: 3, cpuQuotaUS: -1}, Source: map[string]string{psiSource: "proc"}},
		counters{Values: map[string]int64{schedRunNS: 5, schedTasks: 2, cpuQuotaUS: -1}, Source: map[string]string{psiSource: "proc"},
			Unreadable: map[string]string{stealTicks: "no cpu line"}})
	data, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"values":{"sched_run_ns":4,"sched_tasks":-1},"unreadable":{"steal_ticks":"before: not read; after: no cpu line"},` +
		`"partial":["sched_run_ns"],"settings":{"values":{"cpu_quota_us":-1},"source":{"psi_source":"proc"}}}`
	if string(data) != want {
		t.Errorf("delta as JSON\n%s\nwant\n%s", data, want)
	}
	fail, _ := json.Marshal(unreadableDelta("before: 404 Not Found"))
	if string(fail) != `{"unreadable":{"*":"before: 404 Not Found"}}` {
		t.Errorf("a reading that failed as a whole: %s", fail)
	}
}

func TestReadableNames(t *testing.T) {
	deltas := []*deltaResult{
		{
			Values: map[string]int64{stealTicks: 1, cpuNrPeriods: 5}, Unreadable: map[string]string{psiCPUSomeUS: "no file", cpuThrottledUS: "empty"},
			Settings: counters{Unreadable: map[string]string{cpuQuotaUS: "no file", cpuPeriodUS: "no file"}},
		},
		{Unreadable: map[string]string{allUnreadable: "404"}}, // the whole reading failed: nothing was read, and nothing is named
		{
			Values: map[string]int64{stealTicks: 2}, Unreadable: map[string]string{cpuNrPeriods: "went down", psiCPUSomeUS: "no file", cpuThrottledUS: "no file"},
			Settings: counters{Values: map[string]int64{cpuQuotaUS: -1}, Unreadable: map[string]string{cpuPeriodUS: "malformed"}},
		},
	}
	got := readableNames(deltas)
	if want := []string{cpuNrPeriods, cpuQuotaUS, stealTicks}; !slices.Equal(got.Readable, want) {
		t.Errorf("readable %v, want %v", got.Readable, want)
	}
	if want := []string{cpuPeriodUS, cpuThrottledUS, psiCPUSomeUS}; !slices.Equal(got.Unreadable, want) {
		t.Errorf("unreadable in all runs %v, want %v", got.Unreadable, want)
	}
}

func TestReadableNamesOfRunsThatNeverRead(t *testing.T) {
	whole := &deltaResult{Unreadable: map[string]string{allUnreadable: "404"}}
	if got := readableNames([]*deltaResult{whole, whole}); len(got.Readable) != 0 || !slices.Equal(got.Unreadable, []string{allUnreadable}) {
		t.Errorf("two readings that failed as a whole: %+v, want no names and \"*\" unreadable", got)
	}
	// Both lists are arrays, never null, whatever the runs.
	data, err := json.Marshal(readableCounters(nil))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"engine":{"readable":[],"unreadable":[]},"client":{"readable":[],"unreadable":[]}}`; string(data) != want {
		t.Errorf("no runs: %s, want %s", data, want)
	}
}

func TestReadableCountersTellsTheMachinesApart(t *testing.T) {
	records := []runRecord{
		{EngineDelta: &deltaResult{Values: map[string]int64{schedRunNS: 1}}, ClientDelta: &deltaResult{Unreadable: map[string]string{stealTicks: "x"}}},
		{}, // a run whose engine was not started has neither
		{EngineDelta: &deltaResult{Values: map[string]int64{schedRunNS: 2}}, ClientDelta: &deltaResult{Values: map[string]int64{stealTicks: 3}}},
	}
	got := readableCounters(records)
	if !slices.Equal(got.Engine.Readable, []string{schedRunNS}) || len(got.Engine.Unreadable) != 0 {
		t.Errorf("engine %+v", got.Engine)
	}
	if !slices.Equal(got.Client.Readable, []string{stealTicks}) || len(got.Client.Unreadable) != 0 {
		t.Errorf("client %+v: steal was read in one run", got.Client)
	}
}
