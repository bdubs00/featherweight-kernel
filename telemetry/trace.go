package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
)

// instrumentationName names the tracer of the trace projection.
const instrumentationName = "github.com/bdubs00/featherweight-kernel/telemetry"

// Span names. A model span is named "chat <model>" and a tool span
// "execute_tool <tool>", after the OpenTelemetry GenAI conventions.
const (
	SpanRun  = "fw.run"
	SpanTurn = "fw.turn"
)

// Tracer is the trace projection. It implements fw.Observer. A run is one
// span from fw.run.started to fw.run.finished. Each turn is a child of the
// run, and each model call and tool call is a child of its turn. The run
// span is a child of the traceparent of the run, so it joins that trace.
// Span times are the times of the ledger events.
type Tracer struct {
	tracer trace.Tracer

	mu   sync.Mutex
	runs map[string]*runTrace // open runs by id
}

type runTrace struct {
	ctx     context.Context // holds the run span
	span    trace.Span
	turnCtx context.Context // holds the turn span
	turn    trace.Span
	model   trace.Span
	tools   map[string]trace.Span // by call id
}

// NewTracer builds the trace projection with its own tracer provider. The
// provider exports over OTLP HTTP when OTEL_EXPORTER_OTLP_ENDPOINT or
// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT is set, and exports nothing otherwise.
// The exporter reads the other standard OTEL_EXPORTER_OTLP_* variables. The
// returned function ends the spans that are still open, flushes them, and
// shuts the provider down.
func NewTracer(ctx context.Context, serviceName string) (*Tracer, func(context.Context) error, error) {
	// Options apply in order and the last value of a key wins, so
	// OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES override serviceName.
	version, _ := buildVersion()
	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(serviceName), semconv.ServiceVersion(version)),
		resource.WithTelemetrySDK(),
		resource.WithFromEnv(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("telemetry: trace resource: %w", err)
	}
	opts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "" {
		exp, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("telemetry: otlp trace exporter: %w", err)
		}
		opts = append(opts, sdktrace.WithBatcher(exp))
	}
	tp := sdktrace.NewTracerProvider(opts...)
	t := NewTracerFrom(tp)
	shutdown := func(ctx context.Context) error {
		t.endAll(time.Now())
		if err := errors.Join(tp.ForceFlush(ctx), tp.Shutdown(ctx)); err != nil {
			return fmt.Errorf("telemetry: shut down tracer: %w", err)
		}
		return nil
	}
	return t, shutdown, nil
}

// NewTracerFrom builds the trace projection on a tracer provider that the
// caller owns.
func NewTracerFrom(tp trace.TracerProvider) *Tracer {
	return &Tracer{tracer: tp.Tracer(instrumentationName), runs: map[string]*runTrace{}}
}

// Observe implements fw.Observer.
func (t *Tracer) Observe(e fw.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ts := eventTime(e)
	rt := t.runs[e.Subject]
	switch e.Type {
	case fw.EvRunQueued:
		// The run span starts at fw.run.started.
		return
	case fw.EvRunFinished:
		if rt == nil {
			return
		}
		var d fw.RunFinishedData
		_ = e.Decode(&d)
		rt.span.SetAttributes(attribute.String("fw.run.outcome", d.Outcome))
		if d.Outcome == fw.OutcomeFailed {
			rt.span.SetStatus(codes.Error, d.Error)
		}
		t.end(e.Subject, rt, ts)
		return
	case fw.EvRunAnnotated, fw.EvRunChild, fw.EvChildReported:
		// An annotation, and the record and the report of a child, can
		// arrive after the run finished, so they never open a run span.
		if rt != nil {
			rt.current().AddEvent(e.Type, trace.WithTimestamp(ts))
		}
		return
	case fw.EvRunSuspended:
		if rt == nil {
			return
		}
		var d fw.RunSuspendedData
		_ = e.Decode(&d)
		rt.span.SetAttributes(attribute.String("fw.run.suspended", d.Reason))
		t.end(e.Subject, rt, ts)
		return
	}
	if rt == nil {
		// fw.run.started opens the run span. A resume or a follow-up after
		// fw.run.finished opens a new run span in the same trace.
		rt = t.open(e, ts)
	}
	switch e.Type {
	case fw.EvRunStarted:
		var d fw.RunStartedData
		if e.Decode(&d) == nil {
			rt.span.SetAttributes(attribute.String("fw.model", d.Model), attribute.Int("fw.tools", len(d.Tools)))
		}
	case fw.EvTurnStarted:
		rt.endTurn(ts)
		var d fw.TurnData
		_ = e.Decode(&d)
		rt.turnCtx, rt.turn = t.tracer.Start(rt.ctx, SpanTurn,
			trace.WithTimestamp(ts),
			trace.WithAttributes(attribute.Int("fw.turn", d.Turn)))
	case fw.EvTurnFinished:
		rt.endTurn(ts)
	case fw.EvModelRequested:
		var d fw.ModelRequestedData
		_ = e.Decode(&d)
		if rt.model != nil {
			rt.model.End(trace.WithTimestamp(ts))
		}
		_, rt.model = t.tracer.Start(rt.parent(), "chat "+d.Model,
			trace.WithTimestamp(ts),
			trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(
				attribute.String("gen_ai.operation.name", "chat"),
				attribute.String("gen_ai.provider.name", d.Provider),
				attribute.String("gen_ai.request.model", d.Model),
				attribute.String("fw.effort", d.Effort),
				attribute.Int("fw.estimated_input_tokens", d.EstimatedInputTokens),
			))
	case fw.EvModelResponded:
		if rt.model == nil {
			return
		}
		var d fw.ModelRespondedData
		_ = e.Decode(&d)
		rt.model.SetAttributes(
			attribute.String("gen_ai.response.model", d.Model),
			attribute.StringSlice("gen_ai.response.finish_reasons", []string{string(d.StopReason)}),
			attribute.Int("gen_ai.usage.input_tokens", d.Usage.InputTokens),
			attribute.Int("gen_ai.usage.output_tokens", d.Usage.OutputTokens),
			attribute.Int("gen_ai.usage.cache_read.input_tokens", d.Usage.CacheReadTokens),
			attribute.Int("gen_ai.usage.cache_creation.input_tokens", d.Usage.CacheWriteTokens),
			attribute.Int64("fw.latency_ms", d.LatencyMS),
		)
		rt.model.End(trace.WithTimestamp(ts))
		rt.model = nil
	case fw.EvToolRequested:
		var d fw.ToolRequestedData
		_ = e.Decode(&d)
		if old := rt.tools[d.CallID]; old != nil {
			old.End(trace.WithTimestamp(ts))
		}
		_, rt.tools[d.CallID] = t.tracer.Start(rt.parent(), "execute_tool "+d.Tool,
			trace.WithTimestamp(ts),
			trace.WithAttributes(
				attribute.String("gen_ai.operation.name", "execute_tool"),
				attribute.String("gen_ai.tool.name", d.Tool),
				attribute.String("gen_ai.tool.call.id", d.CallID),
			))
	case fw.EvToolResponded:
		var d fw.ToolRespondedData
		_ = e.Decode(&d)
		span := rt.tools[d.CallID]
		if span == nil {
			return
		}
		delete(rt.tools, d.CallID)
		span.SetAttributes(attribute.Bool("fw.tool.is_error", d.IsError), attribute.Int64("fw.latency_ms", d.LatencyMS))
		if d.IsError {
			span.SetStatus(codes.Error, "the tool returned an error result")
		}
		span.End(trace.WithTimestamp(ts))
	case fw.EvToolProgress:
		var d fw.ToolProgressData
		_ = e.Decode(&d)
		if span := rt.tools[d.CallID]; span != nil {
			span.AddEvent(e.Type, trace.WithTimestamp(ts), trace.WithAttributes(
				attribute.Float64("fw.progress", d.Progress), attribute.Float64("fw.total", d.Total)))
		}
	case fw.EvApprovalRequested:
		var d fw.ApprovalRequestedData
		_ = e.Decode(&d)
		rt.current().AddEvent(e.Type, trace.WithTimestamp(ts), trace.WithAttributes(
			attribute.String("fw.approval.id", d.ApprovalID), attribute.String("gen_ai.tool.name", d.Tool)))
	case fw.EvApprovalDecided:
		var d fw.ApprovalDecidedData
		_ = e.Decode(&d)
		rt.current().AddEvent(e.Type, trace.WithTimestamp(ts), trace.WithAttributes(
			attribute.String("fw.approval.id", d.ApprovalID), attribute.String("fw.approval.decision", d.Decision)))
	case fw.EvError:
		var d fw.ErrorData
		_ = e.Decode(&d)
		rt.current().AddEvent(e.Type, trace.WithTimestamp(ts), trace.WithAttributes(
			attribute.String("fw.error.message", d.Message), attribute.Bool("fw.error.retryable", d.Retryable)))
	case fw.EvBudgetExceeded:
		var d fw.BudgetExceededData
		_ = e.Decode(&d)
		rt.current().AddEvent(e.Type, trace.WithTimestamp(ts), trace.WithAttributes(
			attribute.String("fw.budget.limit", d.Limit), attribute.Float64("fw.budget.value", d.Value)))
	case fw.EvRunResumed, fw.EvContextCompacted, fw.EvMessageReceived:
		rt.current().AddEvent(e.Type, trace.WithTimestamp(ts))
	}
}

// open starts the run span of an event as a child of its traceparent.
func (t *Tracer) open(e fw.Event, ts time.Time) *runTrace {
	parent := context.Background()
	if e.TraceParent != "" {
		parent = propagation.TraceContext{}.Extract(parent, propagation.MapCarrier{"traceparent": e.TraceParent})
	}
	attrs := []attribute.KeyValue{
		attribute.String("fw.run.id", e.Subject),
		attribute.String("fw.agent", agentOf(e.Source)),
	}
	if e.Type != fw.EvRunStarted {
		attrs = append(attrs, attribute.String("fw.run.reopened_by", e.Type))
	}
	ctx, span := t.tracer.Start(parent, SpanRun, trace.WithTimestamp(ts), trace.WithAttributes(attrs...))
	rt := &runTrace{ctx: ctx, span: span, tools: map[string]trace.Span{}}
	t.runs[e.Subject] = rt
	return rt
}

// end ends every open span of a run and forgets the run.
func (t *Tracer) end(id string, rt *runTrace, ts time.Time) {
	rt.endTurn(ts)
	rt.span.End(trace.WithTimestamp(ts))
	delete(t.runs, id)
}

// endAll ends the spans of every open run, so a shutdown exports them.
func (t *Tracer) endAll(ts time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, rt := range t.runs {
		rt.span.SetAttributes(attribute.Bool("fw.run.open", true))
		t.end(id, rt, ts)
	}
}

// parent is the context of a new model or tool span: the turn when one is
// open, else the run.
func (rt *runTrace) parent() context.Context {
	if rt.turn != nil {
		return rt.turnCtx
	}
	return rt.ctx
}

// current is the span that receives span events: the turn when one is
// open, else the run.
func (rt *runTrace) current() trace.Span {
	if rt.turn != nil {
		return rt.turn
	}
	return rt.span
}

// endTurn ends the open model and tool spans and the turn span.
func (rt *runTrace) endTurn(ts time.Time) {
	if rt.model != nil {
		rt.model.End(trace.WithTimestamp(ts))
		rt.model = nil
	}
	for id, s := range rt.tools {
		s.End(trace.WithTimestamp(ts))
		delete(rt.tools, id)
	}
	if rt.turn != nil {
		rt.turn.End(trace.WithTimestamp(ts))
		rt.turn, rt.turnCtx = nil, nil
	}
}
