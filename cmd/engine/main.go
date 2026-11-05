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
)

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
	addr := fs.String("addr", ":8080", "the engine's listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	host, port, err := net.SplitHostPort(*addr)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
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
// WebSocket connections are closed, and the engine drains its queues,
// syncs and closes its logs. Before serving it writes one line,
// "listening on HOST:PORT", to stdout.
func run(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("engine", flag.ContinueOnError)
	addr := fs.String("addr", ":8080", "listen address")
	data := fs.String("data", "data", "directory for the command logs")
	symbols := fs.String("symbols", "BTC-USD,ETH-USD", "comma-separated symbols; their order must not change between runs")
	syncInterval := fs.Duration("sync-interval", 10*time.Millisecond, "how often to fsync the command logs; negative: after every batch")
	queueSize := fs.Int("queue-size", 4096, "capacity of each symbol's command queue")
	shutdownTimeout := fs.Duration("shutdown-timeout", 10*time.Second, "how long to wait for in-flight requests on shutdown")
	pprofAddr := fs.String("pprof", "", "address to serve net/http/pprof on, apart from the API; off if empty")
	if err := fs.Parse(args); err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	fsyncTimes := api.NewFsyncTimes()
	eng, err := engine.New(engine.Config{
		Symbols:      strings.Split(*symbols, ","),
		DataDir:      *data,
		SyncInterval: *syncInterval,
		QueueSize:    *queueSize,
		Logger:       logger,
		OnSync:       fsyncTimes.Observe,
	})
	if err != nil {
		return err
	}
	srv := api.New(eng, api.Options{Logger: logger, FsyncTimes: fsyncTimes})
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return errors.Join(err, eng.Close())
	}
	// The timeouts bound each request, so a stalled client cannot hold a
	// handler or delay shutdown. A WebSocket connection is hijacked, which
	// clears them; ws.go bounds its own writes.
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
			return errors.Join(err, ln.Close(), eng.Close())
		}
		// No write timeout: a CPU profile or trace streams for as long as
		// it was asked to run.
		ps := &http.Server{Handler: pprofHandler(), ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = ps.Serve(pl) }() // ends with ps.Close
		defer func() { _ = ps.Close() }()
		logger.Info("serving pprof", "addr", pl.Addr().String())
	}
	if _, err := fmt.Fprintf(stdout, "listening on %s\n", ln.Addr()); err != nil {
		return errors.Join(err, ln.Close(), eng.Close())
	}
	logger.Info("listening", "addr", ln.Addr().String(), "symbols", *symbols, "data", *data)

	served := make(chan error, 1)
	go func() { served <- hs.Serve(ln) }()
	select {
	case <-ctx.Done():
	case err := <-served: // the listener failed
		return errors.Join(err, eng.Close())
	}

	logger.Info("shutting down")
	srv.BeginShutdown()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), *shutdownTimeout)
	defer cancel()
	errHTTP := hs.Shutdown(shutdownCtx)
	srv.CloseStreams()
	errEngine := eng.Close()
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		errHTTP = errors.Join(errHTTP, err)
	}
	if err := errors.Join(errHTTP, errEngine); err != nil {
		return err
	}
	logger.Info("stopped")
	return nil
}

// pprofHandler serves the net/http/pprof endpoints.
func pprofHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}
