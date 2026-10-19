package fw

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// readCounter counts the reads and the cancels of each child of a
// treeSpawner, and fails the first cancels of a child when told to.
type readCounter struct {
	*treeSpawner
	reads      atomic.Int64
	mu         sync.Mutex
	readsOf    map[string]int
	failCancel map[string]int // cancels of the child that fail first
	cancels    map[string]int
}

func newReadCounter() *readCounter {
	return &readCounter{treeSpawner: newTreeSpawner(), readsOf: map[string]int{}, failCancel: map[string]int{}, cancels: map[string]int{}}
}

func (s *readCounter) Get(ctx context.Context, member, id string) (*Run, error) {
	s.reads.Add(1)
	s.mu.Lock()
	s.readsOf[id]++
	s.mu.Unlock()
	return s.treeSpawner.Get(ctx, member, id)
}

func (s *readCounter) readsOfChild(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readsOf[id]
}

func (s *readCounter) Cancel(ctx context.Context, member, id string) error {
	s.mu.Lock()
	s.cancels[id]++
	fail := s.failCancel[id] > 0
	if fail {
		s.failCancel[id]--
	}
	s.mu.Unlock()
	if fail {
		return errors.New("the router does not answer")
	}
	return s.treeSpawner.Cancel(ctx, member, id)
}

func (s *readCounter) cancelsOf(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancels[id]
}

// readsIn returns the reads of the spawner during d.
func (s *readCounter) readsIn(d time.Duration) int64 {
	before := s.reads.Load()
	time.Sleep(d)
	return s.reads.Load() - before
}

// TestReaderReadsOnlyWorkingChildren proves that the background reads of a
// child in another process stop while the child does not work: when it
// waits for a person, sleeps, or finished with no run under it that works.
// A finished child with a run under it that works is still read.
func TestReaderReadsOnlyWorkingChildren(t *testing.T) {
	ctx := testCtx(t)
	sp := newReadCounter()
	a := newTestAgent(t, newScript(endTurn("parent done")).Model(), WithSpawner(sp), withFollow(10*time.Millisecond, time.Hour))
	parent, err := a.Run(ctx, "parent")
	mustState(t, parent, err, StateCompleted)
	child, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("background"))
	if err != nil {
		t.Fatal(err)
	}
	if n := sp.readsIn(100 * time.Millisecond); n < 3 {
		t.Fatalf("a running child was read %d times in 100ms", n)
	}
	for _, st := range []RunState{StateInputRequired, StateSuspended} {
		sp.report(child.ID, st, tokens(5), tokens(5))
		eventually(t, "a read of the child "+string(st), func() bool { return a.Following() == 0 })
		if n := sp.readsIn(150 * time.Millisecond); n > 0 {
			t.Fatalf("a child that is %s was read %d times", st, n)
		}
		// A wait or a person reads the child, and it works again.
		sp.report(child.ID, StateRunning, tokens(5), tokens(5))
		if _, err := a.Get(ctx, child.ID); err != nil {
			t.Fatal(err)
		}
		if a.Following() != 1 {
			t.Fatalf("Following = %d for a child that works again", a.Following())
		}
	}
	// A finished child whose grandchild works is read; one whose grandchild
	// waits for a person is not.
	sp.treeSpawner.mu.Lock()
	sp.runs[child.ID].State, sp.runs[child.ID].TreeOpen, sp.runs[child.ID].TreeWorking = StateCompleted, 1, 1
	sp.treeSpawner.mu.Unlock()
	if n := sp.readsIn(100 * time.Millisecond); n < 3 {
		t.Fatalf("a finished child with a working grandchild was read %d times", n)
	}
	sp.treeSpawner.mu.Lock()
	sp.runs[child.ID].TreeWorking = 0
	sp.treeSpawner.mu.Unlock()
	eventually(t, "a read of the child", func() bool { return a.Following() == 0 })
	if n := sp.readsIn(150 * time.Millisecond); n > 0 {
		t.Fatalf("a finished child whose tree waits was read %d times", n)
	}
}

// TestReaderAfterARestart proves that a restart reads again only a child
// that worked when the parent knew it last: not a child that waits for a
// person, and not a child that an older kernel recorded without a state.
func TestReaderAfterARestart(t *testing.T) {
	ctx := testCtx(t)
	l := NewMemLedger()
	sp := newReadCounter()
	a := newTestAgent(t, newScript(endTurn("parent done")).Model(), WithSpawner(sp), WithLedger(l), withFollow(time.Hour, time.Hour))
	parent, err := a.Run(ctx, "parent")
	mustState(t, parent, err, StateCompleted)
	waiting, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("waits"))
	if err != nil {
		t.Fatal(err)
	}
	working, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("works"))
	if err != nil {
		t.Fatal(err)
	}
	sp.report(waiting.ID, StateInputRequired, tokens(5), tokens(5))
	if _, err := a.Get(ctx, waiting.ID); err != nil {
		t.Fatal(err)
	}
	// A child that an older kernel recorded: no kind, no report.
	for _, d := range []RunChildData{{Child: "OLD", Member: "remote", Metadata: map[string]string{MetaParent: parent.ID}}, {Child: "OLD", Member: "remote", Started: true}} {
		if _, err := a.emit(nil, mustRun(t, a, parent.ID), EvRunChild, d); err != nil {
			t.Fatal(err)
		}
	}
	sp.treeSpawner.mu.Lock()
	sp.runs["OLD"] = &Run{ID: "OLD", State: StateCompleted}
	sp.treeSpawner.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	a.Close(cctx)
	cancel()

	b := newTestAgent(t, newScript().Model(), WithSpawner(sp), WithLedger(l), withFollow(10*time.Millisecond, time.Hour))
	if b.Following() != 1 {
		t.Fatalf("Following after the restart = %d, want the working child alone", b.Following())
	}
	before := map[string]int{waiting.ID: sp.readsOfChild(waiting.ID), "OLD": sp.readsOfChild("OLD")}
	eventually(t, "a read of the working child", func() bool { return sp.readsOfChild(working.ID) > 0 })
	time.Sleep(100 * time.Millisecond)
	for id, n := range before {
		if got := sp.readsOfChild(id); got != n {
			t.Fatalf("the new process read the child %s that does not work %d times", id, got-n)
		}
	}
}

// mustRun returns the run state of id.
func mustRun(t *testing.T, a *Agent, id string) *runState {
	t.Helper()
	rs, err := a.k.lookup(id)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

// TestTreeStopTriesAFailedCancelAgain proves that a stop of a tree tries a
// cancel that failed again, while nothing else reads the child.
func TestTreeStopTriesAFailedCancelAgain(t *testing.T) {
	ctx := testCtx(t)
	sp := newReadCounter()
	a := newTestAgent(t, newScript(endTurn("parent done")).Model(), WithSpawner(sp), withFollow(time.Hour, time.Hour))
	parent, err := a.Run(ctx, "parent", WithRunBudget(Budget{MaxTokens: 100}))
	mustState(t, parent, err, StateCompleted)
	child, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("spends"))
	if err != nil {
		t.Fatal(err)
	}
	sp.mu.Lock()
	sp.failCancel[child.ID] = 3
	sp.mu.Unlock()
	sp.report(child.ID, StateRunning, tokens(200), tokens(200))
	if _, err := a.Get(ctx, child.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the child is canceled after three failed cancels", func() bool { return sp.wasCanceled(child.ID) })
	if n := sp.cancelsOf(child.ID); n != 4 {
		t.Fatalf("%d cancels, want 4", n)
	}
}

// pausedPlanner holds Plan until the test lets it go, so the tree can run
// out between the check of a start and the record of the child.
type pausedPlanner struct {
	*treeSpawner
	once    sync.Once
	reached chan struct{}
	release chan struct{}
}

func (s *pausedPlanner) Plan(p *Run, req RunRequest) (string, error) {
	if req.Input == "late" {
		s.once.Do(func() { close(s.reached) })
		<-s.release
	}
	return s.treeSpawner.Plan(p, req)
}

// TestTreeStopCancelsAChildThatStartsAfterIt proves that a child whose
// start passed the check before its tree ran out, and that this process
// recorded only after the stop, is canceled when it started.
func TestTreeStopCancelsAChildThatStartsAfterIt(t *testing.T) {
	ctx := testCtx(t)
	sp := &pausedPlanner{treeSpawner: newTreeSpawner(), reached: make(chan struct{}), release: make(chan struct{})}
	a := newTestAgent(t, newScript(endTurn("parent done")).Model(), WithSpawner(sp), withFollow(time.Hour, time.Hour))
	parent, err := a.Run(ctx, "parent", WithRunBudget(Budget{MaxTokens: 100}))
	mustState(t, parent, err, StateCompleted)
	first, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("first"))
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		r   *Run
		err error
	}
	done := make(chan result, 1)
	go func() {
		r, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("late"))
		done <- result{r, err}
	}()
	<-sp.reached
	sp.report(first.ID, StateRunning, tokens(200), tokens(200))
	if _, err := a.Get(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the first child is canceled", func() bool { return sp.wasCanceled(first.ID) })
	close(sp.release)
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	eventually(t, "the late child is canceled", func() bool { return sp.wasCanceled(got.r.ID) })
}

// TestTreeStopCancelsAChildThatALookFinds proves that a child that a look
// after a restart finds in a tree that ran out is canceled.
func TestTreeStopCancelsAChildThatALookFinds(t *testing.T) {
	ctx := testCtx(t)
	l := NewMemLedger()
	// The parent waits for a person, so it neither runs nor ended, and its
	// tree ran out through A. The start of B was open when the process
	// went away.
	for _, e := range []Event{
		MustEvent("P", EvRunQueued, RunQueuedData{Agent: "default", Input: "parent", Budget: &Budget{MaxTokens: 100}}),
		MustEvent("P", EvRunStarted, RunStartedData{}),
		MustEvent("P", EvRunChild, RunChildData{Child: "A", Member: "remote", Metadata: map[string]string{MetaParent: "P"}, Kind: childSpawn}),
		MustEvent("P", EvRunChild, RunChildData{Child: "A", Member: "remote", Started: true}),
		MustEvent("P", EvChildReported, ChildReportedData{Child: "A", State: StateRunning, TreeUsage: tokens(500)}),
		MustEvent("P", EvRunChild, RunChildData{Child: "B", Member: "remote", Metadata: map[string]string{MetaParent: "P"}, Kind: childSpawn}),
		MustEvent("P", EvTurnStarted, TurnData{Turn: 1}),
		MustEvent("P", EvModelRequested, ModelRequestedData{}),
		MustEvent("P", EvModelResponded, ModelRespondedData{Content: []Block{use("c1", "echo", `{}`)}, StopReason: StopToolUse}),
		MustEvent("P", EvApprovalRequested, ApprovalRequestedData{ApprovalID: "ap1", Tool: "echo", CallID: "c1", Input: json.RawMessage(`{}`)}),
	} {
		if _, err := l.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	sp := newTreeSpawner()
	sp.runs["A"] = &Run{ID: "A", State: StateRunning, Usage: tokens(500)}
	sp.runs["B"] = &Run{ID: "B", State: StateRunning}
	a := newTestAgent(t, newScript().Model(), WithSpawner(sp), WithLedger(l), withFollow(time.Hour, time.Hour))
	eventually(t, "the look ends", func() bool { return a.Starting() == 0 })
	if r, _ := a.Get(ctx, "P"); r.State != StateInputRequired {
		t.Fatalf("parent = %s", r.State)
	}
	eventually(t, "the found child is canceled", func() bool { return sp.wasCanceled("B") })
}

// TestTreeStopReachesBelowAFinishedRun proves that a stop of a tree
// cancels an open run under a run that finished, at any depth.
func TestTreeStopReachesBelowAFinishedRun(t *testing.T) {
	ctx := testCtx(t)
	started := make(chan string, 4)
	block := NewTool(ToolSpec{Name: "block", ReadOnly: true}, func(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
		started <- RunInfoFrom(ctx).RunID
		<-ctx.Done()
		return ToolResult{}, ctx.Err()
	})
	spend := spender()
	m := ModelFunc{StreamFn: func(ctx context.Context, req *Request, d func(Delta)) (*Response, error) {
		switch TextOf(req.Messages[0].Content) {
		case "root", "finished":
			return endTurn("done"), nil
		case "waiter":
			return callTools(use("b1", "block", `{}`)), nil
		}
		return spend.Stream(ctx, req, d)
	}}
	a := newTestAgent(t, m, WithTools(echoTool(nil), block))
	root, err := a.Run(ctx, "root", WithRunBudget(Budget{MaxTokens: 100}))
	mustState(t, root, err, StateCompleted)
	// An open child, a finished grandchild under it, and an open run under
	// the grandchild.
	open, err := a.Start(ctx, "waiter", WithParent(root.ID))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	finished, err := a.Run(ctx, "finished", WithParent(open.ID))
	mustState(t, finished, err, StateCompleted)
	deep, err := a.Start(ctx, "waiter", WithParent(finished.ID))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	spent, err := a.Run(ctx, "spend", WithParent(root.ID))
	mustState(t, spent, err, StateFailed)
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	got, err := a.WaitFor(wctx, deep.ID, func(r *Run) bool { return r.State.Terminal() })
	mustState(t, got, err, StateCanceled)
}

// TestCancelReachesBelowAFinishedRunAndLeavesBranches proves that Cancel of
// a run cancels an open run under a child of it that finished, that Cancel
// of a finished run cancels the open runs under it, and that neither
// cancels a branch in another process, which is a run of its own.
func TestCancelReachesBelowAFinishedRunAndLeavesBranches(t *testing.T) {
	ctx := testCtx(t)
	started := make(chan string, 4)
	block := NewTool(ToolSpec{Name: "block", ReadOnly: true}, func(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
		started <- RunInfoFrom(ctx).RunID
		<-ctx.Done()
		return ToolResult{}, ctx.Err()
	})
	m := ModelFunc{StreamFn: func(ctx context.Context, req *Request, _ func(Delta)) (*Response, error) {
		switch TextOf(req.Messages[0].Content) {
		case "waiter":
			return callTools(use("b1", "block", `{}`)), nil
		}
		return endTurn("done"), nil
	}}
	// The branch works in the other process while its source is canceled:
	// its model call waits.
	other := newTestAgent(t, blockingModel(nil))
	sp := &otherSpawner{other: other, branchWait: make(chan struct{})}
	a := newTestAgent(t, m, WithTools(block), WithSpawner(sp))
	root, err := a.Start(ctx, "waiter")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	finished, err := a.Run(ctx, "finished", WithParent(root.ID))
	mustState(t, finished, err, StateCompleted)
	deep, err := a.Start(ctx, "waiter", WithParent(finished.ID))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	branch, err := a.Fork(ctx, root.ID, ForkRequest{Spawn: true})
	if err != nil {
		t.Fatal(err)
	}
	close(sp.branchWait)
	eventually(t, "the branch works", func() bool {
		b, err := other.Get(ctx, branch.ID)
		return a.Starting() == 0 && err == nil && b.State == StateRunning
	})
	if err := a.Cancel(ctx, root.ID); err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	got, err := a.WaitFor(wctx, deep.ID, func(r *Run) bool { return r.State.Terminal() })
	mustState(t, got, err, StateCanceled)
	// The cancels of other processes run in the background, so the branch
	// has the time to see one, and it must not.
	for range 15 {
		if b, err := other.Get(ctx, branch.ID); err != nil || b.State != StateRunning {
			t.Fatalf("the branch of a canceled source = %+v, %v", b, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The branch ends here, so the other process closes at once.
	if err := other.Cancel(ctx, branch.ID); err != nil {
		t.Fatal(err)
	}

	// Cancel of a finished run cancels the open runs under it.
	done, err := a.Run(ctx, "done")
	mustState(t, done, err, StateCompleted)
	under, err := a.Start(ctx, "waiter", WithParent(done.ID))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := a.Cancel(ctx, done.ID); err != nil {
		t.Fatal(err)
	}
	got, err = a.WaitFor(wctx, under.ID, func(r *Run) bool { return r.State.Terminal() })
	mustState(t, got, err, StateCanceled)
	if r, _ := a.Get(ctx, done.ID); r.State != StateCompleted {
		t.Fatalf("the finished run is %s after Cancel", r.State)
	}
}

// TestRemoteChildCountsAtLeastItsOwnUsage proves that a child in another
// process counts at least its own usage, whatever its tree says.
func TestRemoteChildCountsAtLeastItsOwnUsage(t *testing.T) {
	ctx := testCtx(t)
	sp := newTreeSpawner()
	a := newTestAgent(t, newScript(endTurn("parent done")).Model(), WithSpawner(sp), withFollow(time.Hour, time.Hour))
	parent, err := a.Run(ctx, "parent")
	mustState(t, parent, err, StateCompleted)
	child, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("child"))
	if err != nil {
		t.Fatal(err)
	}
	sp.report(child.ID, StateRunning, tokens(300), tokens(20))
	if _, err := a.Get(ctx, child.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := a.Get(ctx, parent.ID)
	if got.TreeUsage == nil || got.TreeUsage.InputTokens != 10+300 {
		t.Fatalf("the tree of the parent = %+v", got.TreeUsage)
	}
}

// TestTreeRoomOfZeroIsNoCap proves that a room that is not above zero
// gives no cap, because a cap of zero is no limit.
func TestTreeRoomOfZeroIsNoCap(t *testing.T) {
	for _, room := range []treeRoom{
		{cost: allowance{set: true, left: 0}},
		{cost: allowance{set: true, left: -0.25}},
		{tokens: allowance{set: true, left: 0.5}},
		{calls: allowance{set: true, left: 0}},
	} {
		if b, ok := room.budget(); ok {
			t.Fatalf("room %+v gives the cap %+v", room, b)
		}
	}
	if b, ok := (treeRoom{cost: allowance{set: true, left: 0.5}, tokens: allowance{set: true, left: 7}}).budget(); !ok || b.MaxCostUSD != 0.5 || b.MaxTokens != 7 {
		t.Fatalf("cap = %+v, %v", b, ok)
	}
}
