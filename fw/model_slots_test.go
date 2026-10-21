package fw

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestModelSlotsHoldACallAndRecordTheWait runs two runs on a provider with
// one slot. The second call waits until the first one ends, and its
// fw.model.responded records the wait in queuedMs.
func TestModelSlotsHoldACallAndRecordTheWait(t *testing.T) {
	ctx := testCtx(t)
	var inFlight, most atomic.Int32
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	m := ModelFunc{StreamFn: func(ctx context.Context, _ *Request, _ func(Delta)) (*Response, error) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			old := most.Load()
			if n <= old || most.CompareAndSwap(old, n) {
				break
			}
		}
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return endTurn("done"), nil
	}}
	cfg := DefaultConfig()
	cfg.Model = "fake/slot"
	cfg.Providers = map[string]ProviderConfig{"fake": {MaxConcurrent: 1}}
	a := newTestAgent(t, m, WithConfig(cfg))

	first, err := a.Start(ctx, "one")
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	second, err := a.Start(ctx, "two")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
		t.Fatal("the second call reached the model while the only slot was taken")
	case <-time.After(150 * time.Millisecond):
	}
	release <- struct{}{}
	<-entered
	release <- struct{}{}
	for _, id := range []string{first.ID, second.ID} {
		r, err := a.Wait(ctx, id)
		mustState(t, r, err, StateCompleted)
	}
	if most.Load() != 1 {
		t.Fatalf("%d calls were in flight at once, want 1", most.Load())
	}
	queued := func(id string) int64 {
		var d ModelRespondedData
		firstOf(t, ledgerEvents(t, a.Ledger(), id), EvModelResponded, &d)
		if d.LatencyMS < d.QueuedMS {
			t.Fatalf("latencyMs %d is less than queuedMs %d", d.LatencyMS, d.QueuedMS)
		}
		return d.QueuedMS
	}
	if q := queued(first.ID); q > 50 {
		t.Fatalf("the first call waited %d ms for a free slot", q)
	}
	if q := queued(second.ID); q < 140 {
		t.Fatalf("queuedMs of the second call = %d, want at least 140", q)
	}
}

// TestModelSlotsOffByDefault checks that a provider without maxConcurrent
// lets calls run together.
func TestModelSlotsOffByDefault(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Providers = map[string]ProviderConfig{"fake": {}}
	if slots := modelSlots(cfg); len(slots) != 0 {
		t.Fatalf("slots = %v", slots)
	}
	m := ModelFunc{GenerateFn: func(context.Context, *Request) (*Response, error) { return endTurn("x"), nil }}
	if got := limitModel(modelSlots(cfg), "fake/x", m); got == nil {
		t.Fatal("limitModel returned nil")
	} else if _, ok := got.(*slotModel); ok {
		t.Fatal("a provider without a limit got slots")
	}
}
