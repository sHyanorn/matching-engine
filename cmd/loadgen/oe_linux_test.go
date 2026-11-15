package main

import (
	"context"
	"maps"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sHyanorn/matching-engine/internal/oe"
)

func TestSpinReadAllocatesNothing(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	s, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var stop atomic.Bool
	read, ok := spinRead(c, &stop)
	if !ok {
		t.Fatal("spinRead does not spin on a TCP connection")
	}
	pong := oe.Pong{ClientSeq: 1}.Append(nil)
	buf := make([]byte, 64)
	allocs := testing.AllocsPerRun(1000, func() {
		if _, err := s.Write(pong); err != nil {
			t.Fatal(err)
		}
		for got := 0; got < len(pong); {
			n, err := read(buf[got:])
			if err != nil {
				t.Fatal(err)
			}
			got += n
		}
	})
	if allocs != 0 {
		t.Errorf("%v allocations per spinning read, want 0", allocs)
	}
}

// A pinned thread does not outlive its run: afterwards every thread of
// the process may run on the CPUs it could run on before.
func TestOEPinningEndsWithTheRun(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a load in real time")
	}
	before := threadMasks(t)
	if len(before) != 1 {
		t.Skipf("threads already differ in where they may run: %v", before)
	}
	cfg := oeConfig(listenOE(t, fakeOE{}))
	cfg.duration = 100 * time.Millisecond
	r, err := runOE(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if r.pinned == [2]int{-1, -1} {
		t.Skip("this machine allows no pinning")
	}
	for wait := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		after := threadMasks(t)
		if len(after) == 1 && maps.Equal(after, before) || time.Now().After(wait) {
			if !maps.Equal(after, before) {
				t.Errorf("after a run pinned to %v, threads may run on %v; before, %v", r.pinned, after, before)
			}
			return
		}
	}
}

// threadMasks returns the CPUs the process's threads may run on, as
// /proc gives them: each list some thread has. The main thread is left
// out: a goroutine that ends locked to it parks it for good instead of
// ending it, so it never runs Go code again.
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
