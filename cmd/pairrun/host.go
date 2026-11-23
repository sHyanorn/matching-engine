package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// sources are the files a machine's description is read from. They are
// parameters so that tests can fake them.
type sources struct {
	cpuinfo  string
	loadavg  string
	bootID   string
	status   string // this process's, for the CPUs it may run on
	meminfo  string
	cgroupV2 string // memory.max
	cgroupV1 string // memory.limit_in_bytes

	// The files of readCounters.
	procDir        string // holds <pid>/task/<tid>/schedstat of each thread
	procStat       string // the machine's CPU times, for steal
	cpuStatV2      string // the cgroup's cpu.stat
	cpuMaxV2       string // cpu.max
	cpuStatV1      string // cpu/cpu.stat
	cfsQuotaV1     string // cpu/cpu.cfs_quota_us
	cfsPeriodV1    string // cpu/cpu.cfs_period_us
	pressureCgroup string // the cgroup's cpu.pressure, v2
	pressureProc   string // the machine's, which counts the tasks of every container
}

var procSources = sources{
	cpuinfo:  "/proc/cpuinfo",
	loadavg:  "/proc/loadavg",
	bootID:   "/proc/sys/kernel/random/boot_id",
	status:   "/proc/self/status",
	meminfo:  "/proc/meminfo",
	cgroupV2: "/sys/fs/cgroup/memory.max",
	cgroupV1: "/sys/fs/cgroup/memory/memory.limit_in_bytes",

	procDir:        "/proc",
	procStat:       "/proc/stat",
	cpuStatV2:      "/sys/fs/cgroup/cpu.stat",
	cpuMaxV2:       "/sys/fs/cgroup/cpu.max",
	cpuStatV1:      "/sys/fs/cgroup/cpu/cpu.stat",
	cfsQuotaV1:     "/sys/fs/cgroup/cpu/cpu.cfs_quota_us",
	cfsPeriodV1:    "/sys/fs/cgroup/cpu/cpu.cfs_period_us",
	pressureCgroup: "/sys/fs/cgroup/cpu.pressure",
	pressureProc:   "/proc/pressure/cpu",
}

// machine is what a run records of a host, apart from the engine: the CPU,
// how many CPUs this process may use, the load and the boot, whose ID is
// the same on two Pods only if they share a host (D043).
type machine struct {
	CPUModel string     `json:"cpu_model"`
	NProc    int        `json:"nproc"`
	Loadavg  [3]float64 `json:"loadavg"`
	BootID   string     `json:"boot_id"`
	Time     string     `json:"time"` // RFC 3339, UTC
}

// hostRecord is the supervisor's answer to /host and /start. It holds
// nothing of the environment but the PILOT_* overrides that differ from the
// defaults.
type hostRecord struct {
	Ready           bool              `json:"ready"`
	Starts          int               `json:"starts"`
	Config          string            `json:"config"` // of the running engine, "" if none
	EnginePID       int               `json:"engine_pid"`
	machine                           // flattened into the record
	MemLimitBytes   int64             `json:"mem_limit_bytes"`
	GoMemLimitBytes int64             `json:"gomemlimit_bytes"`    // what the gogc-off configuration sets
	ClockReadNS     float64           `json:"clock_read_ns"`       // mean cost of reading the clock, measured when the supervisor started
	Overrides       map[string]string `json:"overrides,omitempty"` // the supervisor's PILOT_* overrides, which change its loopback run
}

// clockReadSamples is how many clock reads measureClockRead times.
const clockReadSamples = 3_000_000

// measureClockRead returns the mean cost in nanoseconds of one time.Now, the
// standard clock read. A slow time.Now means a slow clock source, which slows
// every timing in the study.
func measureClockRead() float64 {
	start := time.Now()
	for range clockReadSamples {
		_ = time.Now() // the read is what is timed
	}
	return float64(time.Since(start)) / clockReadSamples
}

// readMachine describes the host at time now.
func readMachine(src sources, now time.Time) (machine, error) {
	m := machine{Time: now.UTC().Format(time.RFC3339)}
	cpuinfo, err := os.ReadFile(src.cpuinfo)
	if err != nil {
		return m, err
	}
	m.CPUModel = cpuModel(string(cpuinfo))
	cpus, err := allowedCPUs(src.status)
	if err != nil {
		return m, err
	}
	m.NProc = len(cpus)
	load, err := os.ReadFile(src.loadavg)
	if err != nil {
		return m, err
	}
	if m.Loadavg, err = parseLoadavg(string(load)); err != nil {
		return m, fmt.Errorf("%s: %w", src.loadavg, err)
	}
	boot, err := os.ReadFile(src.bootID)
	if err != nil {
		return m, err
	}
	m.BootID = strings.TrimSpace(string(boot))
	return m, nil
}

// cpuModel returns the first "model name" of /proc/cpuinfo's text, or ""
// where the kernel names none, as on some ARM machines.
func cpuModel(cpuinfo string) string {
	for line := range strings.Lines(cpuinfo) {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "model name" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// parseLoadavg reads the three averages at the start of /proc/loadavg.
func parseLoadavg(s string) ([3]float64, error) {
	var l [3]float64
	f := strings.Fields(s)
	if len(f) < 3 {
		return l, fmt.Errorf("got %q, want three load averages", s)
	}
	for i := range l {
		v, err := strconv.ParseFloat(f[i], 64)
		if err != nil {
			return l, fmt.Errorf("load average %q: %w", f[i], err)
		}
		l[i] = v
	}
	return l, nil
}

// allowedCPUs lists, ascending, the CPUs the process may run on, from the
// Cpus_allowed_list line of its status file.
func allowedCPUs(status string) ([]int, error) {
	data, err := os.ReadFile(status)
	if err != nil {
		return nil, err
	}
	for line := range strings.Lines(string(data)) {
		if list, ok := strings.CutPrefix(line, "Cpus_allowed_list:"); ok {
			cpus, err := parseCPUList(strings.TrimSpace(list))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", status, err)
			}
			return cpus, nil
		}
	}
	return nil, fmt.Errorf("%s: no Cpus_allowed_list line", status)
}

// parseCPUList reads a kernel CPU list such as "0-3,6", ascending.
func parseCPUList(s string) ([]int, error) {
	var cpus []int
	for part := range strings.SplitSeq(s, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			return nil, fmt.Errorf("CPU list %q: %w", s, err)
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil || b < a {
				return nil, fmt.Errorf("CPU list %q: bad range %q", s, part)
			}
		}
		for cpu := a; cpu <= b; cpu++ {
			cpus = append(cpus, cpu)
		}
	}
	slices.Sort(cpus)
	return slices.Compact(cpus), nil
}

// engineCPUs is the list for taskset that keeps the engine off the two
// highest CPUs of allowed, where loadgen pins its pacer and its reader
// (cmd/loadgen's spareCPUs), or "" for no restriction when there are fewer
// than three CPUs to share.
func engineCPUs(allowed []int) string {
	if len(allowed) < 3 {
		return ""
	}
	rest := slices.Clone(allowed)
	slices.Sort(rest)
	return formatCPUList(rest[:len(rest)-2])
}

// formatCPUList writes ascending CPUs as ranges: "0-5", "0,2-3".
func formatCPUList(cpus []int) string {
	var parts []string
	for i := 0; i < len(cpus); {
		j := i
		for j+1 < len(cpus) && cpus[j+1] == cpus[j]+1 {
			j++
		}
		if j == i {
			parts = append(parts, strconv.Itoa(cpus[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", cpus[i], cpus[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}

// noLimit is the size from which a cgroup's memory limit means none: the
// kernel shows an unlimited cgroup v1 as 9223372036854771712.
const noLimit = 1 << 60

// memoryLimit returns the memory the engine may use: the cgroup's limit,
// v2 before v1, or the machine's MemTotal if there is no limit or it is no
// lower.
func memoryLimit(src sources) (int64, error) {
	total, totalErr := memTotal(src.meminfo)
	limit := int64(0)
	for _, path := range []string{src.cgroupV2, src.cgroupV1} {
		if l, ok := cgroupLimit(path); ok {
			limit = l
			break
		}
	}
	switch {
	case limit > 0 && (totalErr != nil || limit < total):
		return limit, nil
	case totalErr == nil:
		return total, nil
	}
	return 0, fmt.Errorf("no memory limit in %s or %s, and %w", src.cgroupV2, src.cgroupV1, totalErr)
}

// cgroupLimit reads a cgroup memory limit file. A file that is missing, says
// "max" or holds a number too large to be a limit has none.
func cgroupLimit(path string) (int64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || v <= 0 || v >= noLimit {
		return 0, false
	}
	return v, true
}

// memTotal reads MemTotal of /proc/meminfo, in bytes.
func memTotal(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }() // read only
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), "MemTotal:")
		if !ok {
			continue
		}
		num, unit, _ := strings.Cut(strings.TrimSpace(rest), " ")
		kb, err := strconv.ParseInt(num, 10, 64)
		if err != nil || unit != "kB" || kb <= 0 {
			return 0, fmt.Errorf("%s: MemTotal line %q, want a size in kB", path, sc.Text())
		}
		return kb * 1024, nil
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, errors.New(path + ": no MemTotal line")
}
