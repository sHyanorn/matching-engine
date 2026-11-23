package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os/exec"
	"slices"
	"strconv"
	"time"
)

// Flow names: F is the Ping floor, R1 to R3 the order flows over OE1, T the
// order flow with its trade pushes, REST the order flow over HTTP.
const (
	flowF    = "F"
	flowR1   = "R1"
	flowR2   = "R2"
	flowR3   = "R3"
	flowT    = "T"
	flowREST = "REST"
)

// Configurations that G2 and the tuning comparisons single out.
const (
	floorConfig  = "F"
	baseConfig   = "R1"
	longConfig   = "R1-15min"
	engineGOGC   = "gogc-off"
	engineSpin   = "spin-pin"
	engineNormal = "default"
)

// params are the knobs of the matrix. The environment overrides them so
// that a rehearsal can run the whole machinery in minutes.
type params struct {
	Rate       float64       // requests per second
	RateHigh   float64       // requests per second of the high-rate flows
	Warmup     time.Duration // discarded at the start of a run
	Duration   time.Duration // measured
	Long       time.Duration // measured, of the one long R1 run
	MaxSendLag time.Duration // largest p99.9 send lag of a valid OE1 run
	Rounds     int
}

var defaultParams = params{
	Rate:       10_000,
	RateHigh:   71_429,
	Warmup:     10 * time.Second,
	Duration:   60 * time.Second,
	Long:       900 * time.Second,
	MaxSendLag: 10 * time.Microsecond,
	Rounds:     5,
}

// paramsFromEnv reads the PILOT_* overrides through getenv. It returns the
// parameters and, for each variable set to a value other than its default,
// the text it was set to; a run with any is a rehearsal. An empty variable
// counts as unset.
func paramsFromEnv(getenv func(string) string) (params, map[string]string, error) {
	p := defaultParams
	changed := map[string]string{}
	var err error
	for _, v := range []struct {
		name string
		set  func(string) (changes bool, err error)
	}{
		{"PILOT_RATE", func(s string) (bool, error) { return setValue(&p.Rate, s, positiveFloat) }},
		{"PILOT_RATE_HIGH", func(s string) (bool, error) { return setValue(&p.RateHigh, s, positiveFloat) }},
		{"PILOT_WARMUP", func(s string) (bool, error) { return setValue(&p.Warmup, s, nonNegativeDuration) }},
		{"PILOT_DURATION", func(s string) (bool, error) { return setValue(&p.Duration, s, positiveDuration) }},
		{"PILOT_LONG", func(s string) (bool, error) { return setValue(&p.Long, s, positiveDuration) }},
		{"PILOT_MAX_SEND_LAG", func(s string) (bool, error) { return setValue(&p.MaxSendLag, s, positiveDuration) }},
		{"PILOT_ROUNDS", func(s string) (bool, error) { return setValue(&p.Rounds, s, positiveInt) }},
	} {
		s := getenv(v.name)
		if s == "" {
			continue
		}
		differs, e := v.set(s)
		if e != nil {
			err = fmt.Errorf("%s=%q: %w", v.name, s, e)
			break
		}
		if differs {
			changed[v.name] = s
		}
	}
	return p, changed, err
}

// setValue parses s into *dst and reports whether that changed it.
func setValue[T comparable](dst *T, s string, parse func(string) (T, error)) (bool, error) {
	v, err := parse(s)
	if err != nil {
		return false, err
	}
	changed := v != *dst
	*dst = v
	return changed, nil
}

func positiveFloat(s string) (float64, error) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || !(v > 0) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("want a positive number")
	}
	return v, nil
}

func positiveInt(s string) (int, error) {
	v, err := strconv.Atoi(s)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("want a positive whole number")
	}
	return v, nil
}

func positiveDuration(s string) (time.Duration, error) {
	v, err := time.ParseDuration(s)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("want a positive duration such as 60s")
	}
	return v, nil
}

func nonNegativeDuration(s string) (time.Duration, error) {
	v, err := time.ParseDuration(s)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("want a duration such as 10s, not negative")
	}
	return v, nil
}

// config is one entry of the matrix: a flow, the engine configuration it
// runs against and the loadgen arguments that make it.
type config struct {
	Name     string   `json:"name"`
	Flow     string   `json:"flow"`
	Engine   string   `json:"engine_config"`
	Rate     float64  `json:"rate"` // loadgen -rate: requests per second, orders per second for R2
	Warmup   seconds  `json:"warmup_s"`
	Duration seconds  `json:"duration_s"` // measured
	Args     []string `json:"args"`       // beyond -rate, -warmup, -duration and -out
}

// seconds is a duration that JSON holds as a number of seconds, as loadgen's
// reports hold theirs.
type seconds time.Duration

func (s seconds) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(s).Seconds()) }

// bounded reports whether a valid run holds send lag p99.9 to the bound. The
// REST mode sends from a pool of connections, not from a pacing thread, so
// its send lag is recorded and not gated (D042).
func (c config) bounded() bool { return c.Flow != flowREST }

// gatesCancelSkips reports whether the run's cancels that were not sent are
// counted against it: the flows of the SPEC §9 mix. F, R1 and R2 send none
// of the mix's cancels that depend on an earlier order's answer.
func (c config) gatesCancelSkips() bool {
	return c.Flow == flowR3 || c.Flow == flowT || c.Flow == flowREST
}

// planConfigs is the table of the matrix: the nine configurations of a
// round in the order they run. oe and http are the engine's OE1 and HTTP
// addresses as the client reaches them.
func planConfigs(p params, oe, http string) []config {
	lag := durationFlag(p.MaxSendLag)
	mk := func(name, flow, engine string, rate float64, args ...string) config {
		return config{
			Name: name, Flow: flow, Engine: engine, Rate: rate,
			Warmup: seconds(p.Warmup), Duration: seconds(p.Duration), Args: slices.Clone(args),
		}
	}
	r1 := []string{"-oe", oe, "-ioc", "-http", http, "-max-send-lag", lag}
	r3 := []string{"-oe", oe, "-orders", "-http", http, "-max-send-lag", lag}
	return []config{
		mk("F", flowF, engineNormal, p.Rate, "-oe", oe, "-max-send-lag", lag),
		mk("R1", flowR1, engineNormal, p.Rate, r1...),
		mk("R1-gogc-off", flowR1, engineGOGC, p.Rate, r1...),
		mk("R1-spin-pin", flowR1, engineSpin, p.Rate, r1...),
		// R2 sends an order and its cancel, so half the orders make R messages a second.
		mk("R2", flowR2, engineNormal, p.Rate/2, "-oe", oe, "-ack-cancel", "-http", http, "-max-send-lag", lag),
		mk("R3-10k", flowR3, engineNormal, p.Rate, r3...),
		mk("R3-71k", flowR3, engineNormal, p.RateHigh, r3...),
		mk("T-71k", flowT, engineNormal, p.RateHigh, "-oe", oe, "-orders", "-push", "-http", http, "-max-send-lag", lag),
		mk("REST-10k", flowREST, engineNormal, p.Rate, "-addr", http),
	}
}

// longRun is the one R1 run with the long measured window, made once after
// the last round if G2 passed.
func longRun(p params, oe, http string) config {
	c := configNamed(planConfigs(p, oe, http), baseConfig)
	c.Name, c.Duration = longConfig, seconds(p.Long)
	return c
}

// configNamed returns the configuration called name.
func configNamed(configs []config, name string) config {
	i := slices.IndexFunc(configs, func(c config) bool { return c.Name == name })
	return configs[i]
}

// loadgenArgs is the full command line of one run of c: the report goes to
// out.
func loadgenArgs(c config, out string) []string {
	return append(slices.Clone(c.Args),
		"-rate", strconv.FormatFloat(c.Rate, 'f', -1, 64),
		"-warmup", durationFlag(time.Duration(c.Warmup)),
		"-duration", durationFlag(time.Duration(c.Duration)),
		"-out", out)
}

// loadgenCommand is the command of a loadgen run. Loadgen's own code reads
// nothing from its environment, so it gets an empty one: this process's would
// hand it whatever the Pod's start command left here. The Go runtime reads
// GOGC, GOMAXPROCS, GODEBUG and GOMEMLIMIT, and with an empty environment it
// stays at its defaults.
func loadgenCommand(ctx context.Context, bin string, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = []string{}
	return cmd
}

// durationFlag writes d the way loadgen's duration flags read it, in
// ASCII: whole seconds, else microseconds, else nanoseconds.
func durationFlag(d time.Duration) string {
	switch {
	case d%time.Second == 0:
		return strconv.FormatInt(int64(d/time.Second), 10) + "s"
	case d%time.Microsecond == 0:
		return strconv.FormatInt(int64(d/time.Microsecond), 10) + "us"
	}
	return strconv.FormatInt(int64(d), 10) + "ns"
}

// minValid is the number of valid runs a configuration needs out of rounds:
// all but one, so that 4 of 5 are enough.
func minValid(rounds int) int { return max(rounds-1, 1) }
