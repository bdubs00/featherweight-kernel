package telemetry

import (
	"context"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/prometheus/client_golang/prometheus"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
)

// This exercises the real Prometheus bridge and OTLP protobuf transport, not
// an SDK meter or a mock registry. In particular histogram counts and sums
// must survive the bridge alongside counter attributes.
func TestQualityMeasuresOnOTLPWire(t *testing.T) {
	c := newCollector(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", c.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "3600000")
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg, nil)
	f := newFeed(m, "secret-run", "default")
	f.emit(fw.EvRunQueued, fw.RunQueuedData{})
	f.emit(fw.EvRunResumed, fw.RunResumedData{})
	f.emit(fw.EvMessageReceived, fw.MessageReceivedData{Role: fw.RoleUser, Origin: fw.OriginHTTP})
	f.emit(fw.EvModelResponded, fw.ModelRespondedData{})
	f.emit(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted})
	f.emit(fw.EvRunAnnotated, fw.QualityAnnotatedData{Quality: &fw.QualityObservation{ID: "receipt", Completion: 5, Verification: "satisfied", DuplicateEffects: 2}, CostUSD: 0.25, CostComplete: true})
	stop, err := NewMetricsPush(context.Background(), reg, ServiceName, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	got := map[string]*metricspb.Metric{}
	for _, m := range c.metricData {
		got[m.Name] = m
	}
	for name, want := range map[string]float64{"fw_task_completions_total": 1, "fw_task_verifications_total": 1, "fw_operator_interventions_total": 1, "fw_confirmed_duplicate_effects_total": 2} {
		x := got[name]
		if x == nil || x.GetSum() == nil || !x.GetSum().IsMonotonic || len(x.GetSum().DataPoints) != 1 || x.GetSum().DataPoints[0].GetAsDouble() != want {
			t.Fatalf("wire counter %s=%v", name, x)
		}
	}
	for name, want := range map[string]float64{"fw_operator_interventions_per_run": 1, "fw_recovery_duration_seconds": 2, "fw_successful_task_cost_usd": 0.25} {
		x := got[name]
		if x == nil || x.GetHistogram() == nil || len(x.GetHistogram().DataPoints) != 1 {
			t.Fatalf("wire histogram %s=%v", name, x)
		}
		d := x.GetHistogram().DataPoints[0]
		if d.Count != 1 || d.GetSum() != want || len(d.ExplicitBounds) == 0 {
			t.Fatalf("wire histogram %s=%v", name, d)
		}
	}
	// The fail-stop gauge goes out like the others, as 0 for a healthy process.
	if x := got["fw_storage_blocked"]; x == nil || x.GetGauge() == nil || len(x.GetGauge().DataPoints) != 1 || x.GetGauge().DataPoints[0].GetAsDouble() != 0 {
		t.Fatalf("wire gauge fw_storage_blocked=%v", x)
	}
	for _, a := range got["fw_task_verifications_total"].GetSum().DataPoints[0].Attributes {
		if a.Key == "result" && a.Value.GetStringValue() == "satisfied" {
			return
		}
	}
	t.Fatal("verification result label did not survive OTLP bridge")
}
