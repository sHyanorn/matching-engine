package main

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

func allConfigs() []config {
	cs := planConfigs(defaultParams, "oe", "http")
	return append(cs, longRun(defaultParams, "oe", "http"))
}

func summaryOf(records []runRecord, overrides map[string]string) summaryFile {
	return buildSummary(summaryInput{
		Params: defaultParams, Overrides: overrides, Configs: allConfigs(), Records: records,
		G2:     computeG2(planConfigs(defaultParams, "oe", "http"), records, 5, true),
		Memory: engineMemory{LimitBytes: 16 << 30, GoMemLimitBytes: 8 << 30},
	})
}

func summaryConfig(t *testing.T, s summaryFile, name string) configSummary {
	t.Helper()
	for _, c := range s.Configs {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no configuration %s in the summary", name)
	return configSummary{}
}

func TestBuildSummaryKnownInputs(t *testing.T) {
	floorP50 := []float64{56.3, 58.6, 64.2, 56.5, 60.2}
	floorP99 := []float64{65.2, 71.3, 76.0, 67.0, 71.4}
	r1P50 := []float64{70.5, 72.25, 99.5, 71, 500} // the last run is invalid
	r1P99 := []float64{90, 110, 100, 130, 9999}
	f := func(c string, round int) result {
		switch c {
		case "F":
			return result{true, floorP50[round-1], floorP99[round-1]}
		case "R1":
			return result{round != 5, r1P50[round-1], r1P99[round-1]}
		}
		return good(c, round)
	}
	s := summaryOf(matrix(5, f), nil)

	pong := summaryConfig(t, s, "F").Rows["pong"]
	want := rowStats{
		P50:  triple{56.3, 58.6, 64.2},
		P99:  triple{65.2, 71.3, 76.0},
		P999: triple{floorP99[0] * 1.5, floorP99[1] * 1.5, floorP99[2] * 1.5}, // flowRows makes p99.9 of 1.5 times p99
	}
	if pong != want {
		t.Errorf("F pong %+v, want %+v", pong, want)
	}

	// Four valid runs: the median is the mean of the two in the middle, and the invalid run is left out.
	r1 := summaryConfig(t, s, "R1")
	if r1.Runs != 5 || r1.Valid != 4 {
		t.Errorf("R1 has %d runs, %d valid, want 5 and 4", r1.Runs, r1.Valid)
	}
	reply := r1.Rows["reply"]
	if reply.P50 != (triple{70.5, (71 + 72.25) / 2, 99.5}) || reply.P99 != (triple{90, 105, 130}) {
		t.Errorf("R1 reply %+v", reply)
	}
}

func TestBuildSummaryRows(t *testing.T) {
	s := summaryOf(matrix(2, good), nil)
	for name, rows := range map[string][]string{
		"F":           {"hiccup", "pong", "send_lag"},
		"R1":          {"hiccup", "reply", "send_lag", "service"},
		"R2":          {"cancel", "hiccup", "order_to_cancel", "reply", "send_lag"},
		"R3-10k":      {"hiccup", "reply", "send_lag", "service"},
		"T-71k":       {"hiccup", "reply", "send_lag", "service", "trade_push"},
		"REST-10k":    {"hiccup", "reply", "send_lag", "service", "trade_push"},
		"R1-spin-pin": {"hiccup", "reply", "send_lag", "service"},
	} {
		got := map[string]bool{}
		for row := range summaryConfig(t, s, name).Rows {
			got[row] = true
		}
		want := map[string]bool{}
		for _, row := range rows {
			want[row] = true
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s reports rows %v, want %v", name, got, want)
		}
	}
	if long := summaryConfig(t, s, "R1-15min"); long.Runs != 0 || long.Valid != 0 || len(long.Rows) != 0 {
		t.Errorf("a long run that did not run is %+v", long)
	}
}

func TestBuildSummaryRowsOfInvalidRunsAreLeftOut(t *testing.T) {
	records := matrix(2, good)
	records[1].Valid = false // R1, round 1
	records[1].Latency["only_in_invalid"] = row(1, 2)
	records[10].Latency["only_in_valid"] = row(1, 2) // R1, round 2
	got := summaryOf(records, nil)
	rows := summaryConfig(t, got, "R1").Rows
	if _, ok := rows["only_in_invalid"]; ok {
		t.Error("a row of an invalid run is in the summary")
	}
	if _, ok := rows["only_in_valid"]; !ok {
		t.Error("a row of a valid run is not in the summary")
	}
}

func TestBuildSummaryFloorOnEveryConfig(t *testing.T) {
	s := summaryOf(matrix(5, good), nil)
	floor := summaryConfig(t, s, "F").Rows["pong"]
	if len(s.Configs) != 10 {
		t.Fatalf("%d configurations, want the nine and the long run", len(s.Configs))
	}
	for _, c := range s.Configs {
		if c.Floor == nil || *c.Floor != floor {
			t.Errorf("%s has floor %+v, want F's pong %+v", c.Name, c.Floor, floor)
		}
	}
	text := s.text()
	if n := strings.Count(text, "floor (F pong)"); n != 10 {
		t.Errorf("summary.txt shows the floor %d times, want 10", n)
	}

	noFloor := summaryOf(matrix(5, with(func(c string, _ int) bool { return c == "F" }, result{false, 20, 100})), nil)
	for _, c := range noFloor.Configs {
		if c.Floor != nil {
			t.Errorf("%s has a floor though no F run was valid", c.Name)
		}
	}
	if !strings.Contains(noFloor.text(), "floor (F pong): no valid F run") {
		t.Error("summary.txt does not say that the floor is missing")
	}
	data, err := json.Marshal(noFloor.Configs[1])
	if err != nil || !strings.Contains(string(data), `"floor":null`) {
		t.Errorf("a missing floor is written as %s, %v, want null", data, err)
	}
}

func TestBuildSummaryTakesNumbersAsTheyAre(t *testing.T) {
	// One valid run of each configuration, with numbers a recomputation would change.
	odd := []float64{0.1 + 0.2, 1.0 / 3, 56.300000000000004, 1e-7, 123456789.123456789, math.Nextafter(1, 2), 2.5, 99.99999999999999, 5e5}
	var records []runRecord
	for i, c := range planConfigs(defaultParams, "oe", "http") {
		r := validReport(c.Flow, odd[i], odd[(i+1)%len(odd)])
		rep := decodeReport(t, r.JSON())
		records = append(records, runRecord{Config: c.Name, Flow: c.Flow, Valid: true, Latency: rep.Latency})
	}
	s := summaryOf(records, nil)
	for _, rec := range records {
		for name, want := range rec.Latency {
			got := summaryConfig(t, s, rec.Config).Rows[name]
			for _, p := range []struct {
				stat triple
				v    float64
			}{{got.P50, want.P50}, {got.P99, want.P99}, {got.P999, want.P999}} {
				for _, x := range []float64{p.stat.Min, p.stat.Median, p.stat.Max} {
					if math.Float64bits(x) != math.Float64bits(p.v) {
						t.Errorf("%s %s: %v is not the report's %v", rec.Config, name, x, p.v)
					}
				}
			}
		}
	}
}

func TestBuildSummaryHeader(t *testing.T) {
	records := matrix(5, good)
	plain := summaryOf(records, nil)
	if plain.Rehearsal || !plain.Publishable || strings.HasPrefix(plain.text(), "REHEARSAL") {
		t.Errorf("a run with no overrides is marked: rehearsal %v, publishable %v", plain.Rehearsal, plain.Publishable)
	}
	if plain.Runs != 45 || plain.Engine.LimitBytes != 16<<30 || plain.Engine.GoMemLimitBytes != 8<<30 {
		t.Errorf("runs %d, engine %+v", plain.Runs, plain.Engine)
	}
	if !strings.Contains(plain.text(), "17179869184 bytes") || !strings.Contains(plain.text(), "8589934592 bytes") {
		t.Error("summary.txt does not give the engine's memory limit and GOMEMLIMIT")
	}

	rehearsal := summaryOf(records, map[string]string{"PILOT_ROUNDS": "2", "PILOT_DURATION": "2s"})
	if !rehearsal.Rehearsal || rehearsal.Publishable {
		t.Errorf("a rehearsal is marked: rehearsal %v, publishable %v", rehearsal.Rehearsal, rehearsal.Publishable)
	}
	text := rehearsal.text()
	if first, _, _ := strings.Cut(text, "\n"); first != "REHEARSAL: not publishable" {
		t.Errorf("first line of summary.txt is %q", first)
	}
	if !strings.Contains(text, "Override: PILOT_DURATION=2s\nOverride: PILOT_ROUNDS=2\n") {
		t.Errorf("summary.txt does not list the overrides in order:\n%s", text)
	}
	data, err := json.Marshal(rehearsal)
	if err != nil || !strings.Contains(string(data), `"rehearsal":true,"publishable":false`) {
		t.Errorf("summary.json %s, %v", data, err)
	}
}

func TestBuildSummaryStopAndG2(t *testing.T) {
	records := matrix(3, good)
	s := buildSummary(summaryInput{
		Params: defaultParams, Configs: allConfigs(), Records: records,
		G2:      computeG2(planConfigs(defaultParams, "oe", "http"), records, 5, true),
		Stopped: "deadline", StopReason: "no time for a run",
	})
	if s.Stopped != "deadline" || !strings.Contains(s.text(), "Stopped: deadline: no time for a run") {
		t.Errorf("stopped %q, text %q", s.Stopped, s.text())
	}
	if s.G2.Complete || s.G2.Passed || !strings.Contains(s.text(), "G2 not passed, matrix incomplete") {
		t.Errorf("G2 of a stopped matrix: %+v", s.G2)
	}
	if len(s.Tuning) != 2 || s.Tuning[0].Variant != "R1-gogc-off" || s.Tuning[1].Variant != "R1-spin-pin" {
		t.Errorf("tuning %+v", s.Tuning)
	}
}

func TestSummaryJSONIsStable(t *testing.T) {
	s := summaryOf(matrix(5, good), nil)
	a, errA := json.MarshalIndent(s, "", "  ")
	b, errB := json.MarshalIndent(s, "", "  ")
	if errA != nil || errB != nil || string(a) != string(b) {
		t.Fatalf("two encodings differ: %v, %v", errA, errB)
	}
	var back summaryFile
	if err := json.Unmarshal(a, &back); err != nil || !reflect.DeepEqual(back.Configs, s.Configs) {
		t.Errorf("summary.json does not read back: %v", err)
	}
}

func TestSortRows(t *testing.T) {
	got := sortRows([]string{"hiccup", "zzz", "reply", "aaa", "trade_push", "send_lag", "pong", "service", "cancel", "order_to_cancel"})
	want := []string{"trade_push", "pong", "reply", "cancel", "order_to_cancel", "service", "send_lag", "hiccup", "aaa", "zzz"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rows in the order %v, want %v", got, want)
	}
}
