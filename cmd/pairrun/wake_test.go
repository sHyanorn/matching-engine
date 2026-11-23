package main

import (
	"context"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sHyanorn/matching-engine/internal/spin"
)

// wakeScript drives a meter's loop: each sleep moves the clock by the next of
// its steps, and the last one ends the loop.
type wakeScript struct {
	at    time.Time
	steps []time.Duration
}

// until adds steps of step for as long as the clock is before t.
func (w *wakeScript) until(t time.Time, step time.Duration) *wakeScript {
	for at := w.end(); at.Before(t); at = at.Add(step) {
		w.steps = append(w.steps, step)
	}
	return w
}

func (w *wakeScript) one(step time.Duration) *wakeScript {
	w.steps = append(w.steps, step)
	return w
}

func (w *wakeScript) end() time.Time {
	at := w.at
	for _, s := range w.steps {
		at = at.Add(s)
	}
	return at
}

// measure runs a meter over the steps of the script, which starts at its at,
// and returns what the meter would answer for every second.
func measure(t *testing.T, w *wakeScript) wakeAnswer {
	t.Helper()
	m := newWakeMeter(wakePlacement{CPU: 1, Siblings: []int{1}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now, steps := w.at, w.steps
	m.now = func() time.Time { return now }
	m.sleep = func(time.Duration) {
		now = now.Add(steps[0])
		if steps = steps[1:]; len(steps) == 0 {
			cancel()
		}
	}
	m.loop(ctx)
	return m.answer(0, math.MaxInt64)
}

func unixSecond(s int64, offset time.Duration) time.Time { return time.Unix(s, 0).Add(offset) }

// A wake is charged to the second its sleep was due in, not the second it came
// in: the late wake below is due in 1000 and comes in 1001, and shows in 1000
// only. The second that has not ended is not served.
func TestWakeMeterChargesALateWakeToTheSecondItWasDue(t *testing.T) {
	const tick = 1100 * time.Microsecond // a wake that is 100 us late
	w := &wakeScript{at: unixSecond(1000, 0)}
	w.until(unixSecond(1000, 990*time.Millisecond), tick) // 900 wakes
	w.one(20 * time.Millisecond)                          // due at 1000.991, comes at 1001.010: 19 ms late
	w.until(unixSecond(1002, 500*time.Millisecond), tick)
	a := measure(t, w)
	if a.T0 != 1000 || len(a.N) != 2 {
		t.Fatalf("the answer starts at %d and has %d seconds, want 1000 and the 2 that were finished: %+v", a.T0, len(a.N), a)
	}
	if a.N[0] != 901 || a.Max[0] != 19000 || a.P99[0] != 100 {
		t.Errorf("second 1000: n %d, p99 %v, max %v, want 901, 100 and 19000", a.N[0], a.P99[0], a.Max[0])
	}
	if a.Max[1] != 100 || a.P99[1] != 100 || a.N[1] < 800 {
		t.Errorf("second 1001: n %d, p99 %v, max %v, want the 19 ms wake in no other second than 1000", a.N[1], a.P99[1], a.Max[1])
	}
}

// A stall of three seconds is one very late wake in the second it began, and
// the seconds that it covered have no wake.
func TestWakeMeterStallShowsInOneRowAndLeavesTheNextOnesEmpty(t *testing.T) {
	const tick = 1100 * time.Microsecond
	w := &wakeScript{at: unixSecond(2000, 0)}
	w.until(unixSecond(2000, 500*time.Millisecond), tick)
	w.one(3*time.Second + time.Millisecond) // due at 2000.501, comes 3 s late
	w.until(unixSecond(2004, 500*time.Millisecond), tick)
	a := measure(t, w)
	if a.T0 != 2000 || len(a.N) != 4 {
		t.Fatalf("the answer starts at %d and has %d seconds, want 2000 and 4 (2004 is not finished): %+v", a.T0, len(a.N), a)
	}
	if a.Max[0] != 3e6 || a.P99[0] != 100 {
		t.Errorf("second 2000: p99 %v, max %v, want 100 and 3e6 us", a.P99[0], a.Max[0])
	}
	for i := 1; i <= 2; i++ {
		if a.N[i] != 0 || a.P99[i] != 0 || a.Max[i] != 0 {
			t.Errorf("second %d: n %d, p99 %v, max %v, want 0 in every column", 2000+i, a.N[i], a.P99[i], a.Max[i])
		}
	}
	if a.N[3] == 0 || a.Max[3] != 100 {
		t.Errorf("second 2003: n %d, max %v, want the wakes after the stall, 100 us late", a.N[3], a.Max[3])
	}
}

func TestWakeMeterServesASecondOnlyOnceALaterOneHasAWake(t *testing.T) {
	m := newWakeMeter(wakePlacement{CPU: 1})
	m.record(unixSecond(5, 100*time.Millisecond), time.Millisecond)
	m.record(unixSecond(5, 900*time.Millisecond), 2*time.Millisecond)
	if a := m.answer(0, math.MaxInt64); len(a.N) != 0 {
		t.Errorf("second 5 was served before a wake of a later second was in: %+v", a)
	}
	m.record(unixSecond(6, time.Millisecond), time.Millisecond)
	a := m.answer(0, math.MaxInt64)
	if a.T0 != 5 || !reflect.DeepEqual(a.N, []int{2}) || a.Max[0] != 2000 {
		t.Errorf("after a wake of second 6: %+v, want second 5 alone, with 2 wakes and a max of 2000 us", a)
	}
}

// The percentiles are the nearest-rank values of the second's lateness, in us,
// to two significant digits.
func TestWakeMeterPercentilesAreNearestRankAndRounded(t *testing.T) {
	m := newWakeMeter(wakePlacement{CPU: 1})
	// 200 wakes, the kth 12.3k us late, in a shuffled order: p99 is the 198th
	// (2435.4 us) and the max the 200th (2460 us).
	for i := range 200 {
		k := (i*37)%200 + 1
		m.record(unixSecond(3000, time.Duration(i)*time.Millisecond), time.Duration(k)*12300)
	}
	m.record(unixSecond(3001, 0), 0)
	a := m.answer(0, math.MaxInt64)
	if a.N[0] != 200 || a.P99[0] != 2400 || a.Max[0] != 2500 {
		t.Errorf("n %d, p99 %v, max %v, want 200, 2400 and 2500", a.N[0], a.P99[0], a.Max[0])
	}
}

func TestNearestRank(t *testing.T) {
	for _, c := range []struct {
		n    int
		p    float64
		want int
	}{
		{1, 99, 0}, {2, 99, 1}, {100, 99, 98}, {101, 99, 99}, {200, 99, 197}, {1000, 99, 989}, {300, 99, 296}, {5, 50, 2},
	} {
		if got := nearestRank(c.n, c.p); got != c.want {
			t.Errorf("nearestRank(%d, %v) = %d, want %d", c.n, c.p, got, c.want)
		}
	}
}

func TestTwoDigits(t *testing.T) {
	for _, c := range []struct{ in, want float64 }{{0, 0}, {0.253, 0.25}, {87.951, 88}, {1234.5, 1200}, {2999000, 3e6}, {99.5, 100}} {
		if got := twoDigits(c.in); got != c.want {
			t.Errorf("twoDigits(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// A sleep that returns early is a wake on time, not a negative lateness that
// would pull a percentile below zero.
func TestWakeMeterEarlyWakeCountsAsOnTime(t *testing.T) {
	m := newWakeMeter(wakePlacement{CPU: 1})
	m.record(unixSecond(7, 0), -3*time.Millisecond)
	m.record(unixSecond(8, 0), 0)
	if a := m.answer(0, math.MaxInt64); !reflect.DeepEqual(a.N, []int{1}) || a.P99[0] != 0 || a.Max[0] != 0 {
		t.Errorf("got %+v, want one wake with no lateness", a)
	}
}

// If the wall clock steps back, a wake due in an earlier second than the one
// held joins it: the rows stay in ascending order.
func TestWakeMeterClockThatStepsBackKeepsRowsAscending(t *testing.T) {
	m := newWakeMeter(wakePlacement{CPU: 1})
	m.record(unixSecond(10, 0), time.Millisecond)
	m.record(unixSecond(9, 0), time.Millisecond)
	m.record(unixSecond(11, 0), time.Millisecond)
	if a := m.answer(0, math.MaxInt64); a.T0 != 10 || !reflect.DeepEqual(a.N, []int{2}) {
		t.Errorf("got %+v, want second 10 with both wakes", a)
	}
}

// answer keeps the seconds whose start is from from to to, ends included, and
// fills the seconds between two of them that had no wake.
func TestWakeMeterAnswerRangeAndGaps(t *testing.T) {
	m := newWakeMeter(wakePlacement{CPU: 1})
	for _, s := range []int64{100, 101, 104, 105} {
		m.record(unixSecond(s, 0), time.Duration(s-99)*10*time.Microsecond)
	}
	m.record(unixSecond(106, 0), 0) // finishes 105
	for _, c := range []struct {
		name     string
		from, to int64
		t0       int64
		n        []int
		max      []float64
	}{
		{"all, the gap filled", 0, math.MaxInt64, 100, []int{1, 1, 0, 0, 1, 1}, []float64{10, 20, 0, 0, 50, 60}},
		{"both ends are in", 100_000, 105_000, 100, []int{1, 1, 0, 0, 1, 1}, []float64{10, 20, 0, 0, 50, 60}},
		{"from just after a start leaves that second out", 100_001, 105_000, 101, []int{1, 0, 0, 1, 1}, []float64{20, 0, 0, 50, 60}},
		{"to just before a start leaves that second out", 100_000, 104_999, 100, []int{1, 1, 0, 0, 1}, []float64{10, 20, 0, 0, 50}},
		{"one second", 101_000, 101_000, 101, []int{1}, []float64{20}},
		{"inside a gap", 102_000, 103_999, 0, []int{}, []float64{}},
		{"before the first", 0, 99_999, 0, []int{}, []float64{}},
		{"after the last finished one", 106_000, 200_000, 0, []int{}, []float64{}},
	} {
		a := m.answer(c.from, c.to)
		if a.T0 != c.t0 || !reflect.DeepEqual(a.N, c.n) || !reflect.DeepEqual(a.Max, c.max) || len(a.P99) != len(c.n) {
			t.Errorf("%s: t0 %d, n %v, max %v, p99 %v, want t0 %d, n %v, max %v", c.name, a.T0, a.N, a.Max, a.P99, c.t0, c.n, c.max)
		}
	}
}

func TestWakeAnswerCheck(t *testing.T) {
	ok := wakeAnswer{NowUnixNS: 5, T0: 3, N: []int{1, 2}, P99: []float64{1, 2}, Max: []float64{3, 4}}
	if err := ok.check(); err != nil {
		t.Errorf("a whole answer: %v", err)
	}
	if got, want := ok.rows(), []wakeRow{{3, 1, 1, 3}, {4, 2, 2, 4}}; !reflect.DeepEqual(got, want) {
		t.Errorf("rows %+v, want %+v", got, want)
	}
	for name, mutate := range map[string]func(*wakeAnswer){
		"no clock":          func(a *wakeAnswer) { a.NowUnixNS = 0 },
		"short p99":         func(a *wakeAnswer) { a.P99 = a.P99[:1] },
		"short max":         func(a *wakeAnswer) { a.Max = nil },
		"long n":            func(a *wakeAnswer) { a.N = append(a.N, 3) },
		"no clock and gaps": func(a *wakeAnswer) { a.NowUnixNS, a.Max = -1, nil },
	} {
		a := ok
		a.P99, a.Max = slices.Clone(ok.P99), slices.Clone(ok.Max)
		mutate(&a)
		if a.check() == nil {
			t.Errorf("%s: check accepted %+v", name, a)
		}
	}
}

func TestWakeRange(t *testing.T) {
	for _, c := range []struct {
		query    string
		from, to int64
		bad      bool
	}{
		{"from=0&to=0", 0, 0, false},
		{"from=1000&to=2000", 1000, 2000, false},
		{"to=2000&from=1000", 1000, 2000, false},
		{"", 0, 0, true},
		{"from=1000", 0, 0, true},
		{"to=1000", 0, 0, true},
		{"from=&to=5", 0, 0, true},
		{"from=5&to=", 0, 0, true},
		{"from=abc&to=5", 0, 0, true},
		{"from=1.5&to=5", 0, 0, true},
		{"from=0x10&to=50", 0, 0, true},
		{"from=-1&to=5", 0, 0, true},
		{"from=5&to=-5", 0, 0, true},
		{"from=9&to=8", 0, 0, true},
		{"from=99999999999999999999&to=5", 0, 0, true},
	} {
		q, err := url.ParseQuery(c.query)
		if err != nil {
			t.Fatal(err)
		}
		from, to, err := wakeRange(q)
		if (err != nil) != c.bad || from != c.from || to != c.to {
			t.Errorf("wakeRange(%q) = %d, %d, %v, want %d, %d, bad %v", c.query, from, to, err, c.from, c.to, c.bad)
		}
	}
}

// fakeTopology writes the thread_siblings_list of each CPU of lists into a
// sysfs directory and returns it.
func fakeTopology(t *testing.T, lists map[int]string) string {
	t.Helper()
	sysfs := t.TempDir()
	for cpu, list := range lists {
		dir := filepath.Join(sysfs, "cpu"+strconv.Itoa(cpu), "topology")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "thread_siblings_list"), []byte(list+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return sysfs
}

func cpuRange(from, to int) []int {
	var cpus []int
	for cpu := from; cpu <= to; cpu++ {
		cpus = append(cpus, cpu)
	}
	return cpus
}

// sharesPool is what the note of a placement says when the meter's CPU is one
// that the engine's spin pool may hand out.
const sharesPool = "shares a CPU with the spin pool"

func TestPlaceWake(t *testing.T) {
	// Four cores of two threads on 8 CPUs, cpu0 and cpu4 the threads of a core.
	ht := map[int]string{0: "0,4", 1: "1,5", 2: "2,6", 3: "3,7", 4: "0,4", 5: "1,5", 6: "2,6", 7: "3,7"}
	// The same on a 128-CPU host, of which a Pod has 8: the cores 8 to 11, with
	// their threads 72 to 75.
	pod := map[int]string{8: "8,72", 9: "9,73", 10: "10,74", 11: "11,75", 72: "8,72", 73: "9,73", 74: "10,74", 75: "11,75"}
	for _, c := range []struct {
		name         string
		lists        map[int]string
		allowed      []int
		cpu          int
		siblings     []int
		cores        int
		readable     bool
		noteContains string
		poolReaches  bool // the spin pool may hand out the meter's CPU, see placeWake
	}{
		{"hyper-threads", ht, cpuRange(0, 7), 7, []int{3, 7}, 4, true, "", false},
		{"no hyper-threads", map[int]string{0: "0", 1: "1", 2: "2", 3: "3"}, cpuRange(0, 3), 3, []int{3}, 4, true, "", false},
		{"the topology is unreadable", nil, cpuRange(0, 3), 3, []int{3}, 4, false, "unreadable", false},
		{"8 allowed CPUs of 128", pod, append(cpuRange(8, 11), cpuRange(72, 75)...), 75, []int{11, 75}, 4, true, "", false},
		{"one CPU", map[int]string{5: "5"}, []int{5}, -1, []int{}, 0, false, "1 CPU", false},
		{"no CPU", nil, nil, -1, []int{}, 0, false, "0 CPU", false},
		{"threads numbered side by side", map[int]string{0: "0-1", 1: "0-1", 2: "2-3", 3: "2-3"}, cpuRange(0, 3), 3, []int{2, 3}, 2, true, "", false},
		{"the last core has one thread allowed, another core two", ht, []int{0, 1, 4}, 1, []int{1}, 2, true, sharesPool, true},
		{"a CPU of its own after a core of two threads", map[int]string{0: "0-1", 1: "0-1", 2: "2"}, cpuRange(0, 2), 2, []int{2}, 2, true, sharesPool, true},
		{"both threads of the last core allowed", ht, []int{0, 1, 5}, 5, []int{1, 5}, 2, true, "", false},
		{"two CPUs that are the threads of one core", map[int]string{0: "0-1", 1: "0-1"}, []int{0, 1}, 1, []int{0, 1}, 1, true, "", false},
		{"two cores of one thread, the pilot's rehearsal", map[int]string{0: "0", 1: "1"}, []int{0, 1}, 1, []int{1}, 2, true, "", false},
		{"only the last core's list is unreadable", map[int]string{0: "0", 1: "1", 2: "2"}, cpuRange(0, 3), 3, []int{3}, 4, false, "unreadable", false},
		{"a list that does not hold the CPU", map[int]string{0: "0", 1: "1", 2: "2", 3: "5-6"}, cpuRange(0, 3), 3, []int{3}, 4, false, "unreadable", false},
		{"a list that is no list", map[int]string{0: "0", 1: "1", 2: "2", 3: "three"}, cpuRange(0, 3), 3, []int{3}, 4, false, "unreadable", false},
		{"an empty list", map[int]string{0: "0", 1: "1", 2: "2", 3: ""}, cpuRange(0, 3), 3, []int{3}, 4, false, "unreadable", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			sysfs := fakeTopology(t, c.lists)
			p := placeWake(sysfs, c.allowed)
			if p.CPU != c.cpu || !reflect.DeepEqual(p.Siblings, c.siblings) || p.Cores != c.cores || p.TopologyReadable != c.readable || p.Pinned {
				t.Errorf("got cpu %d, siblings %v, cores %d, readable %v, pinned %v, want %d, %v, %d, %v, false",
					p.CPU, p.Siblings, p.Cores, p.TopologyReadable, p.Pinned, c.cpu, c.siblings, c.cores, c.readable)
			}
			if !strings.Contains(p.Note, c.noteContains) || (c.noteContains == "") != (p.Note == "") {
				t.Errorf("note %q, want one that holds %q, and none if that is empty", p.Note, c.noteContains)
			}
			if p.CPU < 0 || c.poolReaches {
				return
			}
			// The CPU is never one that the engine's spin pool hands out, whatever
			// the engine's GOMAXPROCS up to the number of CPUs.
			cores := spin.Cores(sysfs, c.allowed)
			for procs := 1; procs <= len(c.allowed); procs++ {
				pool := cores[:min(len(cores), procs-1)]
				if slices.Contains(pool, p.CPU) {
					t.Errorf("the meter's CPU %d is in the spin pool %v of GOMAXPROCS %d", p.CPU, pool, procs)
				}
			}
		})
	}
}

// The pool hands out the lowest CPU of each core, lowest core first, and at
// most GOMAXPROCS-1 of them, and the engine has one P for each allowed CPU at
// most. With cpu0 and cpu1 the threads of one core and cpu2 on a core of its
// own, the pool is cpu0 and cpu2, so the meter on cpu2 shares its CPU with a
// spinner, and the placement says so.
func TestPlaceWakeNotesACPUTheSpinPoolMayHandOut(t *testing.T) {
	sysfs := fakeTopology(t, map[int]string{0: "0-1", 1: "0-1", 2: "2"})
	allowed := cpuRange(0, 2)
	p := placeWake(sysfs, allowed)
	if p.CPU != 2 || !reflect.DeepEqual(p.Siblings, []int{2}) || p.Cores != 2 || !p.TopologyReadable || p.Pinned {
		t.Errorf("got cpu %d, siblings %v, cores %d, readable %v, pinned %v, want 2, [2], 2, true, false", p.CPU, p.Siblings, p.Cores, p.TopologyReadable, p.Pinned)
	}
	if !strings.Contains(p.Note, sharesPool) {
		t.Errorf("note %q does not say %q", p.Note, sharesPool)
	}
	if pool := spin.PoolCPUs(spin.Cores(sysfs, allowed), len(allowed)); !slices.Equal(pool, []int{0, 2}) {
		t.Errorf("the spin pool is %v, want [0 2], which holds the meter's CPU", pool)
	}
}

// A meter on a CPU that the pool cannot reach has no note: the pool leaves one
// P to the rest of the engine, and takes the lowest CPU of a core, not its
// other threads.
func TestPlaceWakeDoesNotNoteACPUTheSpinPoolCannotReach(t *testing.T) {
	for _, c := range []struct {
		name    string
		lists   map[int]string
		allowed []int
		cpu     int
	}{
		{"every CPU a core: the cap keeps the last one out", map[int]string{0: "0", 1: "1", 2: "2", 3: "3"}, cpuRange(0, 3), 3},
		{"the meter takes a core's higher thread", map[int]string{0: "0-1", 1: "0-1", 2: "2-3", 3: "2-3"}, cpuRange(0, 3), 3},
		{"two threads of one core", map[int]string{0: "0-1", 1: "0-1"}, cpuRange(0, 1), 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := placeWake(fakeTopology(t, c.lists), c.allowed)
			if p.CPU != c.cpu || p.Note != "" {
				t.Errorf("got cpu %d and note %q, want cpu %d and no note", p.CPU, p.Note, c.cpu)
			}
		})
	}
}

// A topology that cannot be read and a CPU that the pool may hand out are both
// said. Here cpu3 has no list, so it is a core of its own after cpu0 and cpu1,
// the threads of one, and cpu2.
func TestPlaceWakeKeepsTheTopologyNoteBesideTheSharingNote(t *testing.T) {
	p := placeWake(fakeTopology(t, map[int]string{0: "0-1", 1: "0-1", 2: "2"}), cpuRange(0, 3))
	if p.CPU != 3 || p.TopologyReadable {
		t.Fatalf("got cpu %d, readable %v, want cpu 3 and an unreadable topology", p.CPU, p.TopologyReadable)
	}
	unreadable, shares := strings.Index(p.Note, "unreadable"), strings.Index(p.Note, sharesPool)
	if unreadable < 0 || shares < unreadable || strings.Count(p.Note, "; ") != 1 {
		t.Errorf("note %q, want the unreadable-topology note and then %q, joined by a semicolon", p.Note, sharesPool)
	}
}

// The placement is written into a JSON file: no field is null, and a meter
// that does not exist says -1.
func TestWakePlacementJSONHasNoNulls(t *testing.T) {
	a := newWakeMeter(placeWake(fakeTopology(t, nil), []int{3})).answer(0, 0)
	keys, err := jsonKeys(a.Placement)
	slices.Sort(keys)
	if want := []string{"cores", "cpu", "note", "pinned", "siblings", "topology_readable"}; err != nil || !reflect.DeepEqual(keys, want) {
		t.Errorf("placement keys %v, %v, want %v", keys, err, want)
	}
	if got, err := jsonKeys(a); err != nil || !reflect.DeepEqual(got, []string{"now_unix_ns", "tick_us", "placement", "t0", "n", "p99", "max"}) {
		t.Errorf("answer keys %v, %v", got, err)
	}
	if a.Placement.Siblings == nil || a.N == nil || a.P99 == nil || a.Max == nil {
		t.Errorf("a column is nil, which JSON writes as null: %+v", a)
	}
}

func TestWakeForWindow(t *testing.T) {
	rows := []wakeRow{
		{Second: 1000, N: 900, P99: 110, Max: 400},
		{Second: 1001, N: 880, P99: 150, Max: 9000},
		// 1002 is missing
		{Second: 1003, N: 905, P99: 105, Max: 300},
		{Second: 1004, N: 0}, // a second in which the meter saw no wake
	}
	at := unixSecond
	for _, c := range []struct {
		name   string
		offset time.Duration
		start  time.Time
		length time.Duration
		n      int
		p99    float64
		max    float64
		seen   bool // some second of the rows overlaps the window
	}{
		{"inside one second", 0, at(1000, 200*time.Millisecond), 500 * time.Millisecond, 900, 110, 400, true},
		{"straddles two seconds", 0, at(1000, 700*time.Millisecond), time.Second, 1780, 150, 9000, true},
		{"a whole second, aligned", 0, at(1001, 0), time.Second, 880, 150, 9000, true},
		{"ends on a second's start", 0, at(1000, 500*time.Millisecond), 500 * time.Millisecond, 900, 110, 400, true},
		{"an offset moves it by a second", time.Second, at(1000, 200*time.Millisecond), 500 * time.Millisecond, 880, 150, 9000, true},
		{"a negative offset moves it back", -time.Second, at(1001, 200*time.Millisecond), 500 * time.Millisecond, 900, 110, 400, true},
		{"an offset of a few ms moves it over a boundary", 100 * time.Millisecond, at(1000, 950*time.Millisecond), 20 * time.Millisecond, 880, 150, 9000, true},
		{"a missing second counts nothing", 0, at(1002, 200*time.Millisecond), 500 * time.Millisecond, 0, 0, 0, false},
		{"straddles a missing second and a present one", 0, at(1002, 700*time.Millisecond), time.Second, 905, 105, 300, true},
		{"spans a missing second", 0, at(1001, 500*time.Millisecond), 2 * time.Second, 880 + 905, 150, 9000, true},
		{"a second of no wake is seen", 0, at(1004, 200*time.Millisecond), 500 * time.Millisecond, 0, 0, 0, true},
		{"before every row", 0, at(990, 0), time.Second, 0, 0, 0, false},
		{"after every row", 0, at(1100, 0), time.Second, 0, 0, 0, false},
		{"an offset moves it out of the rows", 5 * time.Second, at(1000, 200*time.Millisecond), 500 * time.Millisecond, 0, 0, 0, false},
		{"a window of no length", 0, at(1000, 500*time.Millisecond), 0, 0, 0, 0, false},
		{"no rows", 0, at(1000, 0), time.Second, 0, 0, 0, false},
	} {
		in := rows
		if c.name == "no rows" {
			in = nil
		}
		n, p99, worst, seen := wakeForWindow(in, c.offset, c.start, c.length)
		if n != c.n || p99 != c.p99 || worst != c.max || seen != c.seen {
			t.Errorf("%s: n %d, p99 %v, max %v, seen %v, want %d, %v, %v, %v", c.name, n, p99, worst, seen, c.n, c.p99, c.max, c.seen)
		}
	}
}

func TestMeasureOffset(t *testing.T) {
	send := time.Date(2026, 11, 23, 8, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name         string
		rtt          time.Duration
		server       time.Duration // the supervisor's clock after send
		offset, errR float64
	}{
		{"a supervisor 1.5 s ahead", 40 * time.Millisecond, 1500*time.Millisecond + 20*time.Millisecond + 123*time.Microsecond, 1500123, 20000},
		{"a supervisor behind", 10 * time.Millisecond, -2*time.Second + 5*time.Millisecond, -2000000, 5000},
		{"the same clock", 2 * time.Millisecond, time.Millisecond, 0, 1000},
		{"no time passed", 0, 7 * time.Microsecond, 7, 0},
	} {
		got := measureOffset(send, send.Add(c.rtt), send.Add(c.server).UnixNano())
		if got.OffsetUS != c.offset || got.ErrorUS != c.errR {
			t.Errorf("%s: got %+v, want offset %v us, error %v us", c.name, got, c.offset, c.errR)
		}
		if d := got.duration(); d != time.Duration(c.offset*1000) {
			t.Errorf("%s: duration %v, want %v", c.name, d, time.Duration(c.offset*1000))
		}
	}
}

func TestWakeWait(t *testing.T) {
	end := unixSecond(5000, 300*time.Millisecond)
	ns := func(s int64, offset time.Duration) int64 { return unixSecond(s, offset).UnixNano() }
	for _, c := range []struct {
		name   string
		now    int64
		end    time.Time
		offset time.Duration
		want   time.Duration
	}{
		{"just ended", ns(5000, 400*time.Millisecond), end, 0, 600*time.Millisecond + wakeSettleSlack},
		{"the next second's first wake is in", ns(5001, wakeSettleSlack), end, 0, 0},
		{"a hair short of it", ns(5001, wakeSettleSlack-time.Millisecond), end, 0, time.Millisecond},
		{"long after", ns(5100, 0), end, 0, 0},
		{"the supervisor's clock is ahead", ns(5001, 400*time.Millisecond), end, 800 * time.Millisecond, 600*time.Millisecond + wakeSettleSlack},
		{"a window that ends on a second's start", ns(4999, 995*time.Millisecond), unixSecond(5000, 0), 0, 5*time.Millisecond + wakeSettleSlack},
	} {
		if got := wakeWait(c.now, c.end, c.offset); got != c.want {
			t.Errorf("%s: waits %v, want %v", c.name, got, c.want)
		}
	}
}
