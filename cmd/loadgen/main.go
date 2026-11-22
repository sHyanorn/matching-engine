// Command loadgen offers the SPEC §9 order flow to a running engine at a
// fixed rate and reports the latency distribution (docs/BENCHMARKS.md).
//
// Latency counts from the time each request was due, so it covers the
// whole path from a REST order to the trade push on the WebSocket, and any
// wait for a free connection. A run is valid only if the target rate was
// met, every request succeeded and every trade was pushed once and in
// order (check.go); loadgen exits with an error otherwise.
//
// With -oe it measures the round trip of OE1 Pings instead. With -oe and one
// of three flows it sends orders over one OE1 session and times each from
// its due time to its Done (oe.go): -orders, the same flow (R3); -ioc, IOC
// orders that the engine cancels at once (R1); -ack-cancel, GTC orders that
// are each canceled once their Done is in (R2; oe_flows.go). With -http, the
// engine's HTTP address, a flow also checks the rise of the engine's trades
// counter against the trade Events in the replies, and -orders -push
// subscribes to the trade stream, times from each order's due time to its
// first trade push (T) and checks the pushes as the REST mode does
// (oe_check.go).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"runtime/pprof"
	"slices"
	"strings"
	"syscall"
	"time"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("loadgen", flag.ContinueOnError)
	var cfg config
	fs.StringVar(&cfg.addr, "addr", "localhost:8080", "engine address")
	fs.StringVar(&cfg.symbol, "symbol", "BTC-USD", "symbol to trade")
	fs.Float64Var(&cfg.rate, "rate", 50_000, "requests per second; orders per second with -ioc and -ack-cancel")
	fs.DurationVar(&cfg.warmup, "warmup", 10*time.Second, "how long to run before measuring")
	fs.DurationVar(&cfg.duration, "duration", 30*time.Second, "how long to measure")
	fs.IntVar(&cfg.conns, "conns", 64, "keep-alive connections")
	fs.Uint64Var(&cfg.seed, "seed", 1, "seed of the order flow")
	fs.Float64Var(&cfg.sd, "sd", 5, "standard deviation of prices around the mid, in ticks")
	fs.StringVar(&cfg.oeAddr, "oe", "", "send Pings, or with -orders, -ioc or -ack-cancel orders, over one OE1 session to this address instead of orders over REST")
	fs.BoolVar(&cfg.orders, "orders", false, "with -oe: send the order flow (R3) instead of Pings")
	fs.BoolVar(&cfg.ioc, "ioc", false, "with -oe: send IOC buys that cannot trade, each canceled at once (R1), instead of Pings")
	fs.BoolVar(&cfg.ackCancel, "ack-cancel", false, "with -oe: send GTC buys that cannot trade, each canceled once its Done is in (R2), instead of Pings")
	fs.StringVar(&cfg.httpAddr, "http", "", "with an order flow over -oe: the engine's HTTP address, to check the rise of its trades counter against the replies")
	fs.BoolVar(&cfg.push, "push", false, "with -orders and -http: also time each order's trade push (T) and check the pushes")
	fs.BoolVar(&cfg.spin, "spin", true, "with -oe: spin the pacing thread, and on Linux the reading one, pinned to CPUs, instead of sleeping")
	fs.DurationVar(&cfg.maxSendLag, "max-send-lag", 10*time.Microsecond, "with -oe: the largest p99.9 send lag of a valid run")
	out := fs.String("out", "", "file to write the JSON report to")
	cpuProfile := fs.String("cpuprofile", "", "file to write loadgen's own CPU profile to")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("loadgen: unexpected argument %q, want flags only", fs.Arg(0))
	}
	if err := checkFlows(cfg); err != nil {
		return err
	}
	// The comparisons are written so that NaN fails them.
	requests := cfg.rate * (cfg.warmup + cfg.duration).Seconds()
	if !(cfg.rate > 0) || cfg.duration <= 0 || cfg.warmup < 0 || cfg.conns <= 0 || !(cfg.sd >= 0) || !(requests < math.MaxInt32) {
		return fmt.Errorf("loadgen: -rate %v, -duration %v, -conns %d, -warmup %v, -sd %v (%.0f requests): want -rate, -duration and -conns above 0, -warmup and -sd not below 0, and under 2^31 requests",
			cfg.rate, cfg.duration, cfg.conns, cfg.warmup, cfg.sd, requests)
	}
	if *cpuProfile != "" {
		f, err := os.Create(*cpuProfile)
		if err != nil {
			return err
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			return errors.Join(err, f.Close())
		}
		defer func() {
			pprof.StopCPUProfile()
			_ = f.Close() // a short profile shows when it is read
		}()
	}
	measureFn := measure
	if cfg.oeAddr != "" {
		measureFn = measureOE
	}
	r, err := measureFn(ctx, cfg)
	if err != nil {
		return err
	}
	if err := printReport(stdout, r); err != nil {
		return err
	}
	if *out != "" {
		data, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(*out, append(data, '\n'), 0o644); err != nil {
			return err
		}
	}
	if !r.Valid {
		return fmt.Errorf("loadgen: invalid run: %s", strings.Join(r.Problems, "; "))
	}
	return nil
}

// checkFlows checks the flags that choose what an OE1 session carries and
// what is checked against the engine's HTTP side.
func checkFlows(cfg config) error {
	var flows []string
	for _, f := range []struct {
		name string
		on   bool
	}{{"-orders", cfg.orders}, {"-ioc", cfg.ioc}, {"-ack-cancel", cfg.ackCancel}} {
		if f.on {
			flows = append(flows, f.name)
		}
	}
	switch {
	case len(flows) > 1:
		return fmt.Errorf("loadgen: %s together: want at most one of -orders, -ioc and -ack-cancel", strings.Join(flows, " and "))
	case len(flows) == 1 && cfg.oeAddr == "":
		return fmt.Errorf("loadgen: %s without -oe: the REST mode always sends the order flow", flows[0])
	case cfg.push && !cfg.orders:
		return errors.New("loadgen: -push without -orders: only the order flow trades")
	case cfg.push && cfg.httpAddr == "":
		return errors.New("loadgen: -push without -http: the trade stream is on the engine's HTTP address")
	case cfg.httpAddr != "" && len(flows) == 0:
		return errors.New("loadgen: -http without -orders, -ioc or -ack-cancel: only an order flow over -oe has trades to check")
	}
	return nil
}

var latencyRows = []struct{ key, name string }{
	{"trade_push", "order to trade push"},
	{"pong", "request to pong"},
	{"reply", "request to reply"},
	{"service", "service time"},
	{"cancel", "cancel to reply"},
	{"order_to_cancel", "order to canceled"},
	{"send_lag", "send lag"},
	{"hiccup", "loadgen hiccup"},
}

func printReport(w io.Writer, r report) error {
	counts := make([]string, 0, len(r.Outcomes))
	for k, n := range r.Outcomes {
		counts = append(counts, fmt.Sprintf("%s %d", k, n))
	}
	slices.Sort(counts)
	var b strings.Builder
	title := r.Symbol
	switch r.Mode {
	case "oe":
		title = "OE1 Pings"
	case "oe_orders":
		title = "OE1 orders on " + r.Symbol
	case "oe_ioc":
		title = "OE1 IOC orders on " + r.Symbol
	case "oe_ack_cancel":
		title = "OE1 orders and their cancels on " + r.Symbol
	}
	fmt.Fprintf(&b, "%s: target %.0f/s, achieved %.0f/s over %gs after %gs warmup, ", title, r.TargetRate, r.Achieved, r.Duration, r.Warmup)
	format := "%-20s %9d %8.0f %8.0f %8.0f %8.0f %8.0f %8.0f\n"
	if r.Mode == "" {
		fmt.Fprintf(&b, "%d connections\n", r.Conns)
	} else {
		fmt.Fprintf(&b, "one session, spin %v, reader spins %v, pinned to CPUs %v\n", r.Spin, r.SpinRead, r.Pinned)
		format = "%-20s %9d %8.1f %8.1f %8.1f %8.1f %8.1f %8.1f\n" // round trips of tens of microseconds
	}
	fmt.Fprintf(&b, "%s\n", strings.Join(counts, ", "))
	if r.Mode == "oe_ack_cancel" { // the rate and "achieved" count its orders
		fmt.Fprintf(&b, "cancels sent: %d\n", r.Cancels)
	}
	switch r.Mode {
	case "":
		fmt.Fprintf(&b, "trades: %d in replies, %d pushed, counter rose %d\n", r.Trades.Replies, r.Trades.Pushes, r.Trades.Metric)
	case "oe_orders", "oe_ioc", "oe_ack_cancel":
		// The pushes were read with -push, which has the row trade_push, and
		// the counter with -http, which has a rise unless there were no trades.
		_, pushed := r.Latency["trade_push"]
		fmt.Fprintf(&b, "trades: %d in replies", r.Trades.Replies)
		if pushed {
			fmt.Fprintf(&b, ", %d pushed", r.Trades.Pushes)
		}
		if pushed || r.Trades.Metric != 0 {
			fmt.Fprintf(&b, ", counter rose %d", r.Trades.Metric)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "%-20s %9s %8s %8s %8s %8s %8s %8s\n", "latency (µs)", "count", "p50", "p90", "p99", "p99.9", "p99.99", "max")
	for _, row := range latencyRows {
		s, ok := r.Latency[row.key]
		if !ok {
			continue
		}
		fmt.Fprintf(&b, format, row.name, s.Count, s.P50, s.P90, s.P99, s.P999, s.P9999, s.Max)
	}
	if r.Valid {
		b.WriteString("valid\n")
	} else {
		fmt.Fprintf(&b, "invalid: %s\n", strings.Join(r.Problems, "; "))
	}
	_, err := io.WriteString(w, b.String())
	return err
}
