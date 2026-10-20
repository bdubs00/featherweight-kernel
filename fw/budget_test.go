package fw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// noReport turns the final report off in a budget.
func noReport() *bool {
	off := false
	return &off
}

// callSteps answers each call with the next step and records the requests.
// A step that blocks reads ctx, so a test sees how the kernel ends a call.
type callSteps struct {
	mu    sync.Mutex
	steps []func(ctx context.Context, req *Request) (*Response, error)
	reqs  []*Request
}

func newCallSteps(steps ...func(ctx context.Context, req *Request) (*Response, error)) *callSteps {
	return &callSteps{steps: steps}
}

func (s *callSteps) Model() Model {
	return ModelFunc{StreamFn: func(ctx context.Context, req *Request, _ func(Delta)) (*Response, error) {
		s.mu.Lock()
		s.reqs = append(s.reqs, req)
		if len(s.steps) == 0 {
			s.mu.Unlock()
			return nil, errors.New("steps: no step left")
		}
		step := s.steps[0]
		s.steps = s.steps[1:]
		s.mu.Unlock()
		return step(ctx, req)
	}}
}

func (s *callSteps) requests() []*Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*Request(nil), s.reqs...)
}

// answer is a step that returns a response at once.
func answer(r *Response) func(context.Context, *Request) (*Response, error) {
	return func(context.Context, *Request) (*Response, error) { return r, nil }
}

// hang is a step that waits until the kernel ends the call.
func hang(started chan<- struct{}) func(context.Context, *Request) (*Response, error) {
	return func(ctx context.Context, _ *Request) (*Response, error) {
		if started != nil {
			close(started)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
}

// echoCall is a response with one echo call whose input differs by n, so
// the loop guard stays quiet.
func echoCall(n int) *Response {
	return callTools(use(fmt.Sprintf("c%d", n), "echo", fmt.Sprintf(`{"text":"%d"}`, n)))
}

// budgetNotesOf returns the budget notes of a run in order.
func budgetNotesOf(t *testing.T, evs []Event) []MessageReceivedData {
	t.Helper()
	var out []MessageReceivedData
	for _, e := range evs {
		if e.Type != EvMessageReceived {
			continue
		}
		var d MessageReceivedData
		if err := e.Decode(&d); err != nil {
			t.Fatal(err)
		}
		if d.Note == NoteBudget {
			out = append(out, d)
		}
	}
	return out
}

// lastSystem returns the text of the last system message of a request.
func lastSystem(req *Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == RoleSystem {
			return TextOf(req.Messages[i].Content)
		}
	}
	return ""
}

// systemTexts joins the text of every system message of a request.
func systemTexts(req *Request) string {
	var parts []string
	for _, m := range req.Messages {
		if m.Role == RoleSystem {
			parts = append(parts, TextOf(m.Content))
		}
	}
	return strings.Join(parts, "\n")
}

// TestBudgetNotesAtThresholds gives a run of four turns a note at 75% and
// one at 90% of maxTurns, each once, and then the final report at the
// limit.
func TestBudgetNotesAtThresholds(t *testing.T) {
	ctx := testCtx(t)
	s := newScript(echoCall(1), echoCall(2), echoCall(3), echoCall(4), endTurn("the report"))
	a := newTestAgent(t, s.Model(), WithTools(echoTool(nil)))
	r, err := a.Run(ctx, "work", WithRunBudget(Budget{MaxTurns: 4}))
	mustState(t, r, err, StateFailed)
	reqs := s.requests()
	if len(reqs) != 5 || r.Output != "the report" {
		t.Fatalf("model calls = %d, run = %+v", len(reqs), r)
	}
	notes := budgetNotesOf(t, ledgerEvents(t, a.Ledger(), r.ID))
	if len(notes) != 2 {
		t.Fatalf("notes = %+v", notes)
	}
	if n := notes[0].Budget; n == nil || n.Limit != "maxTurns" || n.Percent != 75 || n.Value != 3 || n.Max != 4 {
		t.Fatalf("first note = %+v", notes[0].Budget)
	}
	if n := notes[1].Budget; n == nil || n.Percent != 90 || n.Value != 4 {
		t.Fatalf("second note = %+v", notes[1].Budget)
	}
	if got := lastSystem(reqs[1]); got != "" {
		t.Fatalf("turn 2 got a note: %q", got)
	}
	if got := lastSystem(reqs[2]); !strings.Contains(got, "turn 3 of the 4") || !strings.Contains(got, "1 turn is left") {
		t.Fatalf("turn 3 note = %q", got)
	}
	if got := lastSystem(reqs[3]); !strings.Contains(got, "This is the last turn") || !strings.Contains(got, "Wrap up now") || !strings.Contains(got, "one last call without tools") {
		t.Fatalf("turn 4 note = %q", got)
	}
	if got := lastSystem(reqs[4]); !strings.Contains(got, "Final report") || !strings.Contains(got, "what is left to do") {
		t.Fatalf("report instruction = %q", got)
	}
}

// TestBudgetNoteForTokensSaysTheRunEndsAtOnce gives notes at 75% and 90%
// of maxTokens. A token limit gets no final report, so the 90% note says
// that the run ends at once, and the stop makes no further call.
func TestBudgetNoteForTokensSaysTheRunEndsAtOnce(t *testing.T) {
	ctx := testCtx(t)
	// Each response counts 15 tokens: 75 after five calls, 90 after six,
	// and 105 after seven.
	s := newScript(echoCall(1), echoCall(2), echoCall(3), echoCall(4), echoCall(5), echoCall(6), echoCall(7), endTurn("never"))
	a := newTestAgent(t, s.Model(), WithTools(echoTool(nil)))
	r, err := a.Run(ctx, "work", WithRunBudget(Budget{MaxTokens: 100}))
	mustState(t, r, err, StateFailed)
	reqs := s.requests()
	if len(reqs) != 7 || r.Error != "budget exceeded: maxTokens" || r.Output != "" {
		t.Fatalf("model calls = %d, run = %+v", len(reqs), r)
	}
	notes := budgetNotesOf(t, ledgerEvents(t, a.Ledger(), r.ID))
	if len(notes) != 2 || notes[0].Budget.Percent != 75 || notes[1].Budget.Percent != 90 || notes[1].Budget.Limit != "maxTokens" {
		t.Fatalf("notes = %+v", notes)
	}
	if got := lastSystem(reqs[6]); !strings.Contains(got, "90 of the 100 tokens") || !strings.Contains(got, "the run ends at once") {
		t.Fatalf("90%% note = %q", got)
	}
	var d BudgetExceededData
	firstOf(t, ledgerEvents(t, a.Ledger(), r.ID), EvBudgetExceeded, &d)
	if d.Report {
		t.Fatalf("a token limit asked for a report: %+v", d)
	}
}

// TestBudgetNoteForTheDeadline gives a note about the working time that is
// left before the deadline.
func TestBudgetNoteForTheDeadline(t *testing.T) {
	ctx := testCtx(t)
	clock := NewManualClock(t0)
	slow := NewTool(ToolSpec{Name: "slow", ReadOnly: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		clock.Advance(50 * time.Minute)
		return TextResult("done"), nil
	})
	s := newScript(callTools(use("c1", "slow", `{}`)), endTurn("finished"))
	cfg := DefaultConfig()
	cfg.Time.NoteAfter = 0
	a := clockAgent(t, s.Model(), clock, nil, WithConfig(cfg), WithTools(slow))
	r, err := a.Run(ctx, "work", WithRunBudget(Budget{Deadline: time.Hour}))
	mustState(t, r, err, StateCompleted)
	got := lastSystem(s.requests()[1])
	if !strings.Contains(got, "50 minutes of the 1 hour of working time") || !strings.Contains(got, "10 minutes of working time left") {
		t.Fatalf("deadline note = %q", got)
	}
}

// TestBudgetNotesAreNotRepeatedAfterARestart resumes a run whose model call
// was in flight after its note. The resumed call carries the note once.
func TestBudgetNotesAreNotRepeatedAfterARestart(t *testing.T) {
	ctx := testCtx(t)
	l := NewMemLedger()
	started := make(chan struct{})
	first := newCallSteps(answer(echoCall(1)), answer(echoCall(2)), hang(started))
	a := newTestAgent(t, first.Model(), WithLedger(l), WithTools(echoTool(nil)))
	r, err := a.Start(ctx, "work", WithRunBudget(Budget{MaxTurns: 4}))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	sctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	a.Close(sctx)
	if n := budgetNotesOf(t, ledgerEvents(t, l, r.ID)); len(n) != 1 || n[0].Budget.Percent != 75 {
		t.Fatalf("notes before the restart = %+v", n)
	}
	s := newScript(echoCall(3), endTurn("done"))
	b := newTestAgent(t, s.Model(), WithLedger(l), WithTools(echoTool(nil)))
	got, err := b.Wait(ctx, r.ID)
	mustState(t, got, err, StateCompleted)
	notes := budgetNotesOf(t, ledgerEvents(t, l, r.ID))
	if len(notes) != 2 || notes[0].Budget.Percent != 75 || notes[1].Budget.Percent != 90 {
		t.Fatalf("notes after the restart = %+v", notes)
	}
	var count int
	for _, m := range s.requests()[0].Messages {
		if m.Role == RoleSystem && strings.Contains(TextOf(m.Content), "Budget note") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("the resumed call holds %d budget notes", count)
	}
}

// TestFinalReportCallShape checks the report call: the prefix of a turn,
// the output cap of the budget, the instruction last, the report flag on
// the request, and no tool call of the answer runs.
func TestFinalReportCallShape(t *testing.T) {
	ctx := testCtx(t)
	var calls int
	var mu sync.Mutex
	counted := NewTool(ToolSpec{Name: "echo", ReadOnly: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return TextResult("ok"), nil
	})
	report := callTools(use("r1", "echo", `{"text":"sneak"}`))
	report.Content = append(report.Content, Text(" and the report"))
	s := newScript(echoCall(1), report)
	a := newTestAgent(t, s.Model(), WithTools(counted))
	r, err := a.Run(ctx, "work", WithRunBudget(Budget{MaxTurns: 1, ReportMaxTokens: 300}))
	mustState(t, r, err, StateFailed)
	reqs := s.requests()
	if len(reqs) != 2 || calls != 1 {
		t.Fatalf("model calls = %d, tool calls = %d", len(reqs), calls)
	}
	turn, rep := reqs[0], reqs[1]
	if rep.MaxTokens != 300 || rep.System != turn.System || len(rep.Tools) != len(turn.Tools) {
		t.Fatalf("report request: max tokens %d, same system %v, tools %d of %d", rep.MaxTokens, rep.System == turn.System, len(rep.Tools), len(turn.Tools))
	}
	if last := rep.Messages[len(rep.Messages)-1]; last.Role != RoleSystem || !strings.Contains(TextOf(last.Content), "maxTurns") {
		t.Fatalf("last message = %+v", last)
	}
	if r.Output != "calling and the report" || r.Error != "budget exceeded: maxTurns" {
		t.Fatalf("run = %+v", r)
	}
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	var reportCalls int
	for _, e := range evs {
		var d ModelRequestedData
		if e.Type == EvModelRequested && e.Decode(&d) == nil && d.Report {
			reportCalls++
		}
	}
	if reportCalls != 1 || countType(evs, EvTurnStarted) != 1 || countType(evs, EvToolRequested) != 1 {
		t.Fatalf("events = %v", eventTypes(evs))
	}
	// The usage counts the report call.
	if r.Usage.OutputTokens != 10 {
		t.Fatalf("usage = %+v", r.Usage)
	}
}

// TestFinalReportOff stops a run at once when the budget turns the report
// off.
func TestFinalReportOff(t *testing.T) {
	ctx := testCtx(t)
	s := newScript(echoCall(1), endTurn("never"))
	a := newTestAgent(t, s.Model(), WithTools(echoTool(nil)))
	r, err := a.Run(ctx, "work", WithRunBudget(Budget{MaxTurns: 1, FinalReport: noReport()}))
	mustState(t, r, err, StateFailed)
	if len(s.requests()) != 1 || r.Output != "" || r.Error != "budget exceeded: maxTurns" {
		t.Fatalf("model calls = %d, run = %+v", len(s.requests()), r)
	}
}

// TestNoFinalReportAtTheCostLimit makes no report call at a limit of
// money, also with the report on.
func TestNoFinalReportAtTheCostLimit(t *testing.T) {
	ctx := testCtx(t)
	s := newScript(echoCall(1), endTurn("never"))
	prices := PriceTable{"anthropic/claude-opus-5-5": {Input: 1_000_000, Output: 0}}
	a := newTestAgent(t, s.Model(), WithTools(echoTool(nil)), WithPrices(prices))
	r, err := a.Run(ctx, "spend", WithRunBudget(Budget{MaxCostUSD: 5}))
	mustState(t, r, err, StateFailed)
	if len(s.requests()) != 1 || r.Output != "" || r.Error != "budget exceeded: maxCostUSD" {
		t.Fatalf("model calls = %d, run = %+v", len(s.requests()), r)
	}
}

// TestFinalReportTimeout ends a report call that takes too long. The run
// fails at its limit with no output, and the error event says why.
func TestFinalReportTimeout(t *testing.T) {
	ctx := testCtx(t)
	s := newCallSteps(answer(echoCall(1)), hang(nil))
	a := newTestAgent(t, s.Model(), WithTools(echoTool(nil)))
	r, err := a.Run(ctx, "work", WithRunBudget(Budget{MaxTurns: 1, ReportTimeout: 50 * time.Millisecond}))
	mustState(t, r, err, StateFailed)
	if r.Output != "" || r.Error != "budget exceeded: maxTurns" || len(s.requests()) != 2 {
		t.Fatalf("model calls = %d, run = %+v", len(s.requests()), r)
	}
	var d ErrorData
	firstOf(t, ledgerEvents(t, a.Ledger(), r.ID), EvError, &d)
	if !strings.Contains(d.Message, "final report call failed") || !strings.Contains(d.Message, "budget.reportTimeout") {
		t.Fatalf("error = %+v", d)
	}
}

// TestDeadlineEndsTheModelCall ends a model call that is in flight when
// the working time reaches the deadline, and then makes the final report
// with a time limit of its own.
func TestDeadlineEndsTheModelCall(t *testing.T) {
	ctx := testCtx(t)
	s := newCallSteps(hang(nil), answer(endTurn("the report")))
	a := newTestAgent(t, s.Model())
	start := time.Now()
	r, err := a.Run(ctx, "work", WithRunBudget(Budget{Deadline: 200 * time.Millisecond}))
	mustState(t, r, err, StateFailed)
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the run took %s", took)
	}
	if r.Error != "budget exceeded: deadline" || r.Output != "the report" || len(s.requests()) != 2 {
		t.Fatalf("model calls = %d, run = %+v", len(s.requests()), r)
	}
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	if countType(evs, EvModelResponded) != 1 || countType(evs, EvModelRequested) != 2 || countType(evs, EvTurnFinished) != 1 || r.Usage.Turns != 1 {
		t.Fatalf("events = %v", eventTypes(evs))
	}
	var d ErrorData
	firstOf(t, evs, EvError, &d)
	if !strings.Contains(d.Message, "deadline during the model call") {
		t.Fatalf("error = %+v", d)
	}
	var x BudgetExceededData
	firstOf(t, evs, EvBudgetExceeded, &x)
	if x.Limit != "deadline" || !x.Report || x.Max != 0.2 {
		t.Fatalf("exceeded = %+v", x)
	}
}

// TestToolsPastTheDeadlineDoNotRun gives the calls of a response that came
// after the deadline an error result, and the run stops at its next step.
func TestToolsPastTheDeadlineDoNotRun(t *testing.T) {
	ctx := testCtx(t)
	clock := NewManualClock(t0)
	var calls int
	tool := NewTool(ToolSpec{Name: "echo", ReadOnly: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		calls++
		return TextResult("ok"), nil
	})
	late := func(context.Context, *Request) (*Response, error) {
		clock.Advance(2 * time.Hour)
		return echoCall(1), nil
	}
	s := newCallSteps(late, answer(endTurn("the report")))
	a := clockAgent(t, s.Model(), clock, nil, WithTools(tool))
	r, err := a.Run(ctx, "work", WithRunBudget(Budget{Deadline: time.Hour}))
	mustState(t, r, err, StateFailed)
	if calls != 0 || r.Output != "the report" || r.Error != "budget exceeded: deadline" {
		t.Fatalf("tool calls = %d, run = %+v", calls, r)
	}
	var result *Block
	for _, m := range s.requests()[1].Messages {
		for i, b := range m.Content {
			if b.Type == BlockToolResult {
				result = &m.Content[i]
			}
		}
	}
	if result == nil || !result.IsError || !strings.Contains(TextOf(result.Content), "budget.deadline") {
		t.Fatalf("tool result = %+v", result)
	}
}

// TestSendBudgetContinuesAFailedRun continues a run that failed at its
// limit with a follow-up that raises the limit. The working time and the
// turns before count against the new limit.
func TestSendBudgetContinuesAFailedRun(t *testing.T) {
	ctx := testCtx(t)
	s := newScript(echoCall(1), endTurn("the report"), endTurn("done"))
	a := newTestAgent(t, s.Model(), WithTools(echoTool(nil)))
	r, err := a.Run(ctx, "work", WithRunBudget(Budget{MaxTurns: 1}))
	mustState(t, r, err, StateFailed)
	alice := WithPrincipal(ctx, &Principal{Subject: "alice", Kind: KindUser})
	if err := a.Send(alice, r.ID, UserText("go on"), OriginHTTP, WithSendBudget(Budget{MaxTurns: 3})); err != nil {
		t.Fatal(err)
	}
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)
	if r.Output != "done" || r.Usage.Turns != 2 {
		t.Fatalf("run = %+v", r)
	}
	b, err := a.RunBudget(ctx, r.ID)
	if err != nil || b.MaxTurns != 3 {
		t.Fatalf("budget = %+v, %v", b, err)
	}
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	var d BudgetChangedData
	firstOf(t, evs, EvBudgetChanged, &d)
	if d.Budget.MaxTurns != 3 || d.By != "alice" || d.Origin != OriginHTTP {
		t.Fatalf("change = %+v", d)
	}
	reqs := s.requests()
	last := reqs[2]
	var sawMessage bool
	for _, m := range last.Messages {
		if m.Role == RoleUser && TextOf(m.Content) == "go on" {
			sawMessage = true
		}
	}
	if notes := systemTexts(last); !sawMessage || !strings.Contains(notes, "budget of this run changed") || !strings.Contains(notes, "3 model turns (this call is turn 2)") {
		t.Fatalf("follow-up request: message %v, notes %q", sawMessage, notes)
	}
}

// TestFollowUpWithoutBudgetKeepsTheReport fails a follow-up to a run over
// its limit at once: it makes no second report, and the output stays the
// report.
func TestFollowUpWithoutBudgetKeepsTheReport(t *testing.T) {
	ctx := testCtx(t)
	s := newScript(echoCall(1), endTurn("the report"), endTurn("never"))
	a := newTestAgent(t, s.Model(), WithTools(echoTool(nil)))
	r, err := a.Run(ctx, "work", WithRunBudget(Budget{MaxTurns: 1}))
	mustState(t, r, err, StateFailed)
	if err := a.Send(ctx, r.ID, UserText("status?"), OriginHTTP); err != nil {
		t.Fatal(err)
	}
	r, err = a.WaitFor(ctx, r.ID, func(r *Run) bool { return r.State.Terminal() && r.Seq > 0 && r.Error != "" })
	mustState(t, r, err, StateFailed)
	time.Sleep(20 * time.Millisecond)
	r, _ = a.Get(ctx, r.ID)
	if len(s.requests()) != 2 || r.Output != "the report" || r.Error != "budget exceeded: maxTurns" {
		t.Fatalf("model calls = %d, run = %+v", len(s.requests()), r)
	}
}

// TestChangeBudgetOfARunningRun raises the limit of a run while a tool call
// runs. The run reads the new budget at its next check and goes on.
func TestChangeBudgetOfARunningRun(t *testing.T) {
	ctx := testCtx(t)
	entered, release := make(chan struct{}), make(chan struct{})
	slow := NewTool(ToolSpec{Name: "slow", ReadOnly: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		close(entered)
		<-release
		return TextResult("slow done"), nil
	})
	s := newScript(callTools(use("c1", "slow", `{}`)), endTurn("done"))
	a := newTestAgent(t, s.Model(), WithTools(slow))
	r, err := a.Start(ctx, "work", WithRunBudget(Budget{MaxTurns: 1}))
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	b, err := a.ChangeBudget(ctx, r.ID, Budget{MaxTurns: 2}, OriginHTTP)
	if err != nil || b.MaxTurns != 2 {
		t.Fatalf("change = %+v, %v", b, err)
	}
	close(release)
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)
	if r.Output != "done" {
		t.Fatalf("run = %+v", r)
	}
	if notes := systemTexts(s.requests()[1]); !strings.Contains(notes, "budget of this run changed") || !strings.Contains(notes, "2 model turns (this call is turn 2)") {
		t.Fatalf("notes = %q", notes)
	}
}

// TestChangeBudgetChecks refuses an empty or negative change, a viewer, and
// an unknown run, and keeps a finished run finished.
func TestChangeBudgetChecks(t *testing.T) {
	ctx := testCtx(t)
	a := newTestAgent(t, newScript(endTurn("a")).Model(),
		WithProfile(AgentProfile{Name: "team", Access: &Access{Owners: []string{"alice"}, Viewers: []string{"bob"}}}))
	alice := WithPrincipal(ctx, &Principal{Subject: "alice", Kind: KindUser})
	bob := WithPrincipal(ctx, &Principal{Subject: "bob", Kind: KindUser})
	r, err := a.Run(alice, "x", AsAgent("team"))
	mustState(t, r, err, StateCompleted)
	if _, err := a.ChangeBudget(alice, r.ID, Budget{}, OriginHTTP); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty change = %v", err)
	}
	if _, err := a.ChangeBudget(alice, r.ID, Budget{MaxTurns: -1}, OriginHTTP); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("negative change = %v", err)
	}
	if _, err := a.ChangeBudget(bob, r.ID, Budget{MaxTurns: 9}, OriginHTTP); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer change = %v", err)
	}
	if _, err := a.ChangeBudget(alice, "01JA3Q9Z6K2M4N8P0R1S2T3U4V", Budget{MaxTurns: 9}, OriginHTTP); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("unknown run = %v", err)
	}
	if err := a.Send(alice, r.ID, UserText("x"), OriginHTTP, WithSendBudget(Budget{})); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("send with an empty budget = %v", err)
	}
	b, err := a.ChangeBudget(alice, r.ID, Budget{MaxTurns: 9, FinalReport: noReport()}, OriginHTTP)
	if err != nil || b.MaxTurns != 9 || b.reportOn() {
		t.Fatalf("change = %+v, %v", b, err)
	}
	time.Sleep(20 * time.Millisecond)
	got, _ := a.Get(ctx, r.ID)
	if got.State != StateCompleted || got.Output != "a" {
		t.Fatalf("a change reopened the run: %+v", got)
	}
}

// TestBudgetChangeDuringTheReportKeepsTheRunGoing raises the limit while
// the final report call is in flight. The run records the report and goes
// on with the follow-up instead of finishing.
func TestBudgetChangeDuringTheReportKeepsTheRunGoing(t *testing.T) {
	ctx := testCtx(t)
	inReport, release := make(chan struct{}), make(chan struct{})
	report := func(context.Context, *Request) (*Response, error) {
		close(inReport)
		<-release
		return endTurn("the report"), nil
	}
	s := newCallSteps(answer(echoCall(1)), report, answer(endTurn("done")))
	a := newTestAgent(t, s.Model(), WithTools(echoTool(nil)))
	r, err := a.Start(ctx, "work", WithRunBudget(Budget{MaxTurns: 1}))
	if err != nil {
		t.Fatal(err)
	}
	<-inReport
	if err := a.Send(ctx, r.ID, UserText("take more turns"), OriginHTTP, WithSendBudget(Budget{MaxTurns: 4})); err != nil {
		t.Fatal(err)
	}
	close(release)
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	if r.Output != "done" || countType(evs, EvRunFinished) != 1 || len(s.requests()) != 3 {
		t.Fatalf("run = %+v, model calls = %d, events %v", r, len(s.requests()), eventTypes(evs))
	}
}

// TestReportSurvivesARestart resumes a run whose report call was in
// flight: the call goes out again with the instruction once.
func TestReportSurvivesARestart(t *testing.T) {
	ctx := testCtx(t)
	l := NewMemLedger()
	started := make(chan struct{})
	first := newCallSteps(answer(echoCall(1)), hang(started))
	a := newTestAgent(t, first.Model(), WithLedger(l), WithTools(echoTool(nil)))
	r, err := a.Start(ctx, "work", WithRunBudget(Budget{MaxTurns: 1}))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	sctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	a.Close(sctx)
	s := newScript(endTurn("the report"))
	b := newTestAgent(t, s.Model(), WithLedger(l), WithTools(echoTool(nil)))
	got, err := b.Wait(ctx, r.ID)
	mustState(t, got, err, StateFailed)
	if got.Output != "the report" || len(s.requests()) != 1 {
		t.Fatalf("run = %+v, model calls = %d", got, len(s.requests()))
	}
	var notes int
	for _, e := range ledgerEvents(t, l, r.ID) {
		var d MessageReceivedData
		if e.Type == EvMessageReceived && e.Decode(&d) == nil && d.Note == NoteReport {
			notes++
		}
	}
	if notes != 1 {
		t.Fatalf("report instructions = %d", notes)
	}
}

// TestChildReportIsOffWhenTheParentTurnsItOff caps the final report of a
// child by its parent.
func TestChildReportIsOffWhenTheParentTurnsItOff(t *testing.T) {
	on := true
	child := Budget{MaxTurns: 5, FinalReport: &on, ReportMaxTokens: 4000, ReportTimeout: time.Hour}
	got := child.Cap(Budget{FinalReport: noReport(), ReportMaxTokens: 500, ReportTimeout: time.Minute})
	if got.reportOn() || got.ReportMaxTokens != 500 || got.ReportTimeout != time.Minute || got.MaxTurns != 5 {
		t.Fatalf("capped = %+v", got)
	}
	if got := child.Cap(Budget{MaxTokens: 10}); !got.reportOn() {
		t.Fatalf("a cap without a report setting turned the report off: %+v", got)
	}
	if b := (Budget{}); b.reportOn() || b.reportTokens() != DefaultReportMaxTokens || b.reportTimeout() != DefaultReportTimeout {
		t.Fatalf("zero budget: on %v, tokens %d, timeout %s", b.reportOn(), b.reportTokens(), b.reportTimeout())
	}
	if c := DefaultConfig(); !c.Budget.reportOn() || c.Budget.ReportMaxTokens != 1024 || c.Budget.ReportTimeout != 2*time.Minute {
		t.Fatalf("default budget = %+v", c.Budget)
	}
}

// TestNoFinalReportPastAMoneyLimit makes no report call at a limit of
// turns when the call can pass what is left of maxTokens or maxCostUSD.
func TestNoFinalReportPastAMoneyLimit(t *testing.T) {
	prices := PriceTable{"anthropic/claude-opus-5-5": {Input: 1_000_000, Output: 1_000_000}}
	for name, b := range map[string]Budget{
		"tokens": {MaxTurns: 1, MaxTokens: 100},
		"cost":   {MaxTurns: 1, MaxCostUSD: 100},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := testCtx(t)
			s := newScript(echoCall(1), endTurn("never"))
			a := newTestAgent(t, s.Model(), WithTools(echoTool(nil)), WithPrices(prices))
			r, err := a.Run(ctx, "work", WithRunBudget(b))
			mustState(t, r, err, StateFailed)
			if len(s.requests()) != 1 || r.Output != "" || r.Error != "budget exceeded: maxTurns" {
				t.Fatalf("model calls = %d, run = %+v", len(s.requests()), r)
			}
		})
	}
	// With room for the call, the same stop reports.
	ctx := testCtx(t)
	s := newScript(echoCall(1), endTurn("the report"))
	a := newTestAgent(t, s.Model(), WithTools(echoTool(nil)), WithPrices(prices))
	r, err := a.Run(ctx, "work", WithRunBudget(Budget{MaxTurns: 1, MaxTokens: 100_000, MaxCostUSD: 100_000, ReportMaxTokens: 200}))
	mustState(t, r, err, StateFailed)
	if len(s.requests()) != 2 || r.Output != "the report" {
		t.Fatalf("model calls = %d, run = %+v", len(s.requests()), r)
	}
}
