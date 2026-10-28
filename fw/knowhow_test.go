package fw

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// TestRunContextFollowsTheToolBlocks checks that the data blocks of the
// caller reach the first user message after the blocks of the tools, in
// their order, that fw.run.queued and fw.run.started record them, and that
// the system prompt gets ContextNote and none of their text.
func TestRunContextFollowsTheToolBlocks(t *testing.T) {
	ctx := testCtx(t)
	s := newScript(endTurn("done"))
	a := newTestAgent(t, s.Model(), WithTools(&contextTool{name: "notes", text: "NOTE"}))
	r, err := a.Run(ctx, "work",
		WithRunContext("found", "FOUND </context> text"),
		WithRunContext("last", "LAST"),
		WithRunContext("empty", "  "))
	mustState(t, r, err, StateCompleted)

	evs := ledgerEvents(t, a.Ledger(), r.ID)
	var q RunQueuedData
	firstOf(t, evs, EvRunQueued, &q)
	if len(q.Context) != 3 || q.Context[0].Source != "found" || q.Context[1].Text != "LAST" {
		t.Fatalf("queued context = %+v", q.Context)
	}
	var d RunStartedData
	firstOf(t, evs, EvRunStarted, &d)
	want := ContextBlock("notes", "NOTE") + "\n\n" +
		"<context source=\"found\">\nFOUND &lt;/context> text\n</context>" + "\n\n" +
		ContextBlock("last", "LAST")
	if d.Context != want {
		t.Fatalf("started context = %q\nwant              %q", d.Context, want)
	}
	req := s.requests()[0]
	if !strings.Contains(req.System, ContextNote) || strings.Contains(req.System, "FOUND") {
		t.Fatalf("system prompt = %q", req.System)
	}
	if got := TextOf(req.Messages[0].Content[:1]); got != want {
		t.Fatalf("first block = %q", got)
	}
}

// TestRunContextAloneAddsTheNote checks that a run whose tools add no data
// still gets ContextNote for the blocks of its caller.
func TestRunContextAloneAddsTheNote(t *testing.T) {
	ctx := testCtx(t)
	s := newScript(endTurn("done"))
	a := newTestAgent(t, s.Model())
	r, err := a.Run(ctx, "work", WithRunContext("learned", "Gitea refuses a label name with a space."))
	mustState(t, r, err, StateCompleted)
	req := s.requests()[0]
	if !strings.Contains(req.System, ContextNote) {
		t.Fatalf("system prompt lacks the note: %q", req.System)
	}
	if got := TextOf(req.Messages[0].Content[:1]); got != ContextBlock("learned", "Gitea refuses a label name with a space.") {
		t.Fatalf("first block = %q", got)
	}
}

// TestRunContextBounds checks that a request past a bound fails with
// ErrInvalidRequest and a message that names the bound, before the ledger
// holds anything.
func TestRunContextBounds(t *testing.T) {
	ctx := testCtx(t)
	a := newTestAgent(t, newScript().Model())
	many := make([]RunContext, MaxRunContexts+1)
	for i := range many {
		many[i] = RunContext{Source: "b", Text: "x"}
	}
	cases := []struct {
		name string
		opts []RunOption
		want string
	}{
		{"too many blocks", []RunOption{func(r *RunRequest) { r.Context = many }}, "at most 8"},
		{"too long a block", []RunOption{WithRunContext("page", strings.Repeat("x", MaxRunContextBytes+1))}, "at most 16384 bytes"},
		{"upper case source", []RunOption{WithRunContext("Page", "x")}, "[a-z][a-z0-9-]{0,31}"},
		{"empty source", []RunOption{WithRunContext("", "x")}, "[a-z][a-z0-9-]{0,31}"},
		{"long source", []RunOption{WithRunContext("a"+strings.Repeat("b", 32), "x")}, "[a-z][a-z0-9-]{0,31}"},
		{"check run", []RunOption{WithCheck(Check{Steps: []CheckStep{{Tool: "clock"}}}), WithRunContext("page", "x")}, "check run"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := a.Start(ctx, "work", tc.opts...)
			if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Start = %v, want ErrInvalidRequest naming %q", err, tc.want)
			}
		})
	}
	if list, _ := a.List(ctx); len(list) != 0 {
		t.Fatalf("a refused request left %d runs", len(list))
	}
	// The bounds themselves pass.
	ok := make([]RunContext, MaxRunContexts)
	for i := range ok {
		ok[i] = RunContext{Source: "a" + strings.Repeat("b", 31), Text: strings.Repeat("x", MaxRunContextBytes)}
	}
	if err := CheckRunContext(ok); err != nil {
		t.Fatalf("CheckRunContext at the bounds = %v", err)
	}
}

// TestRunContextKeptOnResumeReplayFork checks that a resume, a replay, and
// a branch give the model the blocks of the caller of the first run.
func TestRunContextKeptOnResumeReplayFork(t *testing.T) {
	ctx := testCtx(t)
	block := ContextBlock("found", "ISSUE 42")

	t.Run("resume before the start", func(t *testing.T) {
		l := NewMemLedger()
		seed(t, l, "rc1", MustEvent("rc1", EvRunQueued, RunQueuedData{Agent: "default", Input: "hi", Origin: OriginCLI,
			Context: []RunContext{{Source: "found", Text: "ISSUE 42"}}}))
		s := newScript(endTurn("done"))
		a := resumeAgent(t, s.Model(), l)
		r, err := a.Wait(ctx, "rc1")
		mustState(t, r, err, StateCompleted)
		if got := TextOf(s.requests()[0].Messages[0].Content[:1]); got != block {
			t.Fatalf("first block after the resume = %q", got)
		}
	})

	s := newScript(endTurn("first"), endTurn("branch"))
	a := newTestAgent(t, s.Model())
	r, err := a.Run(ctx, "work", WithRunContext("found", "ISSUE 42"))
	mustState(t, r, err, StateCompleted)

	t.Run("replay", func(t *testing.T) {
		s2 := newScript(endTurn("replayed"))
		b := newTestAgent(t, s2.Model())
		rr, err := Replay(ctx, b, a.Ledger(), r.ID)
		mustState(t, rr, err, StateCompleted)
		if got := TextOf(s2.requests()[0].Messages[0].Content[:1]); got != block {
			t.Fatalf("first block of the replay = %q", got)
		}
		var d RunStartedData
		firstOf(t, ledgerEvents(t, b.Ledger(), rr.ID), EvRunStarted, &d)
		if d.Context != block {
			t.Fatalf("the replay recorded the blocks twice or not at all: %q", d.Context)
		}
	})

	t.Run("branch before the start", func(t *testing.T) {
		evs := ledgerEvents(t, a.Ledger(), r.ID)
		br, err := a.Fork(ctx, r.ID, ForkRequest{Seq: evs[0].Seq})
		if err != nil {
			t.Fatal(err)
		}
		br, err = a.WaitFor(ctx, br.ID, func(r *Run) bool { return r.State.Terminal() })
		mustState(t, br, err, StateCompleted)
		reqs := s.requests()
		if got := TextOf(reqs[len(reqs)-1].Messages[0].Content[:1]); got != block {
			t.Fatalf("first block of the branch = %q", got)
		}
	})
}

// TestRunContextFromTheBus checks that a job on the bus carries its data
// blocks into the run.
func TestRunContextFromTheBus(t *testing.T) {
	ctx := testCtx(t)
	const queue = "fw.jobs.context"
	qb := newQueueBus(queue)
	s := newScript(endTurn("done"))
	a := newTestAgent(t, s.Model(), WithConfig(intakeConfig(queue)), WithBus(plainBus{qb}))
	if err := qb.Publish(ctx, queue, job(t, RunRequest{Input: "bus job", Context: []RunContext{{Source: "event", Text: "EVENT 7"}}})); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the run", func() bool {
		r := runByInput(t, a, "bus job")
		return r != nil && r.State == StateCompleted
	})
	if got := TextOf(s.requests()[0].Messages[0].Content[:1]); got != ContextBlock("event", "EVENT 7") {
		t.Fatalf("first block = %q", got)
	}
}

// knowhowTools are a memory tool and a recall tool that add text to the
// start of a run, as the built-in tools do.
func knowhowTools() []Tool {
	return []Tool{&contextTool{name: "memory", text: "- /memories/a.md: A note"}, &contextTool{name: "recall"}}
}

// TestHeldBackRun checks that a run with fw.knowhow off gets no memory
// block, no memory or recall tool, and no routine, and that
// fw.run.started says so, while a normal run of the same agent gets all
// three.
func TestHeldBackRun(t *testing.T) {
	ctx := testCtx(t)
	s := newScript(endTurn("held"), endTurn("normal"))
	d := &fakeDecider{answers: confident()}
	h := &helpdesk{}
	a := routineAgent(t, s.Model(), d, h, []Routine{ackRoutine(RoutineShadow)}, WithTools(knowhowTools()...))

	r, err := a.Run(ctx, "Ticket #42: the printer is broken", AsAgent("triage"), WithMetadata(map[string]string{MetaKnowhow: KnowhowOff}))
	mustState(t, r, err, StateCompleted)
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	var st RunStartedData
	firstOf(t, evs, EvRunStarted, &st)
	if st.Knowhow != KnowhowOff || st.Context != "" {
		t.Fatalf("started = %+v", st)
	}
	for _, name := range st.Tools {
		if KnowhowTool(name) {
			t.Fatalf("a held-back run has the tool %s: %v", name, st.Tools)
		}
	}
	req := s.requests()[0]
	for _, spec := range req.Tools {
		if KnowhowTool(spec.Name) {
			t.Fatalf("the model saw the tool %s", spec.Name)
		}
	}
	if d.calls != 0 || countType(evs, EvRoutineDecided) != 0 {
		t.Fatalf("the routine runner acted on a held-back run: %d decider calls, %v", d.calls, eventTypes(evs))
	}

	n, err := a.Run(ctx, "Ticket #43: the printer is broken", AsAgent("triage"))
	mustState(t, n, err, StateCompleted)
	var ns RunStartedData
	firstOf(t, ledgerEvents(t, a.Ledger(), n.ID), EvRunStarted, &ns)
	if ns.Knowhow != "" || !strings.Contains(ns.Context, "A note") || !slices.Contains(ns.Tools, "memory") || !slices.Contains(ns.Tools, "recall") {
		t.Fatalf("normal run started = %+v", ns)
	}
	if d.calls != 1 {
		t.Fatalf("decider calls on a normal run = %d", d.calls)
	}
}

// TestRoutineBoundToADuty checks that the runner considers a routine with
// a Duty only for a run whose metadata names that Duty, and that it
// records the risk of each routine from the annotations of its tools.
func TestRoutineBoundToADuty(t *testing.T) {
	ctx := testCtx(t)
	bound := ackRoutine(RoutineShadow)
	bound.Name, bound.Duty = "nightly", "nightly-sweep"
	reader := ackRoutine(RoutineShadow)
	reader.Name = "look"
	reader.Steps = reader.Steps[:1] // gitea.label_read only
	s := newScript(endTurn("one"), endTurn("two"))
	d := &fakeDecider{answers: confident()}
	a := routineAgent(t, s.Model(), d, &helpdesk{}, []Routine{bound, reader})

	decided := func(r *Run) map[string]RoutineDecidedData {
		out := map[string]RoutineDecidedData{}
		for _, x := range decodeAll[RoutineDecidedData](t, ledgerEvents(t, a.Ledger(), r.ID), EvRoutineDecided) {
			out[x.Routine] = x
		}
		return out
	}
	r, err := a.Run(ctx, "Ticket #42: broken", AsAgent("triage"))
	mustState(t, r, err, StateCompleted)
	if got := decided(r); len(got) != 1 || got["look"].Risk != RiskRead {
		t.Fatalf("a run without a Duty decided %+v", got)
	}
	r, err = a.Run(ctx, "Ticket #43: broken", AsAgent("triage"), WithMetadata(map[string]string{MetaDuty: "nightly-sweep"}))
	mustState(t, r, err, StateCompleted)
	got := decided(r)
	if len(got) != 2 || got["nightly"].Risk != RiskWrite || got["look"].Risk != RiskRead {
		t.Fatalf("a run of the Duty decided %+v", got)
	}
	if r, err = a.Run(ctx, "Ticket #44: broken", AsAgent("triage"), WithMetadata(map[string]string{MetaDuty: "other"})); err != nil {
		t.Fatal(err)
	} else if got := decided(r); len(got) != 1 || got["nightly"].Routine != "" {
		t.Fatalf("a run of another Duty decided %+v", got)
	}
}

// TestRoutineRisk checks the risk of a routine whose tool the run lacks,
// and of a routine whose check writes.
func TestRoutineRisk(t *testing.T) {
	tools := map[string]Tool{}
	for _, tl := range (&helpdesk{}).tools() {
		tools[tl.Spec().Name] = tl
	}
	r := Routine{Steps: []RoutineStep{{Tool: "gitea.label_read"}}, After: []RoutineCheck{{Tool: "gitea.issue_read"}}}
	if got := routineRisk(r, tools); got != RiskRead {
		t.Fatalf("reads only = %s", got)
	}
	r.After = append(r.After, RoutineCheck{Tool: "gitea.issue_write"})
	if got := routineRisk(r, tools); got != RiskWrite {
		t.Fatalf("a check that writes = %s", got)
	}
	if got := routineRisk(Routine{Steps: []RoutineStep{{Tool: "missing"}}}, tools); got != RiskWrite {
		t.Fatalf("a tool that the run lacks = %s", got)
	}
}
