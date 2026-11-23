package main

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// runDetail is a run in summary.json's run_details: what places a raised tail
// in time and on a side, from the run's record. Only what a reader cannot take
// from the summary's other parts is here, and nothing of the series or of the
// wake meter's seconds: the log copy has room for the whole pilot's details,
// and runs.jsonl has the rest.
type runDetail struct {
	Seq         int          `json:"seq"`
	Config      string       `json:"config"`
	Round       int          `json:"round"`
	Valid       bool         `json:"valid"`
	Worst       *worstSecond `json:"worst"`
	ClockOffset *clockOffset `json:"clock_offset"`
	EngineDelta *deltaDetail `json:"engine_delta"` // nil if no engine was started
	ClientDelta *deltaDetail `json:"client_delta"`
}

// deltaDetail is a machine's counter differences over a run, without the
// reasons that some could not be read and without the settings: runs.jsonl
// has both.
type deltaDetail struct {
	Values          map[string]int64 `json:"values,omitempty"`
	UnreadableNames []string         `json:"unreadable_names,omitempty"` // "*" if the machine's reading failed as a whole
	Partial         []string         `json:"partial,omitempty"`          // the counters whose difference is a lower bound
}

// detailsOf makes the details of the records, in run order.
func detailsOf(records []runRecord) []runDetail {
	out := make([]runDetail, 0, len(records))
	for _, r := range records {
		out = append(out, runDetail{
			Seq: r.Seq, Config: r.Config, Round: r.Round, Valid: r.Valid, Worst: r.Worst, ClockOffset: r.ClockOffset,
			EngineDelta: detailOf(r.EngineDelta), ClientDelta: detailOf(r.ClientDelta),
		})
	}
	return out
}

func detailOf(d *deltaResult) *deltaDetail {
	if d == nil {
		return nil
	}
	names := map[string]bool{}
	for _, m := range []map[string]string{d.Unreadable, d.Settings.Unreadable} {
		for name := range m {
			names[name] = true
		}
	}
	out := &deltaDetail{Values: maps.Clone(d.Values), Partial: slices.Clone(d.Partial)}
	if len(names) > 0 {
		out.UnreadableNames = slices.Sorted(maps.Keys(names))
	}
	return out
}

// counterFamilies are the CPU counters that a run is judged by, as the counter
// line lists them. A family is read if every counter of it is.
var counterFamilies = []struct {
	name     string
	counters []string
}{
	{"sched", []string{schedRunNS, schedDelayNS}},
	{"throttle", []string{cpuNrPeriods, cpuNrThrottled}},
	{"quota", []string{cpuQuotaUS, cpuPeriodUS}},
	{"psi", []string{psiCPUSomeUS}},
	{"steal", []string{stealTicks}},
}

// families sorts the counter families of a machine into those that its runs
// could read, with the source of psi if it is known, and those that some of its
// counters no run could read. A family that a machine's runs never tried to
// read, as the client does not read the scheduler's, is in neither.
func families(n namesRead, psi string) (read, never []string) {
	if slices.Contains(n.Unreadable, allUnreadable) {
		return nil, []string{"all"}
	}
	for _, f := range counterFamilies {
		allRead, someNever := true, false
		for _, name := range f.counters {
			allRead = allRead && slices.Contains(n.Readable, name)
			someNever = someNever || slices.Contains(n.Unreadable, name)
		}
		switch {
		case allRead && f.name == "psi" && psi != "":
			read = append(read, f.name+"("+psi+")")
		case allRead:
			read = append(read, f.name)
		case someNever:
			never = append(never, f.name)
		}
	}
	return read, never
}

// counterLine is the counter line of summary.txt: for each machine, the
// families that could be read and those that never could, so that a judgment
// that cannot be made shows at once. psi says where each machine's pressure
// came from.
func counterLine(c countersRead, psi map[string]string) string {
	list := func(names []string) string {
		if len(names) == 0 {
			return "none"
		}
		return strings.Join(names, " ")
	}
	engineRead, engineNever := families(c.Engine, psi["engine"])
	clientRead, clientNever := families(c.Client, psi["client"])
	return fmt.Sprintf("Counters read, engine: %s; client: %s; never readable: engine %s, client %s",
		list(engineRead), list(clientRead), list(engineNever), list(clientNever))
}

// psiSources tells, for each machine whose runs could read the CPU pressure,
// where they got it: "cgroup", "proc", or both joined by "+" if the runs
// differ. A machine none of whose runs could read it is not in the map.
func psiSources(records []runRecord) map[string]string {
	sources := map[string]map[string]bool{"engine": {}, "client": {}}
	for _, r := range records {
		for machine, d := range map[string]*deltaResult{"engine": r.EngineDelta, "client": r.ClientDelta} {
			if d == nil {
				continue
			}
			if _, ok := d.Values[psiCPUSomeUS]; ok && d.Settings.Source[psiSource] != "" {
				sources[machine][d.Settings.Source[psiSource]] = true
			}
		}
	}
	out := map[string]string{}
	for machine, set := range sources {
		if len(set) > 0 {
			out[machine] = strings.Join(slices.Sorted(maps.Keys(set)), "+")
		}
	}
	return out
}

// perRunHeader says what the columns of the per-run lines are.
const perRunHeader = `Per run: number, configuration, round, validity; worst second of the main row; wake meter and clock of the engine machine; CPU counters
  worst: start after the measured part began and in UTC on the client's clock, samples, p99, p99.9 and max in us, and the median second's p99
  wake: the meter's largest p99 and max over that second. clock: the engine machine's clock minus the client's, with its error bound
  engine and client: whole run, warmup included: scheduler delay over run time, throttled periods over periods, psi in us, steal in ticks
  n/r: could not be read. partial: the engine's threads fell in number, so its share is a lower bound. A run with no series has no worst, wake or clock
`

// notRead stands for a value that could not be read.
const notRead = "n/r"

// text is the run's line in summary.txt.
func (d runDetail) text() string {
	validity := "valid"
	if !d.Valid {
		validity = "INVALID"
	}
	return fmt.Sprintf("%02d %-12s r%d %-7s  worst %s  wake %s  clock %s  engine %s  client %s",
		d.Seq, d.Config, d.Round, validity, d.Worst.text(), d.Worst.wakeText(), d.ClockOffset.text(),
		d.EngineDelta.text(true), d.ClientDelta.text(false))
}

// text is the worst second as the summary shows it, or "-" if the run has none.
func (w *worstSecond) text() string {
	if w == nil {
		return "-"
	}
	at := w.Time
	if t, err := time.Parse(time.RFC3339Nano, w.Time); err == nil {
		at = t.UTC().Format("15:04:05Z")
	}
	return fmt.Sprintf("+%gs %s n %d p99 %.1f (median second %.1f) p99.9 %.1f max %.1f", w.OffsetS, at, w.N, w.P99, w.MedianP99, w.P999, w.Max)
}

// wakeText is what the wake meter saw at the worst second. A wake file with no
// wake in that second says so, as a stalled meter would: a zero is not shown.
func (w *worstSecond) wakeText() string {
	switch {
	case w == nil || w.Wake == nil:
		return "-"
	case w.Wake.N == 0:
		return "n 0"
	}
	return fmt.Sprintf("p99 %.1fms max %.1fms", w.Wake.P99/1000, w.Wake.Max/1000)
}

// text is the clock offset in ms, or "-" if the run has none.
func (o *clockOffset) text() string {
	if o == nil {
		return "-"
	}
	offset := fmt.Sprintf("%+.1f", o.OffsetUS/1000)
	if offset == "-0.0" {
		offset = "+0.0"
	}
	return fmt.Sprintf("%sms±%.1fms", offset, o.ErrorUS/1000)
}

// text is a machine's host differences: the scheduler's delay as a share of run
// time if sched is set, throttled periods over periods, psi in us and steal in
// ticks. A difference that could not be read is n/r.
func (d *deltaDetail) text(sched bool) string {
	value := func(name string) (int64, bool) {
		if d == nil {
			return 0, false
		}
		v, ok := d.Values[name]
		return v, ok
	}
	show := func(name, unit string) string {
		if v, ok := value(name); ok {
			return fmt.Sprintf("%d%s", v, unit)
		}
		return notRead
	}
	throttled := notRead
	if n, ok := value(cpuNrThrottled); ok {
		if periods, ok := value(cpuNrPeriods); ok {
			throttled = fmt.Sprintf("%d/%d", n, periods)
		}
	}
	text := fmt.Sprintf("throttled %s psi %s steal %s", throttled, show(psiCPUSomeUS, "us"), show(stealTicks, ""))
	if !sched {
		return text
	}
	share := notRead
	run, okRun := value(schedRunNS)
	delay, okDelay := value(schedDelayNS)
	if okRun && okDelay && run > 0 {
		share = fmt.Sprintf("%.1f%%", 100*float64(delay)/float64(run))
		if slices.Contains(d.Partial, schedRunNS) || slices.Contains(d.Partial, schedDelayNS) {
			share += " partial"
		}
	}
	return "sched-delay " + share + " " + text
}
