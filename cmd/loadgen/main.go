// Command loadgen offers the SPEC §9 order flow to a running engine at a
// fixed rate and reports the latency distribution (docs/BENCHMARKS.md).
//
// Latency counts from the time each request was due, so it covers the
// whole path from a REST order to the trade push on the WebSocket, and any
// wait for a free connection. A run is valid only if the target rate was
// met and every request succeeded; loadgen exits with an error otherwise.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
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
	fs.Float64Var(&cfg.rate, "rate", 50_000, "requests per second")
	fs.DurationVar(&cfg.warmup, "warmup", 10*time.Second, "how long to run before measuring")
	fs.DurationVar(&cfg.duration, "duration", 30*time.Second, "how long to measure")
	fs.IntVar(&cfg.conns, "conns", 64, "keep-alive connections")
	fs.Uint64Var(&cfg.seed, "seed", 1, "seed of the order flow")
	fs.Float64Var(&cfg.sd, "sd", 5, "standard deviation of prices around the mid, in ticks")
	out := fs.String("out", "", "file to write the JSON report to")
	cpuProfile := fs.String("cpuprofile", "", "file to write loadgen's own CPU profile to")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if cfg.rate <= 0 || cfg.duration <= 0 || cfg.warmup < 0 || cfg.conns <= 0 || cfg.sd < 0 {
		return errors.New("loadgen: -rate, -duration and -conns must be positive, -warmup and -sd not negative")
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
	r, err := measure(ctx, cfg)
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

var latencyRows = []struct{ key, name string }{
	{"trade_push", "order to trade push"},
	{"reply", "request to reply"},
	{"service", "service time"},
	{"send_lag", "send lag"},
	{"hiccup", "loadgen hiccup"},
}

// printReport writes r as a short table.
func printReport(w io.Writer, r report) error {
	keys := make([]string, 0, len(r.Outcomes))
	for k := range r.Outcomes {
		keys = append(keys, fmt.Sprintf("%s %d", k, r.Outcomes[k]))
	}
	slices.Sort(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "%s: target %.0f/s, achieved %.0f/s over %gs after %gs warmup, %d connections\n",
		r.Symbol, r.TargetRate, r.Achieved, r.Duration, r.Warmup, r.Conns)
	fmt.Fprintf(&b, "%s\n", strings.Join(keys, ", "))
	fmt.Fprintf(&b, "%-20s %9s %8s %8s %8s %8s %8s %8s\n", "latency (µs)", "count", "p50", "p90", "p99", "p99.9", "p99.99", "max")
	for _, row := range latencyRows {
		s := r.Latency[row.key]
		fmt.Fprintf(&b, "%-20s %9d %8.0f %8.0f %8.0f %8.0f %8.0f %8.0f\n", row.name, s.Count, s.P50, s.P90, s.P99, s.P999, s.P9999, s.Max)
	}
	if r.Valid {
		b.WriteString("valid\n")
	} else {
		fmt.Fprintf(&b, "invalid: %s\n", strings.Join(r.Problems, "; "))
	}
	_, err := io.WriteString(w, b.String())
	return err
}
