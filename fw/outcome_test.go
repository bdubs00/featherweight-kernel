package fw

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// outcomeProvider is a model provider for the tests of a grader with a
// model of its own. Each of its models answers satisfied and names itself
// in the notes.
const outcomeProvider = "outcometest"

func init() {
	RegisterModel(outcomeProvider, func(_ context.Context, name string, _ *Config, _ Secrets) (Model, error) {
		return ModelFunc{StreamFn: func(context.Context, *Request, func(Delta)) (*Response, error) {
			return endTurn(`{"verdict": "satisfied", "notes": "judged by ` + name + `"}`), nil
		}}, nil
	})
}

// gradeModel answers as the worker and as the grader of a run. A request
// whose system prompt is the prompt of a grader takes the next text of
// grades, or else the next response of graderCalls. Every other request
// takes the next response of work.
type gradeModel struct {
	mu          sync.Mutex
	work        []*Response
	grades      []string
	graderCalls []*Response
	workReqs    []*Request
	gradeReqs   []*Request
}

func (g *gradeModel) Model() Model {
	return ModelFunc{StreamFn: func(_ context.Context, req *Request, _ func(Delta)) (*Response, error) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if strings.Contains(req.System, "You are the grader of a run of an agent.") {
			g.gradeReqs = append(g.gradeReqs, req)
			if len(g.graderCalls) > 0 {
				r := g.graderCalls[0]
				g.graderCalls = g.graderCalls[1:]
				return r, nil
			}
			if len(g.grades) == 0 {
				return nil, errors.New("grade: no answer left")
			}
			text := g.grades[0]
			g.grades = g.grades[1:]
			return endTurn(text), nil
		}
		g.workReqs = append(g.workReqs, req)
		if len(g.work) == 0 {
			return nil, errors.New("work: no response left")
		}
		r := g.work[0]
		g.work = g.work[1:]
		return r, nil
	}}
}

func (g *gradeModel) requests() (work, grade []*Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.workReqs), slices.Clone(g.gradeReqs)
}

// passesOf returns the fw.outcome.evaluated events of a run, decoded.
func passesOf(t *testing.T, evs []Event) []OutcomeEvaluatedData {
	t.Helper()
	var out []OutcomeEvaluatedData
	for _, e := range evs {
		if e.Type == EvOutcomeEvaluated {
			var d OutcomeEvaluatedData
			if err := e.Decode(&d); err != nil {
				t.Fatal(err)
			}
			out = append(out, d)
		}
	}
	return out
}

// lastSeqOf returns the sequence number of the last event of a type.
func lastSeqOf(evs []Event, typ string) uint64 {
	var seq uint64
	for _, e := range evs {
		if e.Type == typ {
			seq = e.Seq
		}
	}
	return seq
}

// outcomeNotes returns the texts of the messages that gave the run the
// notes of its grader.
func outcomeNotes(t *testing.T, evs []Event) []string {
	t.Helper()
	var out []string
	for _, e := range evs {
		var d MessageReceivedData
		if e.Type == EvMessageReceived && e.Decode(&d) == nil && d.Note == NoteOutcome {
			if d.Origin != OriginKernel || d.Role != RoleUser {
				t.Fatalf("notes message = %+v", d)
			}
			out = append(out, TextOf(d.Content))
		}
	}
	return out
}

func TestOutcomeSatisfied(t *testing.T) {
	ctx := testCtx(t)
	root := t.TempDir()
	cfg := DefaultConfig()
	cfg.Home = filepath.Join(root, ".fw")
	cfg.Tools.Files.Root = root
	cfg.Model = "anthropic/claude-opus-5-5"
	writer := NewTool(ToolSpec{Name: "writer"}, func(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
		if err := FilesFrom(ctx).WriteFile("notes/plan.md", []byte("# Plan\n\nStep one.\n")); err != nil {
			return ErrorResult(err.Error()), nil
		}
		return TextResult("written"), nil
	})
	g := &gradeModel{
		work: []*Response{callTools(use("c1", "writer", `{}`)), endTurn("I wrote notes/plan.md."), endTurn("child done")},
		grades: []string{"The plan exists and the answer names it.\n" +
			`{"verdict": "satisfied", "notes": "The answer names notes/plan.md."}`},
	}
	prof := AgentProfile{Name: "planner", Outcome: &Outcome{Rubric: "The answer names the file that the run wrote."}}
	a := newTestAgent(t, g.Model(), WithConfig(cfg), WithTools(writer), WithProfile(prof))
	r, err := a.Run(ctx, "Write the plan.", AsAgent("planner"))
	mustState(t, r, err, StateCompleted)
	if r.Output != "I wrote notes/plan.md." {
		t.Fatalf("output = %q", r.Output)
	}
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	var q RunQueuedData
	firstOf(t, evs, EvRunQueued, &q)
	if q.Outcome == nil || q.Outcome.Rubric != prof.Outcome.Rubric || q.Outcome.Iterations != 1 || q.Outcome.Grader != "" {
		t.Fatalf("queued outcome = %+v", q.Outcome)
	}
	passes := passesOf(t, evs)
	if len(passes) != 1 {
		t.Fatalf("passes = %+v", passes)
	}
	p := passes[0]
	if p.Verdict != GradeSatisfied || p.Notes != "The answer names notes/plan.md." || p.Iteration != 1 || p.Iterations != 1 ||
		p.Continue || p.Reason != "" || p.Grader == "" || p.Answer != lastSeqOf(evs, EvModelResponded) {
		t.Fatalf("pass = %+v", p)
	}
	// The grader runs after the turn closed and before the run finished.
	types := eventTypes(evs)
	n := len(types)
	if types[n-3] != EvTurnFinished || types[n-2] != EvOutcomeEvaluated || types[n-1] != EvRunFinished {
		t.Fatalf("events = %v", types)
	}
	// The run object shows the verdict next to the answer, as the ledger
	// projects it.
	want := RunOutcome{Verdict: GradeSatisfied, Notes: p.Notes, Iterations: 1, Grader: p.Grader}
	if r.Outcome == nil || *r.Outcome != want {
		t.Fatalf("run outcome = %+v", r.Outcome)
	}
	if got := ProjectRun(evs).Outcome; got == nil || *got != want {
		t.Fatalf("projected outcome = %+v", got)
	}
	// The grader is a child of the run with the same profile and model, and
	// it is not graded itself.
	kids, err := a.Children(ctx, r.ID)
	if err != nil || len(kids) != 1 || kids[0].ID != p.Grader {
		t.Fatalf("children = %+v, %v", kids, err)
	}
	gr := kids[0]
	if gr.State != StateCompleted || gr.Agent != "planner" || gr.Parent != r.ID || gr.Origin != OriginKernel || gr.Outcome != nil {
		t.Fatalf("grader run = %+v", gr)
	}
	gevs := ledgerEvents(t, a.Ledger(), gr.ID)
	var gs RunStartedData
	firstOf(t, gevs, EvRunStarted, &gs)
	var gq RunQueuedData
	firstOf(t, gevs, EvRunQueued, &gq)
	if gs.Model != "anthropic/claude-opus-5-5" || gq.Outcome != nil || countType(gevs, EvOutcomeEvaluated) != 0 {
		t.Fatalf("grader started = %+v, queued outcome = %+v", gs, gq.Outcome)
	}
	// The grader reads the rubric, the task, the answer, and the content of
	// the file that the run wrote.
	_, grades := g.requests()
	if len(grades) != 1 {
		t.Fatalf("grader requests = %d", len(grades))
	}
	in := TextOf(grades[0].Messages[0].Content)
	for _, s := range []string{"# Rubric\n\nThe answer names the file that the run wrote.", "Write the plan.", "I wrote notes/plan.md.",
		"## notes/plan.md (created, 18 bytes)", "# Plan\n\nStep one.", "pass 1 of at most 1"} {
		if !strings.Contains(in, s) {
			t.Errorf("grader input lacks %q:\n%s", s, in)
		}
	}
	if len(grades[0].Tools) != 0 {
		t.Errorf("grader tools = %+v", grades[0].Tools)
	}
	// A child of the run takes no outcome from its profile.
	child, err := a.SpawnChild(ctx, r.ID, NewRunRequest("a part of the work", AsAgent("planner")))
	if err != nil {
		t.Fatal(err)
	}
	child, err = a.Wait(ctx, child.ID)
	mustState(t, child, err, StateCompleted)
	if cevs := ledgerEvents(t, a.Ledger(), child.ID); countType(cevs, EvOutcomeEvaluated) != 0 || child.Outcome != nil {
		t.Fatalf("child events = %v", eventTypes(cevs))
	}
}

func TestOutcomeRevision(t *testing.T) {
	ctx := testCtx(t)
	g := &gradeModel{
		work: []*Response{endTurn("draft"), endTurn("final, with tests")},
		grades: []string{
			`{"verdict": "needs_revision", "notes": "Add the tests that the rubric asks for."}`,
			"Better.\n" + `{"verdict": "satisfied", "notes": ""}`,
		},
	}
	a := newTestAgent(t, g.Model())
	r, err := a.Run(ctx, "Fix the bug.", WithOutcome(Outcome{Rubric: "The change has tests.", Iterations: 2}))
	mustState(t, r, err, StateCompleted)
	if r.Output != "final, with tests" || r.Usage.Turns != 2 {
		t.Fatalf("run = %+v", r)
	}
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	passes := passesOf(t, evs)
	if len(passes) != 2 {
		t.Fatalf("passes = %+v", passes)
	}
	if p := passes[0]; p.Verdict != GradeNeedsRevision || p.Iteration != 1 || p.Iterations != 2 || !p.Continue || p.Notes != "Add the tests that the rubric asks for." {
		t.Fatalf("first pass = %+v", p)
	}
	if p := passes[1]; p.Verdict != GradeSatisfied || p.Iteration != 2 || p.Continue || p.Grader == passes[0].Grader {
		t.Fatalf("second pass = %+v", p)
	}
	// The run got one more turn with the notes of the grader.
	notes := outcomeNotes(t, evs)
	if len(notes) != 1 || !strings.Contains(notes[0], "Add the tests that the rubric asks for.") || !strings.Contains(notes[0], "pass 1 of at most 2") {
		t.Fatalf("notes messages = %q", notes)
	}
	work, grades := g.requests()
	if len(work) != 2 || len(grades) != 2 {
		t.Fatalf("requests: work %d, grades %d", len(work), len(grades))
	}
	last := work[1].Messages[len(work[1].Messages)-1]
	if last.Role != RoleUser || TextOf(last.Content) != notes[0] {
		t.Fatalf("the revision turn got %+v", last)
	}
	// The second pass reads the revised answer and the earlier notes.
	in := TextOf(grades[1].Messages[0].Content)
	for _, s := range []string{"final, with tests", "pass 2 of at most 2", "Pass 1, needs_revision: Add the tests that the rubric asks for."} {
		if !strings.Contains(in, s) {
			t.Errorf("second grader input lacks %q:\n%s", s, in)
		}
	}
	if r.Outcome == nil || r.Outcome.Verdict != GradeSatisfied || r.Outcome.Iterations != 2 || r.Outcome.Grader != passes[1].Grader {
		t.Fatalf("run outcome = %+v", r.Outcome)
	}
	if kids, _ := a.Children(ctx, r.ID); len(kids) != 2 {
		t.Fatalf("grader runs = %d", len(kids))
	}
}

func TestOutcomeUnparsed(t *testing.T) {
	ctx := testCtx(t)
	g := &gradeModel{
		work:   []*Response{endTurn("done")},
		grades: []string{"The work looks fine to me."},
	}
	a := newTestAgent(t, g.Model())
	// An inline profile can carry the outcome too.
	r, err := a.Run(ctx, "Do it.", WithRunProfile(AgentProfile{Outcome: &Outcome{Rubric: "It is done.", Iterations: 3}}))
	mustState(t, r, err, StateCompleted)
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	passes := passesOf(t, evs)
	if len(passes) != 1 || passes[0].Verdict != GradeUnparsed || passes[0].Continue || passes[0].Reason == "" || passes[0].Grader == "" {
		t.Fatalf("passes = %+v", passes)
	}
	if r.Output != "done" || r.Outcome == nil || r.Outcome.Verdict != GradeUnparsed || len(outcomeNotes(t, evs)) != 0 {
		t.Fatalf("run = %+v, outcome %+v", r, r.Outcome)
	}
	if _, grades := g.requests(); len(grades) != 1 {
		t.Fatalf("grader requests = %d", len(grades))
	}
}

func TestOutcomeMaxIterations(t *testing.T) {
	ctx := testCtx(t)
	g := &gradeModel{
		work:   []*Response{endTurn("The tests pass.")},
		grades: []string{`{"verdict": "needs_revision", "notes": "Name the command that ran the tests."}`},
	}
	a := newTestAgent(t, g.Model())
	r, err := a.Run(ctx, "Run the tests.", WithOutcome(Outcome{Rubric: "Every claim names the command that shows it.", Iterations: 1}))
	mustState(t, r, err, StateCompleted)
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	passes := passesOf(t, evs)
	if len(passes) != 1 {
		t.Fatalf("passes = %+v", passes)
	}
	if p := passes[0]; p.Verdict != GradeMaxIterations || p.Notes != "Name the command that ran the tests." || p.Continue || p.Iteration != 1 || p.Iterations != 1 {
		t.Fatalf("pass = %+v", p)
	}
	if r.Output != "The tests pass." || r.Usage.Turns != 1 || len(outcomeNotes(t, evs)) != 0 {
		t.Fatalf("run = %+v", r)
	}
	if r.Outcome == nil || r.Outcome.Verdict != GradeMaxIterations || r.Outcome.Notes != "Name the command that ran the tests." {
		t.Fatalf("run outcome = %+v", r.Outcome)
	}
}

func TestOutcomeSkippedAtTheBudget(t *testing.T) {
	ctx := testCtx(t)
	// The answer uses 15 tokens, all that the tree of the run has.
	g := &gradeModel{work: []*Response{endTurn("done")}}
	a := newTestAgent(t, g.Model())
	r, err := a.Run(ctx, "Do it.", WithOutcome(Outcome{Rubric: "It is done.", Iterations: 2}), WithRunBudget(Budget{MaxTokens: 15}))
	mustState(t, r, err, StateCompleted)
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	passes := passesOf(t, evs)
	if len(passes) != 1 {
		t.Fatalf("passes = %+v", passes)
	}
	if p := passes[0]; p.Verdict != GradeSkipped || p.Grader != "" || p.Continue || !strings.Contains(p.Reason, "maxTokens") {
		t.Fatalf("pass = %+v", p)
	}
	if _, grades := g.requests(); len(grades) != 0 {
		t.Fatalf("grader requests = %d", len(grades))
	}
	if kids, _ := a.Children(ctx, r.ID); len(kids) != 0 {
		t.Fatalf("children = %+v", kids)
	}
	if r.Output != "done" || r.Outcome == nil || r.Outcome.Verdict != GradeSkipped || r.Outcome.Reason != passes[0].Reason {
		t.Fatalf("run = %+v, outcome %+v", r, r.Outcome)
	}
}

func TestOutcomeOffLeavesTheRunAsItWas(t *testing.T) {
	ctx := testCtx(t)
	g := &gradeModel{work: []*Response{endTurn("one"), endTurn("two"), endTurn("three")}}
	prof := AgentProfile{Name: "graded", Outcome: &Outcome{Rubric: "It is done."}}
	a := newTestAgent(t, g.Model(), WithProfile(prof))
	plain, err := a.Run(ctx, "Do it.")
	mustState(t, plain, err, StateCompleted)
	// An empty block in the request turns the outcome of the profile off.
	off, err := a.Run(ctx, "Do it.", AsAgent("graded"), WithOutcome(Outcome{}))
	mustState(t, off, err, StateCompleted)
	// So does an empty block in the inline profile.
	inline, err := a.Run(ctx, "Do it.", AsAgent("graded"), WithRunProfile(AgentProfile{Outcome: &Outcome{}}))
	mustState(t, inline, err, StateCompleted)
	want := []string{EvRunQueued, EvRunStarted, EvTurnStarted, EvModelRequested, EvModelResponded, EvTurnFinished, EvRunFinished}
	for _, r := range []*Run{plain, off, inline} {
		evs := ledgerEvents(t, a.Ledger(), r.ID)
		if got := eventTypes(evs); !slices.Equal(got, want) {
			t.Fatalf("events = %v", got)
		}
		var q RunQueuedData
		firstOf(t, evs, EvRunQueued, &q)
		if q.Outcome != nil || r.Outcome != nil {
			t.Fatalf("queued = %s, outcome %+v", evs[0].Data, r.Outcome)
		}
	}
	// The ledger of a run without an outcome is the same as before.
	if evs := ledgerEvents(t, a.Ledger(), plain.ID); strings.Contains(string(evs[0].Data), `"outcome"`) {
		t.Fatalf("queued = %s", evs[0].Data)
	}
	if _, grades := g.requests(); len(grades) != 0 {
		t.Fatalf("grader requests = %d", len(grades))
	}
}

func TestOutcomeGraderModel(t *testing.T) {
	ctx := testCtx(t)
	g := &gradeModel{work: []*Response{endTurn("done")}}
	a := newTestAgent(t, g.Model())
	r, err := a.Run(ctx, "Do it.", WithOutcome(Outcome{Rubric: "It is done.", Grader: outcomeProvider + "/judge"}))
	mustState(t, r, err, StateCompleted)
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	passes := passesOf(t, evs)
	if len(passes) != 1 || passes[0].Verdict != GradeSatisfied || !strings.Contains(passes[0].Notes, "judge") {
		t.Fatalf("passes = %+v", passes)
	}
	var gs RunStartedData
	firstOf(t, ledgerEvents(t, a.Ledger(), passes[0].Grader), EvRunStarted, &gs)
	if gs.Model != outcomeProvider+"/judge" {
		t.Fatalf("grader model = %q", gs.Model)
	}
	var s RunStartedData
	firstOf(t, evs, EvRunStarted, &s)
	if s.Model != "anthropic/claude-opus-5-5" {
		t.Fatalf("run model = %q", s.Model)
	}
}

func TestOutcomeResumesBetweenTheVerdictAndTheNotes(t *testing.T) {
	ctx := testCtx(t)
	l := NewMemLedger()
	id := "run-outcome"
	o := &Outcome{Rubric: "It has tests.", Iterations: 2}
	seed(t, l, id,
		MustEvent(id, EvRunQueued, RunQueuedData{Agent: "default", Input: "Fix it.", Origin: OriginCLI, Outcome: o}),
		MustEvent(id, EvRunStarted, RunStartedData{Model: "anthropic/claude-opus-5-5"}),
		MustEvent(id, EvTurnStarted, TurnData{Turn: 1}),
		MustEvent(id, EvModelRequested, ModelRequestedData{Provider: "anthropic", Model: "claude-opus-5-5"}),
		MustEvent(id, EvModelResponded, ModelRespondedData{Content: []Block{Text("draft")}, StopReason: StopEndTurn}),
		MustEvent(id, EvTurnFinished, TurnData{Turn: 1}),
		MustEvent(id, EvOutcomeEvaluated, OutcomeEvaluatedData{Verdict: GradeNeedsRevision, Notes: "Add tests.", Iteration: 1, Iterations: 2,
			Grader: "grader-1", Continue: true, Answer: 5}),
	)
	// The second grader run started before the process stopped, and it
	// resumes with its parent.
	gid := "grader-2"
	seed(t, l, gid,
		MustEvent(gid, EvRunQueued, RunQueuedData{Agent: "default", Input: "grade it", Origin: OriginKernel, Parent: id, Depth: 1,
			Metadata: map[string]string{"idempotencyKey": "outcome:" + id + ":12"}, Profile: &AgentProfile{System: graderSystem, Outcome: &Outcome{}}}),
	)
	g := &gradeModel{
		work:   []*Response{endTurn("final, with tests")},
		grades: []string{`{"verdict": "satisfied", "notes": "Tests exist."}`},
	}
	a := newTestAgent(t, g.Model(), WithLedger(l))
	r, err := a.Wait(ctx, id)
	mustState(t, r, err, StateCompleted)
	evs := ledgerEvents(t, l, id)
	if notes := outcomeNotes(t, evs); len(notes) != 1 || !strings.Contains(notes[0], "Add tests.") {
		t.Fatalf("notes = %q (events %v)", notes, eventTypes(evs))
	}
	passes := passesOf(t, evs)
	if len(passes) != 2 || passes[1].Verdict != GradeSatisfied || passes[1].Iteration != 2 || passes[1].Answer != 12 {
		t.Fatalf("passes = %+v (events %v)", passes, eventTypes(evs))
	}
	// The pass after the restart took the grader run of its answer instead
	// of a second one.
	if passes[1].Grader != gid {
		t.Fatalf("grader = %q, want %q", passes[1].Grader, gid)
	}
	if r.Output != "final, with tests" || r.Outcome == nil || r.Outcome.Verdict != GradeSatisfied {
		t.Fatalf("run = %+v", r)
	}
}

func TestOutcomeGraderReadsOnly(t *testing.T) {
	ctx := testCtx(t)
	var mu sync.Mutex
	var commands []string
	files := NewTool(ToolSpec{Name: "files"}, func(_ context.Context, in json.RawMessage) (ToolResult, error) {
		var v struct{ Command string }
		_ = json.Unmarshal(in, &v)
		mu.Lock()
		commands = append(commands, v.Command)
		mu.Unlock()
		return TextResult("ok"), nil
	})
	g := &gradeModel{
		work: []*Response{endTurn("done")},
		graderCalls: []*Response{callTools(
			use("g1", "files", `{"command":"view","path":"a.txt"}`),
			use("g2", "files", `{"command":"create","path":"a.txt","file_text":"x"}`),
		)},
		grades: []string{`{"verdict": "satisfied", "notes": ""}`},
	}
	a := newTestAgent(t, g.Model(), WithTools(files, echoTool(nil)))
	r, err := a.Run(ctx, "Do it.", WithOutcome(Outcome{Rubric: "It is done."}))
	mustState(t, r, err, StateCompleted)
	if r.Outcome == nil || r.Outcome.Verdict != GradeSatisfied {
		t.Fatalf("outcome = %+v", r.Outcome)
	}
	mu.Lock()
	got := slices.Clone(commands)
	mu.Unlock()
	if !slices.Equal(got, []string{"view"}) {
		t.Fatalf("commands = %v", got)
	}
	_, grades := g.requests()
	if len(grades) != 2 || len(grades[0].Tools) != 1 || grades[0].Tools[0].Name != "files" {
		t.Fatalf("grader requests = %d, tools %+v", len(grades), grades[0].Tools)
	}
	res := lastToolResults(t, grades[1])
	if len(res) != 2 || res[0].IsError || !res[1].IsError {
		t.Fatalf("tool results = %+v", res)
	}
}

func TestOutcomeChecks(t *testing.T) {
	ctx := testCtx(t)
	a := newTestAgent(t, newScript().Model())
	for _, o := range []Outcome{
		{Rubric: "x", Iterations: MaxOutcomeIterations + 1},
		{Rubric: "x", Iterations: -1},
		{Grader: "anthropic/claude-haiku-5-5"},
		{Iterations: 2},
		{Rubric: "x", Grader: "no-selector"},
		{Rubric: strings.Repeat("x", maxInstructionBytes+1)},
	} {
		if _, err := a.Start(ctx, "x", WithOutcome(o)); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("Start with %+v: %v", o.Iterations, err)
		}
		if err := a.CheckProfile(AgentProfile{Name: "p", Outcome: &o}); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("CheckProfile with grader %q: %v", o.Grader, err)
		}
	}
	if err := CheckOutcome(nil, &Outcome{Rubric: "x", Grader: "anthropic/claude-haiku-5-5", Iterations: 3}); err != nil {
		t.Errorf("a valid outcome: %v", err)
	}
	if err := CheckOutcome(nil, &Outcome{}); err != nil {
		t.Errorf("an empty outcome: %v", err)
	}
}

func TestParseGrade(t *testing.T) {
	for _, tc := range []struct {
		in      string
		verdict string
		ok      bool
	}{
		{`{"verdict": "satisfied", "notes": "ok"}`, GradeSatisfied, true},
		{"Reasons.\n\n" + `{"verdict": "needs_revision", "notes": "fix x"}` + "\n", GradeNeedsRevision, true},
		{`{"verdict": "satisfied"}`, "", false},
		{`{"verdict": "great", "notes": ""}`, "", false},
		{`{"verdict": "satisfied", "notes": "", "score": 1}`, "", false},
		{`{"verdict": "satisfied", "notes": ""} more`, "", false},
		{"```json\n" + `{"verdict": "satisfied", "notes": ""}` + "\n```", "", false},
		{"", "", false},
	} {
		v, _, ok := parseGrade(tc.in)
		if v != tc.verdict || ok != tc.ok {
			t.Errorf("parseGrade(%q) = %q, %t", tc.in, v, ok)
		}
	}
}
