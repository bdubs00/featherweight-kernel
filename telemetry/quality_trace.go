package telemetry

import (
	"context"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func qualityAttributes(e fw.Event) []attribute.KeyValue {
	if e.Type == fw.EvOutcomeEvaluated {
		var d fw.OutcomeEvaluatedData
		_ = e.Decode(&d)
		return []attribute.KeyValue{attribute.String("fw.outcome.basis", "grader_judgment"), attribute.String("fw.outcome.verdict", d.Verdict), attribute.String("fw.outcome.grader", d.Grader), attribute.Int64("fw.outcome.answer", int64(d.Answer))}
	}
	if e.Type == fw.EvRunAnnotated {
		var d fw.QualityAnnotatedData
		if e.Decode(&d) == nil && d.Quality != nil {
			return []attribute.KeyValue{attribute.Bool("fw.quality.cost_complete", d.CostComplete), attribute.String("fw.quality.receipt", d.Quality.ID), attribute.String("fw.quality.verification", d.Quality.Verification), attribute.Int64("fw.quality.completion", int64(d.Quality.Completion)), attribute.Int64("fw.quality.duplicate_effects", int64(d.Quality.DuplicateEffects)), attribute.Float64("fw.quality.cost_usd", d.CostUSD), attribute.Int("fw.quality.unpriced_tokens", d.UnpricedTokens), attribute.String("fw.quality.basis", "trusted_application")}
		}
	}
	return nil
}

// receiptSpan correlates late receipts without retaining finished spans or
// creating an apparently open execution. Logs keep the same envelope trace.
func (t *Tracer) receiptSpan(e fw.Event, ts time.Time, attrs []attribute.KeyValue) {
	ctx := propagation.TraceContext{}.Extract(context.Background(), propagation.MapCarrier{"traceparent": e.TraceParent})
	attrs = append(attrs, attribute.String("fw.run.id", e.Subject))
	_, span := t.tracer.Start(ctx, "fw.quality.observation", trace.WithTimestamp(ts), trace.WithAttributes(attrs...))
	span.End(trace.WithTimestamp(ts))
}

func traceRecovery(rt *runTrace, e fw.Event, ts time.Time) {
	defer func() { rt.last = ts }()
	if !rt.recovering.IsZero() {
		result := ""
		switch e.Type {
		case fw.EvModelResponded, fw.EvToolResponded, fw.EvToolProgress:
			result = "progress"
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
	if e.Type == fw.EvRunResumed {
		rt.recovering = ts
	}
}
