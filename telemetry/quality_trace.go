package telemetry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// qualityAttributes returns the attributes of a grader judgment and of a
// receipt of an application, for the spans and the log records. No value
// names a person or a run, and none holds free text of an integration: the
// grader run is a boolean, and the receipt id is its SHA-256.
func qualityAttributes(e fw.Event) []attribute.KeyValue {
	if e.Type == fw.EvOutcomeEvaluated {
		var d fw.OutcomeEvaluatedData
		_ = e.Decode(&d)
		return []attribute.KeyValue{attribute.String("fw.outcome.basis", "grader_judgment"), attribute.String("fw.outcome.verdict", d.Verdict), attribute.Bool("fw.outcome.graded", d.Grader != ""), attribute.Int64("fw.outcome.answer", int64(d.Answer))}
	}
	if e.Type == fw.EvRunAnnotated {
		var d fw.QualityAnnotatedData
		if e.Decode(&d) == nil && d.Quality != nil {
			return []attribute.KeyValue{attribute.Bool("fw.quality.cost_complete", d.CostComplete), attribute.String("fw.quality.receipt_sha256", receiptHash(d.Quality.ID)), attribute.String("fw.quality.verification", d.Quality.Verification), attribute.Int64("fw.quality.completion", int64(d.Quality.Completion)), attribute.Int64("fw.quality.duplicate_effects", int64(d.Quality.DuplicateEffects)), attribute.Float64("fw.quality.cost_usd", d.CostUSD), attribute.Int("fw.quality.unpriced_tokens", d.UnpricedTokens), attribute.String("fw.quality.basis", "trusted_application")}
		}
	}
	return nil
}

// receiptHash returns the SHA-256 of a receipt id in hex, so a trace store
// can match the receipts of one id without reading the id. An empty id
// gives "".
func receiptHash(id string) string {
	if id == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

// Kinds of the actor of a decision or a budget change (actorKind).
const (
	actorNone     = "none"     // no actor recorded
	actorKernel   = "kernel"   // the kernel itself
	actorToken    = "token"    // the static bearer token of the process
	actorLocal    = "local"    // the local user, over stdio or fw run
	actorWorkload = "workload" // a workload with a SPIFFE identity
	actorParent   = "parent"   // a run: the parent of a child, or the run that a grader grades
	actorPerson   = "person"   // any other subject, such as the subject of an OIDC token
)

// actorKind returns the kind of the subject that decided an approval or
// changed a budget, never the subject: a span attribute names no person
// and no run.
func actorKind(subject string) string {
	switch {
	case subject == "":
		return actorNone
	case subject == fw.OriginKernel:
		return actorKernel
	case subject == fw.KindToken:
		return actorToken
	case subject == "cli", strings.HasPrefix(subject, "local:"):
		return actorLocal
	case strings.HasPrefix(subject, "spiffe://"):
		return actorWorkload
	}
	if _, ok := fw.IDTime(subject); ok {
		return actorParent
	}
	return actorPerson
}

// receiptSpan correlates late receipts without retaining finished spans or
// creating an apparently open execution. Logs keep the same envelope trace.
// The span names its run in gen_ai.conversation.id, as the model and the
// tool spans do.
func (t *Tracer) receiptSpan(e fw.Event, ts time.Time, attrs []attribute.KeyValue) {
	ctx := propagation.TraceContext{}.Extract(context.Background(), propagation.MapCarrier{"traceparent": e.TraceParent})
	attrs = append(attrs, attribute.String("gen_ai.conversation.id", e.Subject))
	_, span := t.tracer.Start(ctx, "fw.quality.observation", trace.WithTimestamp(ts), trace.WithAttributes(attrs...))
	span.End(trace.WithTimestamp(ts))
}

// traceRecovery adds the span event fw.recovery when a recovery ends.
// waiting tells that the run waited for an approval before e: the resume
// of such a run starts no clock, because the run waits for a person.
func traceRecovery(rt *runTrace, e fw.Event, ts time.Time, waiting bool) {
	defer func() { rt.last = ts }()
	if !rt.recovering.IsZero() {
		result := ""
		switch e.Type {
		case fw.EvModelResponded, fw.EvToolResponded, fw.EvToolProgress:
			result = "progress"
		case fw.EvApprovalRequested:
			result = "input_required"
		case fw.EvRunFinished:
			result = outcomeOf(e)
		case fw.EvRunSuspended, fw.EvRunResumed:
			result = "interrupted"
		}
		if result != "" {
			end := ts
			if e.Type == fw.EvRunResumed {
				end = rt.last
			}
			rt.span.AddEvent("fw.recovery", trace.WithTimestamp(ts), trace.WithAttributes(attribute.String("fw.recovery.result", result), attribute.Float64("fw.recovery.duration_seconds", end.Sub(rt.recovering).Seconds())))
			rt.recovering = time.Time{}
		}
	}
	if e.Type == fw.EvRunResumed && !waiting {
		rt.recovering = ts
	}
}
