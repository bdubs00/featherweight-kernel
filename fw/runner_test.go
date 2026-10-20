package fw

import (
	"context"
	"sync/atomic"
	"testing"
)

// hostRunner drives a run the way a hosted harness does: one turn per
// message of the run, then the end of the run, and a wait for a person
// when the message asks for one.
type hostRunner struct {
	decline bool
	calls   atomic.Int32
}

func (h *hostRunner) Drive(ctx context.Context, hr *Hosted) (bool, error) {
	if h.decline {
		return false, nil
	}
	h.calls.Add(1)
	evs := hr.Events()
	r := ProjectRun(evs)
	if r.Pending != nil {
		return true, nil
	}
	// The messages that the runner answered end at the latest turn.
	var answered, last uint64
	var started, decided bool
	for _, e := range evs {
		switch e.Type {
		case EvRunStarted:
			started = true
		case EvTurnFinished:
			answered = e.Seq
		case EvRunQueued, EvMessageReceived:
			last = e.Seq
		case EvApprovalDecided:
			decided = true
		}
	}
	if !started {
		if _, err := hr.Emit(ctx, EvRunStarted, RunStartedData{Model: hr.Model()}); err != nil {
			return true, err
		}
	}
	if answered > last {
		return true, nil
	}
	if hr.Queued().Input == "ask" && !decided {
		_, err := hr.Emit(ctx, EvApprovalRequested, ApprovalRequestedData{ApprovalID: "ap1", Tool: "bash", CallID: "c1"})
		return true, err
	}
	if _, _, over := hr.Over(); over {
		_, err := hr.Emit(ctx, EvRunFinished, RunFinishedData{Outcome: OutcomeFailed, Error: "over"})
		return true, err
	}
	for _, step := range []struct {
		typ  string
		data any
	}{
		{EvTurnStarted, TurnData{Turn: 1}},
		{EvModelResponded, ModelRespondedData{Content: []Block{Text("hosted")}, StopReason: StopEndTurn, Usage: Usage{InputTokens: 7, OutputTokens: 3}}},
		{EvTurnFinished, TurnData{Turn: 1}},
	} {
		if _, err := hr.Emit(ctx, step.typ, step.data); err != nil {
			return true, err
		}
	}
	_, err := hr.Finish(ctx, RunFinishedData{Outcome: OutcomeCompleted, Output: "hosted"}, last)
	return true, err
}

func TestRunnerDrivesRuns(t *testing.T) {
	ctx := testCtx(t)
	hr := &hostRunner{}
	a := newTestAgent(t, newScript().Model(), WithRunner(hr))
	if a.Runner() != hr {
		t.Fatal("the agent has another runner")
	}
	r, err := a.Run(ctx, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if r.State != StateCompleted || r.Output != "hosted" || r.Usage.InputTokens != 7 || r.Usage.Turns != 1 {
		t.Fatalf("run = %+v", r)
	}
	// A follow-up reopens the run, and the runner drives it again.
	if err := a.Send(ctx, r.ID, Message{Role: RoleUser, Content: []Block{Text("again")}}, OriginCLI); err != nil {
		t.Fatal(err)
	}
	r, err = a.WaitFor(ctx, r.ID, func(r *Run) bool { return r.State.Terminal() })
	if err != nil {
		t.Fatal(err)
	}
	if r.State != StateCompleted || r.Usage.Turns != 2 {
		t.Fatalf("after the follow-up: %+v", r)
	}
}

func TestRunnerWaitsForADecision(t *testing.T) {
	ctx := testCtx(t)
	hr := &hostRunner{}
	a := newTestAgent(t, newScript().Model(), WithRunner(hr))
	r, err := a.Start(ctx, "ask")
	if err != nil {
		t.Fatal(err)
	}
	r, err = a.WaitFor(ctx, r.ID, func(r *Run) bool { return r.State == StateInputRequired })
	if err != nil || r.Pending == nil || r.Pending.ID != "ap1" {
		t.Fatalf("run = %+v, %v", r, err)
	}
	if err := a.Decide(ctx, r.ID, ApprovalDecision{ApprovalID: "ap1", Decision: "allow"}); err != nil {
		t.Fatal(err)
	}
	r, err = a.WaitFor(ctx, r.ID, func(r *Run) bool { return r.State.Terminal() })
	if err != nil || r.State != StateCompleted {
		t.Fatalf("run = %+v, %v", r, err)
	}
}

func TestRunnerThatDeclinesLeavesTheTurnLoop(t *testing.T) {
	ctx := testCtx(t)
	hr := &hostRunner{decline: true}
	a := newTestAgent(t, newScript(endTurn("native")).Model(), WithRunner(hr))
	r, err := a.Run(ctx, "hello")
	if err != nil || r.Output != "native" {
		t.Fatalf("run = %+v, %v", r, err)
	}
}

func TestHostedFinishWaitsForAMessage(t *testing.T) {
	ctx := testCtx(t)
	var got *Hosted
	a := newTestAgent(t, newScript().Model(), WithRunner(runnerFunc(func(ctx context.Context, h *Hosted) (bool, error) {
		got = h
		if !ProjectRun(h.Events()).State.Terminal() && len(h.Events()) == 1 {
			_, err := h.Emit(ctx, EvRunStarted, RunStartedData{})
			return true, err
		}
		return true, nil
	})))
	r, err := a.Start(ctx, "x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.WaitFor(ctx, r.ID, func(r *Run) bool { return r.Seq >= 2 }); err != nil {
		t.Fatal(err)
	}
	if err := a.Send(ctx, r.ID, Message{Content: []Block{Text("late")}}, OriginCLI); err != nil {
		t.Fatal(err)
	}
	if _, err := a.WaitFor(ctx, r.ID, func(r *Run) bool { return r.Seq >= 3 }); err != nil {
		t.Fatal(err)
	}
	if _, err := got.Emit(ctx, EvRunQueued, RunQueuedData{}); err == nil {
		t.Fatal("a runner recorded fw.run.queued")
	}
	if err := a.holdEvents(got.rs); err != nil {
		t.Fatal(err)
	}
	defer got.rs.releaseEvents()
	done, err := got.Finish(ctx, RunFinishedData{Outcome: OutcomeCompleted}, 2)
	if err != nil || done {
		t.Fatalf("finish after an unread message = %v, %v", done, err)
	}
	done, err = got.Finish(ctx, RunFinishedData{Outcome: OutcomeCompleted}, 3)
	if err != nil || !done {
		t.Fatalf("finish = %v, %v", done, err)
	}
}

type runnerFunc func(ctx context.Context, h *Hosted) (bool, error)

func (f runnerFunc) Drive(ctx context.Context, h *Hosted) (bool, error) { return f(ctx, h) }
