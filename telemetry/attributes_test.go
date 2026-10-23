package telemetry

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestActorKind checks that the kind comes from the record, never from
// the shape of the subject, and that a kind that the kernel does not
// know, or a record without a kind, names no subject.
func TestActorKind(t *testing.T) {
	for _, c := range []struct{ subject, kind, want string }{
		{"", "", "none"},
		{fw.OriginKernel, fw.ActorKernel, "kernel"},
		{fw.KindToken, fw.KindToken, "token"},
		{"cli", fw.KindLocal, "local"},
		{"spiffe://td/ns/prod/sa/rev", fw.KindWorkload, "workload"},
		{fw.NewID(), fw.ActorParent, "parent"},
		{"alice@example.com", fw.ActorPerson, "person"},
		{"spiffe://td/ns/prod/sa/bob", fw.ActorPerson, "person"},
		{fw.NewID(), fw.ActorPerson, "person"},
		{"local:carol", fw.ActorPerson, "person"},
		{"anonymous", fw.KindAnonymous, "anonymous"},
		{"spiffe://td/ns/prod/sa/rev", "", "unknown"},
		{"alice@example.com", "alice@example.com", "unknown"},
	} {
		if got := actorKind(c.subject, c.kind); got != c.want {
			t.Errorf("actorKind(%q, %q) = %q, want %q", c.subject, c.kind, got, c.want)
		}
	}
}

// allAttributes returns every attribute of the spans and their events.
func allAttributes(spans tracetest.SpanStubs) []attribute.KeyValue {
	var out []attribute.KeyValue
	for _, s := range spans {
		out = append(out, s.Attributes...)
		for _, ev := range s.Events {
			out = append(out, ev.Attributes...)
		}
	}
	return out
}

// TestAttributesNameNoActor feeds a person, a parent run, a grader run, and
// the free text of a receipt, and makes sure that no span attribute and no
// quality attribute of a log record holds them.
func TestAttributesNameNoActor(t *testing.T) {
	tr, exp := newTestTracer()
	rec := &recorder{}
	logs := NewLogsFrom(sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(rec))))
	const person = "alice@example.com"
	const receipt = "order 4711 for alice@example.com"
	parent, grader := fw.NewID(), fw.NewID()
	f := newFeed(fw.ObserverFunc(func(e fw.Event) { tr.Observe(e); logs.Observe(e) }), "run", "default")
	f.emit(fw.EvRunStarted, fw.RunStartedData{})
	f.emit(fw.EvApprovalRequested, fw.ApprovalRequestedData{ApprovalID: "ap1", Tool: "rm"})
	f.emit(fw.EvApprovalDecided, fw.ApprovalDecidedData{ApprovalID: "ap1", Decision: "allow", Decider: person, DeciderKind: fw.ActorPerson})
	f.emit(fw.EvApprovalRequested, fw.ApprovalRequestedData{ApprovalID: "ap2", Tool: "rm"})
	f.emit(fw.EvApprovalDecided, fw.ApprovalDecidedData{ApprovalID: "ap2", Decision: "deny", Decider: parent, DeciderKind: fw.ActorParent})
	f.emit(fw.EvBudgetChanged, fw.BudgetChangedData{Budget: fw.Budget{MaxTurns: 3}, By: person, ByKind: fw.ActorPerson, Origin: fw.OriginHTTP})
	f.emit(fw.EvOutcomeEvaluated, fw.OutcomeEvaluatedData{Verdict: fw.GradeSatisfied, Grader: grader})
	fin := f.emit(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted})
	f.emit(fw.EvRunAnnotated, fw.QualityAnnotatedData{Quality: &fw.QualityObservation{ID: receipt, Completion: fin.Seq, Verification: "accepted"}, CostComplete: true})

	spans := exp.GetSpans()
	attrs := allAttributes(spans)
	kinds := map[string]bool{}
	for _, kv := range attrs {
		v := kv.Value.Emit()
		if strings.Contains(v, person) || v == parent || v == grader || strings.Contains(v, "4711") {
			t.Fatalf("span attribute %s holds %q", kv.Key, v)
		}
		switch kv.Key {
		case "fw.approval.decider", "fw.budget.by", "fw.outcome.grader", "fw.quality.receipt":
			t.Fatalf("span attribute %s is still there", kv.Key)
		case "fw.approval.decider_kind", "fw.budget.by_kind":
			kinds[string(kv.Key)+"="+v] = true
		}
	}
	for _, want := range []string{"fw.approval.decider_kind=person", "fw.approval.decider_kind=parent", "fw.budget.by_kind=person"} {
		if !kinds[want] {
			t.Fatalf("missing %s in %v", want, kinds)
		}
	}
	sum := sha256.Sum256([]byte(receipt))
	obs := spansNamed(spans, "fw.quality.observation")
	if len(obs) != 1 {
		t.Fatalf("observation spans = %d", len(obs))
	}
	if v, ok := attr(obs[0], "fw.quality.receipt_sha256"); !ok || v.AsString() != hex.EncodeToString(sum[:]) {
		t.Fatalf("receipt hash = %v", v)
	}
	if v, ok := attr(obs[0], "gen_ai.conversation.id"); !ok || v.AsString() != "run" {
		t.Fatalf("conversation = %v", v)
	}
	if _, ok := attr(obs[0], "fw.run.id"); ok {
		t.Fatal("the observation span names its run in fw.run.id")
	}
	graded := false
	for _, kv := range attrs {
		if kv.Key == "fw.outcome.graded" && kv.Value.AsBool() {
			graded = true
		}
	}
	if !graded {
		t.Fatal("the grader judgment lost fw.outcome.graded")
	}

	// The body of a log record is the event, as the ledger holds it. The
	// attributes from the quality facts name no actor and no receipt.
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, r := range rec.recs {
		for k, v := range attrsOf(r) {
			if !strings.HasPrefix(k, "fw.outcome.") && !strings.HasPrefix(k, "fw.quality.") {
				continue
			}
			if strings.Contains(v, person) || v == grader || strings.Contains(v, "4711") {
				t.Fatalf("log attribute %s holds %q", k, v)
			}
		}
	}
}
