package fw

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// fw.run.queued records the resolved profile next to the inline profile of
// the request: the overlay with the values of the configuration filled in,
// and without the access block of the named profile.
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
		Description:  "Works.",
		Instructions: []string{"named"},
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
	got := q.Resolved
	if got == nil {
		t.Fatal("fw.run.queued has no resolved profile")
	}
	want := AgentProfile{
		Name:         "worker",
		Description:  "Works.",
		System:       DefaultSystemPrompt,
		Instructions: []string{"named", "inline"},
		Tools:        []string{"alpha", "beta"},
		Model:        "pintest/quick",
		Effort:       "medium",
		TimeZone:     "Europe/Berlin",
		Budget:       &Budget{MaxTurns: 9, MaxTokens: 1000},
	}
	gotPolicy := got.Policy
	cp := *got
	cp.Policy = nil
	if !reflect.DeepEqual(cp, want) {
		t.Fatalf("resolved profile = %+v\nwant %+v", cp, want)
	}
	if gotPolicy == nil || !reflect.DeepEqual(*gotPolicy, Rules{Ask: []string{"beta"}}) {
		t.Fatalf("resolved policy = %+v", gotPolicy)
	}
	if strings.Contains(string(e.Data), "alice") || strings.Contains(string(e.Data), `"access"`) {
		t.Fatalf("fw.run.queued holds the access block: %s", e.Data)
	}
	if c := rec.call(0); c.model != "quick" || c.effort != "medium" || c.tools != "alpha,beta" {
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
	if bq.Resolved == nil || bq.Resolved.Model != "pintest/branch" || bq.Resolved.Budget.MaxTurns != 9 || q.Resolved.Model != "pintest/quick" {
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
