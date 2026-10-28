package fw

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// lastWait returns the wait of the last fw.approval.requested of a run.
func lastWait(t *testing.T, evs []Event) string {
	t.Helper()
	var d ApprovalRequestedData
	for _, e := range evs {
		if e.Type == EvApprovalRequested {
			d = ApprovalRequestedData{}
			if err := e.Decode(&d); err != nil {
				t.Fatal(err)
			}
		}
	}
	return d.Wait
}

// waitFor waits until the run waits for a person.
func waitFor(t *testing.T, a *Agent, id string) *Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if r, err := a.Get(context.Background(), id); err == nil && r.Pending != nil {
			return r
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("run %s never waited for a person", id)
	return nil
}

// Every request for a person carries what the run waits for, on the event
// and on the pending approval of the run object.
func TestWaitOnEachRequest(t *testing.T) {
	ctx := testCtx(t)
	t.Run("approval", func(t *testing.T) {
		send := NewTool(ToolSpec{Name: "send"}, func(context.Context, json.RawMessage) (ToolResult, error) { return TextResult("sent"), nil })
		a := newTestAgent(t, newScript(callTools(use("c1", "send", `{}`)), endTurn("done")).Model(), WithTools(send),
			WithProfile(AgentProfile{Name: "mailer", Policy: &Rules{Ask: []string{"send"}}}))
		r, err := a.Run(ctx, "Send it.", AsAgent("mailer"))
		mustState(t, r, err, StateInputRequired)
		if r.Pending.Wait != WaitApproval || lastWait(t, ledgerEvents(t, a.Ledger(), r.ID)) != WaitApproval {
			t.Fatalf("pending = %+v", r.Pending)
		}
	})
	t.Run("question", func(t *testing.T) {
		ask := NewTool(ToolSpec{Name: "ask"}, func(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
			b, err := AskCaller(ctx, "Which branch?", nil)
			if err != nil {
				return ErrorResult(err.Error()), nil
			}
			return TextResult(string(b)), nil
		})
		a := newTestAgent(t, newScript(callTools(use("c1", "ask", `{}`)), endTurn("done")).Model(), WithTools(ask))
		r, err := a.Start(ctx, "Ask me.")
		if err != nil {
			t.Fatal(err)
		}
		p := waitFor(t, a, r.ID).Pending
		if p.Wait != WaitQuestion || lastWait(t, ledgerEvents(t, a.Ledger(), r.ID)) != WaitQuestion {
			t.Fatalf("pending = %+v", p)
		}
	})
	t.Run("connect", func(t *testing.T) {
		gh := NewTool(ToolSpec{Name: "github"}, func(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
			if err := RequestConnect(ctx, ConnectRequest{Server: "github", Reason: ConnectMissing}); err != nil {
				return ErrorResult(err.Error()), nil
			}
			return TextResult("ok"), nil
		})
		a := newTestAgent(t, newScript(callTools(use("c1", "github", `{}`)), endTurn("done")).Model(), WithTools(gh))
		r, err := a.Start(ctx, "Open the issue.")
		if err != nil {
			t.Fatal(err)
		}
		p := waitFor(t, a, r.ID).Pending
		if p.Wait != WaitConnect || p.Connect == nil || lastWait(t, ledgerEvents(t, a.Ledger(), r.ID)) != WaitConnect {
			t.Fatalf("pending = %+v", p)
		}
	})
	t.Run("rerun", func(t *testing.T) {
		l := NewMemLedger()
		seedToolRequested(t, l, "r1", "send")
		send := NewTool(ToolSpec{Name: "send"}, func(context.Context, json.RawMessage) (ToolResult, error) { return TextResult("sent"), nil })
		a := newTestAgent(t, newScript(endTurn("done")).Model(), WithLedger(l), WithTools(send))
		r, err := a.Wait(ctx, "r1")
		mustState(t, r, err, StateInputRequired)
		if r.Pending.Wait != WaitRerun || lastWait(t, ledgerEvents(t, l, "r1")) != WaitRerun {
			t.Fatalf("pending = %+v", r.Pending)
		}
	})
}

// finishedStop returns the stop of the last fw.run.finished of a run.
func finishedStop(t *testing.T, a *Agent, id string) string {
	t.Helper()
	return finishedOf(t, ledgerEvents(t, a.Ledger(), id)).Stop
}

// Every end of a run carries why it stopped, on the event and on the run
// object.
func TestStopOnEachEnd(t *testing.T) {
	ctx := testCtx(t)
	loop := NewTool(ToolSpec{Name: "look", ReadOnly: true, Idempotent: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		return TextResult("same"), nil
	})
	again := callTools(use("c1", "look", `{"at": "x"}`))
	failing := ModelFunc{StreamFn: func(context.Context, *Request, func(Delta)) (*Response, error) {
		return nil, errors.New("503 overloaded")
	}}
	g := &gate{}
	for _, c := range []struct {
		name  string
		model Model
		opts  []RunOption
		prof  *AgentProfile
		state RunState
		stop  string
	}{
		{name: "answered", model: newScript(endTurn("hi")).Model(), state: StateCompleted, stop: RunStopAnswered},
		{name: "checked", model: newScript(endTurn("fixed")).Model(), state: StateCompleted, stop: RunStopChecked,
			prof: &AgentProfile{Name: "fixer", Done: &Done{Checks: []DoneCheck{{Tool: "probe"}}}}},
		{name: "not done", model: newScript(endTurn("fixed")).Model(), state: StateFailed, stop: RunStopNotDone,
			prof: &AgentProfile{Name: "fixer", Done: &Done{Checks: []DoneCheck{{Tool: "tests"}}, Attempts: 1}}},
		{name: "budget", model: newScript(again, callTools(use("c2", "look", `{"at": "y"}`)), endTurn("late")).Model(),
			opts: []RunOption{WithRunBudget(Budget{MaxTurns: 1})}, state: StateFailed, stop: "budget:maxTurns"},
		{name: "loop", model: newScript(again, again, again, again).Model(),
			opts: []RunOption{WithRunBudget(Budget{MaxRepeats: 2})}, state: StateFailed, stop: RunStopLoop},
		{name: "refusal", model: newScript(&Response{StopReason: StopRefusal}).Model(), state: StateFailed, stop: RunStopRefusal},
		{name: "max tokens", model: newScript(&Response{Content: []Block{Text("a")}, StopReason: StopMaxTokens},
			&Response{Content: []Block{Text("b")}, StopReason: StopMaxTokens}).Model(), state: StateFailed, stop: RunStopMaxTokens},
		{name: "model error", model: failing, state: StateFailed, stop: RunStopModelError},
	} {
		t.Run(c.name, func(t *testing.T) {
			probe := NewTool(ToolSpec{Name: "probe"}, func(context.Context, json.RawMessage) (ToolResult, error) { return TextResult("ok"), nil })
			opts := []Option{WithTools(append(g.tools(), loop, probe)...)}
			ropts := c.opts
			if c.prof != nil {
				opts = append(opts, WithProfile(*c.prof))
				ropts = append(ropts, AsAgent(c.prof.Name))
			}
			a := newTestAgent(t, c.model, opts...)
			r, err := a.Run(ctx, "Do the work.", ropts...)
			mustState(t, r, err, c.state)
			if got := finishedStop(t, a, r.ID); got != c.stop {
				t.Fatalf("stop on the event = %q, want %q (error %q)", got, c.stop, r.Error)
			}
			if r.Stop != c.stop {
				t.Fatalf("stop on the run = %q, want %q", r.Stop, c.stop)
			}
		})
	}
	t.Run("canceled", func(t *testing.T) {
		send := NewTool(ToolSpec{Name: "send"}, func(context.Context, json.RawMessage) (ToolResult, error) { return TextResult("sent"), nil })
		a := newTestAgent(t, newScript(callTools(use("c1", "send", `{}`))).Model(), WithTools(send),
			WithProfile(AgentProfile{Name: "mailer", Policy: &Rules{Ask: []string{"send"}}}))
		r, err := a.Run(ctx, "Send it.", AsAgent("mailer"))
		mustState(t, r, err, StateInputRequired)
		if r.Stop != "" {
			t.Fatalf("a run that waits has the stop %q", r.Stop)
		}
		if err := a.Cancel(ctx, r.ID); err != nil {
			t.Fatal(err)
		}
		r, err = a.Wait(ctx, r.ID)
		mustState(t, r, err, StateCanceled)
		if r.Stop != RunStopCanceled || finishedStop(t, a, r.ID) != RunStopCanceled {
			t.Fatalf("stop = %q", r.Stop)
		}
	})
	t.Run("check run", func(t *testing.T) {
		q := &fakeQueue{}
		a := newTestAgent(t, newScript().Model(), WithTools(q.tool("queue.read", TextResult("{}"), nil)))
		r, err := a.Run(ctx, "", WithCheck(Check{Steps: []CheckStep{step("queue.read", "")}}))
		mustState(t, r, err, StateCompleted)
		if r.Stop != RunStopAnswered {
			t.Fatalf("stop of a check run = %q", r.Stop)
		}
		r, err = a.Run(ctx, "", WithCheck(Check{Steps: []CheckStep{step("missing", "")}}))
		mustState(t, r, err, StateFailed)
		if r.Stop != RunStopError {
			t.Fatalf("stop of a failed check run = %q", r.Stop)
		}
	})
}

// A shutdown suspends a working run with the stop interrupted, and the
// stop goes when the run works again.
func TestStopOnSuspend(t *testing.T) {
	ctx := testCtx(t)
	l := NewMemLedger()
	started := make(chan struct{}, 1)
	a := newTestAgent(t, blockingModel(started), WithLedger(l))
	r, err := a.Start(ctx, "long job")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	sctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	a.Close(sctx)
	evs := ledgerEvents(t, l, r.ID)
	var sd RunSuspendedData
	if last := evs[len(evs)-1]; last.Type != EvRunSuspended || last.Decode(&sd) != nil || sd.Stop != RunStopInterrupted {
		t.Fatalf("suspend = %+v (events %v)", sd, eventTypes(evs))
	}
	if got := ProjectRun(evs); got.State != StateSuspended || got.Stop != RunStopInterrupted {
		t.Fatalf("projection = %s, stop %q", got.State, got.Stop)
	}
	b := newTestAgent(t, newScript(endTurn("finished later")).Model(), WithLedger(l))
	got, err := b.Wait(ctx, r.ID)
	mustState(t, got, err, StateCompleted)
	if got.Stop != RunStopAnswered {
		t.Fatalf("stop after the resume = %q", got.Stop)
	}
}

// An event of an older kernel has no wait and no stop. The projection and
// WaitOf and StopOf derive them, so old and new ledgers read the same way.
func TestReasonsOfAnOlderKernel(t *testing.T) {
	for _, c := range []struct {
		d    ApprovalRequestedData
		want string
	}{
		{ApprovalRequestedData{Tool: "send"}, WaitApproval},
		{ApprovalRequestedData{Tool: "ask", Schema: json.RawMessage(`{}`)}, WaitQuestion},
		{ApprovalRequestedData{Tool: "gh", Connect: &ConnectRequest{Server: "github"}, Schema: json.RawMessage(`{}`)}, WaitConnect},
		{ApprovalRequestedData{Tool: "send", Reason: "rerun after resume"}, WaitRerun},
		{ApprovalRequestedData{Tool: "send", Wait: WaitQuestion}, WaitQuestion},
	} {
		if got := WaitOf(c.d); got != c.want {
			t.Errorf("WaitOf(%+v) = %q, want %q", c.d, got, c.want)
		}
	}
	for _, c := range []struct {
		d    RunFinishedData
		want string
	}{
		{RunFinishedData{Outcome: OutcomeCompleted, Output: "hi"}, RunStopAnswered},
		{RunFinishedData{Outcome: OutcomeCompleted, Done: DoneChecked}, RunStopChecked},
		{RunFinishedData{Outcome: OutcomeFailed, Done: DoneFailed, Error: "not done: check 1 (tests) failed"}, RunStopNotDone},
		{RunFinishedData{Outcome: OutcomeFailed, Error: "budget exceeded: maxTurns"}, "budget:maxTurns"},
		{RunFinishedData{Outcome: OutcomeFailed, Error: "budget exceeded: maxCostUSD of the run tree (this run used 2 of 1)"}, "budget:maxCostUSD"},
		{RunFinishedData{Outcome: OutcomeFailed, Error: "budget exceeded: maxRepeats (the model made the same call 5 times in a row)"}, RunStopLoop},
		{RunFinishedData{Outcome: OutcomeFailed, Error: "model: 529 overloaded"}, RunStopModelError},
		{RunFinishedData{Outcome: OutcomeFailed, Error: "max_tokens"}, RunStopMaxTokens},
		{RunFinishedData{Outcome: OutcomeFailed, Error: checkInterrupted}, RunStopInterrupted},
		{RunFinishedData{Outcome: OutcomeCanceled, Error: "canceled"}, RunStopCanceled},
		{RunFinishedData{Outcome: OutcomeFailed, Error: "something else"}, RunStopError},
		{RunFinishedData{Outcome: OutcomeFailed, Error: "refusal", Stop: RunStopRefusal}, RunStopRefusal},
	} {
		if got := StopOf(c.d); got != c.want {
			t.Errorf("StopOf(%+v) = %q, want %q", c.d, got, c.want)
		}
	}
	if StopLimit("budget:maxTokens") != "maxTokens" || StopLimit(RunStopLoop) != "" {
		t.Error("StopLimit")
	}
	// A ledger of an older kernel: no field on any event.
	id := "old"
	evs := []Event{
		MustEvent(id, EvRunQueued, RunQueuedData{Agent: "default", Input: "x"}),
		MustEvent(id, EvRunStarted, RunStartedData{}),
		MustEvent(id, EvApprovalRequested, ApprovalRequestedData{ApprovalID: "a1", Tool: "send", Reason: "rerun after resume"}),
	}
	r := ProjectRun(evs)
	if r.Pending == nil || r.Pending.Wait != WaitRerun || r.Stop != "" {
		t.Fatalf("pending = %+v, stop %q", r.Pending, r.Stop)
	}
	evs = append(evs, MustEvent(id, EvRunFinished, RunFinishedData{Outcome: OutcomeFailed, Error: "budget exceeded: deadline"}))
	if r = ProjectRun(evs); r.Stop != "budget:deadline" {
		t.Fatalf("stop = %q", r.Stop)
	}
	evs = append(evs, MustEvent(id, EvMessageReceived, MessageReceivedData{Role: RoleUser, Content: []Block{Text("go on")}, Origin: OriginCLI}))
	if r = ProjectRun(evs); r.Stop != "" || r.State != StateRunning {
		t.Fatalf("after a follow-up: state %s, stop %q", r.State, r.Stop)
	}
	// The JSON of an older event has no field, and a new event without a
	// stop omits it.
	b, _ := json.Marshal(RunFinishedData{Outcome: OutcomeCompleted})
	if string(b) != `{"outcome":"completed"}` {
		t.Fatalf("json = %s", b)
	}
}
