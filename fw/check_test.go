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

// fakeQueue is a tool that answers with a fixed result and records the
// input of each call.
type fakeQueue struct {
	mu     sync.Mutex
	inputs []string
}

func (q *fakeQueue) tool(name string, res ToolResult, err error) Tool {
	return NewTool(ToolSpec{Name: name, ReadOnly: true, Idempotent: true}, func(_ context.Context, in json.RawMessage) (ToolResult, error) {
		q.mu.Lock()
		defer q.mu.Unlock()
		q.inputs = append(q.inputs, string(in))
		return res, err
	})
}

func (q *fakeQueue) calls() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.inputs...)
}

// checkOutputOf decodes the output of a check run.
func checkOutputOf(t *testing.T, r *Run) CheckOutput {
	t.Helper()
	var out CheckOutput
	if err := json.Unmarshal([]byte(r.Output), &out); err != nil {
		t.Fatalf("the output %q is not a check output: %v", r.Output, err)
	}
	return out
}

func step(tool, input string) CheckStep { return CheckStep{Tool: tool, Input: json.RawMessage(input)} }

// sameJSON reports whether two texts hold the same JSON value. The ledger
// keeps an input with the HTML escapes of encoding/json, such as \u003c.
func sameJSON(a, b string) bool {
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	ja, _ := json.Marshal(x)
	jb, _ := json.Marshal(y)
	return string(ja) == string(jb)
}

// A check run with one step calls the tool with the input of the step,
// calls no model, and finishes with the JSON of the result. It has the
// kind check, a usage without tokens or cost, and the events of a call of
// the model.
func TestCheckRunOneStep(t *testing.T) {
	ctx := testCtx(t)
	s := newScript() // the model must not be called
	q := &fakeQueue{}
	incidents := TextResult(`{"result": {"records": [{"number": "INC0012", "priority": 1}]}}`)
	a := newTestAgent(t, s.Model(), WithTools(q.tool("servicenow.query_incidents", incidents, nil)))
	r, err := a.Run(ctx, "", WithCheck(Check{Steps: []CheckStep{step("servicenow.query_incidents", `{"query": "priority<=2"}`)}}),
		WithMetadata(map[string]string{MetaTitle: "Check of the queue"}))
	mustState(t, r, err, StateCompleted)
	if want := `{"results":[{"tool":"servicenow.query_incidents","output":{"result":{"records":[{"number":"INC0012","priority":1}]}},"isError":false}]}`; r.Output != want {
		t.Fatalf("output = %s\nwant     %s", r.Output, want)
	}
	if n := len(s.requests()); n != 0 {
		t.Fatalf("the model got %d calls", n)
	}
	if got := q.calls(); len(got) != 1 || !sameJSON(got[0], `{"query": "priority<=2"}`) {
		t.Fatalf("calls = %q", got)
	}
	if r.Kind() != RunKindCheck || r.Metadata[MetaKind] != RunKindCheck || r.Title != "Check of the queue" {
		t.Fatalf("kind %q, metadata %v, title %q", r.Kind(), r.Metadata, r.Title)
	}
	u := r.Usage
	if u.InputTokens != 0 || u.OutputTokens != 0 || u.CacheReadTokens != 0 || u.CacheWriteTokens != 0 || u.CostUSD != 0 || u.UnpricedTokens != 0 || u.Turns != 0 || u.ToolCalls != 1 {
		t.Fatalf("usage = %+v", u)
	}
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	want := []string{EvRunQueued, EvRunStarted, EvToolRequested, EvToolResponded, EvRunFinished}
	if got := strings.Join(eventTypes(evs), " "); got != strings.Join(want, " ") {
		t.Fatalf("events = %s", got)
	}
	var started RunStartedData
	firstOf(t, evs, EvRunStarted, &started)
	if started.Model != "" || started.Brief != "" {
		t.Fatalf("started = %+v", started)
	}
	req := requested(evs)[0]
	if !strings.HasPrefix(req.CallID, checkCallPrefix) || req.Tool != "servicenow.query_incidents" || req.Routine != "" || !sameJSON(string(req.Input), `{"query":"priority<=2"}`) {
		t.Fatalf("requested = %+v", req)
	}
	var queued RunQueuedData
	firstOf(t, evs, EvRunQueued, &queued)
	if c := queued.Check; c == nil || c.Timeout != DefaultCheckTimeout || c.MaxBytes != DefaultCheckMaxBytes || len(c.Steps) != 1 {
		t.Fatalf("queued check = %+v", queued.Check)
	}
	checkCursorPrefixes(t, evs)
}

// The steps run in order, each with its own result: JSON, text, an error
// result of the tool, and a tool that returns a Go error. An error of a
// tool does not fail the run, and the steps after it run.
func TestCheckRunSeveralSteps(t *testing.T) {
	ctx := testCtx(t)
	s := newScript()
	q := &fakeQueue{}
	a := newTestAgent(t, s.Model(), WithTools(
		q.tool("queue.count", TextResult("[1, 2,\n 3]"), nil),
		q.tool("queue.note", TextResult("all quiet <ok>"), nil),
		q.tool("queue.down", ErrorResult("queue: 503 Service Unavailable"), nil),
		q.tool("queue.broken", ToolResult{}, errors.New("connection refused")),
	))
	r, err := a.Run(ctx, "look at the queues", WithCheck(Check{Steps: []CheckStep{
		step("queue.count", ""), step("queue.note", `{"a": 1}`), step("queue.down", "null"), step("queue.broken", `{}`),
	}}))
	mustState(t, r, err, StateCompleted)
	out := checkOutputOf(t, r)
	if len(out.Results) != 4 {
		t.Fatalf("results = %+v", out.Results)
	}
	want := []struct{ tool, output string }{
		{"queue.count", `[1,2,3]`},
		{"queue.note", `"all quiet <ok>"`},
		{"queue.down", `"queue: 503 Service Unavailable"`},
		{"queue.broken", `"tool queue.broken: connection refused"`},
	}
	for i, w := range want {
		got := out.Results[i]
		if got.Tool != w.tool || string(got.Output) != w.output || got.IsError != (i >= 2) || got.Cut {
			t.Fatalf("result %d = %+v (output %s), want %s %s", i, got, got.Output, w.tool, w.output)
		}
	}
	if !strings.Contains(r.Output, "<ok>") {
		t.Fatalf("the output escapes HTML: %s", r.Output)
	}
	if got := q.calls(); strings.Join(got, " ") != `{} {"a":1} {} {}` {
		t.Fatalf("calls = %q", got)
	}
	if r.Usage.ToolCalls != 4 || len(s.requests()) != 0 {
		t.Fatalf("usage %+v, model calls %d", r.Usage, len(s.requests()))
	}
}

// A step whose tool the policy asks for fails the run at once, with the
// reason: nobody answers a check, so it never waits for a decision. The
// tool does not run, and the steps after it do not run either.
func TestCheckRunPolicyAsks(t *testing.T) {
	ctx := testCtx(t)
	q := &fakeQueue{}
	a := newTestAgent(t, newScript().Model(), WithTools(q.tool("queue.read", TextResult("{}"), nil), q.tool("queue.write", TextResult("ok"), nil)),
		WithProfile(AgentProfile{Name: "triage", Policy: &Rules{Ask: []string{"queue.write"}}}))
	r, err := a.Run(ctx, "", AsAgent("triage"), WithCheck(Check{Steps: []CheckStep{step("queue.read", ""), step("queue.write", ""), step("queue.read", "")}}))
	mustState(t, r, err, StateFailed)
	want := "check step 2 (queue.write): the policy asks before this tool (rule: ask queue.write), and a check runs only tools that the policy allows without a question"
	if r.Error != want || r.Pending != nil {
		t.Fatalf("error %q, pending %+v", r.Error, r.Pending)
	}
	if got := q.calls(); len(got) != 1 {
		t.Fatalf("calls = %q", got)
	}
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	if countType(evs, EvApprovalRequested) != 0 || countType(evs, EvToolRequested) != 1 {
		t.Fatalf("events = %v", eventTypes(evs))
	}
	// The results of the steps before the failure stay in the output.
	if out := checkOutputOf(t, r); len(out.Results) != 1 || out.Results[0].Tool != "queue.read" {
		t.Fatalf("output = %s", r.Output)
	}
}

// A step whose tool the policy denies fails the run with the reason of the
// policy. With the default policy, a destructive tool asks.
func TestCheckRunPolicyDenies(t *testing.T) {
	ctx := testCtx(t)
	q := &fakeQueue{}
	a := newTestAgent(t, newScript().Model(), WithTools(q.tool("queue.read", TextResult("{}"), nil)),
		WithProfile(AgentProfile{Name: "triage", Policy: &Rules{Deny: []string{"queue.*"}}}))
	r, err := a.Run(ctx, "", AsAgent("triage"), WithCheck(Check{Steps: []CheckStep{step("queue.read", "")}}))
	mustState(t, r, err, StateFailed)
	if r.Error != "check step 1 (queue.read): denied by policy: rule: deny queue.*" || r.Output != "" || len(q.calls()) != 0 {
		t.Fatalf("error %q, output %q, calls %q", r.Error, r.Output, q.calls())
	}

	var calls = 0
	rm := NewTool(ToolSpec{Name: "rm", Destructive: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		calls++
		return TextResult("removed"), nil
	})
	b := newTestAgent(t, newScript().Model(), WithTools(rm))
	r, err = b.Run(ctx, "", WithCheck(Check{Steps: []CheckStep{step("rm", "")}}))
	mustState(t, r, err, StateFailed)
	if !strings.Contains(r.Error, "the policy asks before this tool (rule: ask destructive)") || calls != 0 {
		t.Fatalf("error %q, calls %d", r.Error, calls)
	}
}

// A step that names a tool that the agent does not have fails the run: a
// tool of no source, and a tool that the profile of the agent leaves out.
func TestCheckRunUnknownTool(t *testing.T) {
	ctx := testCtx(t)
	q := &fakeQueue{}
	a := newTestAgent(t, newScript().Model(), WithTools(q.tool("queue.read", TextResult("{}"), nil), q.tool("queue.other", TextResult("{}"), nil)),
		WithProfile(AgentProfile{Name: "narrow", Tools: []string{"queue.read"}}))
	for _, tool := range []string{"queue.nothing", "queue.other"} {
		r, err := a.Run(ctx, "", AsAgent("narrow"), WithCheck(Check{Steps: []CheckStep{step(tool, "")}}))
		mustState(t, r, err, StateFailed)
		if r.Error != "check step 1 ("+tool+"): the agent has no tool of this name" {
			t.Fatalf("error = %q", r.Error)
		}
	}
	if len(q.calls()) != 0 {
		t.Fatalf("calls = %q", q.calls())
	}
}

// A result longer than maxBytes keeps its first bytes, on a rune boundary,
// as a JSON string with cut, and the ledger records the cut result. The
// output limit of the tools does not cut a result of a check: a result
// above tools.limits.maxOutputBytes comes whole.
func TestCheckRunCut(t *testing.T) {
	ctx := testCtx(t)
	cfg := DefaultConfig()
	cfg.Tools.Limits.MaxOutputBytes = 64
	long := `{"records": [` + strings.Repeat(`{"number": "INC0012"}, `, 20) + `{"number": "INC0099"}]}`
	q := &fakeQueue{}
	a := newTestAgent(t, newScript().Model(), WithConfig(cfg), WithTools(
		q.tool("queue.read", TextResult(long), nil),
		q.tool("queue.words", TextResult(strings.Repeat("é", 9)), nil),
		q.tool("queue.image", ToolResult{Content: []Block{Text("a chart"), {Type: BlockImage, MediaType: "image/png", Data: []byte("png")}}}, nil),
	))
	r, err := a.Run(ctx, "", WithCheck(Check{Steps: []CheckStep{step("queue.read", "")}}))
	mustState(t, r, err, StateCompleted)
	res := checkOutputOf(t, r).Results[0]
	var records struct{ Records []map[string]string }
	if res.Cut || json.Unmarshal(res.Output, &records) != nil || len(records.Records) != 21 {
		t.Fatalf("a result above the output limit of the tools: cut %v, output %s", res.Cut, res.Output)
	}

	r, err = a.Run(ctx, "", WithCheck(Check{MaxBytes: 15, Steps: []CheckStep{step("queue.read", ""), step("queue.words", ""), step("queue.image", "")}}))
	mustState(t, r, err, StateCompleted)
	out := checkOutputOf(t, r)
	if res := out.Results[0]; !res.Cut || string(res.Output) != `"{\"records\": [{\""` {
		t.Fatalf("result 1 = %+v (output %s)", res, res.Output)
	}
	// Nine runes of two bytes: the cut at 15 bytes keeps 14, never half a
	// rune.
	if res := out.Results[1]; !res.Cut || string(res.Output) != `"`+strings.Repeat("é", 7)+`"` {
		t.Fatalf("result 2 = %+v (output %s)", res, res.Output)
	}
	if res := out.Results[2]; !res.Cut || string(res.Output) != `"a chart"` {
		t.Fatalf("result 3 = %+v (output %s)", res, res.Output)
	}
	resps := decodeAll[ToolRespondedData](t, ledgerEvents(t, a.Ledger(), r.ID), EvToolResponded)
	if got := TextOf(resps[0].Content); got != `{"records": [{"` {
		t.Fatalf("the ledger holds %q", got)
	}
	if len(resps[2].Content) != 1 || resps[2].Content[0].Type != BlockText {
		t.Fatalf("the ledger holds %+v", resps[2].Content)
	}
}

// The time limit of a check ends the call in flight, which the ledger
// records with its result, and fails the run. A step that has no time
// left does not start.
func TestCheckRunTimeout(t *testing.T) {
	ctx := testCtx(t)
	saw := make(chan error, 1)
	slow := NewTool(ToolSpec{Name: "queue.slow", ReadOnly: true}, func(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
		<-ctx.Done()
		saw <- context.Cause(ctx)
		return ToolResult{}, ctx.Err()
	})
	q := &fakeQueue{}
	a := newTestAgent(t, newScript().Model(), WithTools(slow, q.tool("queue.read", TextResult("{}"), nil)))
	start := time.Now()
	r, err := a.Run(ctx, "", WithCheck(Check{Timeout: 100 * time.Millisecond, Steps: []CheckStep{step("queue.read", ""), step("queue.slow", ""), step("queue.read", "")}}))
	mustState(t, r, err, StateFailed)
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the check took %s", took)
	}
	if r.Error != "check step 2 (queue.slow): the check did not finish within 100ms" {
		t.Fatalf("error = %q", r.Error)
	}
	if cause := <-saw; !errors.Is(cause, errCheckTimeout) {
		t.Fatalf("the tool saw %v", cause)
	}
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	resps := decodeAll[ToolRespondedData](t, evs, EvToolResponded)
	if len(resps) != 2 || !resps[1].IsError || !strings.Contains(TextOf(resps[1].Content), "the check reached its time limit") {
		t.Fatalf("responded = %+v", resps)
	}
	if len(q.calls()) != 1 || countType(evs, EvError) != 1 {
		t.Fatalf("calls %q, events %v", q.calls(), eventTypes(evs))
	}
}

// A tool of a check that asks a question gets ErrNoAsker, so the run
// never waits for a person.
func TestCheckRunAsksNobody(t *testing.T) {
	ctx := testCtx(t)
	asking := NewTool(ToolSpec{Name: "queue.ask", ReadOnly: true}, func(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
		if _, err := AskCaller(ctx, "Which queue?", nil); err != nil {
			return ErrorResult("queue.ask: " + err.Error()), nil
		}
		return TextResult("answered"), nil
	})
	a := newTestAgent(t, newScript().Model(), WithTools(asking))
	r, err := a.Run(ctx, "", WithCheck(Check{Steps: []CheckStep{step("queue.ask", "")}}))
	mustState(t, r, err, StateCompleted)
	if res := checkOutputOf(t, r).Results[0]; !res.IsError || !strings.Contains(string(res.Output), ErrNoAsker.Error()) {
		t.Fatalf("result = %+v (output %s)", res, res.Output)
	}
	if countType(ledgerEvents(t, a.Ledger(), r.ID), EvApprovalRequested) != 0 {
		t.Fatal("the check asked a person")
	}
}

// Resolved fills the defaults, compacts each input, and refuses a check
// out of bounds with a reason that names the field.
func TestCheckResolved(t *testing.T) {
	c, err := Check{Steps: []CheckStep{step("a", ""), step("b", " { \"x\" : 1 } ")}}.Resolved()
	if err != nil || c.Timeout != DefaultCheckTimeout || c.MaxBytes != DefaultCheckMaxBytes || string(c.Steps[0].Input) != "{}" || string(c.Steps[1].Input) != `{"x":1}` {
		t.Fatalf("resolved = %+v, %v", c, err)
	}
	if c, err := (Check{Steps: []CheckStep{step("a", "")}, Timeout: MaxCheckTimeout, MaxBytes: MaxCheckMaxBytes}).Resolved(); err != nil || c.Timeout != MaxCheckTimeout || c.MaxBytes != MaxCheckMaxBytes {
		t.Fatalf("the limits = %+v, %v", c, err)
	}
	one := []CheckStep{step("a", "")}
	for _, tc := range []struct {
		c    Check
		want string
	}{
		{Check{}, "check.steps has 0 steps; a check has 1 to 5"},
		{Check{Steps: make([]CheckStep, 6)}, "check.steps has 6 steps; a check has 1 to 5"},
		{Check{Steps: []CheckStep{step(" ", "")}}, "check.steps[0].tool is empty"},
		{Check{Steps: []CheckStep{step("a", ""), step("b", "[1]")}}, "check.steps[1].input is not a JSON object"},
		{Check{Steps: []CheckStep{step("a", `"text"`)}}, "check.steps[0].input is not a JSON object"},
		{Check{Steps: one, Timeout: 61 * time.Second}, "check.timeout 1m1s is longer than the limit of 1m0s"},
		{Check{Steps: one, Timeout: -time.Second}, "check.timeout -1s is negative"},
		{Check{Steps: one, MaxBytes: MaxCheckMaxBytes + 1}, "check.maxBytes 4194305 is above the limit of 4194304 (4 MiB)"},
		{Check{Steps: one, MaxBytes: -1}, "check.maxBytes -1 is negative"},
	} {
		_, err := tc.c.Resolved()
		if !errors.Is(err, ErrInvalidRequest) || !strings.HasSuffix(err.Error(), tc.want) {
			t.Errorf("Resolved(%+v) = %v; want %q", tc.c, err, tc.want)
		}
	}
}

// In JSON, the timeout of a check is a duration string or a number of
// nanoseconds, and the kernel writes a string.
func TestCheckJSON(t *testing.T) {
	var c Check
	if err := json.Unmarshal([]byte(`{"steps": [{"tool": "a", "input": {"q": 1}}], "timeout": "45s", "maxBytes": 1024}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.Timeout != 45*time.Second || c.MaxBytes != 1024 || len(c.Steps) != 1 || string(c.Steps[0].Input) != `{"q": 1}` {
		t.Fatalf("check = %+v", c)
	}
	b, err := json.Marshal(c)
	if err != nil || string(b) != `{"steps":[{"tool":"a","input":{"q":1}}],"timeout":"45s","maxBytes":1024}` {
		t.Fatalf("marshal = %s, %v", b, err)
	}
	if err := json.Unmarshal([]byte(`{"steps": [], "timeout": 2000000000}`), &c); err != nil || c.Timeout != 2*time.Second {
		t.Fatalf("a number of nanoseconds = %+v, %v", c, err)
	}
	if err := json.Unmarshal([]byte(`{"steps": [], "timeout": "soon"}`), &c); err == nil || !strings.Contains(err.Error(), `check.timeout "soon" is not a duration`) {
		t.Fatalf("a bad timeout = %v", err)
	}
	if b, _ := json.Marshal(Check{Steps: []CheckStep{step("a", "{}")}}); string(b) != `{"steps":[{"tool":"a","input":{}}]}` {
		t.Fatalf("no timeout = %s", b)
	}
}

// The kernel refuses a check that does not fit before it records
// anything, a request that sets the kind itself, and an empty input
// without a check, as before.
func TestCheckRunRefusals(t *testing.T) {
	ctx := testCtx(t)
	a := newTestAgent(t, newScript().Model(), WithTools((&fakeQueue{}).tool("queue.read", TextResult("{}"), nil)))
	if _, err := a.Start(ctx, "", WithCheck(Check{Timeout: time.Hour, Steps: []CheckStep{step("queue.read", "")}})); !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "check.timeout") {
		t.Fatalf("a long timeout = %v", err)
	}
	if _, err := a.Start(ctx, "look", WithMetadata(map[string]string{MetaKind: RunKindCheck})); !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "fw.kind is reserved") {
		t.Fatalf("a request with the kind = %v", err)
	}
	if err := CheckMetadata(map[string]string{MetaKind: "check"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("CheckMetadata = %v", err)
	}
	if _, err := a.Start(ctx, " "); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("an empty input = %v", err)
	}
	if runs, _ := a.List(ctx); len(runs) != 0 {
		t.Fatalf("runs = %d", len(runs))
	}
}

// A check run takes no follow-up, which would start the model, and no
// branch. An annotation cannot change its kind.
func TestCheckRunTakesNoMessage(t *testing.T) {
	ctx := testCtx(t)
	s := newScript(endTurn("never"))
	a := newTestAgent(t, s.Model(), WithTools((&fakeQueue{}).tool("queue.read", TextResult("{}"), nil)))
	r, err := a.Run(ctx, "", WithCheck(Check{Steps: []CheckStep{step("queue.read", "")}}))
	mustState(t, r, err, StateCompleted)
	if err := a.Send(ctx, r.ID, Message{Content: []Block{Text("go on")}}, OriginCLI); !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "check run") {
		t.Fatalf("Send = %v", err)
	}
	if _, err := a.Fork(ctx, r.ID, ForkRequest{Input: "again"}); !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "check run") {
		t.Fatalf("Fork = %v", err)
	}
	if err := a.Annotate(ctx, r.ID, RunAnnotatedData{Metadata: map[string]string{MetaKind: ""}}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Annotate = %v", err)
	}
	if len(s.requests()) != 0 {
		t.Fatal("the model got a call")
	}
}

// A process without a model refuses an ordinary run and takes a check
// run, which calls none.
func TestCheckRunWithoutAModel(t *testing.T) {
	t.Setenv("FW_ATESPACE", "")
	ctx := testCtx(t)
	a, err := New(WithoutDefaults(), WithAuthn(NoAuthn{}), WithLedger(NewMemLedger()), WithBus(NewMemBus()), WithLogger(quietLogger()),
		WithTools((&fakeQueue{}).tool("queue.read", TextResult(`{"n": 0}`), nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(context.Background()) })
	if _, err := a.Start(ctx, "hello"); !errors.Is(err, ErrNoModel) {
		t.Fatalf("an ordinary run = %v", err)
	}
	r, err := a.Run(ctx, "", WithCheck(Check{Steps: []CheckStep{step("queue.read", "")}}))
	mustState(t, r, err, StateCompleted)
	if r.Output != `{"results":[{"tool":"queue.read","output":{"n":0},"isError":false}]}` {
		t.Fatalf("output = %s", r.Output)
	}
}

// A process whose runs a Runner drives runs a check in the turn loop: the
// runner never sees it.
func TestCheckRunSkipsTheRunner(t *testing.T) {
	ctx := testCtx(t)
	var mu sync.Mutex
	var hosted []string
	runner := WithRunner(runnerFunc(func(_ context.Context, h *Hosted) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		hosted = append(hosted, h.ID())
		return false, nil
	}))
	a := newTestAgent(t, newScript(endTurn("done")).Model(), runner, WithTools((&fakeQueue{}).tool("queue.read", TextResult("{}"), nil)))
	r, err := a.Run(ctx, "", WithCheck(Check{Steps: []CheckStep{step("queue.read", "")}}))
	mustState(t, r, err, StateCompleted)
	o, err := a.Run(ctx, "an ordinary run")
	mustState(t, o, err, StateCompleted)
	mu.Lock()
	defer mu.Unlock()
	if len(hosted) == 0 || strings.Contains(strings.Join(hosted, " "), r.ID) {
		t.Fatalf("the runner saw %v; the check run is %s", hosted, r.ID)
	}
}

// After a restart, a check that made no call yet runs, and a check that a
// restart cut off after its first call fails: the kernel never runs a
// check twice.
func TestCheckRunAfterARestart(t *testing.T) {
	l := NewMemLedger()
	chk := &Check{Steps: []CheckStep{step("queue.read", "{}")}, Timeout: DefaultCheckTimeout, MaxBytes: DefaultCheckMaxBytes}
	queued := RunQueuedData{Agent: "default", Origin: OriginBus, Metadata: map[string]string{MetaKind: RunKindCheck}, Check: chk,
		Resolved: &ResolvedProfile{Model: "anthropic/claude-opus-5-5"}}
	seed(t, l, "run-check-fresh",
		MustEvent("run-check-fresh", EvRunQueued, queued),
		MustEvent("run-check-fresh", EvRunStarted, RunStartedData{}))
	seed(t, l, "run-check-cut",
		MustEvent("run-check-cut", EvRunQueued, queued),
		MustEvent("run-check-cut", EvRunStarted, RunStartedData{}),
		MustEvent("run-check-cut", EvToolRequested, ToolRequestedData{Tool: "queue.read", CallID: checkCallPrefix + "1", Input: json.RawMessage(`{}`)}))
	q := &fakeQueue{}
	a := newTestAgent(t, newScript().Model(), WithLedger(l), WithTools(q.tool("queue.read", TextResult(`{"n": 1}`), nil)))
	fresh := waitTerminal(t, a, "run-check-fresh")
	mustState(t, fresh, nil, StateCompleted)
	if fresh.Output != `{"results":[{"tool":"queue.read","output":{"n":1},"isError":false}]}` {
		t.Fatalf("output = %s", fresh.Output)
	}
	cut := waitTerminal(t, a, "run-check-cut")
	mustState(t, cut, nil, StateFailed)
	if cut.Error != checkInterrupted || len(q.calls()) != 1 {
		t.Fatalf("error %q, calls %q", cut.Error, q.calls())
	}
}

// The tools of a check count against budget.maxToolCalls of the run.
func TestCheckRunReachesMaxToolCalls(t *testing.T) {
	ctx := testCtx(t)
	q := &fakeQueue{}
	a := newTestAgent(t, newScript().Model(), WithTools(q.tool("queue.read", TextResult("{}"), nil)))
	r, err := a.Run(ctx, "", WithRunBudget(Budget{MaxToolCalls: 1}), WithCheck(Check{Steps: []CheckStep{step("queue.read", ""), step("queue.read", "")}}))
	mustState(t, r, err, StateFailed)
	if !strings.HasPrefix(r.Error, "check step 2 (queue.read): not run: ") || !strings.Contains(r.Error, "budget.maxToolCalls") || len(q.calls()) != 1 {
		t.Fatalf("error %q, calls %q", r.Error, q.calls())
	}
}

// cutCheck keeps the head of the text on a rune boundary and turns
// structured content without text into text.
func TestCutCheck(t *testing.T) {
	res, cut := cutCheck(TextResult("héllo"), 2)
	if !cut || TextOf(res.Content) != "h" {
		t.Fatalf("cut = %v, %q", cut, TextOf(res.Content))
	}
	res, cut = cutCheck(ToolResult{Structured: json.RawMessage(`{"n":1}`)}, 100)
	if cut || TextOf(res.Content) != `{"n":1}` || res.Structured != nil {
		t.Fatalf("structured = %v, %+v", cut, res)
	}
	res, cut = cutCheck(TextResult("short"), 5)
	if cut || TextOf(res.Content) != "short" {
		t.Fatalf("short = %v, %+v", cut, res)
	}
}
