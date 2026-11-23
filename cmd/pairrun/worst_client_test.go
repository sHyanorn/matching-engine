//go:build linux

package main

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// worstRig is a rig whose runs have a series of five windows that begin 20 s
// before the clock's first reading, whose worst window is the fourth, and whose
// supervisor's wake meter has a second for every one of them. The client's clock
// stands still, so the supervisor's, which reads 1.5 s ahead, is exactly 1.5 s
// ahead; second s has 1000+(s-base) wakes, a p99 of 100+(s-base) us and a max of
// twice that, where base is the first second of the clock.
func worstRig(t *testing.T, mutate func(cfg config, f fakeRun) fakeRun) (*rig, time.Time) {
	t.Helper()
	p := rounds(1)
	r := newRig(t, p, nil)
	r.clock.step = 0
	start := clockStart.Add(-20 * time.Second)
	writeMatrixScript(t, p, func(cfg config, _ int, f fakeRun) fakeRun {
		if mutate != nil {
			f = mutate(cfg, f)
		}
		if cfg.Flow == flowREST || f.NoReport || f.RawReport != "" {
			return f
		}
		row := "reply"
		if cfg.Flow == flowF {
			row = "pong"
		}
		raw := seriesOf(t, start.UTC().Format(time.RFC3339Nano), 1, row, []int{10000, 10000, 9000, 8000, 10000}, []float64{100, 110, 120, 900, 130})
		return editReport(f, func(c *cannedReport) { c.Series = raw })
	})
	base := clockStart.Unix()
	r.sup.wakeAnswer = func(_ int, from, to int64) (int, []byte) {
		a := wakeAnswer{NowUnixNS: clockStart.Add(1500 * time.Millisecond).UnixNano(), TickUS: 1000, N: []int{}, P99: []float64{}, Max: []float64{},
			Placement: wakePlacement{CPU: 3, Siblings: []int{3}, Cores: 4, TopologyReadable: true, Pinned: true}}
		for sec := (from + 999) / 1000; sec*1000 <= to; sec++ {
			if len(a.N) == 0 {
				a.T0 = sec
			}
			rel := float64(sec - base)
			a.N, a.P99, a.Max = append(a.N, 1000+int(rel)), append(a.P99, 100+rel), append(a.Max, 2*(100+rel))
		}
		data, err := json.Marshal(a)
		if err != nil {
			panic(err)
		}
		return http.StatusOK, data
	}
	return r, start
}

// The record names the worst window of each run that has a series, with the
// wake meter over it, and the REST run, which has none, has null.
func TestClientRecordsTheWorstSecondOfEachRun(t *testing.T) {
	r, start := worstRig(t, nil)
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	// Window 3 starts at base-17 s on the client's clock, base-15.5 s on the
	// supervisor's, and lasts into the second after: seconds base-16 and base-15.
	want := &worstSecond{
		Window: 3, OffsetS: 3, Time: start.Add(3 * time.Second).UTC().Format(time.RFC3339Nano), N: 8000, P99: 900, P999: 1800, Max: 2700,
		MedianP99: 120, Wake: &wakeSeen{N: (1000 - 16) + (1000 - 15), P99: 85, Max: 170},
	}
	records := r.records()
	if len(records) != 10 {
		t.Fatalf("%d records, want 10", len(records))
	}
	for _, rec := range records {
		if rec.Flow == flowREST {
			if rec.Worst != nil {
				t.Errorf("%s has no series but worst %+v", rec.Config, rec.Worst)
			}
			continue
		}
		if !reflect.DeepEqual(rec.Worst, want) {
			t.Errorf("%s: worst %+v (wake %+v), want %+v (wake %+v)", rec.Config, rec.Worst, rec.Worst.Wake, want, want.Wake)
		}
		if len(rec.Notes) != 0 {
			t.Errorf("%s: notes %q", rec.Config, rec.Notes)
		}
	}
	lines := strings.Split(strings.TrimSpace(string(r.file("runs.jsonl"))), "\n")
	if !strings.Contains(lines[0], `"worst":{"window":3,"offset_s":3,"time":"`) || !strings.Contains(lines[0], `,"wake":{"n":1969,"p99":85,"max":170}},`) {
		t.Errorf("the first line of runs.jsonl does not hold the worst window as specified: %s", lines[0])
	}
	if !strings.Contains(lines[8], `"worst":null,`) {
		t.Errorf("the REST line does not have a null worst: %s", lines[8])
	}
	// Summary.json holds the same, run for run.
	details := r.summary().RunDetails
	if len(details) != len(records) {
		t.Fatalf("%d run details for %d runs", len(details), len(records))
	}
	for i, d := range details {
		if d.Seq != records[i].Seq || d.Config != records[i].Config || !reflect.DeepEqual(d.Worst, records[i].Worst) || !reflect.DeepEqual(d.ClockOffset, records[i].ClockOffset) {
			t.Errorf("run detail %d is %+v, want the record's seq, config, worst and clock offset", i, d)
		}
	}
}

// A run without a wake file still has its worst window, with a null wake; a run
// whose report cannot be read has none. Neither changes the run's validity.
func TestClientWorstWithoutAWakeOrAReport(t *testing.T) {
	r, _ := worstRig(t, func(cfg config, f fakeRun) fakeRun {
		if cfg.Name == "R3-71k" {
			return fakeRun{RawReport: `{"valid": tru`, Exit: 1}
		}
		return f
	})
	r.sup.wakeAnswer = func(int, int64, int64) (int, []byte) { return http.StatusInternalServerError, []byte("no meter") }
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	for _, rec := range r.records() {
		switch {
		case rec.Flow == flowREST, rec.Config == "R3-71k":
			if rec.Worst != nil {
				t.Errorf("%s: worst %+v, want none", rec.Config, rec.Worst)
			}
		default:
			if rec.Worst == nil || rec.Worst.Window != 3 || rec.Worst.Wake != nil || rec.Wake != "" || rec.ClockOffset != nil {
				t.Errorf("%s: worst %+v, wake file %q, offset %+v, want window 3 with no wake", rec.Config, rec.Worst, rec.Wake, rec.ClockOffset)
			}
			if len(rec.Notes) != 1 || !strings.HasPrefix(rec.Notes[0], "wake: ") {
				t.Errorf("%s: notes %q, want the wake's alone", rec.Config, rec.Notes)
			}
			if !rec.Valid {
				t.Errorf("%s is invalid though only its wake is missing", rec.Config)
			}
		}
	}
}
