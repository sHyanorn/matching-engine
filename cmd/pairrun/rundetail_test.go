package main

import (
	"encoding/json"
	"maps"
	"math"
	"reflect"
	"strings"
	"testing"
)

// fullDelta is the difference of a machine's counters over a run, all read:
// 6 of 1200 periods throttled, a pressure of 5300 us from source, 2 ticks of
// steal, and for the engine (sched set) a scheduler delay of 0.3% of its run
// time over 12 threads.
func fullDelta(sched bool, source string) *deltaResult {
	d := &deltaResult{
		Values: map[string]int64{
			cpuNrPeriods: 1200, cpuNrThrottled: 6, cpuThrottledUS: 4500, psiCPUSomeUS: 5300, stealTicks: 2,
		},
		Settings: counters{
			Values: map[string]int64{cpuQuotaUS: 400000, cpuPeriodUS: 100000},
			Source: map[string]string{psiSource: source, cpuStatSource: cgroupV2, cpuLimitSource: cgroupV2},
		},
	}
	if sched {
		d.Values[schedRunNS], d.Values[schedDelayNS], d.Values[schedSlices], d.Values[schedTasks] = 10_000_000_000, 30_000_000, 52000, 0
	}
	return d
}

// without is d with the named counters unreadable, each for the reason why.
func without(d *deltaResult, why string, names ...string) *deltaResult {
	out := &deltaResult{Values: maps.Clone(d.Values), Unreadable: map[string]string{}, Partial: d.Partial, Settings: counters{
		Values: maps.Clone(d.Settings.Values), Unreadable: map[string]string{}, Source: d.Settings.Source,
	}}
	for _, name := range names {
		delete(out.Values, name)
		delete(out.Settings.Values, name)
		if name == cpuQuotaUS || name == cpuPeriodUS {
			out.Settings.Unreadable[name] = why
		} else {
			out.Unreadable[name] = why
		}
	}
	return out
}

// counterLineOf is the counter line of the summary of records.
func counterLineOf(t *testing.T, records []runRecord) string {
	t.Helper()
	for line := range strings.Lines(summaryOf(records, nil).text()) {
		if strings.HasPrefix(line, "Counters read") {
			return strings.TrimSuffix(line, "\n")
		}
	}
	t.Fatal("summary.txt has no counter line")
	return ""
}

func TestSummaryCounterLine(t *testing.T) {
	const why = "no such file"
	run := func(engine, client *deltaResult) runRecord {
		return runRecord{EngineDelta: engine, ClientDelta: client}
	}
	for _, c := range []struct {
		name    string
		records []runRecord
		want    string
	}{
		{
			"all readable",
			[]runRecord{run(fullDelta(true, "cgroup"), fullDelta(false, "cgroup"))},
			"Counters read, engine: sched throttle quota psi(cgroup) steal; client: throttle quota psi(cgroup) steal; never readable: engine none, client none",
		},
		{
			"steal unreadable on the client",
			[]runRecord{run(fullDelta(true, "cgroup"), without(fullDelta(false, "cgroup"), why, stealTicks))},
			"Counters read, engine: sched throttle quota psi(cgroup) steal; client: throttle quota psi(cgroup); never readable: engine none, client steal",
		},
		{
			"pressure unreadable on both",
			[]runRecord{run(without(fullDelta(true, "cgroup"), why, psiCPUSomeUS), without(fullDelta(false, "cgroup"), why, psiCPUSomeUS))},
			"Counters read, engine: sched throttle quota steal; client: throttle quota steal; never readable: engine psi, client psi",
		},
		{
			"pressure from the cgroup on one machine and from the machine on the other",
			[]runRecord{run(fullDelta(true, "proc"), fullDelta(false, "cgroup"))},
			"Counters read, engine: sched throttle quota psi(proc) steal; client: throttle quota psi(cgroup) steal; never readable: engine none, client none",
		},
		{
			"pressure from the cgroup in one run and from the machine in another",
			[]runRecord{run(fullDelta(true, "cgroup"), fullDelta(false, "proc")), run(fullDelta(true, "proc"), fullDelta(false, "proc"))},
			"Counters read, engine: sched throttle quota psi(cgroup+proc) steal; client: throttle quota psi(proc) steal; never readable: engine none, client none",
		},
		{
			"read in one run, not in the other, is read",
			[]runRecord{run(without(fullDelta(true, "cgroup"), why, schedRunNS, schedDelayNS), fullDelta(false, "cgroup")), run(fullDelta(true, "cgroup"), fullDelta(false, "cgroup"))},
			"Counters read, engine: sched throttle quota psi(cgroup) steal; client: throttle quota psi(cgroup) steal; never readable: engine none, client none",
		},
		{
			"scheduler statistics and quota unreadable",
			[]runRecord{run(without(fullDelta(true, "cgroup"), why, schedRunNS, schedDelayNS, schedSlices, cpuQuotaUS, cpuPeriodUS), fullDelta(false, "cgroup"))},
			"Counters read, engine: throttle psi(cgroup) steal; client: throttle quota psi(cgroup) steal; never readable: engine sched quota, client none",
		},
		{
			"a family with one counter that was never read is not read",
			[]runRecord{run(without(fullDelta(true, "cgroup"), why, cpuNrPeriods), fullDelta(false, "cgroup"))},
			"Counters read, engine: sched quota psi(cgroup) steal; client: throttle quota psi(cgroup) steal; never readable: engine throttle, client none",
		},
		{
			"the engine machine's reading failed as a whole",
			[]runRecord{run(&deltaResult{Unreadable: map[string]string{allUnreadable: "404"}}, fullDelta(false, "cgroup"))},
			"Counters read, engine: none; client: throttle quota psi(cgroup) steal; never readable: engine all, client none",
		},
		{
			"no run",
			nil,
			"Counters read, engine: none; client: none; never readable: engine none, client none",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := counterLineOf(t, c.records); got != c.want {
				t.Errorf("counter line\n%s\nwant\n%s", got, c.want)
			}
		})
	}
}

// The counter line comes right after the title lines, before anything about the
// matrix, so that it is read first.
func TestSummaryCounterLineIsRightAfterTheTitleLines(t *testing.T) {
	records := matrix(1, good)
	for i := range records {
		records[i].EngineDelta, records[i].ClientDelta = fullDelta(true, "proc"), fullDelta(false, "proc")
	}
	for _, c := range []struct {
		name      string
		overrides map[string]string
		before    []string
	}{
		{"no override", nil, []string{"Pilot matrix: "}},
		{"overrides", map[string]string{"PILOT_ROUNDS": "1", "PILOT_DURATION": "2s"}, []string{"REHEARSAL: not publishable", "Pilot matrix: ", "Override: PILOT_DURATION=2s", "Override: PILOT_ROUNDS=1"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			lines := strings.Split(summaryOf(records, c.overrides).text(), "\n")
			for i, prefix := range c.before {
				if !strings.HasPrefix(lines[i], prefix) {
					t.Errorf("line %d is %q, want it to start with %q", i, lines[i], prefix)
				}
			}
			if line := lines[len(c.before)]; !strings.HasPrefix(line, "Counters read, engine: sched throttle quota psi(proc) steal; client: ") {
				t.Errorf("line %d is %q, want the counter line", len(c.before), line)
			}
			if line := lines[len(c.before)+1]; !strings.HasPrefix(line, "Runs: ") {
				t.Errorf("line %d is %q, want the line of runs", len(c.before)+1, line)
			}
		})
	}
}

// perRunLines are the lines of the Per run section of summary.txt, which is
// its last, and the lines of its header.
func perRunLines(t *testing.T, text string) (header, runs []string) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "Per run: ") {
			n := strings.Count(perRunHeader, "\n")
			return lines[i : i+n], lines[i+n:]
		}
	}
	t.Fatalf("summary.txt has no Per run section:\n%s", text)
	return nil, nil
}

// handMade are the records of one run each of the kinds that the Per run
// section tells apart, in run order.
func handMade() []runRecord {
	worst := func(window int, at string, n int, p99, p999, max, median float64, wake *wakeSeen) *worstSecond {
		return &worstSecond{Window: window, OffsetS: float64(window), Time: at, N: n, P99: p99, P999: p999, Max: max, MedianP99: median, Wake: wake}
	}
	partial := fullDelta(true, "cgroup")
	partial.Partial = []string{schedDelayNS, schedRunNS, schedSlices}
	return []runRecord{
		{ // a series and a wake
			Seq: 1, Config: "R1", Round: 1, Valid: true,
			Worst:       worst(37, "2026-11-23T05:21:14Z", 10000, 317.4, 912.5, 1410.2, 80.1, &wakeSeen{N: 900, P99: 1200, Max: 4100}),
			ClockOffset: &clockOffset{OffsetUS: 1300, ErrorUS: 200}, EngineDelta: fullDelta(true, "cgroup"), ClientDelta: fullDelta(false, "cgroup"),
		},
		{ // a series, and no wake file
			Seq: 2, Config: "R1-gogc-off", Round: 1, Valid: true,
			Worst:       worst(5, "2026-11-23T05:21:14.75Z", 9000, 100, 150, 210.5, 80.1, nil),
			EngineDelta: fullDelta(true, "cgroup"), ClientDelta: fullDelta(false, "cgroup"),
		},
		{ // REST: no series
			Seq: 3, Config: "REST-10k", Round: 1, Valid: false, EngineDelta: fullDelta(true, "cgroup"), ClientDelta: fullDelta(false, "cgroup"),
		},
		{ // counters that could not be read, and a wake that saw nothing
			Seq: 4, Config: "F", Round: 2, Valid: true,
			Worst:       worst(0, "2026-11-23T05:22:01Z", 10000, 66.5, 99, 120, 66.5, &wakeSeen{}),
			ClockOffset: &clockOffset{OffsetUS: -1300, ErrorUS: 200},
			EngineDelta: without(fullDelta(true, "cgroup"), "no such file", schedRunNS, schedDelayNS, schedSlices, psiCPUSomeUS, stealTicks, cpuNrThrottled),
			ClientDelta: &deltaResult{Unreadable: map[string]string{allUnreadable: "404"}},
		},
		{ // threads of the engine ended: its scheduler counters are lower bounds
			Seq: 5, Config: "R2", Round: 2, Valid: true,
			Worst:       worst(12, "2026-11-23T05:23:30Z", 5000, 400, 800, 1600, 90, &wakeSeen{N: 450, P99: 100, Max: 400}),
			ClockOffset: &clockOffset{OffsetUS: -40, ErrorUS: 120}, EngineDelta: partial, ClientDelta: fullDelta(false, "cgroup"),
		},
		{ // an engine that did not start
			Seq: 6, Config: "T-71k", Round: 2, Valid: false,
		},
	}
}

func TestSummaryPerRunLines(t *testing.T) {
	text := summaryOf(handMade(), nil).text()
	header, runs := perRunLines(t, text)
	for _, want := range []string{"number, configuration, round, validity", "worst", "wake", "clock", "engine and client", "n/r: could not be read", "partial"} {
		if !strings.Contains(strings.Join(header, "\n"), want) {
			t.Errorf("the header of Per run does not say %q:\n%s", want, strings.Join(header, "\n"))
		}
	}
	host := "engine sched-delay 0.3% throttled 6/1200 psi 5300us steal 2  client throttled 6/1200 psi 5300us steal 2"
	want := []string{
		"01 R1           r1 valid    worst +37s 05:21:14Z n 10000 p99 317.4 (median second 80.1) p99.9 912.5 max 1410.2  wake p99 1.2ms max 4.1ms  clock +1.3ms±0.2ms  " + host,
		"02 R1-gogc-off  r1 valid    worst +5s 05:21:14Z n 9000 p99 100.0 (median second 80.1) p99.9 150.0 max 210.5  wake -  clock -  " + host,
		"03 REST-10k     r1 INVALID  worst -  wake -  clock -  " + host,
		"04 F            r2 valid    worst +0s 05:22:01Z n 10000 p99 66.5 (median second 66.5) p99.9 99.0 max 120.0  wake n 0  clock -1.3ms±0.2ms  " +
			"engine sched-delay n/r throttled n/r psi n/r steal n/r  client throttled n/r psi n/r steal n/r",
		"05 R2           r2 valid    worst +12s 05:23:30Z n 5000 p99 400.0 (median second 90.0) p99.9 800.0 max 1600.0  wake p99 0.1ms max 0.4ms  clock +0.0ms±0.1ms  " +
			"engine sched-delay 0.3% partial throttled 6/1200 psi 5300us steal 2  client throttled 6/1200 psi 5300us steal 2",
		"06 T-71k        r2 INVALID  worst -  wake -  clock -  " +
			"engine sched-delay n/r throttled n/r psi n/r steal n/r  client throttled n/r psi n/r steal n/r",
	}
	if len(runs) != len(want) {
		t.Fatalf("%d lines after the header, want %d:\n%s", len(runs), len(want), strings.Join(runs, "\n"))
	}
	for i := range want {
		if runs[i] != want[i] {
			t.Errorf("line %d\n%s\nwant\n%s", i+1, runs[i], want[i])
		}
	}
}

// A value that could not be read is n/r and not 0, in every place.
func TestSummaryPerRunUnreadableIsNeverZero(t *testing.T) {
	zero := &deltaResult{Values: map[string]int64{
		cpuNrPeriods: 0, cpuNrThrottled: 0, psiCPUSomeUS: 0, stealTicks: 0, schedRunNS: 1000, schedDelayNS: 0,
	}}
	_, runs := perRunLines(t, summaryOf([]runRecord{{Seq: 1, Config: "R1", Valid: true, EngineDelta: zero, ClientDelta: &deltaResult{}}}, nil).text())
	want := "01 R1           r0 valid    worst -  wake -  clock -  engine sched-delay 0.0% throttled 0/0 psi 0us steal 0  client throttled n/r psi n/r steal n/r"
	if len(runs) != 1 || runs[0] != want {
		t.Errorf("lines %q\nwant\n%s", runs, want)
	}
	// An engine that ran for no time has no share to give.
	idle := &deltaResult{Values: map[string]int64{schedRunNS: 0, schedDelayNS: 0}}
	_, runs = perRunLines(t, summaryOf([]runRecord{{Seq: 1, Config: "R1", EngineDelta: idle}}, nil).text())
	if len(runs) != 1 || !strings.Contains(runs[0], "engine sched-delay n/r ") {
		t.Errorf("an engine with no run time: %q, want its share n/r", runs)
	}
}

func TestSummaryHasNoPerRunSectionWithoutRuns(t *testing.T) {
	if text := summaryOf(nil, nil).text(); strings.Contains(text, "Per run") {
		t.Errorf("summary.txt of no run has a Per run section:\n%s", text)
	}
}

// buildSummary takes the numbers of the records as they are: the worst window
// and the clock offset are the record's own, bit for bit, and the details keep
// the order of the runs.
func TestRunDetailsAreTheRecordsOwn(t *testing.T) {
	records := handMade()
	records[0].Worst.P99 = 0.30000000000000004
	records[0].ClockOffset.OffsetUS = 1.0 / 3
	s := summaryOf(records, nil)
	if len(s.RunDetails) != len(records) {
		t.Fatalf("%d run details for %d records", len(s.RunDetails), len(records))
	}
	for i, d := range s.RunDetails {
		r := records[i]
		if d.Seq != r.Seq || d.Config != r.Config || d.Round != r.Round || d.Valid != r.Valid {
			t.Errorf("detail %d is run %d %s r%d valid %v, want the record's %d %s r%d %v", i, d.Seq, d.Config, d.Round, d.Valid, r.Seq, r.Config, r.Round, r.Valid)
		}
		if !reflect.DeepEqual(d.Worst, r.Worst) || !reflect.DeepEqual(d.ClockOffset, r.ClockOffset) {
			t.Errorf("detail %d: worst %+v, clock %+v, want the record's %+v and %+v", i, d.Worst, d.ClockOffset, r.Worst, r.ClockOffset)
		}
		if r.EngineDelta != nil && !reflect.DeepEqual(d.EngineDelta.Values, r.EngineDelta.Values) {
			t.Errorf("detail %d: engine values %v, want the record's %v", i, d.EngineDelta.Values, r.EngineDelta.Values)
		}
	}
	if math.Float64bits(s.RunDetails[0].Worst.P99) != math.Float64bits(0.30000000000000004) || s.RunDetails[0].ClockOffset.OffsetUS != 1.0/3 {
		t.Errorf("the numbers of the record changed: %+v, %+v", s.RunDetails[0].Worst, s.RunDetails[0].ClockOffset)
	}
	if data, err := json.Marshal(summaryOf(nil, nil)); err != nil || !strings.Contains(string(data), `"run_details":[]`) {
		t.Errorf("summary.json of no run: %s, %v, want an empty array of run details", data, err)
	}
}

// The details name what could not be read and leave out why: the reasons are in
// runs.jsonl, and the whole pilot's details must fit the log copy.
func TestRunDetailJSON(t *testing.T) {
	engine := without(fullDelta(true, "cgroup"), "open /proc/pressure/cpu: no such file or directory", psiCPUSomeUS, stealTicks, cpuQuotaUS, cpuPeriodUS)
	engine.Partial = []string{schedDelayNS}
	r := runRecord{
		Seq: 4, Config: "R1", Round: 2, Valid: true, EngineDelta: engine, ClientDelta: &deltaResult{Unreadable: map[string]string{allUnreadable: "before: 404"}},
		Worst: &worstSecond{Window: 2, OffsetS: 2, Time: "2026-11-23T05:21:14Z", N: 10, P99: 5, P999: 6, Max: 7, MedianP99: 4, Wake: &wakeSeen{N: 3, P99: 2, Max: 1}},
	}
	data, err := json.Marshal(detailsOf([]runRecord{r})[0])
	if err != nil {
		t.Fatal(err)
	}
	want := `{"seq":4,"config":"R1","round":2,"valid":true,` +
		`"worst":{"window":2,"offset_s":2,"time":"2026-11-23T05:21:14Z","n":10,"p99":5,"p99_9":6,"max":7,"median_p99":4,"wake":{"n":3,"p99":2,"max":1}},` +
		`"clock_offset":null,` +
		`"engine_delta":{"values":{"cpu_nr_periods":1200,"cpu_nr_throttled":6,"cpu_throttled_us":4500,"sched_delay_ns":30000000,"sched_run_ns":10000000000,"sched_slices":52000,"sched_tasks":0},` +
		`"unreadable_names":["cpu_period_us","cpu_quota_us","psi_cpu_some_us","steal_ticks"],"partial":["sched_delay_ns"]},` +
		`"client_delta":{"unreadable_names":["*"]}}`
	if string(data) != want {
		t.Errorf("run detail\n%s\nwant\n%s", data, want)
	}
	if strings.Contains(string(data), "no such file") || strings.Contains(string(data), "404") {
		t.Errorf("the details hold a reason: %s", data)
	}
	// A run with no counters has null, as the record has.
	data, _ = json.Marshal(detailsOf([]runRecord{{Seq: 1, Config: "F"}})[0])
	if want := `{"seq":1,"config":"F","round":0,"valid":false,"worst":null,"clock_offset":null,"engine_delta":null,"client_delta":null}`; string(data) != want {
		t.Errorf("run detail of a run with nothing\n%s\nwant\n%s", data, want)
	}
}

// maxRunDetailBytes bounds the details of one run, so that no field that the
// record already has creeps in: the whole pilot's results have to fit a log copy
// of 2500 lines of base64. The details' own fields take about 650 bytes of a run
// whose counters were all read. The log copy is compressed, so the names that
// every run repeats cost little there.
const maxRunDetailBytes = 700

// A typical run of the real pilot: a minute measured, the worst second well
// over the median, every counter read on both machines.
func TestRunDetailSize(t *testing.T) {
	engine := &deltaResult{Values: map[string]int64{
		cpuNrPeriods: 700, cpuNrThrottled: 2, cpuThrottledUS: 31245, psiCPUSomeUS: 254317, stealTicks: 12,
		schedRunNS: 50_123_456_789, schedDelayNS: 123_456_789, schedSlices: 456_789, schedTasks: 0,
	}}
	client := &deltaResult{Values: map[string]int64{cpuNrPeriods: 700, cpuNrThrottled: 0, cpuThrottledUS: 0, psiCPUSomeUS: 1_254_317, stealTicks: 3}}
	r := runRecord{
		Seq: 44, Config: "R1-spin-pin", Round: 5, Valid: true, EngineDelta: engine, ClientDelta: client,
		Worst: &worstSecond{
			Window: 37, OffsetS: 37, Time: "2026-11-23T05:21:14.123456789Z", N: 10000, P99: 317.4, P999: 912.5, Max: 1410.2, MedianP99: 80.1,
			Wake: &wakeSeen{N: 893, P99: 1200, Max: 4100},
		},
		ClockOffset: &clockOffset{OffsetUS: -117.328, ErrorUS: 220.327},
	}
	data, err := json.Marshal(detailsOf([]runRecord{r})[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > maxRunDetailBytes {
		t.Errorf("the details of a typical run take %d bytes, want at most %d: %s", len(data), maxRunDetailBytes, data)
	}
	t.Logf("%d bytes", len(data))
}
