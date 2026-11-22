//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// expectedRun is a run of the matrix as the plan lists it.
type expectedRun struct {
	cfg   config
	round int
	seq   int
}

func expectedRuns(p params, oe, http string, withLong bool) []expectedRun {
	var runs []expectedRun
	for round := 1; round <= p.Rounds; round++ {
		for _, cfg := range planConfigs(p, oe, http) {
			runs = append(runs, expectedRun{cfg, round, len(runs) + 1})
		}
	}
	if withLong {
		runs = append(runs, expectedRun{longRun(p, oe, http), 0, len(runs) + 1})
	}
	return runs
}

func TestClientFullMatrix(t *testing.T) {
	p := defaultParams
	r := newRig(t, p, nil)
	log := writeMatrixScript(t, p, nil)
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	runs := expectedRuns(p, "engine.test:9000", r.http, true)
	if len(runs) != 46 {
		t.Fatalf("test plan has %d runs", len(runs))
	}

	// One engine start per run, in the plan's order, with the engine config of each.
	var wantStarts []string
	for _, e := range runs {
		wantStarts = append(wantStarts, e.cfg.Engine)
	}
	if got := r.sup.startedConfigs(); !slices.Equal(got, wantStarts) {
		t.Errorf("engine configs asked for %v, want %v", got, wantStarts)
	}
	if want := []string{"default", "default", "gogc-off", "spin-pin", "default", "default", "default", "default", "default"}; !slices.Equal(wantStarts[:9], want) {
		t.Errorf("a round asks for %v, want %v", wantStarts[:9], want)
	}

	records := r.records()
	logged := loggedArgs(t, log)
	if len(records) != 46 || len(logged) != 46 {
		t.Fatalf("%d records and %d loadgen runs, want 46 each", len(records), len(logged))
	}
	progress := strings.Split(strings.TrimSpace(r.stdout.String()), "\n")
	if len(progress) != 46 {
		t.Errorf("%d progress lines, want 46:\n%s", len(progress), r.stdout.String())
	}
	var lastEnd time.Time
	for i, e := range runs {
		rec := records[i]
		base := fmt.Sprintf("%02d-%s-r%d", e.seq, e.cfg.Name, e.round)
		reportFile := filepath.Join(r.out, "runs", base+".json")
		wantArgs := loadgenArgs(e.cfg, reportFile)
		if rec.Seq != e.seq || rec.Round != e.round || rec.Config != e.cfg.Name || rec.Flow != e.cfg.Flow || rec.EngineConfig != e.cfg.Engine {
			t.Errorf("record %d is seq %d round %d %s %s %s", i+1, rec.Seq, rec.Round, rec.Config, rec.Flow, rec.EngineConfig)
		}
		if !slices.Equal(rec.Args, wantArgs) || !slices.Equal(logged[i], wantArgs) {
			t.Errorf("%s: recorded args %v, started with %v, want %v", base, rec.Args, logged[i], wantArgs)
		}
		if rec.Report != "runs/"+base+".json" {
			t.Errorf("%s: report named %q", base, rec.Report)
		}

		// loadgen's files are kept as they were written, and the record takes its numbers from them as they are.
		canned := baseRun(e.cfg, e.seq)
		if got, _ := os.ReadFile(reportFile); string(got) != string(canned.Report) {
			t.Errorf("%s: loadgen's report was changed", base)
		}
		if got, _ := os.ReadFile(filepath.Join(r.out, "runs", base+".txt")); string(got) != canned.Text {
			t.Errorf("%s: loadgen's output %q, want %q", base, got, canned.Text)
		}
		want := decodeReport(t, canned.Report)
		if !reflect.DeepEqual(rec.Latency, want.Latency) || !reflect.DeepEqual(rec.Outcomes, want.Outcomes) || rec.Trades != want.Trades {
			t.Errorf("%s: the record's numbers differ from loadgen's:\n%+v\n%+v", base, rec.Latency, want.Latency)
		}

		if !rec.Valid || !rec.LoadgenValid || rec.LoadgenExit != 0 || len(rec.LoadgenProblems) != 0 {
			t.Errorf("%s: valid %v, loadgen valid %v, exit %d, problems %v", base, rec.Valid, rec.LoadgenValid, rec.LoadgenExit, rec.LoadgenProblems)
		}
		var rules []string
		for _, rule := range rec.Study {
			rules = append(rules, rule.Rule)
		}
		if !slices.Equal(rules, []string{"rate", "failures", "cancel_skip", "send_lag", "loadgen_valid", "engine_stable"}) {
			t.Errorf("%s: study rules %v", base, rules)
		}
		if rec.EngineHost.After == nil || rec.EngineHost.Before.Starts != e.seq || rec.EngineHost.After.Starts != e.seq ||
			rec.EngineHost.Before.Config != e.cfg.Engine {
			t.Errorf("%s: engine host %+v", base, rec.EngineHost)
		}
		if rec.ClientHost.Before.CPUModel != "Test CPU @ 3GHz" || rec.ClientHost.After.NProc != 4 || rec.ClientHost.After.Loadavg != [3]float64{0.5, 1.25, 2} {
			t.Errorf("%s: client host %+v", base, rec.ClientHost)
		}
		if rec.EngineMetrics == nil || *rec.EngineMetrics != (engineMetrics{GCCount: 12, RSSBytes: 210083840}) {
			t.Errorf("%s: engine metrics %+v", base, rec.EngineMetrics)
		}
		start, err1 := time.Parse(time.RFC3339, rec.Started)
		end, err2 := time.Parse(time.RFC3339, rec.Ended)
		if err1 != nil || err2 != nil || !strings.HasSuffix(rec.Started, "Z") || start.Before(lastEnd) || !end.After(start) {
			t.Errorf("%s: started %q, ended %q (after %v)", base, rec.Started, rec.Ended, lastEnd)
		}
		lastEnd = end
		if want := fmt.Sprintf("%02d %-12s valid=true", e.seq, e.cfg.Name); !strings.HasPrefix(progress[i], want) {
			t.Errorf("progress line %q, want it to start %q", progress[i], want)
		}
	}
	// R2 offers half the rate, the high-rate flows the high rate, and the long run measures 900 s.
	rate := map[string]string{}
	for _, rec := range records[:9] {
		rate[rec.Config] = rec.Args[slices.Index(rec.Args, "-rate")+1]
	}
	if rate["R2"] != "5000" || rate["R3-71k"] != "71429" || rate["T-71k"] != "71429" || rate["R3-10k"] != "10000" || rate["REST-10k"] != "10000" {
		t.Errorf("rates %v", rate)
	}
	if long := records[45]; long.Config != "R1-15min" || long.Args[slices.Index(long.Args, "-duration")+1] != "900s" {
		t.Errorf("long run %+v", long)
	}

	// Every record has every field the spec lists.
	var first map[string]any
	line, _, _ := strings.Cut(string(r.file("runs.jsonl")), "\n")
	if err := json.Unmarshal([]byte(line), &first); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"seq", "round", "config", "flow", "engine_config", "args", "started", "ended", "report", "loadgen_exit",
		"loadgen_valid", "loadgen_problems", "study", "valid", "client_host", "engine_host", "engine_metrics", "outcomes", "trades", "latency_us"} {
		if _, ok := first[key]; !ok {
			t.Errorf("a run record has no %q", key)
		}
	}

	// Results directory: every file in its place, none half written.
	for _, name := range []string{"plan.json", "runs.jsonl", "g2.json", "summary.json", "summary.txt", "loopback.json", "loopback.txt"} {
		if _, err := os.Stat(filepath.Join(r.out, name)); err != nil {
			t.Errorf("no %s: %v", name, err)
		}
	}
	if got := string(r.file("loopback.json")); got != r.sup.loopback["loopback.json"] {
		t.Errorf("loopback.json is %q", got)
	}
	if got := string(r.file("loopback.txt")); got != r.sup.loopback["loopback.txt"] {
		t.Errorf("loopback.txt is %q", got)
	}
	entries, err := os.ReadDir(filepath.Join(r.out, "runs"))
	if err != nil || len(entries) != 92 {
		t.Errorf("runs/ has %d files, %v, want 92", len(entries), err)
	}
	if err := filepath.WalkDir(r.out, func(path string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(path, ".tmp") {
			t.Errorf("a temporary file is left: %s", path)
		}
		return err
	}); err != nil {
		t.Error(err)
	}

	var plan struct {
		Rehearsal bool
		Overrides map[string]string
		Rounds    int
		Configs   []struct {
			Name, Flow string
			Engine     string  `json:"engine_config"`
			Rate       float64 `json:"rate"`
			WarmupS    float64 `json:"warmup_s"`
			DurationS  float64 `json:"duration_s"`
			Args       []string
		}
		LongRun struct{ Name string } `json:"long_run"`
	}
	if err := json.Unmarshal(r.file("plan.json"), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Rehearsal || plan.Overrides == nil || len(plan.Overrides) != 0 || plan.Rounds != 5 || len(plan.Configs) != 9 || plan.LongRun.Name != "R1-15min" {
		t.Errorf("plan.json: %+v", plan)
	}
	for i, c := range planConfigs(p, "engine.test:9000", r.http) {
		g := plan.Configs[i]
		if g.Name != c.Name || g.Flow != c.Flow || g.Engine != c.Engine || g.Rate != c.Rate || g.WarmupS != 10 || g.DurationS != 60 || !slices.Equal(g.Args, c.Args) {
			t.Errorf("plan.json configuration %d is %+v, want %+v", i+1, g, c)
		}
	}

	g := r.g2()
	if !g.Passed || !g.Complete || g.EarlyStop {
		t.Errorf("g2.json: %+v", g)
	}
	s := r.summary()
	if s.Rehearsal || !s.Publishable || s.Stopped != "" || s.Runs != 46 || len(s.Configs) != 10 || !s.G2.Passed ||
		s.Engine.LimitBytes != 8<<30 || s.Engine.GoMemLimitBytes != 4<<30 {
		t.Errorf("summary.json: %+v", s)
	}
	if long := summaryConfig(t, s, "R1-15min"); long.Runs != 1 || long.Valid != 1 || long.Floor == nil {
		t.Errorf("long run in the summary: %+v", long)
	}
	// The summary's numbers are loadgen's: F's floor row has the min, median and max of its five p50.
	var floorP50 []float64
	for i := range 5 {
		floorP50 = append(floorP50, decodeReport(t, baseRun(runs[i*9].cfg, runs[i*9].seq).Report).Latency["pong"].P50)
	}
	if got := summaryConfig(t, s, "F").Rows["pong"].P50; got != spread(floorP50) {
		t.Errorf("summary F pong p50 %+v, want %+v", got, spread(floorP50))
	}
	if strings.HasPrefix(string(r.file("summary.txt")), "REHEARSAL") {
		t.Error("summary.txt of a run with the defaults says REHEARSAL")
	}
}

func TestClientEngineStartedDuringARun(t *testing.T) {
	p := rounds(2)
	r := newRig(t, p, nil)
	writeMatrixScript(t, p, nil)
	r.sup.bump[3] = true // after R1-gogc-off's start, the supervisor starts another engine
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	records := r.records()
	rec := records[2]
	if rec.Config != "R1-gogc-off" || rec.Valid || rec.EngineHost.Before.Starts != 3 || rec.EngineHost.After.Starts != 4 {
		t.Errorf("run 3 is %s, valid %v, starts %d then %d", rec.Config, rec.Valid, rec.EngineHost.Before.Starts, rec.EngineHost.After.Starts)
	}
	for _, rule := range rec.Study {
		if (rule.Rule == "engine_stable") != (rule.Verdict == verdictFail) {
			t.Errorf("rule %s: %s (%s)", rule.Rule, rule.Verdict, rule.Value)
		}
		if rule.Rule == "engine_stable" && !strings.Contains(rule.Value, "the supervisor started an engine during the run") {
			t.Errorf("engine_stable says %q", rule.Value)
		}
	}
	if len(rec.Latency) == 0 || !rec.LoadgenValid {
		t.Error("the run's own numbers are not kept")
	}
	for i, other := range records {
		if i != 2 && !other.Valid {
			t.Errorf("run %d is invalid: %+v", i+1, other.Study)
		}
	}
	// The supervisor's count carries on from 4, and one invalid run does not stop the matrix.
	if records[3].EngineHost.Before.Starts != 5 || len(records) != 19 || !r.g2().Passed {
		t.Errorf("run 4 saw starts %d, %d runs, g2 passed %v", records[3].EngineHost.Before.Starts, len(records), r.g2().Passed)
	}
}

func TestClientRecordsInvalidRuns(t *testing.T) {
	p := rounds(2)
	r := newRig(t, p, nil)
	edits := map[string]func(fakeRun) fakeRun{
		"F": func(f fakeRun) fakeRun { // 98.9% of the rate
			return editReport(f, func(c *cannedReport) { c.Achieved = 9890 })
		},
		"R1": func(f fakeRun) fakeRun { // loadgen says valid
			return editReport(f, func(c *cannedReport) { c.Outcomes = map[string]int{"order_rested": 5, "busy": 1} })
		},
		"R1-gogc-off": func(f fakeRun) fakeRun {
			f.Exit = 1
			return editReport(f, func(c *cannedReport) { c.Valid, c.Problems = false, []string{"3 orders that traded got no trade push"} })
		},
		"R1-spin-pin": func(f fakeRun) fakeRun {
			return editReport(f, func(c *cannedReport) { c.Latency["send_lag"] = latencyRow{P999: 10.5} })
		},
		"R2": func(f fakeRun) fakeRun {
			return editReport(f, func(c *cannedReport) { c.Outcomes = map[string]int{"order_rested": 5, "error": 2} })
		},
		"R3-10k": func(f fakeRun) fakeRun {
			return editReport(f, func(c *cannedReport) {
				c.Outcomes = map[string]int{"order_rested": 5, "cancel_canceled": 99900, "cancel_skipped": 100}
			})
		},
		"R3-71k": func(f fakeRun) fakeRun {
			return fakeRun{NoReport: true, Exit: 2, Text: "loadgen: flag provided but not defined\n"}
		},
		"T-71k": func(f fakeRun) fakeRun { return fakeRun{RawReport: `{"valid": tru`, Exit: 1} },
		"REST-10k": func(f fakeRun) fakeRun { // send lag is not gated for REST
			return editReport(f, func(c *cannedReport) { c.Latency["send_lag"] = latencyRow{P999: 5000} })
		},
	}
	failed := map[string][]string{
		"F": {"rate"}, "R1": {"failures"}, "R1-gogc-off": {"loadgen_valid"}, "R1-spin-pin": {"send_lag"}, "R2": {"failures"},
		"R3-10k": {"cancel_skip"}, "R3-71k": {"rate", "failures", "cancel_skip", "send_lag", "loadgen_valid"},
		"T-71k": {"rate", "failures", "cancel_skip", "send_lag", "loadgen_valid"}, "REST-10k": nil,
	}
	writeMatrixScript(t, p, func(cfg config, round int, f fakeRun) fakeRun {
		if round == 1 {
			return edits[cfg.Name](f)
		}
		return f
	})
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	for _, rec := range r.records()[:9] {
		var got []string
		for _, rule := range rec.Study {
			if rule.Verdict == verdictFail {
				got = append(got, rule.Rule)
			}
		}
		if !slices.Equal(got, failed[rec.Config]) || rec.Valid != (len(failed[rec.Config]) == 0) {
			t.Errorf("%s: failed rules %v, valid %v, want %v", rec.Config, got, rec.Valid, failed[rec.Config])
		}
	}
	byName := map[string]runRecord{}
	for _, rec := range r.records()[:9] {
		byName[rec.Config] = rec
	}
	if rec := byName["R1-gogc-off"]; rec.LoadgenValid || rec.LoadgenExit != 1 || !slices.Equal(rec.LoadgenProblems, []string{"3 orders that traded got no trade push"}) {
		t.Errorf("loadgen's own verdict is not copied: valid %v, exit %d, problems %v", rec.LoadgenValid, rec.LoadgenExit, rec.LoadgenProblems)
	}
	if rec := byName["R3-71k"]; rec.LoadgenExit != 2 || len(rec.Latency) != 0 || len(rec.Outcomes) != 0 || !strings.Contains(rec.Study[0].Value, "no report") {
		t.Errorf("a run with no report is recorded as %+v", rec)
	}
	if rec := byName["T-71k"]; !strings.Contains(rec.Study[0].Value, "no report") {
		t.Errorf("a run with a report that is no JSON: %+v", rec.Study)
	}
	if got, _ := os.ReadFile(filepath.Join(r.out, "runs", "08-T-71k-r1.json")); string(got) != `{"valid": tru` {
		t.Errorf("loadgen's broken report was not left as it is: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(r.out, "runs", "07-R3-71k-r1.txt")); !strings.Contains(string(got), "flag provided but not defined") {
		t.Errorf("loadgen's output: %q", got)
	}
	if rule := byName["REST-10k"].Study[3]; rule.Rule != "send_lag" || rule.Verdict != verdictNA || rule.Value != "p99.9 5000 us" {
		t.Errorf("REST send lag rule %+v", rule)
	}
}

func TestClientEarlyStops(t *testing.T) {
	for _, c := range []struct {
		name   string
		mod    func(cfg config, round int, f fakeRun) fakeRun
		starts int
		reason string
	}{
		{"F with two invalid runs", func(cfg config, round int, f fakeRun) fakeRun {
			if cfg.Name == "F" && round <= 2 {
				return editReport(f, func(c *cannedReport) { c.Achieved = 100 })
			}
			return f
		}, 18, "condition 1 can no longer be met for F"},
		{"R1 with two invalid runs", func(cfg config, round int, f fakeRun) fakeRun {
			if cfg.Name == "R1" && round <= 2 {
				return editReport(f, func(c *cannedReport) { c.Outcomes = map[string]int{"busy": 3} })
			}
			return f
		}, 18, "condition 1 can no longer be met for R1"},
		{"R1 p99 more than twice apart", func(cfg config, round int, f fakeRun) fakeRun {
			if cfg.Name == "R1" && round == 2 {
				return editReport(f, func(c *cannedReport) { c.Latency["reply"] = row(60, 250) })
			}
			return f
		}, 18, "condition 2 failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := rounds(5)
			r := newRig(t, p, nil)
			log := writeMatrixScript(t, p, c.mod)
			if err := r.run(); err != nil {
				t.Fatal(err)
			}
			s := r.summary()
			if got := len(r.sup.startedConfigs()); got != c.starts || len(loggedArgs(t, log)) != c.starts || len(r.records()) != c.starts {
				t.Errorf("%d engine starts, %d records: want %d, the matrix to stop after round 2", got, len(r.records()), c.starts)
			}
			if s.Stopped != "g2" || !strings.Contains(s.StopReason, c.reason) || s.G2.Passed || !r.g2().EarlyStop {
				t.Errorf("stopped %q (%s), g2 passed %v", s.Stopped, s.StopReason, s.G2.Passed)
			}
			if !strings.Contains(string(r.file("summary.txt")), "Stopped: g2: ") {
				t.Error("summary.txt does not say why the matrix stopped")
			}
		})
	}
}

func TestClientGoesOnWhenAnotherConfigurationFails(t *testing.T) {
	p := rounds(5)
	r := newRig(t, p, nil)
	writeMatrixScript(t, p, func(cfg config, round int, f fakeRun) fakeRun {
		if cfg.Name == "REST-10k" && round <= 2 {
			return editReport(f, func(c *cannedReport) { c.Outcomes = map[string]int{"error": 1} })
		}
		return f
	})
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	s := r.summary()
	// All 45 runs are made, G2 does not pass, and so the 15-minute run is not made.
	if got := len(r.sup.startedConfigs()); got != 45 || s.Stopped != "" || s.G2.Passed || s.G2.Cond1.Status != statusFail || !s.G2.Complete {
		t.Errorf("%d starts, stopped %q, g2 %+v", got, s.Stopped, s.G2)
	}
	if long := summaryConfig(t, s, "R1-15min"); long.Runs != 0 {
		t.Errorf("the long run was made: %+v", long)
	}
	if s.G2.EarlyStop {
		t.Error("a failure of REST-10k stops the matrix")
	}
}

func TestClientDeadlineGuard(t *testing.T) {
	t.Run("stops before a run that would not end in time", func(t *testing.T) {
		p := defaultParams
		r := newRig(t, p, nil)
		r.clock.step = 0
		r.sup.onStart = func(int) { r.clock.advance(100 * time.Second) } // a run takes 100 s
		writeMatrixScript(t, p, nil)
		// Run k starts at 100k s and needs 70 s and the margin: run 6 (k = 5) ends exactly at the deadline.
		r.c.Margin = 10 * time.Minute
		r.c.Deadline = clockStart.Add(500*time.Second + 70*time.Second + 10*time.Minute)
		if err := r.run(); err != nil {
			t.Fatal(err)
		}
		if got := r.sup.startedConfigs(); len(got) != 6 {
			t.Errorf("%d runs started, want 6: %v", len(got), got)
		}
		s := r.summary()
		if s.Stopped != "deadline" || s.Runs != 6 || s.G2.Passed || s.G2.Complete || !strings.Contains(s.StopReason, "deadline") {
			t.Errorf("stopped %q (%s), %d runs, g2 %+v", s.Stopped, s.StopReason, s.Runs, s.G2)
		}
		for _, rec := range r.records() {
			if !rec.Valid {
				t.Errorf("results so far are not valid: %+v", rec.Study)
			}
		}
		if g := r.g2(); g.Passed || g.Complete {
			t.Errorf("g2.json after the deadline: %+v", g)
		}
		if !strings.Contains(string(r.file("summary.txt")), "Stopped: deadline: ") {
			t.Error("summary.txt does not say that the deadline stopped the matrix")
		}
	})
	t.Run("one second short stops a run earlier", func(t *testing.T) {
		p := defaultParams
		r := newRig(t, p, nil)
		r.clock.step = 0
		r.sup.onStart = func(int) { r.clock.advance(100 * time.Second) }
		writeMatrixScript(t, p, nil)
		r.c.Margin = 10 * time.Minute
		r.c.Deadline = clockStart.Add(500*time.Second + 70*time.Second + 10*time.Minute - time.Second)
		if err := r.run(); err != nil {
			t.Fatal(err)
		}
		if got := len(r.sup.startedConfigs()); got != 5 {
			t.Errorf("%d runs started, want 5", got)
		}
	})
	t.Run("the long run needs its 15 minutes too", func(t *testing.T) {
		p := rounds(1)
		r := newRig(t, p, map[string]string{"PILOT_ROUNDS": "1"})
		r.clock.step = 0
		writeMatrixScript(t, p, nil)
		r.c.Margin = time.Minute
		// The nine runs need 70 s and the margin each; the long run needs 910 s and the margin.
		r.c.Deadline = clockStart.Add(900 * time.Second)
		if err := r.run(); err != nil {
			t.Fatal(err)
		}
		s := r.summary()
		if got := len(r.sup.startedConfigs()); got != 9 || s.Stopped != "deadline" || !s.G2.Passed {
			t.Errorf("%d runs, stopped %q, g2 passed %v: want the nine runs, a stop for the deadline, and G2 passed", got, s.Stopped, s.G2.Passed)
		}
	})
	t.Run("no deadline, no guard", func(t *testing.T) {
		p := rounds(1)
		r := newRig(t, p, nil)
		writeMatrixScript(t, p, nil)
		if err := r.run(); err != nil {
			t.Fatal(err)
		}
		if got := len(r.sup.startedConfigs()); got != 10 {
			t.Errorf("%d runs started, want the nine and the long run", got)
		}
	})
}

func TestClientWaitsUntilTheSupervisorIsReady(t *testing.T) {
	p := rounds(1)
	r := newRig(t, p, nil)
	writeMatrixScript(t, p, nil)
	r.sup.readyAfter = 3
	var pollsAtFirstStart int
	r.sup.onStart = func(n int) {
		if n == 1 {
			pollsAtFirstStart = r.sup.polls
		}
	}
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	if pollsAtFirstStart < 4 {
		t.Errorf("the first engine was started after %d polls of /host, want 4 or more", pollsAtFirstStart)
	}
	if got := string(r.file("loopback.json")); got != r.sup.loopback["loopback.json"] {
		t.Errorf("loopback.json is %q", got)
	}

	t.Run("gives up", func(t *testing.T) {
		r := newRig(t, p, nil)
		r.sup.readyAfter = 1 << 30
		r.c.readyTimeout = 50 * time.Millisecond
		err := r.run()
		if err == nil || !strings.Contains(err.Error(), "not ready after 50ms") {
			t.Errorf("error %v, want one that says the supervisor was not ready", err)
		}
		if len(r.sup.startedConfigs()) != 0 {
			t.Error("an engine was started")
		}
		if _, err := os.Stat(filepath.Join(r.out, "plan.json")); err != nil {
			t.Errorf("no plan.json: %v", err)
		}
	})
	t.Run("supervisor that is not there", func(t *testing.T) {
		r := newRig(t, p, nil)
		r.c.Ctl = freeAddr(t)
		r.c.readyTimeout = 50 * time.Millisecond
		if err := r.run(); err == nil || !strings.Contains(err.Error(), r.c.Ctl) {
			t.Errorf("error %v, want one that names %s", err, r.c.Ctl)
		}
	})
}

func TestClientWithoutLoopbackFiles(t *testing.T) {
	p := rounds(1)
	r := newRig(t, p, nil)
	writeMatrixScript(t, p, nil)
	r.sup.loopback = map[string]string{}
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.stdout.String(), "warning: no loopback.json") {
		t.Error("the missing loopback report is not mentioned")
	}
	if _, err := os.Stat(filepath.Join(r.out, "loopback.json")); err == nil {
		t.Error("a loopback.json was written")
	}
	if len(r.records()) != 10 {
		t.Errorf("%d runs", len(r.records()))
	}
}

func TestClientRehearsal(t *testing.T) {
	p := rounds(2)
	p.Duration = 5 * time.Second
	overrides := map[string]string{"PILOT_ROUNDS": "2", "PILOT_DURATION": "5s"}
	r := newRig(t, p, overrides)
	writeMatrixScript(t, p, nil)
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	var plan struct {
		Rehearsal bool
		Overrides map[string]string
	}
	if err := json.Unmarshal(r.file("plan.json"), &plan); err != nil {
		t.Fatal(err)
	}
	if !plan.Rehearsal || !reflect.DeepEqual(plan.Overrides, overrides) {
		t.Errorf("plan.json: %+v", plan)
	}
	s := r.summary()
	if !s.Rehearsal || s.Publishable || !reflect.DeepEqual(s.Overrides, overrides) {
		t.Errorf("summary.json: rehearsal %v, publishable %v, overrides %v", s.Rehearsal, s.Publishable, s.Overrides)
	}
	if first, _, _ := strings.Cut(string(r.file("summary.txt")), "\n"); first != "REHEARSAL: not publishable" {
		t.Errorf("first line of summary.txt is %q", first)
	}
	// Two rounds are enough for G2, so the long run follows the 18.
	if got := len(r.sup.startedConfigs()); got != 19 || !s.G2.Passed {
		t.Errorf("%d runs, g2 passed %v, want 19 and a pass", got, s.G2.Passed)
	}
	if args := r.records()[0].Args; args[slices.Index(args, "-duration")+1] != "5s" {
		t.Errorf("run 1 args %v", args)
	}
}

func TestClientStartFailure(t *testing.T) {
	p := rounds(2)
	r := newRig(t, p, nil)
	writeMatrixScript(t, p, nil)
	r.sup.failStart = 4
	err := r.run()
	if err == nil || !strings.Contains(err.Error(), `starting engine "spin-pin"`) || !strings.Contains(err.Error(), "engine did not listen") {
		t.Fatalf("error %v, want the failed start and the supervisor's reason", err)
	}
	if got := len(r.records()); got != 3 {
		t.Errorf("%d runs recorded, want the 3 before the failure", got)
	}
	if s := r.summary(); s.Stopped != "error" || !strings.Contains(s.StopReason, "engine did not listen") || s.Runs != 3 {
		t.Errorf("summary after the failure: stopped %q (%s), %d runs", s.Stopped, s.StopReason, s.Runs)
	}
}

func TestClientWhatCannotBeReadIsNoted(t *testing.T) {
	p := rounds(1)
	r := newRig(t, p, nil)
	writeMatrixScript(t, p, nil)
	r.http = metricsServer(t, 404)
	r.c.HTTP = r.http
	r.sup.hostFails[2] = true // /host cannot be read after the second run
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	records := r.records()
	if records[0].EngineMetrics != nil || len(records[0].Notes) != 1 || !strings.Contains(records[0].Notes[0], "engine metrics") || !records[0].Valid {
		t.Errorf("run 1 without metrics: %+v %v valid %v", records[0].EngineMetrics, records[0].Notes, records[0].Valid)
	}
	rec := records[1]
	if rec.Valid || rec.EngineHost.After != nil || rec.EngineHost.Before.Starts != 2 {
		t.Errorf("run 2 with no host record after it: valid %v, after %+v", rec.Valid, rec.EngineHost.After)
	}
	if !slices.ContainsFunc(rec.Notes, func(n string) bool { return strings.Contains(n, "engine host record after the run") }) {
		t.Errorf("run 2 notes %v, want one that says the host record was not read", rec.Notes)
	}
	var stable ruleResult
	for _, rule := range rec.Study {
		if rule.Rule == "engine_stable" {
			stable = rule
		}
	}
	if stable.Verdict != verdictFail || !strings.Contains(stable.Value, "not read") {
		t.Errorf("engine_stable: %+v", stable)
	}
}

func TestClientKillsALoadgenThatOutlastsItsWindow(t *testing.T) {
	p := rounds(1)
	p.Warmup, p.Duration = 0, 100*time.Millisecond
	r := newRig(t, p, map[string]string{"PILOT_WARMUP": "0s"})
	r.c.slack = 300 * time.Millisecond
	writeMatrixScript(t, p, func(cfg config, round int, f fakeRun) fakeRun {
		if cfg.Name == "F" {
			return fakeRun{Hang: true}
		}
		return f
	})
	began := time.Now()
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	rec := r.records()[0]
	if rec.Config != "F" || rec.Valid || rec.LoadgenExit != -1 || len(rec.Notes) != 1 || !strings.Contains(rec.Notes[0], "loadgen killed after 400ms") {
		t.Errorf("a loadgen that never ended is recorded as %s, valid %v, exit %d, notes %v", rec.Config, rec.Valid, rec.LoadgenExit, rec.Notes)
	}
	if !strings.Contains(rec.Study[0].Value, "no report") {
		t.Errorf("rule %+v of a run with no report", rec.Study[0])
	}
	if took := time.Since(began); took > 20*time.Second {
		t.Errorf("the matrix took %v", took)
	}
	if len(r.records()) != 9 {
		t.Errorf("the matrix did not go on after the killed run: %d records", len(r.records()))
	}
}

func TestClientStopsWhenItsContextEnds(t *testing.T) {
	p := rounds(1)
	r := newRig(t, p, nil)
	writeMatrixScript(t, p, func(cfg config, round int, f fakeRun) fakeRun {
		if cfg.Name == "F" {
			return fakeRun{Hang: true}
		}
		return f
	})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := r.c.run(ctx)
	if err == nil {
		t.Fatal("a client whose context ended returned no error")
	}
	if _, statErr := os.Stat(filepath.Join(r.out, "runs.jsonl")); statErr == nil {
		t.Error("a run that was cut short was recorded")
	}
	if s := r.summary(); s.Stopped != "error" || s.Runs != 0 {
		t.Errorf("summary after the signal: stopped %q, %d runs", s.Stopped, s.Runs)
	}
}

func TestClientKilledInTheMiddleLeavesWholeFiles(t *testing.T) {
	sup := newFakeSupervisor(t)
	metrics := metricsServer(t, 200)
	p := rounds(2)
	writeMatrixScript(t, p, func(cfg config, round int, f fakeRun) fakeRun {
		if cfg.Name == "R3-10k" && round == 1 {
			f.Hang = true
		}
		return f
	})
	out := filepath.Join(t.TempDir(), "results")
	cmd := exec.Command(fakeBinary(t, "pairrun"), "client", "-loadgen", fakeBinary(t, "fake-loadgen"), "-oe", "engine.test:9000",
		"-http", metrics, "-ctl", sup.addr(), "-out", out,
		"-deadline", strconv.FormatInt(time.Now().Add(24*time.Hour).Unix(), 10), "-deadline-margin", "1m")
	cmd.Env = append(os.Environ(), "PILOT_ROUNDS=2")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // so that the hung loadgen goes with it
	output := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the sixth run to start", func() bool { return len(sup.startedConfigs()) == 6 })
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait() // killed

	lines := 0
	err := filepath.WalkDir(out, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(path, ".tmp") || strings.HasSuffix(path, ".txt") {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.HasSuffix(path, ".jsonl") {
			for line := range strings.Lines(string(data)) {
				lines++
				if !json.Valid([]byte(line)) || !strings.HasSuffix(line, "\n") {
					t.Errorf("%s holds a partial line %q", path, line)
				}
			}
		} else if !json.Valid(data) {
			t.Errorf("%s is not whole JSON: %q", path, data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if lines != 5 {
		t.Errorf("runs.jsonl has %d runs, want the 5 that finished\n%s", lines, output.String())
	}
	if _, err := os.Stat(filepath.Join(out, "plan.json")); err != nil {
		t.Errorf("no plan.json: %v", err)
	}
}

func TestParseMetrics(t *testing.T) {
	page := `# HELP go_gc_duration_seconds A summary.
go_gc_duration_seconds{quantile="0"} 3.84e-05
go_gc_duration_seconds_sum 0.0123
go_gc_duration_seconds_count 144
go_goroutines 21
process_resident_memory_bytes 2.1008384e+08
process_resident_memory_bytes_other 7
`
	got, err := parseMetrics([]byte(page))
	if err != nil || got != (engineMetrics{GCCount: 144, RSSBytes: 210083840}) {
		t.Errorf("got %+v, %v", got, err)
	}
	for name, bad := range map[string]string{
		"no gc count":  "process_resident_memory_bytes 5\n",
		"no rss":       "go_gc_duration_seconds_count 5\n",
		"not a number": "go_gc_duration_seconds_count lots\nprocess_resident_memory_bytes 5\n",
		"empty":        "",
	} {
		if m, err := parseMetrics([]byte(bad)); err == nil {
			t.Errorf("%s: parsed as %+v", name, m)
		}
	}
}

func TestClientNeedsItsFlags(t *testing.T) {
	full := []string{"client", "-loadgen", "l", "-oe", "o:1", "-http", "h:2", "-ctl", "c:3", "-out", "out"}
	for drop := 1; drop < len(full); drop += 2 {
		args := append(slices.Clone(full[:drop]), full[drop+2:]...)
		if err := run(context.Background(), args, io.Discard); err == nil || !strings.Contains(err.Error(), "required") {
			t.Errorf("run(%v) = %v, want an error that says what is required", args, err)
		}
	}
	if err := run(context.Background(), append(slices.Clone(full), "extra"), io.Discard); err == nil {
		t.Error("an extra argument was accepted")
	}
	t.Setenv("PILOT_RATE", "fast")
	if err := run(context.Background(), full, io.Discard); err == nil || !strings.Contains(err.Error(), "PILOT_RATE") {
		t.Errorf("a bad override: %v", err)
	}
}
