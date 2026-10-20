package telemetry

import (
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// TestToolSpanCarriesTheExitCode checks process.exit.code, which the
// GenAI conventions name for a span of a tool that runs one command.
func TestToolSpanCarriesTheExitCode(t *testing.T) {
	tr, exp := newTestTracer()
	f := newFeed(tr, "01RUN", "migrator")
	f.emit(fw.EvRunStarted, fw.RunStartedData{})
	f.emit(fw.EvToolRequested, fw.ToolRequestedData{Tool: "bash", CallID: "c1"})
	code := 2
	f.emit(fw.EvToolResponded, fw.ToolRespondedData{CallID: "c1", Tool: "bash", ExitCode: &code})
	f.emit(fw.EvToolRequested, fw.ToolRequestedData{Tool: "echo", CallID: "c2"})
	f.emit(fw.EvToolResponded, fw.ToolRespondedData{CallID: "c2", Tool: "echo"})
	f.emit(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted})
	spans := exp.GetSpans()
	bash, echo := spansNamed(spans, "execute_tool bash"), spansNamed(spans, "execute_tool echo")
	if len(bash) != 1 || len(echo) != 1 {
		t.Fatalf("spans = %v", spans)
	}
	if v, ok := attr(bash[0], "process.exit.code"); !ok || v.AsInt64() != 2 {
		t.Fatalf("bash attrs = %v", bash[0].Attributes)
	}
	if _, ok := attr(echo[0], "process.exit.code"); ok {
		t.Fatalf("echo has an exit code: %v", echo[0].Attributes)
	}
}
