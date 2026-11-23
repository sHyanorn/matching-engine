//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The tests run the test binary as the engine, loadgen and pairrun itself,
// through symbolic links named for them (fakeBinary): TestMain looks at the
// name it was started as. The engine and loadgen the code under test starts
// are therefore real processes, with real arguments and environments, that
// load nothing.
func TestMain(m *testing.M) {
	switch name := filepath.Base(os.Args[0]); {
	case strings.HasPrefix(name, "fake-engine"):
		fakeEngine(name)
	case name == "fake-loadgen":
		fakeLoadgen()
	case name == "fake-loadgen-env":
		fakeLoadgenEnv()
	case name == "pairrun":
		main()
		os.Exit(0)
	}
	// The race detector holds every exiting process for a second, which the
	// processes started here inherit.
	if err := os.Setenv("GORACE", "atexit_sleep_ms=0"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// fakeBinary makes a link to the test binary that runs as kind, one of
// "fake-engine", "fake-engine-mute", "fake-engine-deaf", "fake-loadgen",
// "fake-loadgen-env" and "pairrun", and returns its path.
func fakeBinary(t *testing.T, kind string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	link := filepath.Join(dir, kind)
	if err := os.Symlink(self, link); err != nil {
		t.Fatal(err)
	}
	return link
}

// stubReport is what the fake engine writes into its data directory once
// started: how it was started, and what its data directory held then.
type stubReport struct {
	Args           []string
	Env            []string
	EntriesAtStart []string
	CPUsAllowed    string
}

// fakeEngine stands in for the engine: it takes the engine's flags, writes a
// line to stdout and one to stderr that name its data directory, writes a
// stubReport to stub.json in the directory and listens on both addresses.
// "fake-engine-mute" never listens and "fake-engine-deaf" ignores SIGTERM.
func fakeEngine(kind string) {
	fs := flag.NewFlagSet("engine", flag.ContinueOnError)
	addr := fs.String("addr", "", "")
	oeAddr := fs.String("oe-addr", "", "")
	data := fs.String("data", "", "")
	fs.Bool("oe-spin", false, "")
	if err := fs.Parse(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	fmt.Println("fake engine stdout, data", *data)
	fmt.Fprintln(os.Stderr, "fake engine stderr, data", *data)
	entries, _ := os.ReadDir(*data)
	rep := stubReport{Args: os.Args[1:], Env: os.Environ(), EntriesAtStart: []string{}}
	for _, e := range entries {
		rep.EntriesAtStart = append(rep.EntriesAtStart, e.Name())
	}
	if status, err := os.ReadFile("/proc/self/status"); err == nil {
		for line := range strings.Lines(string(status)) {
			if v, ok := strings.CutPrefix(line, "Cpus_allowed_list:"); ok {
				rep.CPUsAllowed = strings.TrimSpace(v)
			}
		}
	}
	data1, _ := json.Marshal(rep)
	if err := os.WriteFile(filepath.Join(*data, "stub.json"), data1, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if kind == "fake-engine-mute" {
		select {}
	}
	if kind == "fake-engine-deaf" {
		signal.Ignore(syscall.SIGTERM)
	}
	for _, a := range []string{*addr, *oeAddr} {
		ln, err := net.Listen("tcp", a)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				_ = c.Close()
			}
		}()
	}
	if kind == "fake-engine-deaf" {
		select {} // Notify would stop the ignoring
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM)
	<-stop
	// The supervisor gives the engine an empty environment, so GORACE cannot
	// spare it the race detector's second at exit: it ends itself instead.
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
}

// fakeRun is what the fake loadgen does for one run.
type fakeRun struct {
	Report    json.RawMessage // written to -out
	RawReport string          // written to -out instead, if not empty: may be no JSON at all
	NoReport  bool            // write nothing
	Exit      int
	Hang      bool   // never exit
	WaitFile  string // do not start before this file exists
	Text      string // stdout
}

type fakeScript struct {
	Runs   map[string]fakeRun // by the report's file name without its number and extension: "R1-gogc-off-r3"
	NoDial bool               // do not connect to -oe and -http first
}

// The supervisor and the client give loadgen an empty environment, so a fake
// loadgen finds its script, and the log of its arguments, in its working
// directory, which it inherits (writeFakeScript).
const (
	fakeScriptFile = "fake-loadgen-script.json"
	fakeLogFile    = "fake-loadgen.log"
)

// exitNow ends a fake loadgen at once: it has no environment, so GORACE cannot
// spare it the race detector's second at exit.
func exitNow(code int) { syscall.Exit(code) }

// fakeLoadgen stands in for loadgen: it logs its arguments, looks its run up
// in the script by the name of its -out file, and does what the script says.
// Where it is given -oe or -http it first connects to them, as the real one
// does, unless the script says not to. Without a script it does nothing else.
func fakeLoadgen() {
	var out, oe, httpAddr string
	args := os.Args[1:]
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "-out":
			out = args[i+1]
		case "-oe":
			oe = args[i+1]
		case "-http":
			httpAddr = args[i+1]
		}
	}
	var script fakeScript
	data, err := os.ReadFile(fakeScriptFile)
	scripted := err == nil
	if scripted {
		if err := json.Unmarshal(data, &script); err != nil {
			fmt.Fprintln(os.Stderr, err)
			exitNow(4)
		}
		line, _ := json.Marshal(args)
		if f, err := os.OpenFile(fakeLogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			_, _ = f.Write(append(line, '\n'))
			_ = f.Close()
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(os.Stderr, err)
		exitNow(4)
	}
	for _, a := range []string{oe, httpAddr} {
		if a == "" || script.NoDial {
			continue
		}
		c, err := net.DialTimeout("tcp", a, 2*time.Second)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake loadgen:", err)
			exitNow(3)
		}
		_ = c.Close()
	}
	var run fakeRun
	if scripted {
		key := strings.TrimSuffix(filepath.Base(out), ".json")
		if _, rest, ok := strings.Cut(key, "-"); ok && key[0] >= '0' && key[0] <= '9' {
			key = rest
		}
		r, ok := script.Runs[key]
		if !ok {
			fmt.Fprintf(os.Stderr, "fake loadgen: no script for %q\n", key)
			exitNow(4)
		}
		run = r
	}
	for run.WaitFile != "" {
		if _, err := os.Stat(run.WaitFile); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if run.Hang {
		select {}
	}
	if run.Text != "" {
		fmt.Print(run.Text)
	}
	if !run.NoReport && out != "" {
		data := []byte(run.RawReport)
		if run.RawReport == "" {
			data = run.Report
		}
		if err := os.WriteFile(out, data, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			exitNow(4)
		}
	}
	exitNow(run.Exit)
}

// fakeLoadgenEnv stands in for loadgen and writes its environment, as a JSON
// array, to the file named by its -out with ".env" added.
func fakeLoadgenEnv() {
	var out string
	args := os.Args[1:]
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-out" {
			out = args[i+1]
		}
	}
	data, _ := json.Marshal(os.Environ())
	if err := os.WriteFile(out+".env", data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		exitNow(1)
	}
	exitNow(0)
}

// namesOfFakeLoadgenEnv reads what a "fake-loadgen-env" that was given -out
// report wrote, and returns the names of its environment's variables: a
// failing test must not print their values.
func namesOfFakeLoadgenEnv(t *testing.T, report string) []string {
	t.Helper()
	data, err := os.ReadFile(report + ".env")
	if err != nil {
		t.Fatalf("loadgen did not write its environment: %v", err)
	}
	var env []string
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		names = append(names, name)
	}
	return names
}

// syncBuffer is a bytes.Buffer that a test may read while a goroutine
// writes it.
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

// freeAddrs returns n loopback addresses that nothing listens on now. All n
// listeners are open before any is closed, so the addresses differ. Another
// process can still take one before the caller's server binds it.
func freeAddrs(t *testing.T, n int) []string {
	t.Helper()
	var lns []net.Listener
	defer func() {
		for _, ln := range lns {
			_ = ln.Close()
		}
	}()
	addrs := make([]string, n)
	for i := range addrs {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		lns = append(lns, ln)
		addrs[i] = ln.Addr().String()
	}
	return addrs
}

// freeAddr returns one address of freeAddrs.
func freeAddr(t *testing.T) string {
	t.Helper()
	return freeAddrs(t, 1)[0]
}

func TestFreeAddrs(t *testing.T) {
	addrs := freeAddrs(t, 64)
	seen := map[string]bool{}
	for _, a := range addrs {
		if seen[a] {
			t.Fatalf("address %s given twice in %v", a, addrs)
		}
		seen[a] = true
		ln, err := net.Listen("tcp", a)
		if err != nil {
			t.Fatalf("address %s is not free: %v", a, err)
		}
		_ = ln.Close()
	}
}

// eventually polls cond until it holds or the test's patience ends.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}
