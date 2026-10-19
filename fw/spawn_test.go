package fw

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// otherSpawner runs the children in another agent, as a process of its own
// would, and knows them by the member name "other".
type otherSpawner struct {
	other *Agent

	placeErr   error         // Place fails with it
	branchErr  error         // Branch fails with it
	branchWait chan struct{} // Branch waits for it first

	mu        sync.Mutex
	reqs      []RunRequest
	abandoned []string // the members that Abandon removed
}

// Abandon records the member of a lost branch.
func (s *otherSpawner) Abandon(_ context.Context, member string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.abandoned = append(s.abandoned, member)
	return nil
}

func (s *otherSpawner) abandonedMembers() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.abandoned...)
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

// Place names the other agent as the process of a branch. A test that sets
// placeErr gets that error instead.
func (s *otherSpawner) Place(context.Context, *Run) (string, error) {
	if s.placeErr != nil {
		return "", s.placeErr
	}
	return "other", nil
}

// Branch starts the branch in the other agent from the kept events, as the
// native API of another process does for a trusted caller. A test that sets
// branchErr gets that error instead, and one that sets branchWait waits for
// that channel or the context first, as a child Task that boots would.
func (s *otherSpawner) Branch(ctx context.Context, _ string, events []Event, req ForkRequest) (*Run, error) {
	if s.branchWait != nil {
		select {
		case <-s.branchWait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if s.branchErr != nil {
		return nil, s.branchErr
	}
	return s.other.ForkEvents(ctx, events, req)
}

// Plan names the other agent as the process of every child.
func (s *otherSpawner) Plan(*Run, RunRequest) (string, error) { return "other", nil }

// Find looks for a child in the other agent, as after a restart.
func (s *otherSpawner) Find(ctx context.Context, _, id string) (*Run, error) {
	return s.other.Get(ctx, id)
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

// testTools gives a test agent a tools list with a few names to intersect.
func testTools() []Tool {
	noop := func(context.Context, json.RawMessage) (ToolResult, error) { return TextResult("done"), nil }
	return []Tool{
		NewTool(ToolSpec{Name: "files", ReadOnly: true}, noop),
		NewTool(ToolSpec{Name: "bash", Destructive: true}, noop),
		NewTool(ToolSpec{Name: "plus", ReadOnly: true}, noop),
		NewTool(ToolSpec{Name: "deep", ReadOnly: true, OptIn: true}, noop),
	}
}

// TestSpawnChildToolsStayInsideTheParent proves that a child run in another
// process gets only the tools that the parent and every ancestor have. An
// explicit tool list of the child cannot name what the parent lacks, an
// empty list cannot mean every tool, and no intersection refuses the child
// instead of handing it everything.
func TestSpawnChildToolsStayInsideTheParent(t *testing.T) {
	ctx := testCtx(t)
	other := newTestAgent(t, newScript(endTurn("child done")).Model())
	sp := &otherSpawner{other: other}
	a := newTestAgent(t, newScript(endTurn("parent done")).Model(), WithSpawner(sp),
		WithProfile(AgentProfile{Name: "default", Tools: []string{"files", "spawn_task"}}),
		WithTools(testTools()...))
	parent, err := a.Run(ctx, "parent work")
	mustState(t, parent, err, StateCompleted)

	// A child of its own with broad tools cannot widen the parent.
	if _, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("child work",
		WithRunProfile(AgentProfile{Tools: []string{"bash"}}))); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a bash child of a files-only parent = %v", err)
	}
	// An empty tools list stays the tools of the parent. It does not become
	// an unrestricted list, which an empty list means at run time.
	if _, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("child work",
		WithRunProfile(AgentProfile{Tools: []string{}}))); err != nil {
		t.Fatalf("an empty tools list = %v", err)
	}
	// A child with a broad list of its own keeps only the tools of the
	// parent, and an opt-in tool the parent never named stays out.
	if _, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("child work",
		WithRunProfile(AgentProfile{Tools: []string{"files", "bash", "deep"}}))); err != nil {
		t.Fatalf("SpawnChild: %v", err)
	}
	// A child with no list of its own gets the tools of the parent too.
	if _, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("child work")); err != nil {
		t.Fatalf("SpawnChild: %v", err)
	}
	reqs := sp.requests()
	if len(reqs) != 3 {
		t.Fatalf("requests = %d", len(reqs))
	}
	for i, req := range reqs {
		if req.Profile == nil || join(req.Profile.Tools) != "files" {
			t.Fatalf("tools of the child request %d = %+v", i, req.Profile)
		}
	}
}

// TestSpawnChildCarriesTheRulesOfTheParent proves that a remote child
// carries the base rules of this configuration and the rules of every
// ancestor, so the other process enforces them without the tree.
func TestSpawnChildCarriesTheRulesOfTheParent(t *testing.T) {
	ctx := testCtx(t)
	other := newTestAgent(t, newScript(endTurn("child done")).Model())
	sp := &otherSpawner{other: other}
	cfg := DefaultConfig()
	cfg.Policy.Rules = Rules{Default: "ask", Deny: []string{"bash: rm *"}}
	a := newTestAgent(t, newScript(endTurn("parent work")).Model(), WithSpawner(sp), WithConfig(cfg), WithTools(testTools()...))
	parent, err := a.Start(ctx, "parent work", WithRunProfile(AgentProfile{
		Policy: &Rules{Deny: []string{"send"}},
		Tools:  []string{"files", "bash", "plus"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	child, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("child work",
		WithRunProfile(AgentProfile{Policy: &Rules{Ask: []string{"clock"}}})))
	if err != nil {
		t.Fatalf("SpawnChild: %v", err)
	}
	reqs := sp.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d", len(reqs))
	}
	q := reqs[0].Profile
	if q == nil || q.Policy == nil {
		t.Fatalf("profile = %+v", q)
	}
	// Check behavior rather than a flattened encoding of independent rules.
	for _, tc := range []struct {
		tool, input string
		want        Verdict
	}{
		{"bash", `{"command":"rm a"}`, VerdictDeny},
		{"send", `{}`, VerdictDeny},
		{"clock", `{}`, VerdictAsk},
		{"files", `{}`, VerdictAsk},
	} {
		if got := NewRulesPolicy(*q.Policy).Decide(ctx, ToolCall{Spec: ToolSpec{Name: tc.tool}, Input: json.RawMessage(tc.input)}); got.Verdict != tc.want {
			t.Fatalf("carried policy for %s = %+v, want %v", tc.tool, got, tc.want)
		}
	}
	// The tools of the child are the intersection, as exact names.
	if join(q.Tools) != "files,bash,plus" {
		t.Fatalf("tools of the child = %v", q.Tools)
	}
	// The child run keeps its own request and its lineage only in this
	// process record; the request carries the parent in the metadata.
	if child.Parent != parent.ID || reqs[0].Parent != "" || reqs[0].Metadata[MetaParent] != parent.ID {
		t.Fatalf("child = %+v request = %+v", child, reqs[0])
	}
}

// TestSpawnDepthDepletesOverARemoteTree walks the depth boundary: a parent
// starts a child, a child starts a grandchild at the last level, and the
// grandchild carries a deny of spawn_task, so nothing goes past
// spawn.maxDepth even in a process that counts its own depth from zero.
func TestSpawnDepthDepletesOverARemoteTree(t *testing.T) {
	ctx := testCtx(t)
	other := newTestAgent(t, newScript(endTurn("child done")).Model())
	sp := &otherSpawner{other: other}
	cfg := DefaultConfig()
	cfg.Spawn.MaxDepth = 2
	a := newTestAgent(t, newScript(endTurn("parent done")).Model(), WithSpawner(sp), WithConfig(cfg))
	parent, err := a.Start(ctx, "parent work")
	if err != nil {
		t.Fatal(err)
	}
	// A parent at depth 0 starts a child at depth 1: no deny of spawn_task
	// travels with it, because the child may still start one child.
	child, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("child work"))
	if err != nil {
		t.Fatal(err)
	}
	if child == nil || child.Depth != 1 || child.Parent != parent.ID {
		t.Fatalf("child = %+v", child)
	}
	if q := sp.requests()[0].Profile; q == nil || containsRule(q.Policy, "spawn_task") {
		t.Fatalf("policy of the child = %+v", q)
	}
	// A child at depth 1 starts a grandchild at depth 2, the last level.
	// The grandchild carries a deny of spawn_task.
	mid, err := a.Start(ctx, "mid work", WithParent(parent.ID))
	if err != nil {
		t.Fatal(err)
	}
	grand, err := a.SpawnChild(ctx, mid.ID, NewRunRequest("grandchild work"))
	if err != nil {
		t.Fatalf("SpawnChild: %v", err)
	}
	_ = grand
	if q := sp.requests()[1].Profile; q == nil || !containsRule(q.Policy, "spawn_task") {
		t.Fatalf("policy of the grandchild = %+v", q)
	}
	// A run that carries the deny, as the grandchild would, starts no
	// child: the check that the child process applies refuses it here too,
	// so the depth counts past a process border.
	leaf, err := a.Start(ctx, "leaf work", WithParent(mid.ID), WithRunProfile(AgentProfile{
		Policy: &Rules{Deny: []string{"spawn_task"}, Default: "deny"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.SpawnChild(ctx, leaf.ID, NewRunRequest("again")); !errors.Is(err, ErrSpawnLimit) {
		t.Fatalf("a child of the carrying leaf = %v", err)
	}
}

// containsRule reports whether the rules deny or ask or allow the tool.
func containsRule(r *Rules, tool string) bool {
	if r == nil {
		return false
	}
	for _, list := range [][]string{r.Deny, r.Ask, r.Allow} {
		for _, rule := range list {
			if rule == tool {
				return true
			}
		}
	}
	for i := range r.All {
		if containsRule(&r.All[i], tool) {
			return true
		}
	}
	return false
}

func TestSpawnDisabledStopsEveryChildEntryPoint(t *testing.T) {
	ctx := testCtx(t)
	for _, remote := range []bool{false, true} {
		cfg := DefaultConfig()
		cfg.Spawn.Disabled, cfg.Spawn.MaxDepth = true, 0
		opts := []Option{WithConfig(cfg)}
		if remote {
			opts = append(opts, WithSpawner(&otherSpawner{})) // must never be reached
		}
		a := newTestAgent(t, newScript(endTurn("parent")).Model(), opts...)
		parent, err := a.Run(ctx, "parent")
		mustState(t, parent, err, StateCompleted)
		if _, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("child")); !errors.Is(err, ErrSpawnLimit) {
			t.Fatalf("remote=%v SpawnChild = %v", remote, err)
		}
		if _, err := a.Start(ctx, "child", WithParent(parent.ID)); !errors.Is(err, ErrSpawnLimit) {
			t.Fatalf("remote=%v Start with parent = %v", remote, err)
		}
	}
}

func TestRemoteChildResolvesNamedRestrictions(t *testing.T) {
	ctx := testCtx(t)
	script := newScript(callTools(use("f", "files", `{}`), use("b", "bash", `{}`)), endTurn("done"))
	other := newTestAgent(t, script.Model(), WithTools(testTools()...), WithProfile(AgentProfile{Name: "worker", Tools: []string{"*"}}))
	sp := &otherSpawner{other: other}
	a := newTestAgent(t, newScript(endTurn("parent")).Model(), WithTools(testTools()...), WithSpawner(sp),
		WithProfile(AgentProfile{Name: "worker", Tools: []string{"files"}, Policy: &Rules{Default: "deny", Allow: []string{"files"}}}))
	parent, err := a.Run(ctx, "parent")
	mustState(t, parent, err, StateCompleted)
	child, err := a.SpawnChild(ctx, parent.ID, RunRequest{Input: "child", Agent: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := a.Wait(ctx, child.ID)
	mustState(t, r, err, StateCompleted)
	q := sp.requests()[0].Profile
	if join(q.Tools) != "files" {
		t.Fatalf("named child tools widened: %v", q.Tools)
	}
	results := lastToolResults(t, script.requests()[1])
	if results[0].IsError || !results[1].IsError || !strings.Contains(TextOf(results[1].Content), "unknown tool") {
		t.Fatalf("remote model received wrong capability results: %+v", results)
	}
	if _, err := a.SpawnChild(ctx, parent.ID, RunRequest{Input: "unknown", Agent: "absent"}); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("unresolved remote profile = %v", err)
	}
}

func TestRemoteChildCannotOptInOrExpandLiteralNames(t *testing.T) {
	ctx := testCtx(t)
	sp := &otherSpawner{other: newTestAgent(t, newScript(endTurn("done")).Model())}
	a := newTestAgent(t, newScript(endTurn("parent")).Model(), WithTools(testTools()...), WithSpawner(sp),
		WithProfile(AgentProfile{Name: "privileged", Tools: []string{"deep"}}))
	parent, err := a.Run(ctx, "parent")
	mustState(t, parent, err, StateCompleted)
	if _, err := a.SpawnChild(ctx, parent.ID, RunRequest{Input: "child", Agent: "privileged"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil parent tool list granted opt-in tool: %v", err)
	}
	literal := NewTool(ToolSpec{Name: "x*", ReadOnly: true}, func(context.Context, json.RawMessage) (ToolResult, error) { return TextResult("x"), nil })
	b := newTestAgent(t, newScript(endTurn("parent")).Model(), WithTools(literal), WithSpawner(sp))
	p, err := b.Run(ctx, "parent")
	mustState(t, p, err, StateCompleted)
	if _, err := b.SpawnChild(ctx, p.ID, NewRunRequest("child")); err != nil {
		t.Fatal(err)
	}
	tools := sp.requests()[0].Profile.Tools
	if !toolAllowed(tools, literal.Spec()) || toolAllowed(tools, ToolSpec{Name: "xyz"}) {
		t.Fatalf("literal tool name became a glob: %v", tools)
	}
}

func TestRemoteExternalPolicyNeedsAnEnforcementContract(t *testing.T) {
	ctx := testCtx(t)
	sp := &otherSpawner{}
	a := newTestAgent(t, newScript(endTurn("parent")).Model(), WithSpawner(sp), WithPolicy(&externalPolicy{}))
	parent, err := a.Run(ctx, "parent")
	mustState(t, parent, err, StateCompleted)
	if _, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("child")); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("external policy was silently omitted: %v", err)
	}
}

// TestSpawnChildWithACustomPolicyDoesNotTravel proves that a policy in Go
// code, which is not rules and not the control plane, keeps its process:
// a child in another one does not start, because the rules would not say
// the same things there.
func TestSpawnChildWithACustomPolicyDoesNotTravel(t *testing.T) {
	ctx := testCtx(t)
	other := newTestAgent(t, newScript(endTurn("child done")).Model())
	sp := &otherSpawner{other: other}
	policy := PolicyFunc(func(context.Context, ToolCall) Decision { return Decision{Verdict: VerdictAllow, Reason: "custom"} })
	a := newTestAgent(t, newScript(endTurn("parent done")).Model(), WithSpawner(sp), WithPolicy(policy))
	parent, err := a.Run(ctx, "parent work")
	mustState(t, parent, err, StateCompleted)
	if _, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("child work")); err == nil || !strings.Contains(err.Error(), "does not travel") {
		t.Fatalf("SpawnChild with a custom policy = %v", err)
	}
	if len(sp.requests()) != 0 {
		t.Fatal("the child started though the policy does not travel")
	}
	// In this process the same policy still holds for a child.
	b := newTestAgent(t, newScript(endTurn("child done")).Model(), WithPolicy(policy))
	p2, err := b.Start(ctx, "child work")
	if err != nil {
		t.Fatal(err)
	}
	done, err := b.SpawnChild(ctx, p2.ID, NewRunRequest("do x"))
	if err != nil || done == nil {
		t.Fatalf("SpawnChild in the same process = %+v, %v", done, err)
	}
}
