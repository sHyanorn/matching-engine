package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// worstSecond is the window of a run's series in which the main row's p99 was
// highest, with what the engine machine's wake meter saw over the same time.
// The latencies are the series' own, in us, as loadgen rounded them.
type worstSecond struct {
	Window    int       `json:"window"`   // its index in the series
	OffsetS   float64   `json:"offset_s"` // its start, in seconds from the start of the measured part
	Time      string    `json:"time"`     // its start, UTC, on the client's clock
	N         int       `json:"n"`
	P99       float64   `json:"p99"`
	P999      float64   `json:"p99_9"`
	Max       float64   `json:"max"`
	MedianP99 float64   `json:"median_p99"` // of every window that has samples: the scale to read p99 against
	Wake      *wakeSeen `json:"wake"`       // nil if the run has no wake file
}

// wakeSeen is the wake meter over one window, in us.
type wakeSeen struct {
	N   int     `json:"n"`
	P99 float64 `json:"p99"` // the largest of the p99 of the seconds that overlap the window
	Max float64 `json:"max"`
}

// seriesColumns are the columns of a row of loadgen's series that the worst
// window is taken from.
type seriesColumns struct {
	P99  []float64 `json:"p99"`
	P999 []float64 `json:"p99_9"`
	Max  []float64 `json:"max"`
}

// mainRows are the rows of a series that can be its main one: reply for the
// order flows, pong for Pings.
var mainRows = []string{"reply", "pong"}

// worstOf finds the worst window of the report's series and sets against it
// what the wake meter saw, from the seconds that keepWake kept (nil if it kept
// none) and the offset it measured. It returns nil for a report without a
// series, and for one whose series keepWake has already noted as unusable. A
// series that has a start and windows but no usable main row is noted in rec.
// If no kept second overlaps the window its wake stays nil, which is not a wake
// that saw nothing, and rec says so.
func worstOf(rep *loadgenReport, wake []wakeRow, off *clockOffset, rec *runRecord) *worstSecond {
	ser, err := decodeSeries(rep)
	if ser == nil || err != nil {
		return nil
	}
	w, err := worstWindow(*ser, rep.Series)
	if err != nil {
		rec.Notes = append(rec.Notes, "worst: "+err.Error())
		return nil
	}
	if wake != nil && off != nil {
		start := ser.Start.Add(time.Duration(w.Window) * ser.window())
		// The last window is shorter when the measured part is not a whole number of windows.
		n, p99, worst, seen := wakeForWindow(wake, off.duration(), start, min(ser.window(), ser.End.Sub(start)))
		if seen {
			w.Wake = &wakeSeen{N: n, P99: p99, Max: worst}
		} else {
			rec.Notes = append(rec.Notes, "wake: no second of the meter overlaps the worst second: the clocks may differ by more than the offset's error, or the meter recorded nothing then")
		}
	}
	return w
}

// worstWindow returns the window of ser with the largest p99 of the main row
// of raw, the report's series: the first of those that tie, and never one
// without samples. The last window may be shorter than the others; it starts
// where they do.
func worstWindow(ser reportSeries, raw json.RawMessage) (*worstSecond, error) {
	var rows struct {
		Rows map[string]json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	name, col := "", seriesColumns{}
	for _, n := range mainRows {
		if data, ok := rows.Rows[n]; ok {
			name = n
			if err := json.Unmarshal(data, &col); err != nil {
				return nil, fmt.Errorf("the %s row: %w", n, err)
			}
			break
		}
	}
	if name == "" {
		return nil, errors.New("the series has no reply or pong row")
	}
	if len(col.P99) != len(ser.N) || len(col.P999) != len(ser.N) || len(col.Max) != len(ser.N) {
		return nil, fmt.Errorf("the %s row has %d p99, %d p99_9 and %d max values for %d windows", name, len(col.P99), len(col.P999), len(col.Max), len(ser.N))
	}
	best := -1
	var p99s []float64
	for k, n := range ser.N {
		if n == 0 {
			continue
		}
		p99s = append(p99s, col.P99[k])
		if best < 0 || col.P99[k] > col.P99[best] {
			best = k
		}
	}
	if best < 0 {
		return nil, errors.New("no window of the series has a sample")
	}
	at := time.Duration(best) * ser.window()
	return &worstSecond{
		Window: best, OffsetS: at.Seconds(), Time: ser.Start.Add(at).UTC().Format(time.RFC3339Nano),
		N: ser.N[best], P99: col.P99[best], P999: col.P999[best], Max: col.Max[best], MedianP99: median(p99s),
	}, nil
}
