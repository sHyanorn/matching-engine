//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// wakeSeries is the series of a loadgen report whose measured part began at
// start and has the given number of windows, each with 10,000 samples and the
// same latencies.
func wakeSeries(start time.Time, windows int) map[string]any {
	n, flat := make([]int, windows), make([]float64, windows)
	for i := range n {
		n[i], flat[i] = 10000, 100
	}
	row := map[string]any{"p50": flat, "p99": flat, "p99_9": flat, "max": flat}
	return map[string]any{"window_s": 1, "start": start.UTC().Format(time.RFC3339Nano), "n": n, "rows": map[string]any{"reply": row, "send_lag": row}}
}

// scriptedNow returns a clock that gives the times in order, and fails the
// test if it is read more often.
func scriptedNow(t *testing.T, times ...time.Time) func() time.Time {
	t.Helper()
	i := 0
	return func() time.Time {
		if i == len(times) {
			t.Fatal("the clock was read more often than the test planned")
		}
		i++
		return times[i-1]
	}
}

// wakeReport is a report whose series starts at start and has the given
// windows, as keepWake reads it.
func wakeReport(t *testing.T, start time.Time, windows int) *loadgenReport {
	t.Helper()
	data, err := json.Marshal(wakeSeries(start, windows))
	if err != nil {
		t.Fatal(err)
	}
	return &loadgenReport{Series: data}
}

// wakeRig is a client with a fake supervisor and an empty runs directory, for
// calling keepWake on its own.
func wakeRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t, rounds(1), nil)
	if err := os.MkdirAll(filepath.Join(r.out, "runs"), 0o755); err != nil {
		t.Fatal(err)
	}
	return r
}

// answerAt makes the fake supervisor answer with its seconds and a clock that
// reads the nth of nows at the nth request, and the last one after them.
func (f *fakeSupervisor) answerAt(nows ...time.Time) {
	f.wakeAnswer = func(n int, from, to int64) (int, []byte) {
		var a wakeAnswer
		if err := json.Unmarshal(fakeWakeAnswer(from, to), &a); err != nil {
			panic(err)
		}
		a.NowUnixNS = nows[min(n, len(nows))-1].UnixNano()
		data, err := json.Marshal(a)
		if err != nil {
			panic(err)
		}
		return http.StatusOK, data
	}
}

func readWakeFile(t *testing.T, r *rig, name string) wakeFile {
	t.Helper()
	data := r.file(name)
	if !strings.HasSuffix(string(data), "}\n") || strings.Count(string(data), "\n") != 1 {
		t.Errorf("%s is not one line of JSON: %q", name, data)
	}
	var f wakeFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// The offset is the supervisor's clock at its answer minus the client's clock
// half way between its request and the answer, and the error is half the round
// trip. The request asks for the measured part, with a second to spare on each
// side, in the client's clock.
func TestClientWakeOffsetFromTheClockAroundTheRequest(t *testing.T) {
	r := wakeRig(t)
	send := unixSecond(9000, 0)
	r.c.now = scriptedNow(t, send, send.Add(40*time.Millisecond))
	// The supervisor read 1.5 s and 123 us ahead of the client when it answered,
	// 20 ms after the request was sent.
	r.sup.answerAt(send.Add(20*time.Millisecond + 1500*time.Millisecond + 123*time.Microsecond))
	start := unixSecond(8990, 500*time.Millisecond)
	var rec runRecord
	r.c.keepWake(context.Background(), "01-R1-r1", wakeReport(t, start, 3), &rec)

	want := clockOffset{OffsetUS: 1_500_123, ErrorUS: 20_000}
	if rec.ClockOffset == nil || *rec.ClockOffset != want {
		t.Errorf("clock offset %+v, want %+v", rec.ClockOffset, want)
	}
	if rec.Wake != "runs/01-R1-r1.wake.json" || len(rec.Notes) != 0 {
		t.Errorf("wake %q, notes %v", rec.Wake, rec.Notes)
	}
	asked := r.sup.wakeAsked
	from, to := start.Add(-time.Second).UnixMilli(), start.Add(4*time.Second).UnixMilli()
	if len(asked) != 1 || asked[0].Get("from") != strconv.FormatInt(from, 10) || asked[0].Get("to") != strconv.FormatInt(to, 10) {
		t.Errorf("asked %v, want one request from %d to %d (a second either side of the 3 windows)", asked, from, to)
	}
	f := readWakeFile(t, r, rec.Wake)
	if f.Clock != want {
		t.Errorf("the file's clock %+v, want %+v", f.Clock, want)
	}
	var answered wakeAnswer
	if err := json.Unmarshal(fakeWakeAnswer(from, to), &answered); err != nil {
		t.Fatal(err)
	}
	answered.NowUnixNS = send.Add(20*time.Millisecond + 1500*time.Millisecond + 123*time.Microsecond).UnixNano()
	if !reflect.DeepEqual(f.wakeAnswer, answered) {
		t.Errorf("the file holds %+v, want the supervisor's answer %+v", f.wakeAnswer, answered)
	}
	if len(f.N) != 5 || f.T0 != start.Unix() || f.Placement.CPU != 7 || f.TickUS != 1000 {
		t.Errorf("file columns: t0 %d, %d seconds, placement %+v, tick %d", f.T0, len(f.N), f.Placement, f.TickUS)
	}
}

// With no series there is nothing to align the wake with, so there is no
// request, no file, no offset and no note.
func TestClientWakeNeedsASeries(t *testing.T) {
	for name, rep := range map[string]*loadgenReport{
		"no report":       nil,
		"no series":       {},
		"a null series":   {Series: json.RawMessage("null")},
		"an empty series": {Series: json.RawMessage("")},
	} {
		t.Run(name, func(t *testing.T) {
			r := wakeRig(t)
			var rec runRecord
			r.c.keepWake(context.Background(), "01-REST-r1", rep, &rec)
			if rec.Wake != "" || rec.ClockOffset != nil || len(rec.Notes) != 0 || r.sup.wakeReads != 0 {
				t.Errorf("wake %q, offset %+v, notes %v, %d requests, want none of them", rec.Wake, rec.ClockOffset, rec.Notes, r.sup.wakeReads)
			}
			if entries, _ := filepath.Glob(filepath.Join(r.out, "runs", "*")); len(entries) != 0 {
				t.Errorf("files written: %v", entries)
			}
		})
	}
}

func TestClientWakeNotesASeriesItCannotUse(t *testing.T) {
	for name, series := range map[string]string{
		"a start that is no time": `{"window_s":1,"start":"yesterday","n":[1,2]}`,
		"no start":                `{"window_s":1,"n":[1,2]}`,
		"no windows":              `{"window_s":1,"start":"2026-11-23T08:00:00Z","n":[]}`,
		"not an object":           `[1,2,3]`,
	} {
		t.Run(name, func(t *testing.T) {
			r := wakeRig(t)
			var rec runRecord
			r.c.keepWake(context.Background(), "01-R1-r1", &loadgenReport{Series: json.RawMessage(series)}, &rec)
			if rec.Wake != "" || rec.ClockOffset != nil || len(rec.Notes) != 1 || !strings.HasPrefix(rec.Notes[0], "wake: the report's series") || r.sup.wakeReads != 0 {
				t.Errorf("wake %q, offset %+v, notes %v, %d requests, want a note and no request", rec.Wake, rec.ClockOffset, rec.Notes, r.sup.wakeReads)
			}
		})
	}
}

// An answer that cannot be had or used is noted, and leaves no file and no
// offset.
func TestClientWakeThatCannotBeFetchedIsNoted(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   string
		note   string
	}{
		{"a server error", 500, "meter broken", "500"},
		{"a supervisor of an older build", 404, "404 page not found", "404"},
		{"a bad request", 400, "from is required", "400"},
		{"text that is not JSON", 200, "not json", "invalid character"},
		{"columns of different lengths", 200, `{"now_unix_ns":5,"n":[1,2],"p99":[1],"max":[1,2]}`, "differ in length"},
		{"no clock", 200, `{"n":[],"p99":[],"max":[]}`, "no now_unix_ns"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := wakeRig(t)
			r.sup.wakeAnswer = func(int, int64, int64) (int, []byte) { return c.status, []byte(c.body) }
			var rec runRecord
			r.c.keepWake(context.Background(), "01-R1-r1", wakeReport(t, unixSecond(9000, 0), 3), &rec)
			if rec.Wake != "" || rec.ClockOffset != nil || len(rec.Notes) != 1 || !strings.HasPrefix(rec.Notes[0], "wake: ") || !strings.Contains(rec.Notes[0], c.note) {
				t.Errorf("wake %q, offset %+v, notes %v, want a note that holds %q", rec.Wake, rec.ClockOffset, rec.Notes, c.note)
			}
			if entries, _ := filepath.Glob(filepath.Join(r.out, "runs", "*")); len(entries) != 0 || r.sup.wakeReads != 1 {
				t.Errorf("files %v and %d requests, want none and one", entries, r.sup.wakeReads)
			}
		})
	}
}

// The supervisor serves a second only once a later one has a wake, so a client
// that asks just after the window asks again, once, after the wait.
func TestClientWakeAsksAgainWhenTheLastSecondIsNotFinished(t *testing.T) {
	start := unixSecond(7000, 250*time.Millisecond) // three windows: the measured part ends at 7003.25
	// The supervisor serves second 7003 once its clock is past 7004 by the settle slack.
	early, late := unixSecond(7003, 970*time.Millisecond), unixSecond(7004, 16*time.Millisecond)

	t.Run("the second answer is the one kept", func(t *testing.T) {
		r := wakeRig(t)
		r.c.now = scriptedNow(t, early.Add(-time.Millisecond), early.Add(time.Millisecond), late.Add(-time.Millisecond), late.Add(time.Millisecond))
		r.sup.answerAt(early, late)
		var rec runRecord
		began := time.Now()
		r.c.keepWake(context.Background(), "01-R1-r1", wakeReport(t, start, 3), &rec)
		if took := time.Since(began); took < 40*time.Millisecond {
			t.Errorf("keepWake took %v, want it to wait the 40 ms that the supervisor needs", took)
		}
		if r.sup.wakeReads != 2 || len(rec.Notes) != 0 {
			t.Errorf("%d requests, notes %v, want 2 and none", r.sup.wakeReads, rec.Notes)
		}
		f := readWakeFile(t, r, rec.Wake)
		if f.NowUnixNS != late.UnixNano() || rec.ClockOffset == nil || *rec.ClockOffset != (clockOffset{OffsetUS: 0, ErrorUS: 1000}) || f.Clock != *rec.ClockOffset {
			t.Errorf("the file has the clock of %d and %+v, offset %+v, want the second answer's, %d, offset 0 and error 1000 us", f.NowUnixNS, f.Clock, rec.ClockOffset, late.UnixNano())
		}
	})

	t.Run("a supervisor that does not settle is asked twice and noted", func(t *testing.T) {
		r := wakeRig(t)
		r.c.now = scriptedNow(t, early.Add(-time.Millisecond), early.Add(time.Millisecond), early.Add(-time.Millisecond), early.Add(time.Millisecond))
		r.sup.answerAt(early)
		var rec runRecord
		r.c.keepWake(context.Background(), "01-R1-r1", wakeReport(t, start, 3), &rec)
		if r.sup.wakeReads != 2 || len(rec.Notes) != 1 || !strings.Contains(rec.Notes[0], "not finished") {
			t.Errorf("%d requests, notes %v, want 2 and a note that the last second was not finished", r.sup.wakeReads, rec.Notes)
		}
		if rec.Wake == "" || rec.ClockOffset == nil {
			t.Errorf("wake %q, offset %+v: the answer is kept all the same", rec.Wake, rec.ClockOffset)
		}
	})

	t.Run("a context that ends during the wait keeps nothing", func(t *testing.T) {
		r := wakeRig(t)
		r.c.now = scriptedNow(t, early.Add(-time.Millisecond), early.Add(time.Millisecond))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r.sup.answerAt(early)
		answer := r.sup.wakeAnswer
		r.sup.wakeAnswer = func(n int, from, to int64) (int, []byte) {
			time.AfterFunc(10*time.Millisecond, cancel) // while the client waits
			return answer(n, from, to)
		}
		var rec runRecord
		r.c.keepWake(ctx, "01-R1-r1", wakeReport(t, start, 3), &rec)
		if r.sup.wakeReads != 1 || rec.Wake != "" || rec.ClockOffset != nil {
			t.Errorf("%d requests, wake %q, offset %+v, want one request and nothing kept", r.sup.wakeReads, rec.Wake, rec.ClockOffset)
		}
	})
}

// Through the matrix: every run whose report has a series has a wake file next
// to it and an offset in its record, and the REST run, which has none, has
// neither; a wake that cannot be had leaves every run as valid as it was.
func TestClientKeepsTheWakeOfEachRunThatHasASeries(t *testing.T) {
	// Over before the test clock's first reading, so that the supervisor has
	// finished every second of it.
	start := clockStart.Add(-time.Hour + 250*time.Millisecond)
	matrix := func(t *testing.T, mutate func(*fakeSupervisor)) (*rig, []runRecord) {
		t.Helper()
		p := rounds(1)
		r := newRig(t, p, nil)
		writeMatrixScript(t, p, func(cfg config, round int, f fakeRun) fakeRun {
			if cfg.Flow == flowREST {
				return f
			}
			return editReport(f, func(c *cannedReport) { c.Series = wakeSeries(start, 5) })
		})
		if mutate != nil {
			mutate(r.sup)
		}
		if err := r.run(); err != nil {
			t.Fatal(err)
		}
		return r, r.records()
	}
	r, records := matrix(t, nil)
	_, without := matrix(t, func(f *fakeSupervisor) {
		f.wakeAnswer = func(int, int64, int64) (int, []byte) { return http.StatusInternalServerError, []byte("no meter") }
	})
	if len(records) != 10 || len(without) != 10 {
		t.Fatalf("%d and %d records, want 10", len(records), len(without))
	}
	withSeries := 0
	for i, rec := range records {
		base := strings.TrimSuffix(strings.TrimPrefix(rec.Report, "runs/"), ".json")
		if rec.Flow == flowREST {
			if rec.Wake != "" || rec.ClockOffset != nil || len(rec.Notes) != 0 {
				t.Errorf("%s has no series: wake %q, offset %+v, notes %v", base, rec.Wake, rec.ClockOffset, rec.Notes)
			}
			if _, err := os.Stat(filepath.Join(r.out, "runs", base+".wake.json")); err == nil {
				t.Errorf("%s has a wake file though it has no series", base)
			}
			continue
		}
		withSeries++
		if rec.Wake != "runs/"+base+".wake.json" || rec.ClockOffset == nil || len(rec.Notes) != 0 {
			t.Errorf("%s: wake %q, offset %+v, notes %v", base, rec.Wake, rec.ClockOffset, rec.Notes)
			continue
		}
		f := readWakeFile(t, r, rec.Wake)
		if f.Clock != *rec.ClockOffset || len(f.N) != 7 || len(f.P99) != 7 || len(f.Max) != 7 || f.T0 != start.Unix() {
			t.Errorf("%s: the file's clock %+v (record %+v), %d seconds from %d, want the record's clock and the 5 windows with a second either side", base, f.Clock, rec.ClockOffset, len(f.N), f.T0)
		}

		// A wake that could not be had changes nothing else.
		bad := without[i]
		if bad.Wake != "" || bad.ClockOffset != nil || len(bad.Notes) != 1 || !strings.HasPrefix(bad.Notes[0], "wake: ") {
			t.Errorf("%s without a wake: wake %q, offset %+v, notes %v", base, bad.Wake, bad.ClockOffset, bad.Notes)
		}
		if bad.Valid != rec.Valid || !reflect.DeepEqual(bad.Study, rec.Study) {
			t.Errorf("%s: valid %v and rules %+v without the wake, want %v and %+v", base, bad.Valid, bad.Study, rec.Valid, rec.Study)
		}
	}
	if withSeries != 9 {
		t.Errorf("%d runs had a series, want the 9 that are not REST", withSeries)
	}
	lines := strings.Split(strings.TrimSpace(string(r.file("runs.jsonl"))), "\n")
	if !strings.Contains(lines[0], `"wake":"runs/01-F-r1.wake.json","clock_offset":{"offset_us":`) {
		t.Errorf("the first line of runs.jsonl does not name the wake file and the offset: %s", lines[0])
	}
	if i := slices.IndexFunc(records, func(r runRecord) bool { return r.Flow == flowREST }); !strings.Contains(lines[i], `"wake":"","clock_offset":null`) {
		t.Errorf("the REST line has %s, want an empty wake and a null offset", lines[i])
	}
	if got, want := fmt.Sprint(len(r.sup.wakeAsked)), "9"; got != want {
		t.Errorf("%s /wake requests, want one for each of the %s runs with a series", got, want)
	}
}
