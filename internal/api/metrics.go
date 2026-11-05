package api

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/sHyanorn/matching-engine/internal/engine"
)

// metrics are the Prometheus metrics of docs/API.md, in a registry of
// their own. Labels take only configured symbols, never client input.
type metrics struct {
	reg     *prometheus.Registry
	orders  *prometheus.CounterVec
	cancels *prometheus.CounterVec
	trades  *prometheus.CounterVec
	latency *prometheus.HistogramVec
	streams prometheus.Gauge
}

var (
	orderResults  = []string{"accepted", "rejected", "invalid", "busy", "failed"}
	cancelResults = []string{"canceled", "unknown", "finished", "invalid", "busy", "failed"}
)

func newMetrics(eng *engine.Engine) *metrics {
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
	}
	m.reg.MustRegister(m.orders, m.cancels, m.trades, m.latency, m.streams)
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
		}))
	}
	return m
}
