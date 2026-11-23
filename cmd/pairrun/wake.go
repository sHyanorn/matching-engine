package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sHyanorn/matching-engine/internal/spin"
)

// The wake meter measures, second by second, how late a thread that sleeps
// 1 ms is woken on the engine machine, whatever the engine does. Read next to
// loadgen's series, it tells whether a raised tail comes with a machine whose
// sleepers all wake late, or with the engine's threads alone. It runs in the
// supervisor, once the loopback run is over, and takes no lock of the engine.

// wakeTick is how long the meter sleeps.
const wakeTick = time.Millisecond

// wakePlacement says where the meter runs and how that was decided. Every wake
// file holds it.
type wakePlacement struct {
	CPU              int    `json:"cpu"`      // the CPU the meter's thread is pinned to; -1 if there is no meter
	Siblings         []int  `json:"siblings"` // the allowed CPUs of that CPU's physical core
	Cores            int    `json:"cores"`    // the physical cores among the allowed CPUs
	TopologyReadable bool   `json:"topology_readable"`
	Pinned           bool   `json:"pinned"` // false if the system refused the pin, or there is no meter
	Note             string `json:"note"`
}

// placeWake chooses the CPU of the meter among the CPUs this process may use.
// The engine's spin pool (internal/spin) hands out the lowest CPU of each
// physical core, lowest core first, and never more than GOMAXPROCS-1 of them.
// The meter takes the last core of that order and on it the highest allowed
// CPU, which the pool does not hand out: the meter does not share a logical CPU
// with a spinner. The exception is a last core that has one allowed CPU while
// another core has two, for the pool then reaches the last core, and the note
// says so. The engine is started with no GOMAXPROCS override, so it has at most
// one P for each allowed CPU, and the pool is sized for that. sysfs is the
// directory Linux describes the CPUs in.
//
// If the sibling list of that core cannot be read, the topology is unknown
// (spin.Cores then takes every CPU for a core of its own): the highest allowed
// CPU is used and the note says so. With fewer than two CPUs there is no meter,
// which would share the only CPU with the engine.
func placeWake(sysfs string, allowed []int) wakePlacement {
	p := wakePlacement{CPU: -1, Siblings: []int{}}
	if len(allowed) < 2 {
		p.Note = fmt.Sprintf("%d CPU may be used, too few for a meter that does not share a CPU with the engine", len(allowed))
		return p
	}
	cores := spin.Cores(sysfs, allowed)
	p.Cores = len(cores)
	if siblings, ok := coreSiblings(sysfs, cores[len(cores)-1], allowed); ok {
		p.TopologyReadable, p.Siblings = true, siblings
	} else {
		p.Siblings = []int{slices.Max(allowed)}
		p.Note = "the thread siblings of the last core are unreadable: the highest allowed CPU is used"
	}
	p.CPU = slices.Max(p.Siblings)
	if slices.Contains(spin.PoolCPUs(cores, len(allowed)), p.CPU) {
		p.addNote("shares a CPU with the spin pool: with -oe-spin a spinning thread may be pinned to the meter's CPU and delay its wakes")
	}
	return p
}

// addNote adds s to the note of the placement.
func (p *wakePlacement) addNote(s string) {
	if p.Note != "" {
		p.Note += "; "
	}
	p.Note += s
}

// coreSiblings returns the allowed CPUs among the thread siblings of cpu, and
// false if its thread_siblings_list cannot be read, is not a CPU list or does
// not hold cpu.
func coreSiblings(sysfs string, cpu int, allowed []int) ([]int, bool) {
	data, err := os.ReadFile(filepath.Join(sysfs, "cpu"+strconv.Itoa(cpu), "topology", "thread_siblings_list"))
	if err != nil {
		return nil, false
	}
	list, err := parseCPUList(strings.TrimSpace(string(data)))
	if err != nil || !slices.Contains(list, cpu) {
		return nil, false
	}
	return slices.DeleteFunc(list, func(c int) bool { return !slices.Contains(allowed, c) }), true
}

// wakeRow is one finished second of the meter.
type wakeRow struct {
	Second int64   // Unix second the wakes were due in
	N      int     // wakes due in it
	P99    float64 // of their lateness, in us
	Max    float64
}

// wakeMeter sleeps wakeTick over and over and keeps how late each wake came.
type wakeMeter struct {
	sleep func(time.Duration) // tests replace these three
	now   func() time.Time
	pin   func(cpu int) (restore func() error, err error) // pins the calling thread; restore gives it its CPUs back

	mu    sync.Mutex // guards the fields below
	place wakePlacement
	rows  []wakeRow // finished seconds that had a wake, ascending; kept for the life of the supervisor
	cur   int64     // the second that late holds the wakes of
	late  []int64   // their lateness in ns
}

func newWakeMeter(place wakePlacement) *wakeMeter {
	return &wakeMeter{sleep: time.Sleep, now: time.Now, pin: pinThread, place: place}
}

// placement returns the placement as it is now: Pinned is only known once the
// meter has tried to pin.
func (m *wakeMeter) placement() wakePlacement {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.place
	p.Siblings = slices.Clone(p.Siblings)
	return p
}

// start runs the meter until ctx ends and returns a channel that is closed once
// it has ended. With no CPU there is no meter and the channel is closed
// already.
func (m *wakeMeter) start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	if m.placement().CPU < 0 {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		m.run(ctx)
	}()
	return done
}

// run pins its thread and measures until ctx ends. The goroutine stays locked
// to a thread that it pinned and so ends with it: the thread is never given
// back to run other goroutines, and the threads that the runtime starts
// meanwhile come from its own template thread, so nothing but this goroutine
// has the pin, and no process that the supervisor starts inherits it. The
// runtime ends that thread when the goroutine ends, except the main thread of
// the process, which it keeps parked for good, and /proc/self/status gives the
// CPUs of the main thread as the process's own. The main goroutine keeps that
// thread (main.go), and as a second guard this goroutine gives its thread its
// CPUs back as the last thing it does, still locked.
func (m *wakeMeter) run(ctx context.Context) {
	runtime.LockOSThread()
	restore, err := m.pin(m.placement().CPU)
	if err != nil {
		runtime.UnlockOSThread() // the thread's CPUs are as they were
	} else {
		defer m.putBack(restore)
	}
	m.mu.Lock()
	m.place.Pinned = err == nil
	if err != nil {
		m.place.addNote("the pin was refused, the meter runs unpinned: " + err.Error())
	}
	m.mu.Unlock()
	m.loop(ctx)
}

// putBack gives the calling thread its CPUs back and notes it if it cannot.
func (m *wakeMeter) putBack(restore func() error) {
	if err := restore(); err != nil {
		m.mu.Lock()
		m.place.addNote("the thread's CPUs could not be put back, it keeps the pin: " + err.Error())
		m.mu.Unlock()
	}
}

// loop sleeps wakeTick and records how late each wake came, until ctx ends.
func (m *wakeMeter) loop(ctx context.Context) {
	for ctx.Err() == nil {
		before := m.now()
		m.sleep(wakeTick)
		after := m.now()
		m.record(before.Add(wakeTick), after.Sub(before)-wakeTick)
	}
}

// record takes a wake whose sleep was due at due and came late after it, and
// charges it to the second it was due in, as loadgen's series charges a request
// to the second it was due. A stall therefore shows as one very late wake in
// the second it began and as seconds without a wake after it. A second is
// finished when a wake due in a later one is in. A clock that steps back leaves
// the wakes in the second held.
func (m *wakeMeter) record(due time.Time, late time.Duration) {
	sec := due.Unix()
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case len(m.late) == 0:
		m.cur = sec
	case sec > m.cur:
		m.finish()
		m.cur = sec
	}
	m.late = append(m.late, int64(max(late, 0)))
}

// finish turns the wakes held into a row and empties them. The caller holds
// m.mu and has at least one wake.
func (m *wakeMeter) finish() {
	slices.Sort(m.late)
	n := len(m.late)
	m.rows = append(m.rows, wakeRow{
		Second: m.cur, N: n,
		P99: twoDigits(float64(m.late[nearestRank(n, 99)]) / float64(time.Microsecond)),
		Max: twoDigits(float64(m.late[n-1]) / float64(time.Microsecond)),
	})
	m.late = m.late[:0]
}

// nearestRank is the index of the p-th percentile among n sorted samples: the
// sample of rank ceil(p/100 * n), which rounding error must not push up.
func nearestRank(n int, p float64) int {
	return max(int(math.Ceil(p*float64(n)/100-1e-9)), 1) - 1
}

// twoDigits rounds v to two significant digits, as loadgen's series does.
func twoDigits(v float64) float64 {
	f, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'g', 2, 64), 64) // FormatFloat writes a number
	return f
}

// wakeAnswer is what /wake answers and what a wake file holds of it: the
// finished seconds from t0 on, consecutive, as columns.
type wakeAnswer struct {
	NowUnixNS int64         `json:"now_unix_ns"` // the supervisor's clock as it answered
	TickUS    int           `json:"tick_us"`
	Placement wakePlacement `json:"placement"`
	T0        int64         `json:"t0"`  // the first second, in Unix seconds; 0 if there is none
	N         []int         `json:"n"`   // wakes due in the second; 0 for a second with none
	P99       []float64     `json:"p99"` // us
	Max       []float64     `json:"max"`
}

// answer returns the finished seconds whose start, in Unix milliseconds, lies
// from fromMS to toMS, with a row of n 0 for each second between two of them
// that had no wake. It sets everything but NowUnixNS.
func (m *wakeMeter) answer(fromMS, toMS int64) wakeAnswer {
	a := wakeAnswer{TickUS: int(wakeTick / time.Microsecond), N: []int{}, P99: []float64{}, Max: []float64{}}
	m.mu.Lock()
	defer m.mu.Unlock()
	a.Placement = m.place
	a.Placement.Siblings = slices.Clone(a.Placement.Siblings)
	for _, r := range m.rows {
		if r.Second*1000 < fromMS || r.Second*1000 > toMS {
			continue
		}
		if len(a.N) == 0 {
			a.T0 = r.Second
		}
		for next := a.T0 + int64(len(a.N)); next < r.Second; next++ {
			a.N, a.P99, a.Max = append(a.N, 0), append(a.P99, 0), append(a.Max, 0)
		}
		a.N, a.P99, a.Max = append(a.N, r.N), append(a.P99, r.P99), append(a.Max, r.Max)
	}
	return a
}

// check returns why an answer cannot be used, if it cannot.
func (a wakeAnswer) check() error {
	if a.NowUnixNS <= 0 {
		return errors.New("the answer has no now_unix_ns")
	}
	if len(a.P99) != len(a.N) || len(a.Max) != len(a.N) {
		return fmt.Errorf("the columns differ in length: n %d, p99 %d, max %d", len(a.N), len(a.P99), len(a.Max))
	}
	return nil
}

// rows returns the seconds of an answer that passed check.
func (a wakeAnswer) rows() []wakeRow {
	rows := make([]wakeRow, len(a.N))
	for i := range rows {
		rows[i] = wakeRow{Second: a.T0 + int64(i), N: a.N[i], P99: a.P99[i], Max: a.Max[i]}
	}
	return rows
}

// wakeRange reads the from and to of /wake, both Unix milliseconds, from at
// most to.
func wakeRange(q url.Values) (from, to int64, err error) {
	for _, p := range []struct {
		name string
		dst  *int64
	}{{"from", &from}, {"to", &to}} {
		v := q.Get(p.name)
		if v == "" {
			return 0, 0, fmt.Errorf("%s is required, in Unix milliseconds", p.name)
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return 0, 0, fmt.Errorf("%s %q is not a number of Unix milliseconds", p.name, v)
		}
		*p.dst = n
	}
	if from > to {
		return 0, 0, fmt.Errorf("from %d is after to %d", from, to)
	}
	return from, to, nil
}

// clockOffset is how far the supervisor's clock is from this one, as one
// request and its answer show it.
type clockOffset struct {
	OffsetUS float64 `json:"offset_us"` // the supervisor's clock minus the client's
	ErrorUS  float64 `json:"error_us"`  // half the round trip: the most the offset can be off by
}

// measureOffset estimates the offset from a request sent at send, answered
// when the supervisor's clock read serverNS (Unix ns) and received at recv, on
// the assumption that the answer was made half way.
func measureOffset(send, recv time.Time, serverNS int64) clockOffset {
	rtt := recv.Sub(send)
	mid := send.UnixNano() + rtt.Nanoseconds()/2
	return clockOffset{
		OffsetUS: float64(serverNS-mid) / float64(time.Microsecond),
		ErrorUS:  float64(rtt) / 2 / float64(time.Microsecond),
	}
}

// duration is the offset as a time.Duration, to the nearest ns.
func (o clockOffset) duration() time.Duration {
	return time.Duration(math.Round(o.OffsetUS * float64(time.Microsecond)))
}

const (
	// wakeSlack is how far past the measured part, on each side, a client asks
	// /wake for seconds.
	wakeSlack = time.Second
	// wakeShiftTolerance is how far the offset measured on an answer may be from
	// the one its request's range was built with before the range is built again.
	wakeShiftTolerance = 250 * time.Millisecond
	// wakeAsks is the most /wake requests a run makes: the first measures the
	// offset, the next is built with it, and one more is for an offset that is
	// still moving.
	wakeAsks = 3
)

// wakeSettleSlack is how long past the end of a second the supervisor needs to
// have recorded a wake due in the next one, which is due a tick after it starts.
const wakeSettleSlack = 10 * time.Millisecond

// wakeWait returns how much longer the supervisor must run, counted from the
// moment its clock read nowNS (Unix ns), before every second that overlaps a
// window ending at end on the client's clock is finished and so served, or 0
// if they all are. offset is the supervisor's clock minus the client's.
func wakeWait(nowNS int64, end time.Time, offset time.Duration) time.Duration {
	endNS := end.UnixNano() + offset.Nanoseconds()
	// The second after the one that holds the window's last instant.
	next := ((endNS-1)/int64(time.Second) + 1) * int64(time.Second)
	return time.Duration(max(next+int64(wakeSettleSlack)-nowNS, 0))
}

// wakeFile is a run's wake file: the supervisor's answer and the clock offset
// that aligns it with this machine.
type wakeFile struct {
	wakeAnswer
	Clock clockOffset `json:"clock"`
}

// wakeForWindow returns the wakes of the seconds of rows that overlap a window
// that starts at windowStart on the client's clock and lasts windowLen: their
// count, the largest p99 of a second and the largest max. offset is the
// supervisor's clock minus the client's, so the window lies on the
// supervisor's clock from windowStart plus offset. The largest p99 is an upper
// bound of the p99 of all the wakes, as no more than 1% of a second's wakes lie
// above its p99. A second that is not in rows counts nothing: the supervisor
// fills the gaps of its answer, so a missing one lies outside the answer or was
// not finished. seen is false if no second of rows overlaps the window, when
// the meter has nothing to say about it; a stalled meter has seconds of n 0
// there instead.
func wakeForWindow(rows []wakeRow, offset time.Duration, windowStart time.Time, windowLen time.Duration) (n int, p99, worst float64, seen bool) {
	if windowLen <= 0 {
		return 0, 0, 0, false
	}
	start := windowStart.UnixNano() + offset.Nanoseconds()
	end := start + windowLen.Nanoseconds()
	for _, r := range rows {
		if r.Second*int64(time.Second) < end && (r.Second+1)*int64(time.Second) > start {
			n += r.N
			p99, worst = max(p99, r.P99), max(worst, r.Max)
			seen = true
		}
	}
	return n, p99, worst, seen
}
