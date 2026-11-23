package main

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// The counters of a reading, named with their unit, and the settings and the
// sources that go with them. They tell whether the CPUs of a run were
// contended: whether the engine's threads waited for a CPU, whether the
// container's quota throttled it, or whether the host took the CPU away from
// the whole machine.
//
// Not taken: loadavg beyond the machine record's (smoothed over minutes), context switch counts (voluntary ones follow load, involuntary ones repeat the delay), other columns of /proc/stat (they say how busy the machine was, not who waited).
const (
	// The engine process, summed over its threads: the time they ran and
	// waited on a run queue, in ns, and their timeslices.
	schedRunNS   = "sched_run_ns"
	schedDelayNS = "sched_delay_ns"
	schedSlices  = "sched_slices"
	// schedTasks is the number of threads read. A thread that ends takes its
	// counts with it (the engine's spin mode ends the threads of a session), so
	// when it falls between two readings the differences of the three above are
	// lower bounds, which delta marks as partial. A count that did not fall can
	// still hide an ended thread, if another began.
	schedTasks = "sched_tasks"

	// The container's CPU quota, from cgroup v2 or else v1. The quota and the
	// period are settings, not counters: no difference is taken of them. A
	// quota of -1 means none.
	cpuNrPeriods   = "cpu_nr_periods"
	cpuNrThrottled = "cpu_nr_throttled"
	cpuThrottledUS = "cpu_throttled_us"
	cpuQuotaUS     = "cpu_quota_us"
	cpuPeriodUS    = "cpu_period_us"

	// psiCPUSomeUS is the time in us in which some task wanted a CPU and none
	// was free, from the cgroup's cpu.pressure or else the machine's, which
	// counts the tasks of every container. psiSource says which.
	psiCPUSomeUS = "psi_cpu_some_us"
	psiSource    = "psi_source"

	// stealTicks is the time in USER_HZ ticks that the host ran something else
	// while this machine had work. A zero does not show that the host took
	// nothing: a host that does not report it leaves it at 0.
	stealTicks = "steal_ticks"

	// The source of the cgroup's CPU counters and settings, cgroupV2 or
	// cgroupV1.
	cpuStatSource  = "cpu_stat_source"
	cpuLimitSource = "cpu_limit_source"
	cgroupV2       = "cgroup_v2"
	cgroupV1       = "cgroup_v1"

	// allUnreadable is the name under which a reading that failed as a whole
	// is recorded.
	allUnreadable = "*"
)

// settingNames are the counters' neighbours that are not counters.
var settingNames = []string{cpuQuotaUS, cpuPeriodUS}

// sourceOf names the source that a counter's value depends on: two readings
// whose sources differ cannot be subtracted.
var sourceOf = map[string]string{
	psiCPUSomeUS: psiSource, cpuNrPeriods: cpuStatSource, cpuNrThrottled: cpuStatSource, cpuThrottledUS: cpuStatSource,
}

// counters is one reading. Each name is in exactly one of Values and
// Unreadable; a counter that cannot be read is never recorded as 0.
type counters struct {
	Values     map[string]int64  `json:"values,omitempty"`
	Unreadable map[string]string `json:"unreadable,omitempty"` // name -> why: file missing, malformed (what was found), no statistics
	Source     map[string]string `json:"source,omitempty"`     // where a value came from, e.g. psi_source
}

func (c *counters) fail(reason string, names ...string) {
	for _, name := range names {
		c.Unreadable[name] = reason
	}
}

// readCounters takes one reading. With a pid it includes the counters of that
// process's threads; with 0 it does not. Reading never fails as a whole: each
// file is read on its own, and a file that is missing, empty or malformed
// makes only its own names unreadable. A reading is a handful of small files
// and takes no lock of the engine; its threads are read, not stopped.
func readCounters(src sources, pid int) counters {
	c := counters{Values: map[string]int64{}, Unreadable: map[string]string{}, Source: map[string]string{}}
	if pid > 0 {
		c.readSched(src.procDir, pid)
	}
	c.readCgroupCPU(src)
	c.readPressure(src)
	if steal, err := readText(src.procStat).steal(); err != nil {
		c.fail(err.Error(), stealTicks)
	} else {
		c.Values[stealTicks] = steal
	}
	return c
}

// readSched sums the schedstat of each thread of pid.
func (c *counters) readSched(procDir string, pid int) {
	names := []string{schedRunNS, schedDelayNS, schedSlices, schedTasks}
	dir := filepath.Join(procDir, strconv.Itoa(pid), "task")
	entries, err := os.ReadDir(dir)
	if err != nil {
		c.fail(err.Error(), names...)
		return
	}
	var run, delay, timeslices, tasks int64
	for _, e := range entries {
		file := filepath.Join(dir, e.Name(), "schedstat")
		data, err := os.ReadFile(file)
		if gone(err) {
			continue // the thread ended since the directory was listed
		}
		var r, d, s int64
		if err == nil {
			r, d, s, err = parseSchedstat(file, string(data))
		}
		if err != nil {
			c.fail(err.Error(), names...)
			return
		}
		run, delay, timeslices, tasks = run+r, delay+d, timeslices+s, tasks+1
	}
	if tasks == 0 {
		c.fail("no thread of the process could be read in "+dir, names...)
		return
	}
	c.Values[schedTasks] = tasks
	if run == 0 {
		// A kernel without scheduler statistics shows "0 0 1": no run time
		// and one timeslice.
		c.fail("no scheduler statistics: the run time of every thread is 0", schedRunNS, schedDelayNS, schedSlices)
		return
	}
	c.Values[schedRunNS], c.Values[schedDelayNS], c.Values[schedSlices] = run, delay, timeslices
}

// gone reports whether err says that the thread whose file was read has
// ended: the kernel answers ENOENT to an open and ESRCH to a read.
func gone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}

// parseSchedstat reads the three numbers of a thread's schedstat: run time
// and run-queue delay in ns, and the timeslices.
func parseSchedstat(file, text string) (run, delay, timeslices int64, err error) {
	fields := strings.Fields(text)
	if len(fields) < 3 {
		return 0, 0, 0, fmt.Errorf("%s: found %s, want three numbers", file, found(text))
	}
	var v [3]int64
	for i := range v {
		if v[i], err = strconv.ParseInt(fields[i], 10, 64); err != nil || v[i] < 0 {
			return 0, 0, 0, fmt.Errorf("%s: found %s, want three numbers", file, found(text))
		}
	}
	return v[0], v[1], v[2], nil
}

// candidate is a place that a value may be read from.
type candidate struct {
	source string
	read   func() (int64, error)
}

// pick sets name from the first candidate that reads and returns its source.
// If none does, name is unreadable with the reason of each and the source
// is "".
func (c *counters) pick(name string, candidates ...candidate) string {
	var reasons []string
	for _, cand := range candidates {
		v, err := cand.read()
		if err == nil {
			c.Values[name] = v
			return cand.source
		}
		reasons = append(reasons, cand.source+": "+err.Error())
	}
	c.Unreadable[name] = strings.Join(reasons, "; ")
	return ""
}

// setSource records where the values of a group came from: the source they
// share, or the sources joined by "+" in the case that a cgroup shows its
// files of one version and some of the other.
func (c *counters) setSource(key string, used ...string) {
	used = slices.DeleteFunc(used, func(s string) bool { return s == "" })
	slices.Sort(used)
	if used = slices.Compact(used); len(used) > 0 {
		c.Source[key] = strings.Join(used, "+")
	}
}

// readCgroupCPU reads the container's quota, period and throttling, each
// name from cgroup v2 first and then from v1.
func (c *counters) readCgroupCPU(src sources) {
	statV2, statV1 := readText(src.cpuStatV2), readText(src.cpuStatV1)
	maxV2 := readText(src.cpuMaxV2)
	quotaV1, periodV1 := readText(src.cfsQuotaV1), readText(src.cfsPeriodV1)

	var stat []string
	for _, s := range []struct {
		name, keyV2, keyV1 string
		perV1              int64 // the unit of v1's value per us
	}{
		{cpuNrPeriods, "nr_periods", "nr_periods", 1},
		{cpuNrThrottled, "nr_throttled", "nr_throttled", 1},
		{cpuThrottledUS, "throttled_usec", "throttled_time", 1000}, // v1 counts ns
	} {
		stat = append(stat, c.pick(s.name,
			candidate{cgroupV2, func() (int64, error) { return statV2.stat(s.keyV2) }},
			candidate{cgroupV1, func() (int64, error) { v, err := statV1.stat(s.keyV1); return v / s.perV1, err }}))
	}
	c.setSource(cpuStatSource, stat...)

	quota := c.pick(cpuQuotaUS,
		candidate{cgroupV2, func() (int64, error) { q, _, err := maxV2.cpuMax(); return q, err }},
		candidate{cgroupV1, quotaV1.cfsQuota})
	period := c.pick(cpuPeriodUS,
		candidate{cgroupV2, func() (int64, error) { _, p, err := maxV2.cpuMax(); return p, err }},
		candidate{cgroupV1, periodV1.cfsPeriod})
	c.setSource(cpuLimitSource, quota, period)
}

// readPressure reads the some line's total of the cgroup's cpu.pressure, or
// else of the machine's.
func (c *counters) readPressure(src sources) {
	cgroup, proc := readText(src.pressureCgroup), readText(src.pressureProc)
	if source := c.pick(psiCPUSomeUS,
		candidate{"cgroup", cgroup.pressureTotal},
		candidate{"proc", proc.pressureTotal}); source != "" {
		c.Source[psiSource] = source
	}
}

// textFile is a file read once, as text, for the readers below.
type textFile struct {
	path string
	text string
	err  error // reading it failed, or it holds nothing
}

func readText(path string) textFile {
	data, err := os.ReadFile(path)
	f := textFile{path: path, text: string(data), err: err}
	if err == nil && strings.TrimSpace(f.text) == "" {
		f.err = f.bad("empty")
	}
	return f
}

// bad makes an error that names the file.
func (f textFile) bad(format string, args ...any) error {
	return fmt.Errorf("%s: %s", f.path, fmt.Sprintf(format, args...))
}

// found quotes what a file held, cut short, for the reason of a counter that
// could not be read.
func found(s string) string {
	const limit = 60
	if s = strings.TrimSpace(s); len(s) > limit {
		s = s[:limit] + "..."
	}
	return strconv.Quote(s)
}

// line returns the fields of the first line that starts with key.
func (f textFile) line(key string) ([]string, bool) {
	for line := range strings.Lines(f.text) {
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == key {
			return fields, true
		}
	}
	return nil, false
}

// count reads s, the value of what, as a number that is at least least.
func (f textFile) count(s, what string, least int64) (int64, error) {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < least {
		return 0, f.bad("%s is %s, want a number of at least %d", what, found(s), least)
	}
	return v, nil
}

// stat reads the line "KEY VALUE" of a cpu.stat. The file also holds lines
// that this reader does not look at.
func (f textFile) stat(key string) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	fields, ok := f.line(key)
	switch {
	case !ok:
		return 0, f.bad("no %s line", key)
	case len(fields) != 2:
		return 0, f.bad("the %s line is %s, want a number after it", key, found(strings.Join(fields, " ")))
	}
	return f.count(fields[1], key, 0)
}

// cpuMax reads a cgroup v2 cpu.max, "max 100000" or "200000 100000", as the
// quota (-1 for max) and the period, in us.
func (f textFile) cpuMax() (quota, period int64, err error) {
	if f.err != nil {
		return 0, 0, f.err
	}
	fields := strings.Fields(f.text)
	if len(fields) != 2 {
		return 0, 0, f.bad("found %s, want QUOTA PERIOD", found(f.text))
	}
	quota = -1
	if fields[0] != "max" {
		if quota, err = f.count(fields[0], "the quota", 1); err != nil {
			return 0, 0, err
		}
	}
	if period, err = f.count(fields[1], "the period", 1); err != nil {
		return 0, 0, err
	}
	return quota, period, nil
}

// cfsQuota reads a cgroup v1 cpu.cfs_quota_us, a number of us or -1.
func (f textFile) cfsQuota() (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	s := strings.TrimSpace(f.text)
	if s == "-1" {
		return -1, nil
	}
	return f.count(s, "the quota", 1)
}

// cfsPeriod reads a cgroup v1 cpu.cfs_period_us.
func (f textFile) cfsPeriod() (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.count(strings.TrimSpace(f.text), "the period", 1)
}

// pressureTotal reads the total, in us, of the some line of a pressure file.
func (f textFile) pressureTotal() (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	fields, ok := f.line("some")
	if !ok {
		return 0, f.bad("no some line, found %s", found(f.text))
	}
	for _, field := range fields[1:] {
		if v, ok := strings.CutPrefix(field, "total="); ok {
			return f.count(v, "total", 0)
		}
	}
	return 0, f.bad("no total in the some line %s", found(strings.Join(fields, " ")))
}

// steal reads the eighth number of the cpu line of /proc/stat, after user,
// nice, system, idle, iowait, irq and softirq.
func (f textFile) steal() (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	fields, ok := f.line("cpu")
	switch {
	case !ok:
		return 0, f.bad("no cpu line")
	case len(fields) < 9:
		return 0, f.bad("the cpu line is %s, want at least eight numbers", found(strings.Join(fields, " ")))
	}
	return f.count(fields[8], "steal", 0)
}

// deltaResult is what a machine's counters did between two readings, as a
// run records it. Values holds after minus before of each counter. A
// counter that is unreadable on either side is unreadable here, with the
// reason; Partial lists the counters whose difference is a lower bound. The
// difference of sched_tasks is the change in the number of threads.
// Settings has the quota and period, and the sources, as the later reading
// had them.
type deltaResult struct {
	Values     map[string]int64  `json:"values,omitempty"`
	Unreadable map[string]string `json:"unreadable,omitempty"`
	Partial    []string          `json:"partial,omitempty"`
	Settings   counters          `json:"settings,omitzero"`
}

// unreadableDelta is the difference of readings of which one was not had.
func unreadableDelta(reason string) deltaResult {
	return deltaResult{Unreadable: map[string]string{allUnreadable: reason}}
}

// delta subtracts the reading before from the one after. A counter that
// went down is unreadable, except the number of threads, which may. The
// sums over the threads of a process are partial if its threads fell in
// number, since an ended thread takes its counts with it.
func delta(before, after counters) deltaResult {
	d := deltaResult{Values: map[string]int64{}, Unreadable: map[string]string{}}
	names := map[string]bool{}
	for _, c := range []counters{before, after} {
		for name := range c.Values {
			names[name] = true
		}
		for name := range c.Unreadable {
			names[name] = true
		}
	}
	for _, name := range settingNames {
		delete(names, name)
	}
	for _, name := range slices.Sorted(maps.Keys(names)) {
		b, bok := before.Values[name]
		a, aok := after.Values[name]
		key, hasSource := sourceOf[name]
		switch {
		case !bok || !aok:
			d.Unreadable[name] = whyNot(name, before, after)
		case hasSource && before.Source[key] != after.Source[key]:
			d.Unreadable[name] = fmt.Sprintf("source changed from %s to %s", before.Source[key], after.Source[key])
		case a < b && name != schedTasks:
			d.Unreadable[name] = "went down"
		default:
			d.Values[name] = a - b
		}
	}
	if before.Values[schedTasks] > after.Values[schedTasks] {
		for _, name := range []string{schedDelayNS, schedRunNS, schedSlices} {
			if _, ok := d.Values[name]; ok {
				d.Partial = append(d.Partial, name)
			}
		}
	}
	for _, name := range settingNames {
		if v, ok := after.Values[name]; ok {
			if d.Settings.Values == nil {
				d.Settings.Values = map[string]int64{}
			}
			d.Settings.Values[name] = v
		} else if why, ok := after.Unreadable[name]; ok {
			if d.Settings.Unreadable == nil {
				d.Settings.Unreadable = map[string]string{}
			}
			d.Settings.Unreadable[name] = why
		}
	}
	if len(after.Source) > 0 {
		d.Settings.Source = maps.Clone(after.Source)
	}
	return d
}

// whyNot says why name has no difference, from the two readings, of which
// at least one lacks its value.
func whyNot(name string, before, after counters) string {
	why := func(c counters) string {
		if _, ok := c.Values[name]; ok {
			return ""
		}
		if reason, ok := c.Unreadable[name]; ok {
			return reason
		}
		return "not read"
	}
	b, a := why(before), why(after)
	switch {
	case b == "":
		return "after: " + a
	case a == "":
		return "before: " + b
	case a == b:
		return a
	}
	return "before: " + b + "; after: " + a
}

// namesRead says which counters were readable in a set of runs.
type namesRead struct {
	Readable   []string `json:"readable"`   // readable in at least one run
	Unreadable []string `json:"unreadable"` // unreadable in every run; "*" if no reading was had in any
}

// readableNames lists the names that the runs' deltas of one machine hold,
// by whether they could be read. A delta that failed as a whole has no
// counter readable.
func readableNames(deltas []*deltaResult) namesRead {
	out := namesRead{Readable: []string{}, Unreadable: []string{}}
	names := map[string]bool{}
	failed := 0
	for _, d := range deltas {
		if _, ok := d.Unreadable[allUnreadable]; ok {
			failed++
		}
		for _, m := range []map[string]int64{d.Values, d.Settings.Values} {
			for name := range m {
				names[name] = true
			}
		}
		for _, m := range []map[string]string{d.Unreadable, d.Settings.Unreadable} {
			for name := range m {
				names[name] = true
			}
		}
	}
	delete(names, allUnreadable)
	for _, name := range slices.Sorted(maps.Keys(names)) {
		anyRead, allNot := false, true
		for _, d := range deltas {
			_, read := d.Values[name]
			_, readSetting := d.Settings.Values[name]
			_, not := d.Unreadable[name]
			_, notSetting := d.Settings.Unreadable[name]
			_, whole := d.Unreadable[allUnreadable]
			anyRead = anyRead || read || readSetting
			allNot = allNot && (not || notSetting || whole)
		}
		switch {
		case anyRead:
			out.Readable = append(out.Readable, name)
		case allNot:
			out.Unreadable = append(out.Unreadable, name)
		}
	}
	if len(deltas) > 0 && failed == len(deltas) {
		out.Unreadable = append(out.Unreadable, allUnreadable)
	}
	return out
}

// countersRead is summary.json's counters_readable: for each machine, which
// of the counters its runs could read.
type countersRead struct {
	Engine namesRead `json:"engine"`
	Client namesRead `json:"client"`
}

// readableCounters tells which counters the records' deltas could read.
func readableCounters(records []runRecord) countersRead {
	var engine, client []*deltaResult
	for _, r := range records {
		if r.EngineDelta != nil {
			engine = append(engine, r.EngineDelta)
		}
		if r.ClientDelta != nil {
			client = append(client, r.ClientDelta)
		}
	}
	return countersRead{Engine: readableNames(engine), Client: readableNames(client)}
}
