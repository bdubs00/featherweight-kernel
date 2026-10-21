package fw

import (
	"errors"
	"testing"
)

func qualityReceipt(t *testing.T, a *Agent, id string) QualityAnnotatedData {
	t.Helper()
	var got QualityAnnotatedData
	for e, err := range a.Ledger().Read(testCtx(t), id, 0) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Type == EvRunAnnotated {
			var d QualityAnnotatedData
			_ = e.Decode(&d)
			if d.Quality != nil {
				got = d
			}
		}
	}
	return got
}

func TestQualityReceiptSurvivesRestartWithoutReopening(t *testing.T) {
	ctx := testCtx(t)
	l := NewMemLedger()
	a := newTestAgent(t, newScript(endTurn("done")).Model(), WithLedger(l))
	r, err := a.Run(ctx, "do work")
	mustState(t, r, err, StateCompleted)
	d := QualityObservation{ID: "external-receipt-1", Completion: r.Seq, Verification: "satisfied", DuplicateEffects: 2}
	if err := a.ObserveQuality(ctx, r.ID, d); err != nil {
		t.Fatal(err)
	}
	before := len(ledgerEvents(t, l, r.ID))
	got, err := a.Get(ctx, r.ID)
	mustState(t, got, err, StateCompleted)
	if got.Usage != r.Usage {
		t.Fatal("receipt changed usage")
	}
	a.Close(ctx)
	b := newTestAgent(t, newScript().Model(), WithLedger(l))
	if err := b.ObserveQuality(ctx, r.ID, d); err != nil {
		t.Fatal(err)
	}
	if n := len(ledgerEvents(t, l, r.ID)); n != before {
		t.Fatalf("retry appended: %d vs %d", n, before)
	}
	for _, bad := range []QualityObservation{
		{ID: d.ID, Completion: d.Completion, Verification: "accepted"},
		{ID: "second", Completion: d.Completion, Verification: "accepted"},
		{ID: "wrong", Completion: 1, Verification: "satisfied"},
		{ID: "grade", Completion: d.Completion, Verification: "grader_satisfied"},
		{ID: "no-effect"},
	} {
		if err := b.ObserveQuality(ctx, r.ID, bad); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
}

func TestQualityCostEpisodesAndLateAnnotations(t *testing.T) {
	ctx := testCtx(t)
	// The fake model tokens are intentionally unpriced. A later annotation
	// has an explicit zero price, which must not make the earlier tokens free.
	a := newTestAgent(t, newScript(endTurn("first"), endTurn("second")).Model(), WithPrices(PriceTable{"annotation": {}}))
	r, err := a.Run(ctx, "work")
	mustState(t, r, err, StateCompleted)
	first := r.Seq
	if err := a.Annotate(ctx, r.ID, RunAnnotatedData{Usage: &Usage{InputTokens: 7}, Model: "annotation"}); err != nil {
		t.Fatal(err)
	}
	if err := a.ObserveQuality(ctx, r.ID, QualityObservation{ID: "first", Completion: first, Verification: "accepted"}); err != nil {
		t.Fatal(err)
	}
	d := qualityReceipt(t, a, r.ID)
	if d.CostComplete || d.UnpricedTokens == 0 {
		t.Fatalf("unpriced model became free: %+v", d)
	}
	if err := a.Send(ctx, r.ID, UserText("another"), OriginHTTP); err != nil {
		t.Fatal(err)
	}
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)
	secondFinish := r.Seq
	// Price-complete annotation is part of the second episode. Its cost is
	// not the cumulative RunFinished totals and does not include child reports.
	if err := a.Annotate(ctx, r.ID, RunAnnotatedData{Usage: &Usage{InputTokens: 11}, Model: "annotation"}); err != nil {
		t.Fatal(err)
	}
	if err := a.ObserveQuality(ctx, r.ID, QualityObservation{ID: "second", Completion: secondFinish, Verification: "satisfied"}); err != nil {
		t.Fatal(err)
	}
	second := qualityReceipt(t, a, r.ID)
	if second.UnpricedTokens != d.UnpricedTokens {
		t.Fatalf("episodes double-counted tokens: first=%+v second=%+v", d, second)
	}
}

func TestQualityAuthorization(t *testing.T) {
	ctx := testCtx(t)
	cfg := DefaultConfig()
	cfg.Access.Default = "closed"
	cfg.Access.Admins = []string{"alice"}
	a := newTestAgent(t, newScript(endTurn("done")).Model(), WithConfig(cfg))
	r, err := a.Run(WithPrincipal(ctx, &Principal{Subject: "alice", Kind: KindUser}), "work")
	mustState(t, r, err, StateCompleted)
	err = a.ObserveQuality(WithPrincipal(ctx, &Principal{Subject: "bob", Kind: KindUser}), r.ID, QualityObservation{ID: "bad", DuplicateEffects: 1})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("unauthorized receipt: %v", err)
	}
}

func TestQualityCostUsesOwnEpisodeNotTreeOrCumulativeTotals(t *testing.T) {
	ctx := testCtx(t)
	l := NewMemLedger()
	id := "cost"
	for _, e := range []Event{
		MustEvent(id, EvRunQueued, RunQueuedData{Agent: "default"}),
		MustEvent(id, EvModelResponded, ModelRespondedData{Provider: "p", Model: "m", Usage: Usage{InputTokens: 10}}),
		MustEvent(id, EvChildReported, ChildReportedData{Child: "child", TreeUsage: RunUsage{CostUSD: 999}}),
		MustEvent(id, EvRunFinished, RunFinishedData{Outcome: OutcomeCompleted, Totals: &RunUsage{CostUSD: 1000}}),
		MustEvent(id, EvRunAnnotated, RunAnnotatedData{Provider: "p", Model: "m", Usage: &Usage{InputTokens: 3}}),
		MustEvent(id, EvMessageReceived, MessageReceivedData{Role: RoleUser, Origin: OriginHTTP}),
		MustEvent(id, EvModelResponded, ModelRespondedData{Provider: "p", Model: "m", Usage: Usage{InputTokens: 7}}),
		MustEvent(id, EvRunFinished, RunFinishedData{Outcome: OutcomeCompleted, Totals: &RunUsage{CostUSD: 2000}}),
		MustEvent(id, EvRunAnnotated, RunAnnotatedData{Model: "unknown", Usage: &Usage{CacheReadTokens: 2}}),
	} {
		if _, err := l.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	a := newTestAgent(t, newScript().Model(), WithLedger(l), WithoutResume(), WithPrices(PriceTable{"p/m": {Input: 1e6}}))
	if err := a.ObserveQuality(ctx, id, QualityObservation{ID: "first", Completion: 4, Verification: "accepted"}); err != nil {
		t.Fatal(err)
	}
	first := qualityReceipt(t, a, id)
	if !first.CostComplete || first.CostUSD != 13 || first.UnpricedTokens != 0 {
		t.Fatalf("first cost=%+v", first)
	}
	if err := a.ObserveQuality(ctx, id, QualityObservation{ID: "second", Completion: 8, Verification: "satisfied"}); err != nil {
		t.Fatal(err)
	}
	second := qualityReceipt(t, a, id)
	if second.CostComplete || second.CostUSD != 7 || second.UnpricedTokens != 2 {
		t.Fatalf("second cost=%+v", second)
	}
}

func TestQualityBranchCannotVerifyCopiedCompletionOrChargeCopiedTokens(t *testing.T) {
	ctx := testCtx(t)
	l := NewMemLedger()
	id := "branch"
	for _, e := range []Event{
		MustEvent(id, EvRunQueued, RunQueuedData{Agent: "default", Kept: 3}),
		MustEvent(id, EvModelResponded, ModelRespondedData{Provider: "p", Model: "m", Usage: Usage{InputTokens: 99}}),
		MustEvent(id, EvRunFinished, RunFinishedData{Outcome: OutcomeCompleted}),
		MustEvent(id, EvMessageReceived, MessageReceivedData{Role: RoleUser, Origin: OriginHTTP}),
		MustEvent(id, EvModelResponded, ModelRespondedData{Provider: "p", Model: "m", Usage: Usage{InputTokens: 4}}),
		MustEvent(id, EvRunFinished, RunFinishedData{Outcome: OutcomeCompleted}),
	} {
		if _, err := l.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	a := newTestAgent(t, newScript().Model(), WithLedger(l), WithoutResume(), WithPrices(PriceTable{"p/m": {Input: 1e6}}))
	if err := a.ObserveQuality(ctx, id, QualityObservation{ID: "copied", Completion: 3, Verification: "accepted"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatal(err)
	}
	if err := a.ObserveQuality(ctx, id, QualityObservation{ID: "own", Completion: 6, Verification: "satisfied"}); err != nil {
		t.Fatal(err)
	}
	if got := qualityReceipt(t, a, id); !got.CostComplete || got.CostUSD != 4 {
		t.Fatalf("copied tokens charged: %+v", got)
	}
}
