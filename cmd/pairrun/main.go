// Command pairrun runs the pilot matrix of docs/TASKS.md M8 on a pair of
// rented machines (D043): `pairrun supervise` on the engine's machine and
// `pairrun client` on the other.
//
// The supervisor starts the engine, fresh and in a configuration the client
// names, whenever the client asks, over a control port. Before it says it is
// ready it measures the engine and loadgen on its own machine, over the
// loopback. The client runs loadgen through the matrix of five rounds of nine
// configurations, decides for each run whether it is valid by the study's own
// rules, computes the gate G2 after every round and writes every run, with
// loadgen's numbers as they came, to its results directory (plan.go,
// validity.go, g2.go, summary.go). Each run's results also hold the engine's
// own output for it, bounded in size and without network addresses, and each
// run's record the change in both machines' CPU scheduling counters over the
// run (counters.go), which tells a wait for a CPU from a stall of the host.
// Once it is ready the supervisor also times, second by second, how late a
// thread that sleeps 1 ms is woken on its machine, and the client keeps those
// seconds for the measured part of each run that has a loadgen series, with the
// offset between the two clocks (wake.go).
// `pairrun slim` copies a results directory without the parts that the Pod's
// log has no room for (slim.go).
//
// The control port has no authentication: the supervisor serves a fixed set of
// routes, none of which takes a path, and runs nothing the client names but a
// configuration of its own table.
// scripts/pair.sh starts it without the Pod's API key, and it starts the
// engine and loadgen with environments of their own, not its own. It runs on
// Linux.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"
)

const usage = `usage:
  pairrun supervise -engine BIN -loadgen BIN -work DIR [-listen :9100] [-env FILE]
  pairrun client -loadgen BIN -oe HOST:PORT -http HOST:PORT -ctl HOST:PORT -out DIR [-deadline UNIXSECONDS] [-deadline-margin 10m]
  pairrun slim -in DIR -out DIR`

// The main goroutine keeps the main thread for the life of the process, so that
// no other goroutine is run there. The main thread does not end when a goroutine
// locked to it does, and a pin put on it would show as the process's CPUs in
// /proc/self/status, which names the main thread. A call to LockOSThread from
// an init function locks the main goroutine to the main thread.
func init() {
	runtime.LockOSThread()
}

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
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "supervise":
		return runSupervise(ctx, args[1:], stdout)
	case "client":
		return runClient(ctx, args[1:], stdout)
	case "slim":
		return runSlim(args[1:])
	}
	return fmt.Errorf("pairrun: unknown command %q\n%s", args[0], usage)
}

func runSupervise(ctx context.Context, args []string, stdout io.Writer) error {
	o, err := superviseOptionsFromArgs(args)
	if err != nil {
		return err
	}
	return supervise(ctx, o, stdout)
}

// superviseOptionsFromArgs reads the flags of `pairrun supervise` and the
// PILOT_* overrides of the environment.
func superviseOptionsFromArgs(args []string) (superviseOptions, error) {
	fs := flag.NewFlagSet("supervise", flag.ContinueOnError)
	var o superviseOptions
	fs.StringVar(&o.Engine, "engine", "", "the engine binary")
	fs.StringVar(&o.Loadgen, "loadgen", "", "the loadgen binary, for the loopback run")
	fs.StringVar(&o.Listen, "listen", ":9100", "control address")
	fs.StringVar(&o.Work, "work", "", "directory for engine data, engine logs and the loopback files")
	fs.StringVar(&o.EnvFile, "env", "", "file of text that describes this machine, served to the client for its results")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("pairrun supervise: unexpected argument %q, want flags only", fs.Arg(0))
	}
	if o.Engine == "" || o.Loadgen == "" || o.Work == "" {
		return o, fmt.Errorf("pairrun supervise: -engine, -loadgen and -work are required\n%s", usage)
	}
	var err error
	o.Params, o.Overrides, err = paramsFromEnv(os.Getenv)
	return o, err
}

func runClient(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("client", flag.ContinueOnError)
	var o clientOptions
	fs.StringVar(&o.Loadgen, "loadgen", "", "the loadgen binary")
	fs.StringVar(&o.OE, "oe", "", "the engine's OE1 address, HOST:PORT")
	fs.StringVar(&o.HTTP, "http", "", "the engine's HTTP address, HOST:PORT")
	fs.StringVar(&o.Ctl, "ctl", "", "the supervisor's control address, HOST:PORT")
	fs.StringVar(&o.Out, "out", "", "results directory")
	deadline := fs.Int64("deadline", 0, "Unix time in seconds by which the Pod is gone; no run is started that would not end before it, with the margin")
	fs.DurationVar(&o.Margin, "deadline-margin", 10*time.Minute, "time to leave before the deadline for the end of the last run")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("pairrun client: unexpected argument %q, want flags only", fs.Arg(0))
	}
	if o.Loadgen == "" || o.OE == "" || o.HTTP == "" || o.Ctl == "" || o.Out == "" {
		return fmt.Errorf("pairrun client: -loadgen, -oe, -http, -ctl and -out are required\n%s", usage)
	}
	if *deadline != 0 {
		o.Deadline = time.Unix(*deadline, 0)
	}
	var err error
	if o.Params, o.Overrides, err = paramsFromEnv(os.Getenv); err != nil {
		return err
	}
	return newClient(o, stdout).run(ctx)
}
