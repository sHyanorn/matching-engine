package spin

import (
	"maps"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// allowedOrSkip returns the CPUs this process may run on, or skips the
// test if there are fewer than two: pinning is then not told apart from
// the thread's own CPU.
func allowedOrSkip(t *testing.T) []int {
	t.Helper()
	allowed, err := allowedCPUs()
	if err != nil {
		t.Fatal(err)
	}
	if len(allowed) < 2 {
		t.Skipf("only CPUs %v are allowed", allowed)
	}
	return allowed
}

// threadMask returns the CPUs the calling thread may run on, as /proc has
// them, or why it does not.
func threadMask() string {
	status, err := os.ReadFile("/proc/thread-self/status")
	if err != nil {
		return err.Error()
	}
	for line := range strings.Lines(string(status)) {
		if list, ok := strings.CutPrefix(line, "Cpus_allowed_list:"); ok {
			return strings.TrimSpace(list)
		}
	}
	return "no Cpus_allowed_list in " + string(status)
}

// threadMasks returns the CPUs the process's threads may run on, as /proc
// gives them: each list some thread has. The main thread is left out: a
// goroutine that ends locked to it parks it for good instead of ending it.
func threadMasks(t *testing.T) map[string]bool {
	t.Helper()
	tasks, err := os.ReadDir("/proc/self/task")
	if err != nil {
		t.Skip(err)
	}
	masks := map[string]bool{}
	for _, task := range tasks {
		if task.Name() == strconv.Itoa(os.Getpid()) {
			continue
		}
		status, err := os.ReadFile("/proc/self/task/" + task.Name() + "/status")
		if err != nil {
			continue // the thread has exited
		}
		for line := range strings.Lines(string(status)) {
			if list, ok := strings.CutPrefix(line, "Cpus_allowed_list:"); ok {
				masks[strings.TrimSpace(list)] = true
			}
		}
	}
	return masks
}

func TestPinKeepsTheThreadOnItsCore(t *testing.T) {
	allowed := allowedOrSkip(t)
	cpu := allowed[len(allowed)-1]
	log, out := textLogger()
	p := NewPool([]int{cpu}, log)
	got := make(chan string, 1)
	go func() { // locked for good: the thread ends with the goroutine
		l := p.Reserve("symbol A")
		defer l.Release()
		if !l.Pin() {
			got <- ""
			return
		}
		got <- threadMask()
	}()
	mask := <-got
	if mask == "" {
		t.Skip("this system does not let a thread be pinned to a CPU")
	}
	if mask != strconv.Itoa(cpu) {
		t.Errorf("the pinned thread may run on %q, want only CPU %d", mask, cpu)
	}
	if want := "msg=\"spin: pinned\" cpu=" + strconv.Itoa(cpu) + " for=\"symbol A\"\n"; out.String() != want {
		t.Errorf("logged %q, want %q", out.String(), want)
	}
}

// A pinned thread ends with its goroutine, so that none runs other work
// on one CPU afterwards. The main thread is no test of it: a goroutine that
// ends locked to the main thread parks it instead, so a goroutine that ran
// there is run again, on another thread.
func TestPinnedThreadEndsWithItsGoroutine(t *testing.T) {
	allowed := allowedOrSkip(t)
	before := threadMasks(t)
	if len(before) != 1 {
		t.Skipf("threads already differ in where they may run: %v", before)
	}
	p := NewPool(allowed[len(allowed)-1:], nil)
	// While this goroutine holds its thread, the one it starts runs on another.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	for range 5 {
		tid := make(chan int, 1)
		go func() {
			l := p.Reserve("worker")
			defer l.Release()
			if !l.Pin() {
				tid <- -1
				return
			}
			tid <- unix.Gettid()
		}()
		switch id := <-tid; id {
		case -1:
			t.Skip("this system does not let a thread be pinned to a CPU")
		case os.Getpid():
			continue
		}
		for wait := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			after := threadMasks(t)
			if maps.Equal(after, before) {
				return
			}
			if time.Now().After(wait) {
				t.Fatalf("after the goroutine ended, threads may run on %v; before, %v", after, before)
			}
		}
	}
	t.Skip("every goroutine ran on the main thread")
}

// A pin the system refuses leaves the goroutine free to move and the core
// free to be taken.
func TestPinRefused(t *testing.T) {
	before := make(chan string, 1)
	after := make(chan string, 1)
	log, out := textLogger()
	p := NewPool([]int{1 << 20}, log) // no such CPU
	go func() {
		before <- threadMask()
		l := p.Reserve("symbol A")
		if l.Pin() {
			t.Error("pinned to a CPU that does not exist")
		}
		after <- threadMask()
	}()
	if b, a := <-before, <-after; a != b {
		t.Errorf("the thread may run on %q after a refused pin, was %q", a, b)
	}
	if p.Free() != 1 {
		t.Errorf("%d cores free after a refused pin, want the core given back", p.Free())
	}
	if got := out.String(); !strings.Contains(got, `msg="spin: cannot pin, running without spin"`) || !strings.Contains(got, `for="symbol A"`) {
		t.Errorf("logged %q, want why the pin failed", got)
	}
}

func TestNew(t *testing.T) {
	allowed, err := allowedCPUs()
	if err != nil {
		t.Fatal(err)
	}
	log, out := textLogger()
	p, err := New(log)
	if err != nil {
		t.Fatal(err)
	}
	cpus := p.CPUs()
	if !slices.IsSorted(cpus) || len(cpus) > max(runtime.GOMAXPROCS(0)-1, 0) {
		t.Errorf("pool %v: want ascending CPUs, at most GOMAXPROCS-1 = %d", cpus, runtime.GOMAXPROCS(0)-1)
	}
	for _, cpu := range cpus {
		if !slices.Contains(allowed, cpu) {
			t.Errorf("CPU %d is in the pool but is not allowed: %v", cpu, allowed)
		}
	}
	if want := `msg="spin mode on" cores=` + strconv.Itoa(len(Cores(sysfsCPU, allowed))); !strings.HasPrefix(out.String(), want) {
		t.Errorf("logged %q, want a line that starts %s", out.String(), want)
	}
}

// Without GOMAXPROCS-1 cores to give, a pool has fewer: the rest of the
// program keeps a P.
func TestNewLeavesAPForTheRestOfTheProgram(t *testing.T) {
	allowed, err := allowedCPUs()
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	p, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cpus := p.CPUs(); len(cpus) != 0 {
		t.Errorf("pool %v with GOMAXPROCS 1 from %v, want none", cpus, allowed)
	}
}
