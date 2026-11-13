// Package api serves the engine over HTTP and WebSocket, as described in
// docs/API.md.
package api

import (
	"embed"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/sHyanorn/matching-engine/internal/engine"
)

// Options configures a Server.
type Options struct {
	// SubscriberBuffer is the capacity of each WebSocket channel's engine
	// subscription (default: the engine's, 1024). A client that falls this
	// many updates behind is disconnected.
	SubscriberBuffer int
	// WriteTimeout bounds writing one WebSocket message (default 10s); a
	// client that takes longer is disconnected as a slow consumer.
	WriteTimeout time.Duration
	// Logger receives connection-level logs (default: discarded).
	Logger *slog.Logger
	// FsyncTimes, if set, is served on /metrics; the engine feeds it.
	FsyncTimes *FsyncTimes
}

// Server holds the HTTP handlers. Its methods are safe for concurrent use.
type Server struct {
	eng      *engine.Engine
	opts     Options
	mux      *http.ServeMux
	metrics  *metrics
	symbols  map[string]bool // configured symbols: the only metric labels
	stopping atomic.Bool

	mu      sync.Mutex
	streams map[*stream]struct{}
	wg      sync.WaitGroup // running stream handlers
}

const defaultWriteTimeout = 10 * time.Second

// New returns a Server for eng.
func New(eng *engine.Engine, opts Options) *Server {
	if opts.WriteTimeout <= 0 {
		opts.WriteTimeout = defaultWriteTimeout
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	s := &Server{
		eng:     eng,
		opts:    opts,
		mux:     http.NewServeMux(),
		metrics: newMetrics(eng, opts.FsyncTimes),
		symbols: make(map[string]bool),
		streams: make(map[*stream]struct{}),
	}
	for _, sym := range eng.Symbols() {
		s.symbols[sym] = true
	}
	s.handle("POST /v1/orders", s.placeOrder)
	s.handle("DELETE /v1/orders/{id}", s.cancelOrder)
	s.handle("GET /v1/orders/{id}", s.getOrder)
	s.handle("GET /v1/books/{symbol}", s.getBook)
	s.handle("GET /healthz", s.health)
	s.mux.Handle("GET /metrics", promhttp.HandlerFor(s.metrics.reg, promhttp.HandlerOpts{}))
	s.mux.HandleFunc("GET /v1/stream", s.stream) // long-lived: not timed
	s.mux.HandleFunc("GET /{$}", page)
	return s
}

// Handler returns the handler for every route.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := s.mux.Handler(r); pattern == "" {
			w = &muxError{w} // no route: the mux answers 404 or 405 itself
		}
		s.mux.ServeHTTP(w, r)
	})
}

// muxError turns the mux's plain-text 404 and 405 replies into the error
// shape, keeping headers it set such as Allow.
type muxError struct{ http.ResponseWriter }

func (e *muxError) WriteHeader(code int) {
	errCode := "not_found"
	if code == http.StatusMethodNotAllowed {
		errCode = "method_not_allowed"
	}
	writeError(e.ResponseWriter, code, errCode, strings.ToLower(http.StatusText(code)))
}

func (e *muxError) Write(p []byte) (int, error) { return len(p), nil } // drops the plain-text body

// BeginShutdown makes /healthz answer 503, and new orders, cancels and
// WebSocket connections 503 shutting_down, so a load balancer stops
// sending traffic and nothing is applied whose reply could be lost. Call
// it before http.Server.Shutdown: a request on a connection that Shutdown
// closes as idle is then refused rather than applied.
func (s *Server) BeginShutdown() { s.stopping.Store(true) }

// CloseStreams closes every open WebSocket connection with code 1001 and
// waits for their handlers to finish.
func (s *Server) CloseStreams() {
	s.mu.Lock()
	open := slices.Collect(maps.Keys(s.streams))
	s.mu.Unlock()
	for _, st := range open {
		st.close(shuttingDown)
	}
	s.wg.Wait()
}

// handle registers h for pattern and records its duration by route and
// status code.
func (s *Server) handle(pattern string, h http.HandlerFunc) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		h(rec, r)
		s.metrics.latency.WithLabelValues(pattern, strconv.Itoa(rec.code)).Observe(time.Since(start).Seconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the connection.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

type healthBody struct {
	Status         string   `json:"status"`
	LogFailed      []string `json:"log_failed,omitempty"`
	OrderIDsFailed bool     `json:"order_ids_failed,omitempty"`
}

// health reports shutdown with 503. A symbol whose log failed, or failing
// order-ID reservations, make the status "degraded" but keep 200: cancels
// and queries, and the other symbols, still serve (D029).
func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	if s.stopping.Load() {
		writeJSON(w, http.StatusServiceUnavailable, healthBody{Status: "shutting_down"})
		return
	}
	b := healthBody{Status: "ok"}
	for _, sym := range s.eng.Symbols() {
		if s.eng.LogErr(sym) != nil {
			b.LogFailed = append(b.LogFailed, sym)
		}
	}
	b.OrderIDsFailed = s.eng.ReservationErr() != nil
	if b.LogFailed != nil || b.OrderIDsFailed {
		b.Status = "degraded"
	}
	writeJSON(w, http.StatusOK, b)
}

//go:embed web/index.html
var web embed.FS

// page serves the order book page, which follows one symbol over the
// stream (?symbol=, default BTC-USD).
func page(w http.ResponseWriter, r *http.Request) {
	http.ServeFileFS(w, r, web, "web/index.html")
}
