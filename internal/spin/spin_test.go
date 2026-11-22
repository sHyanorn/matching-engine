package spin

import (
	"bytes"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeSysfs writes a cpuN/topology/thread_siblings_list for each CPU in
// lists, and nothing for a CPU that is not there.
func fakeSysfs(t *testing.T, lists map[int]string) string {
	t.Helper()
	root := t.TempDir()
	for cpu, list := range lists {
		dir := filepath.Join(root, "cpu"+strconv.Itoa(cpu), "topology")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "thread_siblings_list"), []byte(list+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestCores(t *testing.T) {
	pairs := map[int]string{0: "0,2", 1: "1,3", 2: "0,2", 3: "1,3"}
	ranges := map[int]string{0: "0-1", 1: "0-1", 2: "2-3", 3: "2-3"}
	tests := []struct {
		name    string
		lists   map[int]string // nil: no topology directory at all
		allowed []int
		want    []int
	}{
		{"no hyper-threading", map[int]string{0: "0", 1: "1", 2: "2", 3: "3"}, []int{0, 1, 2, 3}, []int{0, 1, 2, 3}},
		{"siblings listed by commas", pairs, []int{0, 1, 2, 3}, []int{0, 1}},
		{"siblings listed as ranges", ranges, []int{0, 1, 2, 3}, []int{0, 2}},
		{"four threads to a core", map[int]string{0: "0-1,8-9", 1: "0-1,8-9", 8: "0-1,8-9", 9: "0-1,8-9"}, []int{0, 1, 8, 9}, []int{0}},
		{"the mask excludes the first sibling", ranges, []int{1, 2, 3}, []int{1, 2}},
		{"the mask excludes the second sibling", pairs, []int{0, 2, 3}, []int{0, 3}},
		{"the mask excludes a whole core", ranges, []int{2, 3}, []int{2}},
		{"the mask keeps one thread of each core", ranges, []int{1, 3}, []int{1, 3}},
		{"the mask excludes every CPU", ranges, nil, nil},
		{"the mask is unsorted and repeats a CPU", ranges, []int{3, 1, 1, 0}, []int{0, 3}},
		{"no topology directory", nil, []int{0, 1, 2, 3}, []int{0, 1, 2, 3}},
		{"no topology directory and a mask", nil, []int{1, 3}, []int{1, 3}},
		{"topology for some CPUs", map[int]string{0: "0-1", 1: "0-1"}, []int{0, 1, 2, 3}, []int{0, 2, 3}},
		{"a list that is not one", map[int]string{0: "x", 1: "1-", 2: "3-2", 3: ""}, []int{0, 1, 2, 3}, []int{0, 1, 2, 3}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "absent")
			if tc.lists != nil {
				root = fakeSysfs(t, tc.lists)
			}
			if got := Cores(root, tc.allowed); !slices.Equal(got, tc.want) {
				t.Errorf("Cores(%v) = %v, want %v", tc.allowed, got, tc.want)
			}
		})
	}
}

func TestLowestCPU(t *testing.T) {
	tests := []struct {
		list string
		want int
		ok   bool
	}{
		{"3", 3, true},
		{"0-1", 0, true},
		{"4,6-7\n", 4, true},
		{"8-9,0-1", 0, true},
		{"", 0, false},
		{"a", 0, false},
		{"1-", 0, false},
		{"-1", 0, false},
		{"3-2", 0, false},
		{"1,,2", 0, false},
	}
	for _, tc := range tests {
		if got, ok := lowestCPU(tc.list); got != tc.want || ok != tc.ok {
			t.Errorf("lowestCPU(%q) = %d, %v; want %d, %v", tc.list, got, ok, tc.want, tc.ok)
		}
	}
}

// cpuOf is the CPU l holds.
func cpuOf(l *Lease) int { return l.pool.cpus[l.slot] }

func TestPoolHandsOutCoresInOrder(t *testing.T) {
	cpus := []int{2, 5, 7}
	p := NewPool(cpus, nil)
	if got := p.CPUs(); !slices.Equal(got, cpus) {
		t.Fatalf("CPUs = %v, want %v", got, cpus)
	}
	p.CPUs()[0] = 99
	if got := p.CPUs(); !slices.Equal(got, cpus) {
		t.Errorf("CPUs shows %v after its result was changed, want %v", got, cpus)
	}
	var leases []*Lease
	for i, want := range cpus {
		if p.Free() != len(cpus)-i {
			t.Errorf("%d cores free before the reservation of core %d, want %d", p.Free(), i, len(cpus)-i)
		}
		l := p.Reserve("goroutine " + strconv.Itoa(i))
		if l == nil || cpuOf(l) != want {
			t.Fatalf("reservation %d: got %+v, want CPU %d", i, l, want)
		}
		leases = append(leases, l)
	}
	if l := p.Reserve("one too many"); l != nil || p.Free() != 0 {
		t.Fatalf("a reservation of a fourth core gave %+v with %d free, want none", l, p.Free())
	}
	// The core that comes back is the next one handed out, and a lease
	// released twice does not free the core of the one that took its place.
	leases[1].Release()
	if p.Free() != 1 {
		t.Fatalf("%d cores free after one release, want 1", p.Free())
	}
	again := p.Reserve("again")
	if again == nil || cpuOf(again) != 5 {
		t.Fatalf("after a release, got %+v, want CPU 5", again)
	}
	leases[1].Release()
	if p.Free() != 0 {
		t.Errorf("%d cores free after a second release of a lease, want 0: it freed the core of another", p.Free())
	}
	for _, l := range append(leases, again) {
		l.Release()
	}
	if p.Free() != len(cpus) {
		t.Errorf("%d cores free after every release, want %d", p.Free(), len(cpus))
	}
}

func TestPoolIsSafeForConcurrentUse(t *testing.T) {
	const cores, goroutines, rounds = 3, 16, 500
	p := NewPool([]int{0, 1, 2}, nil)
	var held, most atomic.Int64
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds {
				if l := p.Reserve("worker"); l != nil {
					n := held.Add(1)
					for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
					}
					held.Add(-1)
					l.Release()
				}
			}
		}()
	}
	wg.Wait()
	if most.Load() > cores {
		t.Errorf("%d cores held at once, the pool has %d", most.Load(), cores)
	}
	if p.Free() != cores {
		t.Errorf("%d cores free at the end, want %d", p.Free(), cores)
	}
}

// With no pool, or no lease, nothing spins and nothing fails.
func TestNilPoolAndLease(t *testing.T) {
	var p *Pool
	if l := p.Reserve("x"); l != nil {
		t.Errorf("a nil pool reserved %+v", l)
	}
	if p.CPUs() != nil || p.Free() != 0 {
		t.Errorf("a nil pool has CPUs %v and %d free, want none", p.CPUs(), p.Free())
	}
	a, b := net.Pipe()
	defer func() { _ = a.Close(); _ = b.Close() }()
	if got := p.Wrap(a); got != a {
		t.Errorf("a nil pool wrapped the connection: %T", got)
	}
	var l *Lease
	if l.Pin() {
		t.Error("a nil lease pinned")
	}
	l.Release()
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// textLogger logs to the returned buffer, one key=value line each.
func textLogger() (*slog.Logger, *syncBuffer) {
	var buf syncBuffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
		if len(groups) == 0 && (a.Key == slog.TimeKey || a.Key == slog.LevelKey) {
			return slog.Attr{}
		}
		return a
	}})
	return slog.New(h), &buf
}

func TestReserveSaysWhenNoCoreIsFree(t *testing.T) {
	log, out := textLogger()
	p := NewPool([]int{4}, log)
	l := p.Reserve("symbol A")
	if got := out.String(); got != "" {
		t.Fatalf("a reservation that worked logged %q", got)
	}
	if p.Reserve("symbol B") != nil {
		t.Fatal("a second reservation from a pool of one core worked")
	}
	if got, want := out.String(), `msg="spin: no free core, running without spin" for="symbol B"`+"\n"; got != want {
		t.Errorf("logged %q, want %q", got, want)
	}
	l.Release()
}
