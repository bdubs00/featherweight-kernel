package fw

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
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

type lockStore struct{ locked string }

func (lockStore) Put(context.Context, AgentProfile) error { return nil }
func (lockStore) Delete(context.Context, string) error    { return nil }
func (s lockStore) Locked(name string) bool               { return name == s.locked }

func TestProfileLocked(t *testing.T) {
	a := newTestAgent(t, newScript().Model(), WithProfileStore(lockStore{locked: "fixed"}))
	if !a.ProfileLocked("fixed") || a.ProfileLocked("free") {
		t.Error("ProfileLocked does not follow the store")
	}
	b := newTestAgent(t, newScript().Model(), WithProfileStore(memStoreForGrants{}))
	if b.ProfileLocked("fixed") {
		t.Error("a store without Locked locks a profile")
	}
}

type memStoreForGrants struct{}

func (memStoreForGrants) Put(context.Context, AgentProfile) error { return nil }
func (memStoreForGrants) Delete(context.Context, string) error    { return nil }

func TestOptInTool(t *testing.T) {
	a := newTestAgent(t, newScript().Model(), WithTools(
		grantTool("files", ToolSpec{}),
		grantTool("propose_agent", ToolSpec{OptIn: true}),
	))
	for _, tc := range []struct {
		tools []string
		want  string
	}{
		{nil, GrantNone},
		{[]string{"*"}, GrantNone},
		{[]string{"files"}, GrantNone},
		{[]string{"propose_agent"}, "allow"},
		{[]string{"propose_*"}, "allow"},
		{[]string{"*", "propose_agent"}, "allow"},
	} {
		g := grantsByName(t, a, AgentProfile{Name: "x", Tools: tc.tools})
		if g["propose_agent"].Verdict != tc.want {
			t.Errorf("tools %v: propose_agent = %s, want %s", tc.tools, g["propose_agent"].Verdict, tc.want)
		}
		if tc.tools == nil && g["files"].Verdict != "allow" {
			t.Errorf("an empty list leaves out files: %+v", g["files"])
		}
	}
}

func TestCheckProfile(t *testing.T) {
	ctx := testCtx(t)
	a := newTestAgent(t, newScript().Model(), WithProfile(AgentProfile{Name: "fixed"}), WithProfileStore(lockStore{locked: "fixed"}))
	exists, err := a.CheckProfile(ctx, AgentProfile{Name: "fresh", System: "s"})
	if err != nil || exists {
		t.Fatalf("check fresh = %v, %v", exists, err)
	}
	if _, ok := a.ProfileNamed("fresh"); ok {
		t.Fatal("CheckProfile saved the profile")
	}
	if _, err := a.CheckProfile(ctx, AgentProfile{Name: "fixed"}); !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "change it there") {
		t.Errorf("check of a locked profile = %v", err)
	}
	if _, err := a.CheckProfile(ctx, AgentProfile{Name: "bad name"}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("check of a bad name = %v", err)
	}
	if _, err := a.CheckProfile(ctx, AgentProfile{Name: "x", Effort: "turbo"}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("check of a bad effort = %v", err)
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
