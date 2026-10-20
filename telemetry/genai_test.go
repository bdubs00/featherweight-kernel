package telemetry

import (
	"encoding/json"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestSpansCarryTheGenAIAgentConventions checks the attributes of the
// OpenTelemetry GenAI agent conventions on the run span (invoke_agent),
// the model span, and the tool spans (execute_tool) of the built-in
// tools, a skill load, an MCP tool, an A2A peer, and a Go tool.
func TestSpansCarryTheGenAIAgentConventions(t *testing.T) {
	tr, exp := newTestTracer()
	f := newFeed(tr, "01RUN", "reviewer")
	f.emit(fw.EvRunStarted, fw.RunStartedData{Model: "openai/qwen"})
	f.emit(fw.EvTurnStarted, fw.TurnData{Turn: 1})
	f.emit(fw.EvModelRequested, fw.ModelRequestedData{Provider: "openai", Model: "qwen"})
	f.emit(fw.EvModelResponded, fw.ModelRespondedData{StopReason: fw.StopToolUse, Provider: "openai", Model: "qwen"})
	calls := []fw.ToolRequestedData{
		{Tool: "bash", CallID: "c1", Source: "builtin"},
		{Tool: "skill", CallID: "c2", Source: "builtin", Input: json.RawMessage(`{"name":"ansible-role"}`)},
		{Tool: "github.create_pull_request", CallID: "c3", Source: "github"},
		{Tool: "ask_planner", CallID: "c4", Source: "planner"},
		{Tool: "echo", CallID: "c5"},
	}
	for _, c := range calls {
		f.emit(fw.EvToolRequested, c)
		f.emit(fw.EvToolResponded, fw.ToolRespondedData{CallID: c.CallID, Tool: c.Tool})
	}
	f.emit(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted})
	spans := exp.GetSpans()

	want := func(s tracetest.SpanStub, attrs map[string]string) {
		t.Helper()
		for k, v := range attrs {
			got, ok := attr(s, k)
			if v == "" {
				if ok {
					t.Errorf("%s: %s = %q, want none", s.Name, k, got.AsString())
				}
				continue
			}
			if !ok || got.AsString() != v {
				t.Errorf("%s: %s = %q, want %q", s.Name, k, got.AsString(), v)
			}
		}
	}
	runs := spansNamed(spans, SpanRun)
	if len(runs) != 1 {
		t.Fatalf("spans = %v", spans)
	}
	want(runs[0], map[string]string{
		"gen_ai.operation.name": "invoke_agent", "gen_ai.agent.name": "reviewer", "gen_ai.agent.id": "reviewer",
		"gen_ai.conversation.id": "01RUN", "gen_ai.request.model": "qwen",
	})
	want(spansNamed(spans, "chat qwen")[0], map[string]string{"gen_ai.operation.name": "chat", "gen_ai.conversation.id": "01RUN"})
	tool := func(name string) tracetest.SpanStub {
		t.Helper()
		s := spansNamed(spans, "execute_tool "+name)
		if len(s) != 1 {
			t.Fatalf("no span execute_tool %s in %v", name, spans)
		}
		return s[0]
	}
	common := map[string]string{"gen_ai.operation.name": "execute_tool", "gen_ai.agent.name": "reviewer", "gen_ai.conversation.id": "01RUN"}
	want(tool("bash"), common)
	cases := []struct {
		name  string
		attrs map[string]string
	}{
		{"bash", map[string]string{"gen_ai.tool.type": "function", "gen_ai.tool.call.id": "c1", "mcp.method.name": "", "gen_ai.skill.name": ""}},
		{"skill", map[string]string{"gen_ai.tool.type": "function", "gen_ai.skill.name": "ansible-role"}},
		{"github.create_pull_request", map[string]string{"gen_ai.tool.type": "extension", "mcp.method.name": "tools/call", "gen_ai.tool.name": "github.create_pull_request"}},
		{"ask_planner", map[string]string{"gen_ai.tool.type": "extension", "mcp.method.name": ""}},
		{"echo", map[string]string{"gen_ai.tool.type": "function", "mcp.method.name": ""}},
	}
	for _, c := range cases {
		want(tool(c.name), c.attrs)
	}
}
