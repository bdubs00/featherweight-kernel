package fw

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fw.run.queued records the resolved profile next to the inline profile of
// the request: the structure of the overlay with the values of the
// configuration filled in, the digests of the text, and no text, no
// description, and no access block of the named profile.
func TestQueuedEventRecordsTheResolvedProfile(t *testing.T) {
	ctx := testCtx(t)
	rec := &pinRecorder{}
	cfg := pinConfig(t, "pintest/base")
	cfg.Effort = "medium"
	cfg.Time.Zone = "Europe/Berlin"
	cfg.Budget = Budget{MaxTurns: 40, MaxTokens: 1000}
	cfg.Models = map[string]string{"fast": "pintest/quick"}
	a := pinAgent(t, cfg, NewMemLedger(), rec)
	setProfiles(t, a, map[string]AgentProfile{"worker": {
		Description:  "DESCRIPTION-OF-WORKER",
		Instructions: []string{"NAMED-INSTRUCTION"},
		Model:        "fast",
		Tools:        []string{"alpha"},
		Policy:       &Rules{Ask: []string{"beta"}},
		Budget:       &Budget{MaxTurns: 9},
		Access:       &Access{Owners: []string{"alice@example.com"}},
	}})
	inline := AgentProfile{Instructions: []string{"inline"}, Tools: []string{"alpha", "beta"}}
	rec.script(endTurn("ok"))
	r, err := a.Run(ctx, "work", AsAgent("worker"), WithRunProfile(inline))
	mustState(t, r, err, StateCompleted)

	var q RunQueuedData
	e := firstOf(t, ledgerEvents(t, a.Ledger(), r.ID), EvRunQueued, &q)
	if q.Profile == nil || !reflect.DeepEqual(*q.Profile, inline) {
		t.Fatalf("inline profile = %+v, want %+v", q.Profile, inline)
	}
	if !reflect.DeepEqual(q.ExtraTools, []string{"beta"}) {
		t.Fatalf("extra tools = %v", q.ExtraTools)
	}
	want := ResolvedProfile{
		Model:              "pintest/quick",
		Effort:             "medium",
		Tools:              []string{"alpha", "beta"},
		TimeZone:           "Europe/Berlin",
		Policy:             &Rules{Ask: []string{"beta"}},
		Budget:             &Budget{MaxTurns: 9, MaxTokens: 1000},
		SystemDigest:       textDigest(DefaultSystemPrompt),
		InstructionsDigest: textDigest("NAMED-INSTRUCTION\n\ninline"),
	}
	if q.Resolved == nil || !reflect.DeepEqual(*q.Resolved, want) {
		t.Fatalf("resolved profile = %+v\nwant %+v", q.Resolved, want)
	}
	for _, text := range []string{"alice", `"access"`, "DESCRIPTION-OF-WORKER", "NAMED-INSTRUCTION", "featherweight agent"} {
		if strings.Contains(string(e.Data), text) {
			t.Fatalf("fw.run.queued holds %q: %s", text, e.Data)
		}
	}
	c := rec.call(0)
	if c.model != "quick" || c.effort != "medium" || c.tools != "alpha,beta" ||
		!strings.HasPrefix(c.system, DefaultSystemPrompt+"\n\n# Instructions\n\nNAMED-INSTRUCTION\n\ninline") {
		t.Fatalf("request = %+v", c)
	}
	// A branch that names a model replaces the model of the resolved
	// profile and keeps the rest.
	rec.script(endTurn("branch"))
	b, err := a.Fork(ctx, r.ID, ForkRequest{Input: "again", Model: "pintest/branch"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Wait(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	var bq RunQueuedData
	firstOf(t, ledgerEvents(t, a.Ledger(), b.ID), EvRunQueued, &bq)
	branch := want
	branch.Model = "pintest/branch"
	if bq.Resolved == nil || !reflect.DeepEqual(*bq.Resolved, branch) || q.Resolved.Model != "pintest/quick" {
		t.Fatalf("resolved profile of the branch = %+v", bq.Resolved)
	}
}

// tighten adds the rules of the named profile now only when they hold a
// clause that the resolved rules do not hold, and the result is the
// stricter of both.
func TestTightenAddsOnlyNewRules(t *testing.T) {
	if tighten(nil, nil) != nil {
		t.Fatal("tighten(nil, nil) is not nil")
	}
	own := &Rules{Deny: []string{"alpha"}}
	if tighten(own, nil) != own || tighten(own, &Rules{Deny: []string{"alpha"}}) != own {
		t.Fatal("an unchanged profile changed the rules")
	}
	merged := MergeRules(*own, Rules{Ask: []string{"gamma"}})
	if tighten(&merged, &Rules{Deny: []string{"alpha"}}) != &merged {
		t.Fatal("rules that the overlay merged were added again")
	}
	if got := tighten(nil, own); got == own || !reflect.DeepEqual(*got, *own) {
		t.Fatalf("tighten(nil, own) = %+v", got)
	}
	got := tighten(&merged, &Rules{Default: "allow", Deny: []string{"beta"}})
	p := NewRulesPolicy(*got)
	for tool, want := range map[string]Verdict{"alpha": VerdictDeny, "beta": VerdictDeny, "gamma": VerdictAsk, "delta": VerdictAllow} {
		if d := p.Decide(context.Background(), ToolCall{Spec: ToolSpec{Name: tool}}); d.Verdict != want {
			t.Errorf("%s = %s, want %s", tool, d.Verdict, want)
		}
	}
}

// A child in another process gets the tools of its parent as the parent was
// queued, and the rules that the profile of the parent has now.
func TestRemoteChildGetsTheResolvedProfileOfItsParent(t *testing.T) {
	ctx := testCtx(t)
	other := pinAgent(t, pinConfig(t, "pintest/base"), NewMemLedger(), &pinRecorder{resps: []*Response{endTurn("child")}})
	sp := &otherSpawner{other: other}
	rec := &pinRecorder{}
	a := pinAgent(t, pinConfig(t, "pintest/base"), NewMemLedger(), rec, WithSpawner(sp))
	setProfiles(t, a, map[string]AgentProfile{"lead": {Tools: []string{"alpha", "beta"}}})
	rec.script(endTurn("led"))
	p, err := a.Run(ctx, "lead", AsAgent("lead"))
	mustState(t, p, err, StateCompleted)
	setProfiles(t, a, map[string]AgentProfile{"lead": {Tools: []string{"alpha", "gamma"}, Policy: &Rules{Deny: []string{"alpha"}}}})

	c, err := a.SpawnChild(ctx, p.ID, NewRunRequest("help"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Wait(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	reqs := sp.requests()
	if len(reqs) != 1 || reqs[0].Profile == nil || reqs[0].Profile.Policy == nil {
		t.Fatalf("requests = %+v", reqs)
	}
	if got := reqs[0].Profile.Tools; !reflect.DeepEqual(got, []string{"alpha", "beta"}) {
		t.Fatalf("tools of the child = %v", got)
	}
	policy := NewRulesPolicy(*reqs[0].Profile.Policy)
	for tool, want := range map[string]Verdict{"alpha": VerdictDeny, "beta": VerdictAllow} {
		if d := policy.Decide(ctx, ToolCall{Spec: ToolSpec{Name: tool, ReadOnly: true}}); d.Verdict != want {
			t.Errorf("%s = %s, want %s", tool, d.Verdict, want)
		}
	}
}

// A branch in another process takes the model of the request in its
// resolved profile, and the text of the profile there. Its text differs
// from the text of the source, so the branch records the change.
func TestBranchInAnotherProcessTakesTheModelOfTheRequest(t *testing.T) {
	ctx := testCtx(t)
	there := &pinRecorder{}
	cfg := pinConfig(t, "pintest/base")
	cfg.Agents = map[string]AgentProfile{"worker": {System: "Elsewhere."}}
	other := pinAgent(t, cfg, NewMemLedger(), there)
	sp := &otherSpawner{other: other}
	rec := &pinRecorder{}
	a := pinAgent(t, pinConfig(t, "pintest/base"), NewMemLedger(), rec, WithSpawner(sp))
	setProfiles(t, a, map[string]AgentProfile{"worker": {System: "Here.", Model: "pintest/old"}})
	rec.script(endTurn("one"))
	r, err := a.Run(ctx, "first", AsAgent("worker"))
	mustState(t, r, err, StateCompleted)

	there.script(endTurn("branch"))
	b, err := a.Fork(ctx, r.ID, ForkRequest{Input: "go on", Model: "pintest/other", Spawn: true})
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.Wait(ctx, b.ID)
	mustState(t, got, err, StateCompleted)
	there.expect(t, 0, 1, "other", "Elsewhere.", "alpha,beta,gamma,delta")
	var q RunQueuedData
	firstOf(t, ledgerEvents(t, other.Ledger(), b.ID), EvRunQueued, &q)
	if q.Resolved == nil || q.Resolved.Model != "pintest/other" || q.Resolved.SystemDigest != textDigest("Here.") {
		t.Fatalf("resolved profile of the branch = %+v", q.Resolved)
	}
	notes := driftNotes(t, other.Ledger(), b.ID)
	if len(notes) != 1 || notes[0][MetaDriftSystem] != textDigest("Here.")+" -> "+textDigest("Elsewhere.") {
		t.Fatalf("records of the change = %v", notes)
	}
}

// A rule that a person removes from the profile stays in force for a run
// that is already going.
func TestRemovedRuleStaysInForce(t *testing.T) {
	ctx := testCtx(t)
	rec := &pinRecorder{}
	l := NewMemLedger()
	a := pinAgent(t, pinConfig(t, "pintest/base"), l, rec)
	setProfiles(t, a, map[string]AgentProfile{"worker": {Policy: &Rules{Deny: []string{"beta"}}}})
	rec.script(callTools(use("b1", "beta", `{}`)), endTurn("one"))
	r, err := a.Run(ctx, "work", AsAgent("worker"))
	mustState(t, r, err, StateCompleted)
	deniedByPolicy(t, l, r.ID, "b1")
	setProfiles(t, a, map[string]AgentProfile{"worker": {}})
	rec.script(callTools(use("b2", "beta", `{}`)), endTurn("two"))
	if err := a.Send(ctx, r.ID, UserText("again"), OriginCLI); err != nil {
		t.Fatal(err)
	}
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)
	deniedByPolicy(t, l, r.ID, "b2")
}

// A run queued without an effort keeps none after a restart with an
// effort in the configuration. A new run gets that effort.
func TestEmptyEffortStaysEmpty(t *testing.T) {
	ctx := testCtx(t)
	dir := filepath.Join(t.TempDir(), "runs")
	rec := &pinRecorder{}
	cfg := pinConfig(t, "pintest/base")
	cfg.Effort = ""
	a := pinAgent(t, cfg, pinLedger(t, dir), rec)
	rec.script(endTurn("one"))
	r, err := a.Run(ctx, "work")
	mustState(t, r, err, StateCompleted)
	closePin(a)

	cfg2 := pinConfig(t, "pintest/base")
	cfg2.Effort = "max"
	b := pinAgent(t, cfg2, pinLedger(t, dir), rec)
	rec.script(endTurn("two"), endTurn("three"))
	if err := b.Send(ctx, r.ID, UserText("again"), OriginCLI); err != nil {
		t.Fatal(err)
	}
	got, err := b.Wait(ctx, r.ID)
	mustState(t, got, err, StateCompleted)
	fresh, err := b.Run(ctx, "fresh")
	mustState(t, fresh, err, StateCompleted)
	for i, want := range []string{"", "", "max"} {
		if c := rec.call(i); c.effort != want {
			t.Fatalf("effort of request %d = %q, want %q", i, c.effort, want)
		}
	}
}

// The text of an inline profile reaches the model as fw.run.queued stores
// it, with secrets replaced, so its digest is the same before and after a
// restart, and a restart records no change.
func TestInlineTextWithASecretRecordsNoChange(t *testing.T) {
	ctx := testCtx(t)
	dir := filepath.Join(t.TempDir(), "runs")
	rec := &pinRecorder{}
	a := pinAgent(t, pinConfig(t, "pintest/base"), pinLedger(t, dir), rec)
	rec.script(endTurn("one"))
	r, err := a.Run(ctx, "work", WithRunProfile(AgentProfile{System: "Rotate the key AKIAIOSFODNN7EXAMPLE today."}))
	mustState(t, r, err, StateCompleted)
	closePin(a)

	b := pinAgent(t, pinConfig(t, "pintest/base"), pinLedger(t, dir), rec)
	rec.script(endTurn("two"))
	if err := b.Send(ctx, r.ID, UserText("again"), OriginCLI); err != nil {
		t.Fatal(err)
	}
	got, err := b.Wait(ctx, r.ID)
	mustState(t, got, err, StateCompleted)
	if n := len(driftNotes(t, b.Ledger(), r.ID)); n != 0 {
		t.Fatalf("%d records of a change, but the text did not change", n)
	}
	for i := range 2 {
		if c := rec.call(i); !strings.HasPrefix(c.system, "Rotate the key "+Redacted+" today.") {
			t.Fatalf("system prompt of request %d = %q", i, c.system)
		}
	}
}
