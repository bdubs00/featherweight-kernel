package fw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// stepsModel answers turn requests from a list of steps, each a *Response
// or an error, and answers a summary request with a fixed summary. It
// records both kinds of request.
type stepsModel struct {
	mu        sync.Mutex
	steps     []any
	turns     []*Request
	summaries []*Request
}

func (s *stepsModel) Model() Model {
	return ModelFunc{StreamFn: func(_ context.Context, req *Request, _ func(Delta)) (*Response, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if n := len(req.Messages); n > 0 && TextOf(req.Messages[n-1].Content) == compactPrompt {
			s.summaries = append(s.summaries, req)
			return &Response{
				Content:    []Block{Text("<summary>SUMMARY</summary>")},
				StopReason: StopEndTurn,
				Usage:      Usage{InputTokens: 7, OutputTokens: 3, CacheReadTokens: 100},
				Provider:   "fake",
				Model:      "summarizer",
			}, nil
		}
		s.turns = append(s.turns, req)
		if len(s.steps) == 0 {
			return nil, errors.New("steps: no step left")
		}
		st := s.steps[0]
		s.steps = s.steps[1:]
		if err, ok := st.(error); ok {
			return nil, err
		}
		return st.(*Response), nil
	}}
}

var errOverflow = errors.New("anthropic: stream claude-opus-5-5: 400 invalid_request_error: prompt is too long: 1048000 tokens > 1000000 maximum")

func TestOverflowCompactsAndRetries(t *testing.T) {
	ctx := testCtx(t)
	s := &stepsModel{steps: []any{callTools(use("c1", "echo", `{"text":"one"}`)), errOverflow, endTurn("done")}}
	a := newTestAgent(t, s.Model(), WithTools(echoTool(nil)))
	r, err := a.Run(ctx, "work")
	mustState(t, r, err, StateCompleted)
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	if n := countType(evs, EvContextCompacted); n != 1 {
		t.Fatalf("compactions = %d in %v", n, eventTypes(evs))
	}
	if n := countType(evs, EvTurnStarted); n != 2 {
		t.Fatalf("turns = %d: the retry belongs to the turn that overflowed", n)
	}
	var d ErrorData
	firstOf(t, evs, EvError, &d)
	if !d.Retryable || !strings.Contains(d.Message, "prompt is too long") || !strings.Contains(d.Message, "compacting") {
		t.Fatalf("error = %+v", d)
	}
	if len(s.summaries) != 1 || len(s.turns) != 3 {
		t.Fatalf("summaries = %d, turns = %d", len(s.summaries), len(s.turns))
	}
	// The summary call has the prefix of a turn: the same system prompt and
	// tools, and the history with the instruction after it.
	sum, turn := s.summaries[0], s.turns[1]
	if sum.System != turn.System || len(sum.Tools) != len(turn.Tools) || sum.Effort != turn.Effort || sum.MaxTokens != turn.MaxTokens {
		t.Fatalf("summary request prefix differs from the turn")
	}
	if len(sum.Messages) != len(turn.Messages)+1 || TextOf(sum.Messages[0].Content) != "work" {
		t.Fatalf("summary messages = %+v", sum.Messages)
	}
	retry := s.turns[2].Messages
	if len(retry) != 1 || !strings.Contains(TextOf(retry[0].Content), "SUMMARY") {
		t.Fatalf("history after compaction = %+v", retry)
	}
	// The run usage, and so the budget, counts the summary call.
	var c ContextCompactedData
	firstOf(t, evs, EvContextCompacted, &c)
	if c.Usage == nil || c.Usage.CacheReadTokens != 100 || c.Model != "summarizer" {
		t.Fatalf("compacted = %+v", c)
	}
	if u := r.Usage; u.InputTokens != 27 || u.OutputTokens != 13 || u.CacheReadTokens != 100 {
		t.Fatalf("run usage = %+v", u)
	}
}

func TestSecondOverflowFailsTheRun(t *testing.T) {
	ctx := testCtx(t)
	s := &stepsModel{steps: []any{callTools(use("c1", "echo", `{}`)), errOverflow, errOverflow}}
	a := newTestAgent(t, s.Model(), WithTools(echoTool(nil)))
	r, err := a.Run(ctx, "work")
	mustState(t, r, err, StateFailed)
	if !strings.Contains(r.Error, "prompt is too long") {
		t.Fatalf("error = %q", r.Error)
	}
	if n := countType(ledgerEvents(t, a.Ledger(), r.ID), EvContextCompacted); n != 1 {
		t.Fatalf("compactions = %d", n)
	}
}

func TestOverflowWithCompactionOffFails(t *testing.T) {
	ctx := testCtx(t)
	s := &stepsModel{steps: []any{callTools(use("c1", "echo", `{}`)), errOverflow}}
	a := newTestAgent(t, s.Model(), WithTools(echoTool(nil)))
	a.k.compactAt = 0
	r, err := a.Run(ctx, "work")
	mustState(t, r, err, StateFailed)
	if n := countType(ledgerEvents(t, a.Ledger(), r.ID), EvContextCompacted); n != 0 || len(s.summaries) != 0 {
		t.Fatalf("compactions = %d, summaries = %d", n, len(s.summaries))
	}
}

func TestCompactionUsesReportedUsage(t *testing.T) {
	ctx := testCtx(t)
	big := callTools(use("c1", "echo", `{}`))
	big.Usage = Usage{InputTokens: 4000, CacheReadTokens: 1000}
	s := newScript(big, endTurn("done"))
	a := newTestAgent(t, s.Model(), WithTools(echoTool(nil)))
	// The text of the run is far below the threshold. The usage of the
	// first response is above it.
	a.k.compactAt = 2000
	r, err := a.Run(ctx, "work")
	mustState(t, r, err, StateCompleted)
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	if n := countType(evs, EvContextCompacted); n != 1 {
		t.Fatalf("compactions = %d in %v", n, eventTypes(evs))
	}
	var req ModelRequestedData
	firstOf(t, evs, EvModelRequested, &req)
	if req.EstimatedInputTokens <= 0 || req.EstimatedInputTokens >= 2000 {
		t.Fatalf("estimate of the first request = %d", req.EstimatedInputTokens)
	}
}

func TestContextEstimate(t *testing.T) {
	var b historyBuilder
	b.apply(MustEvent("R", EvRunQueued, RunQueuedData{Input: strings.Repeat("x", 400)}))
	if got := b.contextTokens(50); got != 150 {
		t.Fatalf("estimate before a response = %d, want 150", got)
	}
	b.apply(MustEvent("R", EvModelResponded, ModelRespondedData{
		Content:    []Block{Text("hi"), use("c1", "echo", `{}`)},
		StopReason: StopToolUse,
		Usage:      Usage{InputTokens: 1000, CacheReadTokens: 500, CacheWriteTokens: 200, OutputTokens: 100},
	}))
	if got := b.contextTokens(50); got != 1800 {
		t.Fatalf("estimate after a response = %d, want 1800", got)
	}
	b.apply(MustEvent("R", EvToolResponded, ToolRespondedData{CallID: "c1", Content: []Block{Text(strings.Repeat("y", 4000))}}))
	if got := b.contextTokens(50); got != 2800 {
		t.Fatalf("estimate after a tool result = %d, want 2800", got)
	}
	img := []Block{{Type: BlockImage, MediaType: "image/png", Data: make([]byte, 1<<20)}}
	if got := estimateTokens(img); got != imageTokens {
		t.Fatalf("image estimate = %d", got)
	}
}

func TestSummaryKeepsTheCallerTail(t *testing.T) {
	ctx := testCtx(t)
	var got *Request
	m := ModelFunc{GenerateFn: func(_ context.Context, req *Request) (*Response, error) {
		got = req
		return &Response{Content: []Block{Text("before <summary>\nS\n</summary> after")}}, nil
	}}
	opaque := Block{Type: BlockOpaque, Provider: "fake", Raw: json.RawMessage(`{"thinking":"x"}`)}
	msgs := []Message{
		UserText("task"),
		{Role: RoleAssistant, Content: []Block{opaque, use("c1", "echo", `{}`)}},
		{Role: RoleUser, Content: []Block{{Type: BlockToolResult, ToolUseID: "c1", Content: []Block{Text("r1")}}}},
		UserText("follow-up"),
	}
	sum, keep, err := summarizeHistory(ctx, m, History{Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 5 || TextOf(got.Messages[4].Content) != compactPrompt {
		t.Fatalf("summary request = %+v", got.Messages)
	}
	if !strings.Contains(sum.Text, "<summary>\nS\n</summary>") || strings.Contains(sum.Text, "after") || strings.Contains(sum.Text, "came after") {
		t.Fatalf("summary = %q", sum.Text)
	}
	if len(keep) != 1 || TextOf(keep[0].Content) != "follow-up" {
		t.Fatalf("keep = %+v", keep)
	}
	if _, _, err := summarizeHistory(ctx, m, History{Messages: []Message{UserText("only")}}); !errors.Is(err, errNothingToCompact) {
		t.Fatalf("error = %v, want errNothingToCompact", err)
	}
}

func TestSummaryThatDoesNotFitStepsBack(t *testing.T) {
	ctx := testCtx(t)
	var sizes []int
	m := ModelFunc{GenerateFn: func(_ context.Context, req *Request) (*Response, error) {
		sizes = append(sizes, len(req.Messages))
		if len(req.Messages) > 4 {
			return nil, fmt.Errorf("openai: HTTP 400: %w", ErrContextOverflow)
		}
		return &Response{Content: []Block{Text("<summary>S</summary>")}}, nil
	}}
	opaque := Block{Type: BlockOpaque, Provider: "fake", Raw: json.RawMessage(`{"thinking":"x"}`)}
	msgs := []Message{
		UserText("task"),
		{Role: RoleAssistant, Content: []Block{opaque, use("c1", "echo", `{"text":"a"}`)}},
		{Role: RoleUser, Content: []Block{{Type: BlockToolResult, ToolUseID: "c1", Content: []Block{Text("r1")}}}},
		{Role: RoleAssistant, Content: []Block{opaque, use("c2", "echo", `{"text":"b"}`)}},
		{Role: RoleUser, Content: []Block{{Type: BlockToolResult, ToolUseID: "c2", Content: []Block{Text(strings.Repeat("z", 5000))}}}},
		UserText("question"),
	}
	sum, keep, err := summarizeHistory(ctx, m, History{Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(sizes) != "[7 4]" {
		t.Fatalf("summary request sizes = %v", sizes)
	}
	for _, want := range []string{"<summary>\nS\n</summary>", "came after", "called tool echo", "tool result: zzz", "[cut]"} {
		if !strings.Contains(sum.Text, want) {
			t.Fatalf("summary lacks %q: %q", want, sum.Text)
		}
	}
	if strings.Contains(sum.Text, "thinking") {
		t.Fatalf("the summary carries provider data: %q", sum.Text)
	}
	if len(keep) != 1 || TextOf(keep[0].Content) != "question" {
		t.Fatalf("keep = %+v", keep)
	}
}

// briefTool is a tool that adds a section to the system prompt.
type briefTool struct {
	text string
	mu   sync.Mutex
	runs []RunInfo
}

func (b *briefTool) Spec() ToolSpec { return ToolSpec{Name: "notes", ReadOnly: true} }

func (b *briefTool) Call(context.Context, json.RawMessage) (ToolResult, error) {
	return TextResult("ok"), nil
}

func (b *briefTool) Brief(_ context.Context, run RunInfo) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.runs = append(b.runs, run)
	return b.text, nil
}

func TestBriefEntersTheSystemPromptAndTheReplay(t *testing.T) {
	ctx := testCtx(t)
	early := &briefTool{text: "# Notes\n\nEARLIER\n"}
	s := newScript(endTurn("done"))
	a := newTestAgent(t, s.Model(), WithTools(early, NewTool(ToolSpec{Name: "plain"}, nil)))
	r, err := a.Run(ctx, "work")
	mustState(t, r, err, StateCompleted)
	var d RunStartedData
	firstOf(t, ledgerEvents(t, a.Ledger(), r.ID), EvRunStarted, &d)
	if d.Brief != "# Notes\n\nEARLIER" {
		t.Fatalf("brief = %q", d.Brief)
	}
	// The limits middleware wraps the tool, and the kernel finds the
	// Briefer behind it.
	if sys := s.requests()[0].System; !strings.HasSuffix(sys, "\n\n# Notes\n\nEARLIER") {
		t.Fatalf("system prompt = %q", sys)
	}
	if len(early.runs) != 1 || early.runs[0].RunID != r.ID || early.runs[0].Agent != "default" {
		t.Fatalf("brief runs = %+v", early.runs)
	}

	late := &briefTool{text: "LATER"}
	s2 := newScript(endTurn("replayed"))
	b := newTestAgent(t, s2.Model(), WithTools(late))
	rr, err := Replay(ctx, b, a.Ledger(), r.ID)
	mustState(t, rr, err, StateCompleted)
	sys := s2.requests()[0].System
	if !strings.Contains(sys, "EARLIER") || strings.Contains(sys, "LATER") || len(late.runs) != 0 {
		t.Fatalf("replay system prompt = %q, brief calls = %d", sys, len(late.runs))
	}
}

func TestIsContextOverflow(t *testing.T) {
	yes := []error{
		errOverflow,
		fmt.Errorf("model: %w", ErrContextOverflow),
		errors.New("anthropic: 400: input length and `max_tokens` exceed context limit: 990000 + 64000 > 1000000"),
		errors.New("openai: HTTP 400 Bad Request: This model's maximum context length is 32768 tokens. However, you requested 40000 tokens"),
		errors.New(`openai: HTTP 400: {"code":"context_length_exceeded"}`),
		errors.New("gemini: Error 400, Message: The input token count (1200000) exceeds the maximum number of tokens allowed (1048576)."),
		errors.New("openai: HTTP 400: the request exceeds the available context size, try increasing it"),
		errors.New("openai: HTTP 400: Prompt length 40000 is greater than max_seq_len 32768"),
	}
	for _, err := range yes {
		if !IsContextOverflow(err) {
			t.Errorf("IsContextOverflow(%q) = false", err)
		}
	}
	no := []error{nil, errors.New("openai: HTTP 429: Rate limit reached for tokens per min"), context.DeadlineExceeded}
	for _, err := range no {
		if IsContextOverflow(err) {
			t.Errorf("IsContextOverflow(%v) = true", err)
		}
	}
}
