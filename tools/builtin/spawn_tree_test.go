package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// toolResultOf returns the recorded result of a tool call of a run.
func toolResultOf(t *testing.T, a *fw.Agent, run, callID string) fw.ToolRespondedData {
	t.Helper()
	for _, e := range runEvents(t, a, run) {
		var d fw.ToolRespondedData
		if e.Type == fw.EvToolResponded && e.Decode(&d) == nil && d.CallID == callID {
			return d
		}
	}
	t.Fatalf("run %s has no result for the call %s", run, callID)
	return fw.ToolRespondedData{}
}

// TestSpawnTaskSaysTheBudgetIsShared proves that the model reads that its
// children share its budget.
func TestSpawnTaskSaysTheBudgetIsShared(t *testing.T) {
	spec := builtinTool(t, "spawn_task", fw.DefaultConfig()).Spec()
	if !strings.Contains(spec.Description, "Children share the budget of this run") {
		t.Fatalf("description = %q", spec.Description)
	}
}

// TestSpawnTaskRefusesWhenTheTreeBudgetIsUsedUp proves that a child does
// not start in a tree that used up a limit, and that the result names the
// limit that ran out: a call of a run that used up its tree does not run,
// and spawn_task itself refuses a child of such a run.
func TestSpawnTaskRefusesWhenTheTreeBudgetIsUsedUp(t *testing.T) {
	ctx := testCtx(t)
	s := newScripted()
	s.say("plan", callTools(use("c1", "spawn_task", `{"input":"child"}`)), endTurn("never"))
	a := newSpawnAgent(t, s.Model())
	r, err := a.Run(ctx, "plan", fw.WithRunBudget(fw.Budget{MaxTokens: 15}))
	mustState(t, r, err, fw.StateFailed)
	res := toolResultOf(t, a, r.ID, "c1")
	got := fw.TextOf(res.Content)
	for _, want := range []string{"not run", "budget.maxTokens", "15 tokens", "This run stops"} {
		if !res.IsError || !strings.Contains(got, want) {
			t.Fatalf("result %q lacks %q", got, want)
		}
	}
	if kids, _ := a.Children(ctx, r.ID); len(kids) != 0 {
		t.Fatalf("children = %v", kids)
	}
	// The tool refuses a child of the run with the limit that ran out.
	spawn := builtinTool(t, "spawn_task", fw.DefaultConfig())
	res2 := call(t, runCtx(ctx, a, r.ID), spawn, `{"input":"child"}`)
	for _, want := range []string{"no child can start", "shares with the runs under it", "budget.maxTokens", "15 tokens", "stop"} {
		mustFail(t, res2, want)
	}
}

// TestSpawnTaskRefusesWhenTheTreeOfAnAncestorIsUsedUp proves that
// spawn_task refuses a child of a run whose own budget has room, because
// the tree of an ancestor used up a limit, and that the result names the
// ancestor and says that the tree shares the limit.
func TestSpawnTaskRefusesWhenTheTreeOfAnAncestorIsUsedUp(t *testing.T) {
	ctx := testCtx(t)
	s := newScripted()
	s.say("root", endTurn("root done"))
	s.say("spender", endTurn("spent"))
	s.say("sibling", endTurn("sibling done"))
	a := newSpawnAgent(t, s.Model())
	root, err := a.Run(ctx, "root", fw.WithRunBudget(fw.Budget{MaxTokens: 30}))
	mustState(t, root, err, fw.StateCompleted)
	// The root and one child use the 30 tokens of the tree of the root.
	spender, err := a.Run(ctx, "spender", fw.WithParent(root.ID), fw.WithRunBudget(fw.Budget{MaxTokens: 1000}))
	mustState(t, spender, err, fw.StateCompleted)
	sibling, err := a.Run(ctx, "sibling", fw.WithParent(root.ID), fw.WithRunBudget(fw.Budget{MaxTokens: 1000}))
	if err == nil {
		t.Fatalf("a child of a spent tree started: %+v", sibling)
	}
	// A child of the spender has room of its own, 15 of 1000 tokens, but
	// not in the tree of the root.
	spawn := builtinTool(t, "spawn_task", fw.DefaultConfig())
	res := call(t, runCtx(ctx, a, spender.ID), spawn, `{"input":"grandchild"}`)
	for _, want := range []string{"no child can start", "shares with the runs under it", "run " + root.ID + " and the runs under it", "budget.maxTokens", "which the whole tree shares"} {
		mustFail(t, res, want)
	}
}

// TestSpawnTaskRefusesPastTheTreeSize proves that the model reads which
// limit stops a child past spawn.maxDescendants, and goes on without it.
func TestSpawnTaskRefusesPastTheTreeSize(t *testing.T) {
	ctx := testCtx(t)
	cfg := fw.DefaultConfig()
	cfg.Spawn.MaxDescendants = 1
	s := newScripted()
	s.say("plan", callTools(use("c1", "spawn_task", `{"input":"one"}`)), callTools(use("c2", "spawn_task", `{"input":"two"}`)), endTurn("did it myself"))
	s.say("one", endTurn("one done"))
	a := newSpawnAgent(t, s.Model(), fw.WithConfig(cfg))
	r, err := a.Run(ctx, "plan")
	mustState(t, r, err, fw.StateCompleted)
	res := lastToolResults(t, s.requestsFor("plan")[2])[0]
	got := fw.TextOf(res.Content)
	for _, want := range []string{"the spawn limit stops this child", "spawn.maxDescendants", "do the work yourself"} {
		if !res.IsError || !strings.Contains(got, want) {
			t.Fatalf("result %q lacks %q", got, want)
		}
	}
}

// chain makes a tree of depth 3 in which each run starts one child with
// spawn_task, waits for it, and ends with the output of the child.
func chain(s *scripted) {
	for _, step := range [][2]string{{"root", "level 1"}, {"level 1", "level 2"}, {"level 2", "level 3"}} {
		s.say(step[0], callTools(use("c-"+step[1], "spawn_task", `{"input":"`+step[1]+`"}`)))
		s.then(step[0], func(req *fw.Request) *fw.Response {
			last := req.Messages[len(req.Messages)-1].Content
			return endTurn(fw.TextOf(last[len(last)-1].Content))
		})
	}
	s.say("level 3", endTurn("leaf done"))
}

// TestWaitingParentsGiveUpTheirSlots proves that a tree of depth 3 runs
// with one slot for child runs: a run that waits for its child gives its
// slot up, so the child can work, and takes a slot again after.
func TestWaitingParentsGiveUpTheirSlots(t *testing.T) {
	ctx := testCtx(t)
	cfg := fw.DefaultConfig()
	cfg.Spawn.MaxRunning = 1
	s := newScripted()
	chain(s)
	a := newSpawnAgent(t, s.Model(), fw.WithConfig(cfg))
	r, err := a.Run(ctx, "root")
	mustState(t, r, err, fw.StateCompleted)
	if r.Output != "leaf done" {
		t.Fatalf("output %q", r.Output)
	}
}

// TestWaitingParentsGiveUpTheirSlotsForAQuestion proves the same for a
// question of the deepest run: each run on the way relays it while the
// runs below wait, with one slot for child runs.
func TestWaitingParentsGiveUpTheirSlotsForAQuestion(t *testing.T) {
	ctx := testCtx(t)
	cfg := fw.DefaultConfig()
	cfg.Spawn.MaxRunning = 1
	ask := builtinTool(t, "ask_user", cfg)
	s := newScripted()
	for _, step := range [][2]string{{"root", "level 1"}, {"level 1", "level 2"}} {
		s.say(step[0], callTools(use("c-"+step[1], "spawn_task", `{"input":"`+step[1]+`"}`)), endTurn(step[0]+" done"))
	}
	s.say("level 2", callTools(use("q1", "ask_user", `{"question":"Which color?"}`)), endTurn("asked"))
	a := newSpawnAgent(t, s.Model(), fw.WithConfig(cfg), fw.WithTools(ask))
	r, err := a.Run(ctx, "root")
	mustState(t, r, err, fw.StateInputRequired)
	if r.Pending == nil || r.Pending.Reason != "Which color?" {
		t.Fatalf("root pending = %+v", r.Pending)
	}
	if err := a.Decide(ctx, r.ID, fw.ApprovalDecision{ApprovalID: r.Pending.ID, Content: json.RawMessage(`{"answer":"blue"}`)}); err != nil {
		t.Fatal(err)
	}
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, fw.StateCompleted)
	if got := fw.TextOf(lastToolResults(t, s.requestsFor("level 2")[1])[0].Content); !strings.Contains(got, "blue") {
		t.Fatalf("the deepest run got %q", got)
	}
}

// TestChildRunsShareTheRunningSlots proves that children of one response
// wait for a slot of spawn.maxRunning: with one slot, no two children call
// the model at the same time, and all of them finish.
func TestChildRunsShareTheRunningSlots(t *testing.T) {
	ctx := testCtx(t)
	cfg := fw.DefaultConfig()
	cfg.Spawn.MaxRunning = 1
	var mu sync.Mutex
	now, most := 0, 0
	inner := newScripted()
	inner.say("root", callTools(use("a", "spawn_task", `{"input":"a"}`), use("b", "spawn_task", `{"input":"b"}`), use("c", "spawn_task", `{"input":"c"}`)), endTurn("root done"))
	for _, k := range []string{"a", "b", "c"} {
		inner.say(k, endTurn(k+" done"))
	}
	base := inner.Model()
	m := fw.ModelFunc{GenerateFn: func(ctx context.Context, req *fw.Request) (*fw.Response, error) {
		child := lastUserText(req.Messages) != "root" && len(req.Messages) == 1
		if child {
			// Each child takes a while, so children without a limit would
			// overlap.
			mu.Lock()
			now++
			most = max(most, now)
			mu.Unlock()
			time.Sleep(30 * time.Millisecond)
			defer func() {
				mu.Lock()
				now--
				mu.Unlock()
			}()
		}
		return base.Generate(ctx, req)
	}}
	a := newSpawnAgent(t, m, fw.WithConfig(cfg))
	r, err := a.Run(ctx, "root")
	mustState(t, r, err, fw.StateCompleted)
	mu.Lock()
	defer mu.Unlock()
	if most != 1 {
		t.Fatalf("%d children called the model at the same time with one slot", most)
	}
	res := lastToolResults(t, inner.requestsFor("root")[1])
	if len(res) != 3 || res[0].IsError || res[1].IsError || res[2].IsError {
		t.Fatalf("results = %+v", res)
	}
}
