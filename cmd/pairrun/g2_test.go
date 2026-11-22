package main

import (
	"strings"
	"testing"
)

// result is what a test decides about a run: whether it was valid and the p50
// and p99 of its main row (pong for F, reply for the others).
type result struct {
	valid    bool
	p50, p99 float64
}

// matrix makes the records of rounds rounds of the nine configurations, in
// the order they run, each as f says.
func matrix(rounds int, f func(config string, round int) result) []runRecord {
	var records []runRecord
	for round := 1; round <= rounds; round++ {
		for _, c := range planConfigs(defaultParams, "oe", "http") {
			r := f(c.Name, round)
			records = append(records, runRecord{
				Seq: len(records) + 1, Round: round, Config: c.Name, Flow: c.Flow, Valid: r.valid,
				Latency: flowRows(c.Flow, r.p50, r.p99),
			})
		}
	}
	return records
}

// good is a run that passes every condition: R1's reply is 40 us above the
// floor, and every p99 is 100 us.
func good(config string, _ int) result {
	if config == "F" {
		return result{true, 20, 100}
	}
	return result{true, 60, 100}
}

// with changes the runs of good that pick selects.
func with(pick func(config string, round int) bool, r result) func(string, int) result {
	return func(config string, round int) result {
		if pick(config, round) {
			return r
		}
		return good(config, round)
	}
}

func g2Of(records []runRecord, rounds int, finished bool) g2Result {
	return computeG2(planConfigs(defaultParams, "oe", "http"), records, rounds, finished)
}

func configStatus(t *testing.T, g g2Result, name string) configCount {
	t.Helper()
	for _, n := range g.Cond1.Configs {
		if n.Config == name {
			return n
		}
	}
	t.Fatalf("no configuration %s in condition 1", name)
	return configCount{}
}

func TestG2AllGood(t *testing.T) {
	g := g2Of(matrix(5, good), 5, true)
	if !g.Passed || !g.Complete || g.Cond1.Status != statusPass || g.Cond2.Status != statusPass || g.Cond3.Status != statusPass || g.EarlyStop {
		t.Errorf("good matrix: %+v", g)
	}
	if len(g.Cond1.Configs) != 9 || g.MinValid != 4 || g.Rounds != 5 {
		t.Errorf("%d configurations, min valid %d, rounds %d, want 9, 4, 5", len(g.Cond1.Configs), g.MinValid, g.Rounds)
	}
}

func TestG2Condition1(t *testing.T) {
	invalid := func(name string, rounds ...int) func(string, int) result {
		return with(func(c string, r int) bool {
			if c != name {
				return false
			}
			for _, x := range rounds {
				if x == r {
					return true
				}
			}
			return false
		}, result{false, 60, 100})
	}
	for _, c := range []struct {
		name        string
		rounds      int // rounds made
		f           func(string, int) result
		config      string
		status      string
		cond1       string
		earlyStop   bool
		passed      bool
		wantValid   int
		wantInvalid int
	}{
		{"4 of 5 valid", 5, invalid("R3-10k", 2), "R3-10k", statusPass, statusPass, false, true, 4, 1},
		{"3 of 5 valid", 5, invalid("R3-10k", 2, 4), "R3-10k", statusFail, statusFail, false, false, 3, 2},
		{"the second invalid run fails it at once", 2, invalid("R3-10k", 1, 2), "R3-10k", statusFail, statusFail, false, false, 0, 2},
		{"one invalid run in two rounds is still open", 2, invalid("R3-10k", 1), "R3-10k", statusOpen, statusOpen, false, false, 1, 1},
		{"F failing stops the matrix", 2, invalid("F", 1, 2), "F", statusFail, statusFail, true, false, 0, 2},
		{"R1 failing stops the matrix", 2, invalid("R1", 1, 2), "R1", statusFail, statusFail, true, false, 0, 2},
		{"R1-gogc-off failing does not stop it", 2, invalid("R1-gogc-off", 1, 2), "R1-gogc-off", statusFail, statusFail, false, false, 0, 2},
		{"REST-10k failing does not stop it", 2, invalid("REST-10k", 1, 2), "REST-10k", statusFail, statusFail, false, false, 0, 2},
		{"T-71k failing does not stop it", 5, invalid("T-71k", 1, 2), "T-71k", statusFail, statusFail, false, false, 3, 2},
		{"F with one invalid run goes on", 2, invalid("F", 1), "F", statusOpen, statusOpen, false, false, 1, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := g2Of(matrix(c.rounds, c.f), 5, c.rounds == 5)
			n := configStatus(t, g, c.config)
			if n.Status != c.status || n.Valid != c.wantValid || n.Invalid != c.wantInvalid || n.Runs != c.rounds {
				t.Errorf("%s: %+v, want status %s, %d valid, %d invalid of %d", c.config, n, c.status, c.wantValid, c.wantInvalid, c.rounds)
			}
			if g.Cond1.Status != c.cond1 || g.EarlyStop != c.earlyStop || g.Passed != c.passed {
				t.Errorf("condition 1 %s, early stop %v (%s), passed %v; want %s, %v, %v",
					g.Cond1.Status, g.EarlyStop, g.EarlyStopReason, g.Passed, c.cond1, c.earlyStop, c.passed)
			}
			if c.earlyStop && !strings.Contains(g.EarlyStopReason, "condition 1") {
				t.Errorf("early stop reason %q does not name condition 1", g.EarlyStopReason)
			}
		})
	}
}

func TestG2Condition2(t *testing.T) {
	r1p99 := func(values ...float64) func(string, int) result {
		return func(c string, round int) result {
			r := good(c, round)
			if c == "R1" && round <= len(values) {
				r.p99 = values[round-1]
			}
			return r
		}
	}
	for _, c := range []struct {
		name      string
		rounds    int
		p99       []float64
		status    string
		earlyStop bool
	}{
		{"ratio exactly 2", 5, []float64{100, 200, 150, 120, 100}, statusPass, false},
		{"ratio just above 2", 5, []float64{100, 200.0001, 150, 120, 100}, statusFail, true},
		{"two runs more than 2x apart fail at once", 2, []float64{100, 201}, statusFail, true},
		{"two runs exactly 2x apart are open", 2, []float64{100, 200}, statusOpen, false},
		{"low and high end exactly 2x apart", 5, []float64{50, 100, 100, 100, 100}, statusPass, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := g2Of(matrix(c.rounds, r1p99(c.p99...)), 5, c.rounds == 5)
			if g.Cond2.Status != c.status || g.EarlyStop != c.earlyStop {
				t.Errorf("condition 2 %s, early stop %v, want %s and %v; %+v", g.Cond2.Status, g.EarlyStop, c.status, c.earlyStop, g.Cond2)
			}
			if c.earlyStop && !strings.Contains(g.EarlyStopReason, "condition 2") {
				t.Errorf("early stop reason %q does not name condition 2", g.EarlyStopReason)
			}
			if c.status == statusPass && !g.Passed {
				t.Errorf("not passed: %+v", g)
			}
			if c.earlyStop && g.Passed {
				t.Error("passed after a failed condition")
			}
		})
	}

	t.Run("invalid runs are not counted", func(t *testing.T) {
		f := func(c string, round int) result {
			r := good(c, round)
			if c == "R1" && round == 3 {
				r.valid, r.p99 = false, 10000
			}
			return r
		}
		g := g2Of(matrix(5, f), 5, true)
		if g.Cond2.Status != statusPass || *g.Cond2.ValidRuns != 4 || *g.Cond2.MaxP99 != 100 || *g.Cond2.MinP99 != 100 || *g.Cond2.Ratio != 1 {
			t.Errorf("condition 2 %+v, want pass over 4 valid runs of 100 us", g.Cond2)
		}
	})
	t.Run("one valid run decides nothing", func(t *testing.T) {
		g := g2Of(matrix(1, good), 5, false)
		if g.Cond2.Status != statusOpen || *g.Cond2.ValidRuns != 1 {
			t.Errorf("condition 2 %+v, want open", g.Cond2)
		}
	})
	t.Run("a run of 0 us has no ratio and fails", func(t *testing.T) {
		g := g2Of(matrix(2, r1p99(0, 5)), 5, false)
		if g.Cond2.Status != statusFail || g.Cond2.Ratio != nil {
			t.Errorf("condition 2 %+v, want fail without a ratio", g.Cond2)
		}
	})
	t.Run("R1 not yet complete is open though the spread is fine", func(t *testing.T) {
		g := g2Of(matrix(4, good), 5, false)
		if g.Cond2.Status != statusOpen {
			t.Errorf("condition 2 %s after 4 of 5 rounds, want open", g.Cond2.Status)
		}
	})
}

func TestG2Condition3(t *testing.T) {
	// R1's reply p50 is 130 us at the median of 100, 140, 120 and 160 (the
	// run of round 3 is invalid): the mean of 120 and 140.
	r1 := func(floor float64) func(string, int) result {
		return func(c string, round int) result {
			switch c {
			case "F":
				return result{true, floor, 100}
			case "R1":
				return result{round != 3, []float64{100, 140, 9999, 120, 160}[round-1], 100}
			}
			return good(c, round)
		}
	}
	for _, c := range []struct {
		name   string
		floor  float64
		status string
	}{
		{"exactly 100 us over the floor", 30, statusPass},
		{"just over 100 us", 29.9999, statusFail},
		{"under", 50, statusPass},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := g2Of(matrix(5, r1(c.floor)), 5, true)
			if g.Cond3.Status != c.status || *g.Cond3.R1Median != 130 || *g.Cond3.FloorMedian != c.floor {
				t.Errorf("condition 3 %+v, want %s with R1 median 130 and floor %v", g.Cond3, c.status, c.floor)
			}
			if c.status == statusPass && !g.Passed || c.status == statusFail && g.Passed {
				t.Errorf("passed %v with condition 3 %s", g.Passed, g.Cond3.Status)
			}
		})
	}
	t.Run("open until the rounds are done or the matrix has stopped", func(t *testing.T) {
		if g := g2Of(matrix(3, r1(30)), 5, false); g.Cond3.Status != statusOpen {
			t.Errorf("condition 3 %s mid-matrix, want open", g.Cond3.Status)
		}
		if g := g2Of(matrix(3, r1(30)), 5, true); g.Cond3.Status == statusOpen {
			t.Error("condition 3 open after the matrix stopped")
		}
		if g := g2Of(matrix(5, r1(30)), 5, false); g.Cond3.Status != statusPass {
			t.Errorf("condition 3 %s after all rounds, want pass", g.Cond3.Status)
		}
	})
	t.Run("no valid floor run", func(t *testing.T) {
		f := func(c string, round int) result {
			r := good(c, round)
			r.valid = r.valid && c != "F"
			return r
		}
		g := g2Of(matrix(5, f), 5, true)
		if g.Cond3.Status != statusFail || g.Cond3.Difference != nil {
			t.Errorf("condition 3 %+v, want fail without a difference", g.Cond3)
		}
	})
}

func TestG2IncompleteMatrixDoesNotPass(t *testing.T) {
	// The deadline stopped the matrix in round 5: every number is fine and one run is missing.
	records := matrix(5, good)
	g := g2Of(records[:len(records)-1], 5, true)
	if g.Passed || g.Complete {
		t.Errorf("passed %v, complete %v for 44 of 45 runs", g.Passed, g.Complete)
	}
	if n := configStatus(t, g, "REST-10k"); n.Runs != 4 || n.Status != statusOpen {
		t.Errorf("REST-10k %+v, want 4 runs and open", n)
	}
}

func TestG2WithFewerRounds(t *testing.T) {
	// A rehearsal of two rounds: all but one run of a configuration must be valid.
	bad := func(n int) func(string, int) result {
		return with(func(c string, r int) bool { return c == "R3-10k" && r <= n }, result{false, 60, 100})
	}
	if g := g2Of(matrix(2, bad(1)), 2, true); g.Cond1.Status != statusPass || !g.Passed || g.MinValid != 1 {
		t.Errorf("one invalid run of 2: %+v", g)
	}
	if g := g2Of(matrix(2, bad(2)), 2, true); g.Cond1.Status != statusFail || g.Passed {
		t.Errorf("two invalid runs of 2: %+v", g)
	}
}

func TestMedian(t *testing.T) {
	for _, c := range []struct {
		in   []float64
		want float64
	}{
		{[]float64{5}, 5},
		{[]float64{3, 1, 2}, 2},
		{[]float64{4, 1}, 2.5},
		{[]float64{40, 10, 30, 20}, 25},
		{[]float64{1, 1, 1, 100}, 1},
		{[]float64{7, 7}, 7},
	} {
		in := append([]float64(nil), c.in...)
		if got := median(c.in); got != c.want {
			t.Errorf("median(%v) = %v, want %v", in, got, c.want)
		}
		for i := range in {
			if in[i] != c.in[i] {
				t.Errorf("median reordered its argument: %v became %v", in, c.in)
				break
			}
		}
	}
}

func TestCompareVariants(t *testing.T) {
	// R1 has p99 of 100 to 140 (median 120, range 40); the variants are made by p99.
	r1 := []float64{100, 140, 120, 110, 130}
	records := func(variantP99 []float64, validVariant, validR1 int) []runRecord {
		var out []runRecord
		for i, v := range r1 {
			out = append(out, runRecord{Config: "R1", Valid: i < validR1, Latency: flowRows(flowR1, 60, v)})
		}
		for i, v := range variantP99 {
			out = append(out, runRecord{Config: "R1-gogc-off", Valid: i < validVariant, Latency: flowRows(flowR1, 60, v)})
		}
		return out
	}
	five := func(v float64) []float64 { return []float64{v, v, v, v, v} }
	for _, c := range []struct {
		name      string
		variant   []float64
		valid     int // of the variant's runs
		validR1   int
		adopted   bool
		gain      float64
		reasonHas string
	}{
		{"gain equal to the range is not adopted", five(80), 5, 5, false, 40, "gain 40 us is not above the range 40 us"},
		{"gain above the range is adopted", five(79), 5, 5, true, 41, ""},
		{"gain below the range", five(100), 5, 5, false, 20, "gain 20 us is not above the range 40 us"},
		{"a slower variant", five(150), 5, 5, false, -30, "gain -30 us"},
		{"four valid runs are enough", five(60), 4, 5, true, 60, ""},
		{"three valid variant runs are not", five(60), 3, 5, false, 60, "R1 has 5 valid runs and R1-gogc-off has 3, want at least 4"},
		{"three valid R1 runs are not", five(60), 5, 3, false, 0, "R1 has 3 valid runs and R1-gogc-off has 5, want at least 4"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := compareVariants(records(c.variant, c.valid, c.validR1), 5, "R1-gogc-off")
			if len(got) != 1 {
				t.Fatalf("%d comparisons, want 1", len(got))
			}
			g := got[0]
			if g.Adopted != c.adopted || !strings.Contains(g.Reason, c.reasonHas) || (c.adopted && g.Reason != "") {
				t.Errorf("adopted %v with reason %q, want %v and %q", g.Adopted, g.Reason, c.adopted, c.reasonHas)
			}
			if c.validR1 == 5 && (g.Gain == nil || *g.Gain != c.gain || *g.Range != 40 || *g.BaseMedian != 120) {
				t.Errorf("gain %v, range %v, R1 median %v; want %v, 40, 120", g.Gain, g.Range, g.BaseMedian, c.gain)
			}
			if g.BaseValid != c.validR1 || g.VariantValid != c.valid {
				t.Errorf("valid counts %d and %d, want %d and %d", g.BaseValid, g.VariantValid, c.validR1, c.valid)
			}
		})
	}

	t.Run("no valid runs at all", func(t *testing.T) {
		got := compareVariants(nil, 5, "R1-gogc-off", "R1-spin-pin")
		if len(got) != 2 || got[0].Adopted || got[1].Adopted || got[0].Gain != nil || got[0].Reason == "" {
			t.Errorf("got %+v", got)
		}
	})
}
