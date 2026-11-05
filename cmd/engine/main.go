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
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sHyanorn/matching-engine/internal/api"
	"github.com/sHyanorn/matching-engine/internal/engine"
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
	if err := fs.Parse(args); err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	eng, err := engine.New(engine.Config{
		Symbols:      strings.Split(*symbols, ","),
		DataDir:      *data,
		SyncInterval: *syncInterval,
		QueueSize:    *queueSize,
	})
	if err != nil {
		return err
	}
	srv := api.New(eng, api.Options{Logger: logger})
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
