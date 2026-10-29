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

// gate is a check tool for the tests of a definition of done. It answers
// with exit code 0 once fixed, and with exit code 1 before, and counts its
// calls. The tool fix fixes it.
type gate struct {
	mu    sync.Mutex
	fixed bool
	calls int
}

func (g *gate) tools() []Tool {
	tests := NewTool(ToolSpec{Name: "tests", ReadOnly: true, Idempotent: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.calls++
		code := 1
		text := "--- FAIL: TestRetry\nFAIL\nexit code 1"
		if g.fixed {
			code, text = 0, "ok  \tcheckout\t0.2s\nexit code 0"
		}
		return ToolResult{Content: []Block{Text(text)}, ExitCode: &code}, nil
	})
	fix := NewTool(ToolSpec{Name: "fix"}, func(context.Context, json.RawMessage) (ToolResult, error) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.fixed = true
		return TextResult("fixed"), nil
	})
	return []Tool{tests, fix}
}

func (g *gate) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// donePassesOf returns the fw.done.checked events of a run, decoded.
func donePassesOf(t *testing.T, evs []Event) []DoneCheckedData {
	t.Helper()
	var out []DoneCheckedData
	for _, e := range evs {
		if e.Type == EvDoneChecked {
			var d DoneCheckedData
			if err := e.Decode(&d); err != nil {
				t.Fatal(err)
			}
			out = append(out, d)
		}
	}
	return out
}

// doneNotes returns the texts of the messages that gave the run the
// failures of its checks.
func doneNotes(t *testing.T, evs []Event) []string {
	t.Helper()
	var out []string
	for _, e := range evs {
		var d MessageReceivedData
		if e.Type == EvMessageReceived && e.Decode(&d) == nil && d.Note == NoteDone {
			if d.Origin != OriginKernel || d.Role != RoleUser {
				t.Fatalf("done message = %+v", d)
			}
			out = append(out, TextOf(d.Content))
		}
	}
	return out
}

// finishedOf returns the data of the last fw.run.finished of a run.
func finishedOf(t *testing.T, evs []Event) RunFinishedData {
	t.Helper()
	var d RunFinishedData
	for _, e := range evs {
		if e.Type == EvRunFinished {
			d = RunFinishedData{}
			if err := e.Decode(&d); err != nil {
				t.Fatal(err)
			}
		}
	}
	return d
}

var testsPass = DoneCheck{Name: "the tests pass", Tool: "tests", Input: `{"package": "./..."}`}

// A run whose checks pass completes, and its end says that the kernel
// checked the work. The checks run after the turn closed and before the
// run finished, and the model gets no other call.
func TestDoneChecked(t *testing.T) {
	ctx := testCtx(t)
	g := &gate{}
	s := newScript(callTools(use("c1", "fix", `{}`)), endTurn("The retries work now."))
	prof := AgentProfile{Name: "fixer", Done: &Done{Checks: []DoneCheck{testsPass}}}
	a := newTestAgent(t, s.Model(), WithTools(g.tools()...), WithProfile(prof))
	r, err := a.Run(ctx, "Fix the checkout retries.", AsAgent("fixer"))
	mustState(t, r, err, StateCompleted)
	if r.Output != "The retries work now." || len(s.requests()) != 2 || g.count() != 1 {
		t.Fatalf("run = %+v, model calls %d, checks %d", r, len(s.requests()), g.count())
	}
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	var q RunQueuedData
	firstOf(t, evs, EvRunQueued, &q)
	if q.Done == nil || q.Done.Attempts != DefaultDoneAttempts || q.Done.Timeout != DefaultDoneTimeout ||
		len(q.Done.Checks) != 1 || q.Done.Checks[0].Input != `{"package":"./..."}` {
		t.Fatalf("queued done = %+v", q.Done)
	}
	types := eventTypes(evs)
	n := len(types)
	if types[n-5] != EvTurnFinished || types[n-4] != EvToolRequested || types[n-3] != EvToolResponded ||
		types[n-2] != EvDoneChecked || types[n-1] != EvRunFinished {
		t.Fatalf("events = %v", types)
	}
	var req ToolRequestedData
	if err := evs[n-4].Decode(&req); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(req.CallID, doneCallPrefix) || req.Tool != "tests" {
		t.Fatalf("check call = %+v", req)
	}
	passes := donePassesOf(t, evs)
	if len(passes) != 1 || !passes[0].Passed || passes[0].Attempt != 1 || passes[0].Continue ||
		passes[0].Answer != lastSeqOf(evs, EvModelResponded) || len(passes[0].Checks) != 1 || !passes[0].Checks[0].Passed {
		t.Fatalf("passes = %+v", passes)
	}
	if d := finishedOf(t, evs); d.Done != DoneChecked || d.DoneWeak || d.Stop != RunStopChecked || d.Outcome != OutcomeCompleted {
		t.Fatalf("finished = %+v", d)
	}
	for _, got := range []*Run{r, ProjectRun(evs)} {
		if got.Done == nil || got.Done.State != DoneChecked || got.Done.Weak || len(got.Done.Checks) != 1 || got.Stop != RunStopChecked {
			t.Fatalf("run done = %+v", got.Done)
		}
	}
	// A consumer of the lifecycle events alone, such as the front of a
	// fleet, learns the state from the end of the run.
	var life []Event
	for _, e := range evs {
		if e.Type == EvRunQueued || e.Type == EvRunStarted || e.Type == EvRunFinished {
			life = append(life, e)
		}
	}
	if got := ProjectRun(life); got.State != StateCompleted || got.Done == nil || got.Done.State != DoneChecked {
		t.Fatalf("lifecycle projection = %+v", got)
	}
}

// A failed check stops the run from finishing. The model gets the failure
// and one more turn, fixes the work, and the run completes after the next
// pass. The checks never reach the history of the model as tool calls.
func TestDoneFixesTheWork(t *testing.T) {
	ctx := testCtx(t)
	g := &gate{}
	s := newScript(endTurn("Done, the retries work."), callTools(use("c1", "fix", `{}`)), endTurn("Fixed the retry loop."))
	prof := AgentProfile{Name: "fixer", Done: &Done{Checks: []DoneCheck{testsPass}, Attempts: 2}}
	a := newTestAgent(t, s.Model(), WithTools(g.tools()...), WithProfile(prof))
	r, err := a.Run(ctx, "Fix the checkout retries.", AsAgent("fixer"))
	mustState(t, r, err, StateCompleted)
	if r.Output != "Fixed the retry loop." || r.Done == nil || r.Done.State != DoneChecked {
		t.Fatalf("run = %+v", r)
	}
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	passes := donePassesOf(t, evs)
	if len(passes) != 2 {
		t.Fatalf("passes = %+v", passes)
	}
	first := passes[0]
	if first.Passed || !first.Continue || first.Attempt != 1 || first.Attempts != 2 || first.Checks[0].Passed ||
		!strings.Contains(first.Checks[0].Why, "the exit code is 1") || !strings.Contains(first.Checks[0].Why, "--- FAIL: TestRetry") {
		t.Fatalf("first pass = %+v", first)
	}
	if second := passes[1]; !second.Passed || second.Attempt != 2 || second.Continue {
		t.Fatalf("second pass = %+v", second)
	}
	notes := doneNotes(t, evs)
	if len(notes) != 1 {
		t.Fatalf("notes = %q", notes)
	}
	for _, want := range []string{`check 1 "the tests pass" (tests) failed`, "--- FAIL: TestRetry", "1 of 1 checks failed", "attempt 1 of at most 2", "Do not change a check"} {
		if !strings.Contains(notes[0], want) {
			t.Errorf("note lacks %q:\n%s", want, notes[0])
		}
	}
	reqs := s.requests()
	if len(reqs) != 3 {
		t.Fatalf("model calls = %d", len(reqs))
	}
	// The second call ends with the note, and no message of the model
	// history holds a call of a check.
	last := reqs[1].Messages[len(reqs[1].Messages)-1]
	if last.Role != RoleUser || TextOf(last.Content) != notes[0] {
		t.Fatalf("the fix turn got %+v", last)
	}
	for _, m := range reqs[2].Messages {
		for _, b := range m.Content {
			if strings.HasPrefix(b.ID, doneCallPrefix) || strings.HasPrefix(b.ToolUseID, doneCallPrefix) {
				t.Fatalf("a check call reached the model: %+v", b)
			}
		}
	}
	// Each model call is a turn: the answer, the call of fix, and the new answer.
	if r.Usage.Turns != 3 || g.count() != 2 {
		t.Fatalf("turns %d, checks %d", r.Usage.Turns, g.count())
	}
}

// At the last failed attempt the run fails as not done, with the answer of
// the model as its output and the failed checks as its error.
func TestDoneNotDone(t *testing.T) {
	ctx := testCtx(t)
	g := &gate{}
	s := newScript(endTurn("All tests pass."), endTurn("Now all tests pass."))
	prof := AgentProfile{Name: "fixer", Done: &Done{Checks: []DoneCheck{testsPass}, Attempts: 2}}
	a := newTestAgent(t, s.Model(), WithTools(g.tools()...), WithProfile(prof))
	r, err := a.Run(ctx, "Fix the checkout retries.", AsAgent("fixer"))
	mustState(t, r, err, StateFailed)
	want := `not done: check 1 "the tests pass" (tests) failed; the checks failed at the last of 2 attempts`
	if r.Error != want || r.Output != "Now all tests pass." {
		t.Fatalf("error %q, output %q", r.Error, r.Output)
	}
	if r.Done == nil || r.Done.State != DoneFailed || r.Done.Attempt != 2 {
		t.Fatalf("run done = %+v", r.Done)
	}
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	if d := finishedOf(t, evs); d.Done != DoneFailed || d.Outcome != OutcomeFailed {
		t.Fatalf("finished = %+v", d)
	}
	passes := donePassesOf(t, evs)
	if len(passes) != 2 || passes[1].Continue || passes[1].Reason == "" || len(doneNotes(t, evs)) != 1 {
		t.Fatalf("passes = %+v", passes)
	}
	// One attempt gives the model no other turn.
	s = newScript(endTurn("All tests pass."))
	prof.Done.Attempts = 1
	a = newTestAgent(t, s.Model(), WithTools((&gate{}).tools()...), WithProfile(prof))
	r, err = a.Run(ctx, "Fix the checkout retries.", AsAgent("fixer"))
	mustState(t, r, err, StateFailed)
	if len(s.requests()) != 1 || !strings.HasPrefix(r.Error, "not done: ") {
		t.Fatalf("model calls %d, error %q", len(s.requests()), r.Error)
	}
}

// A check fails on an error result, a missing string, a forbidden string,
// a tool that the agent lacks, and a tool that the policy asks for. A check
// never asks a person, and every check of a pass runs.
func TestDoneCheckRules(t *testing.T) {
	ctx := testCtx(t)
	q := &fakeQueue{}
	status := q.tool("git.status", TextResult("?? scratch.txt\n M checkout.go"), nil)
	broken := q.tool("lint", ErrorResult("lint: no such file"), nil)
	push := q.tool("git.push", TextResult("pushed"), nil)
	checks := []DoneCheck{
		{Name: "the tree is clean", Tool: "git.status", Lacks: []string{"??"}},
		{Tool: "git.status", Contains: []string{"checkout.go"}},
		{Tool: "git.status", Contains: []string{"release-notes.md"}},
		{Tool: "lint"},
		{Tool: "missing"},
		{Tool: "git.push"},
	}
	prof := AgentProfile{Name: "fixer", Policy: &Rules{Ask: []string{"git.push"}}, Done: &Done{Checks: checks, Attempts: 1}}
	a := newTestAgent(t, newScript(endTurn("done")).Model(), WithTools(status, broken, push), WithProfile(prof))
	r, err := a.Run(ctx, "Tidy up.", AsAgent("fixer"))
	mustState(t, r, err, StateFailed)
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	if countType(evs, EvApprovalRequested) != 0 || r.Pending != nil {
		t.Fatalf("a check asked a person: %v", eventTypes(evs))
	}
	passes := donePassesOf(t, evs)
	if len(passes) != 1 || len(passes[0].Checks) != len(checks) {
		t.Fatalf("passes = %+v", passes)
	}
	res := passes[0].Checks
	wants := []string{`the result holds "??"`, "", `the result lacks "release-notes.md"`, "the result is an error:\nlint: no such file",
		"the agent has no tool missing", "the policy asks before this tool"}
	for i, want := range wants {
		if want == "" {
			if !res[i].Passed || res[i].Why != "" {
				t.Errorf("check %d = %+v, want a pass", i+1, res[i])
			}
			continue
		}
		if res[i].Passed || !strings.Contains(res[i].Why, want) {
			t.Errorf("check %d = %+v, want a failure with %q", i+1, res[i], want)
		}
	}
	if !strings.Contains(r.Error, `check 1 "the tree is clean" (git.status)`) || strings.Contains(r.Error, "check 2 ") {
		t.Fatalf("error = %q", r.Error)
	}
}

// A run without a definition of done ends as before, with no field on its
// end, and a check run takes none from its profile.
func TestDoneOffLeavesTheRunAsItWas(t *testing.T) {
	ctx := testCtx(t)
	a := newTestAgent(t, newScript(endTurn("hi")).Model())
	r, err := a.Run(ctx, "Say hi.")
	mustState(t, r, err, StateCompleted)
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	var q RunQueuedData
	firstOf(t, evs, EvRunQueued, &q)
	if q.Done != nil || r.Done != nil || finishedOf(t, evs).Done != "" || countType(evs, EvDoneChecked) != 0 {
		t.Fatalf("queued %+v, run %+v", q.Done, r.Done)
	}
	q2 := &fakeQueue{}
	prof := AgentProfile{Name: "triage", Done: &Done{Checks: []DoneCheck{{Tool: "queue.read"}}}}
	a = newTestAgent(t, newScript().Model(), WithTools(q2.tool("queue.read", TextResult("{}"), nil)), WithProfile(prof))
	r, err = a.Run(ctx, "", AsAgent("triage"), WithCheck(Check{Steps: []CheckStep{step("queue.read", "")}}))
	mustState(t, r, err, StateCompleted)
	evs = ledgerEvents(t, a.Ledger(), r.ID)
	firstOf(t, evs, EvRunQueued, &q)
	if q.Done != nil || r.Done != nil || len(q2.calls()) != 1 {
		t.Fatalf("check run done = %+v, calls %q", q.Done, q2.calls())
	}
}

// A child takes no definition of done from its named profile. An inline
// profile adds its checks to the checks of the named profile and removes
// none, so a request cannot turn the definition of done off.
func TestDoneChildAndInline(t *testing.T) {
	ctx := testCtx(t)
	g := &gate{fixed: true}
	lint := DoneCheck{Tool: "tests", Input: `{"lint": true}`}
	prof := AgentProfile{Name: "fixer", Done: &Done{Checks: []DoneCheck{testsPass}, Attempts: 2}}
	a := newTestAgent(t, newScript(endTurn("parent"), endTurn("child"), endTurn("inline")).Model(), WithTools(g.tools()...), WithProfile(prof))
	r, err := a.Run(ctx, "Fix it.", AsAgent("fixer"))
	mustState(t, r, err, StateCompleted)
	child, err := a.SpawnChild(ctx, r.ID, NewRunRequest("a part of it", AsAgent("fixer")))
	if err != nil {
		t.Fatal(err)
	}
	child, err = a.Wait(ctx, child.ID)
	mustState(t, child, err, StateCompleted)
	if cevs := ledgerEvents(t, a.Ledger(), child.ID); countType(cevs, EvDoneChecked) != 0 || child.Done != nil {
		t.Fatalf("child events = %v", eventTypes(cevs))
	}
	r, err = a.Run(ctx, "Fix it.", AsAgent("fixer"), WithRunProfile(AgentProfile{Done: &Done{Checks: []DoneCheck{lint}, Attempts: 5}}))
	mustState(t, r, err, StateCompleted)
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	var q RunQueuedData
	firstOf(t, evs, EvRunQueued, &q)
	if q.Done == nil || len(q.Done.Checks) != 2 || q.Done.Checks[0].Name != testsPass.Name || q.Done.Checks[1].Input != `{"lint":true}` || q.Done.Attempts != 2 {
		t.Fatalf("queued done = %+v", q.Done)
	}
	// An empty block on the overlay keeps the checks of the profile.
	merged := prof.Overlay(AgentProfile{Done: &Done{}})
	if merged.Done == nil || len(merged.Done.Checks) != 1 {
		t.Fatalf("overlay = %+v", merged.Done)
	}
}

// A restart that cut a pass off runs the pass again, and a restart between
// a failed pass and its note gives the note once.
func TestDoneAfterARestart(t *testing.T) {
	ctx := testCtx(t)
	l := NewMemLedger()
	id := "run-done"
	d := &Done{Checks: []DoneCheck{{Tool: "tests", Input: "{}"}}, Attempts: 2, Timeout: time.Minute}
	seed(t, l, id,
		MustEvent(id, EvRunQueued, RunQueuedData{Agent: "default", Input: "Fix it.", Origin: OriginCLI, Done: d}),
		MustEvent(id, EvRunStarted, RunStartedData{Model: "anthropic/claude-opus-5-5"}),
		MustEvent(id, EvTurnStarted, TurnData{Turn: 1}),
		MustEvent(id, EvModelRequested, ModelRequestedData{Provider: "anthropic", Model: "claude-opus-5-5"}),
		MustEvent(id, EvModelResponded, ModelRespondedData{Content: []Block{Text("draft")}, StopReason: StopEndTurn}),
		MustEvent(id, EvTurnFinished, TurnData{Turn: 1}),
		// The process stopped during the check.
		MustEvent(id, EvToolRequested, ToolRequestedData{Tool: "tests", CallID: doneCallPrefix + "1", Input: json.RawMessage("{}")}),
	)
	g := &gate{fixed: true}
	a := newTestAgent(t, newScript().Model(), WithLedger(l), WithTools(g.tools()...))
	r, err := a.Wait(ctx, id)
	mustState(t, r, err, StateCompleted)
	if r.Output != "draft" || g.count() != 1 || r.Done == nil || r.Done.State != DoneChecked {
		t.Fatalf("run = %+v, checks %d", r, g.count())
	}

	l = NewMemLedger()
	seed(t, l, id,
		MustEvent(id, EvRunQueued, RunQueuedData{Agent: "default", Input: "Fix it.", Origin: OriginCLI, Done: d}),
		MustEvent(id, EvRunStarted, RunStartedData{Model: "anthropic/claude-opus-5-5"}),
		MustEvent(id, EvTurnStarted, TurnData{Turn: 1}),
		MustEvent(id, EvModelRequested, ModelRequestedData{Provider: "anthropic", Model: "claude-opus-5-5"}),
		MustEvent(id, EvModelResponded, ModelRespondedData{Content: []Block{Text("draft")}, StopReason: StopEndTurn}),
		MustEvent(id, EvTurnFinished, TurnData{Turn: 1}),
		MustEvent(id, EvDoneChecked, DoneCheckedData{Checks: []DoneResult{{Tool: "tests", Why: "the exit code is 1"}}, Attempt: 1, Attempts: 2,
			Answer: 5, Continue: true}),
	)
	g = &gate{fixed: true}
	s := newScript(endTurn("final"))
	a = newTestAgent(t, s.Model(), WithLedger(l), WithTools(g.tools()...))
	r, err = a.Wait(ctx, id)
	mustState(t, r, err, StateCompleted)
	evs := ledgerEvents(t, l, id)
	if notes := doneNotes(t, evs); len(notes) != 1 || !strings.Contains(notes[0], "the exit code is 1") {
		t.Fatalf("notes = %q (events %v)", notes, eventTypes(evs))
	}
	if r.Output != "final" || g.count() != 1 || len(s.requests()) != 1 {
		t.Fatalf("run = %+v, checks %d, model calls %d", r, g.count(), len(s.requests()))
	}
}

// A follow-up of a person after a run that ended as not done starts the
// attempts again.
func TestDoneFollowUpStartsAgain(t *testing.T) {
	ctx := testCtx(t)
	g := &gate{}
	s := newScript(endTurn("All tests pass."), callTools(use("c1", "fix", `{}`)), endTurn("Fixed now."))
	prof := AgentProfile{Name: "fixer", Done: &Done{Checks: []DoneCheck{testsPass}, Attempts: 1}}
	a := newTestAgent(t, s.Model(), WithTools(g.tools()...), WithProfile(prof))
	r, err := a.Run(ctx, "Fix the checkout retries.", AsAgent("fixer"))
	mustState(t, r, err, StateFailed)
	if err := a.Send(ctx, r.ID, UserText("The tests still fail. Fix the retry loop."), OriginCLI); err != nil {
		t.Fatal(err)
	}
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)
	if r.Done == nil || r.Done.State != DoneChecked || r.Error != "" || r.Output != "Fixed now." {
		t.Fatalf("run = %+v", r)
	}
}

func TestCheckDone(t *testing.T) {
	good := &Done{Checks: []DoneCheck{testsPass}, Attempts: 3, Timeout: time.Minute}
	if err := CheckDone(good); err != nil {
		t.Fatal(err)
	}
	if err := CheckDone(nil); err != nil {
		t.Fatal(err)
	}
	if err := CheckDone(&Done{Checks: []DoneCheck{{Tool: DoneAnswer, Input: "{}", Contains: []string{"pens="}}}}); err != nil {
		t.Fatal(err)
	}
	many := make([]DoneCheck, MaxDoneChecks+1)
	for i := range many {
		many[i] = DoneCheck{Tool: "tests"}
	}
	for _, d := range []*Done{
		{Attempts: 2},
		{Checks: many},
		{Checks: []DoneCheck{{Tool: ""}}},
		{Checks: []DoneCheck{{Tool: "bash run"}}},
		{Checks: []DoneCheck{{Tool: "bash", Input: "[1]"}}},
		{Checks: []DoneCheck{{Tool: "bash", Input: "{"}}},
		{Checks: []DoneCheck{{Tool: "bash", Name: "two\nlines"}}},
		{Checks: []DoneCheck{{Tool: "bash", Contains: []string{""}}}},
		{Checks: []DoneCheck{testsPass}, Attempts: MaxDoneAttempts + 1},
		{Checks: []DoneCheck{testsPass}, Attempts: -1},
		{Checks: []DoneCheck{testsPass}, Timeout: MaxDoneTimeout + time.Second},
		{Checks: []DoneCheck{{Tool: DoneAnswer, Input: `{"path": "a.txt"}`, Contains: []string{"x"}}}},
	} {
		if err := CheckDone(d); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("CheckDone(%+v) = %v", d, err)
		}
	}
	// The kernel refuses a bad block on an inline profile.
	a := newTestAgent(t, newScript().Model())
	if _, err := a.Start(context.Background(), "x", WithRunProfile(AgentProfile{Done: &Done{Attempts: 2}})); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("start = %v", err)
	}
}

// A call of a check asks for fresh tool state, and a call of the model
// does not, so a stateful tool such as bash gives a check a shell of its
// own.
func TestDoneCheckAsksForFreshState(t *testing.T) {
	ctx := testCtx(t)
	var mu sync.Mutex
	var seen []bool
	probe := NewTool(ToolSpec{Name: "probe"}, func(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, RunInfoFrom(ctx).Fresh)
		return TextResult("ok"), nil
	})
	s := newScript(callTools(use("c1", "probe", `{}`)), endTurn("done"))
	prof := AgentProfile{Name: "fixer", Done: &Done{Checks: []DoneCheck{{Tool: "probe"}}}}
	a := newTestAgent(t, s.Model(), WithTools(probe), WithProfile(prof))
	r, err := a.Run(ctx, "Probe it.", AsAgent("fixer"))
	mustState(t, r, err, StateCompleted)
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] || !seen[1] {
		t.Fatalf("fresh per call = %v, want [false true]", seen)
	}
}

// The static rule of a weak check: a view or a read of a path that names
// no contains and no lacks.
func TestDoneCheckWeak(t *testing.T) {
	for _, c := range []struct {
		chk  DoneCheck
		weak bool
	}{
		{DoneCheck{Tool: "files", Input: `{"command": "view", "path": "drafts/latest.md"}`}, true},
		{DoneCheck{Tool: "files", Input: `{"command": "view", "path": "drafts/latest.md"}`, Contains: []string{"# "}}, false},
		{DoneCheck{Tool: "files", Input: `{"command": "view", "path": "drafts/latest.md"}`, Lacks: []string{"TODO"}}, false},
		{DoneCheck{Tool: "memory", Input: `{"command": "view", "path": "/memories"}`}, true},
		{DoneCheck{Tool: "front.pages_read", Input: `{"path": "launch/quillwork-3.md"}`}, true},
		{DoneCheck{Tool: "fs.read_text_file", Input: `{"path": "/data/report.md"}`}, true},
		{DoneCheck{Tool: "view", Input: `{"file_path": "a.txt"}`}, true},
		{DoneCheck{Tool: "files", Input: `{"command": "create", "path": "a.txt", "file_text": "x"}`}, false},
		{DoneCheck{Tool: "front.pages_read", Input: `{}`}, false},
		{DoneCheck{Tool: "bash", Input: `{"command": "go test ./..."}`}, false},
		{DoneCheck{Tool: "tests", Input: `{"package": "./..."}`}, false},
		{DoneCheck{Tool: "thread_reader", Input: `{"path": "x"}`}, false},
		{DoneCheck{Tool: "files"}, false},
		{DoneCheck{Tool: DoneAnswer}, true},
		{DoneCheck{Tool: DoneAnswer, Contains: []string{"pens="}}, false},
		{DoneCheck{Tool: DoneAnswer, Lacks: []string{"I could not"}}, false},
	} {
		if got := c.chk.Weak(); got != c.weak {
			t.Errorf("Weak(%s %s) = %v, want %v", c.chk.Tool, c.chk.Input, got, c.weak)
		}
	}
}

// pathTools are tools for the tests of a weak pass: files, a text editor
// whose view reads and whose other commands write; memory, a tool of
// Know-how that writes notes; and lookup, a read-only tool.
func pathTools(t *testing.T) []Tool {
	t.Helper()
	var mu sync.Mutex
	texts := map[string]string{"drafts/latest.md": "# Draft from yesterday\n"}
	files := NewTool(ToolSpec{Name: "files"}, func(_ context.Context, in json.RawMessage) (ToolResult, error) {
		var d struct {
			Command  string `json:"command"`
			Path     string `json:"path"`
			FileText string `json:"file_text"`
		}
		if err := json.Unmarshal(in, &d); err != nil {
			return ErrorResult(err.Error()), nil
		}
		mu.Lock()
		defer mu.Unlock()
		if d.Command == "view" {
			text, ok := texts[d.Path]
			if !ok {
				return ErrorResult("no file " + d.Path), nil
			}
			return TextResult(text), nil
		}
		texts[d.Path] = d.FileText
		return TextResult("File created successfully at: " + d.Path), nil
	})
	memory := NewTool(ToolSpec{Name: "memory"}, func(context.Context, json.RawMessage) (ToolResult, error) {
		return TextResult("File created successfully at: /memories/n.md"), nil
	})
	lookup := NewTool(ToolSpec{Name: "lookup", ReadOnly: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		return TextResult("found"), nil
	})
	return []Tool{files, memory, lookup}
}

// A pass that passed is weak when the run changed nothing, or when no
// check of it is strong. A weak run completes as answered, with done
// checked and weak on its end and on its run object, and a consumer of
// the lifecycle events alone sees the mark too.
func TestDoneWeak(t *testing.T) {
	ctx := testCtx(t)
	view := DoneCheck{Name: "the draft is saved", Tool: "files", Input: `{"command": "view", "path": "drafts/latest.md"}`}
	title := DoneCheck{Name: "the draft has a title", Tool: "files", Input: `{"command": "view", "path": "drafts/latest.md"}`, Contains: []string{"# "}}
	write := callTools(use("c1", "files", `{"command": "create", "path": "drafts/latest.md", "file_text": "# Launch\n"}`))
	for _, c := range []struct {
		name   string
		script []*Response
		checks []DoneCheck
		weak   bool
	}{
		// The finding of the stack: "Do not call a tool", and the checks
		// read the draft of an earlier run.
		{"no call", []*Response{endTurn("Done.")}, []DoneCheck{view, title}, true},
		{"a read only", []*Response{callTools(use("c1", "files", `{"command": "view", "path": "drafts/latest.md"}`)), endTurn("Done.")}, []DoneCheck{title}, true},
		{"a read-only tool", []*Response{callTools(use("c1", "lookup", `{}`)), endTurn("Done.")}, []DoneCheck{title}, true},
		{"a note only", []*Response{callTools(use("c1", "memory", `{"command": "create", "path": "/memories/n.md", "file_text": "x"}`)), endTurn("Done.")}, []DoneCheck{title}, true},
		{"a write and a weak check", []*Response{write, endTurn("Done.")}, []DoneCheck{view}, true},
		{"a write and a strong check", []*Response{write, endTurn("Done.")}, []DoneCheck{view, title}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			prof := AgentProfile{Name: "writer", Done: &Done{Checks: c.checks}}
			a := newTestAgent(t, newScript(c.script...).Model(), WithTools(pathTools(t)...), WithProfile(prof))
			r, err := a.Run(ctx, "Write the draft. Do not call a tool.", AsAgent("writer"))
			mustState(t, r, err, StateCompleted)
			evs := ledgerEvents(t, a.Ledger(), r.ID)
			passes := donePassesOf(t, evs)
			if len(passes) != 1 || !passes[0].Passed || passes[0].Weak != c.weak {
				t.Fatalf("passes = %+v", passes)
			}
			stop := RunStopChecked
			if c.weak {
				stop = RunStopAnswered
			}
			d := finishedOf(t, evs)
			if d.Done != DoneChecked || d.DoneWeak != c.weak || d.Stop != stop || CheckedDone(d.Done, d.DoneWeak) == c.weak {
				t.Fatalf("finished = %+v", d)
			}
			var life []Event
			for _, e := range evs {
				if e.Type == EvRunQueued || e.Type == EvRunStarted || e.Type == EvRunFinished {
					life = append(life, e)
				}
			}
			for _, got := range []*Run{r, ProjectRun(evs), ProjectRun(life)} {
				if got.Done == nil || got.Done.State != DoneChecked || got.Done.Weak != c.weak || got.Stop != stop {
					t.Fatalf("run done = %+v, stop %q", got.Done, got.Stop)
				}
			}
		})
	}
}

// The checks have a tool budget of their own. A run that used its tool
// calls still runs every check, and the calls of the checks neither count
// in the usage of the run nor take a call from the turn that fixes the
// work.
func TestDoneOwnBudget(t *testing.T) {
	ctx := testCtx(t)
	// The model leaves one tool call of the run, and both checks run.
	g := &gate{}
	other := DoneCheck{Name: "the lint passes", Tool: "tests", Input: `{"lint": true}`}
	s := newScript(callTools(use("c1", "fix", `{}`)), endTurn("Fixed."))
	prof := AgentProfile{Name: "fixer", Done: &Done{Checks: []DoneCheck{testsPass, other}}}
	a := newTestAgent(t, s.Model(), WithTools(g.tools()...), WithProfile(prof))
	r, err := a.Run(ctx, "Fix the checkout retries.", AsAgent("fixer"), WithRunBudget(Budget{MaxToolCalls: 2}))
	mustState(t, r, err, StateCompleted)
	if g.count() != 2 || r.Stop != RunStopChecked || r.Usage.ToolCalls != 1 {
		t.Fatalf("checks %d, stop %q, tool calls %d, done %+v", g.count(), r.Stop, r.Usage.ToolCalls, r.Done)
	}
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	if ProjectRun(evs).Usage.ToolCalls != 1 {
		t.Fatalf("projected tool calls = %d", ProjectRun(evs).Usage.ToolCalls)
	}

	// The first pass fails, and the model still has its calls to fix the
	// work, because the two calls of the first pass did not take them.
	g = &gate{}
	s = newScript(endTurn("Done."), callTools(use("c1", "fix", `{}`)), endTurn("Fixed now."))
	prof.Done.Attempts = 2
	a = newTestAgent(t, s.Model(), WithTools(g.tools()...), WithProfile(prof))
	r, err = a.Run(ctx, "Fix the checkout retries.", AsAgent("fixer"), WithRunBudget(Budget{MaxToolCalls: 2}))
	mustState(t, r, err, StateCompleted)
	if r.Output != "Fixed now." || r.Stop != RunStopChecked || g.count() != 4 || r.Usage.ToolCalls != 1 {
		t.Fatalf("run = %+v, checks %d", r, g.count())
	}
}

// In a run that is held back from Know-how, a check of the memory tool is
// skipped, not failed, because the kernel took the tool out of the run. A
// pass whose every check was skipped is weak. A normal run runs the same
// check.
func TestDoneHeldBackSkips(t *testing.T) {
	ctx := testCtx(t)
	notes := DoneCheck{Name: "a note exists", Tool: "memory", Input: `{"command": "view", "path": "/memories"}`, Contains: []string{"File"}}
	write := callTools(use("c1", "files", `{"command": "create", "path": "drafts/latest.md", "file_text": "# Launch\n"}`))
	prof := AgentProfile{Name: "writer", Done: &Done{Checks: []DoneCheck{notes}, Attempts: 1}}
	held := WithMetadata(map[string]string{MetaKnowhow: KnowhowOff})

	a := newTestAgent(t, newScript(write, endTurn("Done.")).Model(), WithTools(pathTools(t)...), WithProfile(prof))
	r, err := a.Run(ctx, "Write the draft.", AsAgent("writer"), held)
	mustState(t, r, err, StateCompleted)
	if r.Done == nil || len(r.Done.Checks) != 1 || !r.Done.Checks[0].Passed || !strings.Contains(r.Done.Checks[0].Skipped, "held back") ||
		!r.Done.Weak || r.Stop != RunStopAnswered {
		t.Fatalf("held back: done %+v, stop %q", r.Done, r.Stop)
	}
	if n := countType(ledgerEvents(t, a.Ledger(), r.ID), EvToolRequested); n != 1 {
		t.Fatalf("a skipped check made a call: %d calls", n)
	}

	// The same check with a strong check beside it: the strong check makes
	// the pass count.
	prof.Done.Checks = []DoneCheck{notes, {Tool: "files", Input: `{"command": "view", "path": "drafts/latest.md"}`, Contains: []string{"# Launch"}}}
	a = newTestAgent(t, newScript(write, endTurn("Done.")).Model(), WithTools(pathTools(t)...), WithProfile(prof))
	r, err = a.Run(ctx, "Write the draft.", AsAgent("writer"), held)
	mustState(t, r, err, StateCompleted)
	if r.Done == nil || r.Done.Weak || r.Stop != RunStopChecked || r.Done.Checks[0].Skipped == "" || r.Done.Checks[1].Skipped != "" {
		t.Fatalf("held back with a strong check: done %+v, stop %q", r.Done, r.Stop)
	}

	// A normal run runs the check of the memory tool.
	a = newTestAgent(t, newScript(write, endTurn("Done.")).Model(), WithTools(pathTools(t)...), WithProfile(prof))
	r, err = a.Run(ctx, "Write the draft.", AsAgent("writer"))
	mustState(t, r, err, StateCompleted)
	if r.Done == nil || r.Done.Checks[0].Skipped != "" || !r.Done.Checks[0].Passed || r.Stop != RunStopChecked {
		t.Fatalf("normal run: done %+v, stop %q", r.Done, r.Stop)
	}
}

// A check of the answer tests the final answer of the run with no tool
// call. With contains or lacks it is strong, also in a run that changed
// nothing, so a job that only reads counts as checked. A wrong answer gets
// the failure and one more turn, as a failed check of a call does. Without
// contains and lacks it is weak. A tool of the agent with the reserved
// name is never called.
func TestDoneAnswer(t *testing.T) {
	ctx := testCtx(t)
	count := DoneCheck{Name: "the answer names the pens", Tool: DoneAnswer, Contains: []string{"pens="}, Lacks: []string{"could not"}}
	read := callTools(use("c1", "lookup", `{}`))
	for _, c := range []struct {
		name    string
		script  []*Response
		checks  []DoneCheck
		output  string
		weak    bool
		turns   int
		failure string
	}{
		{"a read and a right answer", []*Response{read, endTurn("Stock: pens=90.")}, []DoneCheck{count}, "Stock: pens=90.", false, 2, ""},
		{"no call", []*Response{endTurn("Stock: pens=90.")}, []DoneCheck{count}, "Stock: pens=90.", false, 1, ""},
		{"a wrong answer, then a right one", []*Response{read, endTurn("I could not read the stock."), endTurn("Stock: pens=90.")},
			[]DoneCheck{count}, "Stock: pens=90.", false, 3, `the answer lacks "pens="`},
		{"a weak check of the answer", []*Response{read, endTurn("Stock: pens=90.")}, []DoneCheck{{Tool: DoneAnswer}}, "Stock: pens=90.", true, 2, ""},
		{"a weak check of a path beside it", []*Response{read, endTurn("Stock: pens=90.")},
			[]DoneCheck{{Tool: "files", Input: `{"command": "view", "path": "drafts/latest.md"}`}, count}, "Stock: pens=90.", false, 2, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			called := 0
			trap := NewTool(ToolSpec{Name: DoneAnswer}, func(context.Context, json.RawMessage) (ToolResult, error) {
				called++
				return TextResult("pens="), nil
			})
			s := newScript(c.script...)
			prof := AgentProfile{Name: "clerk", Done: &Done{Checks: c.checks}}
			a := newTestAgent(t, s.Model(), WithTools(append(pathTools(t), trap)...), WithProfile(prof))
			r, err := a.Run(ctx, "How many pens are in stock? Only read.", AsAgent("clerk"))
			mustState(t, r, err, StateCompleted)
			if r.Output != c.output || len(s.requests()) != c.turns || called != 0 {
				t.Fatalf("output %q, model calls %d, calls of the tool answer %d", r.Output, len(s.requests()), called)
			}
			evs := ledgerEvents(t, a.Ledger(), r.ID)
			for _, e := range evs {
				var d ToolRequestedData
				if e.Type == EvToolRequested && e.Decode(&d) == nil && strings.HasPrefix(d.CallID, doneCallPrefix) && d.Tool == DoneAnswer {
					t.Fatalf("the check of the answer made a call: %+v", d)
				}
			}
			passes := donePassesOf(t, evs)
			last := passes[len(passes)-1]
			if !last.Passed || last.Weak != c.weak {
				t.Fatalf("passes = %+v", passes)
			}
			notes := doneNotes(t, evs)
			if c.failure != "" && (len(passes) != 2 || passes[0].Passed || len(notes) != 1 || !strings.Contains(notes[0], c.failure)) {
				t.Fatalf("passes = %+v, notes %q", passes, notes)
			}
			stop := RunStopChecked
			if c.weak {
				stop = RunStopAnswered
			}
			if d := finishedOf(t, evs); d.Done != DoneChecked || d.DoneWeak != c.weak || d.Stop != stop {
				t.Fatalf("finished = %+v", d)
			}
		})
	}
}

// A run that stops at a limit of tool calls, of turns, of tokens, or of
// cost runs its checks after its final report, or at once when it makes
// none, with their own budget. A pass gives the run its state of done, and
// the run still fails at its limit. A failed pass gives the model no other
// turn. A check of the answer reads the final report.
func TestDoneAtALimit(t *testing.T) {
	ctx := testCtx(t)
	prices := PriceTable{"anthropic/claude-opus-5-5": {Input: 1_000_000, Output: 0}}
	report := endTurn("I fixed the retries. The tests are left to run.")
	for _, c := range []struct {
		name    string
		script  []*Response
		budget  Budget
		checks  []DoneCheck
		limit   string
		done    string
		weak    bool
		report  bool
		calls   int // the calls of the gate
		reason  string
		priced  bool
		answers int // the model calls
	}{
		{name: "tool calls, the work done", script: []*Response{callTools(use("c1", "fix", `{}`)), report},
			budget: Budget{MaxToolCalls: 1}, checks: []DoneCheck{testsPass}, limit: "maxToolCalls", done: DoneChecked, report: true, calls: 1, answers: 2},
		{name: "tool calls, the work not done", script: []*Response{callTools(use("c1", "tests", `{}`)), report},
			budget: Budget{MaxToolCalls: 1}, checks: []DoneCheck{testsPass}, limit: "maxToolCalls", done: DoneFailed, report: true, calls: 2,
			reason: "the run stopped at its limit of 1 tool calls (budget.maxToolCalls), so the model gets no other turn", answers: 2},
		{name: "turns", script: []*Response{callTools(use("c1", "fix", `{}`)), report},
			budget: Budget{MaxTurns: 1}, checks: []DoneCheck{testsPass}, limit: "maxTurns", done: DoneChecked, report: true, calls: 1, answers: 2},
		{name: "turns, a check of the report", script: []*Response{callTools(use("c1", "fix", `{}`)), report},
			budget: Budget{MaxTurns: 1}, checks: []DoneCheck{testsPass, {Tool: DoneAnswer, Contains: []string{"fixed the retries"}}},
			limit: "maxTurns", done: DoneChecked, report: true, calls: 1, answers: 2},
		// The second response passes the limit of money, so its call does
		// not run, and the run stops with no report.
		{name: "tokens", script: []*Response{callTools(use("c1", "fix", `{}`)), callTools(use("c2", "fix", `{}`)), endTurn("never")},
			budget: Budget{MaxTokens: 20}, checks: []DoneCheck{testsPass}, limit: "maxTokens", done: DoneChecked, calls: 1, answers: 2},
		{name: "cost", script: []*Response{callTools(use("c1", "fix", `{}`)), callTools(use("c2", "fix", `{}`)), endTurn("never")},
			budget: Budget{MaxCostUSD: 15}, checks: []DoneCheck{testsPass}, limit: "maxCostUSD", done: DoneChecked, calls: 1, priced: true, answers: 2},
		{name: "cost, a check of the answer with no report", script: []*Response{callTools(use("c1", "fix", `{}`)), callTools(use("c2", "fix", `{}`)), endTurn("never")},
			budget: Budget{MaxCostUSD: 15}, checks: []DoneCheck{{Tool: DoneAnswer, Contains: []string{"fixed"}}}, limit: "maxCostUSD", done: DoneFailed,
			priced: true, answers: 2, reason: "the run stopped at its limit of $15.0000 (budget.maxCostUSD), so the model gets no other turn"},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := &gate{}
			s := newScript(c.script...)
			prof := AgentProfile{Name: "fixer", Done: &Done{Checks: c.checks}}
			opts := []Option{WithTools(g.tools()...), WithProfile(prof)}
			if c.priced {
				opts = append(opts, WithPrices(prices))
			}
			a := newTestAgent(t, s.Model(), opts...)
			r, err := a.Run(ctx, "Fix the checkout retries.", AsAgent("fixer"), WithRunBudget(c.budget))
			mustState(t, r, err, StateFailed)
			if len(s.requests()) != c.answers || g.count() != c.calls {
				t.Fatalf("model calls %d, calls of the gate %d", len(s.requests()), g.count())
			}
			evs := ledgerEvents(t, a.Ledger(), r.ID)
			if len(doneNotes(t, evs)) != 0 {
				t.Fatalf("the model got the failures after the stop: %v", eventTypes(evs))
			}
			passes := donePassesOf(t, evs)
			if len(passes) != 1 || passes[0].Limit != c.limit || passes[0].Answer != 0 || passes[0].Continue ||
				passes[0].Passed != (c.done == DoneChecked) || passes[0].Weak != c.weak || passes[0].Reason != c.reason {
				t.Fatalf("passes = %+v", passes)
			}
			types := eventTypes(evs)
			n := len(types)
			if types[n-1] != EvRunFinished || types[n-2] != EvDoneChecked {
				t.Fatalf("events = %v", types)
			}
			if c.report && (r.Output != TextOf(report.Content) || types[n-3] == EvBudgetExceeded) {
				t.Fatalf("output %q, events %v", r.Output, types)
			}
			d := finishedOf(t, evs)
			if d.Outcome != OutcomeFailed || d.Done != c.done || d.DoneWeak != c.weak || d.Stop != RunStopBudget(c.limit) ||
				d.Error != "budget exceeded: "+c.limit {
				t.Fatalf("finished = %+v", d)
			}
			for _, got := range []*Run{r, ProjectRun(evs)} {
				if got.State != StateFailed || got.Stop != RunStopBudget(c.limit) || got.Done == nil || got.Done.State != c.done || len(got.Done.Checks) != len(c.checks) {
					t.Fatalf("run = %+v, done %+v", got, got.Done)
				}
			}
			own := 0
			for _, e := range evs {
				if e.Type == EvToolRequested && !doneCall(e) {
					own++
				}
			}
			if r.Usage.ToolCalls != own {
				t.Fatalf("the calls of the checks count in the usage: %+v, own calls %d", r.Usage, own)
			}
		})
	}
}

// At the deadline of its working time, the limit of a person's time, a
// run makes its final report and runs no check.
func TestDoneNotAtTheDeadline(t *testing.T) {
	ctx := testCtx(t)
	g := &gate{}
	s := newCallSteps(hang(nil), answer(endTurn("the report")))
	prof := AgentProfile{Name: "fixer", Done: &Done{Checks: []DoneCheck{testsPass}}}
	a := newTestAgent(t, s.Model(), WithTools(g.tools()...), WithProfile(prof))
	r, err := a.Run(ctx, "Fix the checkout retries.", AsAgent("fixer"), WithRunBudget(Budget{Deadline: 200 * time.Millisecond}))
	mustState(t, r, err, StateFailed)
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	if g.count() != 0 || countType(evs, EvDoneChecked) != 0 || r.Done != nil || r.Stop != RunStopBudget("deadline") || r.Output != "the report" {
		t.Fatalf("checks %d, run %+v", g.count(), r)
	}
}
