package fw

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"
)

// lifecycleTypes are the event types that a worker of a fleet sends on its
// egress stream. fw.model.responded and fw.tool.requested are not among
// them, because they carry the content of the run.
var lifecycleTypes = map[string]bool{
	EvRunQueued: true, EvRunStarted: true, EvRunResumed: true, EvRunSuspended: true, EvRunFinished: true,
	EvApprovalRequested: true, EvApprovalDecided: true, EvRunAnnotated: true, EvTurnStarted: true,
	EvMessageReceived: true, EvRunChild: true,
}

// lifecycleOnly keeps the events that a consumer of such a stream receives.
func lifecycleOnly(evs []Event) []Event {
	var out []Event
	for _, e := range evs {
		if lifecycleTypes[e.Type] {
			out = append(out, e)
		}
	}
	return out
}

// withoutTotals returns the events as an older kernel wrote them: the same
// events with no totals on fw.run.finished and fw.run.suspended.
func withoutTotals(t *testing.T, evs []Event) []Event {
	t.Helper()
	out := make([]Event, len(evs))
	for i, e := range evs {
		var data any
		switch e.Type {
		case EvRunFinished:
			var d RunFinishedData
			if err := e.Decode(&d); err != nil {
				t.Fatal(err)
			}
			d.Totals = nil
			data = d
		case EvRunSuspended:
			var d RunSuspendedData
			if err := e.Decode(&d); err != nil {
				t.Fatal(err)
			}
			d.Totals = nil
			data = d
		}
		if data != nil {
			b, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			e.Data = b
		}
		out[i] = e
	}
	return out
}

// TestProjectRunTakesTotals shows that the totals of fw.run.finished set the
// usage and do not add to it. A consumer that saw every increment and a
// consumer that saw only the lifecycle events end with the same usage, and
// the cost is the cost of the kernel, whatever the prices of the consumer.
func TestProjectRunTakesTotals(t *testing.T) {
	prices := PriceTable{"fake/m": {Input: 1, Output: 2}}
	call := Usage{InputTokens: 1_000_000, OutputTokens: 500_000, CacheReadTokens: 200_000, CacheWriteTokens: 100_000}
	totals := RunUsage{Usage: call, Turns: 1, ToolCalls: 1, CostUSD: 2.3}
	log := evs("R",
		MustEvent("R", EvRunQueued, RunQueuedData{Agent: "a", Input: "in"}),
		MustEvent("R", EvRunStarted, nil),
		MustEvent("R", EvTurnStarted, TurnData{Turn: 1}),
		MustEvent("R", EvModelResponded, ModelRespondedData{Provider: "fake", Model: "m", Usage: call}),
		MustEvent("R", EvToolRequested, ToolRequestedData{Tool: "t", CallID: "c1"}),
		MustEvent("R", EvRunFinished, RunFinishedData{Outcome: OutcomeCompleted, Totals: &totals}),
	)
	if got := ProjectRun(log[:5], prices).Usage; got != totals {
		t.Fatalf("the increments = %+v, want the totals %+v", got, totals)
	}
	r := ProjectRun(log, prices)
	if r.Usage != totals || r.UsageSeq != 6 || r.State != StateCompleted {
		t.Fatalf("every event: usage %+v at %d, want %+v at 6 (counted twice?)", r.Usage, r.UsageSeq, totals)
	}
	// The lifecycle events alone tell the turns until the run finishes,
	// and the totals then tell the rest, with no price table.
	if mid := ProjectRun(lifecycleOnly(log[:5])); mid.Usage != (RunUsage{Turns: 1}) || mid.UsageSeq != 0 {
		t.Fatalf("lifecycle before the end: usage %+v at %d", mid.Usage, mid.UsageSeq)
	}
	if life := ProjectRun(lifecycleOnly(log)); life.Usage != totals || life.UsageSeq != 6 {
		t.Fatalf("lifecycle: usage %+v at %d, want %+v at 6", life.Usage, life.UsageSeq, totals)
	}
	// Other prices of the consumer do not change the cost of the kernel.
	if other := ProjectRun(log, PriceTable{"fake/m": {Input: 100, Output: 100}}); other.Usage.CostUSD != 2.3 {
		t.Fatalf("cost with other prices = %v, want the cost of the kernel 2.3", other.Usage.CostUSD)
	}
	// The totals win even when they differ from the increments.
	odd := RunUsage{Usage: Usage{InputTokens: 7}, Turns: 9, CostUSD: 0.5, UnpricedTokens: 3}
	set := append(log[:5:5], MustEvent("R", EvRunFinished, RunFinishedData{Outcome: OutcomeFailed, Totals: &odd}))
	set[5].Subject, set[5].Seq = "R", 6
	if got := ProjectRun(set, prices).Usage; got != odd {
		t.Fatalf("usage = %+v, want the totals %+v", got, odd)
	}
}

// TestProjectRunTotalsAcrossFollowUp shows that the totals are cumulative: a
// run that finishes, takes a message, and finishes again carries the usage
// of both parts on its second fw.run.finished.
func TestProjectRunTotalsAcrossFollowUp(t *testing.T) {
	p := Price{Input: 1, Output: 1}
	prices := PriceTable{"fake/m": p}
	a := Usage{InputTokens: 100, OutputTokens: 10}
	b := Usage{InputTokens: 300, OutputTokens: 30, CacheReadTokens: 50}
	first := RunUsage{Usage: a, Turns: 1, CostUSD: Cost(a, p)}
	second := RunUsage{Usage: a.Add(b), Turns: 2, ToolCalls: 1, CostUSD: first.CostUSD + Cost(b, p)}
	log := evs("R",
		MustEvent("R", EvRunQueued, RunQueuedData{Agent: "a", Input: "in"}),
		MustEvent("R", EvRunStarted, nil),
		MustEvent("R", EvTurnStarted, TurnData{Turn: 1}),
		MustEvent("R", EvModelResponded, ModelRespondedData{Provider: "fake", Model: "m", Usage: a}),
		MustEvent("R", EvRunFinished, RunFinishedData{Outcome: OutcomeCompleted, Output: "one", Totals: &first}),
		MustEvent("R", EvMessageReceived, MessageReceivedData{Role: RoleUser, Content: []Block{Text("again")}}),
		MustEvent("R", EvTurnStarted, TurnData{Turn: 2}),
		MustEvent("R", EvModelResponded, ModelRespondedData{Provider: "fake", Model: "m", Usage: b}),
		MustEvent("R", EvToolRequested, ToolRequestedData{Tool: "t", CallID: "c1"}),
		MustEvent("R", EvRunFinished, RunFinishedData{Outcome: OutcomeCompleted, Output: "two", Totals: &second}),
	)
	for name, l := range map[string][]Event{"every event": log, "lifecycle events": lifecycleOnly(log)} {
		if r := ProjectRun(l, prices); r.Usage != second || r.UsageSeq != 10 || r.Output != "two" {
			t.Fatalf("%s: usage %+v at %d, want %+v at 10", name, r.Usage, r.UsageSeq, second)
		}
	}
	// Between the two ends, a lifecycle consumer knows the first part and
	// the new turn. UsageSeq tells that the usage dates from the first end.
	mid := ProjectRun(lifecycleOnly(log[:8]))
	want := first
	want.Turns = 2
	if mid.Usage != want || mid.UsageSeq != 5 || mid.Seq != 7 || mid.State != StateRunning {
		t.Fatalf("between the ends: usage %+v at %d, seq %d, state %s", mid.Usage, mid.UsageSeq, mid.Seq, mid.State)
	}
	// A consumer that lost the first end catches up at the second.
	lost := lifecycleOnly(append(log[:4:4], log[5:]...))
	if r := ProjectRun(lost); r.Usage != second {
		t.Fatalf("after a lost event: usage %+v, want %+v", r.Usage, second)
	}
	// The projection of every event is the same with and without totals.
	if old := ProjectRun(withoutTotals(t, log), prices); old.Usage != second || old.UsageSeq != 0 {
		t.Fatalf("without totals: usage %+v at %d", old.Usage, old.UsageSeq)
	}
}

// TestProjectRunWithoutTotals shows that the events of an older kernel,
// without totals, keep the usage that the increments give, and that a
// call without a price counts as unpriced, not as free.
func TestProjectRunWithoutTotals(t *testing.T) {
	p := Price{Input: 1, Output: 1}
	prices := PriceTable{"fake/m": p}
	priced := Usage{InputTokens: 100, OutputTokens: 10}
	unpriced := Usage{InputTokens: 40, OutputTokens: 2, CacheReadTokens: 8}
	old := MustEvent("R", EvRunFinished, nil)
	old.Data = json.RawMessage(`{"outcome":"completed","output":"done"}`)
	log := evs("R",
		MustEvent("R", EvRunQueued, RunQueuedData{Agent: "a", Input: "in"}),
		MustEvent("R", EvTurnStarted, TurnData{Turn: 1}),
		MustEvent("R", EvModelResponded, ModelRespondedData{Provider: "fake", Model: "m", Usage: priced}),
		MustEvent("R", EvToolRequested, ToolRequestedData{Tool: "t", CallID: "c1"}),
		MustEvent("R", EvRunSuspended, RunSuspendedData{Reason: "shutdown"}),
		MustEvent("R", EvRunResumed, RunResumedData{FromSeq: 5}),
		MustEvent("R", EvRunAnnotated, RunAnnotatedData{Metadata: map[string]string{MetaTitle: "T"}, Usage: &unpriced, Provider: "other", Model: "titles"}),
		old,
	)
	want := RunUsage{Usage: priced.Add(unpriced), Turns: 1, ToolCalls: 1, CostUSD: Cost(priced, p), UnpricedTokens: 50}
	r := ProjectRun(log, prices)
	if r.Usage != want || r.UsageSeq != 0 || r.State != StateCompleted || r.Output != "done" {
		t.Fatalf("run = usage %+v at %d, state %s; want usage %+v", r.Usage, r.UsageSeq, r.State, want)
	}
	// Without a price table, every token is unpriced.
	if r := ProjectRun(log); r.Usage.CostUSD != 0 || r.Usage.UnpricedTokens != 160 {
		t.Fatalf("no prices: usage %+v", r.Usage)
	}
	// A lifecycle consumer of an older kernel sees what it saw before: the
	// turns and the usage of the annotation.
	life := ProjectRun(lifecycleOnly(log), prices)
	if life.Usage != (RunUsage{Usage: unpriced, Turns: 1, UnpricedTokens: 50}) || life.UsageSeq != 0 {
		t.Fatalf("lifecycle: usage %+v at %d", life.Usage, life.UsageSeq)
	}
}

// TestTerminalEventsCarryTotals runs a real run with a fake model, a tool
// call, cache tokens, an annotation of a model without a price, and a
// follow-up. Each fw.run.finished carries the projection of the events
// before it, and the last one is the usage of the whole ledger.
func TestTerminalEventsCarryTotals(t *testing.T) {
	ctx := testCtx(t)
	prices := PriceTable{"anthropic/claude-opus-5-5": {Input: 3, Output: 15}}
	calls := callTools(use("c1", "echo", `{"text":"a"}`))
	calls.Usage = Usage{InputTokens: 1200, OutputTokens: 80, CacheReadTokens: 900, CacheWriteTokens: 300}
	first, second := endTurn("first"), endTurn("second")
	first.Usage.CacheReadTokens = 1500
	a := newTestAgent(t, newScript(calls, first, second).Model(), WithTools(echoTool(nil)), WithPrices(prices))
	r, err := a.Run(ctx, "work")
	mustState(t, r, err, StateCompleted)
	title := Usage{InputTokens: 60, OutputTokens: 6}
	if err := a.Annotate(ctx, r.ID, RunAnnotatedData{Metadata: map[string]string{MetaTitle: "Work"}, Usage: &title, Provider: "other", Model: "titles"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Send(ctx, r.ID, UserText("again"), OriginCLI); err != nil {
		t.Fatal(err)
	}
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)

	evs := ledgerEvents(t, a.Ledger(), r.ID)
	var totals []RunUsage
	for i, e := range evs {
		if e.Type != EvRunFinished {
			continue
		}
		var d RunFinishedData
		if err := e.Decode(&d); err != nil || d.Totals == nil {
			t.Fatalf("fw.run.finished at %d has no totals: %s", e.Seq, e.Data)
		}
		if want := ProjectRun(evs[:i], prices).Usage; *d.Totals != want {
			t.Fatalf("totals at %d = %+v, the events before give %+v", e.Seq, *d.Totals, want)
		}
		totals = append(totals, *d.Totals)
	}
	if len(totals) != 2 {
		t.Fatalf("finished events = %d", len(totals))
	}
	last := evs[len(evs)-1]
	full := ProjectRun(evs, prices)
	if last.Type != EvRunFinished || full.Usage != totals[1] || full.UsageSeq != last.Seq {
		t.Fatalf("the last totals %+v, the ledger %+v at %d", totals[1], full.Usage, full.UsageSeq)
	}
	// The status of the kernel, the ledger without totals, and the
	// lifecycle events alone give the same usage.
	got, err := a.Get(ctx, r.ID)
	if err != nil || got.Usage != full.Usage || got.UsageSeq != full.UsageSeq {
		t.Fatalf("Get = %+v, %v; want %+v", got, err, full.Usage)
	}
	if old := ProjectRun(withoutTotals(t, evs), prices); old.Usage != full.Usage {
		t.Fatalf("without totals = %+v, with = %+v", old.Usage, full.Usage)
	}
	if life := ProjectRun(lifecycleOnly(evs)); life.Usage != full.Usage {
		t.Fatalf("lifecycle = %+v, every event = %+v", life.Usage, full.Usage)
	}
	// The second totals add the follow-up and the annotation to the first.
	tokens := calls.Usage.Add(first.Usage).Add(second.Usage).Add(title)
	u := totals[1]
	if u.Usage != tokens || u.Turns != countType(evs, EvTurnStarted) || u.Turns != 3 || u.ToolCalls != 1 ||
		u.UnpricedTokens != 66 || totals[0].UnpricedTokens != 0 || !(u.CostUSD > totals[0].CostUSD) || totals[0].CostUSD <= 0 {
		t.Fatalf("totals = %+v then %+v; want tokens %+v", totals[0], u, tokens)
	}
}

// TestSuspendCarriesTotals stops a process while a run waits for the model.
// fw.run.suspended carries the usage so far, and the run finishes in a new
// process with totals that hold the work of both processes.
func TestSuspendCarriesTotals(t *testing.T) {
	ctx := testCtx(t)
	prices := PriceTable{"anthropic/claude-opus-5-5": {Input: 3, Output: 15}}
	l := NewMemLedger()
	started := make(chan struct{}, 1)
	var n atomic.Int32
	m := ModelFunc{StreamFn: func(ctx context.Context, _ *Request, _ func(Delta)) (*Response, error) {
		if n.Add(1) == 1 {
			return callTools(use("c1", "echo", `{"text":"a"}`)), nil
		}
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	a := newTestAgent(t, m, WithLedger(l), WithTools(echoTool(nil)), WithPrices(prices))
	r, err := a.Start(ctx, "long job")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	sctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	a.Close(sctx)
	evs := ledgerEvents(t, l, r.ID)
	last := evs[len(evs)-1]
	var sd RunSuspendedData
	if last.Type != EvRunSuspended || last.Decode(&sd) != nil || sd.Totals == nil {
		t.Fatalf("last event = %s %s", last.Type, last.Data)
	}
	if want := ProjectRun(evs[:len(evs)-1], prices).Usage; *sd.Totals != want || want.InputTokens != 10 || want.ToolCalls != 1 || want.CostUSD <= 0 {
		t.Fatalf("suspended totals = %+v, the events before give %+v", *sd.Totals, want)
	}

	b := newTestAgent(t, newScript(endTurn("finished later")).Model(), WithLedger(l), WithTools(echoTool(nil)), WithPrices(prices))
	got, err := b.Wait(ctx, r.ID)
	mustState(t, got, err, StateCompleted)
	evs = ledgerEvents(t, l, r.ID)
	last = evs[len(evs)-1]
	var fd RunFinishedData
	if last.Type != EvRunFinished || last.Decode(&fd) != nil || fd.Totals == nil {
		t.Fatalf("last event = %s %s", last.Type, last.Data)
	}
	full := ProjectRun(evs, prices)
	if *fd.Totals != full.Usage || got.Usage != full.Usage || fd.Totals.InputTokens != 20 || fd.Totals.ToolCalls != 1 {
		t.Fatalf("finished totals = %+v, the ledger %+v, the status %+v", *fd.Totals, full.Usage, got.Usage)
	}
	if old := ProjectRun(withoutTotals(t, evs), prices); old.Usage != full.Usage {
		t.Fatalf("without totals = %+v, with = %+v", old.Usage, full.Usage)
	}
}
