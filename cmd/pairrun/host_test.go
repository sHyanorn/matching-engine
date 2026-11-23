package main

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeProc writes files named by name in a directory of its own and returns
// their directory.
func fakeProc(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// fakeSources points the sources at the files that fakeProc wrote; a file
// that was not written is missing.
func fakeSources(dir string) sources {
	p := func(name string) string { return filepath.Join(dir, name) }
	return sources{
		cpuinfo: p("cpuinfo"), loadavg: p("loadavg"), bootID: p("boot_id"), status: p("status"),
		meminfo: p("meminfo"), cgroupV2: p("memory.max"), cgroupV1: p("memory.limit_in_bytes"),
		procDir: p("proc"), procStat: p("stat"), cpuStatV2: p("cpu.stat"), cpuMaxV2: p("cpu.max"), cpuStatV1: p("cpu.stat.v1"),
		cfsQuotaV1: p("cpu.cfs_quota_us"), cfsPeriodV1: p("cpu.cfs_period_us"), pressureCgroup: p("cpu.pressure"), pressureProc: p("pressure.cpu"),
	}
}

const meminfo16G = "MemTotal:       16777216 kB\nMemFree:         1000 kB\n" // 16 GiB

func TestMemoryLimit(t *testing.T) {
	for _, c := range []struct {
		name  string
		files map[string]string
		want  int64
		fails bool
	}{
		{"v2 limit below MemTotal", map[string]string{"memory.max": "8589934592\n", "meminfo": meminfo16G}, 8 << 30, false},
		{"v2 max means none", map[string]string{"memory.max": "max\n", "meminfo": meminfo16G}, 16 << 30, false},
		{"v1 limit below MemTotal", map[string]string{"memory.limit_in_bytes": "4294967296\n", "meminfo": meminfo16G}, 4 << 30, false},
		{"v1 unlimited is an absurd number", map[string]string{"memory.limit_in_bytes": "9223372036854771712\n", "meminfo": meminfo16G}, 16 << 30, false},
		{"v2 before v1", map[string]string{"memory.max": "2147483648", "memory.limit_in_bytes": "4294967296", "meminfo": meminfo16G}, 2 << 30, false},
		{"v2 max, then v1's limit", map[string]string{"memory.max": "max", "memory.limit_in_bytes": "4294967296", "meminfo": meminfo16G}, 4 << 30, false},
		{"no cgroup files", map[string]string{"meminfo": meminfo16G}, 16 << 30, false},
		{"a limit above MemTotal is none", map[string]string{"memory.max": "34359738368", "meminfo": meminfo16G}, 16 << 30, false},
		{"a limit and no meminfo", map[string]string{"memory.max": "8589934592"}, 8 << 30, false},
		{"an absurd limit and no meminfo", map[string]string{"memory.max": "9223372036854771712"}, 0, true},
		{"nothing readable", map[string]string{}, 0, true},
		{"meminfo without MemTotal", map[string]string{"meminfo": "MemFree: 5 kB\n"}, 0, true},
		{"meminfo in another unit", map[string]string{"meminfo": "MemTotal: 5 MB\n"}, 0, true},
		{"a limit that is not a number", map[string]string{"memory.max": "lots", "meminfo": meminfo16G}, 16 << 30, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := memoryLimit(fakeSources(fakeProc(t, c.files)))
			if (err != nil) != c.fails || got != c.want {
				t.Errorf("got %d, %v, want %d, fails %v", got, err, c.want, c.fails)
			}
		})
	}
}

func TestParseCPUList(t *testing.T) {
	for _, c := range []struct {
		in    string
		want  []int
		fails bool
	}{
		{"0", []int{0}, false},
		{"0-3", []int{0, 1, 2, 3}, false},
		{"0,2-4,7", []int{0, 2, 3, 4, 7}, false},
		{"6,0-1", []int{0, 1, 6}, false},
		{"2-2", []int{2}, false},
		{"3-1", nil, true},
		{"a", nil, true},
		{"", nil, true},
		{"1-", nil, true},
	} {
		got, err := parseCPUList(c.in)
		if (err != nil) != c.fails || !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseCPUList(%q) = %v, %v, want %v, fails %v", c.in, got, err, c.want, c.fails)
		}
	}
}

func TestEngineCPUs(t *testing.T) {
	r := func(lo, hi int) []int {
		var s []int
		for i := lo; i <= hi; i++ {
			s = append(s, i)
		}
		return s
	}
	for _, c := range []struct {
		name    string
		allowed []int
		want    string
	}{
		{"8 CPUs: all but the two highest", r(0, 7), "0-5"},
		{"4 CPUs", r(0, 3), "0-1"},
		{"3 CPUs leave one", r(0, 2), "0"},
		{"2 CPUs: no restriction", r(0, 1), ""},
		{"1 CPU: no restriction", []int{4}, ""},
		{"none", nil, ""},
		{"a gap", []int{0, 2, 3, 4, 6}, "0,2-3"},
		{"the highest are not the last numbers asked for", []int{9, 1, 5, 3}, "1,3"},
		{"CPUs from 2 to 9", r(2, 9), "2-7"},
	} {
		in := append([]int(nil), c.allowed...)
		if got := engineCPUs(c.allowed); got != c.want {
			t.Errorf("%s: engineCPUs(%v) = %q, want %q", c.name, in, got, c.want)
		}
		if !reflect.DeepEqual(in, c.allowed) && c.allowed != nil {
			t.Errorf("%s: engineCPUs reordered its argument: %v became %v", c.name, in, c.allowed)
		}
	}
}

func TestFormatCPUList(t *testing.T) {
	for _, c := range []struct {
		in   []int
		want string
	}{
		{nil, ""}, {[]int{3}, "3"}, {[]int{0, 1}, "0-1"}, {[]int{0, 2}, "0,2"}, {[]int{0, 1, 2, 5, 7, 8}, "0-2,5,7-8"},
	} {
		if got := formatCPUList(c.in); got != c.want {
			t.Errorf("formatCPUList(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCPUModel(t *testing.T) {
	info := "processor\t: 0\nvendor_id\t: GenuineIntel\nmodel name\t: Intel(R) Xeon(R) @ 2.10GHz\nprocessor\t: 1\nmodel name\t: Second CPU\n"
	if got := cpuModel(info); got != "Intel(R) Xeon(R) @ 2.10GHz" {
		t.Errorf("cpuModel = %q, want the first model name", got)
	}
	if got := cpuModel("processor : 0\nCPU part : 0xd0c\n"); got != "" {
		t.Errorf("cpuModel of a machine that names none = %q", got)
	}
}

func TestParseLoadavg(t *testing.T) {
	got, err := parseLoadavg("1.44 0.63 0.31 1/113 7452\n")
	if err != nil || got != [3]float64{1.44, 0.63, 0.31} {
		t.Errorf("got %v, %v", got, err)
	}
	for _, bad := range []string{"", "1 2", "a b c"} {
		if _, err := parseLoadavg(bad); err == nil {
			t.Errorf("parseLoadavg(%q) succeeded", bad)
		}
	}
}

func TestReadMachine(t *testing.T) {
	files := map[string]string{
		"cpuinfo": "model name\t: Test CPU @ 3GHz\n",
		"loadavg": "0.50 1.25 2.00 2/300 99\n",
		"boot_id": "abc-123\n",
		"status":  "Name:\tpairrun\nCpus_allowed:\tff\nCpus_allowed_list:\t0-2,5\nMems_allowed:\t1\n",
	}
	now := time.Date(2026, 11, 22, 9, 30, 0, 0, time.FixedZone("x", 3600))
	m, err := readMachine(fakeSources(fakeProc(t, files)), now)
	want := machine{CPUModel: "Test CPU @ 3GHz", NProc: 4, Loadavg: [3]float64{0.5, 1.25, 2}, BootID: "abc-123", Time: "2026-11-22T08:30:00Z"}
	if err != nil || m != want {
		t.Errorf("got %+v, %v, want %+v", m, err, want)
	}
	for name := range files {
		rest := map[string]string{}
		for k, v := range files {
			if k != name {
				rest[k] = v
			}
		}
		if _, err := readMachine(fakeSources(fakeProc(t, rest)), now); err == nil {
			t.Errorf("readMachine without %s succeeded", name)
		}
	}
	files["status"] = "Name:\tpairrun\n"
	if _, err := readMachine(fakeSources(fakeProc(t, files)), now); err == nil || !strings.Contains(err.Error(), "Cpus_allowed_list") {
		t.Errorf("status without the CPU list: %v", err)
	}
}

func TestHostRecordHoldsNothingOfTheEnvironment(t *testing.T) {
	// The record has these keys and no others: a field added to it would show here.
	got, err := jsonKeys(hostRecord{Overrides: map[string]string{"PILOT_ROUNDS": "2"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"boot_id", "clock_read_ns", "config", "cpu_model", "engine_pid", "gomemlimit_bytes", "loadavg", "mem_limit_bytes", "nproc", "overrides", "ready", "starts", "time"}
	slices.Sort(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("host record keys %v, want %v", got, want)
	}
}

func TestMeasureClockReadIsTheCostOfOneRead(t *testing.T) {
	// A read takes tens of nanoseconds: the bounds only tell a mean per call
	// from a total, or from seconds.
	if got := measureClockRead(); !(got > 1 && got < 1e6) {
		t.Errorf("measureClockRead() = %v ns", got)
	}
}
