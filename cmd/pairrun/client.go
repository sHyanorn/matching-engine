package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// The supervisor is polled every readyPoll for up to readyWait, as its
	// loopback run takes about 70 s at the defaults.
	readyPoll = 2 * time.Second
	readyWait = 15 * time.Minute
	// loadgenSlack is how long past its window a loadgen may take for its
	// setup and its end before the client kills it.
	loadgenSlack = 5 * time.Minute
	// startWait bounds /start: the supervisor takes up to 15 s to stop the
	// last engine and 30 s to start the next.
	startWait = 3 * time.Minute
	// startAttempts is how often a run asks for its engine. A start request
	// is safe to repeat: each makes a fresh engine.
	startAttempts = 3
)

// clientOptions are what `pairrun client` is given.
type clientOptions struct {
	Loadgen   string // the loadgen binary
	OE        string // the engine's OE1 address, as loadgen reaches it
	HTTP      string // the engine's HTTP address
	Ctl       string // the supervisor's control address
	Out       string // the results directory
	Deadline  time.Time
	Margin    time.Duration
	Params    params
	Overrides map[string]string // the environment overrides that differ from the defaults
}

// client runs the matrix against a supervisor and writes its results.
type client struct {
	clientOptions
	stdout       io.Writer
	now          func() time.Time
	pollEvery    time.Duration
	readyTimeout time.Duration
	slack        time.Duration  // how long past its window a loadgen may run before it is killed
	src          sources        // this machine's description
	clockRead    func() float64 // this machine's mean cost of a clock read, in ns
	http         *http.Client

	configs []config // of a round
	long    config
	records []runRecord
	lines   []byte     // runs.jsonl so far
	engine  hostRecord // the last record read from the supervisor

	clockNS             float64           // this machine's mean cost of a clock read
	supervisorOverrides map[string]string // from the first record read, nil before it
}

func newClient(o clientOptions, stdout io.Writer) *client {
	return &client{
		clientOptions: o,
		stdout:        stdout,
		now:           time.Now,
		pollEvery:     readyPoll,
		readyTimeout:  readyWait,
		slack:         loadgenSlack,
		src:           procSources,
		clockRead:     measureClockRead,
		http:          &http.Client{},
		configs:       planConfigs(o.Params, o.OE, o.HTTP),
		long:          longRun(o.Params, o.OE, o.HTTP),
	}
}

// logf writes a line to the client's stdout. A line that cannot be written
// is not worth stopping the matrix for.
func (c *client) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(c.stdout, format+"\n", args...)
}

// runRecord is one line of runs.jsonl. The outcomes, trades and latencies
// are loadgen's, copied as they are.
type runRecord struct {
	Seq             int                   `json:"seq"`
	Round           int                   `json:"round"` // 0 for the long run
	Config          string                `json:"config"`
	Flow            string                `json:"flow"`
	EngineConfig    string                `json:"engine_config"`
	Args            []string              `json:"args"`
	Started         string                `json:"started"`      // before the engine was started
	Ended           string                `json:"ended"`        // after the engine's metrics were read
	Report          string                `json:"report"`       // loadgen's JSON, relative to the results directory; "" if no engine was started
	EngineLog       string                `json:"engine_log"`   // the engine's output, bounded, relative to the results directory; "" if none was fetched
	Wake            string                `json:"wake"`         // the engine machine's wake meter over the measured part (wake.go), relative to the results directory; "" if none was fetched, and for a run whose report has no series
	ClockOffset     *clockOffset          `json:"clock_offset"` // of the engine machine's clock, measured with the wake fetch; nil if there was none
	LoadgenExit     int                   `json:"loadgen_exit"` // -1 if loadgen did not run or did not exit by itself
	LoadgenValid    bool                  `json:"loadgen_valid"`
	LoadgenProblems []string              `json:"loadgen_problems"`
	Study           []ruleResult          `json:"study"`
	Valid           bool                  `json:"valid"`
	ClientHost      machinePair           `json:"client_host"`
	EngineHost      enginePair            `json:"engine_host"`
	EngineMetrics   *engineMetrics        `json:"engine_metrics"`
	EngineDelta     *deltaResult          `json:"engine_delta"`    // the engine machine's CPU counters (counters.go) over the whole run, warmup and loadgen's start and end included, which is more than the measured part; nil if no engine was started
	ClientDelta     *deltaResult          `json:"client_delta"`    // this machine's, over the same time
	Notes           []string              `json:"notes,omitempty"` // what could not be read, a cut engine log, and the failed attempts to start the engine
	Outcomes        map[string]int        `json:"outcomes"`
	Trades          tradeCounts           `json:"trades"`
	Latency         map[string]latencyRow `json:"latency_us"`
}

type machinePair struct {
	Before machine `json:"before"`
	After  machine `json:"after"`
}

type enginePair struct {
	Before hostRecord  `json:"before"`
	After  *hostRecord `json:"after"` // nil if it could not be read
}

// engineMetrics are the engine's own counts, read after the run.
type engineMetrics struct {
	GCCount  int64 `json:"gc_count"`
	RSSBytes int64 `json:"rss_bytes"`
}

// planFile is plan.json.
type planFile struct {
	Rehearsal           bool              `json:"rehearsal"`
	Overrides           map[string]string `json:"overrides"`            // of the client
	SupervisorOverrides map[string]string `json:"supervisor_overrides"` // of the supervisor; null until it has answered
	Rounds              int               `json:"rounds"`
	Configs             []config          `json:"configs"`  // run in this order in each round
	LongRun             config            `json:"long_run"` // run once after the last round, if G2 passed
}

// run waits for the supervisor, makes the rounds and writes the summary,
// whatever stopped the matrix.
func (c *client) run(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Join(c.Out, "runs"), 0o755); err != nil {
		return err
	}
	c.clockNS = c.clockRead()
	if err := c.writePlan(); err != nil {
		return err
	}
	var stopped, reason string
	err := c.waitReady(ctx)
	if err == nil {
		// Only now are the supervisor's overrides known.
		c.supervisorOverrides = maps.Clone(c.engine.Overrides)
		if c.supervisorOverrides == nil {
			c.supervisorOverrides = map[string]string{}
		}
		err = c.writePlan()
	}
	if err == nil {
		c.fetchSupervisorFiles(ctx)
		stopped, reason, err = c.matrix(ctx)
	}
	if err != nil {
		stopped, reason = "error", err.Error()
	}
	return errors.Join(err, c.writeSummary(stopped, reason))
}

// writePlan writes plan.json. A run with an override on either machine is a
// rehearsal.
func (c *client) writePlan() error {
	overrides := c.Overrides
	if overrides == nil {
		overrides = map[string]string{}
	}
	return writeJSON(filepath.Join(c.Out, "plan.json"), planFile{
		Rehearsal:           len(overrides) > 0 || len(c.supervisorOverrides) > 0,
		Overrides:           overrides,
		SupervisorOverrides: c.supervisorOverrides,
		Rounds:              c.Params.Rounds,
		Configs:             c.configs,
		LongRun:             c.long,
	})
}

// waitReady polls the supervisor until its loopback run is over.
func (c *client) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(c.readyTimeout)
	for {
		h, err := c.host(ctx)
		if err == nil && h.Ready {
			c.engine = h
			return nil
		}
		if err == nil {
			err = errors.New("not ready")
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("supervisor at %s not ready after %v: %w", c.Ctl, c.readyTimeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.pollEvery):
		}
	}
}

// fetchSupervisorFiles saves the supervisor's loopback run and the text that
// describes its machine next to the results. The matrix does not depend on
// them, so a file that cannot be had is reported and the matrix goes on.
func (c *client) fetchSupervisorFiles(ctx context.Context) {
	for _, name := range []string{"loopback.json", "loopback.txt", "engine-env.txt"} {
		data, err := c.fetch(ctx, http.MethodGet, "http://"+c.Ctl+"/"+name, 30*time.Second)
		if err == nil {
			err = writeFileAtomic(filepath.Join(c.Out, name), data)
		}
		if err != nil {
			c.logf("warning: no %s: %v", name, err)
		}
	}
}

// matrix makes the rounds and, if G2 passed, the long run. It returns why it
// stopped before the end, if it did.
func (c *client) matrix(ctx context.Context) (stopped, reason string, err error) {
	rounds := c.Params.Rounds
	for round := 1; round <= rounds; round++ {
		for _, cfg := range c.configs {
			if why, late := c.noTimeFor(cfg); late {
				return "deadline", why, nil
			}
			if err := c.runOne(ctx, round, cfg); err != nil {
				return "", "", err
			}
		}
		g, err := c.updateG2(round == rounds)
		if err != nil {
			return "", "", err
		}
		if g.EarlyStop && round < rounds {
			return "g2", g.EarlyStopReason, nil
		}
	}
	g, err := c.updateG2(true)
	if err != nil {
		return "", "", err
	}
	if !g.Passed {
		return "g2_failed", "G2 did not pass after the last round, so the " + c.long.Name + " run was not made", nil
	}
	if why, late := c.noTimeFor(c.long); late {
		return "deadline", why, nil
	}
	return "", "", c.runOne(ctx, 0, c.long)
}

// noTimeFor reports whether a run of cfg started now, with the margin, would
// end after the deadline.
func (c *client) noTimeFor(cfg config) (string, bool) {
	if c.Deadline.IsZero() {
		return "", false
	}
	need := time.Duration(cfg.Warmup) + time.Duration(cfg.Duration) + c.Margin
	now := c.now()
	if !now.Add(need).After(c.Deadline) {
		return "", false
	}
	return fmt.Sprintf("a run of %s at %s needs %v with the margin, and the deadline is %s",
		cfg.Name, now.UTC().Format(time.RFC3339), need, c.Deadline.UTC().Format(time.RFC3339)), true
}

// updateG2 evaluates G2 over the runs so far and writes g2.json.
func (c *client) updateG2(finished bool) (g2Result, error) {
	g := computeG2(c.configs, c.records, c.Params.Rounds, finished)
	return g, writeJSON(filepath.Join(c.Out, "g2.json"), g)
}

// writeSummary writes summary.json and summary.txt.
func (c *client) writeSummary(stopped, reason string) error {
	g, err := c.updateG2(true)
	if err != nil {
		return err
	}
	s := buildSummary(summaryInput{
		Params:              c.Params,
		Overrides:           c.Overrides,
		SupervisorOverrides: c.supervisorOverrides,
		SupervisorSilent:    c.supervisorOverrides == nil, // derived from the missing overrides: nil until the supervisor has answered
		Configs:             append(append([]config(nil), c.configs...), c.long),
		Records:             c.records,
		G2:                  g,
		Memory:              engineMemory{LimitBytes: c.engine.MemLimitBytes, GoMemLimitBytes: c.engine.GoMemLimitBytes},
		EngineClockReadNS:   c.engine.ClockReadNS,
		ClientClockReadNS:   c.clockNS,
		Stopped:             stopped,
		StopReason:          reason,
	})
	if err := writeJSON(filepath.Join(c.Out, "summary.json"), s); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(c.Out, "summary.txt"), []byte(s.text()))
}

// runOne starts a fresh engine in the configuration of cfg, runs loadgen
// against it and records the run. A run that comes out invalid is recorded
// and is not an error. A run whose engine could not be started is recorded as
// invalid too, without a report, so that the rules of G2 decide what follows.
// Only a context that ended, or a record that cannot be kept, is an error.
func (c *client) runOne(ctx context.Context, round int, cfg config) error {
	p := c.Params
	rec := runRecord{
		Seq: len(c.records) + 1, Round: round, Config: cfg.Name, Flow: cfg.Flow, EngineConfig: cfg.Engine,
		Started: c.stamp(), Args: []string{}, LoadgenExit: -1, LoadgenProblems: []string{},
		Outcomes: map[string]int{}, Latency: map[string]latencyRow{},
	}
	base := fmt.Sprintf("%02d-%s-r%d", rec.Seq, cfg.Name, round)
	reportFile := filepath.Join(c.Out, "runs", base+".json")

	before, err := c.startEngine(ctx, cfg, &rec)
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		rec.Study = []ruleResult{startRule(cfg.Engine, err)}
		rec.Ended = c.stamp()
		return c.keep(rec)
	}
	rec.Report = path.Join("runs", base+".json")
	rec.Args = loadgenArgs(cfg, reportFile)
	c.engine = before
	rec.EngineHost.Before = before
	rec.ClientHost.Before = c.machine(&rec)
	countersBefore := c.takeCounters(ctx)

	rec.LoadgenExit = c.runLoadgen(ctx, cfg, rec.Args, filepath.Join(c.Out, "runs", base+".txt"), &rec)
	if err := ctx.Err(); err != nil {
		return err
	}
	rec.EngineDelta, rec.ClientDelta = c.deltas(countersBefore, c.takeCounters(ctx), &rec)

	rep, repErr := readReport(reportFile)
	after, afterErr := c.host(ctx)
	var afterPtr *hostRecord
	if afterErr == nil {
		afterPtr = &after
		c.engine = after
	} else {
		rec.Notes = append(rec.Notes, "engine host record after the run: "+afterErr.Error())
	}
	rec.EngineHost.After = afterPtr
	rec.ClientHost.After = c.machine(&rec)
	if m, err := c.scrapeMetrics(ctx); err == nil {
		rec.EngineMetrics = &m
	} else {
		rec.Notes = append(rec.Notes, "engine metrics: "+err.Error())
	}
	c.keepEngineLog(ctx, base, before.Starts, &rec)
	c.keepWake(ctx, base, rep, &rec)

	rec.Study = append(evaluate(cfg, rep, repErr, p.MaxSendLag), stableRule(before.Starts, afterPtr, afterErr))
	rec.Valid = allMet(rec.Study)
	if rep != nil {
		rec.LoadgenValid, rec.Outcomes, rec.Trades, rec.Latency = rep.Valid, rep.Outcomes, rep.Trades, rep.Latency
		rec.LoadgenProblems = append(rec.LoadgenProblems, rep.Problems...)
		if rec.Outcomes == nil {
			rec.Outcomes = map[string]int{}
		}
		if rec.Latency == nil {
			rec.Latency = map[string]latencyRow{}
		}
	}
	rec.Ended = c.stamp()
	return c.keep(rec)
}

// keep adds a run to the records and writes runs.jsonl.
func (c *client) keep(rec runRecord) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	c.records = append(c.records, rec)
	c.lines = append(append(c.lines, line...), '\n')
	if err := writeFileAtomic(filepath.Join(c.Out, "runs.jsonl"), c.lines); err != nil {
		return err
	}
	c.progress(rec)
	return nil
}

func (c *client) stamp() string { return c.now().UTC().Format(time.RFC3339) }

// machine describes this host; a failure to read it goes into rec's notes.
func (c *client) machine(rec *runRecord) machine {
	m, err := readMachine(c.src, c.now())
	if err != nil {
		rec.Notes = append(rec.Notes, "client host record: "+err.Error())
	}
	return m
}

// runLoadgen runs loadgen with args, its output in txt, and returns its exit
// code, or -1 if it did not exit by itself. A loadgen that outlasts its
// window by c.slack is killed. Whatever is odd about the run goes into
// rec's notes.
func (c *client) runLoadgen(ctx context.Context, cfg config, args []string, txt string, rec *runRecord) int {
	f, err := os.Create(txt)
	if err != nil {
		rec.Notes = append(rec.Notes, "loadgen output: "+err.Error())
		return -1
	}
	defer func() { _ = f.Close() }() // loadgen's output is read back as written
	limit := time.Duration(cfg.Warmup) + time.Duration(cfg.Duration) + c.slack
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	cmd := loadgenCommand(ctx, c.Loadgen, args)
	cmd.Stdout, cmd.Stderr = f, f
	cmd.WaitDelay = 5 * time.Second
	err = cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			rec.Notes = append(rec.Notes, "loadgen killed after "+limit.String())
		}
		return exit.ExitCode()
	}
	rec.Notes = append(rec.Notes, "loadgen: "+err.Error())
	return -1
}

// progress prints the line of a finished run.
func (c *client) progress(r runRecord) {
	p50, p99 := "-", "-"
	for _, name := range []string{"reply", "pong"} {
		if row, ok := r.Latency[name]; ok {
			p50, p99 = strconv.FormatFloat(row.P50, 'f', 1, 64), strconv.FormatFloat(row.P99, 'f', 1, 64)
			break
		}
	}
	line := fmt.Sprintf("%02d %-12s valid=%t p50=%s p99=%s", r.Seq, r.Config, r.Valid, p50, p99)
	if !r.Valid {
		var failed []string
		for _, rule := range r.Study {
			if rule.Verdict == verdictFail {
				failed = append(failed, rule.Rule)
			}
		}
		line += " failed=" + strings.Join(failed, ",")
	}
	c.logf("%s", line)
}

// startEngine asks the supervisor for the engine of cfg and, if that fails,
// asks again, up to startAttempts times in all, for as long as a run would
// still end before the deadline. Each failed attempt is noted in rec. It
// returns the host record that the supervisor answers with, or the error of
// the last attempt.
func (c *client) startEngine(ctx context.Context, cfg config, rec *runRecord) (hostRecord, error) {
	var h hostRecord
	var err error
	for attempt := 1; attempt <= startAttempts; attempt++ {
		if attempt > 1 {
			if _, late := c.noTimeFor(cfg); late {
				break
			}
		}
		if h, err = c.requestStart(ctx, cfg.Engine); err == nil {
			return h, nil
		}
		rec.Notes = append(rec.Notes, fmt.Sprintf("engine start, attempt %d of %d: %v", attempt, startAttempts, err))
	}
	return h, err
}

// requestStart makes one request for a fresh engine in the named
// configuration and returns the host record it answers with.
func (c *client) requestStart(ctx context.Context, name string) (hostRecord, error) {
	var h hostRecord
	data, err := c.fetch(ctx, http.MethodPost, "http://"+c.Ctl+"/start?config="+url.QueryEscape(name), startWait)
	if err == nil {
		err = json.Unmarshal(data, &h)
	}
	if err != nil {
		return h, fmt.Errorf("starting engine %q: %w", name, err)
	}
	if h.Config != name {
		return h, fmt.Errorf("started engine %q: the supervisor reports config %q", name, h.Config)
	}
	return h, nil
}

// readings are the counters of both machines at one moment. engineErr says why
// the engine machine's could not be had; they are then empty.
type readings struct {
	client    counters
	engine    counters
	engineErr error
}

// takeCounters reads this machine's counters and, through the supervisor, the
// engine machine's and the running engine's. A run takes it twice, so the
// engine's threads are read twice a run.
func (c *client) takeCounters(ctx context.Context) readings {
	r := readings{client: readCounters(c.src, 0)}
	r.engine, r.engineErr = c.engineCounters(ctx)
	return r
}

// engineCounters reads the supervisor's /counters.
func (c *client) engineCounters(ctx context.Context) (counters, error) {
	var r counters
	data, err := c.fetch(ctx, http.MethodGet, "http://"+c.Ctl+"/counters", 10*time.Second)
	if err == nil {
		err = json.Unmarshal(data, &r)
	}
	if err == nil && len(r.Values)+len(r.Unreadable) == 0 {
		err = errors.New("the answer holds no counter")
	}
	return r, err
}

// deltas makes the differences of two readings for the engine machine and for
// this one. A reading of the engine machine that was not had is noted and
// makes its difference unreadable as a whole. The run's validity does not
// depend on any of this: the counters explain a run and are no rule of it.
func (c *client) deltas(before, after readings, rec *runRecord) (engineDelta, clientDelta *deltaResult) {
	cd, ed := delta(before.client, after.client), delta(before.engine, after.engine)
	var reasons []string
	for _, e := range []struct {
		when string
		err  error
	}{{"before", before.engineErr}, {"after", after.engineErr}} {
		if e.err != nil {
			rec.Notes = append(rec.Notes, fmt.Sprintf("engine counters %s the run: %v", e.when, e.err))
			reasons = append(reasons, e.when+": "+e.err.Error())
		}
	}
	if len(reasons) > 0 {
		ed = unreadableDelta(strings.Join(reasons, "; "))
	}
	return &ed, &cd
}

// host reads the supervisor's host record.
func (c *client) host(ctx context.Context) (hostRecord, error) {
	var h hostRecord
	data, err := c.fetch(ctx, http.MethodGet, "http://"+c.Ctl+"/host", 10*time.Second)
	if err == nil {
		err = json.Unmarshal(data, &h)
	}
	return h, err
}

// scrapeMetrics reads the engine's garbage collection count and resident
// memory from its /metrics.
func (c *client) scrapeMetrics(ctx context.Context) (engineMetrics, error) {
	data, err := c.fetch(ctx, http.MethodGet, "http://"+c.HTTP+"/metrics", 10*time.Second)
	if err != nil {
		return engineMetrics{}, err
	}
	return parseMetrics(data)
}

// keepEngineLog saves the output of the engine of the start numbered start
// next to the report of the run base. The log is evidence, not a rule of
// validity, so one that cannot be had is noted and the run stands.
func (c *client) keepEngineLog(ctx context.Context, base string, start int, rec *runRecord) {
	data, err := c.fetch(ctx, http.MethodGet, "http://"+c.Ctl+"/engine-log?start="+strconv.Itoa(start), 10*time.Second)
	name := path.Join("runs", base+".engine.log")
	if err == nil {
		err = writeFileAtomic(filepath.Join(c.Out, name), data)
	}
	if err != nil {
		rec.Notes = append(rec.Notes, "engine log: "+err.Error())
		return
	}
	rec.EngineLog = name
	if note, cut := engineLogCut(data); cut {
		rec.Notes = append(rec.Notes, "engine log: "+note)
	}
}

// keepWake saves the wake meter's seconds over the measured part of the run
// base, in the engine machine's clock, next to the report, with the offset of
// that clock from this one, which the same request measures. The seconds can be
// set against the report's series only through the offset, so a run whose report
// has no series (the REST runs) gets neither. The supervisor serves a second
// once a later one has a wake, so if the window has only just ended the last
// of its seconds is not served yet: the request is repeated once, after the
// wait that wakeWait gives. Like the engine log, the wake is evidence and not a
// rule of validity: what cannot be had is noted and the run stands.
func (c *client) keepWake(ctx context.Context, base string, rep *loadgenReport, rec *runRecord) {
	if rep == nil || len(rep.Series) == 0 || string(rep.Series) == "null" {
		return
	}
	var ser reportSeries
	err := json.Unmarshal(rep.Series, &ser)
	if err == nil && (ser.Start.IsZero() || len(ser.N) == 0) {
		err = errors.New("no start or no windows")
	}
	if err != nil {
		rec.Notes = append(rec.Notes, "wake: the report's series: "+err.Error())
		return
	}
	window := time.Second
	if ser.WindowS > 0 {
		window = time.Duration(ser.WindowS * float64(time.Second))
	}
	end := ser.Start.Add(time.Duration(len(ser.N)) * window)
	target := fmt.Sprintf("http://%s/wake?from=%d&to=%d", c.Ctl, ser.Start.Add(-time.Second).UnixMilli(), end.Add(time.Second).UnixMilli())
	var a wakeAnswer
	var off clockOffset
	for attempt := 1; ; attempt++ {
		send := c.now()
		data, err := c.fetch(ctx, http.MethodGet, target, 10*time.Second)
		recv := c.now()
		a = wakeAnswer{}
		if err == nil {
			err = json.Unmarshal(data, &a)
		}
		if err == nil {
			err = a.check()
		}
		if err != nil {
			rec.Notes = append(rec.Notes, "wake: "+err.Error())
			return
		}
		off = measureOffset(send, recv, a.NowUnixNS)
		wait := wakeWait(a.NowUnixNS, end, off.duration())
		if wait == 0 || attempt == 2 {
			if wait > 0 {
				rec.Notes = append(rec.Notes, "wake: the last second of the measured part was not finished when the supervisor answered")
			}
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
	rec.ClockOffset = &off
	data, err := json.Marshal(wakeFile{wakeAnswer: a, Clock: off})
	name := path.Join("runs", base+".wake.json")
	if err == nil {
		err = writeFileAtomic(filepath.Join(c.Out, name), append(data, '\n'))
	}
	if err != nil {
		rec.Notes = append(rec.Notes, "wake: "+err.Error())
		return
	}
	rec.Wake = name
}

// parseMetrics finds go_gc_duration_seconds_count and
// process_resident_memory_bytes in a Prometheus text page.
func parseMetrics(page []byte) (engineMetrics, error) {
	found := map[string]float64{}
	for line := range strings.Lines(string(page)) {
		name, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if name != "go_gc_duration_seconds_count" && name != "process_resident_memory_bytes" || !ok {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			return engineMetrics{}, fmt.Errorf("metric %s: %w", name, err)
		}
		found[name] = v
	}
	for _, name := range []string{"go_gc_duration_seconds_count", "process_resident_memory_bytes"} {
		if _, ok := found[name]; !ok {
			return engineMetrics{}, fmt.Errorf("no %s in the metrics", name)
		}
	}
	return engineMetrics{GCCount: int64(found["go_gc_duration_seconds_count"]), RSSBytes: int64(found["process_resident_memory_bytes"])}, nil
}

// fetch makes a request and returns the body of a 200 answer. Anything else
// is an error that says what the server answered.
func (c *client) fetch(ctx context.Context, method, target string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }() // the body was read to its end or the answer is an error
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, target, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s %s: %s: %s", method, target, resp.Status, bytes.TrimSpace(body[:min(len(body), 512)]))
	}
	return body, nil
}
