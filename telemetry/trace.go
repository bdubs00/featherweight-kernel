package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
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
// "execute_tool <tool>", after the OpenTelemetry GenAI conventions. The
// run span keeps its name and carries the attributes of an invoke_agent
// span of the GenAI agent conventions.
const (
	SpanRun  = "fw.run"
	SpanTurn = "fw.turn"
)

// builtinSource is the ToolSpec.Source of the built-in tools (builtin.Source).
const builtinSource = "builtin"

// Tracer is the trace projection. It implements fw.Observer. A run is one
// span from fw.run.started to fw.run.finished. Each turn is a child of the
// run, and each model call and tool call is a child of its turn. The run
// span is a child of the traceparent of the run, so it joins that trace.
// Span times are the times of the ledger events.
type Tracer struct {
	tracer trace.Tracer

	mu   sync.Mutex
	runs map[string]*runTrace // open runs by id
	// kept holds RunQueuedData.Kept of a branch until its last kept event:
	// the events up to it are copies of its source, whose trace has their
	// spans.
	kept map[string]uint64
	// waiting holds the approval that a run waits for, also across the end
	// of its span at a suspend, so a resume of a run that waits for input
	// starts no recovery clock. A decision or the finish removes the entry.
	waiting map[string]string
}

type runTrace struct {
	id, agent  string // the run, the conversation of the GenAI conventions, and its profile
	recovering time.Time
	last       time.Time

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
	return &Tracer{tracer: tp.Tracer(instrumentationName), runs: map[string]*runTrace{}, kept: map[string]uint64{}, waiting: map[string]string{}}
}

// Observe implements fw.Observer.
func (t *Tracer) Observe(e fw.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ts := eventTime(e)
	rt := t.runs[e.Subject]
	// The copies of a branch count here, because a branch can begin by
	// waiting for a decision.
	waiting := t.waiting[e.Subject] != ""
	if p := pendingAfter(t.waiting[e.Subject], e); p != "" {
		t.waiting[e.Subject] = p
	} else {
		delete(t.waiting, e.Subject)
	}
	if k := t.kept[e.Subject]; e.Seq != 0 && e.Seq <= k {
		// A copy of an event of the source opens no span and counts no
		// usage again: the trace of the source holds it.
		if e.Seq == k {
			delete(t.kept, e.Subject)
		}
		return
	}
	attrs := qualityAttributes(e)
	if len(attrs) > 0 {
		if rt != nil {
			rt.current().AddEvent(e.Type, trace.WithTimestamp(ts), trace.WithAttributes(attrs...))
		} else if e.Type == fw.EvRunAnnotated {
			t.receiptSpan(e, ts, attrs)
		}
	}
	if rt != nil {
		traceRecovery(rt, e, ts, waiting)
	}
	switch e.Type {
	case fw.EvRunQueued:
		// The run span starts at fw.run.started. The fw.run.queued of a
		// branch names its copies.
		var d fw.RunQueuedData
		if e.Decode(&d) == nil && d.Kept > e.Seq {
			t.kept[e.Subject] = d.Kept
		}
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
	case fw.EvRunAnnotated, fw.EvRunChild, fw.EvChildReported, fw.EvBudgetChanged, fw.EvRunReacted:
		// An annotation, the record and the report of a child, a change
		// of the budget, and a reaction can arrive after the run finished,
		// so they never open a run span.
		if rt != nil {
			if e.Type == fw.EvBudgetChanged {
				var d fw.BudgetChangedData
				_ = e.Decode(&d)
				rt.current().AddEvent(e.Type, trace.WithTimestamp(ts), trace.WithAttributes(attribute.String("fw.budget.by_kind", actorKind(d.By, d.ByKind)), attribute.String("fw.budget.origin", d.Origin)))
			} else if len(attrs) == 0 {
				rt.current().AddEvent(e.Type, trace.WithTimestamp(ts))
			}
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
		traceRecovery(rt, e, ts, waiting)
	}
	switch e.Type {
	case fw.EvRunStarted:
		var d fw.RunStartedData
		if e.Decode(&d) == nil {
			rt.span.SetAttributes(attribute.String("fw.model", d.Model), attribute.Int("fw.tools", len(d.Tools)))
			if _, model := fw.SplitSelector(d.Model); model != "" {
				rt.span.SetAttributes(attribute.String("gen_ai.request.model", model))
			}
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
				attribute.String("gen_ai.conversation.id", rt.id),
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
			attribute.Int64("fw.ttfb_ms", d.FirstByteMS),
			attribute.Int64("fw.queued_ms", d.QueuedMS),
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
			trace.WithAttributes(rt.toolAttributes(d)...))
	case fw.EvToolResponded:
		var d fw.ToolRespondedData
		_ = e.Decode(&d)
		span := rt.tools[d.CallID]
		if span == nil {
			return
		}
		delete(rt.tools, d.CallID)
		span.SetAttributes(attribute.Bool("fw.tool.is_error", d.IsError), attribute.Int64("fw.latency_ms", d.LatencyMS))
		if d.ExitCode != nil {
			span.SetAttributes(attribute.Int("process.exit.code", *d.ExitCode))
		}
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
			attribute.String("fw.approval.id", d.ApprovalID), attribute.String("fw.approval.decision", d.Decision), attribute.String("fw.approval.decider_kind", actorKind(d.Decider, d.DeciderKind))))
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
	case fw.EvMessageReceived:
		var d fw.MessageReceivedData
		_ = e.Decode(&d)
		rt.current().AddEvent(e.Type, trace.WithTimestamp(ts), trace.WithAttributes(attribute.String("fw.message.origin", d.Origin), attribute.String("fw.message.role", string(d.Role)), attribute.String("fw.message.note", d.Note)))
	case fw.EvRunResumed, fw.EvContextCompacted:
		rt.current().AddEvent(e.Type, trace.WithTimestamp(ts))
	}
}

// open starts the run span of an event as a child of its traceparent.
func (t *Tracer) open(e fw.Event, ts time.Time) *runTrace {
	parent := context.Background()
	if e.TraceParent != "" {
		parent = propagation.TraceContext{}.Extract(parent, propagation.MapCarrier{"traceparent": e.TraceParent})
	}
	agent := agentOf(e.Source)
	attrs := []attribute.KeyValue{
		attribute.String("fw.run.id", e.Subject),
		attribute.String("fw.agent", agent),
		// A run is one invocation of the agent of its profile, and the run
		// is the conversation, as the GenAI agent conventions name them.
		attribute.String("gen_ai.operation.name", "invoke_agent"),
		attribute.String("gen_ai.agent.name", agent),
		attribute.String("gen_ai.agent.id", agent),
		attribute.String("gen_ai.conversation.id", e.Subject),
	}
	if e.Type != fw.EvRunStarted {
		attrs = append(attrs, attribute.String("fw.run.reopened_by", e.Type))
	}
	ctx, span := t.tracer.Start(parent, SpanRun, trace.WithTimestamp(ts), trace.WithAttributes(attrs...))
	rt := &runTrace{id: e.Subject, agent: agent, ctx: ctx, span: span, tools: map[string]trace.Span{}}
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

// toolAttributes returns the attributes of an execute_tool span of the
// GenAI conventions. A built-in tool, or a tool of a Go option, is a
// function, and a tool of an MCP server or an A2A peer is an extension. A
// tool of an MCP server, named <server>.<tool>, also carries the method of
// the MCP call, and a call of the built-in skill tool the skill it loads.
func (rt *runTrace) toolAttributes(d fw.ToolRequestedData) []attribute.KeyValue {
	kind := "function"
	if d.Source != "" && d.Source != builtinSource {
		kind = "extension"
	}
	attrs := []attribute.KeyValue{
		attribute.String("gen_ai.operation.name", "execute_tool"),
		attribute.String("gen_ai.tool.name", d.Tool),
		attribute.String("gen_ai.tool.call.id", d.CallID),
		attribute.String("gen_ai.tool.type", kind),
		attribute.String("gen_ai.agent.name", rt.agent),
		attribute.String("gen_ai.conversation.id", rt.id),
	}
	if kind == "extension" && strings.HasPrefix(d.Tool, d.Source+".") {
		attrs = append(attrs, attribute.String("mcp.method.name", "tools/call"))
	}
	if d.Tool == "skill" && d.Source == builtinSource {
		var in struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(d.Input, &in) == nil && in.Name != "" {
			attrs = append(attrs, attribute.String("gen_ai.skill.name", in.Name))
		}
	}
	return attrs
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
