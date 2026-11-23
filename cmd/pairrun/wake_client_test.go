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

// answerWithClock makes the fake supervisor answer with its seconds and the
// clock that now reads, so that a client whose clock moves finds the
// supervisor's moving with it.
func (f *fakeSupervisor) answerWithClock(now func() time.Time) {
	f.wakeAnswer = func(_ int, from, to int64) (int, []byte) {
		var a wakeAnswer
		if err := json.Unmarshal(fakeWakeAnswer(from, to), &a); err != nil {
			panic(err)
		}
		a.NowUnixNS = now().UnixNano()
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
// trip. An offset within the tolerance leaves the one request, which asks for
// the measured part, with a second to spare on each side, in the client's clock.
func TestClientWakeOffsetFromTheClockAroundTheRequest(t *testing.T) {
	r := wakeRig(t)
	send := unixSecond(9000, 0)
	r.c.now = scriptedNow(t, send, send.Add(40*time.Millisecond))
	// The supervisor read 150 ms and 123 us ahead of the client when it answered,
	// 20 ms after the request was sent.
	r.sup.answerAt(send.Add(20*time.Millisecond + 150*time.Millisecond + 123*time.Microsecond))
	start := unixSecond(8990, 500*time.Millisecond)
	var rec runRecord
	r.c.keepWake(context.Background(), "01-R1-r1", wakeReport(t, start, 3), &rec)

	want := clockOffset{OffsetUS: 150_123, ErrorUS: 20_000}
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
	answered.NowUnixNS = send.Add(20*time.Millisecond + 150*time.Millisecond + 123*time.Microsecond).UnixNano()
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

// askedRange returns the from and to, in Unix ms, of the nth /wake request of
// the fake supervisor, counted from 0.
func askedRange(t *testing.T, f *fakeSupervisor, n int) (from, to int64) {
	t.Helper()
	if n >= len(f.wakeAsked) {
		t.Fatalf("%d /wake requests, no request %d", len(f.wakeAsked), n)
	}
	q := f.wakeAsked[n]
	from, errFrom := strconv.ParseInt(q.Get("from"), 10, 64)
	to, errTo := strconv.ParseInt(q.Get("to"), 10, 64)
	if errFrom != nil || errTo != nil {
		t.Fatalf("request %d asked %v, want a from and a to in Unix ms", n, q)
	}
	return from, to
}

// A client whose engine machine's clock is 5 s ahead gets the seconds of its
// window: the first request measures the offset, and the second asks for the
// measured part on the supervisor's clock. The meter below recorded the seconds
// that the part lies in on its own clock, 8985 to 8988, and the unshifted range
// holds only seconds before them.
func TestClientWakeAsksForThePartOnTheSupervisorsClock(t *testing.T) {
	const ahead = 5 * time.Second
	base := unixSecond(9000, 0)
	r := wakeRig(t)
	r.c.now = func() time.Time { return base } // no time passes, so the offset is exact
	start := base.Add(-20*time.Second + 250*time.Millisecond)
	end := start.Add(3 * time.Second)
	// Second s, from 8983 to 8992, has s-8980 wakes, each s-8980 tens of us late.
	m := newWakeMeter(wakePlacement{CPU: 1, Siblings: []int{1}})
	for sec := int64(8983); sec <= 8992; sec++ {
		for i := range int(sec - 8980) {
			m.record(unixSecond(sec, time.Duration(i)*time.Millisecond), time.Duration(sec-8980)*10*time.Microsecond)
		}
	}
	r.sup.wakeAnswer = func(_ int, from, to int64) (int, []byte) {
		a := m.answer(from, to)
		a.NowUnixNS = base.Add(ahead).UnixNano()
		data, err := json.Marshal(a)
		if err != nil {
			panic(err)
		}
		return http.StatusOK, data
	}
	rep := wakeReport(t, start, 3)
	var rec runRecord
	rows := r.c.keepWake(context.Background(), "01-R1-r1", rep, &rec)
	worst := worstOf(rep, rows, rec.ClockOffset, &rec)

	if len(r.sup.wakeAsked) != 2 {
		t.Fatalf("%d requests, want the probe and one built with the offset", len(r.sup.wakeAsked))
	}
	for i, shift := range []time.Duration{0, ahead} {
		from, to := askedRange(t, r.sup, i)
		wantFrom, wantTo := start.Add(shift-time.Second).UnixMilli(), end.Add(shift+time.Second).UnixMilli()
		if from != wantFrom || to != wantTo {
			t.Errorf("request %d asked from %d to %d, want %d to %d (the part and a second either side, %v on)", i+1, from, to, wantFrom, wantTo, shift)
		}
	}
	if want := (clockOffset{OffsetUS: 5e6}); rec.ClockOffset == nil || *rec.ClockOffset != want {
		t.Errorf("clock offset %+v, want %+v", rec.ClockOffset, want)
	}
	f := readWakeFile(t, r, rec.Wake)
	if f.T0 != 8985 || len(f.N) != 5 || f.N[0] != 5 || f.N[4] != 9 {
		t.Errorf("the file has %d seconds from %d with n %v, want 8985 to 8989, the second request's", len(f.N), f.T0, f.N)
	}
	// Window 0, the worst of the flat series, is [8980.25, 8981.25) here and
	// [8985.25, 8986.25) there: seconds 8985 and 8986.
	if want := (&wakeSeen{N: 5 + 6, P99: 60, Max: 60}); worst == nil || !reflect.DeepEqual(worst.Wake, want) {
		t.Errorf("worst %+v, want a wake of %+v", worst, want)
	}
	if len(rec.Notes) != 0 {
		t.Errorf("notes %v", rec.Notes)
	}
}

// The first request measures the offset and each next one is built with the
// offset of the one before. A request is repeated when its offset is more than
// 250 ms from the one its range was built with, three requests at most, and the
// offset kept with the file is the one measured on the last request, whose
// seconds are the file's.
func TestClientWakeAsksAgainWhileTheOffsetMoves(t *testing.T) {
	base := unixSecond(9000, 0)
	start := base.Add(-time.Minute)
	end := start.Add(3 * time.Second)
	for _, c := range []struct {
		name    string
		offsets []time.Duration // the supervisor's clock minus the client's at each answer
		note    bool            // that it kept moving
	}{
		{"an offset within the tolerance is kept as it comes", []time.Duration{100 * time.Millisecond}, false},
		{"a large offset is asked for again", []time.Duration{5 * time.Second, 5 * time.Second}, false},
		{"a negative one too", []time.Duration{-3 * time.Second, -3 * time.Second}, false},
		{"an offset that moves less than the tolerance since the range was built", []time.Duration{5 * time.Second, 5200 * time.Millisecond}, false},
		{"an offset that moves more than it", []time.Duration{5 * time.Second, 5300 * time.Millisecond, 5300 * time.Millisecond}, false},
		{"an offset that keeps moving stops at three requests", []time.Duration{5 * time.Second, 6 * time.Second, 7 * time.Second, 8 * time.Second}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := wakeRig(t)
			r.c.now = func() time.Time { return base }
			nows := make([]time.Time, len(c.offsets))
			for i, o := range c.offsets {
				nows[i] = base.Add(o)
			}
			r.sup.answerAt(nows...)
			var rec runRecord
			r.c.keepWake(context.Background(), "01-R1-r1", wakeReport(t, start, 3), &rec)

			asks := min(len(c.offsets), wakeAsks)
			if r.sup.wakeReads != asks {
				t.Fatalf("%d requests, want %d", r.sup.wakeReads, asks)
			}
			for i := range asks {
				var shift time.Duration // the offset the range is built with: the one just measured
				if i > 0 {
					shift = c.offsets[i-1]
				}
				from, to := askedRange(t, r.sup, i)
				wantFrom, wantTo := start.Add(shift-time.Second).UnixMilli(), end.Add(shift+time.Second).UnixMilli()
				if from != wantFrom || to != wantTo {
					t.Errorf("request %d asked from %d to %d, want %d to %d (shifted by %v)", i+1, from, to, wantFrom, wantTo, shift)
				}
			}
			kept := c.offsets[asks-1]
			f := readWakeFile(t, r, rec.Wake)
			if rec.ClockOffset == nil || rec.ClockOffset.duration() != kept || f.Clock != *rec.ClockOffset || f.NowUnixNS != nows[asks-1].UnixNano() {
				t.Errorf("kept offset %+v, the file's clock %+v and now %d, want %v and the last request's answer, %d", rec.ClockOffset, f.Clock, f.NowUnixNS, kept, nows[asks-1].UnixNano())
			}
			if moving := len(rec.Notes) == 1 && strings.Contains(rec.Notes[0], "kept moving"); moving != c.note || len(rec.Notes) > 1 {
				t.Errorf("notes %v, want one that the offset kept moving: %v", rec.Notes, c.note)
			}
		})
	}
}

// The wait for the last second and the shift are in the same loop: a part that
// has just ended on a supervisor 5 s ahead takes two requests and one wait.
func TestClientWakeWaitsOnceAndShiftsInTheSameRequest(t *testing.T) {
	base := unixSecond(9000, 0)
	end := base.Add(-250 * time.Millisecond)
	start := end.Add(-3 * time.Second)
	r := wakeRig(t)
	r.c.now = func() time.Time { return base }
	// On the supervisor's clock the part ends at 9004.75, so it must run to
	// 9005.01 for second 9004 to be served: 10 ms after its first answer.
	r.sup.answerAt(base.Add(5*time.Second), base.Add(5*time.Second+30*time.Millisecond))
	var rec runRecord
	began := time.Now()
	r.c.keepWake(context.Background(), "01-R1-r1", wakeReport(t, start, 3), &rec)
	if took := time.Since(began); took < 10*time.Millisecond {
		t.Errorf("keepWake took %v, want it to wait the 10 ms that the supervisor needs", took)
	}
	if r.sup.wakeReads != 2 || len(rec.Notes) != 0 {
		t.Errorf("%d requests, notes %v, want 2 and none", r.sup.wakeReads, rec.Notes)
	}
	from, to := askedRange(t, r.sup, 1)
	if wantFrom, wantTo := start.Add(4*time.Second).UnixMilli(), end.Add(6*time.Second).UnixMilli(); from != wantFrom || to != wantTo {
		t.Errorf("the second request asked from %d to %d, want %d to %d", from, to, wantFrom, wantTo)
	}
	if rec.ClockOffset == nil || rec.ClockOffset.duration() != 5*time.Second+30*time.Millisecond {
		t.Errorf("clock offset %+v, want the second answer's, 5.03 s", rec.ClockOffset)
	}
}

// The last window is shorter when the measured part is not a whole number of
// seconds, so the request ends a second past where the part really ends.
func TestClientWakeAsksUpToTheEndOfAPartThatIsNotWhole(t *testing.T) {
	r := wakeRig(t)
	r.c.now = func() time.Time { return clockStart }
	r.sup.answerAt(clockStart)
	start := clockStart.Add(-time.Hour + 250*time.Millisecond)
	rep := wakeReport(t, start, 3)
	rep.DurationS = 2.2 // the third window holds the last 0.2 s
	var rec runRecord
	r.c.keepWake(context.Background(), "01-R1-r1", rep, &rec)
	if r.sup.wakeReads != 1 || len(rec.Notes) != 0 {
		t.Fatalf("%d requests, notes %v, want 1 and none", r.sup.wakeReads, rec.Notes)
	}
	from, to := askedRange(t, r.sup, 0)
	if wantFrom, wantTo := start.Add(-time.Second).UnixMilli(), start.Add(2200*time.Millisecond+time.Second).UnixMilli(); from != wantFrom || to != wantTo {
		t.Errorf("asked from %d to %d, want %d to %d: the part's end, 2.2 s after its start, and a second", from, to, wantFrom, wantTo)
	}
}

// The note of the placement is in the wake file that the client keeps: a meter
// on cpu2, which the spin pool of cpu0 and cpu2 may hand out.
func TestClientKeepsThePlacementNoteInTheWakeFile(t *testing.T) {
	r := wakeRig(t)
	r.c.now = func() time.Time { return clockStart }
	m := newWakeMeter(placeWake(fakeTopology(t, map[int]string{0: "0-1", 1: "0-1", 2: "2"}), cpuRange(0, 2)))
	r.sup.wakeAnswer = func(_ int, from, to int64) (int, []byte) {
		a := m.answer(from, to)
		a.NowUnixNS = clockStart.UnixNano()
		data, err := json.Marshal(a)
		if err != nil {
			panic(err)
		}
		return http.StatusOK, data
	}
	var rec runRecord
	r.c.keepWake(context.Background(), "01-R1-r1", wakeReport(t, clockStart.Add(-time.Hour), 3), &rec)
	f := readWakeFile(t, r, rec.Wake)
	if f.Placement.CPU != 2 || !strings.Contains(f.Placement.Note, sharesPool) {
		t.Errorf("placement %+v in the wake file, want cpu 2 and a note that says %q", f.Placement, sharesPool)
	}
	if !strings.Contains(string(r.file(rec.Wake)), `"note":"`+sharesPool+": ") {
		t.Errorf("the wake file %s does not hold the note", r.file(rec.Wake))
	}
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
		r.sup.answerWithClock(r.clock.Now) // the same clock as the client's, so that one request settles the offset
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
