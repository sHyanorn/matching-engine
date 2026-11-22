// Command engine serves the matching engine over HTTP and WebSocket.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sHyanorn/matching-engine/internal/api"
	"github.com/sHyanorn/matching-engine/internal/engine"
	"github.com/sHyanorn/matching-engine/internal/oe"
	"github.com/sHyanorn/matching-engine/internal/spin"
)

const defaultAddr = ":8080"

func main() {
	var err error
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		err = healthcheck(os.Args[2:])
	} else {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		err = run(ctx, os.Args[1:], os.Stdout)
		stop()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// healthcheck asks the engine listening on -addr for /healthz and fails
// unless it answers 200, for container health checks: the image has no
// shell or curl. A host left empty or unspecified means this machine.
func healthcheck(args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	addr := fs.String("addr", defaultAddr, "the engine's listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("healthcheck: unexpected argument %q, want flags only", fs.Arg(0))
	}
	host, port, err := net.SplitHostPort(*addr)
	if err != nil {
		return err
	}
	if host == "" || net.ParseIP(host).IsUnspecified() {
		host = "127.0.0.1"
	}
	client := http.Client{Timeout: 2 * time.Second}
	u := url.URL{Scheme: "http", Host: net.JoinHostPort(host, port), Path: "/healthz"} // escapes an IPv6 zone
	resp, err := client.Get(u.String())
	if err != nil {
		return err
	}
	_ = resp.Body.Close() // the status is all that is needed
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: /healthz answered %s", resp.Status)
	}
	return nil
}

// run starts the engine and the HTTP server and serves until ctx ends,
// then shuts down gracefully: /healthz answers 503, the server stops
// accepting and finishes in-flight requests (up to -shutdown-timeout),
// WebSocket connections and OE1 sessions are closed, and the engine drains
// its queues, syncs and closes its logs. Before serving it writes one line,
// "listening on HOST:PORT", to stdout, and with -oe-addr a second one,
// "oe listening on HOST:PORT".
func run(ctx context.Context, args []string, stdout io.Writer) (err error) {
	fs := flag.NewFlagSet("engine", flag.ContinueOnError)
	addr := fs.String("addr", defaultAddr, "listen address")
	data := fs.String("data", "data", "directory for the command logs")
	symbols := fs.String("symbols", "BTC-USD,ETH-USD", "comma-separated symbols; their order must not change between runs")
	syncInterval := fs.Duration("sync-interval", engine.DefaultSyncInterval, "how often to fsync the command logs; negative: after every batch")
	queueSize := fs.Int("queue-size", engine.DefaultQueueSize, "capacity of each symbol's command queue")
	shutdownTimeout := fs.Duration("shutdown-timeout", 10*time.Second, "how long to wait for in-flight requests on shutdown")
	pprofAddr := fs.String("pprof", "", "address to serve net/http/pprof on, apart from the API; off if empty")
	oeAddr := fs.String("oe-addr", "", "address to serve OE1 binary order entry on (docs/API.md); off if empty")
	oeSpin := fs.Bool("oe-spin", false, "spin, lock and pin the OE1 path, Linux only, needs -oe-addr: each symbol's goroutine and each session's reader and writer "+
		"runs locked to an OS thread pinned to a CPU of its own, one per physical core, and polls instead of blocking. "+
		"Cores go to the symbols in order at start, then to each session's reader and writer as it logs in, at most GOMAXPROCS-1 of them; "+
		"a goroutine that finds none free runs as usual, and a session's cores are freed when its connection ends")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("engine: unexpected argument %q, want flags only (healthcheck only as the first argument)", fs.Arg(0))
	}
	if *oeSpin && *oeAddr == "" {
		return errors.New("engine: -oe-spin needs -oe-addr: it spins the OE1 path, which is off without it")
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	var spinPool *spin.Pool // nil without -oe-spin
	if *oeSpin {
		if spinPool, err = spin.New(logger); err != nil {
			return fmt.Errorf("engine: -oe-spin: %w", err)
		}
	}

	fsyncTimes := api.NewFsyncTimes()
	eng, err := engine.New(engine.Config{
		Symbols:      strings.Split(*symbols, ","),
		DataDir:      *data,
		SyncInterval: *syncInterval,
		QueueSize:    *queueSize,
		Logger:       logger,
		OnSync:       fsyncTimes.Observe,
		Spin:         spinPool,
	})
	if err != nil {
		return err
	}
	// Last on every path: the engine closes once nothing can reach it.
	defer func() {
		if err = errors.Join(err, eng.Close()); err == nil {
			logger.Info("stopped")
		}
	}()
	srv := api.New(eng, api.Options{Logger: logger, FsyncTimes: fsyncTimes})
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	// The timeouts bound each request, so a stalled client cannot hold a
	// handler or delay shutdown. An order or cancel restarts the write
	// timeout once its result is ready, as a queued command is applied
	// however long it waited (D020). A WebSocket connection is hijacked,
	// which clears the timeouts; ws.go bounds its own writes.
	hs := &http.Server{
		Handler:        srv.Handler(),
		ReadTimeout:    5 * time.Second,
		WriteTimeout:   5 * time.Second,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 16 << 10,
	}
	if *pprofAddr != "" {
		pl, err := net.Listen("tcp", *pprofAddr)
		if err != nil {
			return errors.Join(err, ln.Close())
		}
		// No write timeout: a CPU profile or trace streams for as long as
		// it was asked to run.
		ps := &http.Server{Handler: pprofHandler(), ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = ps.Serve(pl) }() // ends with ps.Close
		defer func() { _ = ps.Close() }()
		logger.Info("serving pprof", "addr", pl.Addr().String())
	}
	var oeLn net.Listener
	var oeSrv *oe.Server
	if *oeAddr != "" {
		if oeLn, err = net.Listen("tcp", *oeAddr); err != nil {
			return errors.Join(err, ln.Close())
		}
		oeSrv = oe.NewServer(oe.ServerOptions{Engine: eng, Counts: srv, Symbols: eng.Symbols(), Logger: logger, Spin: spinPool})
		go func() {
			if err := oeSrv.Serve(oeLn); err != nil {
				logger.Error("oe listener failed", "err", err)
			}
		}()
		defer func() { _ = oeSrv.Close() }() // before the engine closes
	}
	if _, err := fmt.Fprintf(stdout, "listening on %s\n", ln.Addr()); err != nil {
		return errors.Join(err, ln.Close())
	}
	if oeLn != nil {
		if _, err := fmt.Fprintf(stdout, "oe listening on %s\n", oeLn.Addr()); err != nil {
			return errors.Join(err, ln.Close())
		}
		logger.Info("serving oe", "addr", oeLn.Addr().String())
	}
	logger.Info("listening", "addr", ln.Addr().String(), "symbols", *symbols, "data", *data)

	served := make(chan error, 1)
	go func() { served <- hs.Serve(ln) }()
	select {
	case <-ctx.Done():
	case err := <-served: // the listener failed
		return err
	}

	logger.Info("shutting down")
	srv.BeginShutdown()
	if oeSrv != nil {
		oeSrv.BeginShutdown()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), *shutdownTimeout)
	defer cancel()
	err = hs.Shutdown(shutdownCtx)
	srv.CloseStreams()
	if errServe := <-served; !errors.Is(errServe, http.ErrServerClosed) {
		err = errors.Join(err, errServe)
	}
	return err
}

func pprofHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}
