package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"time"
)

// latencyRow is one row of loadgen's latency_us, in microseconds. The
// report's curve is left out: the run's own file keeps it.
type latencyRow struct {
	Count int     `json:"count"`
	P50   float64 `json:"p50"`
	P90   float64 `json:"p90"`
	P99   float64 `json:"p99"`
	P999  float64 `json:"p99_9"`
	P9999 float64 `json:"p99_99"`
	Max   float64 `json:"max"`
}

// tradeCounts are loadgen's trade counts of a run.
type tradeCounts struct {
	Replies int `json:"replies"`
	Pushes  int `json:"pushes"`
	Metric  int `json:"metric"`
}

// loadgenReport is the part of loadgen's JSON that the runner reads or
// carries into its records.
type loadgenReport struct {
	Valid        bool                  `json:"valid"`
	Problems     []string              `json:"problems"`
	TargetRate   float64               `json:"target_rate"`
	AchievedRate float64               `json:"achieved_rate"`
	Outcomes     map[string]int        `json:"outcomes"`
	Trades       tradeCounts           `json:"trades"`
	Latency      map[string]latencyRow `json:"latency_us"`
	DurationS    float64               `json:"duration_s"` // of the measured part
	Series       json.RawMessage       `json:"series"`     // per second; only the OE1 modes have one. Read apart from the rest (reportSeries) so that nothing in it can fail the report
}

// reportSeries is the part of loadgen's series that the runner reads to align
// it with the engine machine's wake meter: when the measured part began, in wall
// clock, and how many windows it has.
type reportSeries struct {
	WindowS float64   `json:"window_s"`
	Start   time.Time `json:"start"`
	N       []int     `json:"n"`
	End     time.Time `json:"-"` // the end of the measured part, set by decodeSeries
}

// window is the width of a window of the series, a second if the report does
// not say.
func (s reportSeries) window() time.Duration {
	if s.WindowS > 0 {
		return time.Duration(s.WindowS * float64(time.Second))
	}
	return time.Second
}

// decodeSeries reads the series of rep, or returns nil if the report has none,
// as a REST run's has not. A series that has no start or no window is an error.
// The measured part ends where the report's duration_s says if that falls inside
// the last window, which is shorter than the others when the duration is not a
// whole number of them, and else at the end of the last window.
func decodeSeries(rep *loadgenReport) (*reportSeries, error) {
	if rep == nil || len(rep.Series) == 0 || string(rep.Series) == "null" {
		return nil, nil
	}
	var ser reportSeries
	err := json.Unmarshal(rep.Series, &ser)
	if err == nil && (ser.Start.IsZero() || len(ser.N) == 0) {
		err = errors.New("no start or no windows")
	}
	if err != nil {
		return nil, err
	}
	ser.End = ser.Start.Add(time.Duration(len(ser.N)) * ser.window())
	if end := ser.Start.Add(time.Duration(math.Round(rep.DurationS * float64(time.Second)))); end.Before(ser.End) && end.After(ser.End.Add(-ser.window())) {
		ser.End = end
	}
	return &ser, nil
}

// readReport parses the report loadgen wrote to path.
func readReport(path string) (*loadgenReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r loadgenReport
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// A parsable object that is no report would pass every rule below.
	if !(r.TargetRate > 0) {
		return nil, fmt.Errorf("%s: no target_rate above 0, so not a loadgen report", path)
	}
	return &r, nil
}

// Verdicts of a study rule. A rule that does not apply to a flow counts as
// met.
const (
	verdictPass = "pass"
	verdictFail = "fail"
	verdictNA   = "n/a"
)

// ruleResult is one rule of the study's validity test with the value it
// was tested on.
type ruleResult struct {
	Rule    string `json:"rule"`
	Value   string `json:"value"`
	Want    string `json:"want"`
	Verdict string `json:"verdict"`
}

// evaluate applies the study's rules that loadgen's report decides to the
// run of c. They are the study's own and do not depend on loadgen's own
// verdict, which is one of them. repErr says why there is no report, when
// there is none: every rule that applies then fails.
func evaluate(c config, rep *loadgenReport, repErr error, maxSendLag time.Duration) []ruleResult {
	return []ruleResult{
		rateRule(rep, repErr),
		failuresRule(rep, repErr),
		skipRule(c, rep, repErr),
		sendLagRule(c, rep, repErr, maxSendLag),
		validRule(rep, repErr),
	}
}

func noReport(rule, want string, err error) ruleResult {
	return ruleResult{Rule: rule, Value: fmt.Sprintf("no report: %v", err), Want: want, Verdict: verdictFail}
}

func verdictOf(ok bool) string {
	if ok {
		return verdictPass
	}
	return verdictFail
}

func rateRule(rep *loadgenReport, repErr error) ruleResult {
	const want = ">= 0.99 of target"
	if rep == nil {
		return noReport("rate", want, repErr)
	}
	return ruleResult{
		Rule:    "rate",
		Value:   fmt.Sprintf("%g of %g (%.5f)", rep.AchievedRate, rep.TargetRate, rep.AchievedRate/rep.TargetRate),
		Want:    want,
		Verdict: verdictOf(rep.AchievedRate >= 0.99*rep.TargetRate),
	}
}

func failuresRule(rep *loadgenReport, repErr error) ruleResult {
	const want = "0 busy, 0 error"
	if rep == nil {
		return noReport("failures", want, repErr)
	}
	busy, failed := rep.Outcomes["busy"], rep.Outcomes["error"]
	return ruleResult{
		Rule:    "failures",
		Value:   fmt.Sprintf("%d busy, %d error", busy, failed),
		Want:    want,
		Verdict: verdictOf(busy == 0 && failed == 0),
	}
}

func skipRule(c config, rep *loadgenReport, repErr error) ruleResult {
	const want = "< 0.001 of the cancels"
	switch {
	case !c.gatesCancelSkips():
		return ruleResult{Rule: "cancel_skip", Want: "n/a for flow " + c.Flow, Verdict: verdictNA}
	case rep == nil:
		return noReport("cancel_skip", want, repErr)
	}
	o := rep.Outcomes
	skipped := o["cancel_skipped"]
	cancels := o["cancel_canceled"] + o["cancel_finished"] + o["cancel_unknown"] + skipped
	frac := 0.0
	if cancels > 0 {
		frac = float64(skipped) / float64(cancels)
	}
	return ruleResult{
		Rule:  "cancel_skip",
		Value: fmt.Sprintf("%d of %d (%.5f)", skipped, cancels, frac),
		Want:  want,
		// In integers: 100 of 100,000 is 0.001, which is not below it.
		Verdict: verdictOf(skipped*1000 < cancels || cancels == 0),
	}
}

func sendLagRule(c config, rep *loadgenReport, repErr error, maxSendLag time.Duration) ruleResult {
	bound := float64(maxSendLag) / float64(time.Microsecond)
	want := fmt.Sprintf("p99.9 <= %g us", bound)
	if !c.bounded() {
		r := ruleResult{Rule: "send_lag", Want: "n/a: recorded, not gated", Verdict: verdictNA}
		if rep != nil {
			r.Value = fmt.Sprintf("p99.9 %g us", rep.Latency["send_lag"].P999)
		}
		return r
	}
	if rep == nil {
		return noReport("send_lag", want, repErr)
	}
	row, ok := rep.Latency["send_lag"]
	if !ok {
		return ruleResult{Rule: "send_lag", Value: "no send_lag row", Want: want, Verdict: verdictFail}
	}
	return ruleResult{
		Rule:    "send_lag",
		Value:   fmt.Sprintf("p99.9 %g us", row.P999),
		Want:    want,
		Verdict: verdictOf(row.P999 <= bound),
	}
}

func validRule(rep *loadgenReport, repErr error) ruleResult {
	if rep == nil {
		return noReport("loadgen_valid", "valid", repErr)
	}
	value := "valid"
	if !rep.Valid {
		value = "invalid: " + strings.Join(rep.Problems, "; ")
	}
	return ruleResult{Rule: "loadgen_valid", Value: value, Want: "valid", Verdict: verdictOf(rep.Valid)}
}

// startRule is the only rule of a run whose engine could not be started: no
// loadgen ran, so no other rule has anything to decide. err says why.
func startRule(engineConfig string, err error) ruleResult {
	return ruleResult{Rule: "engine_start", Value: err.Error(), Want: "engine started in config " + engineConfig, Verdict: verdictFail}
}

// stableRule is the study's last rule: the supervisor started no engine
// during the run, which it counts in starts. after is the engine's host
// record read after the run, or nil with err saying why it was not read.
func stableRule(before int, after *hostRecord, err error) ruleResult {
	r := ruleResult{Rule: "engine_stable", Want: "starts unchanged"}
	switch {
	case after == nil:
		r.Value, r.Verdict = fmt.Sprintf("starts %d before, host record not read after: %v", before, err), verdictFail
	case after.Starts != before:
		r.Value = fmt.Sprintf("starts %d before, %d after: the supervisor started an engine during the run", before, after.Starts)
		r.Verdict = verdictFail
	default:
		r.Value, r.Verdict = fmt.Sprintf("starts %d before and after", before), verdictPass
	}
	return r
}

// allMet reports whether no rule failed.
func allMet(rules []ruleResult) bool {
	for _, r := range rules {
		if r.Verdict == verdictFail {
			return false
		}
	}
	return true
}
