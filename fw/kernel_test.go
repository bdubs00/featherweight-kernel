package fw

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// scriptModel returns canned responses in order and records every request.
// A compaction request gets the summary without using the script.
type scriptModel struct {
	mu      sync.Mutex
	resps   []*Response
	reqs    []*Request
	summary string
}

func newScript(resps ...*Response) *scriptModel {
	return &scriptModel{resps: resps, summary: "SUMMARY"}
}

func (s *scriptModel) Model() Model {
	return ModelFunc{StreamFn: func(_ context.Context, req *Request, _ func(Delta)) (*Response, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if n := len(req.Messages); n > 0 && TextOf(req.Messages[n-1].Content) == compactPrompt {
			return &Response{Content: []Block{Text(s.summary)}, StopReason: StopEndTurn}, nil
		}
		s.reqs = append(s.reqs, req)
		if len(s.resps) == 0 {
			return nil, errors.New("script: no response left")
		}
		r := s.resps[0]
		s.resps = s.resps[1:]
		return r, nil
	}}
}

func (s *scriptModel) requests() []*Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*Request(nil), s.reqs...)
}

func endTurn(text string) *Response {
	return &Response{Content: []Block{Text(text)}, StopReason: StopEndTurn, Usage: Usage{InputTokens: 10, OutputTokens: 5}}
}

func use(id, name, input string) Block {
	return Block{Type: BlockToolUse, ID: id, Name: name, Input: json.RawMessage(input)}
}

func callTools(uses ...Block) *Response {
	return &Response{
		Content:    append([]Block{{Type: BlockOpaque, Provider: "fake", Raw: json.RawMessage(`{"thinking":"x"}`)}, Text("calling")}, uses...),
		StopReason: StopToolUse,
		Usage:      Usage{InputTokens: 10, OutputTokens: 5},
	}
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestAgent(t *testing.T, m Model, opts ...Option) *Agent {
	t.Helper()
	t.Setenv("FW_ATESPACE", "")
	base := []Option{
		WithoutDefaults(),
		WithAuthn(NoAuthn{}),
		WithModel(m),
		WithLedger(NewMemLedger()),
		WithBus(NewMemBus()),
		WithLogger(quietLogger()),
	}
	a, err := New(append(base, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// A scripted model answers in order, so a test run makes no title
	// call. title_test.go turns model titles on where it needs them.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	return a
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func ledgerEvents(t *testing.T, l Ledger, id string) []Event {
	t.Helper()
	evs, err := readAll(context.Background(), l, id)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func eventTypes(evs []Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}

func countType(evs []Event, typ string) int {
	n := 0
	for _, e := range evs {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func firstOf(t *testing.T, evs []Event, typ string, v any) Event {
	t.Helper()
	for _, e := range evs {
		if e.Type == typ {
			if v != nil {
				if err := e.Decode(v); err != nil {
					t.Fatal(err)
				}
			}
			return e
		}
	}
	t.Fatalf("no %s event in %v", typ, eventTypes(evs))
	return Event{}
}

func mustState(t *testing.T, r *Run, err error, want RunState) {
	t.Helper()
	if err != nil {
		t.Fatalf("run %v: %v", r, err)
	}
	if r.State != want {
		t.Fatalf("state = %s, want %s (error %q)", r.State, want, r.Error)
	}
}

// lastToolResult returns the tool_result blocks of the last message of a request.
func lastToolResults(t *testing.T, req *Request) []Block {
	t.Helper()
	m := req.Messages[len(req.Messages)-1]
	if m.Role != RoleUser {
		t.Fatalf("last message role = %s", m.Role)
	}
	for _, b := range m.Content {
		if b.Type != BlockToolResult {
			t.Fatalf("last message holds a %s block", b.Type)
		}
	}
	return m.Content
}

func echoTool(calls *atomic.Int32) Tool {
	return NewTool(ToolSpec{Name: "echo", ReadOnly: true}, func(_ context.Context, in json.RawMessage) (ToolResult, error) {
		if calls != nil {
			calls.Add(1)
		}
		var v struct{ Text string }
		_ = json.Unmarshal(in, &v)
		return TextResult(v.Text), nil
	})
}

func TestRunCompletes(t *testing.T) {
	ctx := testCtx(t)
	s := newScript(endTurn("hello"))
	a := newTestAgent(t, s.Model())
	r, err := a.Run(ctx, "say hi")
	mustState(t, r, err, StateCompleted)
	if r.Output != "hello" || r.Usage.Turns != 1 || r.Usage.InputTokens != 10 || r.Agent != "default" {
		t.Fatalf("run = %+v", r)
	}
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	want := []string{EvRunQueued, EvRunStarted, EvTurnStarted, EvModelRequested, EvModelResponded, EvTurnFinished, EvRunFinished}
	if strings.Join(eventTypes(evs), " ") != strings.Join(want, " ") {
		t.Fatalf("events = %v", eventTypes(evs))
	}
	tp := regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`)
	for i, e := range evs {
		if e.Seq != uint64(i+1) || e.Source != "fw://local/default/"+r.ID || e.TraceParent != evs[0].TraceParent || !tp.MatchString(e.TraceParent) {
			t.Fatalf("event %d = %+v", i, e)
		}
	}
	reqs := s.requests()
	if len(reqs) != 1 || len(reqs[0].Messages) != 1 || TextOf(reqs[0].Messages[0].Content) != "say hi" ||
		reqs[0].MaxTokens != 64000 || reqs[0].Effort != "high" || reqs[0].Metadata["runId"] != r.ID {
		t.Fatalf("request = %+v", reqs[0])
	}
	var resp ModelRespondedData
	firstOf(t, evs, EvModelResponded, &resp)
	if resp.Provider != "anthropic" || resp.Model != "claude-opus-5-5" {
		t.Fatalf("responded = %+v", resp)
	}
	got, err := a.Get(ctx, r.ID)
	if err != nil || got.State != StateCompleted || got.Seq != uint64(len(evs)) {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if _, err := a.Get(ctx, "nope"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("Get unknown = %v", err)
	}
}

func TestObserversSeeStoredEvents(t *testing.T) {
	ctx := testCtx(t)
	var mu sync.Mutex
	var seen []Event
	l := NewMemLedger()
	obs := ObserverFunc(func(e Event) {
		mu.Lock()
		defer mu.Unlock()
		if e.Seq == 0 {
			t.Errorf("observer saw an unstored event %s", e.Type)
		}
		seen = append(seen, e)
	})
	a := newTestAgent(t, newScript(endTurn("ok")).Model(), WithLedger(l), WithObservers(obs))
	r, err := a.Run(ctx, "go")
	mustState(t, r, err, StateCompleted)
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != len(ledgerEvents(t, l, r.ID)) {
		t.Fatalf("observer saw %d events", len(seen))
	}
}

func TestListNewestFirst(t *testing.T) {
	ctx := testCtx(t)
	a := newTestAgent(t, newScript(endTurn("1"), endTurn("2")).Model())
	r1, err := a.Run(ctx, "one")
	mustState(t, r1, err, StateCompleted)
	time.Sleep(2 * time.Millisecond)
	r2, err := a.Run(ctx, "two")
	mustState(t, r2, err, StateCompleted)
	list, err := a.List(ctx)
	if err != nil || len(list) != 2 || list[0].ID != r2.ID || list[1].ID != r1.ID {
		t.Fatalf("List = %v, %v", list, err)
	}
}

func TestToolResultReachesModel(t *testing.T) {
	ctx := testCtx(t)
	s := newScript(callTools(use("c1", "echo", `{"text":"pong"}`)), endTurn("done"))
	var a *Agent
	var sawCtx atomic.Bool
	probe := NewTool(ToolSpec{Name: "probe", ReadOnly: true}, func(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
		return TextResult("unused"), nil
	})
	echo := NewTool(ToolSpec{Name: "echo", ReadOnly: true}, func(ctx context.Context, in json.RawMessage) (ToolResult, error) {
		info := RunInfoFrom(ctx)
		sawCtx.Store(info.CallID == "c1" && info.Agent == "default" && AgentFrom(ctx) == a)
		var v struct{ Text string }
		_ = json.Unmarshal(in, &v)
		ReportProgress(ctx, 1, 2, "half")
		return TextResult(v.Text), nil
	})
	a = newTestAgent(t, s.Model(), WithTools(echo, probe), WithProfile(AgentProfile{Name: "default", Tools: []string{"ec*"}, System: "be brief"}))
	r, err := a.Run(ctx, "ping")
	mustState(t, r, err, StateCompleted)
	if !sawCtx.Load() {
		t.Fatal("tool context lacks run info or agent")
	}
	reqs := s.requests()
	if len(reqs) != 2 {
		t.Fatalf("model calls = %d", len(reqs))
	}
	if len(reqs[0].Tools) != 1 || reqs[0].Tools[0].Name != "echo" || reqs[0].System != "be brief" {
		t.Fatalf("tools = %+v, system = %q", reqs[0].Tools, reqs[0].System)
	}
	msgs := reqs[1].Messages
	if len(msgs) != 3 || msgs[1].Role != RoleAssistant || msgs[1].Content[0].Type != BlockOpaque {
		t.Fatalf("history = %+v", msgs)
	}
	res := lastToolResults(t, reqs[1])
	if len(res) != 1 || res[0].ToolUseID != "c1" || TextOf(res[0].Content) != "pong" || res[0].IsError {
		t.Fatalf("results = %+v", res)
	}
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	var prog ToolProgressData
	firstOf(t, evs, EvToolProgress, &prog)
	if prog.CallID != "c1" || prog.Total != 2 {
		t.Fatalf("progress = %+v", prog)
	}
	if countType(evs, EvToolRequested) != 1 || countType(evs, EvToolResponded) != 1 {
		t.Fatalf("events = %v", eventTypes(evs))
	}
}

func TestUnknownToolAndToolErrors(t *testing.T) {
	ctx := testCtx(t)
	boom := NewTool(ToolSpec{Name: "boom", ReadOnly: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		return ToolResult{}, errors.New("kaput")
	})
	panicky := NewTool(ToolSpec{Name: "panicky", ReadOnly: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		panic("oh no")
	})
	s := newScript(callTools(use("c1", "nope", `{}`), use("c2", "boom", `{}`), use("c3", "panicky", `{}`)), endTurn("done"))
	a := newTestAgent(t, s.Model(), WithTools(boom, panicky))
	r, err := a.Run(ctx, "go")
	mustState(t, r, err, StateCompleted)
	res := lastToolResults(t, s.requests()[1])
	if len(res) != 3 {
		t.Fatalf("results = %+v", res)
	}
	for i, want := range []string{"unknown tool: nope", "kaput", "panicked"} {
		if !res[i].IsError || !strings.Contains(TextOf(res[i].Content), want) {
			t.Fatalf("result %d = %+v", i, res[i])
		}
	}
}

func destructiveTool(calls *atomic.Int32) Tool {
	return NewTool(ToolSpec{Name: "rm", Destructive: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		calls.Add(1)
		return TextResult("removed"), nil
	})
}

func TestApprovalAllow(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	s := newScript(callTools(use("c1", "rm", `{"path":"x"}`)), endTurn("done"))
	a := newTestAgent(t, s.Model(), WithTools(destructiveTool(&calls)))
	r, err := a.Run(ctx, "clean up")
	mustState(t, r, err, StateInputRequired)
	if r.Pending == nil || r.Pending.Tool != "rm" || r.Pending.CallID != "c1" || r.Pending.Reason != "rule: ask destructive" {
		t.Fatalf("pending = %+v", r.Pending)
	}
	if calls.Load() != 0 {
		t.Fatal("tool ran before approval")
	}
	if err := a.Decide(ctx, r.ID, ApprovalDecision{ApprovalID: "wrong", Decision: "allow"}); !errors.Is(err, ErrNoPendingApproval) {
		t.Fatalf("Decide wrong id = %v", err)
	}
	if err := a.Decide(ctx, r.ID, ApprovalDecision{ApprovalID: r.Pending.ID, Decision: "allow", Decider: "alice"}); err != nil {
		t.Fatal(err)
	}
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)
	if calls.Load() != 1 {
		t.Fatalf("tool calls = %d", calls.Load())
	}
	var d ApprovalDecidedData
	firstOf(t, ledgerEvents(t, a.Ledger(), r.ID), EvApprovalDecided, &d)
	if d.Decider != "alice" || d.Decision != "allow" {
		t.Fatalf("decided = %+v", d)
	}
	if TextOf(lastToolResults(t, s.requests()[1])[0].Content) != "removed" {
		t.Fatal("tool result missing")
	}
}

func TestApprovalDeny(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	s := newScript(callTools(use("c1", "rm", `{}`)), endTurn("done"))
	a := newTestAgent(t, s.Model(), WithTools(destructiveTool(&calls)))
	r, err := a.Run(ctx, "clean up")
	mustState(t, r, err, StateInputRequired)
	pctx := WithPrincipal(ctx, &Principal{Subject: "bob"})
	if err := a.Decide(pctx, r.ID, ApprovalDecision{ApprovalID: r.Pending.ID, Decision: "deny", Note: "too risky"}); err != nil {
		t.Fatal(err)
	}
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)
	if calls.Load() != 0 {
		t.Fatal("a denied tool ran")
	}
	res := lastToolResults(t, s.requests()[1])
	if !res[0].IsError || TextOf(res[0].Content) != "denied by bob: too risky" {
		t.Fatalf("result = %+v", res[0])
	}
}

func TestPolicyDenyRule(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	s := newScript(callTools(use("c1", "rm", `{}`)), endTurn("done"))
	a := newTestAgent(t, s.Model(), WithTools(destructiveTool(&calls)), WithPolicy(NewRulesPolicy(Rules{Deny: []string{"destructive"}})))
	r, err := a.Run(ctx, "clean up")
	mustState(t, r, err, StateCompleted)
	res := lastToolResults(t, s.requests()[1])
	if calls.Load() != 0 || !res[0].IsError || TextOf(res[0].Content) != "denied by policy: rule: deny destructive" {
		t.Fatalf("calls = %d, result = %+v", calls.Load(), res[0])
	}
	if n := countType(ledgerEvents(t, a.Ledger(), r.ID), EvToolRequested); n != 0 {
		t.Fatalf("a denied call has %d fw.tool.requested events", n)
	}
}

func TestProfilePolicyTightens(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	s := newScript(callTools(use("c1", "echo", `{"text":"x"}`)), endTurn("done"))
	a := newTestAgent(t, s.Model(), WithTools(echoTool(&calls)),
		WithProfile(AgentProfile{Name: "careful", Policy: &Rules{Ask: []string{"echo"}}}))
	r, err := a.Run(ctx, "go", AsAgent("careful"))
	mustState(t, r, err, StateInputRequired)
	if _, err := a.Start(ctx, "go", AsAgent("missing")); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("unknown profile = %v", err)
	}
}

func TestBudgetMaxTurns(t *testing.T) {
	ctx := testCtx(t)
	s := newScript(callTools(use("c1", "echo", `{"text":"a"}`)), endTurn("never"))
	a := newTestAgent(t, s.Model(), WithTools(echoTool(nil)))
	r, err := a.Run(ctx, "loop", WithRunBudget(Budget{MaxTurns: 1}))
	mustState(t, r, err, StateFailed)
	if r.Error != "budget exceeded: maxTurns" || len(s.requests()) != 1 {
		t.Fatalf("run = %+v, model calls = %d", r, len(s.requests()))
	}
	var d BudgetExceededData
	firstOf(t, ledgerEvents(t, a.Ledger(), r.ID), EvBudgetExceeded, &d)
	if d.Limit != "maxTurns" || d.Value != 1 {
		t.Fatalf("exceeded = %+v", d)
	}
}

func TestBudgetCost(t *testing.T) {
	ctx := testCtx(t)
	s := newScript(callTools(use("c1", "echo", `{"text":"a"}`)), endTurn("never"))
	prices := PriceTable{"anthropic/claude-opus-5-5": {Input: 1_000_000, Output: 0}}
	a := newTestAgent(t, s.Model(), WithTools(echoTool(nil)), WithPrices(prices))
	r, err := a.Run(ctx, "spend", WithRunBudget(Budget{MaxCostUSD: 5}))
	mustState(t, r, err, StateFailed)
	if r.Usage.CostUSD != 10 || r.Error != "budget exceeded: maxCostUSD" {
		t.Fatalf("run = %+v", r)
	}
}

func blockingModel(started chan<- struct{}) Model {
	return ModelFunc{StreamFn: func(ctx context.Context, _ *Request, _ func(Delta)) (*Response, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
}

func TestCancel(t *testing.T) {
	ctx := testCtx(t)
	started := make(chan struct{}, 1)
	a := newTestAgent(t, blockingModel(started))
	r, err := a.Start(ctx, "wait forever")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := a.Cancel(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCanceled)
	if err := a.Cancel(ctx, r.ID); err != nil {
		t.Fatalf("second cancel = %v", err)
	}
	time.Sleep(20 * time.Millisecond) // let the driver exit
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	last := evs[len(evs)-1]
	var d RunFinishedData
	_ = last.Decode(&d)
	if last.Type != EvRunFinished || d.Outcome != OutcomeCanceled || countType(evs, EvModelResponded) != 0 {
		t.Fatalf("events = %v", eventTypes(evs))
	}
}

func TestSendFollowUp(t *testing.T) {
	ctx := testCtx(t)
	s := newScript(endTurn("first"), endTurn("second"))
	a := newTestAgent(t, s.Model())
	r, err := a.Run(ctx, "hi")
	mustState(t, r, err, StateCompleted)
	if err := a.Send(ctx, r.ID, UserText("again"), OriginCLI); err != nil {
		t.Fatal(err)
	}
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)
	if r.Output != "second" || r.Usage.Turns != 2 {
		t.Fatalf("run = %+v", r)
	}
	msgs := s.requests()[1].Messages
	if len(msgs) != 3 || TextOf(msgs[0].Content) != "hi" || msgs[1].Role != RoleAssistant || TextOf(msgs[2].Content) != "again" {
		t.Fatalf("history = %+v", msgs)
	}
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	if countType(evs, EvRunFinished) != 2 || countType(evs, EvTurnStarted) != 2 {
		t.Fatalf("events = %v", eventTypes(evs))
	}
}

func TestSendMidTurnFollowsToolResults(t *testing.T) {
	ctx := testCtx(t)
	entered, release := make(chan struct{}), make(chan struct{})
	slow := NewTool(ToolSpec{Name: "slow", ReadOnly: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		close(entered)
		<-release
		return TextResult("slow done"), nil
	})
	s := newScript(callTools(use("c1", "slow", `{}`)), endTurn("done"))
	a := newTestAgent(t, s.Model(), WithTools(slow))
	r, err := a.Start(ctx, "start")
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := a.Send(ctx, r.ID, UserText("also this"), OriginHTTP); err != nil {
		t.Fatal(err)
	}
	close(release)
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)
	msgs := s.requests()[1].Messages
	if len(msgs) != 4 || msgs[2].Content[0].Type != BlockToolResult || TextOf(msgs[3].Content) != "also this" {
		t.Fatalf("history = %+v", msgs)
	}
}

func TestIdempotencyKey(t *testing.T) {
	ctx := testCtx(t)
	l := NewMemLedger()
	a := newTestAgent(t, newScript(endTurn("once")).Model(), WithLedger(l))
	r1, err := a.Start(ctx, "job", WithIdempotencyKey("k1"))
	if err != nil {
		t.Fatal(err)
	}
	r2, err := a.Start(ctx, "job", WithIdempotencyKey("k1"))
	if err != nil || r2.ID != r1.ID {
		t.Fatalf("second start = %v, %v", r2, err)
	}
	r, err := a.Wait(ctx, r1.ID)
	mustState(t, r, err, StateCompleted)
	if r.Metadata["idempotencyKey"] != "k1" {
		t.Fatalf("metadata = %v", r.Metadata)
	}
	list, _ := a.List(ctx)
	if len(list) != 1 {
		t.Fatalf("runs = %d", len(list))
	}
	// A restarted process rebuilds the key from the ledger.
	b := newTestAgent(t, newScript().Model(), WithLedger(l))
	r3, err := b.Start(ctx, "job", WithIdempotencyKey("k1"))
	if err != nil || r3.ID != r1.ID || r3.State != StateCompleted {
		t.Fatalf("after restart = %+v, %v", r3, err)
	}
}

func TestParallelSafeToolsOverlap(t *testing.T) {
	ctx := testCtx(t)
	var arrived sync.WaitGroup
	arrived.Add(2)
	both := make(chan struct{})
	go func() {
		arrived.Wait()
		close(both)
	}()
	par := NewTool(ToolSpec{Name: "par", ReadOnly: true, ParallelSafe: true}, func(ctx context.Context, in json.RawMessage) (ToolResult, error) {
		arrived.Done()
		select {
		case <-both:
			return TextResult("overlap " + string(in)), nil
		case <-time.After(3 * time.Second):
			return ErrorResult("no overlap"), nil
		}
	})
	s := newScript(callTools(use("c1", "par", `1`), use("c2", "par", `2`)), endTurn("done"))
	a := newTestAgent(t, s.Model(), WithTools(par))
	r, err := a.Run(ctx, "go")
	mustState(t, r, err, StateCompleted)
	res := lastToolResults(t, s.requests()[1])
	if len(res) != 2 || res[0].ToolUseID != "c1" || TextOf(res[0].Content) != "overlap 1" || TextOf(res[1].Content) != "overlap 2" {
		t.Fatalf("results = %+v", res)
	}
}

func TestEventsCloseAfterTerminal(t *testing.T) {
	ctx := testCtx(t)
	release := make(chan struct{})
	m := ModelFunc{StreamFn: func(ctx context.Context, _ *Request, _ func(Delta)) (*Response, error) {
		<-release
		return endTurn("ok"), nil
	}}
	a := newTestAgent(t, m)
	r, err := a.Start(ctx, "go")
	if err != nil {
		t.Fatal(err)
	}
	ch, err := a.Events(ctx, r.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	var got []Event
	for e := range ch {
		got = append(got, e)
	}
	if len(got) == 0 || got[len(got)-1].Type != EvRunFinished || len(got) != len(ledgerEvents(t, a.Ledger(), r.ID)) {
		t.Fatalf("events = %v", eventTypes(got))
	}
	// A finished run replays and closes too, from any sequence number.
	ch, err = a.Events(ctx, r.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	got = got[:0]
	for e := range ch {
		got = append(got, e)
	}
	if got[0].Seq != 3 || got[len(got)-1].Type != EvRunFinished {
		t.Fatalf("replay = %v", eventTypes(got))
	}
}

func seed(t *testing.T, l Ledger, id string, evs ...Event) {
	t.Helper()
	for _, e := range evs {
		e.Subject = id
		if _, err := l.Append(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
}

func TestResumeModelRequest(t *testing.T) {
	ctx := testCtx(t)
	l := NewMemLedger()
	id := "run-resume"
	seed(t, l, id,
		MustEvent(id, EvRunQueued, RunQueuedData{Agent: "default", Input: "hi", Origin: OriginCLI}),
		MustEvent(id, EvRunStarted, RunStartedData{Model: "anthropic/claude-opus-5-5"}),
		MustEvent(id, EvTurnStarted, TurnData{Turn: 1}),
		MustEvent(id, EvModelRequested, ModelRequestedData{Provider: "anthropic", Model: "claude-opus-5-5"}),
	)
	s := newScript(endTurn("resumed"))
	a := newTestAgent(t, s.Model(), WithLedger(l))
	r, err := a.Wait(ctx, id)
	mustState(t, r, err, StateCompleted)
	if r.Output != "resumed" || r.Usage.Turns != 1 {
		t.Fatalf("run = %+v", r)
	}
	evs := ledgerEvents(t, l, id)
	var rd RunResumedData
	firstOf(t, evs, EvRunResumed, &rd)
	if rd.FromSeq != 4 || countType(evs, EvModelRequested) != 2 || countType(evs, EvTurnStarted) != 1 {
		t.Fatalf("events = %v, resumed = %+v", eventTypes(evs), rd)
	}
	if len(s.requests()) != 1 || TextOf(s.requests()[0].Messages[0].Content) != "hi" {
		t.Fatal("the resumed request lacks the input")
	}
}

func seedToolRequested(t *testing.T, l Ledger, id, tool string) {
	seed(t, l, id,
		MustEvent(id, EvRunQueued, RunQueuedData{Agent: "default", Input: "hi", Origin: OriginCLI}),
		MustEvent(id, EvRunStarted, RunStartedData{}),
		MustEvent(id, EvTurnStarted, TurnData{Turn: 1}),
		MustEvent(id, EvModelRequested, ModelRequestedData{}),
		MustEvent(id, EvModelResponded, ModelRespondedData{Content: []Block{use("c1", tool, `{}`)}, StopReason: StopToolUse}),
		MustEvent(id, EvToolRequested, ToolRequestedData{Tool: tool, CallID: "c1", Input: json.RawMessage(`{}`)}),
	)
}

func TestResumeToolCall(t *testing.T) {
	ctx := testCtx(t)
	t.Run("idempotent runs again", func(t *testing.T) {
		l := NewMemLedger()
		seedToolRequested(t, l, "r1", "put")
		var calls atomic.Int32
		put := NewTool(ToolSpec{Name: "put", Idempotent: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
			calls.Add(1)
			return TextResult("put"), nil
		})
		a := newTestAgent(t, newScript(endTurn("done")).Model(), WithLedger(l), WithTools(put))
		r, err := a.Wait(ctx, "r1")
		mustState(t, r, err, StateCompleted)
		if calls.Load() != 1 {
			t.Fatalf("calls = %d", calls.Load())
		}
	})
	t.Run("not idempotent asks first", func(t *testing.T) {
		l := NewMemLedger()
		seedToolRequested(t, l, "r2", "send")
		var calls atomic.Int32
		send := NewTool(ToolSpec{Name: "send"}, func(context.Context, json.RawMessage) (ToolResult, error) {
			calls.Add(1)
			return TextResult("sent"), nil
		})
		a := newTestAgent(t, newScript(endTurn("done")).Model(), WithLedger(l), WithTools(send))
		r, err := a.Wait(ctx, "r2")
		mustState(t, r, err, StateInputRequired)
		if r.Pending.Reason != "rerun after resume" || r.Pending.CallID != "c1" || calls.Load() != 0 {
			t.Fatalf("pending = %+v", r.Pending)
		}
		if err := a.Decide(ctx, "r2", ApprovalDecision{ApprovalID: r.Pending.ID, Decision: "allow"}); err != nil {
			t.Fatal(err)
		}
		r, err = a.Wait(ctx, "r2")
		mustState(t, r, err, StateCompleted)
		if calls.Load() != 1 {
			t.Fatalf("calls = %d", calls.Load())
		}
	})
	t.Run("pending approval waits", func(t *testing.T) {
		l := NewMemLedger()
		seedToolRequested(t, l, "r3", "send")
		seed(t, l, "r3", MustEvent("r3", EvApprovalRequested, ApprovalRequestedData{ApprovalID: "ap1", Tool: "send", CallID: "c1", Reason: "rerun after resume"}))
		var calls atomic.Int32
		send := NewTool(ToolSpec{Name: "send"}, func(context.Context, json.RawMessage) (ToolResult, error) {
			calls.Add(1)
			return TextResult("sent"), nil
		})
		a := newTestAgent(t, newScript(endTurn("done")).Model(), WithLedger(l), WithTools(send))
		r, err := a.Get(ctx, "r3")
		mustState(t, r, err, StateInputRequired)
		if err := a.Decide(ctx, "r3", ApprovalDecision{ApprovalID: "ap1", Decision: "deny"}); err != nil {
			t.Fatal(err)
		}
		r, err = a.Wait(ctx, "r3")
		mustState(t, r, err, StateCompleted)
		if calls.Load() != 0 {
			t.Fatal("a denied rerun ran")
		}
	})
}

func TestAskerRoundTrip(t *testing.T) {
	ctx := testCtx(t)
	ask := NewTool(ToolSpec{Name: "ask_user", ReadOnly: true}, func(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
		raw, err := AskCaller(ctx, "What is your name?", nil)
		if err != nil {
			return ToolResult{}, err
		}
		var v struct{ Answer string }
		if err := json.Unmarshal(raw, &v); err != nil {
			return ToolResult{}, err
		}
		return TextResult("hello " + v.Answer), nil
	})
	s := newScript(callTools(use("c1", "ask_user", `{}`)), endTurn("done"))
	a := newTestAgent(t, s.Model(), WithTools(ask))
	r, err := a.Run(ctx, "greet me")
	mustState(t, r, err, StateInputRequired)
	if r.Pending.Reason != "What is your name?" || string(r.Pending.Schema) != answerSchema || r.Pending.CallID != "c1" {
		t.Fatalf("pending = %+v", r.Pending)
	}
	if err := a.Decide(ctx, r.ID, ApprovalDecision{ApprovalID: r.Pending.ID, Content: json.RawMessage(`{"answer":"Ada"}`)}); err != nil {
		t.Fatal(err)
	}
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)
	if got := TextOf(lastToolResults(t, s.requests()[1])[0].Content); got != "hello Ada" {
		t.Fatalf("tool result = %q", got)
	}
}

func TestStopReasons(t *testing.T) {
	ctx := testCtx(t)
	t.Run("max_tokens continues once", func(t *testing.T) {
		s := newScript(&Response{Content: []Block{Text("part")}, StopReason: StopMaxTokens}, endTurn("rest"))
		a := newTestAgent(t, s.Model())
		r, err := a.Run(ctx, "long")
		mustState(t, r, err, StateCompleted)
		msgs := s.requests()[1].Messages
		if TextOf(msgs[len(msgs)-1].Content) != "Continue." {
			t.Fatalf("history = %+v", msgs)
		}
		var d ErrorData
		firstOf(t, ledgerEvents(t, a.Ledger(), r.ID), EvError, &d)
		if !d.Retryable {
			t.Fatal("max_tokens error is not retryable")
		}
	})
	t.Run("max_tokens with a cut tool input", func(t *testing.T) {
		cut := &Response{Content: []Block{Text("part"), use("t1", "echo", `{"text":"unfin`)}, StopReason: StopMaxTokens}
		s := newScript(cut, endTurn("rest"))
		a := newTestAgent(t, s.Model(), WithTools(echoTool(nil)))
		r, err := a.Run(ctx, "long")
		mustState(t, r, err, StateCompleted)
		msgs := s.requests()[1].Messages
		if len(msgs) != 4 || !msgs[2].Content[0].IsError || string(msgs[1].Content[1].Input) != `"{\"text\":\"unfin"` {
			t.Fatalf("history = %+v", msgs)
		}
	})
	t.Run("max_tokens twice fails", func(t *testing.T) {
		mt := &Response{Content: []Block{Text("part")}, StopReason: StopMaxTokens}
		s := newScript(mt, mt)
		a := newTestAgent(t, s.Model())
		r, err := a.Run(ctx, "long")
		mustState(t, r, err, StateFailed)
		if r.Error != "max_tokens" || len(s.requests()) != 2 {
			t.Fatalf("run = %+v", r)
		}
	})
	t.Run("refusal fails", func(t *testing.T) {
		s := newScript(&Response{StopReason: StopRefusal, StopDetail: "cyber"})
		a := newTestAgent(t, s.Model())
		r, err := a.Run(ctx, "bad")
		mustState(t, r, err, StateFailed)
		if r.Error != "cyber" {
			t.Fatalf("error = %q", r.Error)
		}
	})
	t.Run("pause_turn loops", func(t *testing.T) {
		s := newScript(&Response{Content: []Block{Text("wait")}, StopReason: StopPauseTurn}, endTurn("done"))
		a := newTestAgent(t, s.Model())
		r, err := a.Run(ctx, "go")
		mustState(t, r, err, StateCompleted)
		if r.Output != "done" || r.Usage.Turns != 2 {
			t.Fatalf("run = %+v", r)
		}
	})
	t.Run("model error fails", func(t *testing.T) {
		a := newTestAgent(t, newScript().Model())
		r, err := a.Run(ctx, "go")
		mustState(t, r, err, StateFailed)
		if !strings.Contains(r.Error, "no response left") {
			t.Fatalf("error = %q", r.Error)
		}
	})
}

func TestMaxRunsQueues(t *testing.T) {
	ctx := testCtx(t)
	cfg := DefaultConfig()
	cfg.MaxRuns = 1
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	m := ModelFunc{StreamFn: func(ctx context.Context, _ *Request, _ func(Delta)) (*Response, error) {
		started <- struct{}{}
		select {
		case <-release:
			return endTurn("ok"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	a := newTestAgent(t, m, WithConfig(cfg))
	r1, _ := a.Start(ctx, "one")
	<-started
	r2, err := a.Start(ctx, "two")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
		t.Fatal("a second run started past MaxRuns")
	case <-time.After(50 * time.Millisecond):
	}
	if got, _ := a.Get(ctx, r2.ID); got.State != StateQueued {
		t.Fatalf("second run state = %s", got.State)
	}
	close(release)
	for _, id := range []string{r1.ID, r2.ID} {
		r, err := a.Wait(ctx, id)
		mustState(t, r, err, StateCompleted)
	}
}

func TestShutdownSuspendsAndResumes(t *testing.T) {
	ctx := testCtx(t)
	l := NewMemLedger()
	started := make(chan struct{}, 1)
	a := newTestAgent(t, blockingModel(started), WithLedger(l))
	r, err := a.Start(ctx, "long job")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	sctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	a.Close(sctx)
	a.Close(sctx) // a second close returns at once
	if _, err := a.Start(ctx, "late"); !errors.Is(err, ErrClosed) {
		t.Fatalf("start after close = %v", err)
	}
	evs := ledgerEvents(t, l, r.ID)
	var sd RunSuspendedData
	last := evs[len(evs)-1]
	_ = last.Decode(&sd)
	if last.Type != EvRunSuspended || sd.Reason != "shutdown" {
		t.Fatalf("events = %v", eventTypes(evs))
	}
	if got, _ := a.Get(ctx, r.ID); got.State != StateSuspended {
		t.Fatalf("state = %s", got.State)
	}
	b := newTestAgent(t, newScript(endTurn("finished later")).Model(), WithLedger(l))
	got, err := b.Wait(ctx, r.ID)
	mustState(t, got, err, StateCompleted)
	if got.Output != "finished later" {
		t.Fatalf("output = %q", got.Output)
	}
}

func TestResumeAfterResponse(t *testing.T) {
	ctx := testCtx(t)
	head := func(id string) []Event {
		return []Event{
			MustEvent(id, EvRunQueued, RunQueuedData{Agent: "default", Input: "hi"}),
			MustEvent(id, EvRunStarted, RunStartedData{}),
			MustEvent(id, EvTurnStarted, TurnData{Turn: 1}),
			MustEvent(id, EvModelRequested, ModelRequestedData{}),
		}
	}
	t.Run("closed end_turn finishes without a model call", func(t *testing.T) {
		l := NewMemLedger()
		seed(t, l, "e1", append(head("e1"),
			MustEvent("e1", EvModelResponded, ModelRespondedData{Content: []Block{Text("recorded")}, StopReason: StopEndTurn}),
			MustEvent("e1", EvTurnFinished, TurnData{Turn: 1}))...)
		s := newScript()
		a := newTestAgent(t, s.Model(), WithLedger(l))
		r, err := a.Wait(ctx, "e1")
		mustState(t, r, err, StateCompleted)
		if r.Output != "recorded" || len(s.requests()) != 0 {
			t.Fatalf("run = %+v, model calls = %d", r, len(s.requests()))
		}
	})
	t.Run("tool_use response runs its tools", func(t *testing.T) {
		l := NewMemLedger()
		seed(t, l, "e2", append(head("e2"),
			MustEvent("e2", EvModelResponded, ModelRespondedData{Content: []Block{use("c1", "echo", `{"text":"x"}`)}, StopReason: StopToolUse}))...)
		var calls atomic.Int32
		s := newScript(endTurn("done"))
		a := newTestAgent(t, s.Model(), WithLedger(l), WithTools(echoTool(&calls)))
		r, err := a.Wait(ctx, "e2")
		mustState(t, r, err, StateCompleted)
		if calls.Load() != 1 || TextOf(lastToolResults(t, s.requests()[0])[0].Content) != "x" {
			t.Fatalf("calls = %d", calls.Load())
		}
	})
}

func TestWithRequest(t *testing.T) {
	ctx := testCtx(t)
	a := newTestAgent(t, newScript(endTurn("parent"), endTurn("ok")).Model())
	p, err := a.Run(ctx, "parent")
	mustState(t, p, err, StateCompleted)
	req := RunRequest{ID: "custom-id", Input: "from body", Origin: OriginHTTP, Parent: p.ID, Metadata: map[string]string{"a": "b"}, Budget: &Budget{MaxTurns: 3}}
	r, err := a.Run(ctx, "", WithRequest(req))
	mustState(t, r, err, StateCompleted)
	if r.ID != "custom-id" || r.Input != "from body" || r.Origin != OriginHTTP || r.Parent != p.ID || r.Depth != 1 || r.Metadata["a"] != "b" {
		t.Fatalf("run = %+v", r)
	}
	if _, err := a.Start(ctx, "x", WithRequest(RunRequest{Parent: "missing"})); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("unknown parent = %v", err)
	}
	if _, err := a.Start(ctx, "x", WithRequest(RunRequest{ID: "../bad"})); err == nil {
		t.Fatal("an unsafe run id was accepted")
	}
	if _, err := a.Start(ctx, "x", WithRequest(RunRequest{ID: "custom-id"})); err == nil {
		t.Fatal("a duplicate run id was accepted")
	}
	if _, err := a.Start(ctx, "  "); err == nil {
		t.Fatal("an empty input was accepted")
	}
}

func TestRunBudgetMergesRequestAndConfiguration(t *testing.T) {
	ctx := testCtx(t)
	s := newScript(endTurn("done"))
	a := newTestAgent(t, s.Model())
	r, err := a.Run(ctx, "work", WithRunBudget(Budget{MaxTurns: 3}))
	mustState(t, r, err, StateCompleted)
	b, err := a.RunBudget(ctx, r.ID)
	if err != nil || b.MaxTurns != 3 {
		t.Fatalf("budget = %+v, err = %v", b, err)
	}
	if _, err := a.RunBudget(ctx, "missing"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("missing run: err = %v", err)
	}
}

// storeCounter is a StoreObserver that counts what the kernel reports.
type storeCounter struct {
	mu      sync.Mutex
	appends int
	drivers map[string]bool
}

func (c *storeCounter) Observe(Event) {}

func (c *storeCounter) ObserveLedgerAppend(driver string, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.appends++
	if c.drivers == nil {
		c.drivers = map[string]bool{}
	}
	c.drivers[driver] = d >= 0
}

func (c *storeCounter) CountBusMessage(string, string) {}

func TestStoreObserverSeesLedgerAppends(t *testing.T) {
	ctx := testCtx(t)
	c := &storeCounter{}
	a := newTestAgent(t, newScript(endTurn("done")).Model(), WithObservers(c))
	r, err := a.Run(ctx, "work")
	mustState(t, r, err, StateCompleted)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.appends != len(ledgerEvents(t, a.Ledger(), r.ID)) || len(c.drivers) != 1 {
		t.Fatalf("appends = %d, drivers = %v", c.appends, c.drivers)
	}
	if len(a.ToolSources()) != 0 {
		t.Fatalf("tool sources = %d", len(a.ToolSources()))
	}
}
