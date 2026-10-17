package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// scripted is a fake model that answers by the text of the last user
// message that holds text: the input of a run, or a follow-up. Each key has
// a list of steps that the model takes in order, so a parent and its
// children get different answers in one test. The model records every
// request.
type scripted struct {
	mu   sync.Mutex
	plan map[string][]func(*fw.Request) *fw.Response
	seen map[string]int
	reqs map[string][]*fw.Request
}

func newScripted() *scripted {
	return &scripted{
		plan: map[string][]func(*fw.Request) *fw.Response{},
		seen: map[string]int{},
		reqs: map[string][]*fw.Request{},
	}
}

// say adds fixed answers for a key.
func (s *scripted) say(key string, resps ...*fw.Response) {
	for _, r := range resps {
		s.then(key, func(*fw.Request) *fw.Response { return r })
	}
}

// then adds an answer that reads the request, for example a tool result.
func (s *scripted) then(key string, fn func(*fw.Request) *fw.Response) {
	s.plan[key] = append(s.plan[key], fn)
}

// Model returns the fw.Model.
func (s *scripted) Model() fw.Model {
	return fw.ModelFunc{GenerateFn: func(_ context.Context, req *fw.Request) (*fw.Response, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		key := lastUserText(req.Messages)
		s.reqs[key] = append(s.reqs[key], req)
		i := s.seen[key]
		steps := s.plan[key]
		if i >= len(steps) {
			return nil, fmt.Errorf("scripted: no answer %d for %q", i+1, key)
		}
		s.seen[key]++
		return steps[i](req), nil
	}}
}

// requestsFor returns the requests of the run with that input, in order.
func (s *scripted) requestsFor(key string) []*fw.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*fw.Request(nil), s.reqs[key]...)
}

func lastUserText(msgs []fw.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != fw.RoleUser {
			continue
		}
		if t := fw.TextOf(msgs[i].Content); t != "" {
			return t
		}
	}
	return ""
}

func endTurn(text string) *fw.Response {
	return &fw.Response{Content: []fw.Block{fw.Text(text)}, StopReason: fw.StopEndTurn, Usage: fw.Usage{InputTokens: 10, OutputTokens: 5}}
}

func callTools(uses ...fw.Block) *fw.Response {
	return &fw.Response{Content: append([]fw.Block{fw.Text("calling")}, uses...), StopReason: fw.StopToolUse, Usage: fw.Usage{InputTokens: 10, OutputTokens: 5}}
}

func use(id, name, input string) fw.Block {
	return fw.Block{Type: fw.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(input)}
}

// lastToolResults returns the tool_result blocks of the last message of a
// request.
func lastToolResults(t *testing.T, req *fw.Request) []fw.Block {
	t.Helper()
	m := req.Messages[len(req.Messages)-1]
	if m.Role != fw.RoleUser {
		t.Fatalf("last message role = %s", m.Role)
	}
	for _, b := range m.Content {
		if b.Type != fw.BlockToolResult {
			t.Fatalf("last message holds a %s block", b.Type)
		}
	}
	return m.Content
}

func builtinTool(t *testing.T, name string, cfg *fw.Config) fw.Tool {
	t.Helper()
	tool, err := fw.NewBuiltinTool(name, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

// newSpawnAgent builds an agent with the real spawn_task and await_task
// tools, a scripted model, and the options given.
func newSpawnAgent(t *testing.T, m fw.Model, opts ...fw.Option) *fw.Agent {
	t.Helper()
	cfg := fw.DefaultConfig()
	base := []fw.Option{
		fw.WithoutDefaults(),
		fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()),
		fw.WithAuthn(fw.NoAuthn{}),
		fw.WithModel(m),
		fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		fw.WithTools(builtinTool(t, "spawn_task", cfg), builtinTool(t, "await_task", cfg)),
	}
	a, err := fw.New(append(base, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	return a
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// runCtx is the context that the kernel gives a tool of a run.
func runCtx(ctx context.Context, a *fw.Agent, runID string) context.Context {
	return fw.WithAgent(fw.WithRunInfo(ctx, fw.RunInfo{RunID: runID, Agent: "default", CallID: "call-1"}), a)
}

func mustState(t *testing.T, r *fw.Run, err error, want fw.RunState) {
	t.Helper()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if r.State != want {
		t.Fatalf("state = %s, want %s (error %q)", r.State, want, r.Error)
	}
}

func runEvents(t *testing.T, a *fw.Agent, id string) []fw.Event {
	t.Helper()
	var out []fw.Event
	for e, err := range a.Ledger().Read(context.Background(), id, 0) {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func decidedOn(t *testing.T, a *fw.Agent, id string) fw.ApprovalDecidedData {
	t.Helper()
	for _, e := range runEvents(t, a, id) {
		if e.Type == fw.EvApprovalDecided {
			var d fw.ApprovalDecidedData
			if err := e.Decode(&d); err != nil {
				t.Fatal(err)
			}
			return d
		}
	}
	t.Fatalf("run %s has no approval decision", id)
	return fw.ApprovalDecidedData{}
}

func onlyChild(t *testing.T, a *fw.Agent, parent string) *fw.Run {
	t.Helper()
	kids, err := a.Children(context.Background(), parent)
	if err != nil || len(kids) != 1 {
		t.Fatalf("children of %s = %v, %v", parent, kids, err)
	}
	return kids[0]
}

func destructiveTool(calls *atomic.Int32) fw.Tool {
	return fw.NewTool(fw.ToolSpec{Name: "rm", Destructive: true}, func(context.Context, json.RawMessage) (fw.ToolResult, error) {
		calls.Add(1)
		return fw.TextResult("removed"), nil
	})
}

func TestSpawnTaskOneChild(t *testing.T) {
	ctx := testCtx(t)
	s := newScripted()
	s.say("plan", callTools(use("c1", "spawn_task", `{"input":"child one","instructions":"Be brief.","effort":"low","max_turns":3,"deadline":"5m"}`)), endTurn("parent done"))
	s.say("child one", endTurn("child one done"))
	a := newSpawnAgent(t, s.Model())

	r, err := a.Run(ctx, "plan")
	mustState(t, r, err, fw.StateCompleted)
	if r.Output != "parent done" {
		t.Fatalf("output %q", r.Output)
	}
	reqs := s.requestsFor("plan")
	if len(reqs) != 2 {
		t.Fatalf("parent made %d model calls", len(reqs))
	}
	res := lastToolResults(t, reqs[1])
	if len(res) != 1 || res[0].IsError || fw.TextOf(res[0].Content) != "child one done" {
		t.Fatalf("tool result = %+v", res)
	}
	child := onlyChild(t, a, r.ID)
	if child.State != fw.StateCompleted || child.Parent != r.ID || child.Depth != 1 || child.Origin != fw.OriginTool || child.Input != "child one" {
		t.Fatalf("child = %+v", child)
	}
	var q fw.RunQueuedData
	if err := runEvents(t, a, child.ID)[0].Decode(&q); err != nil {
		t.Fatal(err)
	}
	if q.Instructions != "Be brief." || q.Profile == nil || q.Profile.Effort != "low" || q.Budget == nil || q.Budget.MaxTurns != 3 || q.Budget.Deadline != 5*time.Minute {
		t.Fatalf("queued = %+v profile %+v budget %+v", q, q.Profile, q.Budget)
	}
}

func TestSpawnTaskTwoChildrenRunTogether(t *testing.T) {
	ctx := testCtx(t)
	// meet returns only when two calls are inside it at the same time, so
	// the test fails when the children run one after the other.
	var mu sync.Mutex
	arrived := 0
	both := make(chan struct{})
	meet := fw.NewTool(fw.ToolSpec{Name: "meet", ReadOnly: true, ParallelSafe: true}, func(ctx context.Context, _ json.RawMessage) (fw.ToolResult, error) {
		mu.Lock()
		arrived++
		if arrived == 2 {
			close(both)
		}
		mu.Unlock()
		select {
		case <-both:
			return fw.TextResult("met"), nil
		case <-time.After(5 * time.Second):
			return fw.ErrorResult("alone"), nil
		case <-ctx.Done():
			return fw.ToolResult{}, ctx.Err()
		}
	})
	s := newScripted()
	s.say("fanout", callTools(use("c1", "spawn_task", `{"input":"left"}`), use("c2", "spawn_task", `{"input":"right"}`)), endTurn("both"))
	s.say("left", callTools(use("l1", "meet", `{}`)), endTurn("L"))
	s.say("right", callTools(use("r1", "meet", `{}`)), endTurn("R"))
	a := newSpawnAgent(t, s.Model(), fw.WithTools(meet))

	r, err := a.Run(ctx, "fanout")
	mustState(t, r, err, fw.StateCompleted)
	res := lastToolResults(t, s.requestsFor("fanout")[1])
	if len(res) != 2 || res[0].IsError || res[1].IsError || fw.TextOf(res[0].Content) != "L" || fw.TextOf(res[1].Content) != "R" {
		t.Fatalf("tool results = %+v", res)
	}
	for _, key := range []string{"left", "right"} {
		if got := fw.TextOf(lastToolResults(t, s.requestsFor(key)[1])[0].Content); got != "met" {
			t.Fatalf("%s: meet returned %q, so the children did not run together", key, got)
		}
	}
	kids, _ := a.Children(ctx, r.ID)
	if len(kids) != 2 {
		t.Fatalf("children = %d", len(kids))
	}
}

var runIDPattern = regexp.MustCompile(`child run ([0-9A-Z]{26})`)

func TestSpawnTaskDetachThenAwait(t *testing.T) {
	ctx := testCtx(t)
	s := newScripted()
	s.say("detach", callTools(use("c1", "spawn_task", `{"input":"background","wait":false}`)))
	s.then("detach", func(req *fw.Request) *fw.Response {
		text := fw.TextOf(lastToolResults(t, req)[0].Content)
		m := runIDPattern.FindStringSubmatch(text)
		if m == nil {
			t.Errorf("no run id in %q", text)
			return endTurn("lost")
		}
		return callTools(use("c2", "await_task", `{"run_id":"`+m[1]+`"}`))
	})
	s.say("detach", endTurn("collected"))
	s.say("background", endTurn("bg done"))
	a := newSpawnAgent(t, s.Model())

	r, err := a.Run(ctx, "detach")
	mustState(t, r, err, fw.StateCompleted)
	reqs := s.requestsFor("detach")
	if len(reqs) != 3 {
		t.Fatalf("parent made %d model calls", len(reqs))
	}
	if got := fw.TextOf(lastToolResults(t, reqs[1])[0].Content); !strings.Contains(got, "await_task") {
		t.Fatalf("wait false result = %q", got)
	}
	if res := lastToolResults(t, reqs[2]); res[0].IsError || fw.TextOf(res[0].Content) != "bg done" {
		t.Fatalf("await result = %+v", res)
	}
}

func TestSpawnTaskDirect(t *testing.T) {
	ctx := testCtx(t)
	s := newScripted()
	s.say("noop", endTurn("ok"))
	s.say("background", endTurn("bg done"))
	s.say("child one", endTurn("child one done"))
	s.say("boom") // no answer, so the child fails
	a := newSpawnAgent(t, s.Model())
	parent, err := a.Run(ctx, "noop")
	mustState(t, parent, err, fw.StateCompleted)
	cfg := fw.DefaultConfig()
	spawn, await := builtinTool(t, "spawn_task", cfg), builtinTool(t, "await_task", cfg)
	pctx := runCtx(ctx, a, parent.ID)

	// wait false returns the id at once; await_task collects the result.
	res := call(t, pctx, spawn, `{"input":"background","wait":false}`)
	var st childStatus
	if err := json.Unmarshal(res.Structured, &st); err != nil || st.RunID == "" {
		t.Fatalf("structured = %s (%v)", res.Structured, err)
	}
	if got := mustOK(t, res); !strings.Contains(got, st.RunID) || !strings.Contains(got, "await_task") {
		t.Fatalf("wait false result = %q", got)
	}
	res = call(t, pctx, await, `{"run_id":"`+st.RunID+`"}`)
	if got := mustOK(t, res); got != "bg done" {
		t.Fatalf("await = %q", got)
	}
	if err := json.Unmarshal(res.Structured, &st); err != nil || st.State != "completed" || st.Turns != 1 || st.ToolCalls != 0 || st.Error != "" {
		t.Fatalf("await structured = %s", res.Structured)
	}
	got := mustOK(t, call(t, pctx, await, `{}`))
	if !strings.Contains(got, st.RunID) || !strings.Contains(got, "completed") || !strings.Contains(got, "Output: bg done") {
		t.Fatalf("list = %q", got)
	}
	mustFail(t, call(t, pctx, await, `{"run_id":"NOPE"}`), "no run with id")

	// wait true returns the output and the status.
	// The model selector of the configuration is the one the fake serves.
	res = call(t, pctx, spawn, `{"input":"child one","system":"You are brief.","tools":["spawn_*"],"skills":[],"model":"`+cfg.Model+`"}`)
	if got := mustOK(t, res); got != "child one done" {
		t.Fatalf("result = %q", got)
	}
	if err := json.Unmarshal(res.Structured, &st); err != nil || st.State != "completed" || st.Turns != 1 {
		t.Fatalf("structured = %s", res.Structured)
	}
	var q fw.RunQueuedData
	if err := runEvents(t, a, st.RunID)[0].Decode(&q); err != nil {
		t.Fatal(err)
	}
	if q.Profile == nil || q.Profile.System != "You are brief." || len(q.Profile.Tools) != 1 || q.Profile.Skills == nil || len(q.Profile.Skills) != 0 || q.Profile.Model != cfg.Model {
		t.Fatalf("profile = %+v", q.Profile)
	}

	// A child that fails gives an error result that says so.
	res = call(t, pctx, spawn, `{"input":"boom"}`)
	mustFail(t, res, "failed")
	if err := json.Unmarshal(res.Structured, &st); err != nil || st.State != "failed" || st.Error == "" {
		t.Fatalf("failed structured = %s", res.Structured)
	}

	// Input the model can fix comes back as an error result.
	mustFail(t, call(t, pctx, spawn, `{"input":"x","tools":[]}`), "tools list is empty")
	mustFail(t, call(t, pctx, spawn, `{"input":"x","deadline":"soon"}`), "not a duration")
	mustFail(t, call(t, pctx, spawn, `{"input":"x","max_turns":-1}`), "max_turns")
	mustFail(t, call(t, pctx, spawn, `{"input":"x","effort":"ultra"}`), "not valid")
	mustFail(t, call(t, pctx, spawn, `{"input":"x","model":"nonsense"}`), "not valid")
	mustFail(t, call(t, pctx, spawn, `{"input":"x","agent":"ghost"}`), "no agent profile named")
	mustFail(t, call(t, pctx, spawn, `{"input":"  "}`), "empty")
	mustFail(t, call(t, fw.WithAgent(ctx, a), spawn, `{"input":"x"}`), "no run is in this context")
	mustFail(t, call(t, fw.WithAgent(ctx, a), await, `{}`), "no run is in this context")
	mustFail(t, call(t, ctx, await, `{}`), "no agent")
}

func TestSpawnTaskRelaysApproval(t *testing.T) {
	type answer struct {
		name     string
		decision fw.ApprovalDecision
		allowed  bool
		note     string // what the child's decision note contains
	}
	for _, tc := range []answer{
		{name: "allow with content", decision: fw.ApprovalDecision{Content: json.RawMessage(`{"decision":"allow","note":"go ahead"}`)}, allowed: true, note: "go ahead"},
		{name: "allow without content", decision: fw.ApprovalDecision{Decision: "allow"}, allowed: true},
		{name: "deny with content", decision: fw.ApprovalDecision{Content: json.RawMessage(`{"decision":"deny","note":"not now"}`)}, note: "not now"},
		{name: "deny the relay", decision: fw.ApprovalDecision{Decision: "deny", Note: "too risky", Decider: "bob"}, note: "denied by bob: too risky"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testCtx(t)
			var calls atomic.Int32
			s := newScripted()
			s.say("approve", callTools(use("c1", "spawn_task", `{"input":"clean up"}`)), endTurn("parent done"))
			s.say("clean up", callTools(use("k1", "rm", `{"path":"x"}`)), endTurn("cleaned"))
			a := newSpawnAgent(t, s.Model(), fw.WithTools(destructiveTool(&calls)))

			r, err := a.Run(ctx, "approve")
			mustState(t, r, err, fw.StateInputRequired)
			child := onlyChild(t, a, r.ID)
			if child.State != fw.StateInputRequired || child.Pending == nil || child.Pending.Tool != "rm" {
				t.Fatalf("child = %+v", child)
			}
			p := r.Pending
			if p.Tool != "spawn_task" || p.CallID != "c1" || string(p.Schema) != approvalRelaySchema {
				t.Fatalf("parent pending = %+v", p)
			}
			for _, want := range []string{child.ID, "rm", `{"path":"x"}`, "rule: ask destructive"} {
				if !strings.Contains(p.Reason, want) {
					t.Fatalf("question %q lacks %q", p.Reason, want)
				}
			}
			if calls.Load() != 0 {
				t.Fatal("rm ran before the approval")
			}

			d := tc.decision
			d.ApprovalID = p.ID
			if err := a.Decide(ctx, r.ID, d); err != nil {
				t.Fatal(err)
			}
			r, err = a.Wait(ctx, r.ID)
			mustState(t, r, err, fw.StateCompleted)
			if r.Output != "parent done" {
				t.Fatalf("output %q", r.Output)
			}
			if got := fw.TextOf(lastToolResults(t, s.requestsFor("approve")[1])[0].Content); got != "cleaned" {
				t.Fatalf("parent saw %q", got)
			}
			decided := decidedOn(t, a, child.ID)
			if decided.Decider != r.ID || !strings.Contains(decided.Note, tc.note) {
				t.Fatalf("child decision = %+v", decided)
			}
			childRes := lastToolResults(t, s.requestsFor("clean up")[1])[0]
			if tc.allowed {
				if calls.Load() != 1 || decided.Decision != "allow" || childRes.IsError || fw.TextOf(childRes.Content) != "removed" {
					t.Fatalf("allowed: calls %d, decision %+v, result %+v", calls.Load(), decided, childRes)
				}
			} else {
				if calls.Load() != 0 || decided.Decision != "deny" || !childRes.IsError || !strings.Contains(fw.TextOf(childRes.Content), "denied by "+r.ID) {
					t.Fatalf("denied: calls %d, decision %+v, result %+v", calls.Load(), decided, childRes)
				}
			}
		})
	}
}

func TestSpawnTaskRelaysQuestion(t *testing.T) {
	ctx := testCtx(t)
	ask := builtinTool(t, "ask_user", fw.DefaultConfig())
	s := newScripted()
	s.say("outer", callTools(use("c1", "spawn_task", `{"input":"inner"}`)), endTurn("parent done"))
	s.say("inner", callTools(use("q1", "ask_user", `{"question":"Which color?","schema":{"type":"object","properties":{"color":{"type":"string"}}}}`)), endTurn("asked"))
	a := newSpawnAgent(t, s.Model(), fw.WithTools(ask))

	r, err := a.Run(ctx, "outer")
	mustState(t, r, err, fw.StateInputRequired)
	// The question and its schema pass through unchanged.
	if r.Pending.Reason != "Which color?" || !strings.Contains(string(r.Pending.Schema), `"color"`) {
		t.Fatalf("parent pending = %+v", r.Pending)
	}
	if err := a.Decide(ctx, r.ID, fw.ApprovalDecision{ApprovalID: r.Pending.ID, Content: json.RawMessage(`{"color":"blue"}`)}); err != nil {
		t.Fatal(err)
	}
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, fw.StateCompleted)
	if got := fw.TextOf(lastToolResults(t, s.requestsFor("inner")[1])[0].Content); got != `{"color":"blue"}` {
		t.Fatalf("child got %q", got)
	}
}

func TestSpawnTaskDeniesWhenNoCaller(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	s := newScripted()
	s.say("noop", endTurn("ok"))
	s.say("clean up", callTools(use("k1", "rm", `{"path":"x"}`)), endTurn("cleaned"))
	a := newSpawnAgent(t, s.Model(), fw.WithTools(destructiveTool(&calls)))
	parent, err := a.Run(ctx, "noop")
	mustState(t, parent, err, fw.StateCompleted)

	// The context has an agent and a run, but no asker.
	spawn := builtinTool(t, "spawn_task", fw.DefaultConfig())
	if got := mustOK(t, call(t, runCtx(ctx, a, parent.ID), spawn, `{"input":"clean up"}`)); got != "cleaned" {
		t.Fatalf("result = %q", got)
	}
	child := onlyChild(t, a, parent.ID)
	decided := decidedOn(t, a, child.ID)
	if calls.Load() != 0 || decided.Decision != "deny" || !strings.Contains(decided.Note, "no caller") {
		t.Fatalf("calls %d, decision %+v", calls.Load(), decided)
	}
}

func TestSpawnTaskLimits(t *testing.T) {
	t.Run("maxChildren", func(t *testing.T) {
		ctx := testCtx(t)
		cfg := fw.DefaultConfig()
		cfg.Home = t.TempDir()
		cfg.Spawn.MaxChildren = 1
		s := newScripted()
		s.say("noop", endTurn("ok"))
		s.say("one", endTurn("one done"))
		a := newSpawnAgent(t, s.Model(), fw.WithConfig(cfg))
		parent, err := a.Run(ctx, "noop")
		mustState(t, parent, err, fw.StateCompleted)
		spawn := builtinTool(t, "spawn_task", cfg)
		pctx := runCtx(ctx, a, parent.ID)
		mustOK(t, call(t, pctx, spawn, `{"input":"one"}`))
		res := call(t, pctx, spawn, `{"input":"two"}`)
		mustFail(t, res, "spawn limit")
		mustFail(t, res, "spawn.maxChildren")
	})
	t.Run("maxDepth", func(t *testing.T) {
		ctx := testCtx(t)
		cfg := fw.DefaultConfig()
		cfg.Home = t.TempDir()
		cfg.Spawn.MaxDepth = 1
		s := newScripted()
		s.say("deep", callTools(use("c1", "spawn_task", `{"input":"deeper"}`)), endTurn("deep done"))
		s.say("deeper", callTools(use("c2", "spawn_task", `{"input":"deepest"}`)), endTurn("deeper done"))
		a := newSpawnAgent(t, s.Model(), fw.WithConfig(cfg))
		r, err := a.Run(ctx, "deep")
		mustState(t, r, err, fw.StateCompleted)
		res := lastToolResults(t, s.requestsFor("deeper")[1])[0]
		if !res.IsError || !strings.Contains(fw.TextOf(res.Content), "spawn.maxDepth") {
			t.Fatalf("grandchild result = %+v", res)
		}
		if got := fw.TextOf(lastToolResults(t, s.requestsFor("deep")[1])[0].Content); got != "deeper done" {
			t.Fatalf("parent saw %q", got)
		}
	})
}

func TestAwaitTaskRefusesStrangers(t *testing.T) {
	ctx := testCtx(t)
	s := newScripted()
	s.say("noop", endTurn("ok"), endTurn("ok"))
	s.say("background", endTurn("bg done"))
	s.say("background2", endTurn("bg2 done"))
	a := newSpawnAgent(t, s.Model())
	runA, err := a.Run(ctx, "noop")
	mustState(t, runA, err, fw.StateCompleted)
	runB, err := a.Run(ctx, "noop")
	mustState(t, runB, err, fw.StateCompleted)
	cfg := fw.DefaultConfig()
	spawn, await := builtinTool(t, "spawn_task", cfg), builtinTool(t, "await_task", cfg)
	actx, bctx := runCtx(ctx, a, runA.ID), runCtx(ctx, a, runB.ID)

	mustFail(t, call(t, actx, await, `{"run_id":"`+runB.ID+`"}`), "not a child")
	mustFail(t, call(t, actx, await, `{"run_id":"`+runA.ID+`"}`), "not a child")

	// A grandchild is a descendant; a child of another run is not.
	var child, grandchild childStatus
	if err := json.Unmarshal(call(t, actx, spawn, `{"input":"background","wait":false}`).Structured, &child); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(call(t, runCtx(ctx, a, child.RunID), spawn, `{"input":"background2","wait":false}`).Structured, &grandchild); err != nil {
		t.Fatal(err)
	}
	if got := mustOK(t, call(t, actx, await, `{"run_id":"`+grandchild.RunID+`"}`)); got != "bg2 done" {
		t.Fatalf("grandchild = %q", got)
	}
	mustFail(t, call(t, bctx, await, `{"run_id":"`+child.RunID+`"}`), "not a child")
	if got := mustOK(t, call(t, bctx, await, `{}`)); !strings.Contains(got, "no child runs") {
		t.Fatalf("list of B = %q", got)
	}
}

func TestSpawnTaskCancelsChildWhenParentStops(t *testing.T) {
	ctx := testCtx(t)
	started := make(chan struct{}, 1)
	slow := fw.NewTool(fw.ToolSpec{Name: "slow", ReadOnly: true}, func(ctx context.Context, _ json.RawMessage) (fw.ToolResult, error) {
		started <- struct{}{}
		<-ctx.Done()
		return fw.ToolResult{}, ctx.Err()
	})
	s := newScripted()
	s.say("noop", endTurn("ok"))
	s.say("linger", callTools(use("s1", "slow", `{}`)), endTurn("never"))
	a := newSpawnAgent(t, s.Model(), fw.WithTools(slow))
	parent, err := a.Run(ctx, "noop")
	mustState(t, parent, err, fw.StateCompleted)

	spawn := builtinTool(t, "spawn_task", fw.DefaultConfig())
	cctx, cancel := context.WithCancel(runCtx(ctx, a, parent.ID))
	done := make(chan fw.ToolResult, 1)
	go func() {
		res, _ := spawn.Call(cctx, json.RawMessage(`{"input":"linger"}`))
		done <- res
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("the child never started")
	}
	cancel()
	res := <-done
	mustFail(t, res, "canceled")
	child := onlyChild(t, a, parent.ID)
	if child.State != fw.StateCanceled {
		t.Fatalf("child state = %s", child.State)
	}
}
