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

	RunsStarted       prometheus.Counter
	RunsFinished      *prometheus.CounterVec
	Steps             *prometheus.CounterVec
	StepReplays       *prometheus.CounterVec
	StepDuration      *prometheus.HistogramVec
	LLMTokens         *prometheus.CounterVec
	LLMCost           *prometheus.CounterVec
	ToolExecutions    *prometheus.CounterVec
	PolicyDecisions   *prometheus.CounterVec
	BudgetDenials     *prometheus.CounterVec
	ApprovalDecisions *prometheus.CounterVec
	ApprovalWait      prometheus.Histogram
}

// NewMetrics constructs the registry with Go runtime + process collectors.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	counter := func(name, help string, labels ...string) *prometheus.CounterVec {
		return prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "agentd", Name: name, Help: help}, labels)
	}

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
		RunsStarted: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "agentd", Name: "runs_started_total", Help: "Runs created.",
		}),
		RunsFinished: counter("runs_finished_total", "Runs finished, by final status.", "status"),
		Steps:        counter("steps_total", "Step outcomes (completed, failed, denied, awaiting_approval), by kind.", "kind", "status"),
		StepReplays:  counter("step_replays_total", "Steps answered from the journal without re-execution.", "kind"),
		StepDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "agentd", Name: "step_duration_seconds", Help: "Execution time of LLM and tool steps.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
		}, []string{"kind"}),
		LLMTokens:         counter("llm_tokens_total", "Tokens consumed, by provider, model and direction (input|output).", "provider", "model", "direction"),
		LLMCost:           counter("llm_cost_usd_total", "Estimated LLM spend in USD.", "provider", "model"),
		ToolExecutions:    counter("tool_executions_total", "Sandboxed tool executions by outcome (ok, nonzero_exit, timeout, error).", "tool", "outcome"),
		PolicyDecisions:   counter("policy_decisions_total", "Policy evaluations, by step kind and action.", "kind", "action"),
		BudgetDenials:     counter("budget_denials_total", "Steps refused because a budget limit would be exceeded.", "limit"),
		ApprovalDecisions: counter("approval_decisions_total", "Human approval decisions.", "decision"),
		ApprovalWait: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: "agentd", Name: "approval_wait_seconds", Help: "Time steps waited for a human decision.",
			Buckets: []float64{5, 15, 30, 60, 120, 300, 900, 1800, 3600, 14400},
		}),
	}
	reg.MustRegister(m.HTTPRequestsTotal, m.HTTPRequestDuration, m.BuildInfo, m.RunsStarted, m.RunsFinished,
		m.Steps, m.StepReplays, m.StepDuration, m.LLMTokens, m.LLMCost, m.ToolExecutions, m.PolicyDecisions,
		m.BudgetDenials, m.ApprovalDecisions, m.ApprovalWait)
	return m
}

// RegisterPendingApprovals exposes a gauge computed on scrape.
func (m *Metrics) RegisterPendingApprovals(fn func() float64) {
	m.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace: "agentd", Name: "approvals_pending", Help: "Steps currently awaiting a human decision.",
	}, fn))
}

// Handler returns the /metrics HTTP handler bound to this registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{Registry: m.registry})
}
