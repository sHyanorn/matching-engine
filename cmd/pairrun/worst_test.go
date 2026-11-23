package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// seriesOf is the series of a loadgen report: the main row, named row, and the
// send lag, with a p99.9 of twice and a max of three times each p99 so that a
// value taken from the wrong column or the wrong window shows.
func seriesOf(t *testing.T, start string, windowS float64, row string, n []int, p99 []float64) json.RawMessage {
	t.Helper()
	cols := func() map[string][]float64 {
		c := map[string][]float64{"p50": {}, "p99": {}, "p99_9": {}, "max": {}}
		for _, v := range p99 {
			c["p50"], c["p99"], c["p99_9"], c["max"] = append(c["p50"], v/2), append(c["p99"], v), append(c["p99_9"], 2*v), append(c["max"], 3*v)
		}
		return c
	}
	series := map[string]any{"start": start, "n": n, "rows": map[string]any{row: cols(), "send_lag": cols()}}
	if windowS != 0 {
		series["window_s"] = windowS
	}
	data, err := json.Marshal(series)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// worstOfSeries decodes raw as a client would and returns its worst window.
func worstOfSeries(t *testing.T, raw json.RawMessage) (*worstSecond, error) {
	t.Helper()
	ser, err := decodeSeries(&loadgenReport{Series: raw})
	if err != nil || ser == nil {
		t.Fatalf("series %s does not decode: %v", raw, err)
	}
	return worstWindow(*ser, raw)
}

func TestWorstWindowIsTheOneWithTheLargestP99(t *testing.T) {
	raw := seriesOf(t, "2026-11-23T05:21:00Z", 1, "reply", []int{100, 200, 300, 400}, []float64{50, 90, 70, 30})
	w, err := worstOfSeries(t, raw)
	if err != nil {
		t.Fatal(err)
	}
	want := &worstSecond{Window: 1, OffsetS: 1, Time: "2026-11-23T05:21:01Z", N: 200, P99: 90, P999: 180, Max: 270, MedianP99: 60}
	if !reflect.DeepEqual(w, want) {
		t.Errorf("worst %+v, want %+v", w, want)
	}
}

func TestWorstWindowOfATieIsTheFirst(t *testing.T) {
	raw := seriesOf(t, "2026-11-23T05:21:00Z", 1, "reply", []int{10, 10, 10, 10, 10}, []float64{5, 9, 9, 9, 2})
	w, err := worstOfSeries(t, raw)
	if err != nil {
		t.Fatal(err)
	}
	if w.Window != 1 || w.OffsetS != 1 {
		t.Errorf("window %d at %vs, want the first of the three that tie, 1 at 1s", w.Window, w.OffsetS)
	}
}

// A window without samples has 0 in every column, but a series that was cut or
// made by hand could hold anything in it: it is neither the worst nor part of
// the scale.
func TestWorstWindowSkipsAnEmptyWindow(t *testing.T) {
	raw := seriesOf(t, "2026-11-23T05:21:00Z", 1, "reply", []int{10, 0, 10}, []float64{5, 99, 7})
	w, err := worstOfSeries(t, raw)
	if err != nil {
		t.Fatal(err)
	}
	if w.Window != 2 || w.P99 != 7 {
		t.Errorf("worst window %d with p99 %v, want window 2 with 7: window 1 has no sample", w.Window, w.P99)
	}
	if w.MedianP99 != 6 {
		t.Errorf("median p99 %v, want 6, the mean of 5 and 7: the empty window is not one of the windows", w.MedianP99)
	}
}

func TestWorstWindowMedianOfAnOddAndAnEvenCount(t *testing.T) {
	for _, c := range []struct {
		name   string
		p99    []float64
		window int
		median float64
	}{
		{"odd", []float64{4, 10, 6}, 1, 6},
		{"even", []float64{4, 10, 6, 20}, 3, 8},
		{"one window", []float64{4}, 0, 4},
		{"even, equal middle", []float64{1, 5, 5, 9}, 3, 5},
	} {
		t.Run(c.name, func(t *testing.T) {
			n := make([]int, len(c.p99))
			for i := range n {
				n[i] = 10
			}
			w, err := worstOfSeries(t, seriesOf(t, "2026-11-23T05:21:00Z", 1, "reply", n, c.p99))
			if err != nil {
				t.Fatal(err)
			}
			if w.Window != c.window || w.MedianP99 != c.median {
				t.Errorf("window %d, median %v, want %d and %v", w.Window, w.MedianP99, c.window, c.median)
			}
		})
	}
}

// The window's start is the series' start plus the window's index, on the
// clock the report gave it, written in UTC whatever zone the report wrote.
func TestWorstWindowTimeIsUTC(t *testing.T) {
	n := make([]int, 40)
	p99 := make([]float64, 40)
	for i := range n {
		n[i], p99[i] = 10, 1
	}
	p99[37] = 7
	raw := seriesOf(t, "2026-11-23T07:21:14.25+02:00", 1, "reply", n, p99)
	w, err := worstOfSeries(t, raw)
	if err != nil {
		t.Fatal(err)
	}
	if w.Window != 37 || w.OffsetS != 37 || w.Time != "2026-11-23T05:21:51.25Z" {
		t.Errorf("window %d at %vs, time %q, want 37 at 37s, 2026-11-23T05:21:51.25Z", w.Window, w.OffsetS, w.Time)
	}
}

func TestWorstWindowFollowsTheWindowWidth(t *testing.T) {
	for _, c := range []struct {
		name    string
		windowS float64
		offset  float64
		time    string
	}{
		{"half a second", 0.5, 1.5, "2026-11-23T05:21:01.5Z"},
		{"two seconds", 2, 6, "2026-11-23T05:21:06Z"},
		{"not given: a second", 0, 3, "2026-11-23T05:21:03Z"},
	} {
		t.Run(c.name, func(t *testing.T) {
			raw := seriesOf(t, "2026-11-23T05:21:00Z", c.windowS, "reply", []int{10, 10, 10, 10}, []float64{1, 2, 3, 9})
			w, err := worstOfSeries(t, raw)
			if err != nil {
				t.Fatal(err)
			}
			if w.Window != 3 || w.OffsetS != c.offset || w.Time != c.time {
				t.Errorf("window %d at %vs, time %q, want 3 at %vs, %s", w.Window, w.OffsetS, w.Time, c.offset, c.time)
			}
		})
	}
}

// F runs have a pong row where the others have a reply row. The window is the
// one of the largest p99, not of the largest p99.9 or max, which can differ.
func TestWorstWindowReadsThePongRowOfAPing(t *testing.T) {
	w, err := worstOfSeries(t, seriesOf(t, "2026-11-23T05:21:00Z", 1, "pong", []int{10, 10}, []float64{4, 8}))
	if err != nil || w.Window != 1 || w.P99 != 8 {
		t.Errorf("worst %+v, %v, want window 1 with p99 8", w, err)
	}
	raw := json.RawMessage(`{"start":"2026-11-23T05:21:00Z","n":[10,10,10],"rows":{` +
		`"pong":{"p99":[5,9,6],"p99_9":[50,60,70],"max":[500,600,700]},"reply":{"p99":[1,1,1],"p99_9":[1,1,1],"max":[1,1,1]}}}`)
	w, err = worstOfSeries(t, raw)
	want := &worstSecond{Window: 0, OffsetS: 0, Time: "2026-11-23T05:21:00Z", N: 10, P99: 1, P999: 1, Max: 1, MedianP99: 1}
	if err != nil || !reflect.DeepEqual(w, want) {
		t.Errorf("worst %+v, %v, want %+v: the reply row is the main row if a series has both, and all its windows tie", w, err, want)
	}
	raw = json.RawMessage(`{"start":"2026-11-23T05:21:00Z","n":[10,10,10],"rows":{"reply":{"p99":[5,9,6],"p99_9":[50,60,70],"max":[500,600,700]}}}`)
	w, err = worstOfSeries(t, raw)
	want = &worstSecond{Window: 1, OffsetS: 1, Time: "2026-11-23T05:21:01Z", N: 10, P99: 9, P999: 60, Max: 600, MedianP99: 6}
	if err != nil || !reflect.DeepEqual(w, want) {
		t.Errorf("worst %+v, %v, want %+v: window 1 has the largest p99 though window 2 has the largest p99.9 and max", w, err, want)
	}
}

func TestWorstWindowOfAnUnusableSeries(t *testing.T) {
	start := "2026-11-23T05:21:00Z"
	for name, raw := range map[string]json.RawMessage{
		"no main row":       seriesOf(t, start, 1, "service", []int{10, 10}, []float64{4, 8}),
		"short column":      json.RawMessage(`{"start":"` + start + `","n":[10,10],"rows":{"reply":{"p99":[1],"p99_9":[1,2],"max":[1,2]}}}`),
		"no sample at all":  seriesOf(t, start, 1, "reply", []int{0, 0}, []float64{0, 0}),
		"row of strings":    json.RawMessage(`{"start":"` + start + `","n":[10],"rows":{"reply":{"p99":["a"],"p99_9":[1],"max":[1]}}}`),
		"rows of no object": json.RawMessage(`{"start":"` + start + `","n":[10],"rows":[1]}`),
	} {
		t.Run(name, func(t *testing.T) {
			if w, err := worstOfSeries(t, raw); err == nil {
				t.Errorf("worst %+v, want an error", w)
			}
		})
	}
}

// What the wake meter saw is read over the seconds that overlap the worst
// window on the engine machine's clock, which is the offset ahead of the
// client's, and over no other second.
func TestWorstOfSetsTheWakeMeterAgainstTheWorstWindow(t *testing.T) {
	start := unixSecond(5000, 500*time.Millisecond)
	raw := seriesOf(t, start.UTC().Format(time.RFC3339Nano), 1, "reply", []int{10, 10, 10, 10}, []float64{5, 6, 90, 7})
	rep := &loadgenReport{Series: raw}
	// Second 5000+i has 100+i wakes, a p99 of 10(i+1) and a max of 1000+i.
	var wake []wakeRow
	for i := range 10 {
		wake = append(wake, wakeRow{Second: int64(5000 + i), N: 100 + i, P99: float64(10 * (i + 1)), Max: float64(1000 + i)})
	}
	// The engine machine's clock is 2 s ahead: window 2 starts at 5002.5 on the
	// client's and at 5004.5 on its own, and lasts into second 5005.
	off := &clockOffset{OffsetUS: 2e6, ErrorUS: 100}
	var rec runRecord
	w := worstOf(rep, wake, off, &rec)
	if w == nil || w.Window != 2 {
		t.Fatalf("worst %+v, want window 2", w)
	}
	want := &wakeSeen{N: 104 + 105, P99: 60, Max: 1005}
	if !reflect.DeepEqual(w.Wake, want) {
		t.Errorf("wake %+v, want %+v from seconds 5004 and 5005", w.Wake, want)
	}
	if len(rec.Notes) != 0 {
		t.Errorf("notes %v", rec.Notes)
	}

	// A run with no wake file has no wake; one whose file holds no second that
	// overlaps the window has a wake that saw nothing, which is not the same.
	if w := worstOf(rep, nil, nil, &rec); w == nil || w.Wake != nil {
		t.Errorf("without a wake file: worst %+v, want a null wake", w)
	}
	if w := worstOf(rep, []wakeRow{}, off, &rec); w == nil || !reflect.DeepEqual(w.Wake, &wakeSeen{}) {
		t.Errorf("with a wake file of no second: worst %+v, want a wake of n 0", w)
	}
}

func TestWorstOfNeedsASeries(t *testing.T) {
	good := seriesOf(t, "2026-11-23T05:21:00Z", 1, "reply", []int{10}, []float64{4})
	for _, c := range []struct {
		name  string
		rep   *loadgenReport
		notes int
	}{
		{"no report", nil, 0},
		{"no series", &loadgenReport{}, 0},
		{"a null series", &loadgenReport{Series: json.RawMessage("null")}, 0},
		{"a series with no windows, which the wake has already noted", &loadgenReport{Series: json.RawMessage(`{"start":"2026-11-23T05:21:00Z","n":[]}`)}, 0},
		{"a series that is not an object, noted by the wake", &loadgenReport{Series: json.RawMessage(`[1]`)}, 0},
		{"a series with no main row", &loadgenReport{Series: json.RawMessage(`{"start":"2026-11-23T05:21:00Z","n":[10]}`)}, 1},
		{"a good series", &loadgenReport{Series: good}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			var rec runRecord
			w := worstOf(c.rep, nil, nil, &rec)
			if want := c.name == "a good series"; (w != nil) != want {
				t.Errorf("worst %+v, want one: %v", w, want)
			}
			if len(rec.Notes) != c.notes || c.notes == 1 && !strings.HasPrefix(rec.Notes[0], "worst: ") {
				t.Errorf("notes %q, want %d", rec.Notes, c.notes)
			}
		})
	}
}
