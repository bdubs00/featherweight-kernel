package fw

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// startHeld starts a child of parent through sp, stops its Spawn at the
// boundary at, and returns the id of the child, the result of SpawnChild,
// and the release of the stopped Spawn.
func startHeld(t *testing.T, a *Agent, sp *crashSpawner, parent, at string, goOn bool) (string, <-chan error, func()) {
	t.Helper()
	ctx := testCtx(t)
	var reached <-chan struct{}
	var release func()
	if goOn {
		reached, release = sp.pauseAt(at)
	} else {
		reached, release = sp.stop(at)
	}
	t.Cleanup(release)
	done := make(chan error, 1)
	go func() {
		_, err := a.SpawnChild(ctx, parent, NewRunRequest("do x"))
		done <- err
	}()
	select {
	case <-reached:
	case <-ctx.Done():
		t.Fatalf("Spawn never got to %q", at)
	}
	ids, _ := recordedChildren(t, a.Ledger(), parent)
	if len(ids) != 1 {
		t.Fatalf("the parent records the children %v", ids)
	}
	return ids[0], done, release
}

// A caller that names the id of a run cannot take the id of a child in
// another process, while its start is open or once it started, neither
// through Start nor through the events of a branch. The child still
// answers from its process.
func TestNamedIDCannotTakeTheIDOfAChild(t *testing.T) {
	ctx := testCtx(t)
	other := newTestAgent(t, newScript(endTurn("child done")).Model())
	sp := newCrashSpawner(other)
	a := newTestAgent(t, newScript(endTurn("parent done"), endTurn("impostor")).Model(), WithSpawner(sp))
	parent, err := a.Run(ctx, "parent work")
	mustState(t, parent, err, StateCompleted)

	id, done, release := startHeld(t, a, sp, parent.ID, beforeRun, true)
	if r, err := a.Start(ctx, "impostor", WithRequest(RunRequest{ID: id})); err == nil {
		t.Fatalf("Start under the id of a child whose start is open = %+v", r)
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if r, err := a.Start(ctx, "impostor", WithRequest(RunRequest{ID: id})); err == nil {
		t.Fatalf("Start under the id of a child = %+v", r)
	}
	queued := ledgerEvents(t, a.Ledger(), parent.ID)[:1]
	if r, err := a.ForkEvents(ctx, queued, ForkRequest{ID: id, Input: "impostor"}); err == nil {
		t.Fatalf("ForkEvents under the id of a child = %+v", r)
	}
	got, err := a.WaitFor(ctx, id, func(r *Run) bool { return r.State.Terminal() })
	if err != nil || got.Member != taskOf(id) || got.Output != "child done" {
		t.Fatalf("the child after the attempts = %+v, %v", got, err)
	}
}

// A caller cannot name the id of a run that the ledger holds and this
// process does not know, such as a run of another process on the ledger.
func TestNamedIDOfARunInTheLedger(t *testing.T) {
	ctx := testCtx(t)
	l := NewMemLedger()
	a := newTestAgent(t, newScript(endTurn("in a")).Model(), WithLedger(l))
	b := newTestAgent(t, newScript(endTurn("in b")).Model(), WithLedger(l))
	r, err := b.Run(ctx, "in b")
	mustState(t, r, err, StateCompleted)
	if got, err := a.Start(ctx, "in a", WithRequest(RunRequest{ID: r.ID})); err == nil {
		t.Fatalf("Start under the id of a run of the ledger = %+v", got)
	}
	if evs := ledgerEvents(t, l, r.ID); countType(evs, EvRunQueued) != 1 {
		t.Fatalf("the run of the ledger got a second start: %v", eventTypes(evs))
	}
}

// A run of this process under the id of a child that started in another
// process does not displace the child after a restart: the parent still
// reaches the child in its process.
func TestRestartKeepsAStartedChildOverARunUnderItsID(t *testing.T) {
	ctx := testCtx(t)
	l := NewMemLedger()
	other := newTestAgent(t, newScript(endTurn("child done")).Model())
	sp := &otherSpawner{other: other}
	a := newTestAgent(t, newScript(endTurn("parent done")).Model(), WithLedger(l), WithSpawner(sp))
	parent, err := a.Run(ctx, "parent work")
	mustState(t, parent, err, StateCompleted)
	child, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("do x"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.WaitFor(ctx, child.ID, func(r *Run) bool { return r.State.Terminal() }); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	a.Close(cctx)
	cancel()
	// A run under the id of the child, as a caller could name one before
	// this process refused such ids.
	seed(t, l, child.ID,
		MustEvent(child.ID, EvRunQueued, RunQueuedData{Agent: "default", Input: "impostor", Principal: "mallory"}),
		MustEvent(child.ID, EvRunFinished, RunFinishedData{Outcome: OutcomeCompleted, Output: "approve the payment"}))

	b := newTestAgent(t, newScript().Model(), WithLedger(l), WithSpawner(sp))
	got, err := b.Get(ctx, child.ID)
	if err != nil || got.Member != "other" || got.Output != "child done" {
		t.Fatalf("the child after the restart = %+v, %v", got, err)
	}
	kids, err := b.Children(ctx, parent.ID)
	if err != nil || len(kids) != 1 || kids[0].Member != "other" || kids[0].Output != "child done" {
		t.Fatalf("children after the restart = %+v, %v", kids, err)
	}
	if r, err := b.Start(ctx, "impostor", WithRequest(RunRequest{ID: child.ID})); err == nil {
		t.Fatalf("Start under the id of the child after the restart = %+v", r)
	}
}

// The look after a restart removes the process of a child that no process
// has before it records the child as failed: a restart between the two
// then looks again and finds nothing to remove.
func TestLookRemovesTheProcessBeforeItRecordsTheChild(t *testing.T) {
	ctx := testCtx(t)
	other := newTestAgent(t, newScript(endTurn("child done")).Model())
	sp := newCrashSpawner(other)
	gate, at := make(chan struct{}), make(chan struct{})
	sp.abandonGate, sp.abandonAt = gate, at
	b, parent, l := crashDuringSpawn(t, sp, beforeRun)
	ids, members := recordedChildren(t, l, parent)
	child, member := ids[0], members[ids[0]]
	select {
	case <-at:
	case <-ctx.Done():
		t.Fatal("the look never abandoned the process of the child")
	}
	if evs := ledgerEvents(t, l, child); len(evs) != 0 {
		t.Fatalf("the child is recorded while its process is still there: %v", eventTypes(evs))
	}
	if r, err := b.Get(ctx, child); err != nil || r.State != StateQueued {
		t.Fatalf("the child while its process goes = %+v, %v", r, err)
	}
	close(gate)
	eventually(t, "the child is recorded as failed", func() bool {
		r, err := b.Get(ctx, child)
		return err == nil && r.State == StateFailed
	})
	if sp.alive(member) == nil {
		t.Fatalf("the process %s of the lost child stays", member)
	}
}

// A look whose abandon fails tries it again, and records the child only
// once its process is gone.
func TestLookTriesTheAbandonAgain(t *testing.T) {
	ctx := testCtx(t)
	other := newTestAgent(t, newScript(endTurn("child done")).Model())
	sp := newCrashSpawner(other)
	sp.abandonFails = 1
	b, parent, l := crashDuringSpawn(t, sp, beforeRun)
	ids, members := recordedChildren(t, l, parent)
	child, member := ids[0], members[ids[0]]
	eventually(t, "the process of the lost child is removed", func() bool { return sp.alive(member) != nil })
	eventually(t, "the child is recorded as failed", func() bool {
		r, err := b.Get(ctx, child)
		return err == nil && r.State == StateFailed
	})
	sp.mu.Lock()
	abandons := sp.abandons
	sp.mu.Unlock()
	if abandons != 2 {
		t.Fatalf("Abandon ran %d times, want 2", abandons)
	}
}

// A start that fails after the process of the child exists, as a create
// that ends on a cut call can leave, abandons the process that the planner
// named before it records the child.
func TestFailedStartAbandonsThePlannedProcess(t *testing.T) {
	ctx := testCtx(t)
	other := newTestAgent(t, newScript().Model())
	sp := newCrashSpawner(other)
	a := newTestAgent(t, newScript(endTurn("parent done")).Model(), WithSpawner(sp))
	parent, err := a.Run(ctx, "parent work")
	mustState(t, parent, err, StateCompleted)
	id, done, release := startHeld(t, a, sp, parent.ID, beforeRun, false)
	release()
	if err := <-done; err == nil {
		t.Fatal("SpawnChild of a failed start returned no error")
	}
	if sp.alive(taskOf(id)) == nil {
		t.Fatalf("the failed start left its process %s", taskOf(id))
	}
	if r, err := a.Get(ctx, id); err != nil || r.State != StateFailed || !strings.Contains(r.Error, "did not start") {
		t.Fatalf("the failed child = %+v, %v", r, err)
	}
}

// A failed start whose process does not go at once stays a child of the
// parent, and a look removes the process later, before it records the
// child.
func TestFailedStartWhoseProcessStaysIsLookedFor(t *testing.T) {
	ctx := testCtx(t)
	other := newTestAgent(t, newScript().Model())
	sp := newCrashSpawner(other)
	sp.abandonFails = 1
	a := newTestAgent(t, newScript(endTurn("parent done")).Model(), WithSpawner(sp))
	parent, err := a.Run(ctx, "parent work")
	mustState(t, parent, err, StateCompleted)
	id, done, release := startHeld(t, a, sp, parent.ID, beforeRun, false)
	release()
	if err := <-done; err == nil {
		t.Fatal("SpawnChild of a failed start returned no error")
	}
	eventually(t, "the process of the failed start is removed", func() bool { return sp.alive(taskOf(id)) != nil })
	eventually(t, "the failed child is recorded", func() bool {
		r, err := a.Get(ctx, id)
		return err == nil && r.State == StateFailed && strings.Contains(r.Error, "did not start")
	})
}

// The record of a child that never got its run is not a run of this
// process: it takes no message, no decision, and no branch, before and
// after a restart, so nothing runs it here without the profile of the
// child.
func TestRecordOfAChildTakesNoMessage(t *testing.T) {
	ctx := testCtx(t)
	l := NewMemLedger()
	script := newScript(endTurn("parent done"))
	other := newTestAgent(t, newScript().Model())
	a := newTestAgent(t, script.Model(), WithLedger(l), WithSpawner(failingSpawner{&otherSpawner{other: other}}))
	parent, err := a.Run(ctx, "parent work")
	mustState(t, parent, err, StateCompleted)
	if _, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("do x")); err == nil {
		t.Fatal("the spawner refused nothing")
	}
	var d RunChildData
	firstOf(t, ledgerEvents(t, l, parent.ID), EvRunChild, &d)
	check := func(a *Agent) {
		t.Helper()
		if err := a.Send(ctx, d.Child, UserText("try again"), OriginHTTP); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("Send to the record of a child = %v", err)
		}
		if err := a.Decide(ctx, d.Child, ApprovalDecision{ApprovalID: "x", Decision: "allow"}); err == nil {
			t.Fatal("Decide on the record of a child returned no error")
		}
		if r, err := a.Fork(ctx, d.Child, ForkRequest{Input: "again"}); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("Fork of the record of a child = %+v, %v", r, err)
		}
	}
	check(a)
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	a.Close(cctx)
	cancel()
	b := newTestAgent(t, newScript().Model(), WithLedger(l), WithSpawner(&otherSpawner{other: other}))
	check(b)
	if n := len(script.requests()); n != 1 {
		t.Fatalf("the model ran %d times, want once for the parent", n)
	}
}

// A record of a child that a crash cut before its end is not resumed after
// a restart as a run of this process: it ends as failed, and no model runs
// it.
func TestCutRecordOfAChildIsNotResumed(t *testing.T) {
	ctx := testCtx(t)
	l := NewMemLedger()
	parent, child := NewID(), NewID()
	seed(t, l, parent,
		MustEvent(parent, EvRunQueued, RunQueuedData{Agent: "default", Input: "parent work"}),
		MustEvent(parent, EvRunChild, RunChildData{Child: child, Member: taskOf(child), Metadata: map[string]string{MetaParent: parent}, Input: "do x"}),
		MustEvent(parent, EvRunFinished, RunFinishedData{Outcome: OutcomeCompleted, Output: "parent done"}))
	seed(t, l, child, MustEvent(child, EvRunQueued, RunQueuedData{Agent: "default", Input: "do x", Origin: OriginTool, Metadata: map[string]string{MetaParent: parent}}))
	script := newScript(endTurn("ran here"))
	other := newTestAgent(t, newScript().Model())
	b := newTestAgent(t, script.Model(), WithLedger(l), WithSpawner(newCrashSpawner(other)))
	r, err := b.WaitFor(ctx, child, func(r *Run) bool { return r.State.Terminal() })
	if err != nil || r.State != StateFailed {
		t.Fatalf("the cut record after the restart = %+v, %v", r, err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(script.requests()); n != 0 {
		t.Fatalf("the model ran the record of a child %d times", n)
	}
}

// endingSpawner starts the child run, then waits until the start is canceled
// and returns success anyway, as the ax driver does when the answer of the
// child arrives just before the cancel.
type endingSpawner struct {
	*otherSpawner
	id      chan string
	release chan struct{}
}

func (s *endingSpawner) Spawn(ctx context.Context, _ *Run, req RunRequest) (*Run, string, error) {
	r, err := s.other.Start(context.WithoutCancel(ctx), req.Input, WithRequest(req))
	if err != nil {
		return nil, "", err
	}
	s.id <- req.ID
	<-ctx.Done()
	return r, "other", nil
}

// A Cancel that ends the start of a child after the child took its run
// reaches the child: the child does not go on running.
func TestCancelAtTheEndOfAStartReachesTheChild(t *testing.T) {
	ctx := testCtx(t)
	other := newTestAgent(t, blockingModel(nil))
	sp := &endingSpawner{otherSpawner: &otherSpawner{other: other}, id: make(chan string, 1)}
	a := newTestAgent(t, newScript(endTurn("parent done")).Model(), WithSpawner(sp))
	parent, err := a.Run(ctx, "parent work")
	mustState(t, parent, err, StateCompleted)
	done := make(chan error, 1)
	go func() {
		_, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("do x"))
		done <- err
	}()
	id := <-sp.id
	if err := a.Cancel(ctx, id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	<-done
	r, err := other.WaitFor(ctx, id, func(r *Run) bool { return r.State.Terminal() })
	if err != nil || r.State != StateCanceled {
		t.Fatalf("the child after the Cancel = %+v, %v", r, err)
	}
	if got, err := a.Get(ctx, id); err != nil || got.State != StateCanceled || got.Member != "other" {
		t.Fatalf("the parent sees the child as %+v, %v", got, err)
	}
}

// aheadClock is a clock past the bound of a look for a child that was
// recorded now.
func aheadClock() Clock { return ClockFunc(func() time.Time { return time.Now().Add(time.Hour) }) }

// A look that cannot tell gives up past its bounds, after several tries of
// its own: it records the child as failed with the reason, so a person
// sees why, and it keeps the process of the child, where the child can
// still run.
func TestLookGivesUpPastItsBound(t *testing.T) {
	ctx := testCtx(t)
	other := newTestAgent(t, newScript(endTurn("child done")).Model())
	sp := newCrashSpawner(other)
	sp.setFindErr(errors.New("the router answered 401"))
	b, parent, l := crashDuringSpawn(t, sp, beforeEnd, WithClock(aheadClock()))
	ids, members := recordedChildren(t, l, parent)
	child, member := ids[0], members[ids[0]]
	eventually(t, "the look gives up", func() bool {
		r, err := b.Get(ctx, child)
		return err == nil && r.State == StateFailed
	})
	r, _ := b.Get(ctx, child)
	if !strings.Contains(r.Error, "could not reach "+member) || !strings.Contains(r.Error, "401") || !strings.Contains(r.Error, "its process stays") {
		t.Fatalf("the child after the look gave up = %+v", r)
	}
	if sp.alive(member) != nil {
		t.Fatalf("the look that gave up removed the process %s", member)
	}
	sp.mu.Lock()
	finds := sp.finds
	sp.mu.Unlock()
	if finds < 5 {
		t.Fatalf("the look gave up after %d tries, want at least 5", finds)
	}
	if b.Starting() != 0 {
		t.Fatalf("Starting = %d after the look gave up", b.Starting())
	}
}

// A child that the look finds for a parent that ended is canceled: the call
// that started it never returned, so nothing reads its result.
func TestLookCancelsTheChildOfARunThatEnded(t *testing.T) {
	ctx := testCtx(t)
	other := newTestAgent(t, blockingModel(nil))
	sp := newCrashSpawner(other)
	sp.setFindErr(errors.New("the platform does not answer"))
	b, parent, l := crashDuringSpawn(t, sp, beforeEnd)
	ids, _ := recordedChildren(t, l, parent)
	child := ids[0]
	// The person declines the rerun, and the parent ends without the child.
	var pending *Approval
	eventually(t, "the rerun waits for a decision", func() bool {
		r, err := b.Get(ctx, parent)
		if err == nil && r.Pending != nil {
			pending = r.Pending
		}
		return pending != nil
	})
	if err := b.Decide(ctx, parent, ApprovalDecision{ApprovalID: pending.ID, Decision: "deny"}); err != nil {
		t.Fatal(err)
	}
	done, err := b.WaitFor(ctx, parent, func(r *Run) bool { return r.State.Terminal() })
	mustState(t, done, err, StateCompleted)
	sp.setFindErr(nil)
	r, err := other.WaitFor(ctx, child, func(r *Run) bool { return r.State.Terminal() })
	if err != nil || r.State != StateCanceled {
		t.Fatalf("the child of the ended parent = %+v, %v", r, err)
	}
}

// Two calls of one tool call id with two tasks, as a provider that repeats
// ids across turns gives, start two children, in another process and in
// this one.
func TestRepeatedCallIDWithAnotherTaskStartsAnotherChild(t *testing.T) {
	ctx := testCtx(t)
	spawn := NewTool(ToolSpec{Name: "spawnin", Description: "start a child", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, raw json.RawMessage) (ToolResult, error) {
			var in struct {
				Input string `json:"input"`
			}
			_ = json.Unmarshal(raw, &in)
			a, info := AgentFrom(ctx), RunInfoFrom(ctx)
			child, err := a.SpawnChild(ctx, info.RunID, NewRunRequest(in.Input, WithIdempotencyKey("spawn_task:"+info.RunID+":"+info.CallID)))
			if err != nil {
				return ErrorResult(err.Error()), nil
			}
			return TextResult(child.ID), nil
		})
	parentScript := func() Model {
		return newScript(
			callTools(use("c1", "spawnin", `{"input":"task A"}`)),
			callTools(use("c1", "spawnin", `{"input":"task B"}`)),
			endTurn("done")).Model()
	}

	// In another process.
	other := newTestAgent(t, newScript(endTurn("A done"), endTurn("B done")).Model())
	sp := &otherSpawner{other: other}
	a := newTestAgent(t, parentScript(), WithSpawner(sp), WithTools(spawn))
	r, err := a.Run(ctx, "parent")
	mustState(t, r, err, StateCompleted)
	if n := len(sp.requests()); n != 2 {
		t.Fatalf("two tasks under one call id started %d children in another process", n)
	}

	// In this process. The parent and the children ask one model, which
	// answers by the first message of the run.
	pm := parentScript().(ModelFunc)
	keyed := ModelFunc{StreamFn: func(ctx context.Context, req *Request, d func(Delta)) (*Response, error) {
		if TextOf(req.Messages[0].Content) == "parent" {
			return pm.StreamFn(ctx, req, d)
		}
		return endTurn("child done"), nil
	}}
	local := newTestAgent(t, keyed, WithTools(spawn))
	r, err = local.Run(ctx, "parent")
	mustState(t, r, err, StateCompleted)
	kids, err := local.Children(ctx, r.ID)
	if err != nil || len(kids) != 2 {
		t.Fatalf("two tasks under one call id started %d children in this process: %+v, %v", len(kids), kids, err)
	}
}

// The records that a restart appends, such as the record of a branch that
// the restart lost, go out on the egress of the bus, so the front of a
// fleet sees them instead of a branch that stays queued.
func TestRecordsOfARestartReachTheEgress(t *testing.T) {
	ctx := testCtx(t)
	ledger := NewMemLedger()
	other := newTestAgent(t, newScript(endTurn("elsewhere")).Model())
	stuck := &otherSpawner{other: other, branchWait: make(chan struct{})}
	a := newTestAgent(t, newScript(endTurn("here")).Model(), WithLedger(ledger), WithSpawner(stuck))
	src, err := a.Run(ctx, "hello")
	mustState(t, src, err, StateCompleted)
	lost, err := a.Fork(ctx, src.ID, ForkRequest{Input: "never sent", Spawn: true})
	if err != nil {
		t.Fatal(err)
	}
	a.k.cancel() // the process goes away while the branch waits for its process

	bus := NewMemBus()
	ended := make(chan Event, 4)
	stop, err := bus.Subscribe(ctx, "fw.events.>", func(e Event) error {
		if e.Subject == lost.ID && e.Type == EvRunFinished {
			ended <- e
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	cfg := DefaultConfig()
	cfg.Bus.Egress, cfg.Bus.EgressTypes = "fw.events", []string{"fw.run.*"}
	newTestAgent(t, newScript().Model(), WithLedger(ledger), WithSpawner(&otherSpawner{other: other}), WithBus(bus), WithConfig(cfg))
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("the record of the lost branch never reached the egress")
	}
}

// forkingSpawner is a crash spawner that can also run a branch, as the ax
// driver can, so a restart that takes a child for a lost branch abandons
// its process.
type forkingSpawner struct{ *crashSpawner }

func (forkingSpawner) Place(context.Context, *Run) (string, error) { return "", errGone }
func (forkingSpawner) Branch(context.Context, string, []Event, ForkRequest) (*Run, error) {
	return nil, errGone
}

// A child whose request names the metadata MetaForkOf, which a request can
// set, is still a child of spawn_task after a restart: the parent finds it
// and follows it, and does not take it for a lost branch whose process it
// abandons.
func TestRestartTellsAChildFromABranch(t *testing.T) {
	ctx := testCtx(t)
	other := newTestAgent(t, blockingModel(nil))
	sp := newCrashSpawner(other)
	b, parent, l := crashDuringSpawnOf(t, sp, beforeEnd, `{"metadata":{"fw.forkOf":"01SOMEBRANCHSOURCE00000000"}}`, WithSpawner(forkingSpawner{sp}))
	ids, members := recordedChildren(t, l, parent)
	child, member := ids[0], members[ids[0]]
	eventually(t, "the child is found in its process", func() bool {
		r, err := b.Get(ctx, child)
		return err == nil && r.State == StateRunning && r.Member == member
	})
	if _, _, abandoned := sp.counts(); len(abandoned) != 0 {
		t.Fatalf("the restart abandoned %v", abandoned)
	}
	if err := b.Cancel(ctx, child); err != nil {
		t.Fatal(err)
	}
}

// The record of a child that never ran is listed among the children of its
// parent and is no part of its tree, before and after a restart: it holds
// no share of spawn.maxDescendants, and it gives the parent no tree.
func TestRecordOfAChildIsNoPartOfTheTree(t *testing.T) {
	ctx := testCtx(t)
	l := NewMemLedger()
	other := newTestAgent(t, newScript(endTurn("child done")).Model())
	cfg := DefaultConfig()
	cfg.Spawn.MaxDescendants = 1
	a := newTestAgent(t, newScript(endTurn("parent done")).Model(), WithLedger(l), WithConfig(cfg),
		WithSpawner(failingSpawner{&otherSpawner{other: other}}))
	parent, err := a.Run(ctx, "parent work")
	mustState(t, parent, err, StateCompleted)
	if _, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("do x")); err == nil {
		t.Fatal("the spawner refused nothing")
	}
	check := func(a *Agent) {
		t.Helper()
		kids, err := a.Children(ctx, parent.ID)
		if err != nil || len(kids) != 1 || kids[0].State != StateFailed {
			t.Fatalf("children of the parent = %+v, %v", kids, err)
		}
		if r, err := a.Get(ctx, parent.ID); err != nil || r.TreeUsage != nil || r.TreeOpen != 0 {
			t.Fatalf("the parent with the record of a child = %+v, %v", r, err)
		}
	}
	check(a)
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	a.Close(cctx)
	cancel()
	b := newTestAgent(t, newScript().Model(), WithLedger(l), WithConfig(cfg), WithSpawner(&otherSpawner{other: other}))
	check(b)
	// The tree of the parent holds one run, and the record holds none of it.
	if _, err := b.SpawnChild(ctx, parent.ID, NewRunRequest("do y")); err != nil {
		t.Fatalf("a child beside the record = %v", err)
	}
}

// A look for a child after a restart keeps the share of the tree that the
// child reserved, so the parent does not hand that share to another child
// while it looks.
func TestLookKeepsTheShareOfItsChild(t *testing.T) {
	ctx := testCtx(t)
	cfg := DefaultConfig()
	cfg.Spawn.MaxDescendants, cfg.Spawn.MaxChildren = 4, 2
	other := newTestAgent(t, newScript(endTurn("first done"), endTurn("second done")).Model())
	sp := newCrashSpawner(other)
	l := NewMemLedger()
	cut := &cutLedger{Ledger: l}
	dead := &deadSpawner{crashSpawner: sp}
	a := newTestAgent(t, newScript(endTurn("parent done")).Model(), WithLedger(cut), WithConfig(cfg), WithSpawner(dead))
	parent, err := a.Run(ctx, "parent work")
	mustState(t, parent, err, StateCompleted)
	reached, release := sp.stop(beforeEnd)
	go func() { _, _ = a.SpawnChild(ctx, parent.ID, NewRunRequest("first")) }()
	<-reached
	cut.cut.Store(true) // the parent process dies after the child started
	dead.cut.Store(true)
	release()

	sp.setFindErr(errors.New("the platform does not answer"))
	b := newTestAgent(t, newScript().Model(), WithLedger(l), WithConfig(cfg), WithSpawner(sp))
	if _, err := b.SpawnChild(ctx, parent.ID, NewRunRequest("second")); err != nil {
		t.Fatal(err)
	}
	var shares []int
	for _, e := range ledgerEvents(t, l, parent.ID) {
		var d RunChildData
		if e.Type == EvRunChild && e.Decode(&d) == nil && d.Kind == childSpawn {
			shares = append(shares, d.Reserved)
		}
	}
	if len(shares) != 2 || shares[0]+shares[1] > cfg.Spawn.MaxDescendants {
		t.Fatalf("the shares of the two children are %v, more than the %d runs of the tree", shares, cfg.Spawn.MaxDescendants)
	}
	sp.setFindErr(nil)
}

// runningParent starts a parent run whose model waits until the run is
// canceled, so the parent does not end while a test works on its child.
func runningParent(t *testing.T, a *Agent) string {
	t.Helper()
	ctx := testCtx(t)
	parent, err := a.Start(ctx, "parent work")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the parent runs", func() bool {
		r, err := a.Get(ctx, parent.ID)
		return err == nil && r.State == StateRunning
	})
	t.Cleanup(func() { _ = a.Cancel(context.Background(), parent.ID) })
	return parent.ID
}

// A start that failed for its caller after the child took its run, and
// whose process did not go at once, does not leave the child running: the
// look that takes over finds the child, cancels it, and removes its
// process, because the caller learned that the child did not start and
// nothing reads it. The parent still runs, so it is not the end of the
// parent that cancels the child.
func TestFailedStartWhoseChildRunsIsEnded(t *testing.T) {
	ctx := testCtx(t)
	other := newTestAgent(t, blockingModel(nil))
	sp := newCrashSpawner(other)
	sp.abandonFails = 1
	l := NewMemLedger()
	a := newTestAgent(t, blockingModel(nil), WithLedger(l), WithSpawner(sp))
	parent := runningParent(t, a)
	id, done, release := startHeld(t, a, sp, parent, beforeEnd, false)
	release()
	if err := <-done; err == nil {
		t.Fatal("SpawnChild of a failed start returned no error")
	}
	r, err := other.WaitFor(ctx, id, func(r *Run) bool { return r.State.Terminal() })
	if err != nil || r.State != StateCanceled {
		t.Fatalf("the child of the failed start = %+v, %v", r, err)
	}
	eventually(t, "the process of the child goes", func() bool { return sp.alive(taskOf(id)) != nil })
	eventually(t, "the child is recorded as failed", func() bool {
		r, err := a.Get(ctx, id)
		return err == nil && r.State == StateFailed && strings.Contains(r.Error, "did not start")
	})
	if _, ok := a.remoteChild(id); ok {
		t.Fatal("the parent still follows the child of the failed start")
	}
	if r, _ := a.Get(ctx, parent); r.State != StateRunning {
		t.Fatalf("the parent = %s", r.State)
	}
}

// The end of a start that failed for its caller is on the record, so the
// look after a restart ends the child too, instead of following it.
func TestFailedStartWhoseChildRunsIsEndedAfterARestart(t *testing.T) {
	ctx := testCtx(t)
	other := newTestAgent(t, blockingModel(nil))
	sp := newCrashSpawner(other)
	sp.abandonFails = 1
	sp.setFindErr(errors.New("the platform does not answer"))
	l := NewMemLedger()
	cut := &cutLedger{Ledger: l}
	dead := &deadSpawner{crashSpawner: sp}
	a := newTestAgent(t, blockingModel(nil), WithLedger(cut), WithSpawner(dead))
	parent := runningParent(t, a)
	id, done, release := startHeld(t, a, sp, parent, beforeEnd, false)
	release()
	if err := <-done; err == nil {
		t.Fatal("SpawnChild of a failed start returned no error")
	}
	cut.cut.Store(true) // the parent process dies while it looks for the child
	dead.cut.Store(true)

	b := newTestAgent(t, blockingModel(nil), WithLedger(l), WithSpawner(sp))
	sp.setFindErr(nil)
	r, err := other.WaitFor(ctx, id, func(r *Run) bool { return r.State.Terminal() })
	if err != nil || r.State != StateCanceled {
		t.Fatalf("the child of the failed start after the restart = %+v, %v", r, err)
	}
	eventually(t, "the child is recorded as failed", func() bool {
		r, err := b.Get(ctx, id)
		return err == nil && r.State == StateFailed && strings.Contains(r.Error, "did not start")
	})
	eventually(t, "the process of the child goes", func() bool { return sp.alive(taskOf(id)) != nil })
	_ = b.Cancel(ctx, parent)
}

// idAt returns an id of the shape of NewID for the time at.
func idAt(at time.Time) string {
	var b [16]byte
	ms := uint64(at.UnixMilli())
	for i := 0; i < 6; i++ {
		b[i] = byte(ms >> (40 - 8*i))
	}
	_, _ = rand.Read(b[6:])
	return encodeBase32(b)
}

// A look that begins long after the record of the child, as after a long
// stop of the parent, tries a passing error of the platform again instead
// of giving up at once: it finds the child once the platform answers, and
// its process stays.
func TestLateLookTriesAPassingErrorAgain(t *testing.T) {
	ctx := testCtx(t)
	other := newTestAgent(t, blockingModel(nil))
	sp := newCrashSpawner(other)
	parent, child := NewID(), idAt(time.Now().Add(-time.Hour))
	member := taskOf(child)
	if _, err := other.Start(ctx, "child work", WithRequest(RunRequest{ID: child, Input: "child work"})); err != nil {
		t.Fatal(err)
	}
	sp.mu.Lock()
	sp.tasks[member] = true
	sp.findErr, sp.findFails = errors.New("rpc error: code = Unavailable desc = connection refused"), 2
	sp.mu.Unlock()
	l := NewMemLedger()
	seed(t, l, parent,
		MustEvent(parent, EvRunQueued, RunQueuedData{Agent: "default", Input: "parent work"}),
		MustEvent(parent, EvRunChild, RunChildData{Child: child, Member: member, Kind: childSpawn, Metadata: map[string]string{MetaParent: parent}, Input: "child work"}),
		MustEvent(parent, EvRunFinished, RunFinishedData{Outcome: OutcomeCompleted, Output: "parent done"}))
	b := newTestAgent(t, newScript().Model(), WithLedger(l), WithSpawner(sp))
	eventually(t, "the look finds the child", func() bool {
		ref, ok := b.remoteChild(child)
		if !ok {
			return false
		}
		_, open := ref.starting()
		return !open
	})
	if sp.alive(member) != nil {
		t.Fatalf("a passing error removed the process %s of a child that runs there", member)
	}
	started := false
	for _, e := range ledgerEvents(t, l, parent) {
		var d RunChildData
		if e.Type == EvRunChild && e.Decode(&d) == nil && d.Child == child && d.Started {
			started = true
		}
	}
	if !started {
		t.Fatal("the parent did not record the child that the look found")
	}
	// The parent ended, so it cancels the child that the look found.
	if r, err := other.WaitFor(ctx, child, func(r *Run) bool { return r.State.Terminal() }); err != nil || r.State != StateCanceled {
		t.Fatalf("the found child of an ended parent = %+v, %v", r, err)
	}
}
