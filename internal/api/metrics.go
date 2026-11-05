package api

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/sHyanorn/matching-engine/internal/engine"
)

// metrics are the Prometheus metrics of docs/API.md, in a registry of
// their own. Labels take only configured symbols, never client input.
type metrics struct {
	reg      *prometheus.Registry
	orders   *prometheus.CounterVec
	cancels  *prometheus.CounterVec
	trades   *prometheus.CounterVec
	latency  *prometheus.HistogramVec
	streams  prometheus.Gauge
	wsClosed *prometheus.CounterVec
}

var (
	orderResults  = []string{"accepted", "rejected", "invalid", "busy", "failed"}
	cancelResults = []string{"canceled", "unknown", "finished", "busy", "failed"}
)

func newMetrics(eng *engine.Engine, fsync *FsyncTimes) *metrics {
	m := &metrics{
		reg: prometheus.NewRegistry(),
		orders: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "matching_engine_orders_total", Help: "New orders by outcome.",
		}, []string{"symbol", "result"}),
		cancels: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "matching_engine_cancels_total", Help: "Cancel requests by outcome.",
		}, []string{"symbol", "result"}),
		trades: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "matching_engine_trades_total", Help: "Executions.",
		}, []string{"symbol"}),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "matching_engine_http_request_duration_seconds", Help: "HTTP request handling time.",
			Buckets: prometheus.ExponentialBuckets(0.00005, 2, 16), // 50µs to about 1.6s
		}, []string{"route", "code"}),
		streams: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "matching_engine_ws_connections", Help: "Open WebSocket connections.",
		}),
		wsClosed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "matching_engine_ws_disconnects_total", Help: "Closed WebSocket connections by cause.",
		}, []string{"reason"}),
	}
	m.reg.MustRegister(m.orders, m.cancels, m.trades, m.latency, m.streams, m.wsClosed,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	for _, c := range closeCauses {
		m.wsClosed.WithLabelValues(c.label)
	}
	if fsync != nil {
		m.reg.MustRegister(fsync.h)
		for _, s := range eng.Symbols() {
			fsync.h.WithLabelValues(s)
		}
	}
	for _, s := range eng.Symbols() {
		// Every series exists from the start, at 0.
		for _, r := range orderResults {
			m.orders.WithLabelValues(s, r)
		}
		for _, r := range cancelResults {
			m.cancels.WithLabelValues(s, r)
		}
		m.trades.WithLabelValues(s)
		m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "matching_engine_queue_length", Help: "Requests waiting in a symbol's queue.",
			ConstLabels: prometheus.Labels{"symbol": s},
		}, func() float64 {
			n, _ := eng.QueueLen(s)
			return float64(n)
		}), prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "matching_engine_log_failed", Help: "1 once a symbol's command log has failed; it then accepts no commands.",
			ConstLabels: prometheus.Labels{"symbol": s},
		}, func() float64 {
			if eng.LogErr(s) != nil {
				return 1
			}
			return 0
		}))
	}
	return m
}

// FsyncTimes is a histogram of how long the fsyncs of each symbol's command
// log take. Pass its Observe as engine.Config.OnSync and the FsyncTimes as
// Options.FsyncTimes, and /metrics serves it.
type FsyncTimes struct{ h *prometheus.HistogramVec }

// NewFsyncTimes returns an empty FsyncTimes.
func NewFsyncTimes() *FsyncTimes {
	return &FsyncTimes{h: prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "matching_engine_log_fsync_duration_seconds", Help: "Time to fsync a symbol's command log.",
		Buckets: prometheus.ExponentialBuckets(0.00005, 2, 16), // 50µs to about 1.6s
	}, []string{"symbol"})}
}

// Observe records one fsync of symbol's log. It is safe for concurrent use.
func (f *FsyncTimes) Observe(symbol string, took time.Duration) {
	f.h.WithLabelValues(symbol).Observe(took.Seconds())
}
