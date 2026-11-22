// Package spin runs goroutines in spin mode: one bundle that pays only as a
// whole. The goroutine is locked to its OS thread, the thread is pinned to a
// CPU of its own, and the goroutine never blocks: it polls for its work. A
// locked goroutine that still blocks on a channel is slower than an unlocked
// one (golang/go#21827, 5.22 µs against 485 ns), so a goroutine that cannot
// be given a core does not spin at all and runs as it would without the mode.
//
// A Pool holds one CPU for each physical core the process may run on. A
// goroutine takes a core with Reserve, pins itself with Lease.Pin and gives
// the core back with Lease.Release when it ends. A goroutine that spins is
// locked to its thread for good, so its thread ends with it and is never
// reused for other work.
package spin

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// sysfsCPU is where Linux describes the CPUs.
const sysfsCPU = "/sys/devices/system/cpu"

// Pool hands out CPUs to goroutines that spin, lowest first. A nil *Pool is
// the mode switched off: it has no cores, so every goroutine runs as usual.
type Pool struct {
	logger *slog.Logger
	cpus   []int // the CPUs to hand out, in that order; never changes

	mu    sync.Mutex
	taken []bool // taken[i]: cpus[i] is leased
}

// NewPool returns a pool that hands out cpus in the order given. A nil
// logger logs nothing.
func NewPool(cpus []int, logger *slog.Logger) *Pool {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Pool{logger: logger, cpus: slices.Clone(cpus), taken: make([]bool, len(cpus))}
}

// New returns a pool of the physical cores of this machine that the process
// may run on, one CPU for each, lowest first, but never more than
// GOMAXPROCS-1: a spinning goroutine holds its P for good, and the rest of
// the program (the HTTP server, the fsync helpers, the garbage collector)
// needs one. It logs how many cores it found. It fails where spinning is not
// supported, which is any system but Linux.
func New(logger *slog.Logger) (*Pool, error) {
	allowed, err := allowedCPUs()
	if err != nil {
		return nil, fmt.Errorf("spin: listing the CPUs this process may run on: %w", err)
	}
	cores := Cores(sysfsCPU, allowed)
	pool := cores[:min(len(cores), max(runtime.GOMAXPROCS(0)-1, 0))]
	p := NewPool(pool, logger)
	p.logger.Info("spin mode on", "cores", len(cores), "pool", pool, "gomaxprocs", runtime.GOMAXPROCS(0))
	return p, nil
}

// Cores returns one CPU for each physical core among the allowed CPUs, in
// ascending order: the lowest allowed CPU of the core. sysfs is the
// directory Linux describes the CPUs in, /sys/devices/system/cpu. A core is
// the CPUs of its cpuN/topology/thread_siblings_list; a CPU with no readable
// list is a core of its own.
func Cores(sysfs string, allowed []int) []int {
	var cores []int
	seen := map[int]bool{} // cores found, by their lowest CPU
	for _, cpu := range slices.Compact(slices.Sorted(slices.Values(allowed))) {
		core := cpu
		path := filepath.Join(sysfs, "cpu"+strconv.Itoa(cpu), "topology", "thread_siblings_list")
		if list, err := os.ReadFile(path); err == nil {
			if low, ok := lowestCPU(string(list)); ok {
				core = low
			}
		}
		if !seen[core] {
			seen[core] = true
			cores = append(cores, cpu)
		}
	}
	return cores
}

// lowestCPU returns the lowest CPU of a kernel CPU list such as "0-1,4",
// and false if list is not one.
func lowestCPU(list string) (int, bool) {
	low := -1
	for part := range strings.SplitSeq(strings.TrimSpace(list), ",") {
		first, last, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(first)
		if err != nil || a < 0 {
			return 0, false
		}
		if isRange {
			if b, err := strconv.Atoi(last); err != nil || b < a {
				return 0, false
			}
		}
		if low < 0 || a < low {
			low = a
		}
	}
	return low, true
}

// CPUs returns the CPUs the pool hands out, in the order it does.
func (p *Pool) CPUs() []int {
	if p == nil {
		return nil
	}
	return slices.Clone(p.cpus)
}

// Free returns how many cores are not leased.
func (p *Pool) Free() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	free := 0
	for _, t := range p.taken {
		if !t {
			free++
		}
	}
	return free
}

// Reserve takes the first free core for the goroutine described by what,
// which is the one that logs it, and returns nil if none is free or p is
// nil. It is done by the goroutine that starts the one that spins, so that
// cores are handed out in the order goroutines are started.
func (p *Pool) Reserve(what string) *Lease {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	var l *Lease
	for i, t := range p.taken {
		if !t {
			p.taken[i] = true
			l = &Lease{pool: p, slot: i, what: what}
			break
		}
	}
	p.mu.Unlock()
	if l == nil {
		p.logger.Info("spin: no free core, running without spin", "for", what)
	}
	return l
}

// Wrap returns nc with a Read that Spin makes poll the socket, as a Poller;
// it returns nc itself if p is nil or nc has no socket to poll, as a pipe has
// not.
func (p *Pool) Wrap(nc net.Conn) net.Conn {
	if p == nil {
		return nc
	}
	return wrapConn(nc)
}

// Poller is the connection Wrap returns when it can poll a socket.
type Poller interface {
	net.Conn
	// Spin makes every later Read poll the socket until it has data, the
	// read deadline passes or the connection is closed, instead of waiting
	// to be woken. Only the goroutine that reads may call it, after it has
	// pinned itself. From then on a read deadline is kept by Read itself.
	Spin()
}

// Lease is a core reserved from a Pool. A nil *Lease is no core: Pin reports
// false and Release does nothing.
type Lease struct {
	pool *Pool
	slot int
	what string
	done bool // released; guarded by pool.mu
}

// Pin locks the calling goroutine to its OS thread and pins the thread to
// the leased core, and reports whether it did. Call it once, on the goroutine
// that is to spin, and never unlock the thread: the thread must end with the
// goroutine. If it reports false the goroutine and its thread are as they
// were and the core is released.
func (l *Lease) Pin() bool {
	if l == nil {
		return false
	}
	cpu := l.pool.cpus[l.slot]
	runtime.LockOSThread()
	if err := pinThread(cpu); err != nil {
		// A refused pin leaves the thread's CPUs as they were, so it may
		// serve other goroutines.
		runtime.UnlockOSThread()
		l.pool.logger.Warn("spin: cannot pin, running without spin", "cpu", cpu, "for", l.what, "err", err)
		l.Release()
		return false
	}
	l.pool.logger.Info("spin: pinned", "cpu", cpu, "for", l.what)
	return true
}

// Release gives the core back to the pool. Call it when the goroutine that
// holds the lease is done; calling it again does nothing.
func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	if !l.done {
		l.done = true
		l.pool.taken[l.slot] = false
	}
}
