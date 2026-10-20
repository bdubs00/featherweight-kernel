package fw

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// loopEvents returns the loop events of a run with their actions.
func loopEvents(t *testing.T, a *Agent, id string) []LoopDetectedData {
	t.Helper()
	var out []LoopDetectedData
	for _, e := range ledgerEvents(t, a.Ledger(), id) {
		if e.Type != EvLoopDetected {
			continue
		}
		var d LoopDetectedData
		if err := e.Decode(&d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func TestLoopGuardWarnsThenFails(t *testing.T) {
	ctx := testCtx(t)
	same := func(id string) *Response { return callTools(use(id, "echo", `{"text":"a"}`)) }
	s := newScript(same("c1"), same("c2"), same("c3"), endTurn("never"))
	a := newTestAgent(t, s.Model(), WithTools(echoTool(nil)))
	r, err := a.Run(ctx, "loop", WithRunBudget(Budget{MaxRepeats: 3}))
	mustState(t, r, err, StateFailed)
	if !strings.Contains(r.Error, "maxRepeats") || len(s.requests()) != 3 {
		t.Fatalf("run = %+v, model calls = %d", r, len(s.requests()))
	}
	// The third request carries the warning as a system message after the
	// tool results of the second call.
	msgs := s.requests()[2].Messages
	last := msgs[len(msgs)-1]
	if last.Role != RoleSystem || !strings.Contains(TextOf(last.Content), "2 times in a row") {
		t.Fatalf("last message = %+v", last)
	}
	evs := loopEvents(t, a, r.ID)
	if len(evs) != 2 || evs[0].Action != "warned" || evs[0].Count != 2 || evs[1].Action != "failed" || evs[1].Count != 3 || evs[1].Tool != "echo" || evs[1].Period != 1 {
		t.Fatalf("loop events = %+v", evs)
	}
	var d BudgetExceededData
	firstOf(t, ledgerEvents(t, a.Ledger(), r.ID), EvBudgetExceeded, &d)
	if d.Limit != "maxRepeats" || d.Value != 3 {
		t.Fatalf("exceeded = %+v", d)
	}
}

func TestLoopGuardCycle(t *testing.T) {
	ctx := testCtx(t)
	ab := func(i int) *Response {
		if i%2 == 0 {
			return callTools(use("a"+string(rune('0'+i)), "echo", `{"text":"a"}`))
		}
		return callTools(use("b"+string(rune('0'+i)), "echo", `{"text":"b"}`))
	}
	s := newScript(ab(0), ab(1), ab(2), ab(3), ab(4), ab(5), endTurn("never"))
	a := newTestAgent(t, s.Model(), WithTools(echoTool(nil)))
	r, err := a.Run(ctx, "loop", WithRunBudget(Budget{MaxRepeats: 3}))
	mustState(t, r, err, StateFailed)
	evs := loopEvents(t, a, r.ID)
	if len(s.requests()) != 6 || len(evs) != 2 || evs[1].Period != 2 || evs[1].Count != 3 {
		t.Fatalf("model calls = %d, loop events = %+v", len(s.requests()), evs)
	}
}

func TestLoopGuardResetsOnADifferentCall(t *testing.T) {
	ctx := testCtx(t)
	same := func(id string) *Response { return callTools(use(id, "echo", `{"text":"a"}`)) }
	other := callTools(use("x", "echo", `{"text":"other"}`))
	s := newScript(same("c1"), same("c2"), other, same("c3"), same("c4"), endTurn("done"))
	a := newTestAgent(t, s.Model(), WithTools(echoTool(nil)))
	r, err := a.Run(ctx, "work", WithRunBudget(Budget{MaxRepeats: 3}))
	mustState(t, r, err, StateCompleted)
	for _, e := range loopEvents(t, a, r.ID) {
		if e.Action != "warned" {
			t.Fatalf("loop event = %+v", e)
		}
	}
}

func TestLoopGuardOffAtZero(t *testing.T) {
	ctx := testCtx(t)
	same := func(id string) *Response { return callTools(use(id, "echo", `{"text":"a"}`)) }
	s := newScript(same("c1"), same("c2"), same("c3"), same("c4"), endTurn("done"))
	cfg := DefaultConfig()
	cfg.Budget.MaxRepeats = 0
	a := newTestAgent(t, s.Model(), WithConfig(cfg), WithTools(echoTool(nil)))
	r, err := a.Run(ctx, "work")
	mustState(t, r, err, StateCompleted)
	if n := len(loopEvents(t, a, r.ID)); n != 0 {
		t.Fatalf("loop events = %d", n)
	}
}

func TestRepeats(t *testing.T) {
	cases := []struct {
		seq           []uint64
		period, count int
	}{
		{nil, 1, 1},
		{[]uint64{1}, 1, 1},
		{[]uint64{1, 1, 1}, 1, 3},
		{[]uint64{2, 1, 1}, 1, 2},
		{[]uint64{1, 2, 1, 2}, 2, 2},
		{[]uint64{1, 2, 3, 1, 2, 3, 1, 2, 3}, 3, 3},
		{[]uint64{1, 2, 3, 1, 2}, 1, 1},
		{[]uint64{1, 1, 1, 1}, 1, 4}, // period 2 also fits twice, but period 1 fits four times
	}
	for _, c := range cases {
		p, n := repeats(c.seq)
		if p != c.period || n != c.count {
			t.Errorf("repeats(%v) = %d, %d, want %d, %d", c.seq, p, n, c.period, c.count)
		}
	}
	if fingerprint(use("1", "t", `{"a":1,"b":2}`)) != fingerprint(use("2", "t", `{"b":2,"a":1}`)) {
		t.Error("the fingerprint depends on the key order")
	}
	if fingerprint(use("1", "t", `{"a":1}`)) == fingerprint(use("1", "u", `{"a":1}`)) {
		t.Error("the fingerprint ignores the tool name")
	}
}

func TestMaxCallsPerTurn(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	s := newScript(callTools(use("c1", "echo", `{"text":"1"}`), use("c2", "echo", `{"text":"2"}`), use("c3", "echo", `{"text":"3"}`)), endTurn("done"))
	a := newTestAgent(t, s.Model(), WithTools(echoTool(&calls)))
	r, err := a.Run(ctx, "go", WithRunBudget(Budget{MaxCallsPerTurn: 2}))
	mustState(t, r, err, StateCompleted)
	res := lastToolResults(t, s.requests()[1])
	if calls.Load() != 2 || len(res) != 3 || res[2].IsError != true || !strings.Contains(TextOf(res[2].Content), "maxCallsPerTurn") || res[1].IsError {
		t.Fatalf("calls = %d, results = %+v", calls.Load(), res)
	}
}

func TestMaxToolCalls(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	s := newScript(callTools(use("c1", "echo", `{"text":"1"}`), use("c2", "echo", `{"text":"2"}`), use("c3", "echo", `{"text":"3"}`)), endTurn("the report"))
	a := newTestAgent(t, s.Model(), WithTools(echoTool(&calls)))
	r, err := a.Run(ctx, "go", WithRunBudget(Budget{MaxToolCalls: 2}))
	mustState(t, r, err, StateFailed)
	// The second model call is the final report, which runs no tool.
	if calls.Load() != 2 || r.Error != "budget exceeded: maxToolCalls" || r.Usage.ToolCalls != 2 || len(s.requests()) != 2 || r.Output != "the report" {
		t.Fatalf("calls = %d, run = %+v, model calls = %d", calls.Load(), r, len(s.requests()))
	}
}

func TestSystemPromptComposition(t *testing.T) {
	ctx := testCtx(t)
	s := newScript(endTurn("a"), endTurn("b"))
	probe := NewTool(ToolSpec{Name: "probe", ReadOnly: true}, func(context.Context, json.RawMessage) (ToolResult, error) { return TextResult(""), nil })
	a := newTestAgent(t, s.Model(), WithTools(echoTool(nil), probe))
	r, err := a.Run(ctx, "plain")
	mustState(t, r, err, StateCompleted)
	if req := s.requests()[0]; req.System != DefaultSystemPrompt || len(req.Tools) != 2 {
		t.Fatalf("request = %+v", req)
	}
	r, err = a.Run(ctx, "shaped",
		WithRunProfile(AgentProfile{System: "Be terse.", Instructions: []string{"Rule one.", " "}, Tools: []string{"ec*"}}),
		WithInstructions("Today only."))
	mustState(t, r, err, StateCompleted)
	req := s.requests()[1]
	want := "Be terse." + instructionsHeading + "Rule one." + runHeading + "Today only."
	if req.System != want || len(req.Tools) != 1 || req.Tools[0].Name != "echo" {
		t.Fatalf("system = %q, tools = %+v", req.System, req.Tools)
	}
	var q RunQueuedData
	firstOf(t, ledgerEvents(t, a.Ledger(), r.ID), EvRunQueued, &q)
	if q.Profile == nil || q.Profile.System != "Be terse." || q.Instructions != "Today only." {
		t.Fatalf("queued = %+v", q)
	}
}

func TestInlineProfileChecks(t *testing.T) {
	ctx := testCtx(t)
	a := newTestAgent(t, newScript().Model())
	bad := []AgentProfile{
		{Effort: "turbo"},
		{Model: "nonsense"},
		{Budget: &Budget{MaxTurns: -1}},
		{System: strings.Repeat("x", maxInstructionBytes+1)},
	}
	for _, p := range bad {
		if _, err := a.Start(ctx, "x", WithRunProfile(p)); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("profile %+v: err = %v", p, err)
		}
	}
	if _, err := a.Start(ctx, "x", WithInstructions(strings.Repeat("x", maxInstructionBytes+1))); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("long instructions: err = %v", err)
	}
}

// routingModel answers by the text of the first user message of a request,
// so a parent and its children get different responses in one test.
func routingModel(routes map[string][]*Response) Model {
	queues := map[string][]*Response{}
	for k, v := range routes {
		queues[k] = append([]*Response(nil), v...)
	}
	var mu chan struct{} = make(chan struct{}, 1)
	return ModelFunc{StreamFn: func(ctx context.Context, req *Request, _ func(Delta)) (*Response, error) {
		mu <- struct{}{}
		defer func() { <-mu }()
		key := TextOf(req.Messages[0].Content)
		q := queues[key]
		if len(q) == 0 {
			return nil, errors.New("routing model: no response left for " + key)
		}
		queues[key] = q[1:]
		return q[0], nil
	}}
}

func TestChildToolsAndPolicyFollowTheParent(t *testing.T) {
	ctx := testCtx(t)
	var rmCalls atomic.Int32
	probe := NewTool(ToolSpec{Name: "probe", ReadOnly: true}, func(context.Context, json.RawMessage) (ToolResult, error) { return TextResult("probed"), nil })
	m := routingModel(map[string][]*Response{
		"parent": {endTurn("parent done")},
		"child":  {callTools(use("p1", "probe", `{}`), use("r1", "rm", `{}`)), endTurn("child done")},
	})
	a := newTestAgent(t, m, WithTools(echoTool(nil), probe, destructiveTool(&rmCalls)),
		WithProfile(AgentProfile{Name: "narrow", Tools: []string{"echo", "rm"}, Policy: &Rules{Deny: []string{"rm"}}}))
	p, err := a.Run(ctx, "parent", AsAgent("narrow"))
	mustState(t, p, err, StateCompleted)
	// The child asks for every tool and an allow-all policy, and gets neither.
	c, err := a.Run(ctx, "child", WithParent(p.ID), WithRunProfile(AgentProfile{Tools: []string{"*"}, Policy: &Rules{Default: "allow", Allow: []string{"rm"}}}))
	mustState(t, c, err, StateCompleted)
	if c.Depth != 1 || c.Parent != p.ID {
		t.Fatalf("child = %+v", c)
	}
	var started RunStartedData
	firstOf(t, ledgerEvents(t, a.Ledger(), c.ID), EvRunStarted, &started)
	if strings.Join(started.Tools, ",") != "echo,rm" {
		t.Fatalf("child tools = %v", started.Tools)
	}
	var results []Block
	for _, e := range ledgerEvents(t, a.Ledger(), c.ID) {
		if e.Type == EvToolResponded {
			var d ToolRespondedData
			_ = e.Decode(&d)
			results = append(results, Block{ToolUseID: d.CallID, Content: d.Content, IsError: d.IsError})
		}
	}
	if len(results) != 2 || !strings.Contains(TextOf(results[0].Content), "unknown tool") || !strings.Contains(TextOf(results[1].Content), "denied by policy") || rmCalls.Load() != 0 {
		t.Fatalf("child results = %+v, rm calls = %d", results, rmCalls.Load())
	}
	kids, err := a.Children(ctx, p.ID)
	if err != nil || len(kids) != 1 || kids[0].ID != c.ID {
		t.Fatalf("children = %+v, %v", kids, err)
	}
}

func TestSpawnLimits(t *testing.T) {
	ctx := testCtx(t)
	cfg := DefaultConfig()
	cfg.Spawn = SpawnConfig{MaxDepth: 1, MaxChildren: 1}
	a := newTestAgent(t, newScript(endTurn("1"), endTurn("2")).Model(), WithConfig(cfg))
	p, err := a.Run(ctx, "parent")
	mustState(t, p, err, StateCompleted)
	c, err := a.Run(ctx, "child", WithParent(p.ID))
	mustState(t, c, err, StateCompleted)
	if _, err := a.Start(ctx, "grandchild", WithParent(c.ID)); !errors.Is(err, ErrSpawnLimit) {
		t.Fatalf("grandchild = %v", err)
	}
	if _, err := a.Start(ctx, "second child", WithParent(p.ID)); !errors.Is(err, ErrSpawnLimit) {
		t.Fatalf("second child = %v", err)
	}
}

func TestCancelCascadesToChildren(t *testing.T) {
	ctx := testCtx(t)
	started := make(chan struct{}, 4)
	a := newTestAgent(t, blockingModel(started))
	p, err := a.Start(ctx, "parent")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	c, err := a.Start(ctx, "child", WithParent(p.ID))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := a.Cancel(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	got, err := a.Wait(ctx, c.ID)
	mustState(t, got, err, StateCanceled)
}

func TestChildBudgetIsCappedByParent(t *testing.T) {
	ctx := testCtx(t)
	m := routingModel(map[string][]*Response{
		"parent": {endTurn("parent done")},
		"child":  {callTools(use("c1", "echo", `{"text":"a"}`)), endTurn("never")},
	})
	a := newTestAgent(t, m, WithTools(echoTool(nil)))
	p, err := a.Run(ctx, "parent", WithRunBudget(Budget{MaxTurns: 1}))
	mustState(t, p, err, StateCompleted)
	c, err := a.Run(ctx, "child", WithParent(p.ID), WithRunBudget(Budget{MaxTurns: 5}))
	mustState(t, c, err, StateFailed)
	if c.Error != "budget exceeded: maxTurns" {
		t.Fatalf("child = %+v", c)
	}
}

func TestChildDeadlineFollowsTheParent(t *testing.T) {
	ctx := testCtx(t)
	l := NewMemLedger()
	id := "old-parent"
	// The parent worked for two hours in the past of the ledger, so it used
	// up its deadline of one hour, and the child inherits that.
	t0 := time.Now().Add(-3 * time.Hour)
	at := func(d time.Duration, e Event) Event { e.Time = t0.Add(d); return e }
	seed(t, l, id,
		at(0, MustEvent(id, EvRunQueued, RunQueuedData{Agent: "default", Input: "parent", Budget: &Budget{Deadline: time.Hour}})),
		at(time.Second, MustEvent(id, EvRunStarted, RunStartedData{})),
		at(time.Second, MustEvent(id, EvTurnStarted, TurnData{Turn: 1})),
		at(time.Second, MustEvent(id, EvModelRequested, ModelRequestedData{})),
		at(2*time.Hour, MustEvent(id, EvModelResponded, ModelRespondedData{Content: []Block{Text("done")}, StopReason: StopEndTurn})),
		at(2*time.Hour, MustEvent(id, EvTurnFinished, TurnData{Turn: 1})),
		at(2*time.Hour, MustEvent(id, EvRunFinished, RunFinishedData{Outcome: OutcomeCompleted, Output: "done"})),
	)
	a := newTestAgent(t, newScript(endTurn("never")).Model(), WithLedger(l))
	c, err := a.Run(ctx, "child", WithParent(id))
	mustState(t, c, err, StateFailed)
	if c.Error != "budget exceeded: deadline" {
		t.Fatalf("child = %+v", c)
	}
}

func TestChildRunsTakeNoSlot(t *testing.T) {
	ctx := testCtx(t)
	cfg := DefaultConfig()
	cfg.MaxRuns = 1
	started := make(chan struct{}, 1)
	m := ModelFunc{StreamFn: func(ctx context.Context, req *Request, _ func(Delta)) (*Response, error) {
		if TextOf(req.Messages[0].Content) == "child" {
			return endTurn("child done"), nil
		}
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	a := newTestAgent(t, m, WithConfig(cfg))
	p, err := a.Start(ctx, "parent")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	c, err := a.Run(ctx, "child", WithParent(p.ID))
	mustState(t, c, err, StateCompleted)
	if err := a.Cancel(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
}

func TestBudgetCapAndMerge(t *testing.T) {
	child := Budget{MaxTurns: 50, MaxTokens: 0, Deadline: time.Hour, MaxRepeats: 2}
	parent := Budget{MaxTurns: 10, MaxTokens: 100, Deadline: time.Minute, MaxRepeats: 5}
	got := child.Cap(parent)
	want := Budget{MaxTurns: 10, MaxTokens: 100, Deadline: time.Minute, MaxRepeats: 2}
	if got != want {
		t.Fatalf("Cap = %+v, want %+v", got, want)
	}
	if m := (Budget{MaxTurns: 1}).Merge(Budget{MaxTurns: 9, MaxToolCalls: 3, MaxCallsPerTurn: 4, MaxRepeats: 5}); m != (Budget{MaxTurns: 1, MaxToolCalls: 3, MaxCallsPerTurn: 4, MaxRepeats: 5}) {
		t.Fatalf("Merge = %+v", m)
	}
}

func TestMergeRulesAndOverlay(t *testing.T) {
	a := Rules{Default: "allow", Allow: []string{"x"}, Ask: []string{"destructive"}}
	b := Rules{Default: "ask", Deny: []string{"rm"}, Ask: []string{"destructive", "send"}}
	m := MergeRules(a, b)
	// The merged rules hold both sides as clauses. The verdict of a call is
	// exactly the stricter verdict of the two sides: x runs on a and needs
	// an answer on b, so the merged rules ask.
	if d := decideRules(t, m, ToolCall{Spec: ToolSpec{Name: "x"}}); d.Verdict != VerdictAsk {
		t.Fatalf("merged x = %s, want ask", d.Verdict)
	}
	if d := decideRules(t, m, ToolCall{Spec: ToolSpec{Name: "rm"}}); d.Verdict != VerdictDeny {
		t.Fatalf("merged rm = %s, want deny", d.Verdict)
	}
	if d := decideRules(t, m, ToolCall{Spec: ToolSpec{Name: "send"}}); d.Verdict != VerdictAsk {
		t.Fatalf("merged send = %s, want ask", d.Verdict)
	}
	base := AgentProfile{Name: "base", System: "s", Instructions: []string{"i1"}, Tools: []string{"a"}, Policy: &a, Budget: &Budget{MaxTurns: 3, Deadline: time.Hour}}
	over := AgentProfile{Instructions: []string{"i2"}, Tools: []string{"b"}, Model: "fast", Policy: &b, Budget: &Budget{MaxTurns: 9}, Skills: []string{}}
	got := base.Overlay(over)
	// The overlay policy is the merged rules, and it cannot loosen a
	// verdict of b: send stays ask and destructive stays ask.
	if got.Name != "base" || got.System != "s" || strings.Join(got.Instructions, ",") != "i1,i2" || strings.Join(got.Tools, ",") != "b" ||
		got.Model != "fast" || got.Budget.MaxTurns != 9 || got.Budget.Deadline != time.Hour || got.Skills == nil || len(got.Skills) != 0 {
		t.Fatalf("overlay = %+v", got)
	}
	if p := NewRulesPolicy(*got.Policy); p.Decide(context.Background(), ToolCall{Spec: ToolSpec{Name: "send"}}).Verdict != VerdictAsk ||
		p.Decide(context.Background(), ToolCall{Spec: ToolSpec{Name: "destructive", Destructive: true}}).Verdict != VerdictAsk ||
		p.Decide(context.Background(), ToolCall{Spec: ToolSpec{Name: "rm"}}).Verdict != VerdictDeny {
		t.Fatalf("overlay policy = %+v", *got.Policy)
	}
}

func TestLimitTools(t *testing.T) {
	big := NewTool(ToolSpec{Name: "big", ReadOnly: true}, func(_ context.Context, in json.RawMessage) (ToolResult, error) {
		return TextResult(strings.Repeat("ab", 500) + "END"), nil
	})
	sleepy := NewTool(ToolSpec{Name: "sleepy", ReadOnly: true}, func(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
		<-ctx.Done()
		return ToolResult{}, ctx.Err()
	})
	limits := ToolLimits{Limit: Limit{MaxInputBytes: 10, MaxOutputBytes: 300, Timeout: 20 * time.Millisecond}, PerTool: map[string]Limit{"sle*": {Timeout: time.Second}}}
	mw := LimitTools(limits)
	ctx := context.Background()

	res, err := mw(big).Call(ctx, json.RawMessage(`{}`))
	if err != nil || res.IsError {
		t.Fatalf("big = %+v, %v", res, err)
	}
	text := TextOf(res.Content)
	if len(text) > 300+32 || !strings.Contains(text, "bytes cut here") || !strings.HasPrefix(text, "abab") || !strings.HasSuffix(text, "END") {
		t.Fatalf("cut text (%d bytes) = %q", len(text), text)
	}
	res, _ = mw(big).Call(ctx, json.RawMessage(`{"text":"too long"}`))
	if !res.IsError || !strings.Contains(TextOf(res.Content), "the limit is 10 bytes") {
		t.Fatalf("input result = %+v", res)
	}
	if got := limits.For("sleepy"); got.Timeout != time.Second || got.MaxOutputBytes != 300 {
		t.Fatalf("For(sleepy) = %+v", got)
	}
	fast := LimitTools(ToolLimits{Limit: Limit{Timeout: 20 * time.Millisecond}})
	res, err = fast(sleepy).Call(ctx, json.RawMessage(`{}`))
	if err != nil || !res.IsError || !strings.Contains(TextOf(res.Content), "did not finish") {
		t.Fatalf("timeout result = %+v, %v", res, err)
	}

	// The configuration applies the limits to every tool of the agent.
	tctx := testCtx(t)
	s := newScript(callTools(use("c1", "big", `{}`)), endTurn("done"))
	cfg := DefaultConfig()
	cfg.Tools.Limits = ToolLimits{Limit: Limit{MaxOutputBytes: 100}}
	a := newTestAgent(t, s.Model(), WithConfig(cfg), WithTools(big))
	r, err := a.Run(tctx, "go")
	mustState(t, r, err, StateCompleted)
	if got := TextOf(lastToolResults(t, s.requests()[1])[0].Content); !strings.Contains(got, "bytes cut here") || len(got) > 320 {
		t.Fatalf("result through the agent = %q", got)
	}
}

func TestCutMiddle(t *testing.T) {
	s := strings.Repeat("é", 1000)
	got := CutMiddle(s, 400, 400)
	if len(got) > 420 || !strings.Contains(got, "bytes cut here") || !strings.HasPrefix(got, "éé") || !strings.HasSuffix(got, "éé") {
		t.Fatalf("cut = %q", got)
	}
	if CutMiddle("short", 400, 400) != "short" {
		t.Fatal("a short text changed")
	}
}

func TestPolicyFieldRules(t *testing.T) {
	call := func(tool, input string) ToolCall {
		return ToolCall{Spec: ToolSpec{Name: tool}, Input: json.RawMessage(input)}
	}
	cases := []struct {
		rule  string
		call  ToolCall
		match bool
	}{
		{"files: path=*.env", call("files", `{"command":"view","path":"app/.env"}`), true},
		{"files: path=*.env", call("files", `{"command":"view","path":"app/main.go"}`), false},
		{"files: path=*.env", call("files", `{"command":"view"}`), false},
		{"bash: FOO=bar*", call("bash", `{"command":"FOO=bar make"}`), true},
		{"bash: git push*", call("bash", `{"command":"git push origin"}`), true},
		{"bash: *rm -rf*", call("bash", `{"command":"cd /x && rm -rf ./build"}`), true},
		{"bash: git ??sh*", call("bash", `{"command":"git push"}`), true},
		{"bash: git push", call("bash", `{"command":"git pushy"}`), false},
		{"http: url=https://internal.*", call("http", `{"url":"https://internal.example/x"}`), true},
		{"*: path=/etc/*", call("files", `{"path":"/etc/passwd"}`), true},
	}
	for _, c := range cases {
		if got := ruleMatches(c.rule, c.call, false); got != c.match {
			t.Errorf("rule %q on %s: %v, want %v", c.rule, c.call.Input, got, c.match)
		}
	}
	p := NewRulesPolicy(Rules{Deny: []string{"files: path=*.env"}})
	if d := p.Decide(context.Background(), call("files", `{"path":".env"}`)); d.Verdict != VerdictDeny {
		t.Fatalf("decision = %+v", d)
	}
}

func TestPolicyResolvedFields(t *testing.T) {
	resolved := func(input, res string) ToolCall {
		return ToolCall{Spec: ToolSpec{Name: "files"}, Input: json.RawMessage(input), Resolved: json.RawMessage(res)}
	}
	p := NewRulesPolicy(Rules{
		Default: "ask",
		Allow:   []string{"files: path=docs/*"},
		Deny:    []string{"files: path=secrets/*"},
	})
	cases := []struct {
		call ToolCall
		want Verdict
	}{
		// A deny rule sees the resolved path behind "./", an absolute path, and a link.
		{resolved(`{"path":"./secrets/a"}`, `{"path":"secrets/a"}`), VerdictDeny},
		{resolved(`{"path":"/work/secrets/a"}`, `{"path":"secrets/a"}`), VerdictDeny},
		{resolved(`{"path":"docs/link/a"}`, `{"path":"secrets/a"}`), VerdictDeny},
		// An allow rule sees only the resolved path.
		{resolved(`{"path":"docs/../notes.txt"}`, `{"path":"notes.txt"}`), VerdictAsk},
		{resolved(`{"path":"/work/docs/a.md"}`, `{"path":"docs/a.md"}`), VerdictAllow},
		// Without a resolved input, the rules match the input of the model.
		{ToolCall{Spec: ToolSpec{Name: "files"}, Input: json.RawMessage(`{"path":"docs/a.md"}`)}, VerdictAllow},
	}
	for _, c := range cases {
		if d := p.Decide(context.Background(), c.call); d.Verdict != c.want {
			t.Errorf("%s resolved to %s: %s (%s), want %s", c.call.Input, c.call.Resolved, d.Verdict, d.Reason, c.want)
		}
	}
}

// resolvingTool is a tool that resolves every path to "secrets/key".
type resolvingTool struct{ Tool }

func (resolvingTool) Resolve(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{"path":"secrets/key"}`), nil
}

func TestKernelPassesResolvedInput(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	inner := NewTool(ToolSpec{Name: "files", ReadOnly: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		calls.Add(1)
		return TextResult("opened"), nil
	})
	s := newScript(callTools(use("c1", "files", `{"path":"link"}`)), endTurn("done"))
	cfg := DefaultConfig()
	cfg.Tools.Limits = ToolLimits{Limit: Limit{MaxOutputBytes: 1000}} // a middleware between the kernel and the resolver
	a := newTestAgent(t, s.Model(), WithConfig(cfg), WithTools(resolvingTool{inner}),
		WithPolicy(NewRulesPolicy(Rules{Deny: []string{"files: path=secrets/*"}})))
	r, err := a.Run(ctx, "read the link")
	mustState(t, r, err, StateCompleted)
	if calls.Load() != 0 {
		t.Fatal("the call ran through a link to a denied path")
	}
}

func writeSkill(t *testing.T, root, dir, front, body string) {
	t.Helper()
	d := filepath.Join(root, dir)
	if err := os.MkdirAll(filepath.Join(d, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, SkillFile), []byte(front+body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "scripts", "run.sh"), []byte("echo hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestSkills(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "deploy", "---\nname: deploy\ndescription: \"Ship a release.\"\n---\n", "# Deploy\n\nSteps.\n")
	writeSkill(t, root, "review-code", "", "# Review\n")
	if err := os.MkdirAll(filepath.Join(root, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	skills, err := LoadSkills(root, filepath.Join(root, "missing"))
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 2 || skills[0].Name != "deploy" || skills[0].Description != "Ship a release." || skills[1].Name != "review-code" || skills[1].Description != "" {
		t.Fatalf("skills = %+v", skills)
	}
	body, err := skills[0].Body()
	if err != nil || !strings.Contains(body, "Steps.") {
		t.Fatalf("body = %q, %v", body, err)
	}
	files, err := skills[0].Files()
	if err != nil || strings.Join(files, ",") != "scripts/run.sh" {
		t.Fatalf("files = %v, %v", files, err)
	}
	if idx := SkillsIndex(skills, nil); !strings.Contains(idx, "- deploy: Ship a release.") || !strings.Contains(idx, "- review-code") {
		t.Fatalf("index = %q", idx)
	}
	if idx := SkillsIndex(skills, []string{}); idx != "" {
		t.Fatalf("index with no skills allowed = %q", idx)
	}
	if idx := SkillsIndex(skills, []string{"dep*"}); strings.Contains(idx, "review") {
		t.Fatalf("index with a glob = %q", idx)
	}

	ctx := testCtx(t)
	s := newScript(endTurn("ok"), endTurn("ok"))
	cfg := DefaultConfig()
	cfg.Tools.Skills = []string{root}
	a := newTestAgent(t, s.Model(), WithConfig(cfg))
	r, err := a.Run(ctx, "go")
	mustState(t, r, err, StateCompleted)
	if sys := s.requests()[0].System; !strings.Contains(sys, skillsHeading) || !strings.Contains(sys, "- deploy: Ship a release.") {
		t.Fatalf("system = %q", sys)
	}
	r, err = a.Run(ctx, "go", WithRunProfile(AgentProfile{Skills: []string{}}))
	mustState(t, r, err, StateCompleted)
	if sys := s.requests()[1].System; strings.Contains(sys, skillsHeading) {
		t.Fatalf("system with no skills = %q", sys)
	}
	if sk, ok := a.SkillNamed("deploy"); !ok || sk.Dir != filepath.Join(root, "deploy") {
		t.Fatalf("SkillNamed = %+v, %v", sk, ok)
	}
}
