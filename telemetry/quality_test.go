package telemetry

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/prometheus/client_golang/prometheus"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

func TestQualityExecutionIsNotVerification(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg, nil)
	f := newFeed(m, "private-run", "default")
	f.emit(fw.EvRunQueued, fw.RunQueuedData{})
	f.emit(fw.EvOutcomeEvaluated, fw.OutcomeEvaluatedData{Verdict: fw.GradeSatisfied, Grader: "grader-private"})
	fin := f.emit(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted})
	if got := value(t, reg, "fw_task_completions_total", map[string]string{"execution": "completed", "grader_judgment": "satisfied"}); got != 1 {
		t.Fatal(got)
	}
	if _, ok := lookup(t, reg, "fw_task_verifications_total", map[string]string{"result": "satisfied"}); ok {
		t.Fatal("model judgment asserted external verification")
	}
	f.emit(fw.EvRunAnnotated, fw.QualityAnnotatedData{Quality: &fw.QualityObservation{ID: "private-receipt", Completion: fin.Seq, Verification: "satisfied", DuplicateEffects: 2}, CostComplete: true, CostUSD: 0.5})
	if got := value(t, reg, "fw_confirmed_duplicate_effects_total", nil); got != 2 {
		t.Fatal(got)
	}
	if got := histogramSum(t, reg, "fw_successful_task_cost_usd", map[string]string{"basis": "satisfied"}); got != 0.5 {
		t.Fatal(got)
	}
	if len(m.quality.runs) != 0 || len(m.runs) != 0 {
		t.Fatal("late receipt reopened execution")
	}
	// Unknown prices never enter the successful-cost denominator.
	f.emit(fw.EvRunAnnotated, fw.QualityAnnotatedData{Quality: &fw.QualityObservation{Verification: "accepted"}, UnpricedTokens: 1})
	if got := value(t, reg, "fw_successful_task_cost_excluded_total", map[string]string{"reason": "unpriced_tokens"}); got != 1 {
		t.Fatal(got)
	}
	mfs, _ := reg.Gather()
	for _, mf := range mfs {
		for _, s := range mf.Metric {
			for _, l := range s.Label {
				if l.GetValue() == "private-run" || l.GetValue() == "private-receipt" || l.GetValue() == "grader-private" {
					t.Fatal("private metric label", l)
				}
			}
		}
	}
}

func TestQualityInterventionsAndZeroEpisodes(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg, nil)
	f := newFeed(m, "r", "default")
	f.emit(fw.EvRunQueued, fw.RunQueuedData{Origin: fw.OriginHTTP, Parent: "parent"})
	for _, origin := range []string{fw.OriginKernel, fw.OriginBus, fw.OriginTool, fw.OriginA2A} {
		f.emit(fw.EvMessageReceived, fw.MessageReceivedData{Role: fw.RoleUser, Origin: origin})
	}
	f.emit(fw.EvApprovalDecided, fw.ApprovalDecidedData{Decider: "parent"})
	f.emit(fw.EvApprovalDecided, fw.ApprovalDecidedData{})
	f.emit(fw.EvBudgetChanged, fw.BudgetChangedData{Origin: fw.OriginHTTP})
	f.emit(fw.EvMessageReceived, fw.MessageReceivedData{Role: fw.RoleUser, Origin: fw.OriginCLI})
	f.emit(fw.EvApprovalDecided, fw.ApprovalDecidedData{Decider: "person"})
	f.emit(fw.EvBudgetChanged, fw.BudgetChangedData{By: "person", Origin: fw.OriginMCP})
	f.emit(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeFailed})
	if got := histogramSum(t, reg, "fw_operator_interventions_per_run", map[string]string{"coverage": "complete"}); got != 3 {
		t.Fatal(got)
	}
	g := newFeed(m, "zero", "default")
	g.emit(fw.EvRunQueued, fw.RunQueuedData{Origin: fw.OriginKernel})
	g.emit(fw.EvApprovalDecided, fw.ApprovalDecidedData{Decider: "automated-grader-parent"})
	g.emit(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted})
	if got := value(t, reg, "fw_operator_interventions_per_run", map[string]string{"coverage": "complete"}); got != 2 {
		t.Fatal("zero intervention episode missing", got)
	}
	for _, kind := range []string{"followup", "decision", "budget_change"} {
		if got := value(t, reg, "fw_operator_interventions_total", map[string]string{"kind": kind}); got != 1 {
			t.Fatal(kind, got)
		}
	}
}

func TestQualityRecoveryAndAmbiguousRetries(t *testing.T) {
	for _, end := range []string{"progress", fw.OutcomeCanceled, fw.OutcomeFailed, "interrupted"} {
		t.Run(end, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			m := NewMetrics(reg, nil)
			f := newFeed(m, "r", "default")
			f.emit(fw.EvRunQueued, fw.RunQueuedData{})
			f.emit(fw.EvToolRequested, fw.ToolRequestedData{CallID: "call", Tool: "send"})
			f.emit(fw.EvRunSuspended, fw.RunSuspendedData{})
			f.at = f.at.Add(24 * time.Hour)
			f.emit(fw.EvRunResumed, fw.RunResumedData{})
			f.emit(fw.EvRunAnnotated, fw.RunAnnotatedData{Metadata: map[string]string{"title": "late"}})
			f.emit(fw.EvToolRequested, fw.ToolRequestedData{CallID: "call", Tool: "send"})
			switch end {
			case "progress":
				f.emit(fw.EvToolProgress, fw.ToolProgressData{CallID: "call"})
			case "interrupted":
				f.emit(fw.EvRunSuspended, fw.RunSuspendedData{})
			default:
				f.emit(fw.EvRunFinished, fw.RunFinishedData{Outcome: end})
			}
			if got := histogramSum(t, reg, "fw_recovery_duration_seconds", map[string]string{"result": end}); got != 3 {
				t.Fatalf("downtime or annotations ended recovery: %v", got)
			}
			if got := value(t, reg, "fw_ambiguous_tool_retries_total", nil); got != 1 {
				t.Fatal(got)
			}
			if got := value(t, reg, "fw_confirmed_duplicate_effects_total", nil); got != 0 {
				t.Fatal("retry invented duplicate effect", got)
			}
		})
	}
}

func TestQualityAttachSeedsResumedRun(t *testing.T) {
	l := fw.NewMemLedger()
	ctx := context.Background()
	at := time.Now().Add(-time.Hour)
	events := []fw.Event{
		fw.MustEvent("r", fw.EvRunQueued, fw.RunQueuedData{Agent: "default", Origin: fw.OriginHTTP}),
		fw.MustEvent("r", fw.EvMessageReceived, fw.MessageReceivedData{Role: fw.RoleUser, Origin: fw.OriginCLI}),
		fw.MustEvent("r", fw.EvRunSuspended, fw.RunSuspendedData{}),
		fw.MustEvent("r", fw.EvRunResumed, fw.RunResumedData{}),
	}
	for i, e := range events {
		e.Time = at.Add(time.Duration(i) * time.Second)
		if _, err := l.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	a := newAgent(t, (&script{}).model(), fw.WithLedger(l), fw.WithoutResume())
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg, nil)
	m.Attach(a)
	e := fw.MustEvent("r", fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCanceled})
	e.Seq = 5
	e.Time = at.Add(8 * time.Second)
	m.Observe(e)
	m.quality.wait()
	if got := histogramSum(t, reg, "fw_recovery_duration_seconds", map[string]string{"result": "canceled"}); got != 5 {
		t.Fatal(got)
	}
	if got := histogramSum(t, reg, "fw_operator_interventions_per_run", map[string]string{"coverage": "complete"}); got != 1 {
		t.Fatal("seed lost intervention", got)
	}
	if _, ok := lookup(t, reg, "fw_operator_interventions_total", map[string]string{"kind": "followup"}); ok {
		t.Fatal("Attach recounted historical counter")
	}
}

func TestQualityRecoveryCrashGapIsNotRecoveryTime(t *testing.T) {
	for _, attach := range []bool{false, true} {
		t.Run(fmt.Sprint("attach=", attach), func(t *testing.T) {
			ctx := context.Background()
			reg := prometheus.NewRegistry()
			m := NewMetrics(reg, nil)
			l := fw.NewMemLedger()
			base := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
			for i, item := range []struct {
				second int
				typ    string
				data   any
			}{
				{0, fw.EvRunQueued, fw.RunQueuedData{Agent: "default"}},
				{10, fw.EvRunResumed, fw.RunResumedData{}},
				{11, fw.EvModelRequested, fw.ModelRequestedData{}},
			} {
				e := fw.MustEvent("crashed", item.typ, item.data)
				e.Seq = uint64(i + 1)
				e.Time = base.Add(time.Duration(item.second) * time.Second)
				if _, err := l.Append(ctx, e); err != nil {
					t.Fatal(err)
				}
				if !attach {
					m.Observe(e)
				}
			}
			if attach {
				a := newAgent(t, (&script{}).model(), fw.WithLedger(l), fw.WithoutResume())
				m.Attach(a)
			}
			resume := fw.MustEvent("crashed", fw.EvRunResumed, fw.RunResumedData{})
			resume.Seq = 4
			resume.Time = base.Add(100 * time.Second)
			m.Observe(resume)
			progress := fw.MustEvent("crashed", fw.EvModelResponded, fw.ModelRespondedData{})
			progress.Seq = 5
			progress.Time = base.Add(101 * time.Second)
			m.Observe(progress)
			m.quality.wait()
			if got := histogramSum(t, reg, "fw_recovery_duration_seconds", map[string]string{"result": "interrupted"}); got != 1 {
				t.Fatalf("old incomplete attempt included crash downtime: %v", got)
			}
			if got := histogramSum(t, reg, "fw_recovery_duration_seconds", map[string]string{"result": "progress"}); got != 1 {
				t.Fatalf("fresh recovery=%v", got)
			}
		})
	}
}

func TestQualityForkCopiesAndCompletedStateAreNotRetained(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg, nil)
	for i := 0; i < 1000; i++ {
		f := newFeed(m, fmt.Sprint(i), "default")
		f.emit(fw.EvRunQueued, fw.RunQueuedData{Kept: 4})
		f.emit(fw.EvRunResumed, fw.RunResumedData{})
		f.emit(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted})
		f.emit(fw.EvRunAnnotated, fw.QualityAnnotatedData{Quality: &fw.QualityObservation{Verification: "satisfied", DuplicateEffects: 1}, CostComplete: true})
	}
	if len(m.quality.runs) != 0 {
		t.Fatalf("retained %d copied terminal runs", len(m.quality.runs))
	}
	if got := value(t, reg, "fw_confirmed_duplicate_effects_total", nil); got != 0 {
		t.Fatal("copied receipts recounted", got)
	}
	if _, ok := lookup(t, reg, "fw_task_completions_total", map[string]string{"execution": "completed", "grader_judgment": "unverified"}); ok {
		t.Fatal("copied completion counted as own work")
	}
}

func TestQualityLateBudgetAndFollowupHaveOneNextEpisode(t *testing.T) {
	ctx := context.Background()
	l := fw.NewMemLedger()
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg, nil)
	m.quality.ledger = l
	f := newFeed(fw.ObserverFunc(func(e fw.Event) {
		if _, err := l.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
		m.Observe(e)
	}), "r", "default")
	f.emit(fw.EvRunQueued, fw.RunQueuedData{Origin: fw.OriginHTTP})
	f.emit(fw.EvApprovalDecided, fw.ApprovalDecidedData{Decider: "person"})
	f.emit(fw.EvOutcomeEvaluated, fw.OutcomeEvaluatedData{Verdict: fw.GradeSatisfied})
	f.emit(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted})
	f.emit(fw.EvBudgetChanged, fw.BudgetChangedData{By: "person", Origin: fw.OriginHTTP})
	f.emit(fw.EvRunAnnotated, fw.RunAnnotatedData{Usage: &fw.Usage{InputTokens: 7}})
	if len(m.quality.runs) != 0 {
		t.Fatal("late passive events reopened quality state")
	}
	f.emit(fw.EvMessageReceived, fw.MessageReceivedData{Role: fw.RoleUser, Origin: fw.OriginHTTP})
	f.emit(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted})
	m.quality.wait()
	if got := histogramSum(t, reg, "fw_operator_interventions_per_run", map[string]string{"coverage": "complete"}); got != 3 {
		t.Fatalf("late budget was lost or counted twice: %v", got)
	}
	if got := value(t, reg, "fw_operator_interventions_total", map[string]string{"kind": "budget_change"}); got != 1 {
		t.Fatal(got)
	}
	if got := value(t, reg, "fw_task_completions_total", map[string]string{"execution": "completed", "grader_judgment": "unverified"}); got != 1 {
		t.Fatal("followup inherited stale grade", got)
	}
}

func TestQualityPartialObserverHistoryIsNotZeroCompleteHistory(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg, nil)
	m.Observe(fw.MustEvent("unseen", fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted}))
	if got := value(t, reg, "fw_operator_interventions_per_run", map[string]string{"coverage": "partial"}); got != 1 {
		t.Fatal(got)
	}
	if got := value(t, reg, "fw_task_completions_total", map[string]string{"execution": "completed", "grader_judgment": "unknown"}); got != 1 {
		t.Fatal(got)
	}
	if _, ok := lookup(t, reg, "fw_operator_interventions_per_run", map[string]string{"coverage": "complete"}); ok {
		t.Fatal("partial history asserted zero interventions in a complete episode")
	}
}

func TestQualityReceiptAndGraderTraceLogFacts(t *testing.T) {
	tr, exp := newTestTracer()
	rec := &recorder{}
	logs := NewLogsFrom(sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(rec))))
	f := newFeed(fw.ObserverFunc(func(e fw.Event) { tr.Observe(e); logs.Observe(e) }), "run", "default")
	f.emit(fw.EvRunStarted, fw.RunStartedData{})
	f.emit(fw.EvOutcomeEvaluated, fw.OutcomeEvaluatedData{Verdict: fw.GradeSatisfied})
	fin := f.emit(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted})
	f.emit(fw.EvRunAnnotated, fw.QualityAnnotatedData{Quality: &fw.QualityObservation{ID: "receipt", Completion: fin.Seq, Verification: "satisfied", DuplicateEffects: 1}, CostComplete: true})
	if len(tr.runs) != 0 {
		t.Fatal("receipt reopened trace")
	}
	if spans := spansNamed(exp.GetSpans(), "fw.quality.observation"); len(spans) != 1 {
		t.Fatal("missing late observation span")
	}
	if got := attrsOf(rec.recs[1])["fw.outcome.basis"]; got != "grader_judgment" {
		t.Fatal(got)
	}
	if got := attrsOf(rec.recs[3])["fw.quality.basis"]; got != "trusted_application" {
		t.Fatal(got)
	}
}
