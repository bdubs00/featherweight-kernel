package telemetry

import (
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// TestModelSpanCarriesTheWaitAndTheFirstByte checks that a model span
// tells the wait for a slot and the time to the first byte apart from
// the whole call.
func TestModelSpanCarriesTheWaitAndTheFirstByte(t *testing.T) {
	tr, exp := newTestTracer()
	f := newFeed(tr, "01RUN", "migrator")
	f.emit(fw.EvRunStarted, fw.RunStartedData{Model: "openai/qwen"})
	f.emit(fw.EvTurnStarted, fw.TurnData{Turn: 1})
	f.emit(fw.EvModelRequested, fw.ModelRequestedData{Provider: "openai", Model: "qwen"})
	f.emit(fw.EvModelResponded, fw.ModelRespondedData{
		Content: []fw.Block{fw.Text("done")}, StopReason: fw.StopEndTurn,
		LatencyMS: 9000, FirstByteMS: 6000, QueuedMS: 2000, Provider: "openai", Model: "qwen",
	})
	f.emit(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted})
	models := spansNamed(exp.GetSpans(), "chat qwen")
	if len(models) != 1 {
		t.Fatalf("spans = %v", exp.GetSpans())
	}
	for key, want := range map[string]int64{"fw.latency_ms": 9000, "fw.ttfb_ms": 6000, "fw.queued_ms": 2000} {
		if v, ok := attr(models[0], key); !ok || v.AsInt64() != want {
			t.Fatalf("%s = %v, want %d", key, v.Emit(), want)
		}
	}
}
