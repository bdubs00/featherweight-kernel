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
			m["type"] = Answer{Choice: "bug", Probabilities: map[string]float64{"bug": 0.61}}
			return m
		}, reason: "type below act: 0.61"},
		{name: "not expected", answers: func() map[string]Answer {
			m := confident()
			m["type"] = Answer{Choice: "other", Probability: 0.97}
			return m
		}, reason: "type other not expected"},
		{name: "beyond", answers: func() map[string]Answer {
			m := confident()
			m["beyond"] = Answer{Probability: 0.12}
			return m
		}, reason: "beyond expected no: 0.12"},
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
		if CanaryPick(id, 0.5) {
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
			if cmp := decodeAll[RoutineComparedData](t, evs, EvRoutineCompared); len(cmp) != 2 || cmp[0].Mode != RoutineCanary {
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
	if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), `decider "jev" is not a key of deciders`) {
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
