// Package tracing emits OpenTelemetry spans for runs and steps.
//
// A run is a single trace whose ID is derived from the run ID, so steps that
// execute hours apart (across agent restarts) land in the same trace without
// any client-side context propagation. Step spans are emitted when a step
// finishes; the root "invoke_agent" span is emitted when the run finishes.
package tracing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/anupam2105/agentd/internal/domain"
)

// TraceID returns the hex trace ID for a run.
func TraceID(runID string) string {
	t := traceID(runID)
	return hex.EncodeToString(t[:])
}

func traceID(runID string) trace.TraceID {
	sum := sha256.Sum256([]byte("agentd/trace/" + runID))
	var t trace.TraceID
	copy(t[:], sum[:16])
	return t
}

func rootSpanID(runID string) trace.SpanID {
	sum := sha256.Sum256([]byte("agentd/root/" + runID))
	var s trace.SpanID
	copy(s[:], sum[:8])
	return s
}

// Tracer records run and step spans. A nil *Tracer is a no-op.
type Tracer struct {
	tp     *sdktrace.TracerProvider
	tracer trace.Tracer
}

type idsKey struct{}

type fixedIDs struct {
	trace trace.TraceID
	span  trace.SpanID
}

// idGenerator honours IDs pinned on the context (for the root span) and
// otherwise generates random ones.
type idGenerator struct{ fallback sdktrace.IDGenerator }

func (g idGenerator) NewIDs(ctx context.Context) (trace.TraceID, trace.SpanID) {
	if ids, ok := ctx.Value(idsKey{}).(fixedIDs); ok {
		return ids.trace, ids.span
	}
	return g.fallback.NewIDs(ctx)
}

func (g idGenerator) NewSpanID(ctx context.Context, traceID trace.TraceID) trace.SpanID {
	return g.fallback.NewSpanID(ctx, traceID)
}

// randomIDs generates cryptographically random trace and span IDs.
type randomIDs struct{}

func (randomIDs) NewIDs(context.Context) (trace.TraceID, trace.SpanID) {
	var t trace.TraceID
	var s trace.SpanID
	_, _ = randRead(t[:])
	_, _ = randRead(s[:])
	return t, s
}

func (randomIDs) NewSpanID(context.Context, trace.TraceID) trace.SpanID {
	var s trace.SpanID
	_, _ = randRead(s[:])
	return s
}

// New exports spans with OTLP/HTTP. Endpoint configuration follows the
// standard OTEL_EXPORTER_OTLP_* environment variables.
func New(ctx context.Context, version string) (*Tracer, error) {
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("otlp exporter: %w", err)
	}
	return NewWithExporter(exp, version), nil
}

// NewWithExporter builds a tracer around any span exporter (tests use an in-memory one).
func NewWithExporter(exp sdktrace.SpanExporter, version string) *Tracer {
	res := resource.NewSchemaless(
		attribute.String("service.name", "agentd"),
		attribute.String("service.version", version),
	)
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		sdktrace.WithIDGenerator(idGenerator{fallback: randomIDs{}}),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	return &Tracer{tp: tp, tracer: tp.Tracer("github.com/anupam2105/agentd")}
}

// Shutdown flushes pending spans.
func (t *Tracer) Shutdown(ctx context.Context) error {
	if t == nil {
		return nil
	}
	return t.tp.Shutdown(ctx)
}

// ForceFlush exports buffered spans now.
func (t *Tracer) ForceFlush(ctx context.Context) error {
	if t == nil {
		return nil
	}
	return t.tp.ForceFlush(ctx)
}

// StepInfo carries details only known at execution time.
type StepInfo struct {
	Provider     string
	ResponseID   string
	StopReason   string
	ExitCode     *int
	TimedOut     bool
	ErrorMessage string
}

// RecordStep emits a span for an executed step as a child of the run's root span.
func (t *Tracer) RecordStep(run *domain.Run, st *domain.Step, start, end time.Time, info *StepInfo) {
	if t == nil {
		return
	}
	parent := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID(run.ID), SpanID: rootSpanID(run.ID), TraceFlags: trace.FlagsSampled, Remote: true,
	})
	ctx := trace.ContextWithRemoteSpanContext(context.Background(), parent)

	attrs := []attribute.KeyValue{
		attribute.String("gen_ai.agent.name", run.Agent),
		attribute.String("agentd.run.id", run.ID),
		attribute.String("agentd.step.key", st.Key),
		attribute.String("agentd.step.kind", string(st.Kind)),
		attribute.String("agentd.step.status", string(st.Status)),
		attribute.Int("agentd.step.attempt", st.Attempts),
	}
	if st.PolicyRule != "" {
		attrs = append(attrs, attribute.String("agentd.policy.rule", st.PolicyRule))
	}
	var name string
	switch st.Kind {
	case domain.KindLLM:
		name = "chat " + st.Name
		attrs = append(attrs,
			attribute.String("gen_ai.operation.name", "chat"),
			attribute.String("gen_ai.request.model", st.Name),
			attribute.Int64("gen_ai.usage.input_tokens", st.Usage.InputTokens),
			attribute.Int64("gen_ai.usage.output_tokens", st.Usage.OutputTokens),
			attribute.Float64("agentd.cost_usd", st.Usage.CostUSD),
		)
		if info.Provider != "" {
			attrs = append(attrs, attribute.String("gen_ai.provider.name", info.Provider))
		}
		if info.StopReason != "" {
			attrs = append(attrs, attribute.StringSlice("gen_ai.response.finish_reasons", []string{info.StopReason}))
		}
	case domain.KindTool:
		name = "execute_tool " + st.Name
		attrs = append(attrs,
			attribute.String("gen_ai.operation.name", "execute_tool"),
			attribute.String("gen_ai.tool.name", st.Name),
			attribute.String("gen_ai.tool.call.id", st.Key),
		)
		if info.ExitCode != nil {
			attrs = append(attrs, attribute.Int("agentd.tool.exit_code", *info.ExitCode))
		}
		if info.TimedOut {
			attrs = append(attrs, attribute.Bool("agentd.tool.timed_out", true))
		}
	default:
		name = "record " + st.Key
	}
	kind := trace.SpanKindInternal
	if st.Kind == domain.KindLLM {
		kind = trace.SpanKindClient
	}
	_, span := t.tracer.Start(ctx, name, trace.WithTimestamp(start), trace.WithSpanKind(kind), trace.WithAttributes(attrs...))
	if st.Error != nil {
		span.SetAttributes(attribute.String("error.type", st.Error.Code))
		span.SetStatus(codes.Error, st.Error.Message)
	} else if info.ErrorMessage != "" {
		span.SetStatus(codes.Error, info.ErrorMessage)
	}
	span.End(trace.WithTimestamp(end))
}

// RecordRun emits the root span for a finished run.
func (t *Tracer) RecordRun(run *domain.Run) {
	if t == nil {
		return
	}
	end := run.UpdatedAt
	if run.FinishedAt != nil {
		end = *run.FinishedAt
	}
	ctx := context.WithValue(context.Background(), idsKey{}, fixedIDs{trace: traceID(run.ID), span: rootSpanID(run.ID)})
	_, span := t.tracer.Start(ctx, "invoke_agent "+run.Agent,
		trace.WithNewRoot(),
		trace.WithTimestamp(run.CreatedAt),
		trace.WithAttributes(
			attribute.String("gen_ai.operation.name", "invoke_agent"),
			attribute.String("gen_ai.agent.name", run.Agent),
			attribute.String("agentd.run.id", run.ID),
			attribute.String("agentd.run.status", string(run.Status)),
			attribute.Int64("agentd.run.steps", run.Usage.Steps),
			attribute.Int64("gen_ai.usage.input_tokens", run.Usage.InputTokens),
			attribute.Int64("gen_ai.usage.output_tokens", run.Usage.OutputTokens),
			attribute.Float64("agentd.cost_usd", run.Usage.CostUSD),
		))
	if run.Status != domain.RunCompleted {
		span.SetStatus(codes.Error, run.Error)
	}
	span.End(trace.WithTimestamp(end))
}
