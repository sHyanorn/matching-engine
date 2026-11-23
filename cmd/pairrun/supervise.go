package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	engineStopWait  = 15 * time.Second // after SIGTERM, before SIGKILL
	engineReadyWait = 30 * time.Second
	engineHTTPAddr  = "0.0.0.0:8080"
	engineOEAddr    = "0.0.0.0:9000"
)

// What /engine-log serves of an engine's output: a file of at most
// engineLogLimit bytes whole, else its first engineLogHead bytes, a line that
// says how much was left out, and its last engineLogTail bytes. The client
// finds a cut by engineLogCutFormat, so the note is built from it.
const (
	engineLogLimit     = 64 << 10
	engineLogHead      = 16 << 10
	engineLogTail      = engineLogLimit - engineLogHead
	engineLogCutPrefix = "[engine log cut: "
	engineLogCutFormat = engineLogCutPrefix + "%d of %d bytes omitted]"
)

// engineLogName is the file that the output of the engine of the nth start
// goes to in work.
func engineLogName(work string, n int) string {
	return filepath.Join(work, fmt.Sprintf("engine-%03d.log", n))
}

// engineLogCutNote is the separator of a cut log: a line of its own, however
// the head ends.
func engineLogCutNote(omitted, total int64) string {
	return fmt.Sprintf("\n"+engineLogCutFormat+"\n", omitted, total)
}

// engineLogCut returns the note line of a log that /engine-log cut, and
// whether it is one. The head is served redacted and so may be shorter than
// engineLogHead: the note is the line, within the first engineLogHead bytes,
// that engineLogCutNote makes for a file of more than engineLogLimit bytes. A
// log that quotes another number is not cut.
func engineLogCut(body []byte) (string, bool) {
	marker := []byte("\n" + engineLogCutPrefix)
	window := body[:min(len(body), engineLogHead+len(marker))]
	for off := 0; ; {
		i := bytes.Index(window[off:], marker)
		if i < 0 {
			return "", false
		}
		line, _, _ := bytes.Cut(body[off+i+1:], []byte("\n"))
		var omitted, total int64
		if _, err := fmt.Sscanf(string(line), engineLogCutFormat, &omitted, &total); err == nil &&
			omitted > 0 && total-omitted == engineLogLimit && string(line) == fmt.Sprintf(engineLogCutFormat, omitted, total) {
			return string(line), true
		}
		off += i + 1
	}
}

// superviseOptions are what `pairrun supervise` is given.
type superviseOptions struct {
	Engine    string // the engine binary
	Loadgen   string // the loadgen binary, for the loopback run
	Listen    string // the control address
	Work      string // engine data, engine logs and the loopback files
	EnvFile   string // text that describes this machine, served to the client; "" for none
	Params    params
	Overrides map[string]string // the environment overrides that differ from the defaults
	httpAddr  string            // tests: the engine's HTTP address, instead of engineHTTPAddr
	oeAddr    string            // tests: the engine's OE1 address, instead of engineOEAddr
	src       sources           // tests: the files the host is read from, instead of procSources
	clockRead func() float64    // tests: the cost of a clock read, instead of measureClockRead
}

// engineConfig is what a configuration adds to an engine's start, to its
// environment and its flags.
type engineConfig struct {
	env  []string
	args []string
}

// engineConfigs is the only place that defines the configurations of the
// engine; the client asks for one by name and passes nothing else. goMemLimit
// is the GOMEMLIMIT of gogc-off, in bytes.
func engineConfigs(goMemLimit int64) map[string]engineConfig {
	return map[string]engineConfig{
		engineNormal: {},
		engineGOGC:   {env: []string{"GOGC=off", "GOMEMLIMIT=" + strconv.FormatInt(goMemLimit, 10)}},
		engineSpin:   {args: []string{"-oe-spin"}},
	}
}

// supervisor runs the engine on this machine for the client on the other,
// through a control port that has no authentication. It therefore serves
// the routes of ServeHTTP and nothing else, and runs only the binaries it was
// started with in the configurations of its table. One route serves the text
// of the -env file as it is, so that file must hold nothing that is secret;
// /counters and /wake serve numbers only, and /wake the wake meter's own
// placement.
type supervisor struct {
	ctx        context.Context // ends with the process
	out        io.Writer
	engineBin  string
	loadgenBin string
	work       string
	httpAddr   string // the engine's listen addresses
	oeAddr     string
	params     params
	src        sources
	lookPath   func(string) (string, error)
	now        func() time.Time
	stopWait   time.Duration
	readyWait  time.Duration
	memLimit   int64
	goMemLimit int64
	clockNS    float64
	wake       *wakeMeter // starts when the supervisor is ready
	envFile    string
	overrides  map[string]string
	configs    map[string]engineConfig
	op         sync.Mutex // one start, stop or loopback run at a time
	mu         sync.Mutex // guards the fields below
	ready      bool
	starts     int // every start, the loopback run's included
	config     string
	cmd        *exec.Cmd
	exited     chan struct{} // closed once cmd has exited
	dataDir    string
}

func newSupervisor(ctx context.Context, o superviseOptions, out io.Writer) (*supervisor, error) {
	s := &supervisor{
		ctx: ctx, out: out, params: o.Params, src: procSources, lookPath: exec.LookPath, now: time.Now,
		stopWait: engineStopWait, readyWait: engineReadyWait,
		httpAddr: orDefault(o.httpAddr, engineHTTPAddr), oeAddr: orDefault(o.oeAddr, engineOEAddr),
	}
	if o.src != (sources{}) {
		s.src = o.src
	}
	var err error
	for _, p := range []struct {
		dst *string
		src string
	}{{&s.engineBin, o.Engine}, {&s.loadgenBin, o.Loadgen}, {&s.work, o.Work}} {
		if *p.dst, err = filepath.Abs(p.src); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(s.work, 0o755); err != nil {
		return nil, err
	}
	if s.memLimit, err = memoryLimit(s.src); err != nil {
		return nil, err
	}
	s.goMemLimit = s.memLimit / 2
	s.configs = engineConfigs(s.goMemLimit)
	s.overrides = o.Overrides
	if o.EnvFile != "" {
		if s.envFile, err = filepath.Abs(o.EnvFile); err != nil {
			return nil, err
		}
	}
	read := o.clockRead
	if read == nil {
		read = measureClockRead
	}
	s.clockNS = read()
	place := wakePlacement{CPU: -1, Siblings: []int{}}
	if allowed, err := allowedCPUs(s.src.status); err != nil {
		place.Note = "no meter: the CPUs this process may use are unreadable: " + err.Error()
	} else {
		place = placeWake(s.src.sysfs, allowed)
	}
	s.wake = newWakeMeter(place)
	return s, nil
}

// orDefault returns v, or def if v is empty.
func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// logf writes a line to the supervisor's stdout. A line that cannot be
// written is not worth failing a start for.
func (s *supervisor) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(s.out, format+"\n", args...)
}

// supervise runs the supervisor of `pairrun supervise` until ctx ends.
func supervise(ctx context.Context, o superviseOptions, stdout io.Writer) error {
	s, err := newSupervisor(ctx, o, stdout)
	if err != nil {
		return err
	}
	return s.serve(o.Listen)
}

// serve serves the control port on listen, makes the loopback run, starts the
// wake meter and then serves until the supervisor's context ends, when it stops
// the engine and the meter.
func (s *supervisor) serve(listen string) (err error) {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	// /start takes up to 45 s: the engine's stop, then its start.
	hs := &http.Server{
		Handler: s, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 2 * time.Minute, MaxHeaderBytes: 8 << 10,
	}
	served := make(chan error, 1)
	go func() { served <- hs.Serve(ln) }()
	meterCtx, stopMeter := context.WithCancel(s.ctx)
	var meterDone <-chan struct{}
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err = errors.Join(err, hs.Shutdown(shutdown), s.stopEngine())
		stopMeter()
		if meterDone != nil {
			<-meterDone
		}
	}()
	s.logf("listening on %s", ln.Addr())

	if err := s.loopback(); err != nil {
		if s.ctx.Err() != nil {
			return nil
		}
		return err
	}
	s.mu.Lock()
	s.ready = true
	s.mu.Unlock()
	meterDone = s.wake.start(meterCtx)
	s.logf("ready")
	select {
	case <-s.ctx.Done():
		return nil
	case err := <-served:
		return err
	}
}

// ServeHTTP serves the control routes and answers every other path or
// method with 404.
func (s *supervisor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method + " " + r.URL.Path {
	case "GET /host":
		s.serveRecord(w)
	case "GET /counters":
		s.serveCounters(w)
	case "GET /wake":
		s.serveWake(w, r)
	case "POST /start":
		s.serveStart(w, r)
	case "POST /stop":
		s.serveStop(w)
	case "GET /loopback.json":
		http.ServeFile(w, r, filepath.Join(s.work, "loopback.json"))
	case "GET /loopback.txt":
		http.ServeFile(w, r, filepath.Join(s.work, "loopback.txt"))
	case "GET /engine-env.txt":
		s.serveEnvText(w, r)
	case "GET /engine-log":
		s.serveEngineLog(w, r)
	default:
		http.NotFound(w, r)
	}
}

// serveEngineLog serves what the engine of the start numbered by the query's
// start wrote to its stdout and stderr, bounded as engineLogLimit says. The
// engine has not stopped when the client reads its log, so its lines at
// shutdown are not in it. A start is a decimal number from 1 to the count of
// starts, and the file name is built from the number alone: the route takes
// no path, so it serves only files that startEngine named.
//
// The served text is redacted (redactAddresses): the results are published,
// and the log names the machines that connected to the engine. The file stays
// as the engine wrote it, and the note counts that file's bytes, not the
// served ones.
func (s *supervisor) serveEngineLog(w http.ResponseWriter, r *http.Request) {
	arg := r.URL.Query().Get("start")
	n, err := strconv.ParseUint(arg, 10, 64) // no sign, no separators
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		http.Error(w, fmt.Sprintf("start %q is not a number", arg), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	starts := s.starts
	s.mu.Unlock()
	if err != nil || n < 1 || n > uint64(starts) {
		http.Error(w, fmt.Sprintf("no start %q: the supervisor has made %d", arg, starts), http.StatusNotFound)
		return
	}
	body, err := readEngineLog(engineLogName(s.work, int(n)))
	switch {
	case errors.Is(err, os.ErrNotExist):
		http.Error(w, fmt.Sprintf("no log of start %d", n), http.StatusNotFound)
	case err != nil:
		http.Error(w, "reading the engine log: "+err.Error(), http.StatusInternalServerError)
	default:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(body) // a client that has gone has no use for an error
	}
}

// readEngineLog reads the file name as /engine-log serves it. The engine may
// still be appending, so the size is read once and every range lies below it.
func readEngineLog(name string) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // only read
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()
	if size <= engineLogLimit {
		body := make([]byte, size)
		if err := readRange(f, body, 0); err != nil {
			return nil, err
		}
		return redactAddresses(body), nil
	}
	head, tail := make([]byte, engineLogHead), make([]byte, engineLogTail)
	if err := errors.Join(readRange(f, head, 0), readRange(f, tail, size-engineLogTail)); err != nil {
		return nil, err
	}
	body := append(redactAddresses(head), engineLogCutNote(size-engineLogLimit, size)...)
	return append(body, redactAddresses(tail)...), nil
}

// readRange fills p from the file at off.
func readRange(f io.ReaderAt, p []byte, off int64) error {
	n, err := f.ReadAt(p, off)
	if n == len(p) {
		return nil // the last byte of a file may come with io.EOF
	}
	return err
}

// serveEnvText serves the text of the file named by -env as it is, read at
// each request, on the unauthenticated control port: the file must hold
// nothing that is secret.
func (s *supervisor) serveEnvText(w http.ResponseWriter, r *http.Request) {
	if s.envFile == "" {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, s.envFile)
}

// serveCounters answers with a reading of the machine's CPU counters and, if
// an engine runs, of its threads' (counters.go). The engine is the one that
// /host reports; none of the process counters are read when there is none.
func (s *supervisor) serveCounters(w http.ResponseWriter) {
	s.mu.Lock()
	_, pid := s.runningEngine()
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(readCounters(s.src, pid)) // a client that has gone has no use for an error
}

// serveWake answers with the wake meter's finished seconds from the query's
// from to its to, in Unix milliseconds (wake.go).
func (s *supervisor) serveWake(w http.ResponseWriter, r *http.Request) {
	from, to, err := wakeRange(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a := s.wake.answer(from, to)
	a.NowUnixNS = s.now().UnixNano() // as late as it can be: the client puts it half way between its request and its answer
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(a) // a client that has gone has no use for an error
}

func (s *supervisor) serveRecord(w http.ResponseWriter) {
	h, err := s.record()
	if err != nil {
		http.Error(w, "reading the host: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h) // a client that has gone has no use for an error
}

func (s *supervisor) serveStart(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("config")
	cfg, ok := s.configs[name]
	if !ok {
		http.Error(w, fmt.Sprintf("unknown config %q, want one of %s", name, strings.Join(slices.Sorted(maps.Keys(s.configs)), ", ")),
			http.StatusBadRequest)
		return
	}
	if !s.isReady() {
		http.Error(w, "not ready: the loopback run has not finished", http.StatusServiceUnavailable)
		return
	}
	s.op.Lock()
	defer s.op.Unlock()
	// The client may have given up while this request waited for the lock;
	// a restart now would be for nobody.
	if r.Context().Err() != nil {
		return
	}
	if err := s.startEngine(name, cfg, nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.serveRecord(w)
}

func (s *supervisor) serveStop(w http.ResponseWriter) {
	if !s.isReady() {
		http.Error(w, "not ready: the loopback run has not finished", http.StatusServiceUnavailable)
		return
	}
	s.op.Lock()
	defer s.op.Unlock()
	if err := s.stopEngine(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.serveRecord(w)
}

func (s *supervisor) isReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready
}

// record describes the host now. The engine counts as running only until it
// exits.
func (s *supervisor) record() (hostRecord, error) {
	m, err := readMachine(s.src, s.now())
	if err != nil {
		return hostRecord{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h := hostRecord{
		Ready: s.ready, Starts: s.starts, machine: m, MemLimitBytes: s.memLimit, GoMemLimitBytes: s.goMemLimit,
		ClockReadNS: s.clockNS, Overrides: s.overrides,
	}
	h.Config, h.EnginePID = s.runningEngine()
	return h, nil
}

// runningEngine returns the configuration and the PID of the engine, or "" and
// 0 if none runs. The engine counts as running only until it exits. The caller
// holds s.mu.
func (s *supervisor) runningEngine() (config string, pid int) {
	if s.cmd == nil {
		return "", 0
	}
	select {
	case <-s.exited:
		return "", 0
	default:
		return s.config, s.cmd.Process.Pid
	}
}

// startEngine stops the running engine, gives a new one an empty data
// directory and starts it as the configuration name, with prefix in front of
// its command, and returns once it listens on both ports. The caller holds
// s.op.
func (s *supervisor) startEngine(name string, cfg engineConfig, prefix []string) error {
	if err := s.stopEngine(); err != nil {
		return err
	}
	s.mu.Lock()
	s.starts++
	n := s.starts
	s.mu.Unlock()
	dir, err := os.MkdirTemp(s.work, "data-")
	if err != nil {
		return err
	}
	logName := engineLogName(s.work, n)
	logFile, err := os.Create(logName)
	if err != nil {
		return errors.Join(err, os.RemoveAll(dir))
	}
	argv := append(slices.Clone(prefix), s.engineBin, "-addr", s.httpAddr, "-oe-addr", s.oeAddr, "-data", dir)
	argv = append(argv, cfg.args...)
	cmd := exec.Command(argv[0], argv[1:]...)
	// An empty environment, not this process's: the engine gets what its
	// configuration names and nothing else.
	cmd.Env = append([]string{}, cfg.env...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		return errors.Join(fmt.Errorf("starting the engine: %w", err), logFile.Close(), os.RemoveAll(dir))
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait() // how it ended is in its log
		_ = logFile.Close()
		close(exited)
	}()
	s.mu.Lock()
	s.cmd, s.exited, s.dataDir, s.config = cmd, exited, dir, name
	s.mu.Unlock()
	if err := s.waitListening(exited); err != nil {
		return errors.Join(fmt.Errorf("%w (log: %s)", err, logName), s.stopEngine())
	}
	return nil
}

// waitListening waits until the engine accepts connections on both ports.
func (s *supervisor) waitListening(exited <-chan struct{}) error {
	limit := time.NewTimer(s.readyWait)
	defer limit.Stop()
	for _, addr := range []string{dialAddr(s.httpAddr), dialAddr(s.oeAddr)} {
		for {
			conn, err := net.DialTimeout("tcp", addr, time.Second)
			if err == nil {
				_ = conn.Close() // only to see that it accepts
				break
			}
			select {
			case <-exited:
				return fmt.Errorf("the engine exited before it listened on %s", addr)
			case <-s.ctx.Done():
				return s.ctx.Err()
			case <-limit.C:
				return fmt.Errorf("the engine did not listen on %s within %v", addr, s.readyWait)
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	select {
	case <-exited: // the ports were another process's
		return errors.New("the engine exited while the ports were checked")
	default:
		return nil
	}
}

// stopEngine stops the running engine, if any, with SIGTERM and after
// stopWait with SIGKILL, and removes its data directory.
func (s *supervisor) stopEngine() error {
	s.mu.Lock()
	cmd, exited, dir := s.cmd, s.exited, s.dataDir
	s.cmd, s.exited, s.dataDir, s.config = nil, nil, "", ""
	s.mu.Unlock()
	if cmd == nil {
		return nil
	}
	select {
	case <-exited:
	default:
		_ = cmd.Process.Signal(syscall.SIGTERM) // it may have exited just now, which is as good
		select {
		case <-exited:
		case <-time.After(s.stopWait):
			_ = cmd.Process.Kill()
			<-exited
		}
	}
	return os.RemoveAll(dir)
}

// loopback measures the engine over this machine's own loopback, as the
// matrix's R1 does over the network: the same run, so that the pair's runs
// can be set against what the engine and loadgen do without a network.
// Loadgen takes this process's CPUs and spins on the two highest of them, so
// the engine is kept off those. Whether loadgen finds the run valid is in its
// report; only a run that cannot be made fails.
func (s *supervisor) loopback() error {
	s.op.Lock()
	defer s.op.Unlock()
	allowed, err := allowedCPUs(s.src.status)
	if err != nil {
		return err
	}
	var prefix []string
	if list := engineCPUs(allowed); list != "" {
		taskset, err := s.lookPath("taskset")
		if err != nil {
			return fmt.Errorf("the loopback run keeps the engine on CPUs %s with taskset: %w", list, err)
		}
		prefix = []string{taskset, "-c", list}
	}
	jsonName, txtName := filepath.Join(s.work, "loopback.json"), filepath.Join(s.work, "loopback.txt")
	for _, name := range []string{jsonName, txtName} {
		if err := os.Remove(name); err != nil && !os.IsNotExist(err) { // a stale file must not pass for this run's
			return err
		}
	}
	if err := s.startEngine(engineNormal, s.configs[engineNormal], prefix); err != nil {
		return fmt.Errorf("loopback run: %w", err)
	}
	runErr := s.runLoopbackLoadgen(jsonName, txtName)
	return errors.Join(runErr, s.stopEngine())
}

func (s *supervisor) runLoopbackLoadgen(jsonName, txtName string) error {
	r1 := configNamed(planConfigs(s.params, dialAddr(s.oeAddr), dialAddr(s.httpAddr)), baseConfig)
	txt, err := os.Create(txtName)
	if err != nil {
		return err
	}
	defer func() { _ = txt.Close() }() // loadgen's output is read back as written
	ctx, cancel := context.WithTimeout(s.ctx, time.Duration(r1.Warmup)+time.Duration(r1.Duration)+loadgenSlack)
	defer cancel()
	cmd := loadgenCommand(ctx, s.loadgenBin, loadgenArgs(r1, jsonName))
	cmd.Stdout, cmd.Stderr = txt, txt
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting loadgen for the loopback run: %w", err)
	}
	waitErr := cmd.Wait()
	if err := s.ctx.Err(); err != nil {
		return err
	}
	s.logf("loopback run finished: %v", waitErr)
	return nil
}

// dialAddr is the address to connect to for an engine listening on listen:
// this machine, if listen names every interface.
func dialAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}
