package telemetry

import (
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/prometheus/client_golang/prometheus"
)

// TestNotesAfterTheEndLeaveARunFinished proves that the record and the
// report of a child that arrive after its parent finished, as for a child
// that ends after its parent, neither count the parent as active again nor
// open a run span that never ends.
func TestNotesAfterTheEndLeaveARunFinished(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg, nil)
	tr, exp := newTestTracer()
	for _, obs := range []fw.Observer{m, tr} {
		f := newFeed(obs, "r1", "default")
		f.emit(fw.EvRunQueued, fw.RunQueuedData{Agent: "default", Input: "work"})
		f.emit(fw.EvRunStarted, fw.RunStartedData{})
		f.emit(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted})
		f.emit(fw.EvRunChild, fw.RunChildData{Child: "c1", Member: "remote", Started: true})
		f.emit(fw.EvChildReported, fw.ChildReportedData{Child: "c1", State: fw.StateCompleted})
	}
	if got := value(t, reg, "fw_runs_active", map[string]string{"agent": "default"}); got != 0 {
		t.Errorf("fw_runs_active = %v after notes on a finished run", got)
	}
	tr.endAll(time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC))
	if runs := spansNamed(exp.GetSpans(), SpanRun); len(runs) != 1 {
		t.Errorf("run spans = %d, want the one of the run", len(runs))
	}
}
