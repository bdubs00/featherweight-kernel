package fw

import (
	"errors"
	"strings"
	"testing"
)

// queuedOf returns the data of the fw.run.queued of a run.
func queuedOf(t *testing.T, l Ledger, run string) RunQueuedData {
	t.Helper()
	var q RunQueuedData
	firstOf(t, ledgerEvents(t, l, run), EvRunQueued, &q)
	return q
}

// A profile sets the compaction of its runs: its size starts a compaction,
// its model writes the summary, and the run records the setting in its
// resolved profile, with the model as a selector.
func TestProfileCompactionIsResolved(t *testing.T) {
	ctx := testCtx(t)
	rec := &pinRecorder{}
	cfg := pinConfig(t, "pintest/base")
	cfg.Models = map[string]string{"cheap": "pintest/cheap"}
	cfg.Agents = map[string]AgentProfile{"worker": {Compaction: &CompactionConfig{At: 1, Model: "cheap"}}}
	a := pinAgent(t, cfg, NewMemLedger(), rec)
	rec.script(callTools(use("c1", "alpha", `{}`)), endTurn("<summary>SUMMARY</summary>"), endTurn("done"))
	r, err := a.Run(ctx, "work", AsAgent("worker"))
	mustState(t, r, err, StateCompleted)
	q := queuedOf(t, a.Ledger(), r.ID)
	if c := q.Resolved.Compaction; c == nil || *c != (CompactionConfig{At: 1, Model: "pintest/cheap"}) {
		t.Fatalf("resolved compaction = %+v", c)
	}
	if n := countType(ledgerEvents(t, a.Ledger(), r.ID), EvContextCompacted); n != 1 {
		t.Fatalf("compactions = %d", n)
	}
	if rec.call(0).model != "base" || rec.call(1).model != "cheap" || rec.call(2).model != "base" {
		t.Fatalf("models = %s, %s, %s", rec.call(0).model, rec.call(1).model, rec.call(2).model)
	}

	// A run of a profile without the block takes the block of the
	// configuration, which is large, and compacts nothing.
	rec.script(callTools(use("c2", "alpha", `{}`)), endTurn("done"))
	r2, err := a.Run(ctx, "work")
	mustState(t, r2, err, StateCompleted)
	if c := queuedOf(t, a.Ledger(), r2.ID).Resolved.Compaction; c == nil || *c != (CompactionConfig{At: DefaultCompactAt}) {
		t.Fatalf("resolved compaction of the default profile = %+v", c)
	}
	if n := countType(ledgerEvents(t, a.Ledger(), r2.ID), EvContextCompacted); n != 0 {
		t.Fatalf("compactions of the default profile = %d", n)
	}
}

// off turns compaction off for the runs of a profile, also when the
// configuration compacts at once, and the inline profile of a request
// overlays the block as it overlays a budget.
func TestProfileCompactionOffAndOverlay(t *testing.T) {
	ctx := testCtx(t)
	s := &stepsModel{steps: []any{
		callTools(use("c1", "echo", `{}`)), endTurn("one"),
		callTools(use("c2", "echo", `{}`)), endTurn("two"),
	}}
	a := newTestAgent(t, s.Model(), WithTools(echoTool(nil)), WithProfile(AgentProfile{Name: "quiet", Compaction: &CompactionConfig{Off: true}}))
	a.k.compactAt = 1
	r, err := a.Run(ctx, "work", AsAgent("quiet"))
	mustState(t, r, err, StateCompleted)
	if c := queuedOf(t, a.Ledger(), r.ID).Resolved.Compaction; c == nil || !c.Off || c.At != 0 {
		t.Fatalf("resolved compaction = %+v", c)
	}
	if len(s.summaries) != 0 {
		t.Fatalf("a profile with off compacted %d times", len(s.summaries))
	}
	// The overlay of the request turns it on again with its own size.
	r, err = a.Run(ctx, "work", AsAgent("quiet"), WithRunProfile(AgentProfile{Compaction: &CompactionConfig{At: 1}}))
	mustState(t, r, err, StateCompleted)
	if c := queuedOf(t, a.Ledger(), r.ID).Resolved.Compaction; c == nil || c.Off || c.At != 1 {
		t.Fatalf("resolved compaction of the overlay = %+v", c)
	}
	if len(s.summaries) != 1 {
		t.Fatalf("summaries after the overlay = %d", len(s.summaries))
	}
}

// A run keeps the compaction setting of its queue time: a change of the
// profile reaches new runs only.
func TestRunKeepsItsCompaction(t *testing.T) {
	ctx := testCtx(t)
	s := &stepsModel{steps: []any{
		callTools(use("c1", "echo", `{}`)), endTurn("one"),
		callTools(use("c2", "echo", `{}`)), endTurn("two"),
	}}
	a := newTestAgent(t, s.Model(), WithTools(echoTool(nil)))
	setProfiles(t, a, map[string]AgentProfile{"worker": {Compaction: &CompactionConfig{At: 1}}})
	r, err := a.Run(ctx, "work", AsAgent("worker"))
	mustState(t, r, err, StateCompleted)
	if len(s.summaries) != 1 {
		t.Fatalf("summaries = %d", len(s.summaries))
	}
	setProfiles(t, a, map[string]AgentProfile{"worker": {Compaction: &CompactionConfig{Off: true}}})
	if err := a.Send(ctx, r.ID, UserText("again"), OriginCLI); err != nil {
		t.Fatal(err)
	}
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)
	// The follow-up compacts again under the size of the queue time.
	if len(s.summaries) < 2 {
		t.Fatalf("summaries after the change of the profile = %d, want more than 1", len(s.summaries))
	}
}

// CheckProfile and the inline profile of a request refuse a bad block and
// name the key.
func TestCheckCompaction(t *testing.T) {
	a := newTestAgent(t, newScript().Model())
	for _, c := range []struct {
		block CompactionConfig
		key   string
	}{
		{CompactionConfig{At: -1}, "compaction.at"},
		{CompactionConfig{At: 100, Off: true}, "compaction.off"},
		{CompactionConfig{Model: "nosuch"}, "compaction.model"},
	} {
		err := a.CheckProfile(AgentProfile{Name: "x", Compaction: &c.block})
		if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), c.key) {
			t.Errorf("CheckProfile(%+v) = %v, want ErrInvalidRequest about %s", c.block, err, c.key)
		}
		if _, err := a.Start(testCtx(t), "work", WithRunProfile(AgentProfile{Compaction: &c.block})); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("Start with %+v = %v", c.block, err)
		}
	}
	for _, ok := range []CompactionConfig{{}, {At: 1000}, {Off: true}, {Model: "fake/small"}} {
		if err := a.CheckProfile(AgentProfile{Name: "x", Compaction: &ok}); err != nil {
			t.Errorf("CheckProfile(%+v) = %v", ok, err)
		}
	}
}
