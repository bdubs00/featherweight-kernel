package fw

import (
	"slices"
	"testing"
)

func TestTeamsFromGroups(t *testing.T) {
	ctx := testCtx(t)
	cfg := DefaultConfig()
	cfg.Access.Teams = []string{"team-*"}
	a := newTestAgent(t, newScript(endTurn("one"), endTurn("two")).Model(), WithConfig(cfg))
	carol := &Principal{Subject: "carol", Kind: KindUser, Groups: []string{"team-pay", "vpn", "team-ops", "team-pay"}}
	if got := a.TeamsOf(carol); !slices.Equal(got, []string{"team-ops", "team-pay"}) {
		t.Fatalf("teams of carol = %v", got)
	}
	if got := a.TeamsOf(nil); got != nil {
		t.Fatalf("teams of the process = %v", got)
	}
	// An agent belongs to the groups that its access block names exactly.
	acc := &Access{Owners: []string{"carol", "group:team-pay"}, Viewers: []string{"group:team-*", "group:vpn"}}
	if got := a.AgentTeamsOf("", "billing", acc); !slices.Equal(got, []string{"team-pay"}) {
		t.Fatalf("teams of the agent = %v", got)
	}
	// A run keeps the teams of the person who started it, and a child run
	// keeps the teams of its parent.
	r, err := a.Run(WithPrincipal(ctx, carol), "hi")
	mustState(t, r, err, StateCompleted)
	if !slices.Equal(r.Teams, []string{"team-ops", "team-pay"}) {
		t.Fatalf("teams of the run = %v", r.Teams)
	}
	child, err := a.Run(ctx, "sub", WithParent(r.ID))
	mustState(t, child, err, StateCompleted)
	if child.Principal != "carol" || !slices.Equal(child.Teams, r.Teams) {
		t.Fatalf("child = %s %v", child.Principal, child.Teams)
	}
	if !SharesTeam(r.Teams, []string{"x", "team-pay"}) || SharesTeam(r.Teams, []string{"vpn"}) || SharesTeam(nil, r.Teams) {
		t.Fatal("SharesTeam is wrong")
	}
}

func TestTeamsFromTheControlPlane(t *testing.T) {
	ctx := testCtx(t)
	bus := NewMemBus()
	cp := &controlPlane{
		decide: func(p Principal, q AuthzQuery) AuthzDecision {
			return AuthzDecision{Allow: true, Teams: []string{"payments"}}
		},
		teams: func(p Principal) []string {
			if p.Subject == "alice" {
				return []string{"platform", "payments"}
			}
			return nil
		},
	}
	cp.serve(t, bus, "fw.authz")
	a := externalAgent(t, newScript(endTurn("hi"), endTurn("hi")).Model(), bus, nil)
	// With a control plane, the groups of alice do not count as teams.
	if got := a.TeamsOf(alice); got != nil {
		t.Fatalf("teams before an answer = %v", got)
	}
	r, err := a.Run(WithPrincipal(ctx, alice), "hi")
	mustState(t, r, err, StateCompleted)
	if !slices.Equal(r.Teams, []string{"payments", "platform"}) || !slices.Equal(a.TeamsOf(alice), r.Teams) {
		t.Fatalf("teams of the run = %v", r.Teams)
	}
	if got := a.AgentTeamsOf("", "default", &Access{Owners: []string{"group:ops"}}); !slices.Equal(got, []string{"payments"}) {
		t.Fatalf("teams of the agent = %v", got)
	}
	// An answer without teams leaves bob with none.
	r, err = a.Run(WithPrincipal(ctx, bob), "hi")
	mustState(t, r, err, StateCompleted)
	if r.Teams != nil {
		t.Fatalf("teams of the run of bob = %v", r.Teams)
	}
}
