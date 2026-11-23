package main

import (
	"fmt"
	"slices"
	"strconv"
	"time"
)

// The series of an OE1 report gives the main latency row (reply, or pong for
// Pings) and the send lag per window of one second of the measured part, so
// that a raised tail can be placed in time inside a run. It is made when the
// report is, from the samples the report already walks in due order, and costs
// the run nothing.

// seriesWindow is the width of a window of the series.
const seriesWindow = time.Second

// series is the latency of the measured part window by window, in columns.
// Window k holds the requests due in the k-th second after Start, whenever
// their replies came: a stall is charged to the requests due while it lasted,
// as in D030. The last window is shorter when the measured part is not a whole
// number of seconds.
type series struct {
	WindowS float64   `json:"window_s"`
	Start   time.Time `json:"start"` // wall clock, UTC: the start of the measured part
	N       []int     `json:"n"`     // samples per window; an empty window has n 0 and 0 in every array below
	// Rows holds, for the main row ("reply" or "pong") and for "send_lag",
	// the percentiles of each window's samples: the nearest-rank values
	// summarize gives, in microseconds. The series keeps two significant
	// digits of each to stay small; the report's own rows keep their exact
	// values.
	Rows map[string]seriesRow `json:"rows"`
}

// seriesRow is one latency row of the series, a value per window.
type seriesRow struct {
	P50  []float64 `json:"p50"`
	P99  []float64 `json:"p99"`
	P999 []float64 `json:"p99_9"`
	Max  []float64 `json:"max"`
}

func newSeriesRow(windows int) seriesRow {
	return seriesRow{make([]float64, windows), make([]float64, windows), make([]float64, windows), make([]float64, windows)}
}

// set writes the values of window k from its samples, in nanoseconds, which it
// sorts.
func (r seriesRow) set(k int, samples []int64) {
	slices.Sort(samples)
	r.P50[k], r.P99[k], r.P999[k] = twoDigits(at(samples, 50)), twoDigits(at(samples, 99)), twoDigits(at(samples, 99.9))
	r.Max[k] = twoDigits(micros(samples[len(samples)-1]))
}

// twoDigits rounds v to two significant digits: 87.951 is 88, 1234.5 is 1200
// and 0.253 is 0.25. The error is at most 5% of v.
func twoDigits(v float64) float64 {
	f, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'g', 2, 64), 64) // FormatFloat writes a number
	return f
}

// seriesBuilder makes a series from samples that come in due order: it holds
// the samples of the window they are in, and takes their percentiles when the
// next window begins.
type seriesBuilder struct {
	start, end int64        // of the measured part, in ns from the start of the run
	cur        int          // the window of the samples held, -1 before the first
	vals, lags []int64      // of the window held
	rows       [2]seriesRow // the main row and the send lag
	out        series
	problem    string // the first sample that came after a later window's
}

// newSeries starts the series of a run that began at t0, for the main row
// row, over the measured part that follows warmup and lasts duration.
func newSeries(row string, t0 time.Time, warmup, duration time.Duration) *seriesBuilder {
	n := int((duration + seriesWindow - 1) / seriesWindow)
	b := &seriesBuilder{start: int64(warmup), end: int64(warmup + duration), cur: -1,
		rows: [2]seriesRow{newSeriesRow(n), newSeriesRow(n)}}
	b.out = series{WindowS: seriesWindow.Seconds(), Start: t0.Add(warmup).UTC(), N: make([]int, n),
		Rows: map[string]seriesRow{row: b.rows[0], "send_lag": b.rows[1]}}
	return b
}

// add takes the sample of a request due at due, in ns from the start of the
// run: its value in the main row and its send lag, also in ns. A sample due
// outside the measured part is in no window. Samples must come in due order:
// one that is due in an earlier window than the one held is left out, and
// makes the run invalid rather than land in a window it does not belong to.
func (b *seriesBuilder) add(due, val, lag int64) {
	if due < b.start || due >= b.end {
		return
	}
	k := int((due - b.start) / int64(seriesWindow))
	switch {
	case k < b.cur:
		if b.problem == "" {
			b.problem = fmt.Sprintf("series: a request due at %v is in window %d, after samples of window %d", time.Duration(due), k, b.cur)
		}
		return
	case k > b.cur:
		b.flush()
		b.cur = k
	}
	b.vals, b.lags = append(b.vals, val), append(b.lags, lag)
}

// flush takes the percentiles of the window held and empties it, keeping the
// room for the next.
func (b *seriesBuilder) flush() {
	if len(b.vals) == 0 {
		return
	}
	b.out.N[b.cur] = len(b.vals)
	b.rows[0].set(b.cur, b.vals)
	b.rows[1].set(b.cur, b.lags)
	b.vals, b.lags = b.vals[:0], b.lags[:0]
}

// finish returns the series and problems with what makes the series wrong, if
// anything, added. It ends the builder.
func (b *seriesBuilder) finish(problems []string) (*series, []string) {
	b.flush()
	if b.problem != "" {
		problems = append(problems, b.problem)
	}
	return &b.out, problems
}
