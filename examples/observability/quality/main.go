// Run with: go run ./examples/observability/quality
// This local integration uses no model provider or external service.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/telemetry"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
)

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	reg := prometheus.NewRegistry()
	prices := fw.PriceTable{"local/demo": {}} // Explicit zero price, not unknown.
	metrics := telemetry.NewMetrics(reg, prices)
	// The delivery system's committed rows, not model text or tool intents,
	// establish what actually happened. This stands in for querying the
	// application's authoritative delivery database with a stable operation key.
	var committedDeliveries []string
	deliver := fw.NewTool(fw.ToolSpec{Name: "deliver", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, json.RawMessage) (fw.ToolResult, error) {
		// Deliberately faulty integration: a retry created another real row.
		committedDeliveries = append(committedDeliveries, "task-operation", "task-operation")
		return fw.ToolResult{Content: []fw.Block{fw.Text("delivered")}}, nil
	})
	calls := 0
	model := fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
		calls++
		r := &fw.Response{Provider: "local", Model: "demo", Usage: fw.Usage{InputTokens: 1}, StopReason: fw.StopEndTurn, Content: []fw.Block{fw.Text("done")}}
		if calls == 1 {
			r.StopReason = fw.StopToolUse
			r.Content = []fw.Block{{Type: fw.BlockToolUse, ID: "call", Name: "deliver", Input: json.RawMessage(`{}`)}}
		}
		return r, nil
	}}
	a, err := fw.New(fw.WithoutDefaults(), fw.WithAuthn(fw.NoAuthn{}), fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()), fw.WithModel(model), fw.WithModelName("local/demo"), fw.WithPrices(prices), fw.WithTools(deliver))
	if err != nil {
		return err
	}
	defer a.Close(ctx)
	metrics.Attach(a)
	r, err := a.Run(ctx, "Deliver the task notification")
	if err != nil {
		return err
	}
	if r.State != fw.StateCompleted || len(committedDeliveries) == 0 {
		return fmt.Errorf("integration did not deliver")
	}
	// Query the external system after the run and all cost annotations settle.
	// One delivery satisfies the task; each additional committed row is a
	// confirmed duplicate. Prevented attempts would not be counted here.
	receipt := fw.QualityObservation{ID: "delivery-reconciliation-v1", Completion: r.Seq, Verification: "satisfied", DuplicateEffects: uint64(len(committedDeliveries) - 1)}
	if err := a.ObserveQuality(ctx, r.ID, receipt); err != nil {
		return err
	}
	// Retrying the same durable receipt does not count it twice.
	if err := a.ObserveQuality(ctx, r.ID, receipt); err != nil {
		return err
	}
	families, err := reg.Gather()
	if err != nil {
		return err
	}
	for _, f := range families {
		if f.GetName() == "fw_task_verifications_total" || f.GetName() == "fw_confirmed_duplicate_effects_total" || f.GetName() == "fw_successful_task_cost_usd" {
			if _, err := expfmt.MetricFamilyToText(os.Stdout, f); err != nil {
				return err
			}
		}
	}
	return nil
}
