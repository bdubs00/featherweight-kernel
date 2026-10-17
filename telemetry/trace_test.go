package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func newTestTracer() (*Tracer, *tracetest.InMemoryExporter) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	return NewTracerFrom(tp), exp
}

func spansNamed(spans tracetest.SpanStubs, name string) []tracetest.SpanStub {
	var out []tracetest.SpanStub
	for _, s := range spans {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

func attr(s tracetest.SpanStub, key string) (attribute.Value, bool) {
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func TestTracerNesting(t *testing.T) {
	tr, exp := newTestTracer()
	f := newFeed(tr, "01RUN", "reviewer")
	f.emit(fw.EvRunQueued, fw.RunQueuedData{Agent: "reviewer", Input: "x"})
	started := f.emit(fw.EvRunStarted, fw.RunStartedData{Model: "anthropic/claude-x", Tools: []string{"echo"}})
	f.emit(fw.EvTurnStarted, fw.TurnData{Turn: 1})
	f.emit(fw.EvModelRequested, fw.ModelRequestedData{Provider: "anthropic", Model: "claude-x"})
	f.emit(fw.EvModelResponded, fw.ModelRespondedData{
		Content: []fw.Block{toolUse("c1", "echo")}, StopReason: fw.StopToolUse,
		Usage: fw.Usage{InputTokens: 100, OutputTokens: 50}, LatencyMS: 1000, Provider: "anthropic", Model: "claude-x",
	})
	f.emit(fw.EvToolRequested, fw.ToolRequestedData{Tool: "echo", CallID: "c1"})
	f.emit(fw.EvToolProgress, fw.ToolProgressData{CallID: "c1", Progress: 1, Total: 2})
	f.emit(fw.EvToolResponded, fw.ToolRespondedData{CallID: "c1", IsError: true, LatencyMS: 900})
	f.emit(fw.EvTurnFinished, fw.TurnData{Turn: 1})
	f.emit(fw.EvTurnStarted, fw.TurnData{Turn: 2})
	f.emit(fw.EvModelRequested, fw.ModelRequestedData{Provider: "anthropic", Model: "claude-x"})
	f.emit(fw.EvModelResponded, fw.ModelRespondedData{
		Content: []fw.Block{fw.Text("done")}, StopReason: fw.StopEndTurn,
		Usage: fw.Usage{InputTokens: 10, OutputTokens: 5}, LatencyMS: 1000, Provider: "anthropic", Model: "claude-x",
	})
	f.emit(fw.EvTurnFinished, fw.TurnData{Turn: 2})
	finished := f.emit(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeFailed, Error: "boom"})

	spans := exp.GetSpans()
	if len(spans) != 6 {
		t.Fatalf("spans = %d", len(spans))
	}
	runs, turns := spansNamed(spans, SpanRun), spansNamed(spans, SpanTurn)
	models, tools := spansNamed(spans, "chat claude-x"), spansNamed(spans, "execute_tool echo")
	if len(runs) != 1 || len(turns) != 2 || len(models) != 2 || len(tools) != 1 {
		t.Fatalf("names = %v", spans)
	}
	run := runs[0]
	// The run span joins the trace of the traceparent.
	if run.SpanContext.TraceID().String() != "0af7651916cd43dd8448eb211c80319c" ||
		run.Parent.SpanID().String() != "b7ad6b7169203331" || !run.Parent.IsRemote() {
		t.Fatalf("run parent = %v, trace %v", run.Parent, run.SpanContext.TraceID())
	}
	if !run.StartTime.Equal(started.Time) || !run.EndTime.Equal(finished.Time) || run.Status.Code != codes.Error {
		t.Fatalf("run span = %v..%v status %v", run.StartTime, run.EndTime, run.Status)
	}
	if v, ok := attr(run, "fw.run.id"); !ok || v.AsString() != "01RUN" {
		t.Fatalf("run attrs = %v", run.Attributes)
	}
	for _, s := range spans {
		if s.SpanContext.TraceID() != run.SpanContext.TraceID() {
			t.Fatalf("span %s is in another trace", s.Name)
		}
	}
	turnOf := map[trace.SpanID]int64{}
	for _, tu := range turns {
		if tu.Parent.SpanID() != run.SpanContext.SpanID() {
			t.Fatalf("turn parent = %v", tu.Parent.SpanID())
		}
		n, _ := attr(tu, "fw.turn")
		turnOf[tu.SpanContext.SpanID()] = n.AsInt64()
	}
	for i, m := range models {
		if turnOf[m.Parent.SpanID()] != int64(i+1) || m.SpanKind != trace.SpanKindClient {
			t.Fatalf("model call %d is in turn %d", i, turnOf[m.Parent.SpanID()])
		}
	}
	if v, ok := attr(models[0], "gen_ai.usage.input_tokens"); !ok || v.AsInt64() != 100 {
		t.Fatalf("model attrs = %v", models[0].Attributes)
	}
	if v, ok := attr(models[0], "fw.latency_ms"); !ok || v.AsInt64() != 1000 {
		t.Fatalf("model attrs = %v", models[0].Attributes)
	}
	tool := tools[0]
	if turnOf[tool.Parent.SpanID()] != 1 || tool.Status.Code != codes.Error || len(tool.Events) != 1 {
		t.Fatalf("tool span = %+v", tool)
	}
	if v, ok := attr(tool, "fw.latency_ms"); !ok || v.AsInt64() != 900 {
		t.Fatalf("tool attrs = %v", tool.Attributes)
	}
	tr.mu.Lock()
	open := len(tr.runs)
	tr.mu.Unlock()
	if open != 0 {
		t.Fatalf("tracer remembers %d finished runs", open)
	}
}

func TestTracerFollowUpAndOpenSpans(t *testing.T) {
	tr, exp := newTestTracer()
	f := newFeed(tr, "r1", "default")
	f.emit(fw.EvRunStarted, fw.RunStartedData{})
	f.emit(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted})
	// A follow-up opens a second run span in the same trace.
	f.emit(fw.EvMessageReceived, fw.MessageReceivedData{Role: fw.RoleUser, Content: []fw.Block{fw.Text("again")}})
	f.emit(fw.EvTurnStarted, fw.TurnData{Turn: 2})
	f.emit(fw.EvModelRequested, fw.ModelRequestedData{Provider: "anthropic", Model: "m"})
	if n := len(exp.GetSpans()); n != 1 {
		t.Fatalf("ended spans = %d", n)
	}
	// A shutdown ends the open spans so that they are exported.
	tr.endAll(f.at.Add(time.Second))
	spans := exp.GetSpans()
	runs := spansNamed(spans, SpanRun)
	if len(spans) != 4 || len(runs) != 2 || runs[0].SpanContext.TraceID() != runs[1].SpanContext.TraceID() {
		t.Fatalf("spans = %v", spans)
	}
	if v, ok := attr(runs[1], "fw.run.reopened_by"); !ok || v.AsString() != fw.EvMessageReceived {
		t.Fatalf("reopened run attrs = %v", runs[1].Attributes)
	}
	if v, ok := attr(runs[1], "fw.run.open"); !ok || !v.AsBool() {
		t.Fatalf("open run attrs = %v", runs[1].Attributes)
	}
}

func TestNewTracerWithoutEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	ctx := context.Background()
	tr, shutdown, err := NewTracer(ctx, "fw-test")
	if err != nil {
		t.Fatal(err)
	}
	f := newFeed(tr, "r1", "default")
	f.emit(fw.EvRunStarted, fw.RunStartedData{})
	f.emit(fw.EvTurnStarted, fw.TurnData{Turn: 1})
	if err := shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestTracerWithAgent observes a real run and checks the span count.
func TestTracerWithAgent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tr, exp := newTestTracer()
	echo := fw.NewTool(fw.ToolSpec{Name: "echo", ReadOnly: true}, func(context.Context, json.RawMessage) (fw.ToolResult, error) {
		return fw.TextResult("pong"), nil
	})
	s := &script{resps: []*fw.Response{
		{Content: []fw.Block{toolUse("c1", "echo")}, StopReason: fw.StopToolUse},
		{Content: []fw.Block{fw.Text("done")}, StopReason: fw.StopEndTurn},
	}}
	a := newAgent(t, s.model(), fw.WithTools(echo), fw.WithObservers(tr))
	r, err := a.Run(ctx, "go")
	if err != nil || r.State != fw.StateCompleted {
		t.Fatalf("run = %+v, %v", r, err)
	}
	// The kernel notifies observers after Run sees the terminal state.
	eventually(t, func() bool { return len(exp.GetSpans()) == 6 })
	spans := exp.GetSpans()
	if len(spansNamed(spans, "execute_tool echo")) != 1 || len(spansNamed(spans, SpanTurn)) != 2 || len(spansNamed(spans, SpanRun)) != 1 {
		t.Fatalf("spans = %v", spans)
	}
}

// eventually waits up to five seconds for cond.
func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestLogger(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"debug": slog.LevelDebug, "INFO": slog.LevelInfo, "warn": slog.LevelWarn,
		"warning": slog.LevelWarn, " error ": slog.LevelError, "": slog.LevelInfo, "loud": slog.LevelInfo,
	} {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
	var buf bytes.Buffer
	l := newLogger(&buf, "warn")
	l.Info("hidden")
	l.Warn("shown", "run", "r1")
	line := strings.TrimSpace(buf.String())
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil || rec["msg"] != "shown" || rec["run"] != "r1" || strings.Contains(line, "hidden") {
		t.Fatalf("log = %q, %v", line, err)
	}
	if !Logger("debug").Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("Logger(debug) does not log debug")
	}
}
