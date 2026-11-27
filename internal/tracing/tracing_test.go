package tracing

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/anupam2105/agentd/internal/domain"
)

func TestRunAndStepSpansShareTrace(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tr := NewWithExporter(exp, "test")
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := t0.Add(time.Minute)
	run := &domain.Run{ID: "run_1", Agent: "sre", Status: domain.RunCompleted, CreatedAt: t0, FinishedAt: &end}
	step := &domain.Step{Key: "1:llm", Kind: domain.KindLLM, Name: "mock-1", Status: domain.StepCompleted,
		Usage: domain.Usage{InputTokens: 10, OutputTokens: 5}}
	tr.RecordStep(run, step, t0.Add(time.Second), t0.Add(2*time.Second), &StepInfo{Provider: "mock", StopReason: "end_turn"})
	tr.RecordRun(run)
	if err := tr.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	spans := exp.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("got %d spans", len(spans))
	}
	stepSpan, rootSpan := spans[0], spans[1]
	if stepSpan.Name != "chat mock-1" || rootSpan.Name != "invoke_agent sre" {
		t.Fatalf("names: %q %q", stepSpan.Name, rootSpan.Name)
	}
	if got := rootSpan.SpanContext.TraceID().String(); got != TraceID("run_1") || stepSpan.SpanContext.TraceID().String() != got {
		t.Fatalf("trace ids differ: %s %s", got, stepSpan.SpanContext.TraceID())
	}
	if stepSpan.Parent.SpanID() != rootSpan.SpanContext.SpanID() {
		t.Fatal("step span is not a child of the run span")
	}
	if rootSpan.Parent.IsValid() {
		t.Fatal("run span must be a root")
	}
	var nilTracer *Tracer
	nilTracer.RecordRun(run) // must not panic
}
