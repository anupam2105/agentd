// Package observability exposes Prometheus metrics used across the process.
package observability

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds the registered Prometheus collectors and the private registry
// they belong to. A private registry keeps tests isolated.
type Metrics struct {
	registry *prometheus.Registry

	HTTPRequestsTotal   *prometheus.CounterVec
	HTTPRequestDuration *prometheus.HistogramVec
	BuildInfo           *prometheus.GaugeVec
}

// NewMetrics constructs the registry with Go runtime + process collectors and
// the HTTP request instruments.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	m := &Metrics{
		registry: reg,
		HTTPRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "agentd", Subsystem: "http", Name: "requests_total",
			Help: "Total HTTP requests, labelled by method, route and status.",
		}, []string{"method", "route", "status"}),
		HTTPRequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "agentd", Subsystem: "http", Name: "request_duration_seconds",
			Help: "HTTP request latency distribution.", Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
		BuildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "agentd", Name: "build_info",
			Help: "Always 1; the version label carries the running build.",
		}, []string{"version"}),
	}
	reg.MustRegister(m.HTTPRequestsTotal, m.HTTPRequestDuration, m.BuildInfo)
	return m
}

// Handler returns the /metrics HTTP handler bound to this registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{Registry: m.registry})
}
