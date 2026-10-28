package fw

import (
	"errors"
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
