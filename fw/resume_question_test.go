package fw

// The tests here cover the resume of a tool call that died while it waited
// for the answer to its question. The answer is an fw.approval.decided
// event, and no channel survives the restart. A Continuer tool completes
// the call from the recorded answers; any other non-idempotent tool keeps
// the rerun gate.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// questionSchema is the recorded schema of the questions in the tests.
const questionSchema = `{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`

// resumeAgent builds an agent over a ledger that a restart left behind.
func resumeAgent(t *testing.T, model Model, l Ledger, tools ...Tool) *Agent {
	t.Helper()
	return newTestAgent(t, model, WithLedger(l), WithTools(tools...))
}

// seedQuestionPaused seeds a run that died after its tool asked a question.
// The question is a1, from call c1. answered decides it in the ledger; nil
// leaves it unanswered.
func seedQuestionPaused(t *testing.T, l Ledger, id, tool, input string, answered *ApprovalDecidedData, extra ...Event) {
	t.Helper()
	evs := []Event{
		MustEvent(id, EvRunQueued, RunQueuedData{Agent: "default", Input: "hi", Origin: OriginCLI}),
		MustEvent(id, EvRunStarted, RunStartedData{}),
		MustEvent(id, EvTurnStarted, TurnData{Turn: 1}),
		MustEvent(id, EvModelRequested, ModelRequestedData{}),
		MustEvent(id, EvModelResponded, ModelRespondedData{Content: []Block{use("c1", tool, input)}, StopReason: StopToolUse}),
		MustEvent(id, EvToolRequested, ToolRequestedData{Tool: tool, CallID: "c1", Input: json.RawMessage(input)}),
		MustEvent(id, EvApprovalRequested, ApprovalRequestedData{
			ApprovalID: "a1", Tool: tool, CallID: "c1",
			Reason: "Pick a name", Schema: json.RawMessage(questionSchema),
		}),
	}
	if answered != nil {
		evs = append(evs, MustEvent(id, EvApprovalDecided, *answered))
	}
	seed(t, l, id, append(evs, extra...)...)
}

// continuerTool completes a resumed call from the recorded answers, and
// counts the runs and continuations of itself. huge and sleep give the
// continuation work that the limits middleware must bound.
type continuerTool struct {
	runs      atomic.Int32
	continues atomic.Int32
	huge      string        // a large result for an output-limit test
	sleep     time.Duration // the time that a continuation takes

	mu   sync.Mutex
	last []string // what each answered question decided, in order
}

func (t *continuerTool) Spec() ToolSpec {
	return ToolSpec{Name: "pick", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (t *continuerTool) Call(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
	t.runs.Add(1)
	// A resumed call must not reach the caller through an asker when the
	// record of the run already holds the answer.
	if _, err := AskCaller(ctx, "Pick a name", []byte(questionSchema)); err == nil {
		return ErrorResult("pick: the asker is present"), nil
	}
	return TextResult("picked"), nil
}

// Continue implements Continuer.
func (t *continuerTool) Continue(ctx context.Context, _ json.RawMessage, answered []AnsweredQuestion) (ToolResult, error) {
	t.continues.Add(1)
	// A continuation must not ask the caller again.
	if _, err := AskCaller(ctx, "Pick a name", []byte(questionSchema)); !errors.Is(err, ErrNoAsker) {
		return ErrorResult("pick: the asker is present in the continuation"), nil
	}
	if t.sleep > 0 {
		timer := time.NewTimer(t.sleep)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ToolResult{}, context.Cause(ctx)
		case <-timer.C:
		}
	}
	if t.huge != "" {
		return TextResult(t.huge), nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, q := range answered {
		if q.Decision.Decision != "allow" {
			return ErrorResult("name denied:" + q.Decision.Note), nil
		}
		t.last = append(t.last, string(q.Decision.Content))
	}
	return TextResult("name " + joinAnswers(t.last)), nil
}

func joinAnswers(parts []string) string {
	if len(parts) == 0 {
		return "(no answer)"
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += "," + p
	}
	return out
}

// wrappedTool is middleware that hides the tool behind a wrapper. The
// kernel must still find a Continuer behind it.
type wrappedTool struct{ Tool }

func (w wrappedTool) Unwrap() Tool { return w.Tool }

// firstToolResult returns the first tool_result block that the model of a
// test script received in its latest request.
func firstToolResult(t *testing.T, s *scriptModel) Block {
	t.Helper()
	reqs := s.requests()
	if len(reqs) == 0 {
		t.Fatal("the model saw no request")
	}
	blocks := lastToolResults(t, reqs[len(reqs)-1])
	if len(blocks) == 0 {
		t.Fatal("the request holds no tool result")
	}
	return blocks[0]
}

// lastOf returns the last event of a type.
func lastOf(t *testing.T, evs []Event, typ string, v any) Event {
	t.Helper()
	var found Event
	for _, e := range evs {
		if e.Type == typ {
			found = e
		}
	}
	if found.Type == "" {
		t.Fatalf("no %s event in %v", typ, eventTypes(evs))
	}
	if v != nil {
		if err := found.Decode(v); err != nil {
			t.Fatal(err)
		}
	}
	return found
}

func TestResumeQuestion(t *testing.T) {
	ctx := testCtx(t)

	t.Run("durable answer completes the call", func(t *testing.T) {
		l := NewMemLedger()
		id := "rq1"
		seedQuestionPaused(t, l, id, "pick", `{}`,
			&ApprovalDecidedData{ApprovalID: "a1", Decision: "allow", Content: json.RawMessage(`{"answer":"Ada"}`)})
		tool := &continuerTool{}
		s := newScript(endTurn("done"))
		a := resumeAgent(t, s.Model(), l, tool)
		r, err := a.Wait(ctx, id)
		mustState(t, r, err, StateCompleted)
		if tool.runs.Load() != 0 || tool.continues.Load() != 1 {
			t.Fatalf("runs = %d, continues = %d", tool.runs.Load(), tool.continues.Load())
		}
		// The model received the result of the completed call.
		b := firstToolResult(t, s)
		if TextOf(b.Content) != `name {"answer":"Ada"}` {
			t.Fatalf("tool result = %+v", b)
		}
		// The resume asked no new question.
		evs := ledgerEvents(t, a.Ledger(), id)
		if n := countType(evs, EvApprovalRequested); n != 1 {
			t.Fatalf("approval.requested x%d in %v", n, eventTypes(evs))
		}
		// The resume recorded the recovery class of the call: pick is
		// not idempotent, and the ledger says so.
		var d ToolRequestedData
		lastOf(t, evs, EvToolRequested, &d)
		if d.Idempotent {
			t.Fatal("the resumed fw.tool.requested marks the tool idempotent")
		}
	})

	t.Run("answer decided after the restart", func(t *testing.T) {
		l := NewMemLedger()
		id := "rq2"
		seedQuestionPaused(t, l, id, "pick", `{}`, nil)
		tool := &continuerTool{}
		s := newScript(endTurn("done"))
		a := resumeAgent(t, s.Model(), l, tool)
		r, err := a.Get(ctx, id)
		mustState(t, r, err, StateInputRequired)
		if r.Pending.ID != "a1" || string(r.Pending.Schema) != questionSchema {
			t.Fatalf("pending = %+v", r.Pending)
		}
		if err := a.Decide(ctx, id, ApprovalDecision{ApprovalID: "a1", Content: json.RawMessage(`{"answer":"Ada"}`)}); err != nil {
			t.Fatal(err)
		}
		r, err = a.Wait(ctx, id)
		mustState(t, r, err, StateCompleted)
		if tool.runs.Load() != 0 || tool.continues.Load() != 1 {
			t.Fatalf("runs = %d, continues = %d", tool.runs.Load(), tool.continues.Load())
		}
		if TextOf(firstToolResult(t, s).Content) != `name {"answer":"Ada"}` {
			t.Fatalf("tool result = %+v", firstToolResult(t, s))
		}
		if n := countType(ledgerEvents(t, a.Ledger(), id), EvApprovalRequested); n != 1 {
			t.Fatal("the resume asked a fresh question")
		}
	})

	t.Run("denied answer recovers as an error result", func(t *testing.T) {
		l := NewMemLedger()
		id := "rq3"
		seedQuestionPaused(t, l, id, "pick", `{}`, nil)
		tool := &continuerTool{}
		s := newScript(endTurn("done"))
		a := resumeAgent(t, s.Model(), l, tool)
		if err := a.Decide(ctx, id, ApprovalDecision{ApprovalID: "a1", Decision: "deny", Note: "no"}); err != nil {
			t.Fatal(err)
		}
		r, err := a.Wait(ctx, id)
		mustState(t, r, err, StateCompleted)
		if tool.runs.Load() != 0 || tool.continues.Load() != 1 {
			t.Fatalf("runs = %d, continues = %d", tool.runs.Load(), tool.continues.Load())
		}
		b := firstToolResult(t, s)
		if !b.IsError || TextOf(b.Content) != "name denied:no" {
			t.Fatalf("tool result = %+v", b)
		}
	})

	t.Run("every question of the call, in order", func(t *testing.T) {
		l := NewMemLedger()
		id := "rq4"
		seedQuestionPaused(t, l, id, "pick", `{}`, nil,
			MustEvent(id, EvApprovalRequested, ApprovalRequestedData{
				ApprovalID: "a2", Tool: "pick", CallID: "c1",
				Reason: "Pick a color", Schema: json.RawMessage(questionSchema),
			}),
			MustEvent(id, EvApprovalDecided, ApprovalDecidedData{ApprovalID: "a1", Decision: "allow", Content: json.RawMessage(`{"answer":"Ada"}`)}),
			MustEvent(id, EvApprovalDecided, ApprovalDecidedData{ApprovalID: "a2", Decision: "allow", Content: json.RawMessage(`{"answer":"red"}`)}),
		)
		tool := &continuerTool{}
		s := newScript(endTurn("done"))
		a := resumeAgent(t, s.Model(), l, tool)
		r, err := a.Wait(ctx, id)
		mustState(t, r, err, StateCompleted)
		if tool.continues.Load() != 1 {
			t.Fatalf("continues = %d", tool.continues.Load())
		}
		if got := TextOf(firstToolResult(t, s).Content); got != `name {"answer":"Ada"},{"answer":"red"}` {
			t.Fatalf("tool result = %q", got)
		}
	})

	t.Run("the hook works behind middleware", func(t *testing.T) {
		l := NewMemLedger()
		id := "rq6"
		seedQuestionPaused(t, l, id, "pick", `{}`,
			&ApprovalDecidedData{ApprovalID: "a1", Decision: "allow", Content: json.RawMessage(`{"answer":"Ada"}`)})
		tool := &continuerTool{}
		s := newScript(endTurn("done"))
		a := resumeAgent(t, s.Model(), l, wrappedTool{tool})
		r, err := a.Wait(ctx, id)
		mustState(t, r, err, StateCompleted)
		if tool.continues.Load() != 1 || tool.runs.Load() != 0 {
			t.Fatalf("runs = %d, continues = %d", tool.runs.Load(), tool.continues.Load())
		}
		if TextOf(firstToolResult(t, s).Content) != `name {"answer":"Ada"}` {
			t.Fatalf("tool result = %+v", firstToolResult(t, s))
		}
	})

	t.Run("second restart still uses the one answer", func(t *testing.T) {
		l := NewMemLedger()
		id := "rq5"
		// The ledger of a run that resumed, recorded its open call once
		// more, and died again before the continuation finished.
		seedQuestionPaused(t, l, id, "pick", `{}`,
			&ApprovalDecidedData{ApprovalID: "a1", Decision: "allow", Content: json.RawMessage(`{"answer":"Ada"}`)},
			MustEvent(id, EvRunResumed, RunResumedData{FromSeq: 7}),
			MustEvent(id, EvToolRequested, ToolRequestedData{Tool: "pick", CallID: "c1", Input: json.RawMessage(`{}`)}),
		)
		tool := &continuerTool{}
		s := newScript(endTurn("done"))
		a := resumeAgent(t, s.Model(), l, tool)
		r, err := a.Wait(ctx, id)
		mustState(t, r, err, StateCompleted)
		if tool.continues.Load() != 1 || tool.runs.Load() != 0 {
			t.Fatalf("runs = %d, continues = %d", tool.runs.Load(), tool.continues.Load())
		}
		if TextOf(firstToolResult(t, s).Content) != `name {"answer":"Ada"}` {
			t.Fatalf("tool result = %+v", firstToolResult(t, s))
		}
		if n := countType(ledgerEvents(t, a.Ledger(), id), EvApprovalRequested); n != 1 {
			t.Fatal("the second resume asked again")
		}
	})
}

func TestResumeQuestionGate(t *testing.T) {
	ctx := testCtx(t)
	input := `{"question":"Pick a name"}`

	t.Run("non-idempotent asker keeps the rerun gate", func(t *testing.T) {
		l := NewMemLedger()
		id := "rg1"
		// The call asked a question, and the ledger holds an answer from
		// before the restart. The tool declares no Continuer, so the
		// kernel cannot hand the answer to a tool that it will run again.
		seedQuestionPaused(t, l, id, "asker", input,
			&ApprovalDecidedData{ApprovalID: "a1", Decision: "allow", Content: json.RawMessage(`{"answer":"Ann"}`)})
		tool := &askerTool{}
		s := newScript(endTurn("done"))
		a := resumeAgent(t, s.Model(), l, tool)
		r, err := a.Wait(ctx, id)
		mustState(t, r, err, StateInputRequired)
		if r.Pending.Reason != "rerun after resume" || r.Pending.ID == "a1" || tool.ran.Load() != 0 {
			t.Fatalf("pending = %+v, ran = %d", r.Pending, tool.ran.Load())
		}
		// An allow reruns the call, and the tool asks its own question
		// again. The recorded answer never reaches the new question.
		if err := a.Decide(ctx, id, ApprovalDecision{ApprovalID: r.Pending.ID, Decision: "allow"}); err != nil {
			t.Fatal(err)
		}
		r, err = a.Wait(ctx, id)
		mustState(t, r, err, StateInputRequired)
		if r.Pending.Reason != "Pick a name" || tool.ran.Load() != 1 {
			t.Fatalf("pending = %+v, ran = %d", r.Pending, tool.ran.Load())
		}
		if err := a.Decide(ctx, id, ApprovalDecision{ApprovalID: r.Pending.ID, Content: json.RawMessage(`{"answer":"Bob"}`)}); err != nil {
			t.Fatal(err)
		}
		r, err = a.Wait(ctx, id)
		mustState(t, r, err, StateCompleted)
		b := firstToolResult(t, s)
		if TextOf(b.Content) != "asked Bob" {
			t.Fatalf("tool result = %+v", b)
		}
	})

	t.Run("pending question stays idle", func(t *testing.T) {
		l := NewMemLedger()
		id := "rg2"
		seedQuestionPaused(t, l, id, "asker", input, nil)
		tool := &askerTool{}
		s := newScript(endTurn("done"))
		a := resumeAgent(t, s.Model(), l, tool)
		r, err := a.Get(ctx, id)
		mustState(t, r, err, StateInputRequired)
		if r.Pending.ID != "a1" || r.Pending.Reason != "Pick a name" {
			t.Fatalf("pending = %+v", r.Pending)
		}
		if tool.ran.Load() != 0 {
			t.Fatal("the tool ran while the question waited")
		}
		evs := ledgerEvents(t, a.Ledger(), id)
		if n := countType(evs, EvToolResponded); n != 0 {
			t.Fatalf("tool.responded x%d while the question waited", n)
		}
		if n := countType(evs, EvToolRequested); n != 1 {
			t.Fatalf("tool.requested x%d while the question waited", n)
		}
	})

	t.Run("idempotent asker still asks fresh", func(t *testing.T) {
		l := NewMemLedger()
		id := "rg3"
		seedQuestionPaused(t, l, id, "askerp", input,
			&ApprovalDecidedData{ApprovalID: "a1", Decision: "allow", Content: json.RawMessage(`{"answer":"Ann"}`)})
		tool := &askerTool{spec: ToolSpec{Name: "askerp", Idempotent: true}}
		s := newScript(endTurn("done"))
		a := resumeAgent(t, s.Model(), l, tool)
		r, err := a.Wait(ctx, id)
		mustState(t, r, err, StateInputRequired)
		if r.Pending.Reason != "Pick a name" || r.Pending.ID == "a1" || tool.ran.Load() != 1 {
			t.Fatalf("pending = %+v, ran = %d", r.Pending, tool.ran.Load())
		}
	})
}

func TestResumeQuestionLimits(t *testing.T) {
	ctx := testCtx(t)
	// answeredLedger seeds a run whose recorded question has an allow.
	base := func(t *testing.T, id string) Ledger {
		t.Helper()
		l := NewMemLedger()
		seedQuestionPaused(t, l, id, "pick", `{}`,
			&ApprovalDecidedData{ApprovalID: "a1", Decision: "allow", Content: json.RawMessage(`{"answer":"Ada"}`)})
		return l
	}

	t.Run("a large continued answer keeps its bound", func(t *testing.T) {
		l := base(t, "rl1")
		tool := &continuerTool{huge: strings.Repeat("x", 4000)}
		limits := ToolLimits{Limit: Limit{MaxInputBytes: 64 << 10, MaxOutputBytes: 128, Timeout: time.Minute}}
		s := newScript(endTurn("done"))
		a := newTestAgent(t, s.Model(), WithLedger(l), WithToolMiddleware(LimitTools(limits)), WithTools(tool))
		r, err := a.Wait(ctx, "rl1")
		mustState(t, r, err, StateCompleted)
		b := firstToolResult(t, s)
		text := TextOf(b.Content)
		if !strings.Contains(text, "bytes cut here") || len(text) > 300 {
			t.Fatalf("tool result is not bounded: %d bytes, %q", len(text), text)
		}
	})

	t.Run("an oversized input is refused before the continuation", func(t *testing.T) {
		l := NewMemLedger()
		input := `{"q":"` + strings.Repeat("y", 96) + `"}`
		seedQuestionPaused(t, l, "rl2", "pick", input,
			&ApprovalDecidedData{ApprovalID: "a1", Decision: "allow", Content: json.RawMessage(`{"answer":"Ada"}`)})
		// A call input over the limit cannot reach the tool, and a
		// continuation is no exception: the refusal happens before
		// Continue, so the tool never reacts to the recorded answer.
		tool := &continuerTool{}
		limits := ToolLimits{Limit: Limit{MaxInputBytes: 64, MaxOutputBytes: 64 << 10, Timeout: time.Minute}}
		s := newScript(endTurn("done"))
		a := newTestAgent(t, s.Model(), WithLedger(l), WithToolMiddleware(LimitTools(limits)), WithTools(tool))
		r, err := a.Wait(ctx, "rl2")
		mustState(t, r, err, StateCompleted)
		b := firstToolResult(t, s)
		if tool.continues.Load() != 0 {
			t.Fatal("the continuation ran behind the refusal")
		}
		if !b.IsError || !strings.Contains(TextOf(b.Content), "the limit is 64 bytes") {
			t.Fatalf("tool result = %+v", b)
		}
	})

	t.Run("a slow continuation keeps its timeout", func(t *testing.T) {
		l := base(t, "rl3")
		tool := &continuerTool{sleep: 200 * time.Millisecond}
		limits := ToolLimits{Limit: Limit{MaxInputBytes: 64 << 10, MaxOutputBytes: 64 << 10, Timeout: 20 * time.Millisecond}}
		s := newScript(endTurn("done"))
		a := newTestAgent(t, s.Model(), WithLedger(l), WithToolMiddleware(LimitTools(limits)), WithTools(tool))
		r, err := a.Wait(ctx, "rl3")
		mustState(t, r, err, StateCompleted)
		b := firstToolResult(t, s)
		if !b.IsError || !strings.Contains(TextOf(b.Content), "did not finish within") {
			t.Fatalf("tool result = %+v", b)
		}
	})

	t.Run("an ordinary tool behind limits keeps the rerun gate", func(t *testing.T) {
		l := base(t, "rl4")
		tool := &askerTool{spec: ToolSpec{Name: "pick", InputSchema: json.RawMessage(`{"type":"object"}`), Idempotent: false}}
		limits := ToolLimits{Limit: Limit{MaxInputBytes: 64 << 10, MaxOutputBytes: 64 << 10, Timeout: time.Minute}}
		s := newScript(endTurn("done"))
		a := newTestAgent(t, s.Model(), WithLedger(l), WithToolMiddleware(LimitTools(limits)), WithTools(tool))
		r, err := a.Wait(ctx, "rl4")
		mustState(t, r, err, StateInputRequired)
		if r.Pending.Reason != "rerun after resume" || tool.ran.Load() != 0 {
			t.Fatalf("pending = %+v, ran = %d", r.Pending, tool.ran.Load())
		}
	})
}

// askerTool is a plain tool that asks, with no Continuer. A zero tool is
// not idempotent, so a resumed call that it holds keeps the rerun gate.
type askerTool struct {
	spec ToolSpec
	ran  atomic.Int32
}

func (t *askerTool) Spec() ToolSpec {
	if t.spec.Name != "" {
		return t.spec
	}
	return ToolSpec{Name: "asker", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (t *askerTool) Call(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
	t.ran.Add(1)
	raw, err := AskCaller(ctx, "Pick a name", []byte(questionSchema))
	if err != nil {
		return ToolResult{}, err
	}
	var v struct {
		Answer string `json:"answer"`
	}
	if err := json.Unmarshal(raw, &v); err == nil && v.Answer != "" {
		return TextResult("asked " + v.Answer), nil
	}
	return TextResult("asked"), nil
}
