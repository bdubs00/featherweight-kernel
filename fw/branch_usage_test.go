package fw

import (
	"context"
	"testing"
)

// TestBranchCountsOwnUsage forks a finished run of a batch with a message.
// The ledger of the branch holds copies of the model call and of the
// fw.run.finished of its source, with its totals. The branch counts its own
// call only, in the status of the kernel, in its own totals, and for a
// consumer of the lifecycle events, so a sum over the batch counts each
// model call once.
func TestBranchCountsOwnUsage(t *testing.T) {
	ctx := testCtx(t)
	first, second := endTurn("first"), endTurn("second")
	second.Usage = Usage{InputTokens: 70, OutputTokens: 7}
	a := newTestAgent(t, newScript(first, second).Model())
	src, err := a.Run(ctx, "hello", WithRequest(RunRequest{Metadata: map[string]string{MetaBatch: "job"}}))
	mustState(t, src, err, StateCompleted)
	b, err := a.Fork(ctx, src.ID, ForkRequest{Input: "and then?"})
	if err != nil {
		t.Fatal(err)
	}
	b, err = a.WaitFor(ctx, b.ID, func(r *Run) bool { return r.State.Terminal() })
	mustState(t, b, err, StateCompleted)
	if b.Metadata[MetaBatch] != "job" {
		t.Fatalf("branch metadata = %v", b.Metadata)
	}
	own := RunUsage{Usage: second.Usage, Turns: 1, UnpricedTokens: 77}
	if b.Usage != own || src.Usage.Usage.Add(b.Usage.Usage) != first.Usage.Add(second.Usage) {
		t.Fatalf("branch usage = %+v, want its own call %+v; the source has %+v", b.Usage, own, src.Usage)
	}
	evs := ledgerEvents(t, a.Ledger(), b.ID)
	var q RunQueuedData
	firstOf(t, evs, EvRunQueued, &q)
	srcEvs := ledgerEvents(t, a.Ledger(), src.ID)
	if q.Kept != uint64(len(srcEvs)) || countType(evs, EvRunFinished) != 2 {
		t.Fatalf("kept = %d of %d source events; branch events %v", q.Kept, len(srcEvs), eventTypes(evs))
	}
	var d RunFinishedData
	if last := evs[len(evs)-1]; last.Decode(&d) != nil || d.Totals == nil || *d.Totals != own {
		t.Fatalf("the totals of the branch = %+v, want %+v", d.Totals, own)
	}
	for name, l := range map[string][]Event{"every event": evs, "lifecycle events": lifecycleOnly(evs), "without totals": withoutTotals(t, evs)} {
		if r := ProjectRun(l); r.Usage != own {
			t.Fatalf("%s: usage %+v, want %+v", name, r.Usage, own)
		}
	}
}

// TestBranchKeptSkipsChildren shows that Kept counts the events of the
// ledger of the branch, which leaves out the fw.run.child events of the
// source, so the boundary between the copies and the work of the branch is
// exact. fw.forkSeq counts in the source, so the first calls of the branch
// would fall under it.
func TestBranchKeptSkipsChildren(t *testing.T) {
	a := newTestAgent(t, newScript().Model())
	call := Usage{InputTokens: 10, OutputTokens: 5}
	src := evs("S",
		MustEvent("S", EvRunQueued, RunQueuedData{Agent: "default", Input: "go"}),
		MustEvent("S", EvRunStarted, RunStartedData{}),
		MustEvent("S", EvTurnStarted, TurnData{Turn: 1}),
		MustEvent("S", EvRunChild, RunChildData{Child: "c1", Member: "m"}),
		MustEvent("S", EvRunChild, RunChildData{Child: "c2", Member: "m"}),
		MustEvent("S", EvRunChild, RunChildData{Child: "c3", Member: "m"}),
		MustEvent("S", EvRunChild, RunChildData{Child: "c4", Member: "m"}),
		MustEvent("S", EvModelResponded, ModelRespondedData{Content: []Block{Text("one")}, StopReason: StopEndTurn, Usage: call}),
		MustEvent("S", EvRunFinished, RunFinishedData{Outcome: OutcomeCompleted, Totals: &RunUsage{Usage: call, Turns: 1}}),
	)
	req := ForkRequest{}
	kept, q, err := a.branchOf(context.Background(), "S", RunQueuedData{Agent: "default", Input: "go"}, src, &req)
	if err != nil {
		t.Fatal(err)
	}
	var copied RunQueuedData
	if err := kept[0].Decode(&copied); err != nil || q.Kept != 5 || uint64(len(kept)) != q.Kept || copied.Kept != 5 || req.Seq != 9 {
		t.Fatalf("kept %d events, Kept = %d, copied Kept = %d, seq %d", len(kept), q.Kept, copied.Kept, req.Seq)
	}
	// The ledger of the branch: the copies, then the work of the branch,
	// whose first model call has a sequence number below fw.forkSeq.
	mine := Usage{InputTokens: 70, OutputTokens: 7}
	branch := append(kept,
		MustEvent("B", EvMessageReceived, MessageReceivedData{Role: RoleUser, Content: []Block{Text("more")}}),
		MustEvent("B", EvTurnStarted, TurnData{Turn: 2}),
		MustEvent("B", EvModelResponded, ModelRespondedData{Content: []Block{Text("two")}, StopReason: StopEndTurn, Usage: mine}),
		MustEvent("B", EvRunFinished, RunFinishedData{Outcome: OutcomeCompleted}),
	)
	branch = evs("B", branch...)
	if branch[7].Type != EvModelResponded || branch[7].Seq > req.Seq {
		t.Fatalf("the call of the branch is at %d, fw.forkSeq is %d", branch[7].Seq, req.Seq)
	}
	if r := ProjectRun(branch); r.Usage != (RunUsage{Usage: mine, Turns: 1, UnpricedTokens: 77}) {
		t.Fatalf("branch usage = %+v, want its own call %+v", r.Usage, mine)
	}
}

// TestBranchBudgetCountsOwnWork gives a run a budget that its own work
// uses up exactly. A branch with the same budget and the same work
// finishes too, because the turns, the tool calls, and the tokens that it
// kept from its source are not charged to it a second time.
func TestBranchBudgetCountsOwnWork(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget Budget
	}{
		{"maxTurns", Budget{MaxTurns: 2}},
		{"maxTokens", Budget{MaxTokens: 30}},
		{"maxToolCalls", Budget{MaxToolCalls: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testCtx(t)
			s := newScript(callTools(use("c1", "echo", `{"text":"a"}`)), endTurn("one"), callTools(use("c2", "echo", `{"text":"b"}`)), endTurn("two"))
			a := newTestAgent(t, s.Model(), WithTools(echoTool(nil)))
			src, err := a.Run(ctx, "work", WithRunBudget(tc.budget))
			mustState(t, src, err, StateCompleted)
			b, err := a.Fork(ctx, src.ID, ForkRequest{Input: "again"})
			if err != nil {
				t.Fatal(err)
			}
			b, err = a.WaitFor(ctx, b.ID, func(r *Run) bool { return r.State.Terminal() })
			mustState(t, b, err, StateCompleted)
			if b.Output != "two" || b.Usage.Turns != 2 || b.Usage.ToolCalls != 1 || b.Usage.InputTokens != 20 {
				t.Fatalf("branch = %+v", b)
			}
		})
	}
}

// TestForkMetadataCountsUsage shows that the metadata of a fork in a
// request, or in a replay of a branch, which clones the metadata, does not
// make a run skip its usage: only the kernel sets Kept, when it forks.
func TestForkMetadataCountsUsage(t *testing.T) {
	ctx := testCtx(t)
	a := newTestAgent(t, newScript(endTurn("one"), endTurn("two"), endTurn("three")).Model())
	r, err := a.Run(ctx, "free?", WithMetadata(map[string]string{MetaForkOf: "01SRC", MetaForkSeq: "1000"}))
	mustState(t, r, err, StateCompleted)
	if r.Usage.InputTokens != 10 || r.Usage.Turns != 1 {
		t.Fatalf("run with fork metadata = %+v", r.Usage)
	}
	b, err := a.Fork(ctx, r.ID, ForkRequest{Input: "and?"})
	if err != nil {
		t.Fatal(err)
	}
	b, err = a.WaitFor(ctx, b.ID, func(r *Run) bool { return r.State.Terminal() })
	mustState(t, b, err, StateCompleted)
	rp, err := Replay(ctx, a, a.Ledger(), b.ID)
	mustState(t, rp, err, StateCompleted)
	if rp.Metadata[MetaForkOf] != r.ID || rp.Usage.InputTokens != 10 || rp.Usage.Turns != 1 {
		t.Fatalf("replay of the branch = %+v %+v", rp.Metadata, rp.Usage)
	}
}

// TestForkEventsCountsTheCopies sends a branch to ForkEvents, as another
// process does, with a forged Kept and with none, as an older kernel sends
// it. The process counts the copies itself: the branch counts its own model
// calls, and maxTokens stops it.
func TestForkEventsCountsTheCopies(t *testing.T) {
	for _, forged := range []uint64{1 << 40, 0} {
		ctx := testCtx(t)
		s := newScript(callTools(use("c1", "echo", `{"text":"a"}`)), callTools(use("c2", "echo", `{"text":"b"}`)),
			callTools(use("c3", "echo", `{"text":"c"}`)), endTurn("done"))
		a := newTestAgent(t, s.Model(), WithTools(echoTool(nil)))
		q := MustEvent("X", EvRunQueued, RunQueuedData{Agent: "default", Input: "go", Principal: "p", Kept: forged, Budget: &Budget{MaxTokens: 20}})
		q.Seq = 1
		r, err := a.ForkEvents(ctx, []Event{q}, ForkRequest{ID: NewID(), Input: "work"})
		if err != nil {
			t.Fatal(err)
		}
		r, err = a.WaitFor(ctx, r.ID, func(r *Run) bool { return r.State.Terminal() })
		mustState(t, r, err, StateFailed)
		evs := ledgerEvents(t, a.Ledger(), r.ID)
		var kept RunQueuedData
		firstOf(t, evs, EvRunQueued, &kept)
		var d RunFinishedData
		_ = evs[len(evs)-1].Decode(&d)
		if kept.Kept != 1 || countType(evs, EvModelResponded) != 2 || countType(evs, EvBudgetExceeded) != 1 ||
			r.Usage.InputTokens != 20 || d.Totals == nil || d.Totals.InputTokens != 20 {
			t.Fatalf("forged %d: kept %d, %d calls, usage %+v, totals %+v, error %q", forged, kept.Kept, countType(evs, EvModelResponded), r.Usage, d.Totals, r.Error)
		}
	}
}

// TestBranchWithoutWorkIsFinal forks a finished run without a message. The
// branch stays finished and appends no event of its own, so its last kept
// event gives it a known usage of zero, for the kernel and for a consumer
// of the lifecycle events.
func TestBranchWithoutWorkIsFinal(t *testing.T) {
	ctx := testCtx(t)
	a := newTestAgent(t, newScript(endTurn("first")).Model())
	src, err := a.Run(ctx, "hello")
	mustState(t, src, err, StateCompleted)
	b, err := a.Fork(ctx, src.ID, ForkRequest{})
	if err != nil {
		t.Fatal(err)
	}
	evs := ledgerEvents(t, a.Ledger(), b.ID)
	var q RunQueuedData
	firstOf(t, evs, EvRunQueued, &q)
	got, err := a.Get(ctx, b.ID)
	if err != nil || got.State != StateCompleted || got.UsageSeq != q.Kept || got.Usage != (RunUsage{}) || q.Kept != uint64(len(evs)) {
		t.Fatalf("branch = %+v, %v; kept %d of %d events", got, err, q.Kept, len(evs))
	}
	if life := ProjectRun(lifecycleOnly(evs)); life.State != StateCompleted || life.UsageSeq != q.Kept || life.Usage != (RunUsage{}) {
		t.Fatalf("lifecycle: state %s, usage %+v at %d", life.State, life.Usage, life.UsageSeq)
	}
}
