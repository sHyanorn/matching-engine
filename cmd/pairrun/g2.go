package main

import (
	"fmt"
	"slices"
)

// Conditions of G2 are open until the runs decide them, then pass or fail.
const (
	statusPass = "pass"
	statusFail = "fail"
	statusOpen = "open"
)

const (
	maxP99Spread = 2   // condition 2: largest p99 over smallest, of R1's valid runs
	maxOverFloor = 100 // condition 3: µs of p50 that R1's reply may exceed the floor's
)

// g2Result is the state of the pilot's gate after some runs. Every number
// is computed from the valid runs only.
type g2Result struct {
	Rounds   int  `json:"rounds"`
	MinValid int  `json:"min_valid"` // valid runs a configuration needs
	Complete bool `json:"complete"`  // every configuration has all its rounds
	Passed   bool `json:"passed"`
	Cond1    cond `json:"condition_1"`
	Cond2    cond `json:"condition_2"`
	Cond3    cond `json:"condition_3"`
	// EarlyStop says that the matrix should run no further: condition 1 can
	// no longer be met for F or R1, or condition 2 has failed.
	EarlyStop       bool   `json:"early_stop"`
	EarlyStopReason string `json:"early_stop_reason,omitempty"`
}

// cond is one condition: its status and the numbers it used. Fields that a
// condition does not use, or cannot compute yet, are left out.
type cond struct {
	Status string `json:"status"`
	// Condition 1: each configuration's runs.
	Configs []configCount `json:"configs,omitempty"`
	// Condition 2: R1's valid runs, p99 of the reply row, in µs.
	ValidRuns *int     `json:"valid_runs,omitempty"`
	MinP99    *float64 `json:"min_p99,omitempty"`
	MaxP99    *float64 `json:"max_p99,omitempty"`
	Ratio     *float64 `json:"ratio,omitempty"` // max over min, where min is above 0
	Limit     *float64 `json:"limit,omitempty"`
	// Condition 3: medians of p50 over valid runs, in µs.
	R1Median    *float64 `json:"r1_reply_p50_median,omitempty"`
	FloorMedian *float64 `json:"floor_pong_p50_median,omitempty"`
	Difference  *float64 `json:"difference,omitempty"`
}

type configCount struct {
	Config  string `json:"config"`
	Runs    int    `json:"runs"`
	Valid   int    `json:"valid"`
	Invalid int    `json:"invalid"`
	Status  string `json:"status"`
}

// computeG2 evaluates G2 over the runs so far of a matrix of rounds rounds
// of configs. finished says that no more runs will follow: it lets
// condition 3 be decided.
func computeG2(configs []config, records []runRecord, rounds int, finished bool) g2Result {
	need := minValid(rounds)
	g := g2Result{Rounds: rounds, MinValid: need, Complete: true}

	g.Cond1.Status = statusPass
	for _, c := range configs {
		n := configCount{Config: c.Name}
		for _, r := range records {
			if r.Config == c.Name {
				n.Runs++
				if r.Valid {
					n.Valid++
				}
			}
		}
		n.Invalid = n.Runs - n.Valid
		switch {
		case n.Invalid > rounds-need:
			n.Status = statusFail
		case n.Runs == rounds && n.Valid >= need:
			n.Status = statusPass
		default:
			n.Status = statusOpen
		}
		if n.Runs < rounds {
			g.Complete = false
		}
		g.Cond1.Configs = append(g.Cond1.Configs, n)
		switch {
		case n.Status == statusFail:
			g.Cond1.Status = statusFail
		case n.Status == statusOpen && g.Cond1.Status == statusPass:
			g.Cond1.Status = statusOpen
		}
		if n.Status == statusFail && (c.Name == floorConfig || c.Name == baseConfig) {
			g.EarlyStop = true
			g.EarlyStopReason = fmt.Sprintf("condition 1 can no longer be met for %s: %d of %d runs invalid", c.Name, n.Invalid, n.Runs)
		}
	}

	// Condition 2.
	p99s := validValues(records, baseConfig, "reply", func(r latencyRow) float64 { return r.P99 })
	g.Cond2.Limit = ptr(float64(maxP99Spread))
	g.Cond2.ValidRuns = ptr(len(p99s))
	g.Cond2.Status = statusOpen
	if len(p99s) > 0 {
		lo, hi := slices.Min(p99s), slices.Max(p99s)
		g.Cond2.MinP99, g.Cond2.MaxP99 = &lo, &hi
		if lo > 0 {
			g.Cond2.Ratio = ptr(hi / lo)
		}
		// Compared as max <= 2 min, which is exact where the ratio is not.
		switch {
		case len(p99s) >= 2 && hi > maxP99Spread*lo:
			g.Cond2.Status = statusFail
			if !g.EarlyStop {
				g.EarlyStop = true
				g.EarlyStopReason = fmt.Sprintf("condition 2 failed: R1 p99 of %g and %g us are more than %dx apart", lo, hi, maxP99Spread)
			}
		case len(p99s) >= need && countOf(g, baseConfig).Runs == rounds:
			g.Cond2.Status = statusPass
		}
	}

	// Condition 3.
	r1 := validValues(records, baseConfig, "reply", func(r latencyRow) float64 { return r.P50 })
	floor := validValues(records, floorConfig, "pong", func(r latencyRow) float64 { return r.P50 })
	g.Cond3.Limit = ptr(float64(maxOverFloor))
	g.Cond3.Status = statusOpen
	if len(r1) > 0 {
		g.Cond3.R1Median = ptr(median(r1))
	}
	if len(floor) > 0 {
		g.Cond3.FloorMedian = ptr(median(floor))
	}
	if g.Cond3.R1Median != nil && g.Cond3.FloorMedian != nil {
		g.Cond3.Difference = ptr(*g.Cond3.R1Median - *g.Cond3.FloorMedian)
	}
	if finished || g.Complete {
		g.Cond3.Status = statusFail // no valid run on one side decides it too
		if g.Cond3.Difference != nil && *g.Cond3.Difference <= maxOverFloor {
			g.Cond3.Status = statusPass
		}
	}

	g.Passed = g.Complete && g.Cond1.Status == statusPass && g.Cond2.Status == statusPass && g.Cond3.Status == statusPass
	return g
}

// countOf returns condition 1's count of the named configuration.
func countOf(g g2Result, name string) configCount {
	for _, n := range g.Cond1.Configs {
		if n.Config == name {
			return n
		}
	}
	return configCount{}
}

// validValues collects pick of the named row from the valid runs of config.
func validValues(records []runRecord, config, rowName string, pick func(latencyRow) float64) []float64 {
	var v []float64
	for _, r := range records {
		if row, ok := r.Latency[rowName]; ok && r.Valid && r.Config == config {
			v = append(v, pick(row))
		}
	}
	return v
}

// median is the middle value, or the mean of the two middle ones.
func median(v []float64) float64 {
	s := slices.Sorted(slices.Values(v))
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

func ptr[T any](v T) *T { return &v }

// tuning compares one engine variant of R1 with R1 itself, by the p99 of the
// reply row over each one's valid runs.
type tuning struct {
	Variant       string   `json:"variant"`
	BaseValid     int      `json:"r1_valid"`
	VariantValid  int      `json:"variant_valid"`
	BaseMedian    *float64 `json:"r1_median_p99"`
	VariantMedian *float64 `json:"variant_median_p99"`
	Gain          *float64 `json:"gain"`  // R1's median minus the variant's
	Range         *float64 `json:"range"` // largest minus smallest p99 of R1's valid runs
	Adopted       bool     `json:"adopted"`
	Reason        string   `json:"reason,omitempty"` // when not adopted
}

// compareVariants judges the variants of R1 by what they gain over R1's own
// spread. A variant is adopted only if the gain is above the range, and
// only if both it and R1 have the valid runs for a median to mean anything.
func compareVariants(records []runRecord, rounds int, variants ...string) []tuning {
	need := minValid(rounds)
	p99 := func(r latencyRow) float64 { return r.P99 }
	base := validValues(records, baseConfig, "reply", p99)
	var out []tuning
	for _, name := range variants {
		v := validValues(records, name, "reply", p99)
		t := tuning{Variant: name, BaseValid: len(base), VariantValid: len(v)}
		if len(base) > 0 {
			t.BaseMedian = ptr(median(base))
			t.Range = ptr(slices.Max(base) - slices.Min(base))
		}
		if len(v) > 0 {
			t.VariantMedian = ptr(median(v))
		}
		if t.BaseMedian != nil && t.VariantMedian != nil {
			t.Gain = ptr(*t.BaseMedian - *t.VariantMedian)
		}
		switch {
		case len(base) < need || len(v) < need:
			t.Reason = fmt.Sprintf("%s has %d valid runs and %s has %d, want at least %d of each", baseConfig, len(base), name, len(v), need)
		case *t.Gain > *t.Range:
			t.Adopted = true
		default:
			t.Reason = fmt.Sprintf("gain %g us is not above the range %g us of %s", *t.Gain, *t.Range, baseConfig)
		}
		out = append(out, t)
	}
	return out
}
