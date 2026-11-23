package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ruleNamed(t *testing.T, rules []ruleResult, name string) ruleResult {
	t.Helper()
	for _, r := range rules {
		if r.Rule == name {
			return r
		}
	}
	t.Fatalf("no rule %q in %+v", name, rules)
	return ruleResult{}
}

// TestEvaluate holds each study rule to both sides of its boundary, for the
// flows it applies to and those it does not.
func TestEvaluate(t *testing.T) {
	const bound = 10 * time.Microsecond
	// skips are the cancels of a run that made total of them, skipped of which were not sent.
	skips := func(skipped, total int) map[string]int {
		return map[string]int{"cancel_canceled": total - skipped - 900, "cancel_finished": 800, "cancel_unknown": 100, "cancel_skipped": skipped}
	}
	lag := func(p999 float64) map[string]latencyRow {
		return map[string]latencyRow{"send_lag": {P999: p999}}
	}
	tests := []struct {
		name    string
		flow    string
		report  cannedReport
		rule    string
		verdict string
	}{
		{"rate 98.9% fails", flowR1, cannedReport{Valid: true, Target: 10000, Achieved: 9890}, "rate", verdictFail},
		{"rate 99% passes", flowR1, cannedReport{Valid: true, Target: 10000, Achieved: 9900}, "rate", verdictPass},
		{"rate above target passes", flowR1, cannedReport{Valid: true, Target: 10000, Achieved: 10050}, "rate", verdictPass},
		{"rate of the high flows", flowR3, cannedReport{Valid: true, Target: 71429, Achieved: 70714}, "rate", verdictFail},
		{"rate of the high flows at 99%", flowR3, cannedReport{Valid: true, Target: 71429, Achieved: 70714.71}, "rate", verdictPass},

		{"no busy or error passes", flowR1, cannedReport{Valid: true, Target: 1, Achieved: 1, Outcomes: map[string]int{"busy": 0, "error": 0}}, "failures", verdictPass},
		{"absent busy and error pass", flowR1, cannedReport{Valid: true, Target: 1, Achieved: 1}, "failures", verdictPass},
		{"one busy fails", flowR1, cannedReport{Valid: true, Target: 1, Achieved: 1, Outcomes: map[string]int{"busy": 1}}, "failures", verdictFail},
		{"one error fails", flowR1, cannedReport{Valid: true, Target: 1, Achieved: 1, Outcomes: map[string]int{"error": 1}}, "failures", verdictFail},

		{"R3 skips 0.099% pass", flowR3, cannedReport{Valid: true, Target: 1, Achieved: 1, Outcomes: map[string]int{"cancel_canceled": 99901, "cancel_skipped": 99}}, "cancel_skip", verdictPass},
		{"R3 skips 0.1% fail: strictly below", flowR3, cannedReport{Valid: true, Target: 1, Achieved: 1, Outcomes: map[string]int{"cancel_canceled": 99900, "cancel_skipped": 100}}, "cancel_skip", verdictFail},
		{"T skips 0.1% fail", flowT, cannedReport{Valid: true, Target: 1, Achieved: 1, Outcomes: skips(100, 100000)}, "cancel_skip", verdictFail},
		{"T skips 0.099% pass", flowT, cannedReport{Valid: true, Target: 1, Achieved: 1, Outcomes: skips(99, 100000)}, "cancel_skip", verdictPass},
		{"REST skips 0.1% fail", flowREST, cannedReport{Valid: true, Target: 1, Achieved: 1, Outcomes: map[string]int{"cancel_canceled": 99900, "cancel_skipped": 100}}, "cancel_skip", verdictFail},
		{"R3 counts cancel_unknown among the cancels", flowR3, cannedReport{Valid: true, Target: 1, Achieved: 1, Outcomes: map[string]int{"cancel_canceled": 50000, "cancel_unknown": 50000, "cancel_skipped": 100}}, "cancel_skip", verdictPass},
		{"R3 without skips passes", flowR3, cannedReport{Valid: true, Target: 1, Achieved: 1, Outcomes: map[string]int{"cancel_canceled": 5}}, "cancel_skip", verdictPass},
		{"R3 without cancels passes", flowR3, cannedReport{Valid: true, Target: 1, Achieved: 1}, "cancel_skip", verdictPass},
		{"F is not gated on skips", flowF, cannedReport{Valid: true, Target: 1, Achieved: 1, Outcomes: skips(5000, 100000)}, "cancel_skip", verdictNA},
		{"R1 is not gated on skips", flowR1, cannedReport{Valid: true, Target: 1, Achieved: 1, Outcomes: skips(5000, 100000)}, "cancel_skip", verdictNA},
		{"R2 is not gated on skips", flowR2, cannedReport{Valid: true, Target: 1, Achieved: 1, Outcomes: skips(5000, 100000)}, "cancel_skip", verdictNA},

		{"send lag exactly at the bound passes", flowR1, cannedReport{Valid: true, Target: 1, Achieved: 1, Latency: lag(10)}, "send_lag", verdictPass},
		{"send lag above the bound fails", flowR1, cannedReport{Valid: true, Target: 1, Achieved: 1, Latency: lag(10.001)}, "send_lag", verdictFail},
		{"send lag of F is bounded", flowF, cannedReport{Valid: true, Target: 1, Achieved: 1, Latency: lag(11)}, "send_lag", verdictFail},
		{"send lag of T is bounded", flowT, cannedReport{Valid: true, Target: 1, Achieved: 1, Latency: lag(11)}, "send_lag", verdictFail},
		{"a missing send lag row fails", flowR1, cannedReport{Valid: true, Target: 1, Achieved: 1}, "send_lag", verdictFail},
		{"REST send lag is not gated", flowREST, cannedReport{Valid: true, Target: 1, Achieved: 1, Latency: lag(1179)}, "send_lag", verdictNA},

		{"loadgen valid passes", flowR1, cannedReport{Valid: true, Target: 1, Achieved: 1}, "loadgen_valid", verdictPass},
		{"loadgen invalid fails", flowR1, cannedReport{Valid: false, Problems: []string{"1 trade pushes missing"}, Target: 1, Achieved: 1}, "loadgen_valid", verdictFail},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rules := evaluate(config{Flow: tc.flow}, decodeReport(t, tc.report.JSON()), nil, bound)
			if got := ruleNamed(t, rules, tc.rule); got.Verdict != tc.verdict {
				t.Errorf("%s of %s is %s (%s), want %s", tc.rule, tc.flow, got.Verdict, got.Value, tc.verdict)
			}
		})
	}
}

func TestEvaluateRuleValues(t *testing.T) {
	r := cannedReport{
		Valid: false, Problems: []string{"p one", "p two"}, Target: 10000, Achieved: 9890,
		Outcomes: map[string]int{"busy": 2, "error": 3, "cancel_canceled": 90, "cancel_skipped": 10},
		Latency:  map[string]latencyRow{"send_lag": {P999: 12.5}},
	}
	rules := evaluate(config{Flow: flowR3}, decodeReport(t, r.JSON()), nil, 10*time.Microsecond)
	want := map[string][2]string{
		"rate":          {"9890 of 10000 (0.98900)", ">= 0.99 of target"},
		"failures":      {"2 busy, 3 error", "0 busy, 0 error"},
		"cancel_skip":   {"10 of 100 (0.10000)", "< 0.001 of the cancels"},
		"send_lag":      {"p99.9 12.5 us", "p99.9 <= 10 us"},
		"loadgen_valid": {"invalid: p one; p two", "valid"},
	}
	if len(rules) != len(want) {
		t.Fatalf("%d rules, want %d", len(rules), len(want))
	}
	for _, rule := range rules {
		w := want[rule.Rule]
		if rule.Value != w[0] || rule.Want != w[1] || rule.Verdict != verdictFail {
			t.Errorf("%s: value %q, want %q, verdict %s; want %q and fail", rule.Rule, rule.Value, rule.Want, rule.Verdict, w)
		}
	}
	// The order is the study's.
	names := make([]string, len(rules))
	for i, rule := range rules {
		names[i] = rule.Rule
	}
	if got := strings.Join(names, " "); got != "rate failures cancel_skip send_lag loadgen_valid" {
		t.Errorf("rules in the order %s", got)
	}
}

func TestEvaluateRESTRecordsSendLag(t *testing.T) {
	r := cannedReport{Valid: true, Target: 1, Achieved: 1, Latency: map[string]latencyRow{"send_lag": {P999: 1179}}}
	got := ruleNamed(t, evaluate(config{Flow: flowREST}, decodeReport(t, r.JSON()), nil, 10*time.Microsecond), "send_lag")
	if got.Value != "p99.9 1179 us" || got.Want != "n/a: recorded, not gated" {
		t.Errorf("REST send lag rule is %+v", got)
	}
}

func TestEvaluateWithoutReport(t *testing.T) {
	why := errors.New("no such file")
	for _, flow := range []string{flowF, flowR1, flowR3, flowREST} {
		cfg := config{Flow: flow}
		rules := evaluate(cfg, nil, why, 10*time.Microsecond)
		if allMet(rules) {
			t.Errorf("%s with no report is valid", flow)
		}
		for _, r := range rules {
			applies := r.Rule == "rate" || r.Rule == "failures" || r.Rule == "loadgen_valid" ||
				r.Rule == "send_lag" && cfg.bounded() || r.Rule == "cancel_skip" && cfg.gatesCancelSkips()
			if applies && (r.Verdict != verdictFail || !strings.Contains(r.Value, "no such file")) {
				t.Errorf("%s of %s: %s %q, want a failure that says why", r.Rule, flow, r.Verdict, r.Value)
			}
			if !applies && r.Verdict != verdictNA {
				t.Errorf("%s of %s: %s, want n/a", r.Rule, flow, r.Verdict)
			}
		}
	}
}

func TestAllMet(t *testing.T) {
	pass, na, fail := ruleResult{Verdict: verdictPass}, ruleResult{Verdict: verdictNA}, ruleResult{Verdict: verdictFail}
	for _, c := range []struct {
		rules []ruleResult
		want  bool
	}{
		{nil, true}, {[]ruleResult{pass, na, pass}, true}, {[]ruleResult{pass, fail}, false}, {[]ruleResult{fail}, false},
	} {
		if got := allMet(c.rules); got != c.want {
			t.Errorf("allMet(%v) = %v, want %v", c.rules, got, c.want)
		}
	}
}

func TestStableRule(t *testing.T) {
	for _, c := range []struct {
		name    string
		before  int
		after   *hostRecord
		err     error
		verdict string
	}{
		{"unchanged", 7, &hostRecord{Starts: 7}, nil, verdictPass},
		{"one start during the run", 7, &hostRecord{Starts: 8}, nil, verdictFail},
		{"fewer starts: a restarted supervisor", 7, &hostRecord{Starts: 1}, nil, verdictFail},
		{"record not read", 7, nil, errors.New("connection refused"), verdictFail},
	} {
		got := stableRule(c.before, c.after, c.err)
		if got.Rule != "engine_stable" || got.Verdict != c.verdict {
			t.Errorf("%s: %+v, want verdict %s", c.name, got, c.verdict)
		}
	}
	if got := stableRule(7, &hostRecord{Starts: 8}, nil); !strings.Contains(got.Value, "the supervisor started an engine during the run") {
		t.Errorf("value %q does not say what happened", got.Value)
	}
}

func TestReadReport(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := validReport(flowR1, 60, 70)
	rep, err := readReport(write("good.json", string(good.JSON())))
	if err != nil || !rep.Valid || rep.TargetRate != 10000 || rep.Latency["reply"].P99 != 70 {
		t.Errorf("good report read as %+v, %v", rep, err)
	}
	for name, content := range map[string]string{
		"empty.json":     "",
		"truncated.json": string(good.JSON()[:40]),
		"trailing.json":  string(good.JSON()) + "x",
		"array.json":     "[]",
		"object.json":    "{}",
		"notarget.json":  `{"valid": true, "achieved_rate": 5}`,
	} {
		if rep, err := readReport(write(name, content)); err == nil {
			t.Errorf("%s read as %+v, want an error", name, rep)
		}
	}
	if _, err := readReport(filepath.Join(dir, "missing.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: %v, want not exist", err)
	}
}
