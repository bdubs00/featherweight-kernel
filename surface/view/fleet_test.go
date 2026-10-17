package view

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// stubFleet is a fleet with fixed runs of another member.
type stubFleet struct {
	runs []*fw.Run
	err  error
}

func (stubFleet) Name() string { return "here" }

func (stubFleet) Members(context.Context) ([]fw.FleetMember, error) { return nil, nil }

func (s stubFleet) Runs(context.Context) ([]*fw.Run, error) { return s.runs, s.err }

func (s stubFleet) Run(_ context.Context, id string) (*fw.Run, error) {
	for _, r := range s.runs {
		if r.ID == id {
			return r, nil
		}
	}
	return nil, fw.ErrRunNotFound
}

func (stubFleet) Forward(context.Context, string, *http.Request) (*http.Response, error) {
	return nil, fw.ErrFleetNoRoute
}

func ids(runs []*fw.Run) string {
	var out []string
	for _, r := range runs {
		out = append(out, r.ID)
	}
	return strings.Join(out, ",")
}

func TestFleetRunsAndChildren(t *testing.T) {
	ctx := context.Background()
	future := time.Now().Add(time.Hour)
	remote := []*fw.Run{
		{ID: "R1", Agent: "far", Member: "there", State: fw.StateRunning, CreatedAt: future},
		{ID: "R2", Agent: "far", Member: "there", Parent: "R1", Depth: 1, State: fw.StateRunning, CreatedAt: future.Add(time.Second)},
		{ID: "R3", Agent: "far", Member: "yonder", State: fw.StateCompleted, CreatedAt: time.Unix(0, 0)},
	}
	a := newAgent(t, fw.WithFleet(stubFleet{runs: remote}))
	local, err := a.Run(ctx, "here")
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ scope, member, want string }{
		{"", "", "R2,R1," + local.ID + ",R3"},
		{"fleet", "", "R2,R1," + local.ID + ",R3"},
		{"local", "", local.ID},
		{"", "here", local.ID},
		{"", "there", "R2,R1"},
		{"local", "there", ""},
	} {
		runs, err := Runs(ctx, a, tc.scope, tc.member)
		if err != nil || ids(runs) != tc.want {
			t.Errorf("Runs(%q, %q) = %s, %v; want %s", tc.scope, tc.member, ids(runs), err, tc.want)
		}
	}
	if _, err := Runs(ctx, a, "galaxy", ""); !errors.Is(err, fw.ErrInvalidRequest) {
		t.Errorf("an unknown scope = %v", err)
	}

	kids, err := Children(ctx, a, "R1")
	if err != nil || ids(kids) != "R2" {
		t.Errorf("children of R1 = %s, %v", ids(kids), err)
	}
	if kids, err := Children(ctx, a, local.ID); err != nil || len(kids) != 0 {
		t.Errorf("children of the local run = %s, %v", ids(kids), err)
	}
	if _, err := Children(ctx, a, "NOPE"); !errors.Is(err, fw.ErrRunNotFound) {
		t.Errorf("children of an unknown run = %v", err)
	}

	// An outage of the fleet is an error, not an empty list.
	b := newAgent(t, fw.WithFleet(stubFleet{err: fw.ErrAuthzUnavailable}))
	if _, err := Runs(ctx, b, "", ""); !errors.Is(err, fw.ErrAuthzUnavailable) {
		t.Errorf("an outage = %v", err)
	}
	if w := mustWhoami(t, ctx, a); w.Fleet != "here" || !strings.Contains(w.String(), "member here") {
		t.Errorf("whoami = %+v", w)
	}
}

func TestFleetOffRuns(t *testing.T) {
	ctx := context.Background()
	a := newAgent(t)
	r, err := a.Run(ctx, "x")
	if err != nil {
		t.Fatal(err)
	}
	if runs, err := Runs(ctx, a, "", ""); err != nil || ids(runs) != r.ID {
		t.Errorf("runs = %s, %v", ids(runs), err)
	}
	if _, err := Runs(ctx, a, "", "there"); !errors.Is(err, fw.ErrInvalidRequest) {
		t.Errorf("a member without the fleet = %v", err)
	}
	if w := mustWhoami(t, ctx, a); w.Fleet != "" {
		t.Errorf("whoami = %+v", w)
	}
}
