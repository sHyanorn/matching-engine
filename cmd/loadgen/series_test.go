package main

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	usec = int64(time.Microsecond)
	msec = int64(time.Millisecond)
	sec  = int64(time.Second)
)

var seriesT0 = time.Date(2026, 11, 23, 12, 0, 0, 123456789, time.UTC)

// addAll feeds b the sample of each of n requests, 1 ms apart from the first at
// 0, in due order: sample(j) gives the value of request j in the main row and
// its send lag, in ns.
func addAll(b *seriesBuilder, n int, sample func(j int) (val, lag int64)) {
	for j := range n {
		val, lag := sample(j)
		b.add(int64(j)*msec, val, lag)
	}
}

// A stall is charged to the requests that fell due while it lasted, not to the
// window in which their replies came: with no reply from 3.997 s to 4.003 s of
// the measured part, window 3 holds the three requests due before 4 s, which
// are answered in window 4, and window 4 the three due after. The warmup's
// samples, as slow as the stall, are in no window.
func TestSeriesChargesAStallToTheWindowsOfItsDueTimes(t *testing.T) {
	const warmup, windows = 1000, 10 // requests, 1 ms apart, and windows
	b := newSeries("reply", seriesT0, time.Second, windows*time.Second)
	addAll(b, warmup+windows*1000, func(j int) (int64, int64) {
		switch at := j - warmup; { // request j is due at warmup + at
		case at < 0:
			return 20 * msec, 20 * msec
		case at >= 3997 && at <= 4002: // answered at 4.003 s
			return int64(4003-at) * msec, usec
		}
		return 50 * usec, usec
	})
	s, problems := b.finish(nil)
	if len(problems) != 0 {
		t.Fatalf("problems %q", problems)
	}
	reply := s.Rows["reply"]
	for k := range windows {
		wantMax, want999 := 50.0, 50.0
		switch k {
		case 3: // due 3.997 to 3.999 s, answered 6, 5 and 4 ms later
			wantMax, want999 = 6000, 5000
		case 4: // due 4.000 to 4.002 s, answered 3, 2 and 1 ms later
			wantMax, want999 = 3000, 2000
		}
		if s.N[k] != 1000 || reply.Max[k] != wantMax || reply.P999[k] != want999 || reply.P50[k] != 50 || reply.P99[k] != 50 {
			t.Errorf("window %d: n %d, p50 %v, p99 %v, p99.9 %v, max %v; want n 1000, p50 50, p99 50, p99.9 %v, max %v",
				k, s.N[k], reply.P50[k], reply.P99[k], reply.P999[k], reply.Max[k], want999, wantMax)
		}
		if lag := s.Rows["send_lag"]; lag.Max[k] != 1 || lag.P999[k] != 1 {
			t.Errorf("window %d: send lag p99.9 %v, max %v, want 1: the lag is its own row", k, lag.P999[k], lag.Max[k])
		}
	}
}

// A window takes the requests due in [k s, k+1 s) after the warmup.
func TestSeriesWindowEdges(t *testing.T) {
	b := newSeries("reply", seriesT0, 2*time.Second, 3*time.Second)
	// Each sample's value says which it is; those in no window are slower than
	// any in one.
	for _, c := range []struct {
		due, val int64
		window   int // -1: none
	}{
		{0, sec, -1},             // the start of the warmup
		{2*sec - 1, sec, -1},     // the last instant of the warmup
		{2 * sec, 1 * usec, 0},   // the start of the measured part
		{3*sec - 1, 2 * usec, 0}, // just before 1 s
		{3 * sec, 3 * usec, 1},   // exactly 1 s
		{4*sec - 1, 4 * usec, 1}, // just before 2 s
		{4 * sec, 5 * usec, 2},   // exactly 2 s
		{5*sec - 1, 6 * usec, 2}, // the last instant of the measured part
		{5 * sec, sec, -1},       // its end
		{5*sec + 1, sec, -1},     // after it
	} {
		b.add(c.due, c.val, c.val)
	}
	s, problems := b.finish(nil)
	if len(problems) != 0 {
		t.Fatalf("problems %q", problems)
	}
	reply := s.Rows["reply"]
	// Each window has two samples: p50 is the first, max the second.
	if want := []int{2, 2, 2}; !slices.Equal(s.N, want) ||
		!slices.Equal(reply.P50, []float64{1, 3, 5}) || !slices.Equal(reply.Max, []float64{2, 4, 6}) ||
		!slices.Equal(s.Rows["send_lag"].Max, []float64{2, 4, 6}) {
		t.Errorf("n %v, p50 %v, max %v, send lag max %v; want n %v, p50 [1 3 5], max [2 4 6] in both rows",
			s.N, reply.P50, reply.Max, s.Rows["send_lag"].Max, want)
	}
}

// A window with no request, between two with some or after the last, has n 0
// and zeros; a measured part of 2.5 s has a last window of half a second.
func TestSeriesEmptyAndShortWindows(t *testing.T) {
	b := newSeries("pong", seriesT0, 0, 5500*time.Millisecond)
	for _, due := range []int64{0, 900 * msec, 1100 * msec, 3 * sec, 3999 * msec} { // windows 0, 0, 1, 3, 3
		b.add(due, 10*usec, usec)
	}
	s, _ := b.finish(nil)
	want := []int{2, 1, 0, 2, 0, 0}
	if !slices.Equal(s.N, want) {
		t.Fatalf("n %v, want %v: windows 2, 4 and the half second of window 5 are empty", s.N, want)
	}
	for name, row := range s.Rows {
		for _, col := range [][]float64{row.P50, row.P99, row.P999, row.Max} {
			if len(col) != len(want) {
				t.Fatalf("row %s has %d values, want one for each of the %d windows", name, len(col), len(want))
			}
			for k, n := range want {
				if got := col[k]; (n == 0) != (got == 0) {
					t.Errorf("row %s window %d: n %d and value %v, want 0 exactly in an empty window", name, k, n, got)
				}
			}
		}
	}

	// The last window holds what is due in the half second.
	b = newSeries("pong", seriesT0, 0, 2500*time.Millisecond)
	for _, due := range []int64{100 * msec, 1500 * msec, 2400 * msec} {
		b.add(due, 10*usec, usec)
	}
	if s, _ = b.finish(nil); !slices.Equal(s.N, []int{1, 1, 1}) {
		t.Errorf("n %v for 2.5 s, want [1 1 1]", s.N)
	}

	// Nothing measured: the windows are there and empty.
	if s, _ = newSeries("pong", seriesT0, time.Second, 2*time.Second).finish(nil); !slices.Equal(s.N, []int{0, 0}) {
		t.Errorf("n %v with no sample, want [0 0]", s.N)
	}
}

// A sample due in an earlier window than the one held cannot come from the
// fill functions, which walk the requests in due order. If one does, it is
// left out and the run is invalid, rather than counted in a window it does
// not belong to. Samples of one window may come in any order.
func TestSeriesOutOfOrderSampleInvalidatesTheRun(t *testing.T) {
	b := newSeries("reply", seriesT0, 0, 4*time.Second)
	b.add(2500*msec, 7*usec, usec)
	b.add(2100*msec, 8*usec, usec) // earlier, in the same window: fine
	b.add(1900*msec, 9*usec, usec) // window 1, after window 2
	b.add(3*sec, 5*usec, usec)
	b.add(500*msec, 9*usec, usec) // window 0, after window 3: the first is the one named
	s, problems := b.finish([]string{"earlier problem"})
	if len(problems) != 2 || problems[0] != "earlier problem" ||
		!strings.Contains(problems[1], "series: a request due at 1.9s is in window 1, after samples of window 2") {
		t.Errorf("problems %q, want the earlier one and the first sample out of order, due at 1.9s", problems)
	}
	if want := []int{0, 0, 2, 1}; !slices.Equal(s.N, want) || !slices.Equal(s.Rows["reply"].Max, []float64{0, 0, 8, 5}) {
		t.Errorf("n %v and max %v, want n %v and max [0 0 8 5]: the late samples are in no window", s.N, s.Rows["reply"].Max, want)
	}
	if _, problems := newSeries("reply", seriesT0, 0, time.Second).finish(nil); len(problems) != 0 {
		t.Errorf("problems %q for a series with no sample", problems)
	}
}

// seededRun is a run of cfg as its pacer and reader leave it, with every array
// filled from seed: replies 40 to 60 µs after they are due, a raised tail in
// every third window, a few Pings with no Pong and commands that failed or got
// no Done.
func seededRun(cfg config, seed uint64) *oeRun {
	r := newOERun(cfg, discardConn{})
	r.t0 = seriesT0
	rng := rand.New(rand.NewPCG(seed, 1))
	rtt := func(j int) int64 {
		if r.due(j)/sec%3 == 1 && j%97 == 0 {
			return (3 + rng.Int64N(5)) * msec
		}
		return (40 + rng.Int64N(20)) * usec
	}
	for j := range r.total {
		r.lag[j] = rng.Int64N(5) * usec
		if j%211 == 0 {
			r.lag[j] += 2 * msec
		}
		if j%50 == 0 {
			r.gap[j] = 1 + rng.Int64N(30)*usec
		}
	}
	switch {
	case !cfg.orderFlow():
		for j := range r.total {
			r.rtt[j], r.got[j] = rtt(j), j%997 != 5
		}
	case cfg.ioc:
		for j := range r.total {
			r.cmds[j], r.rtt[j], r.res[j] = command{int32(j), int32(j + 1)}, rtt(j), orderCanceled
			switch {
			case j%991 == 7:
				r.res[j] = commandError
			case j%1009 == 11:
				r.res[j] = noDone
			}
		}
		r.nsent = r.total
	case cfg.ackCancel:
		for j := range r.total {
			o, c := 2*j, 2*j+1
			r.cmds[o], r.cmds[c] = command{int32(j), int32(j + 1)}, command{request: int32(j)}
			r.rtt[o], r.rtt[c] = rtt(j), rtt(j)/2
			r.res[o], r.res[c] = orderRested, cancelCanceled
			r.cancelAt[j] = r.due(j) + r.lag[j] + 20*usec
			switch {
			case j%991 == 7:
				r.res[o] = commandError
			case j%1009 == 11:
				r.res[o] = noDone
			}
		}
		r.nsent = 2 * r.total
	default: // -orders: every third request is a cancel, a tenth of them not sent
		for j := range r.total {
			c, out := command{request: int32(j), order: int32(j + 1)}, orderRested
			switch {
			case j%3 == 2 && j%30 == 2:
				if r.due(j) >= int64(cfg.warmup) {
					r.skipped++
				}
				continue
			case j%3 == 2:
				c.order, out = 0, cancelCanceled
			case j%4 == 0:
				out = orderTraded
				r.trades++
			case j%991 == 7:
				out = commandError
			case j%1009 == 11:
				out = noDone
			}
			r.cmds[r.nsent], r.res[r.nsent], r.rtt[r.nsent] = c, out, rtt(j)
			r.nsent++
		}
	}
	return r
}

// seededConfig is the config of a seeded run of 4.5 s, 3.5 s of them measured,
// at 1,000 requests a second, for the flow flags that set chooses.
func seededConfig(set func(*config)) config {
	cfg := config{rate: 1000, warmup: time.Second, duration: 3500 * time.Millisecond, spin: true, maxSendLag: time.Hour, symbol: fakeSymbol}
	set(&cfg)
	return cfg
}

var seededFlows = []struct {
	name, row string
	set       func(*config)
}{
	{"Ping", "pong", func(*config) {}},
	{"R3", "reply", func(c *config) { c.orders = true }},
	{"R1", "reply", func(c *config) { c.ioc = true }},
	{"R2", "reply", func(c *config) { c.ackCancel = true }},
}

// checkSeries checks that rep has a series of the given number of windows
// whose rows are the main row and the send lag, with the samples of the
// report's rows of the same names: as many, and the same slowest.
func checkSeries(t *testing.T, rep report, main string, windows int) {
	t.Helper()
	s := rep.Series
	if s == nil {
		t.Fatalf("the %s report has no series", rep.Mode)
	}
	if s.WindowS != 1 || len(s.N) != windows || len(s.Rows) != 2 {
		t.Fatalf("series of %d windows of %v s with %d rows, want %d windows of 1 s and the rows %s and send_lag", len(s.N), s.WindowS, len(s.Rows), windows, main)
	}
	for _, name := range []string{main, "send_lag"} {
		row, ok := s.Rows[name]
		if !ok {
			t.Fatalf("no row %s in the series", name)
		}
		for _, col := range [][]float64{row.P50, row.P99, row.P999, row.Max} {
			if len(col) != windows {
				t.Fatalf("row %s has %d values, want %d", name, len(col), windows)
			}
		}
		n := 0
		for _, v := range s.N {
			n += v
		}
		// The series keeps two digits, which is within 5% of the row's value.
		if sum := rep.Latency[name]; n != sum.Count || sum.Count == 0 || math.Abs(slices.Max(row.Max)-sum.Max) > 0.05*sum.Max+1e-9 {
			t.Errorf("row %s: %d samples and a slowest of %v µs in the series, %d and %v in the report", name, n, slices.Max(row.Max), sum.Count, sum.Max)
		}
	}
}

// Every flow's report has the series of its main row, and of the send lag, with
// the samples of the rows it sits beside.
func TestSeriesMatchesTheRowsOfEveryFlow(t *testing.T) {
	for _, f := range seededFlows {
		cfg := seededConfig(f.set)
		r := seededRun(cfg, 3)
		rep := r.report()
		checkSeries(t, rep, f.row, 4)
		if want := r.t0.Add(cfg.warmup); !rep.Series.Start.Equal(want) || rep.Series.Start.Location() != time.UTC {
			t.Errorf("%s: series starts at %v, want %v, in UTC", f.name, rep.Series.Start, want)
		}
		if rep.Series.N[3] == 0 || rep.Series.N[3] >= rep.Series.N[0] {
			t.Errorf("%s: windows hold %v samples, want the last, of half a second, to hold fewer than the first", f.name, rep.Series.N)
		}
	}
}

// seqOf is the client_seq of the order of request j in r, or j for a Ping.
func seqOf(r *oeRun, j int) int {
	if !r.cfg.orderFlow() {
		return j
	}
	for seq, c := range r.cmds[:r.nsent] {
		if int(c.request) == j && c.order != 0 {
			return seq
		}
	}
	return -1
}

// A request is in the window of its due time, whenever it was answered: the
// last orders due in each window are answered 100 to 130 ms later, in a later
// window, and are the slowest of their own.
func TestSeriesOfAReportFollowsDueTimes(t *testing.T) {
	for _, f := range seededFlows {
		cfg := seededConfig(f.set)
		r := seededRun(cfg, 5)
		for k := range 4 {
			j := min(2000+1000*k, r.total) - 2 // due 2 ms before the window ends
			if seqOf(r, j) < 0 {               // a cancel of the order flow
				j--
			}
			seq := seqOf(r, j)
			r.rtt[seq] = int64(100+10*k) * msec
			if cfg.orderFlow() {
				r.res[seq] = orderRested // answered, whatever it was
			} else {
				r.got[j] = true
			}
		}
		rep := r.report()
		for k, got := range rep.Series.Rows[f.row].Max {
			if want := float64(100+10*k) * 1000; got != want {
				t.Errorf("%s: window %d: max %v µs, want %v, that of the order due at its end", f.name, k, got, want)
			}
		}
	}
}

// The windows of the Ping and R1 series are what summarize says of the samples
// due in each, to two digits: the same nearest-rank percentiles, from the same
// samples for the send lag as for the round trip.
func TestSeriesWindowsAreTheSummaryOfTheirSamples(t *testing.T) {
	for _, f := range []struct {
		name, row string
		set       func(*config)
		answered  func(r *oeRun, j int) bool // whether request j has a sample, whose client_seq is j
	}{
		{"Ping", "pong", func(*config) {}, func(r *oeRun, j int) bool { return r.got[j] }},
		{"R1", "reply", func(c *config) { c.ioc = true }, func(r *oeRun, j int) bool { return r.res[j] == orderCanceled }},
	} {
		cfg := seededConfig(f.set)
		r := seededRun(cfg, 7)
		rep := r.report()
		rtts, lags := make([][]int64, 4), make([][]int64, 4)
		for j := range r.total {
			if due := r.due(j); f.answered(r, j) && due >= int64(cfg.warmup) && due < int64(cfg.warmup+cfg.duration) {
				k := int((due - int64(cfg.warmup)) / sec)
				rtts[k], lags[k] = append(rtts[k], r.rtt[j]), append(lags[k], r.lag[j])
			}
		}
		for k := range rtts {
			for name, samples := range map[string][]int64{f.row: rtts[k], "send_lag": lags[k]} {
				want, row := summarize(samples), rep.Series.Rows[name]
				if rep.Series.N[k] != want.Count || row.P50[k] != twoDigits(want.P50) || row.P99[k] != twoDigits(want.P99) || row.P999[k] != twoDigits(want.P999) || row.Max[k] != twoDigits(want.Max) {
					t.Errorf("%s window %d of %s: n %d, p50 %v, p99 %v, p99.9 %v, max %v; summarize of its samples says n %d, %v, %v, %v, %v, to two digits",
						f.name, k, name, rep.Series.N[k], row.P50[k], row.P99[k], row.P999[k], row.Max[k], want.Count, want.P50, want.P99, want.P999, want.Max)
				}
			}
		}
	}
}

// The series keeps two significant digits and the report's rows do not: a round
// trip of 87.951 µs is 88 in the series and 87.951 in its row, one of 1234.5 is
// 1200, and a send lag of 12.345 is 12.
func TestSeriesKeepsTwoDigits(t *testing.T) {
	r := newOERun(config{rate: 1000, duration: 2 * time.Second}, discardConn{})
	for j := range r.total {
		r.rtt[j], r.lag[j], r.got[j] = 87_951, 12_345, true
		if j >= 1000 {
			r.rtt[j] = 1_234_500
		}
	}
	rep := r.report()
	if got := rep.Latency["pong"]; got.P50 != 87.951 || got.Max != 1234.5 || rep.Latency["send_lag"].Max != 12.345 {
		t.Fatalf("pong row p50 %v, max %v and send lag max %v, want the exact 87.951, 1234.5 and 12.345", got.P50, got.Max, rep.Latency["send_lag"].Max)
	}
	for _, c := range []struct {
		row  string
		want [2]float64 // by window
	}{{"pong", [2]float64{88, 1200}}, {"send_lag", [2]float64{12, 12}}} {
		row := rep.Series.Rows[c.row]
		for _, got := range [][]float64{row.P50, row.P99, row.P999, row.Max} {
			if len(got) != 2 || got[0] != c.want[0] || got[1] != c.want[1] {
				t.Errorf("series row %s has %v, want %v", c.row, got, c.want)
			}
		}
	}
}

// twoDigits gives the value of two significant digits that is nearest, and the
// even one when two are as near. 9.95 is a little under half way in binary, so it
// is 9.9, where 9950 is half way and is 10,000.
func TestSeriesTwoDigits(t *testing.T) {
	for _, c := range []struct{ v, want float64 }{
		{0, 0}, {87.951, 88}, {1234.5, 1200}, {0.253, 0.25}, {99.6, 100},
		{9.95, 9.9}, {9.96, 10}, {9950, 10_000}, {99.5, 100}, {99.4, 99}, {0.0996, 0.1}, {0.0994, 0.099},
		{10.5, 10}, {-87.951, -88}, {12.345, 12},
	} {
		if got := twoDigits(c.v); got != c.want {
			t.Errorf("twoDigits(%v) = %v, want %v", c.v, got, c.want)
		}
	}
	for _, v := range []float64{10.5, 10.4999, 1.05, 105, 15.5, 99.4999, 0.105} {
		if got := twoDigits(v); math.Abs(got-v) > 0.05*v {
			t.Errorf("twoDigits(%v) = %v, off by more than 5%%", v, got)
		}
	}
}

// The series is written in columns, one array per statistic and as many
// values as windows, with the time the measured part began.
func TestSeriesJSON(t *testing.T) {
	cfg := seededConfig(func(c *config) { c.ioc = true })
	rep := seededRun(cfg, 1).report()
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	var top struct {
		Series map[string]json.RawMessage
	}
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatal(err)
	}
	var start string
	var rows map[string]map[string][]float64
	var n []int
	var window float64
	for key, dst := range map[string]any{"start": &start, "rows": &rows, "n": &n, "window_s": &window} {
		if err := json.Unmarshal(top.Series[key], dst); err != nil {
			t.Fatalf("series.%s in %s: %v", key, top.Series[key], err)
		}
	}
	if len(top.Series) != 4 || window != 1 || start != "2026-11-23T12:00:01.123456789Z" {
		t.Errorf("series keys %d, window_s %v, start %q; want the four keys window_s, start, n and rows, 1 and 2026-11-23T12:00:01.123456789Z", len(top.Series), window, start)
	}
	if len(rows) != 2 {
		t.Errorf("rows %v, want reply and send_lag", rows)
	}
	for name, row := range rows {
		if len(row) != 4 {
			t.Errorf("row %s has the statistics %v, want p50, p99, p99_9 and max", name, row)
		}
		for stat, values := range row {
			if len(values) != len(n) {
				t.Errorf("row %s %s has %d values for %d windows", name, stat, len(values), len(n))
			}
		}
	}
}

// Only the OE1 reports have a series: a report without one, as the REST mode's,
// has no key for it.
func TestSeriesKeyIsLeftOutWithoutASeries(t *testing.T) {
	data, err := json.Marshal(report{Latency: map[string]summary{}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "series") {
		t.Errorf("report %s, want no series", data)
	}
}
