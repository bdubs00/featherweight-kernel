package fw

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The status projection follows what a run that works does now, and a run
// that waits or finished has no activity.
func TestRunActivity(t *testing.T) {
	at := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	r := &Run{}
	seq := uint64(0)
	apply := func(typ string, data any) time.Time {
		t.Helper()
		seq++
		e := MustEvent("R", typ, data)
		e.Seq, e.Time = seq, at.Add(time.Duration(seq)*time.Second)
		r.Apply(e, nil)
		return e.Time
	}
	expect := func(step string, want *RunActivity) {
		t.Helper()
		switch {
		case want == nil && r.Activity != nil:
			t.Fatalf("%s: activity = %+v, want none", step, *r.Activity)
		case want != nil && (r.Activity == nil || *r.Activity != *want):
			t.Fatalf("%s: activity = %+v, want %+v", step, r.Activity, *want)
		}
	}
	apply(EvRunQueued, RunQueuedData{Agent: "default", Input: "work"})
	apply(EvRunStarted, RunStartedData{})
	expect("started", nil)
	t1 := apply(EvTurnStarted, TurnData{Turn: 1})
	expect("turn 1", &RunActivity{Turn: 1, Since: t1})
	apply(EvModelRequested, ModelRequestedData{})
	apply(EvModelResponded, ModelRespondedData{})
	expect("model", &RunActivity{Turn: 1, Since: t1})
	t2 := apply(EvToolRequested, ToolRequestedData{Tool: "bash", CallID: "c1", Input: json.RawMessage(`{"command":"go test ./..."}`)})
	expect("tool", &RunActivity{Turn: 1, Tool: "bash", CallID: "c1", Since: t2})
	apply(EvToolResponded, ToolRespondedData{CallID: "other"})
	expect("result of another call", &RunActivity{Turn: 1, Tool: "bash", CallID: "c1", Since: t2})
	t3 := apply(EvToolResponded, ToolRespondedData{CallID: "c1"})
	expect("result", &RunActivity{Turn: 1, Since: t3})

	// A call that asks a question waits, and goes on after the decision.
	apply(EvToolRequested, ToolRequestedData{Tool: "ask_user", CallID: "c2"})
	apply(EvApprovalRequested, ApprovalRequestedData{ApprovalID: "ap1", Tool: "ask_user", CallID: "c2"})
	expect("question", nil)
	if r.State != StateInputRequired {
		t.Fatalf("state = %s", r.State)
	}
	apply(EvApprovalDecided, ApprovalDecidedData{ApprovalID: "other", Decision: "allow"})
	expect("a decision of another approval", nil)
	t4 := apply(EvApprovalDecided, ApprovalDecidedData{ApprovalID: "ap1", Decision: "allow"})
	expect("decision", &RunActivity{Turn: 1, Tool: "ask_user", CallID: "c2", Since: t4})
	apply(EvToolResponded, ToolRespondedData{CallID: "c2"})
	apply(EvTurnFinished, TurnData{Turn: 1})
	expect("turn finished", nil)

	// A suspend sets the activity aside, and a resume brings it back.
	apply(EvTurnStarted, TurnData{Turn: 2})
	t5 := apply(EvToolRequested, ToolRequestedData{Tool: "bash", CallID: "c3"})
	apply(EvRunSuspended, RunSuspendedData{Reason: "shutdown"})
	expect("suspended", nil)
	t6 := apply(EvRunResumed, RunResumedData{FromSeq: seq})
	expect("resumed", &RunActivity{Turn: 2, Tool: "bash", CallID: "c3", Since: t6})
	if t6 == t5 {
		t.Fatal("the resume kept the old time")
	}
	apply(EvRunFinished, RunFinishedData{Outcome: OutcomeCompleted})
	expect("finished", nil)
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), `"activity"`) {
		t.Fatalf("a finished run shows an activity: %s", b)
	}

	// A follow-up works again from its turn on, and the JSON names it.
	apply(EvMessageReceived, MessageReceivedData{Role: RoleUser, Content: []Block{Text("more")}, Origin: OriginHTTP})
	expect("follow-up", nil)
	apply(EvTurnStarted, TurnData{Turn: 3})
	apply(EvToolRequested, ToolRequestedData{Tool: "bash", CallID: "c4"})
	b, _ = json.Marshal(r)
	if !strings.Contains(string(b), `"activity":{"turn":3,"tool":"bash","callId":"c4","since":`) {
		t.Fatalf("run JSON = %s", b)
	}
	// A late event, which a bus can deliver, changes no activity.
	late := MustEvent("R", EvTurnFinished, TurnData{Turn: 3})
	late.Seq = 2
	r.Apply(late, nil)
	expect("late event", &RunActivity{Turn: 3, Tool: "bash", CallID: "c4", Since: r.Activity.Since})
}

// A run that runs a tool shows the tool as its activity while the tool
// runs, and none after the run finished.
func TestLiveRunActivity(t *testing.T) {
	ctx := testCtx(t)
	entered, release := make(chan struct{}), make(chan struct{})
	slow := NewTool(ToolSpec{Name: "slow", ReadOnly: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		close(entered)
		<-release
		return TextResult("slow done"), nil
	})
	a := newTestAgent(t, newScript(callTools(use("c1", "slow", `{}`)), endTurn("done")).Model(), WithTools(slow))
	r, err := a.Start(ctx, "start")
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	got, err := a.Get(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Activity == nil || got.Activity.Turn != 1 || got.Activity.Tool != "slow" || got.Activity.CallID != "c1" || got.Activity.Since.IsZero() {
		t.Fatalf("activity while the tool runs = %+v", got.Activity)
	}
	close(release)
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)
	if r.Activity != nil {
		t.Fatalf("activity of a finished run = %+v", r.Activity)
	}
}
