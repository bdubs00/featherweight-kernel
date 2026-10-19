package fw

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// withGiveUp shortens followGiveUp, for a test.
func withGiveUp(d time.Duration) Option { return func(o *options) { o.giveUp = d } }

// lastReport returns the latest fw.child.reported of a child on its parent.
func lastReport(t *testing.T, l Ledger, parent, child string) (ChildReportedData, bool) {
	t.Helper()
	var last ChildReportedData
	found := false
	for _, e := range ledgerEvents(t, l, parent) {
		var d ChildReportedData
		if e.Type == EvChildReported && e.Decode(&d) == nil && d.Child == child {
			last, found = d, true
		}
	}
	return last, found
}

// TestLostChildLeavesTheTree proves that a child that its process lost
// counts in the tree of its parent with its last usage alone, neither open
// nor working, that the parent records it as lost, that a spent tree does
// not cancel it, and that a restart does not read it again.
func TestLostChildLeavesTheTree(t *testing.T) {
	ctx := testCtx(t)
	l := NewMemLedger()
	sp := newReadCounter()
	a := newTestAgent(t, newScript(endTurn("parent done")).Model(), WithSpawner(sp), WithLedger(l), withFollow(10*time.Millisecond, time.Hour))
	parent, err := a.Run(ctx, "parent", WithRunBudget(Budget{MaxTokens: 100}))
	mustState(t, parent, err, StateCompleted)
	gone, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("goes away"))
	if err != nil {
		t.Fatal(err)
	}
	sp.report(gone.ID, StateRunning, tokens(50), tokens(50))
	if _, err := a.Get(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	sp.treeSpawner.mu.Lock()
	delete(sp.runs, gone.ID)
	sp.treeSpawner.mu.Unlock()
	eventually(t, "the child is lost", func() bool {
		d, ok := lastReport(t, l, parent.ID, gone.ID)
		return ok && d.Lost
	})
	r, _ := a.Get(ctx, parent.ID)
	if r.TreeOpen != 0 || r.TreeWorking != 0 || r.TreeUsage == nil || r.TreeUsage.InputTokens != 10+50 || a.Following() != 0 {
		t.Fatalf("the tree of the parent of a lost child: open %d, working %d, usage %+v, following %d", r.TreeOpen, r.TreeWorking, r.TreeUsage, a.Following())
	}
	// Another child spends the rest of the tree, and the stop cancels it,
	// and never the lost child, however often it reads the tree.
	spender, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("spends"))
	if err != nil {
		t.Fatal(err)
	}
	sp.report(spender.ID, StateRunning, tokens(100), tokens(100))
	eventually(t, "the spender is canceled", func() bool { return sp.wasCanceled(spender.ID) })
	for range 5 {
		sp.report(spender.ID, StateRunning, tokens(100), tokens(100))
		if _, err := a.Get(ctx, spender.ID); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := sp.cancelsOf(gone.ID); n != 0 {
		t.Fatalf("the spent tree canceled the lost child %d times", n)
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	a.Close(cctx)
	cancel()

	b := newTestAgent(t, newScript().Model(), WithSpawner(sp), WithLedger(l), withFollow(10*time.Millisecond, time.Hour))
	before := sp.readsOfChild(gone.ID)
	time.Sleep(100 * time.Millisecond)
	if n := sp.readsOfChild(gone.ID) - before; n != 0 {
		t.Fatalf("a restart read the lost child %d times", n)
	}
	r, _ = b.Get(ctx, parent.ID)
	if r.TreeOpen != 0 || r.TreeUsage == nil || r.TreeUsage.InputTokens < 10+50 {
		t.Fatalf("the tree of the parent after a restart: open %d, usage %+v", r.TreeOpen, r.TreeUsage)
	}
}

// TestUnansweringChildIsGivenUp proves that a child that does not answer
// for followGiveUp counts as lost: it leaves the tree of its parent as
// open and working, and the parent records it.
func TestUnansweringChildIsGivenUp(t *testing.T) {
	ctx := testCtx(t)
	l := NewMemLedger()
	sp := newReadCounter()
	a := newTestAgent(t, newScript(endTurn("parent done")).Model(), WithSpawner(sp), WithLedger(l),
		withFollow(10*time.Millisecond, time.Hour), withGiveUp(100*time.Millisecond))
	parent, err := a.Run(ctx, "parent")
	mustState(t, parent, err, StateCompleted)
	child, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("background"))
	if err != nil {
		t.Fatal(err)
	}
	sp.treeSpawner.mu.Lock()
	sp.getErr = errors.New("the router does not answer")
	sp.treeSpawner.mu.Unlock()
	eventually(t, "the child is given up", func() bool {
		d, ok := lastReport(t, l, parent.ID, child.ID)
		return ok && d.Lost
	})
	r, _ := a.Get(ctx, parent.ID)
	if r.TreeOpen != 0 || r.TreeWorking != 0 || a.Following() != 0 {
		t.Fatalf("a child given up counts in the tree: open %d, working %d, following %d", r.TreeOpen, r.TreeWorking, a.Following())
	}
	before := sp.readsOfChild(child.ID)
	time.Sleep(100 * time.Millisecond)
	if n := sp.readsOfChild(child.ID) - before; n != 0 {
		t.Fatalf("a child given up was read %d times", n)
	}
}

// syncBuffer is a buffer that a logger and a test share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestNoSpawnerWithChildrenInOtherProcesses proves that a process whose
// ledger holds children in other processes, and that has no spawner, says
// so once, counts the children as neither working nor open, answers for
// them with an error, and cancels and stops their parent without a crash.
func TestNoSpawnerWithChildrenInOtherProcesses(t *testing.T) {
	ctx := testCtx(t)
	l := NewMemLedger()
	for _, e := range []Event{
		MustEvent("P", EvRunQueued, RunQueuedData{Agent: "default", Input: "parent", Budget: &Budget{MaxTokens: 100}}),
		MustEvent("P", EvRunStarted, RunStartedData{}),
		MustEvent("P", EvRunChild, RunChildData{Child: "C", Member: "remote", Metadata: map[string]string{MetaParent: "P"}, Kind: childSpawn}),
		MustEvent("P", EvRunChild, RunChildData{Child: "C", Member: "remote", Started: true}),
		MustEvent("P", EvChildReported, ChildReportedData{Child: "C", State: StateRunning, TreeUsage: tokens(500), Working: 1}),
		MustEvent("P", EvRunFinished, RunFinishedData{Outcome: OutcomeCompleted}),
	} {
		if _, err := l.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	var logs syncBuffer
	a := newTestAgent(t, newScript().Model(), WithLedger(l), WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	if a.Following() != 0 {
		t.Fatalf("Following = %d without a spawner", a.Following())
	}
	r, err := a.Get(ctx, "P")
	if err != nil || r.TreeOpen != 0 || r.TreeWorking != 0 || r.TreeUsage == nil || r.TreeUsage.InputTokens != 500 {
		t.Fatalf("parent = %+v, %v", r, err)
	}
	if _, err := a.Get(ctx, "C"); !errors.Is(err, ErrRunNotFound) || !strings.Contains(err.Error(), "no spawner") {
		t.Fatalf("Get of the child = %v", err)
	}
	if _, err := a.WaitFor(ctx, "C", func(*Run) bool { return true }); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("WaitFor of the child = %v", err)
	}
	if kids, err := a.Children(ctx, "P"); err != nil || len(kids) != 0 {
		t.Fatalf("Children = %v, %v", kids, err)
	}
	if err := a.Cancel(ctx, "P"); err != nil {
		t.Fatal(err)
	}
	rs := mustRun(t, a, "P")
	a.stopTree(overrun{limit: "maxTokens", value: 500, max: 100, owner: rs, tree: true})
	time.Sleep(50 * time.Millisecond)
	if n := strings.Count(logs.String(), "has no spawner"); n != 1 {
		t.Fatalf("the warning came %d times:\n%s", n, logs.String())
	}
}

// blockedCanceler is a spawner whose Cancel waits until its context ends,
// as a child Task that the router cannot reach.
type blockedCanceler struct {
	*treeSpawner
}

func (s *blockedCanceler) Cancel(ctx context.Context, _, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestCancelDoesNotWaitForOtherProcesses proves that Cancel of a run, and
// the stop of a tree in the driver of a run, wait for no other process,
// and that a shutdown ends the cancels in flight.
func TestCancelDoesNotWaitForOtherProcesses(t *testing.T) {
	ctx := testCtx(t)
	sp := &blockedCanceler{treeSpawner: newTreeSpawner()}
	second, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	m := ModelFunc{StreamFn: func(_ context.Context, req *Request, _ func(Delta)) (*Response, error) {
		if TextOf(req.Messages[0].Content) == "root" && len(req.Messages) > 1 {
			once.Do(func() { close(second) })
			<-release
		}
		return callTools(use("e1", "echo", `{"text":"`+TextOf(req.Messages[len(req.Messages)-1].Content)+`"}`)), nil
	}}
	a := newTestAgent(t, m, WithSpawner(sp), WithTools(echoTool(nil)), withFollow(time.Hour, time.Hour))
	parent, err := a.Start(ctx, "parent", WithRunBudget(Budget{MaxTurns: 1}))
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("child")); err != nil {
			t.Fatal(err)
		}
	}
	began := time.Now()
	if err := a.Cancel(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(began); d > time.Second {
		t.Fatalf("Cancel waited %s for the children in other processes", d)
	}
	// A run that spends its tree stops in its driver without waiting: its
	// second model call waits until the child started, and then the third
	// check finds the tree spent.
	root, err := a.Start(ctx, "root", WithRunBudget(Budget{MaxTokens: 20}))
	if err != nil {
		t.Fatal(err)
	}
	<-second
	if _, err := a.SpawnChild(ctx, root.ID, NewRunRequest("child")); err != nil {
		t.Fatal(err)
	}
	close(release)
	began = time.Now()
	r, err := a.Wait(ctx, root.ID)
	mustState(t, r, err, StateFailed)
	if d := time.Since(began); d > 2*time.Second {
		t.Fatalf("the driver waited %s for the children in other processes", d)
	}
	began = time.Now()
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	a.Close(cctx)
	if d := time.Since(began); d > 2*time.Second {
		t.Fatalf("the shutdown waited %s for the cancels in flight", d)
	}
}
