package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds the shared Prometheus collectors used across modules
// (blueprint §4.2).
type Metrics struct {
	registry *prometheus.Registry

	HTTPDuration *prometheus.HistogramVec

	CRLifecycleTransitions *prometheus.CounterVec

	NotificationDispatchLatency prometheus.Histogram

	WebSocketActiveConnections prometheus.Gauge

	NATSConsumerLag *prometheus.GaugeVec

	AuditChainVerifyDuration prometheus.Histogram
}

// NewMetrics registers all shared collectors on a fresh registry.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		registry: reg,
		HTTPDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request latency by route/method/status.",
			Buckets: prometheus.ExponentialBuckets(0.005, 2, 12),
		}, []string{"route", "method", "status"}),
		CRLifecycleTransitions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cr_lifecycle_transitions_total",
			Help: "Change-request lifecycle transitions by target status.",
		}, []string{"status"}),
		NotificationDispatchLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "notification_dispatch_latency_seconds",
			Help:    "End-to-end notification dispatch latency (event receipt to WS push).",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 10),
		}),
		WebSocketActiveConnections: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "websocket_active_connections",
			Help: "Currently open WebSocket connections.",
		}),
		NATSConsumerLag: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "nats_consumer_lag",
			Help: "JetStream consumer pending-message lag by consumer name.",
		}, []string{"consumer"}),
		AuditChainVerifyDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "audit_chain_verify_duration_seconds",
			Help:    "Duration of audit hash-chain integrity verification runs.",
			Buckets: prometheus.ExponentialBuckets(0.1, 2, 10),
		}),
	}
	reg.MustRegister(
		m.HTTPDuration,
		m.CRLifecycleTransitions,
		m.NotificationDispatchLatency,
		m.WebSocketActiveConnections,
		m.NATSConsumerLag,
		m.AuditChainVerifyDuration,
		prometheus.NewGoCollector(),
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
	)
	return m
}

// Registry exposes the underlying Prometheus registry.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// Handler returns the /metrics HTTP handler.
func (m *Metrics) Handler() http.Handler { return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{}) }

// ObserveHTTP records a request latency observation with route/method/status.
func (m *Metrics) ObserveHTTP(route, method string, status int, d time.Duration) {
	m.HTTPDuration.WithLabelValues(route, method, fmt.Sprintf("%d", status)).Observe(d.Seconds())
}

// MetricsMiddleware times each request into HTTPDuration.
func (m *Metrics) MetricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		m.ObserveHTTP(RequestRoutePattern(r), r.Method, rec.status, time.Since(start))
	})
}

// StartConsumerLagPoller periodically records JetStream consumer lag from the
// provided lag function (one gauge per consumer name). It returns a cancel
// func that stops polling.
func (m *Metrics) StartConsumerLagPoller(ctx context.Context, logger *slog.Logger, interval time.Duration, lagFn func() map[string]uint64) context.CancelFunc {
	cctx, cancel := context.WithCancel(ctx)
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-cctx.Done():
				return
			case <-t.C:
				for name, lag := range lagFn() {
					m.NATSConsumerLag.WithLabelValues(name).Set(float64(lag))
				}
			}
		}
	}()
	return cancel
}