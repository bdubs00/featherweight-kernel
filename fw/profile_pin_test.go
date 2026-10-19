package fw

// These tests change a profile or the configuration while a run lives and
// check that the run keeps the configuration that it was queued with. They
// use only the API that existed before the kernel resolved the profile of a
// run, so they also show the drift on a kernel that reads the profile at
// every step.

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// pinProvider is a model provider for these tests. Its models carry only
// their name. The middleware of a pinRecorder replaces each one with a model
// that answers from a script and records each request with that name.
const pinProvider = "pintest"

func init() {
	RegisterModel(pinProvider, func(_ context.Context, name string, _ *Config, _ Secrets) (Model, error) {
		return pinModel{name: name}, nil
	})
}

type pinModel struct{ name string }

var errNoRecorder = errors.New("pintest: no recorder wraps this model")

func (pinModel) Generate(context.Context, *Request) (*Response, error) { return nil, errNoRecorder }

func (pinModel) Stream(context.Context, *Request, func(Delta)) (*Response, error) {
	return nil, errNoRecorder
}

// pinCall is one model request: the name of the model, the system prompt,
// the tool names, and the effort.
type pinCall struct {
	model  string
	system string
	tools  string
	effort string
}

// pinRecorder answers every model of one test from one script, in order.
// A nil response in the script blocks the call until its context ends, and
// sends on started when started is set.
type pinRecorder struct {
	mu      sync.Mutex
	resps   []*Response
	calls   []pinCall
	started chan struct{}
}

func (r *pinRecorder) script(resps ...*Response) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resps = append(r.resps, resps...)
}

func (r *pinRecorder) middleware(m Model) Model {
	name := "?"
	if p, ok := m.(pinModel); ok {
		name = p.name
	}
	return ModelFunc{StreamFn: func(ctx context.Context, req *Request, _ func(Delta)) (*Response, error) {
		r.mu.Lock()
		tools := make([]string, 0, len(req.Tools))
		for _, s := range req.Tools {
			tools = append(tools, s.Name)
		}
		r.calls = append(r.calls, pinCall{model: name, system: req.System, tools: strings.Join(tools, ","), effort: req.Effort})
		if len(r.resps) == 0 {
			r.mu.Unlock()
			return nil, errors.New("pintest: no response left")
		}
		resp := r.resps[0]
		r.resps = r.resps[1:]
		started := r.started
		r.mu.Unlock()
		if resp == nil {
			if started != nil {
				started <- struct{}{}
			}
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return resp, nil
	}}
}

// expect checks the requests from index from to index to, and that no
// other request came after them.
func (r *pinRecorder) expect(t *testing.T, from, to int, model, system, tools string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) != to {
		t.Fatalf("%d model requests, want %d: %+v", len(r.calls), to, r.calls)
	}
	for i := from; i < to; i++ {
		c := r.calls[i]
		if c.model != model || !strings.HasPrefix(c.system, system) || c.tools != tools {
			t.Fatalf("request %d = model %q, tools %q, system %q; want model %q, tools %q, system %q",
				i, c.model, c.tools, c.system, model, tools, system)
		}
	}
}

func (r *pinRecorder) call(i int) pinCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[i]
}

// pinTool is a read-only tool that answers with its name.
func pinTool(name string) Tool {
	return NewTool(ToolSpec{Name: name, ReadOnly: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		return TextResult(name), nil
	})
}

func pinConfig(t *testing.T, model string) *Config {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Model = model
	return cfg
}

func pinLedger(t *testing.T, dir string) *FileLedger {
	t.Helper()
	l, err := OpenFileLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// pinAgent builds an agent with the tools alpha, beta, gamma, and the
// destructive tool delta, whose models answer through rec.
func pinAgent(t *testing.T, cfg *Config, l Ledger, rec *pinRecorder) *Agent {
	t.Helper()
	t.Setenv("FW_ATESPACE", "")
	delta := NewTool(ToolSpec{Name: "delta", Destructive: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		return TextResult("delta"), nil
	})
	a, err := New(
		WithoutDefaults(),
		WithConfig(cfg),
		WithAuthn(NoAuthn{}),
		WithLedger(l),
		WithBus(NewMemBus()),
		WithLogger(quietLogger()),
		WithModelMiddleware(rec.middleware),
		WithTools(pinTool("alpha"), pinTool("beta"), pinTool("gamma"), delta),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { closePin(a) })
	return a
}

func closePin(a *Agent) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a.Close(ctx)
}

// deniedByPolicy checks that the policy denied the call.
func deniedByPolicy(t *testing.T, l Ledger, run, call string) {
	t.Helper()
	for _, e := range ledgerEvents(t, l, run) {
		var d ToolRespondedData
		if e.Type != EvToolResponded || e.Decode(&d) != nil || d.CallID != call {
			continue
		}
		if !d.IsError || !strings.Contains(TextOf(d.Content), "denied by policy") {
			t.Fatalf("result of %s = %+v; want a denial by the policy", call, d)
		}
		return
	}
	t.Fatalf("no result of %s", call)
}

func setProfiles(t *testing.T, a *Agent, set map[string]AgentProfile) {
	t.Helper()
	if err := a.SetProfiles(set); err != nil {
		t.Fatal(err)
	}
}

// A run keeps the model, the system prompt, the tools, and the budget that
// it was queued with when a person changes the profile, for a follow-up and
// after a restart, while a stricter policy of the new profile applies at
// once. A new run gets the new profile.
func TestRunKeepsItsProfileAfterSetProfiles(t *testing.T) {
	ctx := testCtx(t)
	dir := filepath.Join(t.TempDir(), "runs")
	rec := &pinRecorder{}
	cfg := pinConfig(t, "pintest/base")
	cfg.Budget = Budget{MaxTurns: 50}
	a := pinAgent(t, cfg, pinLedger(t, dir), rec)
	old := AgentProfile{System: "You are the old worker.", Model: "pintest/old", Tools: []string{"alpha", "beta"}, Budget: &Budget{MaxTurns: 6}}
	setProfiles(t, a, map[string]AgentProfile{"worker": old})

	rec.script(endTurn("one"))
	r, err := a.Run(ctx, "first", AsAgent("worker"))
	mustState(t, r, err, StateCompleted)
	rec.expect(t, 0, 1, "old", "You are the old worker.", "alpha,beta")
	budget, err := a.RunBudget(ctx, r.ID)
	if err != nil || budget != (Budget{MaxTurns: 6}) {
		t.Fatalf("budget = %+v, %v", budget, err)
	}

	next := AgentProfile{System: "You are the new worker.", Model: "pintest/new", Tools: []string{"alpha", "gamma"},
		Budget: &Budget{MaxTurns: 1}, Policy: &Rules{Deny: []string{"beta"}}}
	setProfiles(t, a, map[string]AgentProfile{"worker": next})

	// With the budget of the new profile, the follow-up fails at its first
	// model call. With the old budget, it goes on, and the new policy denies
	// beta, which only the old tool list holds.
	rec.script(callTools(use("c1", "beta", `{}`)), endTurn("two"))
	if err := a.Send(ctx, r.ID, UserText("second"), OriginCLI); err != nil {
		t.Fatal(err)
	}
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)
	if r.Output != "two" {
		t.Fatalf("output = %q", r.Output)
	}
	rec.expect(t, 1, 3, "old", "You are the old worker.", "alpha,beta")
	deniedByPolicy(t, a.Ledger(), r.ID, "c1")
	if got, _ := a.RunBudget(ctx, r.ID); got != budget {
		t.Fatalf("budget after the change = %+v, want %+v", got, budget)
	}

	rec.script(endTurn("fresh"))
	r2, err := a.Run(ctx, "new work", AsAgent("worker"))
	mustState(t, r2, err, StateCompleted)
	rec.expect(t, 3, 4, "new", "You are the new worker.", "alpha,gamma")
	if got, _ := a.RunBudget(ctx, r2.ID); got.MaxTurns != 1 {
		t.Fatalf("budget of the new run = %+v", got)
	}
	closePin(a)

	// After a restart, the configuration holds the new profile, another
	// default model, and another budget. The recovered run still uses what
	// it was queued with. A budget of one tool call would stop it.
	cfg2 := pinConfig(t, "pintest/base2")
	cfg2.Budget = Budget{MaxTurns: 50, MaxToolCalls: 1}
	cfg2.Agents = map[string]AgentProfile{"worker": next}
	b := pinAgent(t, cfg2, pinLedger(t, dir), rec)
	if got, err := b.RunBudget(ctx, r.ID); err != nil || got != budget {
		t.Fatalf("budget after the restart = %+v, %v; want %+v", got, err, budget)
	}
	rec.script(callTools(use("c2", "beta", `{}`)), endTurn("three"))
	if err := b.Send(ctx, r.ID, UserText("third"), OriginCLI); err != nil {
		t.Fatal(err)
	}
	r, err = b.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)
	if r.Output != "three" {
		t.Fatalf("output after the restart = %q", r.Output)
	}
	rec.expect(t, 4, 6, "old", "You are the old worker.", "alpha,beta")
	deniedByPolicy(t, b.Ledger(), r.ID, "c2")
}

// A run that waits for an approval goes on with the profile that it was
// queued with after the profile changed.
func TestWaitingRunKeepsItsProfile(t *testing.T) {
	ctx := testCtx(t)
	rec := &pinRecorder{}
	a := pinAgent(t, pinConfig(t, "pintest/base"), NewMemLedger(), rec)
	setProfiles(t, a, map[string]AgentProfile{"worker": {System: "Old.", Model: "pintest/old"}})
	rec.script(callTools(use("d1", "delta", `{}`)), endTurn("done"))
	r, err := a.Run(ctx, "change something", AsAgent("worker"))
	mustState(t, r, err, StateInputRequired)
	setProfiles(t, a, map[string]AgentProfile{"worker": {System: "New.", Model: "pintest/new"}})
	if err := a.Decide(ctx, r.ID, ApprovalDecision{ApprovalID: r.Pending.ID, Decision: "allow"}); err != nil {
		t.Fatal(err)
	}
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)
	rec.expect(t, 0, 2, "old", "Old.", "alpha,beta,gamma,delta")
}

// A run that a shutdown suspended in the middle of a model call resumes at
// the next start with the profile that it was queued with, although the
// configuration of that start holds another profile.
func TestSuspendedRunResumesWithItsProfile(t *testing.T) {
	ctx := testCtx(t)
	dir := filepath.Join(t.TempDir(), "runs")
	rec := &pinRecorder{started: make(chan struct{}, 1)}
	cfg := pinConfig(t, "pintest/base")
	cfg.Agents = map[string]AgentProfile{"worker": {System: "Old.", Model: "pintest/old"}}
	a := pinAgent(t, cfg, pinLedger(t, dir), rec)
	rec.script(nil)
	r, err := a.Start(ctx, "long job", AsAgent("worker"))
	if err != nil {
		t.Fatal(err)
	}
	<-rec.started
	sctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	a.Close(sctx)
	cancel()
	if got, _ := a.Get(ctx, r.ID); got.State != StateSuspended {
		t.Fatalf("state after the shutdown = %s", got.State)
	}

	cfg2 := pinConfig(t, "pintest/base2")
	cfg2.Agents = map[string]AgentProfile{"worker": {System: "New.", Model: "pintest/new"}}
	rec.script(endTurn("finished later"))
	b := pinAgent(t, cfg2, pinLedger(t, dir), rec)
	got, err := b.Wait(ctx, r.ID)
	mustState(t, got, err, StateCompleted)
	if got.Output != "finished later" {
		t.Fatalf("output = %q", got.Output)
	}
	rec.expect(t, 0, 2, "old", "Old.", "alpha,beta,gamma,delta")
}

// A change of the default model, of a name in models, or of the effort in
// the configuration reaches new runs only.
func TestRunKeepsTheModelOfTheConfiguration(t *testing.T) {
	ctx := testCtx(t)
	dir := filepath.Join(t.TempDir(), "runs")
	rec := &pinRecorder{}
	cfg := pinConfig(t, "pintest/first")
	cfg.Effort = "low"
	cfg.Models = map[string]string{"fast": "pintest/quick1"}
	cfg.Agents = map[string]AgentProfile{"quick": {Model: "fast"}}
	a := pinAgent(t, cfg, pinLedger(t, dir), rec)
	rec.script(endTurn("a"), endTurn("b"))
	plain, err := a.Run(ctx, "plain")
	mustState(t, plain, err, StateCompleted)
	quick, err := a.Run(ctx, "quick", AsAgent("quick"))
	mustState(t, quick, err, StateCompleted)
	closePin(a)

	cfg2 := pinConfig(t, "pintest/second")
	cfg2.Effort = "max"
	cfg2.Models = map[string]string{"fast": "pintest/quick2"}
	cfg2.Agents = map[string]AgentProfile{"quick": {Model: "fast"}}
	b := pinAgent(t, cfg2, pinLedger(t, dir), rec)
	rec.script(endTurn("c"), endTurn("d"), endTurn("e"))
	for _, id := range []string{plain.ID, quick.ID} {
		if err := b.Send(ctx, id, UserText("again"), OriginCLI); err != nil {
			t.Fatal(err)
		}
		r, err := b.Wait(ctx, id)
		mustState(t, r, err, StateCompleted)
	}
	fresh, err := b.Run(ctx, "fresh")
	mustState(t, fresh, err, StateCompleted)
	want := []pinCall{
		{model: "first", effort: "low"}, {model: "quick1", effort: "low"},
		{model: "first", effort: "low"}, {model: "quick1", effort: "low"},
		{model: "second", effort: "max"},
	}
	for i, w := range want {
		if c := rec.call(i); c.model != w.model || c.effort != w.effort {
			t.Fatalf("request %d = model %q, effort %q; want %q, %q", i, c.model, c.effort, w.model, w.effort)
		}
	}
}

// A run from the ledger of an older kernel records no resolved profile, so
// it reads the named profile at each step, as that kernel did.
func TestRunFromAnOlderLedgerReadsTheNamedProfile(t *testing.T) {
	ctx := testCtx(t)
	l := NewMemLedger()
	id := "run-older"
	seed(t, l, id,
		MustEvent(id, EvRunQueued, RunQueuedData{Agent: "worker", Input: "hi", Origin: OriginCLI}),
		MustEvent(id, EvRunStarted, RunStartedData{Model: "pintest/then", Tools: []string{"alpha"}}),
		MustEvent(id, EvTurnStarted, TurnData{Turn: 1}),
		MustEvent(id, EvModelRequested, ModelRequestedData{Provider: "pintest", Model: "then"}),
		MustEvent(id, EvModelResponded, ModelRespondedData{Content: []Block{Text("hello")}, StopReason: StopEndTurn, Provider: "pintest", Model: "then"}),
		MustEvent(id, EvTurnFinished, TurnData{Turn: 1}),
		MustEvent(id, EvRunFinished, RunFinishedData{Outcome: OutcomeCompleted, Output: "hello"}),
	)
	rec := &pinRecorder{}
	a := pinAgent(t, pinConfig(t, "pintest/base"), l, rec)
	setProfiles(t, a, map[string]AgentProfile{"worker": {System: "Now.", Model: "pintest/now", Tools: []string{"alpha"}}})
	rec.script(endTurn("again"))
	if err := a.Send(ctx, id, UserText("more"), OriginCLI); err != nil {
		t.Fatal(err)
	}
	r, err := a.Wait(ctx, id)
	mustState(t, r, err, StateCompleted)
	rec.expect(t, 0, 1, "now", "Now.", "alpha")

	setProfiles(t, a, map[string]AgentProfile{"worker": {System: "Later.", Model: "pintest/later", Tools: []string{"beta"}}})
	rec.script(endTurn("later"))
	if err := a.Send(ctx, id, UserText("more"), OriginCLI); err != nil {
		t.Fatal(err)
	}
	r, err = a.Wait(ctx, id)
	mustState(t, r, err, StateCompleted)
	if r.Output != "later" {
		t.Fatalf("output = %q", r.Output)
	}
	rec.expect(t, 1, 2, "later", "Later.", "beta")
}

// A branch keeps the profile of its source from the copied events, and a
// branch that names a model uses that model.
func TestBranchKeepsTheProfileOfItsSource(t *testing.T) {
	ctx := testCtx(t)
	rec := &pinRecorder{}
	a := pinAgent(t, pinConfig(t, "pintest/base"), NewMemLedger(), rec)
	setProfiles(t, a, map[string]AgentProfile{"worker": {System: "Old.", Model: "pintest/old", Tools: []string{"alpha"}}})
	rec.script(endTurn("one"))
	r, err := a.Run(ctx, "first", AsAgent("worker"))
	mustState(t, r, err, StateCompleted)
	setProfiles(t, a, map[string]AgentProfile{"worker": {System: "New.", Model: "pintest/new", Tools: []string{"gamma"}}})

	rec.script(endTurn("same"), endTurn("other"))
	same, err := a.Fork(ctx, r.ID, ForkRequest{Input: "go on"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.Wait(ctx, same.ID)
	mustState(t, got, err, StateCompleted)
	rec.expect(t, 1, 2, "old", "Old.", "alpha")

	other, err := a.Fork(ctx, r.ID, ForkRequest{Input: "go on elsewhere", Model: "pintest/other"})
	if err != nil {
		t.Fatal(err)
	}
	got, err = a.Wait(ctx, other.ID)
	mustState(t, got, err, StateCompleted)
	rec.expect(t, 2, 3, "other", "Old.", "alpha")
	evs := ledgerEvents(t, a.Ledger(), other.ID)
	var req ModelRequestedData
	for _, e := range evs {
		if e.Type == EvModelRequested {
			_ = e.Decode(&req)
		}
	}
	if req.Provider != pinProvider || req.Model != "other" {
		t.Fatalf("last request of the branch = %+v", req)
	}
}

// A child takes the tools of its parent from the profile that the parent
// was queued with, and the policy that the profile of the parent has now
// still tightens the child.
func TestChildReadsTheResolvedProfileOfItsParent(t *testing.T) {
	ctx := testCtx(t)
	rec := &pinRecorder{}
	a := pinAgent(t, pinConfig(t, "pintest/base"), NewMemLedger(), rec)
	helper := AgentProfile{System: "You help.", Model: "pintest/helper"}
	setProfiles(t, a, map[string]AgentProfile{
		"lead":   {Model: "pintest/lead", Tools: []string{"alpha", "beta"}, Budget: &Budget{MaxTurns: 7}},
		"helper": helper,
	})
	rec.script(endTurn("led"))
	p, err := a.Run(ctx, "lead", AsAgent("lead"))
	mustState(t, p, err, StateCompleted)
	setProfiles(t, a, map[string]AgentProfile{
		"lead":   {Model: "pintest/lead2", Tools: []string{"alpha", "gamma"}, Budget: &Budget{MaxTurns: 2}, Policy: &Rules{Deny: []string{"alpha"}}},
		"helper": helper,
	})

	rec.script(callTools(use("k1", "alpha", `{}`)), endTurn("helped"))
	c, err := a.SpawnChild(ctx, p.ID, NewRunRequest("help", AsAgent("helper")))
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.Wait(ctx, c.ID)
	mustState(t, got, err, StateCompleted)
	rec.expect(t, 1, 3, "helper", "You help.", "alpha,beta")
	deniedByPolicy(t, a.Ledger(), c.ID, "k1")
	// The budget of the parent caps the child, as the parent was queued.
	if b, _ := a.RunBudget(ctx, c.ID); b.MaxTurns != 7 {
		t.Fatalf("budget of the child = %+v, want at most 7 turns", b)
	}
}
