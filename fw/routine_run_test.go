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

// fakeDecider answers every question from a table by question name (the
// part of the id after the routine), and records the calls.
type fakeDecider struct {
	mu      sync.Mutex
	answers map[string]Answer
	err     error
	calls   int
	states  []string
	ids     [][]string
}

func (f *fakeDecider) Decide(_ context.Context, state string, qs map[string]Question) (DecideResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.states = append(f.states, state)
	var ids []string
	out := DecideResult{Model: "fake-clef", Answers: map[string]Answer{}, LatencyMS: 7}
	for id, q := range qs {
		ids = append(ids, id)
		_, name, _ := strings.Cut(id, ".")
		a, ok := f.answers[name]
		if !ok {
			continue
		}
		a.Type = q.Type
		out.Answers[id] = a
	}
	f.ids = append(f.ids, ids)
	if f.err != nil {
		return DecideResult{}, f.err
	}
	return out, nil
}

// helpdesk is a fake issue tracker: labels, writes, and a read that shows
// the labels that the writes added.
type helpdesk struct {
	mu       sync.Mutex
	writes   []string
	labels   []string
	failOn   string // a method of issue_write that answers with an error
	readText string // replaces the text of issue_read when set
}

func (h *helpdesk) tools() []Tool {
	read := NewTool(ToolSpec{Name: "gitea.label_read", ReadOnly: true, Idempotent: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		return TextResult(`[{"id":11,"name":"type/bug"},{"id":12,"name":"type/outage"},{"id":21,"name":"status/new"},{"id":22,"name":"status/triaged"}]`), nil
	})
	write := NewTool(ToolSpec{Name: "gitea.issue_write"}, func(_ context.Context, in json.RawMessage) (ToolResult, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		var v struct {
			Method string `json:"method"`
			Labels []int  `json:"labels"`
		}
		_ = json.Unmarshal(in, &v)
		if v.Method == h.failOn {
			return ErrorResult("gitea: 503 Service Unavailable"), nil
		}
		h.writes = append(h.writes, string(in))
		for _, id := range v.Labels {
			switch id {
			case 11:
				h.labels = append(h.labels, "type/bug")
			case 22:
				h.labels = append(h.labels, "status/triaged")
			}
		}
		return TextResult("ok"), nil
	})
	issue := NewTool(ToolSpec{Name: "gitea.issue_read", ReadOnly: true, Idempotent: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.readText != "" {
			return TextResult(h.readText), nil
		}
		return TextResult(`{"number": 42, "labels": "` + strings.Join(h.labels, ",") + `"}`), nil
	})
	return []Tool{read, write, issue}
}

// ackRoutine is a small routine of the helpdesk with two questions.
func ackRoutine(standing string) Routine {
	zero := 0.0
	return Routine{
		Name: "ack", Version: 2, Standing: standing, Canary: &zero, Decider: "clef",
		Questions: map[string]RoutineQuestion{
			"type": {
				Question: Question{Type: QuestionChoice, Instructions: "What kind of ticket is this?", Criteria: map[string]any{"bug": "Broken", "outage": "Down", "other": "Else"}},
				Act:      0.85, Expect: Expect{"bug", "outage"},
				Observe: &Observe{Answer: `"type": "type/([a-z-]+)"`},
			},
			"beyond": {
				Question: Question{Type: QuestionNoul, Instructions: "Does it ask for more than a label and a comment?"},
				Act:      0.95, Expect: Expect{"no"},
				Observe: &Observe{Tool: "gitea.issue_write", Match: `"method":"close"`},
			},
		},
		Steps: []RoutineStep{
			{Tool: "gitea.label_read", Input: `{"method": "list"}`, As: "labels"},
			{Tool: "gitea.issue_write", Input: `{"method": "add_labels", "issue_number": {{ find .input "#(\\d+)" }}, "labels": [{{ find .labels (printf "\"id\":(\\d+),\"name\":\"type/%s\"" .type) }}, {{ find .labels "\"id\":(\\d+),\"name\":\"status/triaged\"" }}]}`},
			{Tool: "gitea.issue_write", Input: `{"method": "add_comment", "issue_number": {{ find .input "#(\\d+)" }}, "body": {{ json (printf "Thanks. This looks like a %s." .type) }}}`},
		},
		After: []RoutineCheck{{
			Tool: "gitea.issue_read", Input: `{"method": "get", "issue_number": {{ find .input "#(\\d+)" }}}`,
			Contains: []string{"status/triaged", "type/{{ .type }}"}, Lacks: []string{"status/new"},
		}},
		Answer: `TRIAGE {"number": {{ find .input "#(\\d+)" }}, "type": "type/{{ .type }}"}`,
	}
}

// confident answers that let ack apply.
func confident() map[string]Answer {
	return map[string]Answer{
		"type":   {Choice: "bug", Probability: 0.93, Probabilities: map[string]float64{"bug": 0.93, "outage": 0.05, "other": 0.02}},
		"beyond": {Probability: 0.01},
	}
}

func withDeciders(o *options) {
	if o.cfg == nil {
		o.cfg = DefaultConfig()
	}
	o.cfg.Deciders = map[string]DeciderConfig{"clef": {BaseURL: "http://127.0.0.1:1", Model: "clef"}}
}

func routineAgent(t *testing.T, m Model, d Decider, h *helpdesk, routines []Routine, opts ...Option) *Agent {
	t.Helper()
	base := []Option{withDeciders, WithDecider("clef", d), WithTools(h.tools()...),
		WithProfile(AgentProfile{Name: "triage", Routines: routines})}
	return newTestAgent(t, m, append(base, opts...)...)
}

// requested returns the data of every fw.tool.requested of a run.
func requested(evs []Event) []ToolRequestedData {
	var out []ToolRequestedData
	for _, e := range evs {
		if e.Type == EvToolRequested {
			var d ToolRequestedData
			_ = e.Decode(&d)
			out = append(out, d)
		}
	}
	return out
}

func decodeAll[T any](t *testing.T, evs []Event, typ string) []T {
	t.Helper()
	var out []T
	for _, e := range evs {
		if e.Type == typ {
			var v T
			if err := e.Decode(&v); err != nil {
				t.Fatal(err)
			}
			out = append(out, v)
		}
	}
	return out
}

// firstMessage returns the text of the first user message of a request.
func firstMessage(req *Request) string { return TextOf(req.Messages[0].Content) }

func TestRoutineLiveApplies(t *testing.T) {
	ctx := testCtx(t)
	s := newScript() // the model must not be called
	d := &fakeDecider{answers: confident()}
	h := &helpdesk{}
	a := routineAgent(t, s.Model(), d, h, []Routine{ackRoutine(RoutineLive)})
	r, err := a.Run(ctx, "Ticket #42: the printer is broken", AsAgent("triage"))
	mustState(t, r, err, StateCompleted)
	if r.Output != `TRIAGE {"number": 42, "type": "type/bug"}` {
		t.Fatalf("output = %q", r.Output)
	}
	if n := len(s.requests()); n != 0 {
		t.Fatalf("the model got %d calls", n)
	}
	if d.calls != 1 || d.states[0] != "Ticket #42: the printer is broken" || len(d.ids[0]) != 2 {
		t.Fatalf("decider calls %d, states %v, ids %v", d.calls, d.states, d.ids)
	}
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	want := []string{EvRunQueued, EvRunStarted, EvToolRequested, EvToolResponded, EvRoutineDecided,
		EvToolRequested, EvToolResponded, EvToolRequested, EvToolResponded, EvToolRequested, EvToolResponded,
		EvToolRequested, EvToolResponded, EvRoutineApplied, EvRunFinished}
	if got := strings.Join(eventTypes(evs), " "); got != strings.Join(want, " ") {
		t.Fatalf("events =\n%s", got)
	}
	calls := requested(evs)
	if calls[0].Tool != "decide" || calls[0].Routine != "ack" {
		t.Errorf("the decider call = %+v", calls[0])
	}
	for _, c := range calls {
		if c.Routine != "ack" || !strings.HasPrefix(c.CallID, routineCallPrefix) {
			t.Errorf("a call of the runner lacks the routine: %+v", c)
		}
	}
	if string(calls[2].Input) != `{"method":"add_labels","issue_number":42,"labels":[11,22]}` {
		t.Errorf("the labels input = %s", calls[2].Input)
	}
	if string(calls[3].Input) != `{"method":"add_comment","issue_number":42,"body":"Thanks. This looks like a bug."}` {
		t.Errorf("the comment input = %s", calls[3].Input)
	}
	var dec RoutineDecidedData
	firstOf(t, evs, EvRoutineDecided, &dec)
	if dec.Routine != "ack" || dec.Version != 2 || dec.Mode != RoutineLive || dec.Standing != RoutineLive || !dec.Applies ||
		dec.Decider != "clef" || dec.Model != "fake-clef" || dec.Answers["type"].Choice != "bug" || dec.LatencyMS != 7 {
		t.Errorf("decided = %+v", dec)
	}
	var app RoutineAppliedData
	firstOf(t, evs, EvRoutineApplied, &app)
	if !app.OK || app.Steps != 4 || app.FailedStep != 0 || app.Answer != r.Output {
		t.Errorf("applied = %+v", app)
	}
	if r.Usage.ToolCalls != 5 {
		t.Errorf("the budget counts %d tool calls", r.Usage.ToolCalls)
	}
	checkCursorPrefixes(t, evs)
}

func TestRoutineFallsThrough(t *testing.T) {
	cases := []struct {
		name    string
		answers func() map[string]Answer
		failOn  string
		read    string
		input   string
		reason  string // of fw.routine.decided, or of fw.routine.applied
		applied bool
		failed  int
		steps   int // fw.tool.requested of the steps and checks
	}{
		{name: "below act", answers: func() map[string]Answer {
			m := confident()
			m["type"] = Answer{Choice: "bug", Probability: 0.61, Probabilities: map[string]float64{"bug": 0.61}}
			return m
		}, reason: "type below act: 0.61"},
		{name: "step failure", answers: confident, failOn: "add_comment", applied: true, failed: 3, steps: 3,
			reason: "step 3 gitea.issue_write: error: gitea: 503 Service Unavailable"},
		{name: "check failure", answers: confident, read: `{"labels": "type/bug,status/new"}`, applied: true, failed: 4, steps: 4,
			reason: `check 1 gitea.issue_read: the result lacks "status/triaged"`},
		{name: "empty hole", answers: confident, input: "Ticket without a number", applied: true, failed: 2, steps: 1,
			reason: `step 2 gitea.issue_write: hole #(\d+) empty`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := testCtx(t)
			s := newScript(endTurn(`TRIAGE {"type": "type/bug"}`))
			h := &helpdesk{failOn: c.failOn, readText: c.read}
			a := routineAgent(t, s.Model(), &fakeDecider{answers: c.answers()}, h, []Routine{ackRoutine(RoutineLive)})
			input := c.input
			if input == "" {
				input = "Ticket #42: the printer is broken"
			}
			r, err := a.Run(ctx, input, AsAgent("triage"))
			mustState(t, r, err, StateCompleted)
			if r.Output != `TRIAGE {"type": "type/bug"}` || len(s.requests()) != 1 {
				t.Fatalf("output %q, model calls %d", r.Output, len(s.requests()))
			}
			evs := ledgerEvents(t, a.Ledger(), r.ID)
			var dec RoutineDecidedData
			firstOf(t, evs, EvRoutineDecided, &dec)
			apps := decodeAll[RoutineAppliedData](t, evs, EvRoutineApplied)
			if c.applied {
				if len(apps) != 1 || apps[0].OK || apps[0].FailedStep != c.failed || apps[0].Reason != c.reason || !dec.Applies {
					t.Fatalf("applied = %+v", apps)
				}
			} else if dec.Applies || dec.Reason != c.reason || len(apps) != 0 {
				t.Fatalf("decided = %+v, applied = %+v", dec, apps)
			}
			if n := len(requested(evs)) - 1; n != c.steps {
				t.Errorf("%d calls of steps and checks; want %d", n, c.steps)
			}
			// The model starts warm: one block with the decision and what
			// the routine did, and the sentence about context blocks.
			req := s.requests()[0]
			first := firstMessage(req)
			if strings.Count(first, `<context source="routine">`) != 1 || !strings.Contains(first, c.reason) ||
				!strings.Contains(first, "beyond: the probability of yes is") || !strings.HasSuffix(first, input) {
				t.Errorf("first message =\n%s", first)
			}
			if c.applied && !strings.Contains(first, "Calls of routine ack:\n1. gitea.label_read, ok:") {
				t.Errorf("the block lacks the calls:\n%s", first)
			}
			if !strings.Contains(req.System, ContextNote) {
				t.Error("the system prompt lacks the context note")
			}
			// A live routine that did not complete is compared, before
			// the end of the run.
			cmp := decodeAll[RoutineComparedData](t, evs, EvRoutineCompared)
			if len(cmp) != 2 || evs[len(evs)-1].Type != EvRunFinished {
				t.Fatalf("compared = %+v, events %v", cmp, eventTypes(evs))
			}
			for _, x := range cmp {
				if x.Mode != RoutineLive || x.Routine != "ack" || x.Version != 2 {
					t.Errorf("compared = %+v", x)
				}
			}
			checkCursorPrefixes(t, evs)
		})
	}
}

func TestRoutineShadow(t *testing.T) {
	ctx := testCtx(t)
	s := newScript(endTurn(`TRIAGE {"type": "type/outage"}`))
	h := &helpdesk{}
	a := routineAgent(t, s.Model(), &fakeDecider{answers: confident()}, h, []Routine{ackRoutine(RoutineShadow)})
	r, err := a.Run(ctx, "Ticket #42: the printer is broken", AsAgent("triage"))
	mustState(t, r, err, StateCompleted)
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	var dec RoutineDecidedData
	firstOf(t, evs, EvRoutineDecided, &dec)
	if dec.Mode != RoutineShadow || !dec.Applies {
		t.Fatalf("decided = %+v", dec)
	}
	if len(h.writes) != 0 || countType(evs, EvRoutineApplied) != 0 || len(requested(evs)) != 1 {
		t.Fatalf("a shadow routine acted: writes %v, events %v", h.writes, eventTypes(evs))
	}
	// The model never sees the answers of a rehearsal.
	req := s.requests()[0]
	if first := firstMessage(req); strings.Contains(first, "<context") || strings.Contains(req.System, ContextNote) {
		t.Fatalf("a shadow run got a block:\n%s", first)
	}
	cmp := decodeAll[RoutineComparedData](t, evs, EvRoutineCompared)
	got := map[string]RoutineComparedData{}
	for _, c := range cmp {
		got[c.Question] = c
	}
	if c := got["type"]; c.Mode != RoutineShadow || c.Said != "bug" || c.Observed != "outage" || c.Agree {
		t.Errorf("type = %+v", c)
	}
	if c := got["beyond"]; c.Said != "no" || c.Observed != "no" || !c.Agree {
		t.Errorf("beyond = %+v", c)
	}
	// A finished run stays finished: the comparison comes before the end,
	// and nothing follows it.
	if evs[len(evs)-1].Type != EvRunFinished || evs[len(evs)-2].Type != EvRoutineCompared {
		t.Fatalf("events = %v", eventTypes(evs))
	}
	time.Sleep(50 * time.Millisecond)
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)
	if after := ledgerEvents(t, a.Ledger(), r.ID); len(after) != len(evs) {
		t.Fatalf("events after the end: %v", eventTypes(after[len(evs):]))
	}
	checkCursorPrefixes(t, evs)
}

func TestRoutineCanaryPick(t *testing.T) {
	ctx := testCtx(t)
	var picked, passed string
	for i := 0; picked == "" || passed == ""; i++ {
		id := fmt.Sprintf("run-canary-%d", i)
		if canaryPick(id, 0.5) {
			picked = id
		} else {
			passed = id
		}
	}
	routine := ackRoutine(RoutineLive)
	half := 0.5
	routine.Canary = &half
	for _, c := range []struct {
		id   string
		mode string
	}{{picked, RoutineCanary}, {passed, RoutineLive}} {
		s := newScript(endTurn(`TRIAGE {"type": "type/bug"}`))
		h := &helpdesk{}
		a := routineAgent(t, s.Model(), &fakeDecider{answers: confident()}, h, []Routine{routine})
		r, err := a.Run(ctx, "Ticket #42: the printer is broken", AsAgent("triage"), func(r *RunRequest) { r.ID = c.id })
		mustState(t, r, err, StateCompleted)
		evs := ledgerEvents(t, a.Ledger(), r.ID)
		var dec RoutineDecidedData
		firstOf(t, evs, EvRoutineDecided, &dec)
		if dec.Mode != c.mode || !dec.Applies {
			t.Fatalf("%s: decided = %+v", c.id, dec)
		}
		if c.mode == RoutineCanary {
			// The agent handles a canary run, without the answers.
			if len(h.writes) != 0 || len(s.requests()) != 1 || strings.Contains(firstMessage(s.requests()[0]), "<context") {
				t.Fatalf("canary: writes %v, model calls %d", h.writes, len(s.requests()))
			}
			// Two questions, and the calls: the agent made none, so the
			// calls differ at the first step.
			cmp := decodeAll[RoutineComparedData](t, evs, EvRoutineCompared)
			if len(cmp) != 3 || cmp[0].Mode != RoutineCanary || !cmp[2].Calls || cmp[2].Agree || cmp[2].Step != 1 || cmp[2].Mode != RoutineCanary {
				t.Fatalf("canary compared = %+v", cmp)
			}
		} else if len(s.requests()) != 0 || r.Output == "" {
			t.Fatalf("live: model calls %d, output %q", len(s.requests()), r.Output)
		}
	}
}

func TestRoutinePolicy(t *testing.T) {
	t.Run("deny rule", func(t *testing.T) {
		ctx := testCtx(t)
		s := newScript(endTurn("handled"))
		h := &helpdesk{}
		a := routineAgent(t, s.Model(), &fakeDecider{answers: confident()}, h, []Routine{ackRoutine(RoutineLive)},
			WithPolicy(NewRulesPolicy(Rules{Default: "allow", Deny: []string{"gitea.issue_write"}})))
		r, err := a.Run(ctx, "Ticket #42: the printer is broken", AsAgent("triage"))
		mustState(t, r, err, StateCompleted)
		evs := ledgerEvents(t, a.Ledger(), r.ID)
		var app RoutineAppliedData
		firstOf(t, evs, EvRoutineApplied, &app)
		if app.OK || app.FailedStep != 2 || !strings.HasPrefix(app.Reason, "step 2 gitea.issue_write: denied by policy:") {
			t.Fatalf("applied = %+v", app)
		}
		for _, c := range requested(evs) {
			if c.Tool == "gitea.issue_write" {
				t.Fatal("a denied call has a fw.tool.requested")
			}
		}
		if len(h.writes) != 0 || r.Output != "handled" {
			t.Fatalf("writes %v, output %q", h.writes, r.Output)
		}
	})
	t.Run("a person denies", func(t *testing.T) {
		ctx := testCtx(t)
		s := newScript(endTurn("handled"))
		h := &helpdesk{}
		a := routineAgent(t, s.Model(), &fakeDecider{answers: confident()}, h, []Routine{ackRoutine(RoutineLive)},
			WithPolicy(NewRulesPolicy(Rules{Default: "allow", Ask: []string{"gitea.issue_write"}})))
		r, err := a.Start(ctx, "Ticket #42: the printer is broken", AsAgent("triage"))
		if err != nil {
			t.Fatal(err)
		}
		r = waitPending(t, a, r.ID)
		if r.Pending.Tool != "gitea.issue_write" || !strings.HasPrefix(r.Pending.CallID, routineCallPrefix) {
			t.Fatalf("pending = %+v", r.Pending)
		}
		if err := a.Decide(ctx, r.ID, ApprovalDecision{ApprovalID: r.Pending.ID, Decision: "deny", Decider: "ada", Note: "not now"}); err != nil {
			t.Fatal(err)
		}
		r, err = a.Wait(ctx, r.ID)
		mustState(t, r, err, StateCompleted)
		var app RoutineAppliedData
		firstOf(t, ledgerEvents(t, a.Ledger(), r.ID), EvRoutineApplied, &app)
		if app.OK || app.Reason != "step 2 gitea.issue_write: denied by ada: not now" || len(h.writes) != 0 || r.Output != "handled" {
			t.Fatalf("applied = %+v, writes %v", app, h.writes)
		}
	})
	t.Run("a person allows", func(t *testing.T) {
		ctx := testCtx(t)
		s := newScript()
		h := &helpdesk{}
		a := routineAgent(t, s.Model(), &fakeDecider{answers: confident()}, h, []Routine{ackRoutine(RoutineLive)},
			WithPolicy(NewRulesPolicy(Rules{Default: "allow", Ask: []string{"gitea.issue_write"}})))
		r, err := a.Start(ctx, "Ticket #42: the printer is broken", AsAgent("triage"))
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			r = waitPending(t, a, r.ID)
			if err := a.Decide(ctx, r.ID, ApprovalDecision{ApprovalID: r.Pending.ID, Decision: "allow"}); err != nil {
				t.Fatal(err)
			}
		}
		r, err = a.Wait(ctx, r.ID)
		mustState(t, r, err, StateCompleted)
		if len(h.writes) != 2 || len(s.requests()) != 0 || !strings.HasPrefix(r.Output, "TRIAGE") {
			t.Fatalf("writes %v, model calls %d, output %q", h.writes, len(s.requests()), r.Output)
		}
	})
}

// waitPending waits until the run waits for an approval.
func waitPending(t *testing.T, a *Agent, id string) *Run {
	t.Helper()
	ctx := testCtx(t)
	for {
		r, err := a.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if r.State == StateInputRequired && r.Pending != nil {
			return r
		}
		if r.State.Terminal() {
			t.Fatalf("the run ended: %+v", r)
		}
		select {
		case <-ctx.Done():
			t.Fatal("no approval came")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestRoutineDeciderError(t *testing.T) {
	ctx := testCtx(t)
	s := newScript(endTurn("handled"))
	shadow := ackRoutine(RoutineShadow)
	shadow.Name = "ack-shadow"
	d := &fakeDecider{err: errors.New("the server answered 529: overloaded")}
	a := routineAgent(t, s.Model(), d, &helpdesk{}, []Routine{ackRoutine(RoutineLive), shadow})
	r, err := a.Run(ctx, "Ticket #42: the printer is broken", AsAgent("triage"))
	mustState(t, r, err, StateCompleted)
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	decs := decodeAll[RoutineDecidedData](t, evs, EvRoutineDecided)
	if d.calls != 1 || len(decs) != 2 || len(d.ids[0]) != 4 {
		t.Fatalf("decider calls %d, ids %v, decided %+v", d.calls, d.ids, decs)
	}
	for _, x := range decs {
		if x.Applies || !strings.HasPrefix(x.Reason, "decider: the server answered 529") {
			t.Errorf("decided = %+v", x)
		}
	}
	if calls := requested(evs); len(calls) != 1 || calls[0].Routine != "ack,ack-shadow" {
		t.Errorf("the shared decider call = %+v", calls)
	}
	// No answer, no comparison; the model runs.
	if countType(evs, EvRoutineCompared) != 0 || r.Output != "handled" {
		t.Fatalf("events = %v", eventTypes(evs))
	}
}

func TestRoutineResumeClosesAnInterruptedRoutine(t *testing.T) {
	ctx := testCtx(t)
	l := NewMemLedger()
	id := "run-routine-resume"
	routine := ackRoutine(RoutineLive)
	seed(t, l, id,
		MustEvent(id, EvRunQueued, RunQueuedData{Agent: "triage", Input: "Ticket #42", Origin: OriginCLI,
			Resolved: &ResolvedProfile{Model: "anthropic/claude-opus-5-5", Routines: []Routine{routine}}}),
		MustEvent(id, EvRunStarted, RunStartedData{Model: "anthropic/claude-opus-5-5"}),
		MustEvent(id, EvToolRequested, ToolRequestedData{Tool: "decide", CallID: routineCallPrefix + "1", Input: json.RawMessage(`{}`), Routine: "ack"}),
		MustEvent(id, EvToolResponded, ToolRespondedData{Tool: "decide", CallID: routineCallPrefix + "1", Content: []Block{Text("{}")}}),
		MustEvent(id, EvRoutineDecided, RoutineDecidedData{Routine: "ack", Version: 2, Standing: RoutineLive, Mode: RoutineLive, Applies: true,
			Answers: map[string]Answer{"type": {Type: QuestionChoice, Choice: "bug", Probability: 0.93}}}),
		MustEvent(id, EvToolRequested, ToolRequestedData{Tool: "gitea.label_read", CallID: routineCallPrefix + "2", Input: json.RawMessage(`{}`), Routine: "ack"}),
	)
	s := newScript(endTurn("handled"))
	d := &fakeDecider{answers: confident()}
	a := routineAgent(t, s.Model(), d, &helpdesk{}, []Routine{routine}, WithLedger(l))
	r, err := a.Wait(ctx, id)
	mustState(t, r, err, StateCompleted)
	evs := ledgerEvents(t, l, id)
	var app RoutineAppliedData
	firstOf(t, evs, EvRoutineApplied, &app)
	if app.OK || app.Reason != routineInterrupted || app.Steps != 1 {
		t.Fatalf("applied = %+v", app)
	}
	// The runner never acts again on a resumed run.
	if d.calls != 0 || countType(evs, EvRoutineDecided) != 1 {
		t.Fatalf("the runner acted again: %v", eventTypes(evs))
	}
	first := firstMessage(s.requests()[0])
	if !strings.Contains(first, `<context source="routine">`) || !strings.Contains(first, "1. gitea.label_read, no result") || !strings.Contains(first, routineInterrupted) {
		t.Fatalf("first message =\n%s", first)
	}
	checkCursorPrefixes(t, evs)
}

func TestRoutineAnInlineProfileIsChecked(t *testing.T) {
	ctx := testCtx(t)
	a := routineAgent(t, newScript().Model(), &fakeDecider{}, &helpdesk{}, nil)
	bad := ackRoutine(RoutineLive)
	bad.Decider = "jev"
	_, err := a.Start(ctx, "x", WithRunProfile(AgentProfile{Routines: []Routine{bad}}))
	if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), `no model answers questions under the name "jev"`) {
		t.Fatalf("start = %v", err)
	}
	// The inline profile replaces the routines and pins them.
	s := newScript()
	h := &helpdesk{}
	a = routineAgent(t, s.Model(), &fakeDecider{answers: confident()}, h, nil)
	r, err := a.Run(ctx, "Ticket #42: the printer is broken", AsAgent("triage"), WithRunProfile(AgentProfile{Routines: []Routine{ackRoutine(RoutineLive)}}))
	mustState(t, r, err, StateCompleted)
	if len(h.writes) != 2 {
		t.Fatalf("writes = %v", h.writes)
	}
	var q RunQueuedData
	firstOf(t, ledgerEvents(t, a.Ledger(), r.ID), EvRunQueued, &q)
	if q.Resolved == nil || len(q.Resolved.Routines) != 1 || q.Resolved.Routines[0].Name != "ack" {
		t.Fatalf("resolved = %+v", q.Resolved)
	}
}

// A restart while a call of the routine waits for an approval ends the
// routine at once: the kernel denies the approval, so a later answer of a
// person is refused, and the model starts warm.
func TestRoutineRestartDuringApproval(t *testing.T) {
	ctx := testCtx(t)
	l := NewMemLedger()
	id := "run-routine-approval"
	routine := ackRoutine(RoutineLive)
	seed(t, l, id,
		MustEvent(id, EvRunQueued, RunQueuedData{Agent: "triage", Input: "Ticket #42", Origin: OriginCLI,
			Resolved: &ResolvedProfile{Model: "anthropic/claude-opus-5-5", Routines: []Routine{routine}}}),
		MustEvent(id, EvRunStarted, RunStartedData{Model: "anthropic/claude-opus-5-5"}),
		MustEvent(id, EvRoutineDecided, RoutineDecidedData{Routine: "ack", Version: 2, Standing: RoutineLive, Mode: RoutineLive, Applies: true}),
		MustEvent(id, EvToolRequested, ToolRequestedData{Tool: "gitea.label_read", CallID: routineCallPrefix + "1", Input: json.RawMessage(`{}`), Routine: "ack"}),
		MustEvent(id, EvToolResponded, ToolRespondedData{Tool: "gitea.label_read", CallID: routineCallPrefix + "1", Content: []Block{Text("[]")}}),
		MustEvent(id, EvApprovalRequested, ApprovalRequestedData{ApprovalID: "ap1", Tool: "gitea.issue_write", CallID: routineCallPrefix + "2", Input: json.RawMessage(`{}`), Reason: "rule: ask"}),
	)
	s := newScript(endTurn("handled"))
	h := &helpdesk{}
	a := routineAgent(t, s.Model(), &fakeDecider{answers: confident()}, h, []Routine{routine}, WithLedger(l))
	r := waitTerminal(t, a, id)
	mustState(t, r, nil, StateCompleted)
	evs := ledgerEvents(t, l, id)
	var dec ApprovalDecidedData
	firstOf(t, evs, EvApprovalDecided, &dec)
	if dec.ApprovalID != "ap1" || dec.Decision != "deny" || dec.Decider != OriginKernel || dec.Note != routineInterrupted {
		t.Fatalf("decided = %+v", dec)
	}
	var app RoutineAppliedData
	firstOf(t, evs, EvRoutineApplied, &app)
	if app.OK || app.FailedStep != 2 || app.Steps != 2 || app.Reason != routineInterrupted {
		t.Fatalf("applied = %+v", app)
	}
	if len(h.writes) != 0 || r.Output != "handled" {
		t.Fatalf("writes %v, output %q", h.writes, r.Output)
	}
	if err := a.Decide(ctx, id, ApprovalDecision{ApprovalID: "ap1", Decision: "allow"}); !errors.Is(err, ErrNoPendingApproval) {
		t.Fatalf("a late answer = %v; want ErrNoPendingApproval", err)
	}
	checkCursorPrefixes(t, evs)
}

// waitTerminal waits until the run ends. Wait returns at a pending
// approval too, which this test passes on its way.
func waitTerminal(t *testing.T, a *Agent, id string) *Run {
	t.Helper()
	ctx := testCtx(t)
	for {
		r, err := a.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if r.State.Terminal() {
			return r
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the run did not end: %+v", r)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// A hosted harness drives the runs in place of the turn loop, so a profile
// with routines is refused at load, for a named and for an inline profile.
func TestRoutinesRefusedOnAHostedHarness(t *testing.T) {
	hosted := WithRunner(runnerFunc(func(context.Context, *Hosted) (bool, error) { return false, nil }))
	_, err := New(WithoutDefaults(), WithAuthn(NoAuthn{}), WithModelName("anthropic/claude-opus-5-5"), WithModel(newScript().Model()),
		WithLedger(NewMemLedger()), WithBus(NewMemBus()), WithLogger(quietLogger()), withDeciders, hosted,
		WithProfile(AgentProfile{Name: "triage", Routines: []Routine{ackRoutine(RoutineShadow)}}))
	if err == nil || !strings.Contains(err.Error(), "agents.triage") || !strings.Contains(err.Error(), "a hosted harness drives the runs") {
		t.Fatalf("New = %v", err)
	}
	a := newTestAgent(t, newScript().Model(), withDeciders, hosted)
	_, err = a.Start(testCtx(t), "x", WithRunProfile(AgentProfile{Routines: []Routine{ackRoutine(RoutineShadow)}}))
	if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "routines need the turn loop") {
		t.Fatalf("start = %v", err)
	}
}

// A replay never runs a routine, also after a restart, when only its
// metadata says that it is a replay. A branch never compares the
// decisions that it kept from its source.
func TestRoutineSkipsReplaysAndBranches(t *testing.T) {
	routine := ackRoutine(RoutineShadow)
	resolved := &ResolvedProfile{Model: "anthropic/claude-opus-5-5", Routines: []Routine{routine}}
	t.Run("replay", func(t *testing.T) {
		l := NewMemLedger()
		id := "run-replay"
		seed(t, l, id,
			MustEvent(id, EvRunQueued, RunQueuedData{Agent: "triage", Input: "Ticket #42", Origin: OriginCLI, Metadata: map[string]string{"replayOf": "run-source"}, Resolved: resolved}),
			MustEvent(id, EvRunStarted, RunStartedData{Model: "anthropic/claude-opus-5-5"}),
		)
		d := &fakeDecider{answers: confident()}
		a := routineAgent(t, newScript(endTurn(`TRIAGE {"type": "type/bug"}`)).Model(), d, &helpdesk{}, []Routine{routine}, WithLedger(l))
		r := waitTerminal(t, a, id)
		mustState(t, r, nil, StateCompleted)
		if evs := ledgerEvents(t, l, id); d.calls != 0 || countType(evs, EvRoutineDecided) != 0 || countType(evs, EvRoutineCompared) != 0 {
			t.Fatalf("a replay ran the routine: %v", eventTypes(evs))
		}
	})
	t.Run("branch", func(t *testing.T) {
		l := NewMemLedger()
		id := "run-branch"
		seed(t, l, id,
			MustEvent(id, EvRunQueued, RunQueuedData{Agent: "triage", Input: "Ticket #42", Origin: OriginCLI, Kept: 3, Resolved: resolved}),
			MustEvent(id, EvRunStarted, RunStartedData{Model: "anthropic/claude-opus-5-5"}),
			MustEvent(id, EvRoutineDecided, RoutineDecidedData{Routine: "ack", Version: 2, Standing: RoutineShadow, Mode: RoutineShadow, Applies: true,
				Answers: map[string]Answer{"type": {Type: QuestionChoice, Choice: "bug", Probability: 0.93}, "beyond": {Type: QuestionNoul, Probability: 0.01}}}),
		)
		d := &fakeDecider{answers: confident()}
		a := routineAgent(t, newScript(endTurn(`TRIAGE {"type": "type/bug"}`)).Model(), d, &helpdesk{}, []Routine{routine}, WithLedger(l))
		r := waitTerminal(t, a, id)
		mustState(t, r, nil, StateCompleted)
		if evs := ledgerEvents(t, l, id); d.calls != 0 || countType(evs, EvRoutineCompared) != 0 {
			t.Fatalf("a branch compared the decision of its source: %v", eventTypes(evs))
		}
	})
}

// A message that arrives while a live routine works goes to the model,
// which starts with the block, also after a routine that completed.
func TestRoutineFollowUpDuringTheRoutine(t *testing.T) {
	ctx := testCtx(t)
	s := newScript(endTurn("answered the follow-up"))
	h := &helpdesk{}
	started, release := make(chan struct{}), make(chan struct{})
	slow := NewTool(ToolSpec{Name: "gitea.label_read", ReadOnly: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		close(started)
		<-release
		return TextResult(`[{"id":11,"name":"type/bug"},{"id":22,"name":"status/triaged"}]`), nil
	})
	tools := append([]Tool{slow}, h.tools()[1:]...)
	a := newTestAgent(t, s.Model(), withDeciders, WithDecider("clef", &fakeDecider{answers: confident()}), WithTools(tools...),
		WithProfile(AgentProfile{Name: "triage", Routines: []Routine{ackRoutine(RoutineLive)}}))
	r, err := a.Start(ctx, "Ticket #42: the printer is broken", AsAgent("triage"))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := a.Send(ctx, r.ID, Message{Role: RoleUser, Content: []Block{Text("Is it fixed?")}}, OriginCLI); err != nil {
		t.Fatal(err)
	}
	close(release)
	r = waitTerminal(t, a, r.ID)
	mustState(t, r, nil, StateCompleted)
	if r.Output != "answered the follow-up" || len(s.requests()) != 1 {
		t.Fatalf("output %q, model calls %d", r.Output, len(s.requests()))
	}
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	var app RoutineAppliedData
	firstOf(t, evs, EvRoutineApplied, &app)
	if !app.OK || countType(evs, EvRunFinished) != 1 {
		t.Fatalf("applied = %+v, events %v", app, eventTypes(evs))
	}
	if first := firstMessage(s.requests()[0]); !strings.Contains(first, "a message came after it") || !strings.Contains(first, "Its answer: TRIAGE") {
		t.Fatalf("first message =\n%s", first)
	}
	checkCursorPrefixes(t, evs)
}

// A restart between fw.routine.applied and fw.run.finished finishes the
// run with the answer of the routine, and no model call.
func TestRoutineRestartAfterApplied(t *testing.T) {
	l := NewMemLedger()
	id := "run-routine-applied"
	routine := ackRoutine(RoutineLive)
	seed(t, l, id,
		MustEvent(id, EvRunQueued, RunQueuedData{Agent: "triage", Input: "Ticket #42", Origin: OriginCLI,
			Resolved: &ResolvedProfile{Model: "anthropic/claude-opus-5-5", Routines: []Routine{routine}}}),
		MustEvent(id, EvRunStarted, RunStartedData{Model: "anthropic/claude-opus-5-5"}),
		MustEvent(id, EvRoutineDecided, RoutineDecidedData{Routine: "ack", Version: 2, Standing: RoutineLive, Mode: RoutineLive, Applies: true}),
		MustEvent(id, EvRoutineApplied, RoutineAppliedData{Routine: "ack", Version: 2, OK: true, Steps: 4, Answer: "TRIAGE done"}),
	)
	s := newScript()
	a := routineAgent(t, s.Model(), &fakeDecider{answers: confident()}, &helpdesk{}, []Routine{routine}, WithLedger(l))
	r := waitTerminal(t, a, id)
	mustState(t, r, nil, StateCompleted)
	if r.Output != "TRIAGE done" || len(s.requests()) != 0 {
		t.Fatalf("output %q, model calls %d", r.Output, len(s.requests()))
	}
}

// The calls of the runner count against budget.maxToolCalls: a routine
// that reaches the limit stops at the step, and the run stops at its
// limit as any run does.
func TestRoutineReachesMaxToolCalls(t *testing.T) {
	ctx := testCtx(t)
	s := newScript(endTurn("handled"))
	h := &helpdesk{}
	a := routineAgent(t, s.Model(), &fakeDecider{answers: confident()}, h, []Routine{ackRoutine(RoutineLive)})
	r, err := a.Run(ctx, "Ticket #42: the printer is broken", AsAgent("triage"), WithRunBudget(Budget{MaxToolCalls: 2}))
	mustState(t, r, err, StateFailed)
	if !strings.Contains(r.Error, "maxToolCalls") || r.Usage.ToolCalls != 2 {
		t.Fatalf("run = %+v", r)
	}
	var app RoutineAppliedData
	firstOf(t, ledgerEvents(t, a.Ledger(), r.ID), EvRoutineApplied, &app)
	if app.OK || app.FailedStep != 2 || !strings.Contains(app.Reason, "budget.maxToolCalls") || len(h.writes) != 0 {
		t.Fatalf("applied = %+v, writes %v", app, h.writes)
	}
}

// A step or a check whose command fails with a non-zero exit code fails
// the routine, although the tool reports the code as a normal result, and
// the model takes the run over.
func TestRoutineFailedCommand(t *testing.T) {
	cmd := func(name string, code int) Tool {
		return NewTool(ToolSpec{Name: name, ReadOnly: true, Idempotent: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
			return ToolResult{Content: []Block{Text(fmt.Sprintf("exit code %d", code))}, ExitCode: &code}, nil
		})
	}
	zero := 0.0
	for _, c := range []struct {
		name   string
		r      Routine
		failed int
		reason string
	}{
		{name: "step", failed: 1, reason: "step 1 build: exit code 2: exit code 2",
			r: Routine{Name: "ship", Version: 1, Standing: RoutineLive, Canary: &zero,
				Steps: []RoutineStep{{Tool: "build", Input: "{}"}}, After: []RoutineCheck{{Tool: "ok", Input: "{}"}}, Answer: "shipped"}},
		{name: "check", failed: 2, reason: "check 1 verify: exit code 1: exit code 1",
			r: Routine{Name: "ship", Version: 1, Standing: RoutineLive, Canary: &zero,
				Steps: []RoutineStep{{Tool: "ok", Input: "{}"}}, After: []RoutineCheck{{Tool: "verify", Input: "{}"}}, Answer: "shipped"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := testCtx(t)
			s := newScript(endTurn("the model shipped it"))
			a := newTestAgent(t, s.Model(), WithTools(cmd("build", 2), cmd("verify", 1), cmd("ok", 0)),
				WithProfile(AgentProfile{Name: "release", Routines: []Routine{c.r}}))
			r, err := a.Run(ctx, "Ship it.", AsAgent("release"))
			mustState(t, r, err, StateCompleted)
			if r.Output != "the model shipped it" || len(s.requests()) != 1 {
				t.Fatalf("output %q, model calls %d", r.Output, len(s.requests()))
			}
			apps := decodeAll[RoutineAppliedData](t, ledgerEvents(t, a.Ledger(), r.ID), EvRoutineApplied)
			if len(apps) != 1 || apps[0].OK || apps[0].FailedStep != c.failed || apps[0].Reason != c.reason {
				t.Fatalf("applied = %+v", apps)
			}
			// The model reads the failed call as an error.
			if first := firstMessage(s.requests()[0]); !strings.Contains(first, "error: exit code") {
				t.Errorf("first message =\n%s", first)
			}
		})
	}
}

// stock is a fake stock file and a fake report for the tests of the
// comparison of the calls: read shows the file with line numbers, as the
// files tool does, and write keeps the text of the report.
type stock struct {
	mu     sync.Mutex
	text   string
	report string
}

func (st *stock) tools() []Tool {
	read := NewTool(ToolSpec{Name: "stock.read", ReadOnly: true, Idempotent: true}, func(_ context.Context, in json.RawMessage) (ToolResult, error) {
		st.mu.Lock()
		defer st.mu.Unlock()
		var v struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(in, &v)
		if v.Path == "report.txt" {
			return TextResult("     1\t" + st.report + "\n"), nil
		}
		return TextResult("Here is the file with line numbers:\n     1\t" + st.text + "\n"), nil
	})
	write := NewTool(ToolSpec{Name: "stock.write"}, func(_ context.Context, in json.RawMessage) (ToolResult, error) {
		st.mu.Lock()
		defer st.mu.Unlock()
		var v struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(in, &v)
		st.report = v.Text
		return TextResult("written"), nil
	})
	return []Tool{read, write}
}

// stockRoutine copies the first line of the stock file into the report,
// with no question.
func stockRoutine(standing string) Routine {
	zero := 0.0
	return Routine{
		Name: "stock-ack", Version: 1, Standing: standing, Canary: &zero,
		Steps: []RoutineStep{
			{Tool: "stock.read", Input: `{"path": "levels.txt"}`, As: "read"},
			{Tool: "stock.write", Input: `{"path": "report.txt", "text": {{ json (find .read "\t(.+?)\n") }}}`},
		},
		Answer: `ACK {{ find .read "\t(.+?)\n" }}`,
	}
}

// A rehearsing routine without questions gains agreement: at the end of
// the run, its steps, rendered on the run with the results of the calls of
// the agent, are compared with those calls. The calls of the checks of
// done are no calls of the agent.
func TestRoutineRehearsalComparesTheCalls(t *testing.T) {
	cases := []struct {
		name   string
		text   string // the text that the agent writes
		file   string // the first line of the stock file
		agree  bool
		none   bool // no comparison of the calls: the routine hands the run over
		step   int
		reason string
	}{
		{name: "agrees", text: "STOCK pens=90", file: "STOCK pens=90", agree: true},
		{name: "differs", text: "STOCK pens=120", file: "STOCK pens=90", step: 2,
			reason: "step 2 stock.write: the key text differs"},
		{name: "handed", text: "STOCK pens=90", file: "", none: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := testCtx(t)
			st := &stock{text: c.file}
			s := newScript(
				callTools(use("t1", "stock.read", `{"path": "levels.txt"}`)),
				callTools(use("t2", "stock.write", fmt.Sprintf(`{"path": "report.txt", "text": %q}`, c.text))),
				endTurn("ACK "+c.text),
			)
			prof := AgentProfile{Name: "clerk", Routines: []Routine{stockRoutine(RoutineShadow)},
				Done: &Done{Checks: []DoneCheck{{Name: "the report is saved", Tool: "stock.read", Input: `{"path": "report.txt"}`, Contains: []string{"STOCK"}}}}}
			a := newTestAgent(t, s.Model(), WithTools(st.tools()...), WithProfile(prof))
			r, err := a.Run(ctx, "Acknowledge the stock file.", AsAgent("clerk"))
			mustState(t, r, err, StateCompleted)
			evs := ledgerEvents(t, a.Ledger(), r.ID)
			var dec RoutineDecidedData
			firstOf(t, evs, EvRoutineDecided, &dec)
			if dec.Mode != RoutineShadow || !dec.Applies || len(dec.Answers) != 0 {
				t.Fatalf("decided = %+v", dec)
			}
			cmp := decodeAll[RoutineComparedData](t, evs, EvRoutineCompared)
			if c.none {
				if len(cmp) != 0 {
					t.Fatalf("compared = %+v", cmp)
				}
				return
			}
			if len(cmp) != 1 {
				t.Fatalf("compared = %+v", cmp)
			}
			got := cmp[0]
			if !got.Calls || got.Question != "" || got.Mode != RoutineShadow || got.Routine != "stock-ack" || got.Version != 1 ||
				got.Agree != c.agree || got.Step != c.step || got.Reason != c.reason {
				t.Fatalf("compared = %+v", got)
			}
			if !c.agree && (got.Said != `{"path":"report.txt","text":"STOCK pens=90"}` || got.Observed != `{"path":"report.txt","text":"STOCK pens=120"}`) {
				t.Errorf("said %s, observed %s", got.Said, got.Observed)
			}
			// The comparison comes before the end, after the checks of done.
			types := eventTypes(evs)
			if types[len(types)-1] != EvRunFinished || types[len(types)-2] != EvRoutineCompared || countType(evs, EvDoneChecked) != 1 {
				t.Fatalf("events = %v", types)
			}
			checkCursorPrefixes(t, evs)
		})
	}
}

// ModelCalls leaves out the calls that the kernel made itself: of the
// routine runner, of a check of done, and of a check run.
func TestModelCallsLeavesOutTheCallsOfTheKernel(t *testing.T) {
	id := "run-calls"
	req := func(callID, tool, routine string) Event {
		return MustEvent(id, EvToolRequested, ToolRequestedData{CallID: callID, Tool: tool, Input: json.RawMessage(`{}`), Routine: routine})
	}
	res := func(callID, text string) Event {
		return MustEvent(id, EvToolResponded, ToolRespondedData{CallID: callID, Content: []Block{Text(text)}})
	}
	evs := []Event{
		MustEvent(id, EvRunQueued, RunQueuedData{Agent: "clerk", Input: "go"}),
		req(routineCallPrefix+"1", "decide", "ack"), res(routineCallPrefix+"1", "{}"),
		req("toolu_1", "stock.read", ""), res("toolu_1", "STOCK"),
		req(doneCallPrefix+"1", "stock.read", ""), res(doneCallPrefix+"1", "STOCK"),
		req(checkCallPrefix+"1", "stock.read", ""), res(checkCallPrefix+"1", "STOCK"),
	}
	for i := range evs {
		evs[i].Seq = uint64(i + 1)
	}
	calls := ModelCalls(evs)
	if len(calls) != 1 || calls[0].Tool != "stock.read" || calls[0].Result != "STOCK" {
		t.Fatalf("calls = %+v", calls)
	}
}

// A run that a live routine finishes runs the checks of done of the run,
// as a run of the model does. When they pass, the run ends as checked with
// no model call. When one fails, the routine did not complete: the model
// takes the run with the reason in its block, and its own answer gets
// every attempt.
func TestRoutineLiveRunsTheChecksOfDone(t *testing.T) {
	check := DoneCheck{Name: "the report holds the line of today", Tool: "stock.read", Input: `{"path": "report.txt"}`, Contains: []string{"pens=90"}}
	t.Run("passed", func(t *testing.T) {
		ctx := testCtx(t)
		s := newScript() // the model must not be called
		st := &stock{text: "STOCK pens=90"}
		prof := AgentProfile{Name: "clerk", Routines: []Routine{stockRoutine(RoutineLive)}, Done: &Done{Checks: []DoneCheck{check}, Attempts: 1}}
		a := newTestAgent(t, s.Model(), WithTools(st.tools()...), WithProfile(prof))
		r, err := a.Run(ctx, "Acknowledge the stock file.", AsAgent("clerk"))
		mustState(t, r, err, StateCompleted)
		if r.Output != "ACK STOCK pens=90" || len(s.requests()) != 0 {
			t.Fatalf("output %q, model calls %d", r.Output, len(s.requests()))
		}
		if r.Done == nil || r.Done.State != DoneChecked || r.Stop != RunStopChecked || !CheckedDone(r.Done.State, r.Done.Weak) {
			t.Fatalf("run done = %+v, stop %q", r.Done, r.Stop)
		}
		evs := ledgerEvents(t, a.Ledger(), r.ID)
		types := eventTypes(evs)
		n := len(types)
		if types[n-5] != EvToolRequested || types[n-3] != EvDoneChecked || types[n-2] != EvRoutineApplied || types[n-1] != EvRunFinished {
			t.Fatalf("events = %v", types)
		}
		passes := donePassesOf(t, evs)
		if len(passes) != 1 || !passes[0].Passed || passes[0].Answer != 0 || passes[0].Continue {
			t.Fatalf("passes = %+v", passes)
		}
		if d := finishedOf(t, evs); d.Done != DoneChecked || d.Stop != RunStopChecked {
			t.Fatalf("finished = %+v", d)
		}
		checkCursorPrefixes(t, evs)
	})
	t.Run("failed", func(t *testing.T) {
		ctx := testCtx(t)
		// The routine writes an old line as a constant. The check of done
		// catches it, and the model writes the right line.
		zero := 0.0
		stale := Routine{Name: "stock-ack", Version: 1, Standing: RoutineLive, Canary: &zero,
			Steps:  []RoutineStep{{Tool: "stock.write", Input: `{"path": "report.txt", "text": "STOCK pens=120"}`}},
			Answer: "ACK STOCK pens=120"}
		s := newScript(
			callTools(use("t1", "stock.write", `{"path": "report.txt", "text": "STOCK pens=90"}`)),
			endTurn("ACK STOCK pens=90"),
		)
		st := &stock{text: "STOCK pens=90"}
		prof := AgentProfile{Name: "clerk", Routines: []Routine{stale}, Done: &Done{Checks: []DoneCheck{check}, Attempts: 1}}
		a := newTestAgent(t, s.Model(), WithTools(st.tools()...), WithProfile(prof))
		r, err := a.Run(ctx, "Acknowledge the stock file.", AsAgent("clerk"))
		mustState(t, r, err, StateCompleted)
		if r.Output != "ACK STOCK pens=90" || len(s.requests()) != 2 || st.report != "STOCK pens=90" {
			t.Fatalf("output %q, model calls %d, report %q", r.Output, len(s.requests()), st.report)
		}
		evs := ledgerEvents(t, a.Ledger(), r.ID)
		var app RoutineAppliedData
		firstOf(t, evs, EvRoutineApplied, &app)
		want := `done: check 1 "the report holds the line of today" (stock.read) failed: the result lacks "pens=90"`
		if app.OK || app.Answer != "" || app.FailedStep != 2 || app.Reason != want {
			t.Fatalf("applied = %+v", app)
		}
		passes := donePassesOf(t, evs)
		if len(passes) != 2 || passes[0].Passed || passes[0].Answer != 0 || !passes[0].Continue ||
			!passes[1].Passed || passes[1].Attempt != 1 || passes[1].Answer == 0 {
			t.Fatalf("passes = %+v", passes)
		}
		if first := firstMessage(s.requests()[0]); !strings.Contains(first, `<context source="routine">`) || !strings.Contains(first, "It stopped: "+want) {
			t.Fatalf("first message =\n%s", first)
		}
		if d := finishedOf(t, evs); d.Done != DoneChecked || d.Stop != RunStopChecked {
			t.Fatalf("finished = %+v", d)
		}
		checkCursorPrefixes(t, evs)
	})
}
