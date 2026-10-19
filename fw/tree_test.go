package fw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// withFollow shortens the reads of the children in other processes and
// their records, for a test.
func withFollow(every, record time.Duration) Option {
	return func(o *options) { o.followEvery, o.recordEvery = every, record }
}

// treeSpawner plays the children in other processes. Each child is a run
// that the test reports by hand: its state, its own usage, and the usage
// of its tree, as the process of the child would answer a read.
type treeSpawner struct {
	mu       sync.Mutex
	reqs     []RunRequest
	runs     map[string]*Run
	canceled []string
	getErr   error
}

func newTreeSpawner() *treeSpawner { return &treeSpawner{runs: map[string]*Run{}} }

func (s *treeSpawner) Plan(*Run, RunRequest) (string, error) { return "remote", nil }

func (s *treeSpawner) Spawn(_ context.Context, _ *Run, req RunRequest) (*Run, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, req)
	r := &Run{ID: req.ID, Agent: "default", Input: req.Input, State: StateRunning, Metadata: req.Metadata}
	s.runs[req.ID] = r
	return cloneRun(r), "remote", nil
}

func (s *treeSpawner) Find(ctx context.Context, member, id string) (*Run, error) {
	return s.Get(ctx, member, id)
}

func (s *treeSpawner) Abandon(context.Context, string) error { return nil }

func (s *treeSpawner) Get(_ context.Context, _, id string) (*Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return nil, s.getErr
	}
	r := s.runs[id]
	if r == nil {
		return nil, fmt.Errorf("%w: %s", ErrRunNotFound, id)
	}
	return cloneRun(r), nil
}

func (s *treeSpawner) WaitFor(ctx context.Context, member, id string, done func(*Run) bool) (*Run, error) {
	for {
		r, err := s.Get(ctx, member, id)
		if err != nil {
			return nil, err
		}
		if done(r) {
			return r, nil
		}
		select {
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
			return r, ctx.Err()
		}
	}
}

func (s *treeSpawner) Decide(context.Context, string, string, ApprovalDecision) error {
	return ErrNoPendingApproval
}

func (s *treeSpawner) Cancel(_ context.Context, _, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.canceled = append(s.canceled, id)
	if r := s.runs[id]; r != nil && !r.State.Terminal() {
		r.State = StateCanceled
	}
	return nil
}

// report sets what a read of the child answers.
func (s *treeSpawner) report(id string, state RunState, own, tree RunUsage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.runs[id]
	r.State, r.Usage = state, own
	r.TreeUsage = &tree
}

func (s *treeSpawner) requests() []RunRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RunRequest(nil), s.reqs...)
}

func (s *treeSpawner) wasCanceled(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.canceled {
		if c == id {
			return true
		}
	}
	return false
}

// tokens returns a usage of n input tokens.
func tokens(n int) RunUsage { return RunUsage{Usage: Usage{InputTokens: n}} }

// spender is a model for a tree: the root answers its first call with the
// tool calls of rootCalls and then ends, and every other run calls echo
// again and again with a new input, so it spends until a limit stops it.
// Each response uses 15 tokens.
func spender(rootCalls ...Block) Model {
	var n atomic.Int64
	return ModelFunc{StreamFn: func(_ context.Context, req *Request, _ func(Delta)) (*Response, error) {
		if TextOf(req.Messages[0].Content) == "root" {
			if len(req.Messages) == 1 && len(rootCalls) > 0 {
				return callTools(rootCalls...), nil
			}
			return endTurn("root done"), nil
		}
		i := n.Add(1)
		return callTools(use(fmt.Sprintf("e%d", i), "echo", fmt.Sprintf(`{"text":"%d"}`, i))), nil
	}}
}

// fanTool starts n children of its run at once and waits for all of them.
func fanTool(n int, ids *[]string) Tool {
	spec := ToolSpec{Name: "fan", InputSchema: json.RawMessage(`{"type":"object"}`)}
	return NewTool(spec, func(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
		a, parent := AgentFrom(ctx), RunInfoFrom(ctx).RunID
		for i := range n {
			r, err := a.Start(ctx, fmt.Sprintf("child %d", i), WithParent(parent))
			if err != nil {
				return ErrorResult(err.Error()), nil
			}
			*ids = append(*ids, r.ID)
		}
		for _, id := range *ids {
			if _, err := a.Wait(ctx, id); err != nil {
				return ErrorResult(err.Error()), nil
			}
		}
		return TextResult("fanned"), nil
	})
}

// usedTokens sums the tokens of the runs, each by its own usage.
func usedTokens(t *testing.T, a *Agent, ids ...string) int {
	t.Helper()
	total := 0
	for _, id := range ids {
		r, err := a.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		total += r.Usage.InputTokens + r.Usage.OutputTokens
	}
	return total
}

// TestTreeBudgetHoldsForParallelSiblings proves that children that work at
// the same time share the maxTokens of their root: together they stop at
// the limit, past it by at most one model call of each run that works,
// instead of each spending the whole limit of the root.
func TestTreeBudgetHoldsForParallelSiblings(t *testing.T) {
	ctx := testCtx(t)
	var ids []string
	a := newTestAgent(t, spender(use("f1", "fan", `{}`)), WithTools(echoTool(nil), fanTool(4, &ids)))
	root, err := a.Run(ctx, "root", WithRunBudget(Budget{MaxTokens: 100}))
	mustState(t, root, err, StateFailed)
	total := usedTokens(t, a, append([]string{root.ID}, ids...)...)
	t.Logf("the tree used %d tokens with a limit of 100", total)
	// The bound: the limit, plus one call of 15 tokens of each of the four
	// children that work at the same time.
	if total > 100+4*15 {
		t.Fatalf("the tree used %d tokens with a limit of 100", total)
	}
	if !strings.Contains(root.Error, "maxTokens of the run tree") {
		t.Fatalf("root error = %q", root.Error)
	}
	var d BudgetExceededData
	firstOf(t, ledgerEvents(t, a.Ledger(), root.ID), EvBudgetExceeded, &d)
	if d.Limit != "maxTokens" || d.Scope != ScopeTree || d.Run != root.ID || d.Max != 100 || d.Value < 100 {
		t.Fatalf("budget event = %+v", d)
	}
	got, err := a.Get(ctx, root.ID)
	if err != nil || got.TreeUsage == nil || got.TreeUsage.InputTokens+got.TreeUsage.OutputTokens != total || got.TreeOpen != 0 {
		t.Fatalf("root = %+v, %v; want a tree of %d tokens", got, err, total)
	}
	// Usage stays the usage of the run alone.
	if got.Usage.InputTokens+got.Usage.OutputTokens != 15 {
		t.Fatalf("root usage = %+v", got.Usage)
	}
	for _, id := range ids {
		c, _ := a.Get(ctx, id)
		if c.State != StateFailed && c.State != StateCanceled {
			t.Fatalf("child %s = %s", id, c.State)
		}
	}
}

// TestTreeBudgetCountsAGrandchild proves that the usage of a grandchild
// counts against the root, even after the root and the child finished.
func TestTreeBudgetCountsAGrandchild(t *testing.T) {
	ctx := testCtx(t)
	spend := spender()
	m := ModelFunc{StreamFn: func(ctx context.Context, req *Request, d func(Delta)) (*Response, error) {
		switch TextOf(req.Messages[0].Content) {
		case "root", "child":
			return endTurn("done"), nil
		}
		return spend.Stream(ctx, req, d)
	}}
	a := newTestAgent(t, m, WithTools(echoTool(nil)))
	root, err := a.Run(ctx, "root", WithRunBudget(Budget{MaxTokens: 60}))
	mustState(t, root, err, StateCompleted)
	child, err := a.Run(ctx, "child", WithParent(root.ID))
	mustState(t, child, err, StateCompleted)
	grand, err := a.Run(ctx, "grandchild", WithParent(child.ID))
	mustState(t, grand, err, StateFailed)
	// The root, the child, and the grandchild used 15 + 15 + 30 tokens:
	// the grandchild stopped when the tree of the root reached 60.
	if grand.Usage.InputTokens+grand.Usage.OutputTokens != 30 {
		t.Fatalf("grandchild used %+v", grand.Usage)
	}
	if !strings.Contains(grand.Error, "maxTokens of the run tree of run "+root.ID) {
		t.Fatalf("grandchild error = %q", grand.Error)
	}
	got, err := a.Get(ctx, root.ID)
	if err != nil || got.TreeUsage == nil || got.TreeUsage.InputTokens+got.TreeUsage.OutputTokens != 60 {
		t.Fatalf("root = %+v, %v", got, err)
	}
	// The tree has nothing left, so no child starts in it any more.
	if _, err := a.Start(ctx, "child", WithParent(root.ID)); !errors.Is(err, ErrTreeBudget) || !strings.Contains(err.Error(), "maxTokens") {
		t.Fatalf("a child of a spent tree = %v", err)
	}
}

// TestTreeBudgetCancelsOpenChildren proves that a tree that used up its
// limit cancels its children that are open, here one that waits in a long
// tool call and would never reach a check of its own.
func TestTreeBudgetCancelsOpenChildren(t *testing.T) {
	ctx := testCtx(t)
	started := make(chan struct{}, 1)
	block := NewTool(ToolSpec{Name: "block", ReadOnly: true}, func(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
		started <- struct{}{}
		<-ctx.Done()
		return ToolResult{}, ctx.Err()
	})
	spend := spender()
	m := ModelFunc{StreamFn: func(ctx context.Context, req *Request, d func(Delta)) (*Response, error) {
		switch TextOf(req.Messages[0].Content) {
		case "root":
			return endTurn("done"), nil
		case "waiter":
			return callTools(use("b1", "block", `{}`)), nil
		}
		return spend.Stream(ctx, req, d)
	}}
	a := newTestAgent(t, m, WithTools(echoTool(nil), block))
	root, err := a.Run(ctx, "root", WithRunBudget(Budget{MaxTokens: 40}))
	mustState(t, root, err, StateCompleted)
	waiter, err := a.Start(ctx, "waiter", WithParent(root.ID))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	spent, err := a.Run(ctx, "spend", WithParent(root.ID))
	mustState(t, spent, err, StateFailed)
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	got, err := a.WaitFor(wctx, waiter.ID, func(r *Run) bool { return r.State.Terminal() })
	mustState(t, got, err, StateCanceled)
	if !strings.Contains(got.Error, "maxTokens") || !strings.Contains(got.Error, root.ID) {
		t.Fatalf("the canceled child says %q", got.Error)
	}
}

// TestTreeBudgetCapsARemoteChild proves that a child in another process
// gets what the tree has left as its cap, that the parent counts what the
// child reports of its whole tree, and that the parent cancels its open
// children in other processes when the tree runs out.
func TestTreeBudgetCapsARemoteChild(t *testing.T) {
	ctx := testCtx(t)
	sp := newTreeSpawner()
	a := newTestAgent(t, newScript(endTurn("parent done")).Model(), WithSpawner(sp))
	parent, err := a.Run(ctx, "parent", WithRunBudget(Budget{MaxTokens: 1000, MaxToolCalls: 50}))
	mustState(t, parent, err, StateCompleted)

	first, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("first"))
	if err != nil {
		t.Fatal(err)
	}
	if b := sp.requests()[0].Budget; b == nil || b.MaxTokens != 985 || b.MaxToolCalls != 50 {
		t.Fatalf("the cap of the first child = %+v", b)
	}
	// The child reports 100 tokens of its own and 500 for its tree, with
	// its children in its process. The parent counts the tree.
	sp.report(first.ID, StateRunning, tokens(100), RunUsage{Usage: Usage{InputTokens: 500}, ToolCalls: 7})
	if _, err := a.Get(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	got, err := a.Get(ctx, parent.ID)
	if err != nil || got.TreeUsage == nil || got.TreeUsage.InputTokens+got.TreeUsage.OutputTokens != 515 || got.TreeUsage.ToolCalls != 7 {
		t.Fatalf("parent = %+v, %v", got.TreeUsage, err)
	}
	second, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("second"))
	if err != nil {
		t.Fatal(err)
	}
	if b := sp.requests()[1].Budget; b == nil || b.MaxTokens != 485 || b.MaxToolCalls != 43 {
		t.Fatalf("the cap of the second child = %+v", b)
	}
	// The tree of the first child grows past what the parent has: the
	// parent cancels both children and starts no third one.
	sp.report(first.ID, StateRunning, tokens(100), tokens(990))
	if _, err := a.Get(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the open children are canceled", func() bool { return sp.wasCanceled(first.ID) && sp.wasCanceled(second.ID) })
	if _, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("third")); !errors.Is(err, ErrTreeBudget) {
		t.Fatalf("a third child = %v", err)
	}
}

// TestRemoteChildIsReadWithoutAWait proves that the kernel reads a child
// in another process that nobody waits for, so its spend counts, and that
// it records the end of the child on the parent once.
func TestRemoteChildIsReadWithoutAWait(t *testing.T) {
	ctx := testCtx(t)
	sp := newTreeSpawner()
	a := newTestAgent(t, newScript(endTurn("parent done")).Model(), WithSpawner(sp), withFollow(10*time.Millisecond, time.Hour))
	parent, err := a.Run(ctx, "parent", WithRunBudget(Budget{MaxTokens: 1000}))
	mustState(t, parent, err, StateCompleted)
	child, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("background"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Following() != 1 {
		t.Fatalf("Following = %d while the child works", a.Following())
	}
	sp.report(child.ID, StateCompleted, tokens(40), tokens(70))
	eventually(t, "the parent counts the child", func() bool {
		r, _ := a.Get(ctx, parent.ID)
		return r.TreeUsage != nil && r.TreeUsage.InputTokens == 10+70
	})
	if a.Following() != 0 {
		t.Fatalf("Following = %d after the child ended", a.Following())
	}
	time.Sleep(50 * time.Millisecond)
	if n := countType(ledgerEvents(t, a.Ledger(), parent.ID), EvChildReported); n != 1 {
		t.Fatalf("the parent recorded %d reports of the child", n)
	}
}

// TestTreeSurvivesARestartOfTheParent proves that a new process rebuilds
// the tree of a parent from its ledger: the usage of its children here,
// the last report of its child in another process, and the count of its
// children against spawn.maxChildren.
func TestTreeSurvivesARestartOfTheParent(t *testing.T) {
	ctx := testCtx(t)
	root := t.TempDir()
	sp := newTreeSpawner()
	cfg := DefaultConfig()
	cfg.Spawn.MaxChildren = 2
	m := ModelFunc{StreamFn: func(context.Context, *Request, func(Delta)) (*Response, error) { return endTurn("done"), nil }}
	a := newTestAgent(t, m, WithSpawner(sp), WithConfig(cfg), WithLedger(openFileLedgerAt(t, root)))
	parent, err := a.Run(ctx, "parent", WithRunBudget(Budget{MaxTokens: 1000}))
	mustState(t, parent, err, StateCompleted)
	local, err := a.Run(ctx, "local child", WithParent(parent.ID))
	mustState(t, local, err, StateCompleted)
	remote, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("remote child"))
	if err != nil {
		t.Fatal(err)
	}
	sp.report(remote.ID, StateCompleted, tokens(100), tokens(300))
	if _, err := a.Get(ctx, remote.ID); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	a.Close(cctx)
	cancel()

	// The process of the child does not answer the new process, so only the
	// ledger of the parent tells what the child used.
	sp2 := newTreeSpawner()
	sp2.getErr = errors.New("the router does not answer")
	b := newTestAgent(t, m, WithSpawner(sp2), WithConfig(cfg), WithLedger(openFileLedgerAt(t, root)))
	got, err := b.Get(ctx, parent.ID)
	if err != nil || got.TreeUsage == nil || got.TreeUsage.InputTokens+got.TreeUsage.OutputTokens != 15+15+300 {
		t.Fatalf("parent after the restart = %+v, %v", got, err)
	}
	if _, err := b.Start(ctx, "a third child", WithParent(parent.ID)); !errors.Is(err, ErrSpawnLimit) || !strings.Contains(err.Error(), "spawn.maxChildren") {
		t.Fatalf("a third child after the restart = %v", err)
	}
	if _, err := b.SpawnChild(ctx, parent.ID, NewRunRequest("a third remote child")); !errors.Is(err, ErrSpawnLimit) {
		t.Fatalf("a third remote child after the restart = %v", err)
	}
}

// TestTreeSizeHoldsAcrossLocalAndRemoteChildren proves spawn.maxDescendants
// for a tree with children here and in another process: a child in
// another process holds its share, which travels to it, and the runs here
// count one by one at every depth.
func TestTreeSizeHoldsAcrossLocalAndRemoteChildren(t *testing.T) {
	ctx := testCtx(t)
	sp := newTreeSpawner()
	cfg := DefaultConfig()
	cfg.Spawn.MaxDescendants, cfg.Spawn.MaxChildren = 6, 2
	a := newTestAgent(t, newScript(endTurn("root done")).Model(), WithSpawner(sp), WithConfig(cfg))
	root, err := a.Run(ctx, "root")
	mustState(t, root, err, StateCompleted)
	// Six runs and two children of the root: the child in another process
	// takes an equal share of three, itself and two runs under it.
	if _, err := a.SpawnChild(ctx, root.ID, NewRunRequest("remote")); err != nil {
		t.Fatal(err)
	}
	if got := sp.requests()[0].Metadata[MetaMaxDescendants]; got != "2" {
		t.Fatalf("the child in another process can have %q runs under it", got)
	}
	local, err := a.Start(ctx, "local", WithParent(root.ID))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if _, err := a.Start(ctx, fmt.Sprintf("grandchild %d", i), WithParent(local.ID)); err != nil {
			t.Fatalf("grandchild %d: %v", i, err)
		}
	}
	// 3 + 1 + 2 runs: the tree is full at every depth.
	kids, _ := a.Children(ctx, local.ID)
	_, err = a.Start(ctx, "great-grandchild", WithParent(kids[0].ID))
	if !errors.Is(err, ErrSpawnLimit) || !strings.Contains(err.Error(), "spawn.maxDescendants") {
		t.Fatalf("a seventh run = %v", err)
	}
}

// TestTreeSizeHoldsForParallelSiblings proves that children that start at
// the same time, here and in another process, never hold more runs
// together than spawn.maxDescendants.
func TestTreeSizeHoldsForParallelSiblings(t *testing.T) {
	ctx := testCtx(t)
	sp := newTreeSpawner()
	cfg := DefaultConfig()
	cfg.Spawn.MaxDescendants, cfg.Spawn.MaxChildren = 6, 0
	a := newTestAgent(t, newScript(endTurn("root done")).Model(), WithSpawner(sp), WithConfig(cfg))
	root, err := a.Run(ctx, "root")
	mustState(t, root, err, StateCompleted)
	var ok, refused atomic.Int32
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var err error
			if i%2 == 0 {
				_, err = a.SpawnChild(ctx, root.ID, NewRunRequest(fmt.Sprintf("remote %d", i)))
			} else {
				_, err = a.Start(ctx, fmt.Sprintf("local %d", i), WithParent(root.ID))
			}
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ErrSpawnLimit):
				refused.Add(1)
			default:
				t.Errorf("child %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if ok.Load() != 6 || refused.Load() != 14 {
		t.Fatalf("%d children started and %d were refused", ok.Load(), refused.Load())
	}
	// A child in another process at an unknown number of siblings takes a
	// share of one: it starts no child of its own.
	for _, req := range sp.requests() {
		if req.Metadata[MetaMaxDescendants] != "0" {
			t.Fatalf("a remote sibling can have %q runs under it", req.Metadata[MetaMaxDescendants])
		}
	}
}

// TestTreeSizeFromTheShareOfAParent proves that a run that a parent in
// another process started with a share holds its tree to that share.
func TestTreeSizeFromTheShareOfAParent(t *testing.T) {
	ctx := testCtx(t)
	a := newTestAgent(t, newScript(endTurn("done")).Model())
	r, err := a.Run(ctx, "child of a remote parent", WithMetadata(map[string]string{MetaMaxDescendants: "1"}))
	mustState(t, r, err, StateCompleted)
	if _, err := a.Start(ctx, "one", WithParent(r.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Start(ctx, "two", WithParent(r.ID)); !errors.Is(err, ErrSpawnLimit) {
		t.Fatalf("a second run under a share of one = %v", err)
	}
}

// TestTreeToolCallsAreShared proves that the tool calls of a child count
// against maxToolCalls of its parent: a call past the room of the tree does
// not run, and the model reads why.
func TestTreeToolCallsAreShared(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	m := routingModel(map[string][]*Response{
		"root":  {callTools(use("r1", "echo", `{"text":"r"}`)), endTurn("root done")},
		"child": {callTools(use("e1", "echo", `{"text":"a"}`), use("e2", "echo", `{"text":"b"}`), use("e3", "echo", `{"text":"c"}`)), endTurn("never")},
	})
	a := newTestAgent(t, m, WithTools(echoTool(&calls)))
	root, err := a.Run(ctx, "root", WithRunBudget(Budget{MaxToolCalls: 3}))
	mustState(t, root, err, StateCompleted)
	// The child alone could make 3 calls, but the root made one of the 3
	// of its tree.
	child, err := a.Run(ctx, "child", WithParent(root.ID), WithRunBudget(Budget{MaxToolCalls: 10}))
	mustState(t, child, err, StateFailed)
	if calls.Load() != 3 {
		t.Fatalf("%d calls ran in a tree with a limit of 3", calls.Load())
	}
	var res ToolRespondedData
	evs := ledgerEvents(t, a.Ledger(), child.ID)
	for _, e := range evs {
		var d ToolRespondedData
		if e.Type == EvToolResponded && e.Decode(&d) == nil && d.CallID == "e3" {
			res = d
		}
	}
	if !res.IsError || !strings.Contains(TextOf(res.Content), "budget.maxToolCalls") || !strings.Contains(TextOf(res.Content), root.ID) {
		t.Fatalf("the third call = %+v", res)
	}
	if !strings.Contains(child.Error, "maxToolCalls of the run tree of run "+root.ID) {
		t.Fatalf("child error = %q", child.Error)
	}
}
