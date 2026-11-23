package main

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// triple is the spread of one percentile over a configuration's valid
// runs, in microseconds. Each is a value of a run's report; the median of
// an even number of runs is the mean of the two middle ones.
type triple struct {
	Min    float64 `json:"min"`
	Median float64 `json:"median"`
	Max    float64 `json:"max"`
}

// spread is the minimum, median and maximum of v. The median is median's: of
// an even number of values, the mean of the two middle ones.
func spread(v []float64) triple {
	return triple{Min: slices.Min(v), Median: median(v), Max: slices.Max(v)}
}

// rowStats are the percentiles that the summary reports for a latency row.
type rowStats struct {
	P50  triple `json:"p50"`
	P99  triple `json:"p99"`
	P999 triple `json:"p999"`
}

// configSummary is a configuration's runs and the spread of its rows over
// the valid ones.
type configSummary struct {
	Name   string              `json:"name"`
	Flow   string              `json:"flow"`
	Engine string              `json:"engine_config"`
	Rate   float64             `json:"rate"`
	Runs   int                 `json:"runs"`
	Valid  int                 `json:"valid"`
	Rows   map[string]rowStats `json:"rows"`
	// Floor is the pair's Ping round trip (the pong row of F) next to every
	// configuration, so that no latency is read without it; nil if F has no
	// valid run.
	Floor *rowStats `json:"floor"`
}

type engineMemory struct {
	LimitBytes      int64 `json:"mem_limit_bytes"`
	GoMemLimitBytes int64 `json:"gomemlimit_bytes"` // what gogc-off sets
}

// summaryFile is summary.json.
type summaryFile struct {
	Rehearsal           bool              `json:"rehearsal"`
	Publishable         bool              `json:"publishable"`
	Overrides           map[string]string `json:"overrides,omitempty"`            // of the client
	SupervisorOverrides map[string]string `json:"supervisor_overrides,omitempty"` // of the supervisor
	Params              summaryParams     `json:"params"`
	Stopped             string            `json:"stopped"` // "", "deadline", "g2", "g2_failed" or "error"
	StopReason          string            `json:"stop_reason,omitempty"`
	Runs                int               `json:"runs"`
	Engine              engineMemory      `json:"engine"`
	EngineClockReadNS   float64           `json:"engine_clock_read_ns"` // mean cost of reading the clock on each machine
	ClientClockReadNS   float64           `json:"client_clock_read_ns"`
	ClockUnfit          bool              `json:"clock_unfit"` // a read costs more than maxClockReadNS on either machine; G2 does not depend on it
	Configs             []configSummary   `json:"configs"`
	Tuning              []tuning          `json:"tuning"`
	G2                  g2Result          `json:"g2"`
}

// maxClockReadNS is the cost of a clock read, in ns, above which a pair is
// recorded as unfit: the latencies it measures are of the order of
// microseconds.
const maxClockReadNS = 100

type summaryParams struct {
	Rate         float64 `json:"rate"`
	RateHigh     float64 `json:"rate_high"`
	WarmupS      float64 `json:"warmup_s"`
	DurationS    float64 `json:"duration_s"`
	LongS        float64 `json:"long_s"`
	MaxSendLagUS float64 `json:"max_send_lag_us"`
	Rounds       int     `json:"rounds"`
}

// summaryInput is what buildSummary needs of a finished or stopped matrix.
type summaryInput struct {
	Params              params
	Overrides           map[string]string // of the client
	SupervisorOverrides map[string]string
	SupervisorSilent    bool     // it never answered, so its overrides are unknown
	Configs             []config // in the order they are listed, the long run last
	Records             []runRecord
	G2                  g2Result
	Memory              engineMemory
	EngineClockReadNS   float64
	ClientClockReadNS   float64
	Stopped             string
	StopReason          string
}

// buildSummary describes the runs without recomputing anything of them: each
// number is a number of a run's report, or the minimum, median or maximum
// of such numbers.
func buildSummary(in summaryInput) summaryFile {
	p := in.Params
	rehearsal := len(in.Overrides) > 0 || len(in.SupervisorOverrides) > 0
	s := summaryFile{
		Rehearsal:           rehearsal,
		Publishable:         !rehearsal && !in.SupervisorSilent,
		Overrides:           in.Overrides,
		SupervisorOverrides: in.SupervisorOverrides,
		Params: summaryParams{
			Rate: p.Rate, RateHigh: p.RateHigh, WarmupS: p.Warmup.Seconds(), DurationS: p.Duration.Seconds(),
			LongS: p.Long.Seconds(), MaxSendLagUS: float64(p.MaxSendLag.Microseconds()), Rounds: p.Rounds,
		},
		Stopped:           in.Stopped,
		StopReason:        in.StopReason,
		Runs:              len(in.Records),
		Engine:            in.Memory,
		EngineClockReadNS: in.EngineClockReadNS,
		ClientClockReadNS: in.ClientClockReadNS,
		ClockUnfit:        in.EngineClockReadNS > maxClockReadNS || in.ClientClockReadNS > maxClockReadNS,
		Tuning:            compareVariants(in.Records, p.Rounds, "R1-gogc-off", "R1-spin-pin"),
		G2:                in.G2,
	}
	var floor *rowStats
	if st, ok := statsOf(in.Records, floorConfig, "pong"); ok {
		floor = &st
	}
	for _, c := range in.Configs {
		cs := configSummary{Name: c.Name, Flow: c.Flow, Engine: c.Engine, Rate: c.Rate, Rows: map[string]rowStats{}, Floor: floor}
		for _, r := range in.Records {
			if r.Config != c.Name {
				continue
			}
			cs.Runs++
			if r.Valid {
				cs.Valid++
			}
		}
		for _, row := range rowsOf(in.Records, c.Name) {
			cs.Rows[row], _ = statsOf(in.Records, c.Name, row)
		}
		s.Configs = append(s.Configs, cs)
	}
	return s
}

// statsOf gives the spread of the named row over the valid runs of config,
// or false if none has it.
func statsOf(records []runRecord, config, row string) (rowStats, bool) {
	p50 := validValues(records, config, row, func(r latencyRow) float64 { return r.P50 })
	if len(p50) == 0 {
		return rowStats{}, false
	}
	return rowStats{
		P50:  spread(p50),
		P99:  spread(validValues(records, config, row, func(r latencyRow) float64 { return r.P99 })),
		P999: spread(validValues(records, config, row, func(r latencyRow) float64 { return r.P999 })),
	}, true
}

// rowOrder is the order in which rows are listed; a row it does not name
// comes after, by name.
var rowOrder = []string{"trade_push", "pong", "reply", "cancel", "order_to_cancel", "service", "send_lag", "hiccup"}

// rowsOf lists the rows that the valid runs of config report.
func rowsOf(records []runRecord, config string) []string {
	seen := map[string]bool{}
	for _, r := range records {
		if r.Config == config && r.Valid {
			for row := range r.Latency {
				seen[row] = true
			}
		}
	}
	return sortRows(slices.Collect(maps.Keys(seen)))
}

// sortRows puts rows in rowOrder.
func sortRows(rows []string) []string {
	rank := func(row string) int {
		if i := slices.Index(rowOrder, row); i >= 0 {
			return i
		}
		return len(rowOrder)
	}
	slices.SortFunc(rows, func(a, b string) int {
		if d := rank(a) - rank(b); d != 0 {
			return d
		}
		return strings.Compare(a, b)
	})
	return rows
}

// text is summary.txt: the same facts as a table to read.
func (s summaryFile) text() string {
	var b strings.Builder
	if s.Rehearsal {
		b.WriteString("REHEARSAL: not publishable\n")
	}
	p := s.Params
	fmt.Fprintf(&b, "Pilot matrix: %d rounds, %g/s and %g/s, %gs warmup and %gs measured (%gs for the long run), send lag p99.9 at most %g us\n",
		p.Rounds, p.Rate, p.RateHigh, p.WarmupS, p.DurationS, p.LongS, p.MaxSendLagUS)
	for _, k := range slices.Sorted(maps.Keys(s.Overrides)) {
		fmt.Fprintf(&b, "Override: %s=%s\n", k, s.Overrides[k])
	}
	for _, k := range slices.Sorted(maps.Keys(s.SupervisorOverrides)) {
		fmt.Fprintf(&b, "Supervisor override: %s=%s\n", k, s.SupervisorOverrides[k])
	}
	if s.Stopped != "" {
		fmt.Fprintf(&b, "Stopped: %s: %s\n", s.Stopped, s.StopReason)
	}
	fmt.Fprintf(&b, "Runs: %d. Engine memory limit %d bytes, GOMEMLIMIT of gogc-off %d bytes\n", s.Runs, s.Engine.LimitBytes, s.Engine.GoMemLimitBytes)
	clock := "fit"
	switch {
	case s.EngineClockReadNS == 0 || s.ClientClockReadNS == 0:
		clock = "unknown"
	case s.ClockUnfit:
		clock = "UNFIT"
	}
	fmt.Fprintf(&b, "Clock read, mean per call: engine %s, client %s, unfit above %d ns: %s\n\n",
		clockText(s.EngineClockReadNS), clockText(s.ClientClockReadNS), maxClockReadNS, clock)

	g := s.G2
	verdict := "not passed"
	if g.Passed {
		verdict = "passed"
	}
	complete := "incomplete"
	if g.Complete {
		complete = "complete"
	}
	fmt.Fprintf(&b, "G2 %s, matrix %s\n", verdict, complete)
	fmt.Fprintf(&b, "  1. at least %d of %d runs valid in every configuration: %s\n", g.MinValid, g.Rounds, g.Cond1.Status)
	for _, n := range g.Cond1.Configs {
		if n.Status != statusPass {
			fmt.Fprintf(&b, "       %s: %s, %d of %d runs valid\n", n.Config, n.Status, n.Valid, n.Runs)
		}
	}
	fmt.Fprintf(&b, "  2. R1 p99 of the reply row, largest over smallest at most %g: %s%s\n", *g.Cond2.Limit, g.Cond2.Status, number(" (%.3g)", g.Cond2.Ratio))
	fmt.Fprintf(&b, "  3. R1 p50 of the reply row, median, at most %g us over the floor's: %s%s\n", *g.Cond3.Limit, g.Cond3.Status, number(" (%.1f us)", g.Cond3.Difference))
	if g.EarlyStop {
		fmt.Fprintf(&b, "  early stop: %s\n", g.EarlyStopReason)
	}

	b.WriteString("\nTuning against R1, by p99 of the reply row: gain over range, in us\n")
	for _, t := range s.Tuning {
		if t.Adopted {
			fmt.Fprintf(&b, "  %s: adopted, gain %.1f above range %.1f\n", t.Variant, *t.Gain, *t.Range)
		} else {
			fmt.Fprintf(&b, "  %s: not adopted, %s\n", t.Variant, t.Reason)
		}
	}

	b.WriteString("\nLatency in us, min/median/max over the valid runs; every configuration with its pair's floor\n")
	b.WriteString("The median of an even number of valid runs is the mean of the two middle ones, so it may be the number of no run.\n")
	for _, c := range s.Configs {
		fmt.Fprintf(&b, "\n%s: %s, engine %s, %g/s, %d of %d runs valid\n", c.Name, c.Flow, c.Engine, c.Rate, c.Valid, c.Runs)
		if c.Floor != nil {
			writeRow(&b, "floor (F pong)", *c.Floor)
		} else {
			b.WriteString("  floor (F pong): no valid F run\n")
		}
		for _, row := range sortRows(slices.Collect(maps.Keys(c.Rows))) {
			writeRow(&b, row, c.Rows[row])
		}
	}
	return b.String()
}

// clockText writes a cost of a clock read, or says that it was not read: a
// supervisor that never answered has no number.
func clockText(ns float64) string {
	if ns == 0 {
		return "not read"
	}
	return fmt.Sprintf("%.1f ns", ns)
}

// number formats v with format, or gives "" if v is nil.
func number(format string, v *float64) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf(format, *v)
}

func writeRow(b *strings.Builder, name string, r rowStats) {
	fmt.Fprintf(b, "  %-16s p50 %s  p99 %s  p99.9 %s\n", name, r.P50.text(), r.P99.text(), r.P999.text())
}

func (t triple) text() string { return fmt.Sprintf("%.1f/%.1f/%.1f", t.Min, t.Median, t.Max) }
