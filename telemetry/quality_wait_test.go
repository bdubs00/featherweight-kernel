package telemetry

import (
	"context"
	"iter"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/prometheus/client_golang/prometheus"
)

var recoveryResults = []string{"progress", "input_required", "interrupted", fw.OutcomeCompleted, fw.OutcomeFailed, fw.OutcomeCanceled}

// recoverySamples counts the samples of fw_recovery_duration_seconds by
// result.
func recoverySamples(t *testing.T, reg prometheus.Gatherer) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for _, r := range recoveryResults {
		if n, ok := lookup(t, reg, "fw_recovery_duration_seconds", map[string]string{"result": r}); ok && n > 0 {
			out[r] = n
		}
	}
	return out
}

// TestRecoveryOnlyForWorkingRuns: a run that waits for a decision at a
// restart waits for a person, so its resume starts no recovery clock, and
// its next suspend or resume adds no interrupted sample. A run that was
// working still recovers, and an approval request ends its recovery.
func TestRecoveryOnlyForWorkingRuns(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg, nil)
	tr, exp := newTestTracer()
	obs := fw.ObserverFunc(func(e fw.Event) { m.Observe(e); tr.Observe(e) })

	f := newFeed(obs, "waits", "default")
	f.emit(fw.EvRunQueued, fw.RunQueuedData{})
	f.emit(fw.EvRunStarted, fw.RunStartedData{})
	f.emit(fw.EvApprovalRequested, fw.ApprovalRequestedData{ApprovalID: "ap1", Tool: "rm"})
	f.emit(fw.EvRunSuspended, fw.RunSuspendedData{})
	f.at = f.at.Add(time.Hour)
	f.emit(fw.EvRunResumed, fw.RunResumedData{})
	f.emit(fw.EvRunSuspended, fw.RunSuspendedData{})
	f.at = f.at.Add(time.Hour)
	f.emit(fw.EvRunResumed, fw.RunResumedData{})
	f.at = f.at.Add(time.Hour)
	f.emit(fw.EvApprovalDecided, fw.ApprovalDecidedData{ApprovalID: "ap1", Decision: "allow", Decider: "alice@example.com"})
	f.emit(fw.EvToolResponded, fw.ToolRespondedData{CallID: "c1"})
	f.emit(fw.EvModelResponded, fw.ModelRespondedData{})
	f.emit(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted})
	if got := recoverySamples(t, reg); len(got) != 0 {
		t.Fatalf("a run that waited for input got recovery samples %v", got)
	}

	// A branch that begins with a copied approval request waits too.
	b := newFeed(obs, "branch", "default")
	b.emit(fw.EvRunQueued, fw.RunQueuedData{Kept: 2})
	b.emit(fw.EvApprovalRequested, fw.ApprovalRequestedData{ApprovalID: "ap9"})
	b.emit(fw.EvRunResumed, fw.RunResumedData{})
	b.emit(fw.EvRunSuspended, fw.RunSuspendedData{})
	if got := recoverySamples(t, reg); len(got) != 0 {
		t.Fatalf("a branch that waits for input got recovery samples %v", got)
	}
	for _, s := range exp.GetSpans() {
		for _, ev := range s.Events {
			if ev.Name == "fw.recovery" {
				t.Fatalf("span %s has a recovery event for a run that waits", s.Name)
			}
		}
	}

	g := newFeed(obs, "works", "default")
	g.emit(fw.EvRunQueued, fw.RunQueuedData{})
	g.emit(fw.EvRunStarted, fw.RunStartedData{})
	g.emit(fw.EvModelRequested, fw.ModelRequestedData{})
	g.emit(fw.EvRunSuspended, fw.RunSuspendedData{})
	g.at = g.at.Add(time.Hour)
	g.emit(fw.EvRunResumed, fw.RunResumedData{})
	g.emit(fw.EvModelResponded, fw.ModelRespondedData{})
	g.emit(fw.EvRunSuspended, fw.RunSuspendedData{})
	g.at = g.at.Add(time.Hour)
	g.emit(fw.EvRunResumed, fw.RunResumedData{})
	g.emit(fw.EvApprovalRequested, fw.ApprovalRequestedData{ApprovalID: "ap2"})
	g.at = g.at.Add(time.Hour)
	g.emit(fw.EvApprovalDecided, fw.ApprovalDecidedData{ApprovalID: "ap2", Decision: "allow"})
	g.emit(fw.EvModelResponded, fw.ModelRespondedData{})
	g.emit(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted})
	got := recoverySamples(t, reg)
	if got["progress"] != 1 || got["input_required"] != 1 || len(got) != 2 {
		t.Fatalf("working run recovery samples = %v", got)
	}
	if sum := histogramSum(t, reg, "fw_recovery_duration_seconds", map[string]string{"result": "input_required"}); sum != 1 {
		t.Fatalf("the decision time counted as recovery: %v", sum)
	}
	results := map[string]int{}
	for _, s := range exp.GetSpans() {
		for _, ev := range s.Events {
			if ev.Name != "fw.recovery" {
				continue
			}
			for _, kv := range ev.Attributes {
				if kv.Key == "fw.recovery.result" {
					results[kv.Value.AsString()]++
				}
			}
		}
	}
	if results["progress"] != 1 || results["input_required"] != 1 || len(results) != 2 {
		t.Fatalf("recovery span events = %v", results)
	}
}

// blockingReadLedger blocks every Read until release closes.
type blockingReadLedger struct {
	fw.Ledger
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (l *blockingReadLedger) Read(ctx context.Context, id string, from uint64) iter.Seq2[fw.Event, error] {
	return func(yield func(fw.Event, error) bool) {
		l.once.Do(func() { close(l.entered) })
		<-l.release
		for e, err := range l.Ledger.Read(ctx, id, from) {
			if !yield(e, err) {
				return
			}
		}
	}
}

// TestQualitySeedReadsNoLedgerInObserve: the kernel notifies observers
// inside the append of an event, so the observer of a run with unseen
// history must not read the ledger there. The seed runs in the background,
// and the events that wait for it count once, in order.
func TestQualitySeedReadsNoLedgerInObserve(t *testing.T) {
	ctx := context.Background()
	base := fw.NewMemLedger()
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	for i, item := range []struct {
		typ  string
		data any
	}{
		{fw.EvRunQueued, fw.RunQueuedData{Agent: "default", Origin: fw.OriginHTTP}},
		{fw.EvMessageReceived, fw.MessageReceivedData{Role: fw.RoleUser, Origin: fw.OriginCLI}},
		{fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted}},
		{fw.EvBudgetChanged, fw.BudgetChangedData{Budget: fw.Budget{MaxTurns: 9}, By: "alice@example.com", Origin: fw.OriginHTTP}},
	} {
		e := fw.MustEvent("old", item.typ, item.data)
		e.Time = at.Add(time.Duration(i) * time.Second)
		if _, err := base.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	l := &blockingReadLedger{Ledger: base, entered: make(chan struct{}), release: make(chan struct{})}
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg, nil)
	m.quality.ledger = l
	follow := fw.MustEvent("old", fw.EvMessageReceived, fw.MessageReceivedData{Role: fw.RoleUser, Origin: fw.OriginHTTP})
	follow.Seq, follow.Time = 5, at.Add(10*time.Second)
	fin := fw.MustEvent("old", fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted})
	fin.Seq, fin.Time = 6, at.Add(11*time.Second)
	done := make(chan struct{})
	go func() {
		m.Observe(follow)
		m.Observe(fin)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		close(l.release)
		t.Fatal("Observe waited for a ledger read")
	}
	select {
	case <-l.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the background seed never read the ledger")
	}
	if _, ok := lookup(t, reg, "fw_task_completions_total", map[string]string{"execution": "completed", "grader_judgment": "unverified"}); ok {
		t.Fatal("the finish counted before its seed")
	}
	close(l.release)
	m.quality.wait()
	if got := value(t, reg, "fw_task_completions_total", map[string]string{"execution": "completed", "grader_judgment": "unverified"}); got != 1 {
		t.Fatalf("completions = %v", got)
	}
	// The episode of the follow-up holds the late budget change from the
	// ledger and the follow-up itself, and the history counts nothing again.
	if got := histogramSum(t, reg, "fw_operator_interventions_per_run", map[string]string{"coverage": "complete"}); got != 2 {
		t.Fatalf("interventions of the episode = %v", got)
	}
	if got := value(t, reg, "fw_operator_interventions_total", map[string]string{"kind": "followup"}); got != 1 {
		t.Fatalf("followups = %v", got)
	}
	if _, ok := lookup(t, reg, "fw_operator_interventions_total", map[string]string{"kind": "budget_change"}); ok {
		t.Fatal("the seed recounted the history")
	}
	if len(m.quality.runs) != 0 {
		t.Fatalf("retained %d runs", len(m.quality.runs))
	}
}
