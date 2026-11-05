// Package api serves the engine over HTTP and WebSocket, as described in
// docs/API.md.
package api

import (
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/sHyanorn/matching-engine/internal/engine"
)

// Options configures a Server.
type Options struct {
	// SubscriberBuffer is the capacity of each WebSocket channel's engine
	// subscription (default 1024). A client that falls this many updates
	// behind is disconnected.
	SubscriberBuffer int
	// Logger receives connection-level logs (default: discarded).
	Logger *slog.Logger
}

// Server holds the HTTP handlers. Its methods are safe for concurrent use.
type Server struct {
	eng      *engine.Engine
	opts     Options
	mux      *http.ServeMux
	metrics  *metrics
	stopping atomic.Bool

	mu      sync.Mutex
	streams map[*stream]struct{}
	wg      sync.WaitGroup // running stream handlers
}

const defaultSubscriberBuffer = 1024

// New returns a Server for eng.
func New(eng *engine.Engine, opts Options) *Server {
	if opts.SubscriberBuffer <= 0 {
		opts.SubscriberBuffer = defaultSubscriberBuffer
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &Server{
		eng:     eng,
		opts:    opts,
		mux:     http.NewServeMux(),
		metrics: newMetrics(eng),
		streams: make(map[*stream]struct{}),
	}
	s.handle("POST /v1/orders", s.placeOrder)
	s.handle("DELETE /v1/orders/{id}", s.cancelOrder)
	s.handle("GET /v1/orders/{id}", s.getOrder)
	s.handle("GET /v1/books/{symbol}", s.getBook)
	s.handle("GET /healthz", s.health)
	s.mux.Handle("GET /metrics", promhttp.HandlerFor(s.metrics.reg, promhttp.HandlerOpts{}))
	s.mux.HandleFunc("GET /v1/stream", s.stream) // long-lived: not timed
	return s
}

// Handler returns the handler for every route.
func (s *Server) Handler() http.Handler { return s.mux }

// BeginShutdown makes /healthz answer 503 and refuses new WebSocket
// connections, so a load balancer stops sending traffic.
func (s *Server) BeginShutdown() { s.stopping.Store(true) }

// CloseStreams closes every open WebSocket connection with code 1001 and
// waits for their handlers to finish.
func (s *Server) CloseStreams() {
	s.mu.Lock()
	open := make([]*stream, 0, len(s.streams))
	for st := range s.streams {
		open = append(open, st)
	}
	s.mu.Unlock()
	for _, st := range open {
		st.close(1001, "shutting down")
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

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	if s.stopping.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "shutting_down"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
