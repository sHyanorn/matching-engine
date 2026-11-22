package main

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestPlanConfigs(t *testing.T) {
	const oe, http = "10.0.0.1:9000", "10.0.0.1:8080"
	want := []struct {
		name, flow, engine string
		rate               float64
		args               string
	}{
		{"F", "F", "default", 10000, "-oe " + oe + " -max-send-lag 10us"},
		{"R1", "R1", "default", 10000, "-oe " + oe + " -ioc -http " + http + " -max-send-lag 10us"},
		{"R1-gogc-off", "R1", "gogc-off", 10000, "-oe " + oe + " -ioc -http " + http + " -max-send-lag 10us"},
		{"R1-spin-pin", "R1", "spin-pin", 10000, "-oe " + oe + " -ioc -http " + http + " -max-send-lag 10us"},
		{"R2", "R2", "default", 5000, "-oe " + oe + " -ack-cancel -http " + http + " -max-send-lag 10us"},
		{"R3-10k", "R3", "default", 10000, "-oe " + oe + " -orders -http " + http + " -max-send-lag 10us"},
		{"R3-71k", "R3", "default", 71429, "-oe " + oe + " -orders -http " + http + " -max-send-lag 10us"},
		{"T-71k", "T", "default", 71429, "-oe " + oe + " -orders -push -http " + http + " -max-send-lag 10us"},
		{"REST-10k", "REST", "default", 10000, "-addr " + http},
	}
	got := planConfigs(defaultParams, oe, http)
	if len(got) != len(want) {
		t.Fatalf("%d configurations, want %d", len(got), len(want))
	}
	for i, w := range want {
		g := got[i]
		if g.Name != w.name || g.Flow != w.flow || g.Engine != w.engine || g.Rate != w.rate || strings.Join(g.Args, " ") != w.args {
			t.Errorf("configuration %d is %s %s %s %g %q, want %s %s %s %g %q",
				i+1, g.Name, g.Flow, g.Engine, g.Rate, strings.Join(g.Args, " "), w.name, w.flow, w.engine, w.rate, w.args)
		}
		if time.Duration(g.Warmup) != 10*time.Second || time.Duration(g.Duration) != 60*time.Second {
			t.Errorf("%s runs %v after %v, want 60s after 10s", g.Name, time.Duration(g.Duration), time.Duration(g.Warmup))
		}
	}
}

func TestPlanRates(t *testing.T) {
	p := defaultParams
	p.Rate, p.RateHigh = 1001, 4004
	for _, c := range planConfigs(p, "oe", "http") {
		want := p.Rate
		switch c.Name {
		case "R3-71k", "T-71k":
			want = p.RateHigh
		case "R2":
			want = p.Rate / 2
		}
		if c.Rate != want {
			t.Errorf("%s offers %g/s, want %g", c.Name, c.Rate, want)
		}
	}
}

func TestPlanArgsDoNotShareStorage(t *testing.T) {
	// R1 and its variants are made from one list; changing one must not change the others.
	cs := planConfigs(defaultParams, "oe", "http")
	cs[1].Args[1] = "changed"
	if cs[2].Args[1] != "oe" || cs[3].Args[1] != "oe" {
		t.Errorf("R1-gogc-off args %v and R1-spin-pin args %v changed with R1's", cs[2].Args, cs[3].Args)
	}
}

func TestLongRun(t *testing.T) {
	p := defaultParams
	long := longRun(p, "oe", "http")
	r1 := configNamed(planConfigs(p, "oe", "http"), "R1")
	if long.Name != "R1-15min" || long.Flow != "R1" || long.Engine != "default" || long.Rate != p.Rate {
		t.Errorf("long run is %+v", long)
	}
	if time.Duration(long.Duration) != 900*time.Second || time.Duration(long.Warmup) != 10*time.Second {
		t.Errorf("long run measures %v after %v, want 15m0s after 10s", time.Duration(long.Duration), time.Duration(long.Warmup))
	}
	if !slices.Equal(long.Args, r1.Args) {
		t.Errorf("long run args %v differ from R1's %v", long.Args, r1.Args)
	}
	if time.Duration(r1.Duration) != 60*time.Second {
		t.Errorf("making the long run changed R1's duration to %v", time.Duration(r1.Duration))
	}
}

func TestLoadgenArgs(t *testing.T) {
	c := planConfigs(defaultParams, "oe:9000", "http:8080")[4] // R2
	got := loadgenArgs(c, "/out/run.json")
	want := []string{"-oe", "oe:9000", "-ack-cancel", "-http", "http:8080", "-max-send-lag", "10us",
		"-rate", "5000", "-warmup", "10s", "-duration", "60s", "-out", "/out/run.json"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("args %v, want %v", got, want)
	}
	if !slices.Equal(c.Args, want[:7]) {
		t.Errorf("building the command line changed the plan's args to %v", c.Args)
	}
}

func TestDurationFlag(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{10 * time.Second, "10s"},
		{15 * time.Minute, "900s"},
		{10 * time.Microsecond, "10us"},
		{1500 * time.Millisecond, "1500000us"},
		{1234567, "1234567ns"},
	} {
		if got := durationFlag(c.d); got != c.want {
			t.Errorf("durationFlag(%v) = %q, want %q", c.d, got, c.want)
		}
		if back, err := time.ParseDuration(durationFlag(c.d)); err != nil || back != c.d {
			t.Errorf("durationFlag(%v) = %q reads back as %v, %v", c.d, durationFlag(c.d), back, err)
		}
	}
}

func TestFlowGates(t *testing.T) {
	for _, c := range []struct {
		flow          string
		bounded, skip bool
	}{
		{flowF, true, false}, {flowR1, true, false}, {flowR2, true, false},
		{flowR3, true, true}, {flowT, true, true}, {flowREST, false, true},
	} {
		cfg := config{Flow: c.flow}
		if cfg.bounded() != c.bounded || cfg.gatesCancelSkips() != c.skip {
			t.Errorf("%s: send lag bounded %v, cancel skips gated %v, want %v and %v",
				c.flow, cfg.bounded(), cfg.gatesCancelSkips(), c.bounded, c.skip)
		}
	}
}

func TestParamsFromEnv(t *testing.T) {
	env := func(kv ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}

	t.Run("defaults", func(t *testing.T) {
		p, changed, err := paramsFromEnv(env())
		if err != nil || p != defaultParams || len(changed) != 0 {
			t.Errorf("got %+v, %v, %v", p, changed, err)
		}
		d := defaultParams
		if d.Rate != 10000 || d.RateHigh != 71429 || d.Warmup != 10*time.Second || d.Duration != 60*time.Second ||
			d.Long != 900*time.Second || d.MaxSendLag != 10*time.Microsecond || d.Rounds != 5 {
			t.Errorf("defaults are %+v", d)
		}
	})
	t.Run("each override differs", func(t *testing.T) {
		for _, c := range []struct {
			name, value string
			check       func(params) bool
		}{
			{"PILOT_RATE", "2000", func(p params) bool { return p.Rate == 2000 }},
			{"PILOT_RATE_HIGH", "3000.5", func(p params) bool { return p.RateHigh == 3000.5 }},
			{"PILOT_WARMUP", "0s", func(p params) bool { return p.Warmup == 0 }},
			{"PILOT_DURATION", "2s", func(p params) bool { return p.Duration == 2*time.Second }},
			{"PILOT_LONG", "5s", func(p params) bool { return p.Long == 5*time.Second }},
			{"PILOT_MAX_SEND_LAG", "20us", func(p params) bool { return p.MaxSendLag == 20*time.Microsecond }},
			{"PILOT_ROUNDS", "2", func(p params) bool { return p.Rounds == 2 }},
		} {
			p, changed, err := paramsFromEnv(env(c.name, c.value))
			if err != nil || !c.check(p) || !reflect.DeepEqual(changed, map[string]string{c.name: c.value}) {
				t.Errorf("%s=%s: got %+v, %v, %v", c.name, c.value, p, changed, err)
			}
		}
	})
	t.Run("a value equal to its default is no override", func(t *testing.T) {
		_, changed, err := paramsFromEnv(env(
			"PILOT_RATE", "10000", "PILOT_RATE_HIGH", "71429.0", "PILOT_WARMUP", "10s", "PILOT_DURATION", "1m",
			"PILOT_LONG", "15m", "PILOT_MAX_SEND_LAG", "10000ns", "PILOT_ROUNDS", "5"))
		if err != nil || len(changed) != 0 {
			t.Errorf("changed %v, %v, want none", changed, err)
		}
	})
	t.Run("empty is unset", func(t *testing.T) {
		_, changed, err := paramsFromEnv(env("PILOT_RATE", ""))
		if err != nil || len(changed) != 0 {
			t.Errorf("changed %v, %v, want none", changed, err)
		}
	})
	t.Run("bad values", func(t *testing.T) {
		for _, c := range [][2]string{
			{"PILOT_RATE", "0"}, {"PILOT_RATE", "-5"}, {"PILOT_RATE", "fast"}, {"PILOT_RATE", "NaN"}, {"PILOT_RATE_HIGH", "Inf"},
			{"PILOT_WARMUP", "-1s"}, {"PILOT_WARMUP", "10"}, {"PILOT_DURATION", "0s"}, {"PILOT_LONG", "0"},
			{"PILOT_MAX_SEND_LAG", "0s"}, {"PILOT_ROUNDS", "0"}, {"PILOT_ROUNDS", "2.5"},
		} {
			if _, _, err := paramsFromEnv(env(c[0], c[1])); err == nil || !strings.Contains(err.Error(), c[0]) {
				t.Errorf("%s=%s: error %v, want one that names the variable", c[0], c[1], err)
			}
		}
	})
}

func TestMinValid(t *testing.T) {
	for rounds, want := range map[int]int{1: 1, 2: 1, 3: 2, 5: 4, 6: 5} {
		if got := minValid(rounds); got != want {
			t.Errorf("minValid(%d) = %d, want %d", rounds, got, want)
		}
	}
}
