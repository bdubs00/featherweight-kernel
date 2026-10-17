package fw

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// otherSpawner runs the children in another agent, as a process of its own
// would, and knows them by the member name "other".
type otherSpawner struct {
	other *Agent

	mu   sync.Mutex
	reqs []RunRequest
}

func (s *otherSpawner) Spawn(ctx context.Context, _ *Run, req RunRequest) (*Run, string, error) {
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	s.mu.Unlock()
	// The other process starts the run as its native API does for a
	// trusted caller: with the principal of the request.
	r, err := s.other.Start(ctx, req.Input, WithRequest(req))
	if err != nil {
		return nil, "", err
	}
	return r, "other", nil
}

func (s *otherSpawner) Get(ctx context.Context, _, id string) (*Run, error) {
	return s.other.Get(ctx, id)
}

func (s *otherSpawner) WaitFor(ctx context.Context, _, id string, done func(*Run) bool) (*Run, error) {
	return s.other.WaitFor(ctx, id, done)
}

func (s *otherSpawner) Decide(ctx context.Context, _, id string, d ApprovalDecision) error {
	return s.other.Decide(ctx, id, d)
}

func (s *otherSpawner) Cancel(ctx context.Context, _, id string) error {
	return s.other.Cancel(ctx, id)
}

func (s *otherSpawner) requests() []RunRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RunRequest(nil), s.reqs...)
}

func TestSpawnChildThroughASpawner(t *testing.T) {
	ctx := testCtx(t)
	ada := &Principal{Subject: "ada", Kind: KindUser}
	other := newTestAgent(t, newScript(endTurn("child done")).Model())
	sp := &otherSpawner{other: other}
	a := newTestAgent(t, newScript(endTurn("parent done")).Model(), WithSpawner(sp))
	if a.Spawner() != sp {
		t.Fatal("WithSpawner did not set the spawner")
	}
	parent, err := a.Run(WithPrincipal(ctx, ada), "parent work")
	mustState(t, parent, err, StateCompleted)

	child, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("do x", WithRunBudget(Budget{MaxTurns: 3})))
	if err != nil {
		t.Fatalf("SpawnChild: %v", err)
	}
	if child.Member != "other" || child.Parent != parent.ID || child.Depth != 1 {
		t.Fatalf("child = %+v", child)
	}
	reqs := sp.requests()
	if len(reqs) != 1 || reqs[0].Principal != "ada" || reqs[0].PrincipalKind != KindUser || reqs[0].Metadata[MetaParent] != parent.ID ||
		reqs[0].Origin != OriginTool || reqs[0].Parent != "" || reqs[0].Budget == nil || reqs[0].Budget.MaxTurns != 3 {
		t.Fatalf("request = %+v", reqs)
	}
	// The run methods reach the child in the other process.
	got, err := a.Get(ctx, child.ID)
	if err != nil || got.Member != "other" || got.Parent != parent.ID || got.Principal != "ada" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	done, err := a.Wait(ctx, child.ID)
	if err != nil || done.State != StateCompleted || done.Output != "child done" || done.Member != "other" {
		t.Fatalf("Wait = %+v, %v", done, err)
	}
	kids, err := a.Children(ctx, parent.ID)
	if err != nil || len(kids) != 1 || kids[0].ID != child.ID || kids[0].Member != "other" {
		t.Fatalf("Children = %+v, %v", kids, err)
	}
	if err := a.Decide(ctx, child.ID, ApprovalDecision{ApprovalID: "ap1", Decision: "allow"}); !errors.Is(err, ErrNoPendingApproval) {
		t.Fatalf("Decide on a finished remote child = %v", err)
	}
	// The ledger of the parent names the child and its process.
	var d RunChildData
	firstOf(t, ledgerEvents(t, a.Ledger(), parent.ID), EvRunChild, &d)
	if d.Child != child.ID || d.Member != "other" {
		t.Fatalf("child event = %+v", d)
	}
	// An unknown run is still unknown.
	if _, err := a.Get(ctx, "01NOPE"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("Get unknown = %v", err)
	}
}

func TestSpawnChildLimitsAndCancel(t *testing.T) {
	ctx := testCtx(t)
	other := newTestAgent(t, blockingModel(nil))
	sp := &otherSpawner{other: other}
	cfg := DefaultConfig()
	cfg.Spawn.MaxChildren = 1
	cfg.Spawn.MaxDepth = 1
	a := newTestAgent(t, blockingModel(nil), WithSpawner(sp), WithConfig(cfg))
	parent, err := a.Start(ctx, "parent work")
	if err != nil {
		t.Fatal(err)
	}
	child, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("child work"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("one more")); !errors.Is(err, ErrSpawnLimit) {
		t.Fatalf("second child = %v", err)
	}
	// A local child at depth 1 cannot start a child at depth 2.
	local, err := a.Start(ctx, "local child", WithParent(parent.ID))
	if err == nil {
		if _, err := a.SpawnChild(ctx, local.ID, NewRunRequest("grandchild")); !errors.Is(err, ErrSpawnLimit) {
			t.Fatalf("grandchild = %v", err)
		}
	}
	// A canceled parent cancels its remote children.
	if err := a.Cancel(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	got, err := a.Wait(ctx, child.ID)
	if err != nil || got.State != StateCanceled {
		t.Fatalf("child after the parent was canceled = %+v, %v", got, err)
	}
}

func TestRemoteChildrenSurviveARestart(t *testing.T) {
	ctx := testCtx(t)
	l := NewMemLedger()
	other := newTestAgent(t, newScript(endTurn("child done")).Model())
	sp := &otherSpawner{other: other}
	cfg := DefaultConfig()
	cfg.Spawn.MaxChildren = 1
	a := newTestAgent(t, newScript(endTurn("parent done")).Model(), WithSpawner(sp), WithLedger(l), WithConfig(cfg))
	parent, err := a.Run(ctx, "parent work")
	mustState(t, parent, err, StateCompleted)
	child, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("do x"))
	if err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	a.Close(cctx)
	cancel()

	// The next process reads the ledger and finds the child again.
	b := newTestAgent(t, newScript().Model(), WithSpawner(sp), WithLedger(l), WithConfig(cfg))
	got, err := b.Get(ctx, child.ID)
	if err != nil || got.Member != "other" || got.Parent != parent.ID || got.Depth != 1 {
		t.Fatalf("Get after the restart = %+v, %v", got, err)
	}
	kids, err := b.Children(ctx, parent.ID)
	if err != nil || len(kids) != 1 || kids[0].ID != child.ID {
		t.Fatalf("Children after the restart = %+v, %v", kids, err)
	}
	if _, err := b.SpawnChild(ctx, parent.ID, NewRunRequest("one more")); !errors.Is(err, ErrSpawnLimit) {
		t.Fatalf("the restart forgot the count of children: %v", err)
	}
}

func TestSpawnChildWithoutASpawnerRunsHere(t *testing.T) {
	ctx := testCtx(t)
	a := newTestAgent(t, newScript(endTurn("parent done"), endTurn("child done")).Model())
	parent, err := a.Run(ctx, "parent work")
	mustState(t, parent, err, StateCompleted)
	child, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("do x"))
	if err != nil || child.Parent != parent.ID || child.Member != "" || child.Depth != 1 {
		t.Fatalf("SpawnChild = %+v, %v", child, err)
	}
	if done, err := a.Wait(ctx, child.ID); err != nil || done.Output != "child done" {
		t.Fatalf("Wait = %+v, %v", done, err)
	}
}
