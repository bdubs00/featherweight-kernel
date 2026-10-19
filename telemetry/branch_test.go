package telemetry

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/prometheus/client_golang/prometheus"
)

// TestMetricsCountTheOwnWorkOfABranch runs a run with a tool call, then a
// branch with a message, which makes one model call, and a branch without
// a message, which does no work. The metrics count each model call, token,
// cost, turn, and tool call once, as the usage of the runs does. The runs
// end four times: the source, each branch at the end of its copies, which
// end the run, and the branch with a message again, as a run that takes a
// follow-up after its end does.
func TestMetricsCountTheOwnWorkOfABranch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	echo := fw.NewTool(fw.ToolSpec{Name: "echo", ReadOnly: true}, func(context.Context, json.RawMessage) (fw.ToolResult, error) {
		return fw.TextResult("echoed"), nil
	})
	s := &script{resps: []*fw.Response{
		{Content: []fw.Block{toolUse("c1", "echo")}, StopReason: fw.StopToolUse, Usage: fw.Usage{InputTokens: 100, OutputTokens: 10}},
		{Content: []fw.Block{fw.Text("one")}, StopReason: fw.StopEndTurn, Usage: fw.Usage{InputTokens: 100, OutputTokens: 20}},
		{Content: []fw.Block{fw.Text("two")}, StopReason: fw.StopEndTurn, Usage: fw.Usage{InputTokens: 50, OutputTokens: 5}},
	}}
	prices := fw.PriceTable{"anthropic/claude-opus-5-5": {Input: 1, Output: 1}}
	a := newAgent(t, s.model(), fw.WithPrices(prices), fw.WithTools(echo))
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg, prices)
	a.Observe(m)
	src, err := a.Run(ctx, "work")
	if err != nil || src.State != fw.StateCompleted {
		t.Fatalf("source = %+v, %v", src, err)
	}
	b, err := a.Fork(ctx, src.ID, fw.ForkRequest{Input: "again"})
	if err != nil {
		t.Fatal(err)
	}
	if b, err = a.WaitFor(ctx, b.ID, func(r *fw.Run) bool { return r.State.Terminal() }); err != nil || b.State != fw.StateCompleted {
		t.Fatalf("branch = %+v, %v", b, err)
	}
	idle, err := a.Fork(ctx, src.ID, fw.ForkRequest{})
	if err != nil || idle.State != fw.StateCompleted {
		t.Fatalf("idle branch = %+v, %v", idle, err)
	}

	model := map[string]string{"provider": "anthropic", "model": "claude-opus-5-5"}
	with := func(k, v string) map[string]string {
		out := map[string]string{k: v}
		for key, val := range model {
			out[key] = val
		}
		return out
	}
	runs := map[string]string{"agent": "default", "outcome": fw.OutcomeCompleted, "batch": ""}
	eventually(t, func() bool { v, ok := lookup(t, reg, "fw_runs_total", runs); return ok && v == 4 })
	for _, c := range []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"fw_model_requests_total", with("outcome", OutcomeOK), 3},
		{"fw_model_tokens_total", with("kind", "input"), 250},
		{"fw_model_tokens_total", with("kind", "output"), 35},
		{"fw_model_cost_usd_total", model, 285.0 / 1_000_000},
		{"fw_tool_calls_total", map[string]string{"tool": "echo", "outcome": OutcomeOK}, 1},
		{"fw_runs_active", map[string]string{"agent": "default"}, 0},
		{"fw_run_turns", map[string]string{"agent": "default"}, 4},
	} {
		if got := value(t, reg, c.name, c.labels); got != c.want {
			t.Errorf("%s%v = %v, want %v", c.name, c.labels, got, c.want)
		}
	}
}

// TestTracerSkipsTheCopiesOfABranch feeds a branch whose ledger begins with
// copies of a model call of its source, and then makes a model call of its
// own, and a branch that does no work. Only the own call has a span with
// its usage, and the branch without work has no span.
func TestTracerSkipsTheCopiesOfABranch(t *testing.T) {
	tr, exp := newTestTracer()
	copies := func(f *feed, kept uint64) {
		f.emit(fw.EvRunQueued, fw.RunQueuedData{Agent: "reviewer", Input: "x", Kept: kept})
		f.emit(fw.EvRunStarted, fw.RunStartedData{Model: "anthropic/claude-x"})
		f.emit(fw.EvTurnStarted, fw.TurnData{Turn: 1})
		f.emit(fw.EvModelRequested, fw.ModelRequestedData{Provider: "anthropic", Model: "claude-x"})
		f.emit(fw.EvModelResponded, fw.ModelRespondedData{Content: []fw.Block{fw.Text("one")}, StopReason: fw.StopEndTurn,
			Usage: fw.Usage{InputTokens: 100, OutputTokens: 50}, Provider: "anthropic", Model: "claude-x"})
		f.emit(fw.EvTurnFinished, fw.TurnData{Turn: 1})
		f.emit(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted})
	}
	branch := newFeed(tr, "01BRANCH", "reviewer")
	copies(branch, 7)
	branch.emit(fw.EvMessageReceived, fw.MessageReceivedData{Role: fw.RoleUser, Content: []fw.Block{fw.Text("again")}})
	branch.emit(fw.EvTurnStarted, fw.TurnData{Turn: 2})
	branch.emit(fw.EvModelRequested, fw.ModelRequestedData{Provider: "anthropic", Model: "claude-x"})
	branch.emit(fw.EvModelResponded, fw.ModelRespondedData{Content: []fw.Block{fw.Text("two")}, StopReason: fw.StopEndTurn,
		Usage: fw.Usage{InputTokens: 7, OutputTokens: 3}, Provider: "anthropic", Model: "claude-x"})
	branch.emit(fw.EvTurnFinished, fw.TurnData{Turn: 2})
	branch.emit(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted})
	copies(newFeed(tr, "01IDLE", "reviewer"), 7)

	spans := exp.GetSpans()
	chats := spansNamed(spans, "chat claude-x")
	if len(chats) != 1 {
		t.Fatalf("model spans = %d of %d spans, want the one own call", len(chats), len(spans))
	}
	if in, _ := attr(chats[0], "gen_ai.usage.input_tokens"); in.AsInt64() != 7 {
		t.Fatalf("the span of the own call has %d input tokens", in.AsInt64())
	}
	if runs := spansNamed(spans, SpanRun); len(runs) != 1 {
		t.Fatalf("run spans = %d, want the span of the own work of the branch", len(runs))
	}
}
