package fw

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
)

func grantTool(name string, spec ToolSpec) Tool {
	spec.Name = name
	return NewTool(spec, func(context.Context, json.RawMessage) (ToolResult, error) { return TextResult("ok"), nil })
}

func grantsByName(t *testing.T, a *Agent, p AgentProfile) map[string]ToolGrant {
	t.Helper()
	gs, err := a.ProfileGrants(testCtx(t), p)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]ToolGrant{}
	for _, g := range gs {
		out[g.Spec.Name] = g
	}
	return out
}

func TestProfileGrants(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Policy.Rules = Rules{Ask: []string{"destructive"}, Deny: []string{"bash: git push*"}}
	a := newTestAgent(t, newScript().Model(), WithConfig(cfg), WithTools(
		grantTool("bash", ToolSpec{Destructive: true}),
		grantTool("files", ToolSpec{}),
		grantTool("http", ToolSpec{ReadOnly: true}),
	))

	all := grantsByName(t, a, AgentProfile{Name: "every"})
	if all["bash"].Verdict != "ask" || all["files"].Verdict != "allow" || all["http"].Verdict != "allow" {
		t.Fatalf("grants of an empty tool list = %+v", all)
	}
	want := []PatternRule{{Verdict: "deny", Field: "command", Pattern: "git push*"}}
	if !slices.Equal(all["bash"].Patterns, want) {
		t.Errorf("bash patterns = %+v", all["bash"].Patterns)
	}

	// A profile narrows the tools and adds its own rules.
	p := AgentProfile{Name: "reviewer", Tools: []string{"files", "bash"}, Policy: &Rules{
		Deny: []string{"bash"},
		Ask:  []string{"files: path=*.env"},
	}}
	got := grantsByName(t, a, p)
	if got["http"].Verdict != GrantNone {
		t.Errorf("http = %+v, want none", got["http"])
	}
	if got["bash"].Verdict != "deny" || len(got["bash"].Patterns) != 0 {
		t.Errorf("bash = %+v, want deny without patterns", got["bash"])
	}
	want = []PatternRule{{Verdict: "ask", Field: "path", Pattern: "*.env"}}
	if got["files"].Verdict != "allow" || !slices.Equal(got["files"].Patterns, want) {
		t.Errorf("files = %+v", got["files"])
	}
}

func TestProfileGrantsOptionPolicy(t *testing.T) {
	// A policy from a Go option decides the verdict. Its rules are unknown.
	cfg := DefaultConfig()
	cfg.Policy.Rules = Rules{Deny: []string{"rm: *"}}
	a := newTestAgent(t, newScript().Model(), WithConfig(cfg),
		WithPolicy(NewRulesPolicy(Rules{Default: "ask"})),
		WithTools(grantTool("rm", ToolSpec{})))
	g := grantsByName(t, a, AgentProfile{Name: "x"})["rm"]
	if g.Verdict != "ask" || len(g.Patterns) != 0 {
		t.Errorf("rm = %+v", g)
	}
}

// The configuration defines every profile but the ones that the loader read
// from files, and SetProfiles keeps the defined ones as they are.
func TestProfileLockedAndSetProfiles(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Agents = map[string]AgentProfile{"fixed": {System: "from fw.yaml"}, "free": {System: "from a file"}}
	cfg.FileProfiles = map[string]bool{"free": true}
	a := newTestAgent(t, newScript().Model(), WithConfig(cfg))
	if !a.ProfileLocked("fixed") || a.ProfileLocked("free") || a.ProfileLocked("nope") {
		t.Error("ProfileLocked does not follow the configuration")
	}
	next := a.ProfileSet()
	next["free"] = AgentProfile{System: "changed"}
	next["new"] = AgentProfile{Name: "new", System: "added"}
	if err := a.SetProfiles(next); err != nil {
		t.Fatal(err)
	}
	if p, ok := a.ProfileNamed("new"); !ok || p.Name != "new" || p.System != "added" {
		t.Errorf("new = %+v, %v", p, ok)
	}
	if p, _ := a.ProfileNamed("free"); p.System != "changed" {
		t.Errorf("free = %+v", p)
	}
	if a.ProfileLocked("new") || !a.ProfileLocked("fixed") {
		t.Error("the lock changed with the set")
	}
	// A change to a fixed profile, its removal, and an invalid profile are
	// refused, and the set stays as it was.
	for name, change := range map[string]func(map[string]AgentProfile){
		"edit":    func(m map[string]AgentProfile) { m["fixed"] = AgentProfile{System: "mine"} },
		"remove":  func(m map[string]AgentProfile) { delete(m, "fixed") },
		"invalid": func(m map[string]AgentProfile) { m["bad name"] = AgentProfile{} },
		"effort":  func(m map[string]AgentProfile) { m["x"] = AgentProfile{Effort: "turbo"} },
	} {
		m := a.ProfileSet()
		change(m)
		if err := a.SetProfiles(m); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: SetProfiles = %v, want ErrInvalidRequest", name, err)
		}
	}
	if p, _ := a.ProfileNamed("fixed"); p.System != "from fw.yaml" || len(a.ProfileSet()) != 3 {
		t.Errorf("the set changed on a refusal: %+v", a.ProfileSet())
	}
}

func TestCheckProfile(t *testing.T) {
	a := newTestAgent(t, newScript().Model(), WithProfile(AgentProfile{Name: "fixed"}))
	if err := a.CheckProfile(AgentProfile{Name: "fresh", System: "s"}); err != nil {
		t.Fatalf("check fresh = %v", err)
	}
	if _, ok := a.ProfileNamed("fresh"); ok {
		t.Fatal("CheckProfile saved the profile")
	}
	for name, p := range map[string]AgentProfile{
		"bad name":  {Name: "bad name"},
		"dot":       {Name: "..bad"},
		"effort":    {Name: "x", Effort: "turbo"},
		"model":     {Name: "x", Model: "nope"},
		"time zone": {Name: "x", TimeZone: "Mars/Olympus"},
	} {
		if err := a.CheckProfile(p); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("check of %s = %v", name, err)
		}
	}
}

func TestExtraTools(t *testing.T) {
	ctx := testCtx(t)
	a := newTestAgent(t, newScript(endTurn("a"), endTurn("b"), endTurn("c"), endTurn("d")).Model(),
		WithProfile(AgentProfile{Name: "reviewer", Tools: []string{"files"}}),
		WithTools(grantTool("files", ToolSpec{}), grantTool("bash", ToolSpec{}), grantTool("http", ToolSpec{})))
	start := func(opts ...RunOption) *Run {
		t.Helper()
		r, err := a.Start(ctx, "go", append([]RunOption{AsAgent("reviewer")}, opts...)...)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := start(WithRunProfile(AgentProfile{Tools: []string{"files", "bash"}})); !slices.Equal(r.ExtraTools, []string{"bash"}) {
		t.Errorf("a wider list: extra tools = %v", r.ExtraTools)
	}
	if r := start(WithRunProfile(AgentProfile{Tools: []string{"files"}})); r.ExtraTools != nil {
		t.Errorf("the same list: extra tools = %v", r.ExtraTools)
	}
	parent := start()
	if parent.ExtraTools != nil {
		t.Errorf("no inline profile: extra tools = %v", parent.ExtraTools)
	}
	// A child never gets a tool that its parent lacks, so none is extra.
	if r := start(WithParent(parent.ID), WithRunProfile(AgentProfile{Tools: []string{"*"}})); r.ExtraTools != nil {
		t.Errorf("a child of reviewer: extra tools = %v", r.ExtraTools)
	}
	// The run object of the ledger keeps them.
	r := start(WithRunProfile(AgentProfile{Tools: []string{"*"}}))
	got, err := a.Get(ctx, r.ID)
	if err != nil || !slices.Equal(got.ExtraTools, []string{"bash", "http"}) {
		t.Errorf("get: extra tools = %v, %v", got.ExtraTools, err)
	}
}
