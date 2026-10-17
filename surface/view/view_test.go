package view

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func newAgent(t *testing.T, opts ...fw.Option) *fw.Agent {
	t.Helper()
	t.Setenv("FW_ATESPACE", "")
	model := fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
		return &fw.Response{Content: []fw.Block{fw.Text("ok")}, StopReason: fw.StopEndTurn, Usage: fw.Usage{InputTokens: 10, OutputTokens: 5}}, nil
	}}
	base := []fw.Option{
		fw.WithoutDefaults(),
		fw.WithAuthn(fw.NoAuthn{}),
		fw.WithModel(model),
		fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()),
		fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	}
	a, err := fw.New(append(append(base, fw.WithTitles(fw.TitlesInput)), opts...)...)
	if err != nil {
		t.Fatalf("fw.New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	return a
}

func mustWhoami(t *testing.T, ctx context.Context, a *fw.Agent) Whoami {
	t.Helper()
	w, err := WhoamiOf(ctx, a)
	if err != nil {
		t.Fatalf("WhoamiOf: %v", err)
	}
	return w
}

func TestRolesAndAgents(t *testing.T) {
	ctx := context.Background()
	a := newAgent(t,
		fw.WithProfile(fw.AgentProfile{Name: "reviewer", Description: "Reviews code", Access: &fw.Access{Owners: []string{"alice"}, Viewers: []string{"bob"}}}),
		fw.WithProfile(fw.AgentProfile{Name: "writer", Access: &fw.Access{Operators: []string{"group:docs"}}}),
	)
	alice := fw.WithPrincipal(ctx, &fw.Principal{Subject: "alice", Kind: fw.KindUser})
	bob := fw.WithPrincipal(ctx, &fw.Principal{Subject: "bob", Kind: fw.KindUser, Groups: []string{"docs"}})
	if _, err := a.Run(alice, "review", fw.AsAgent("reviewer")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(bob, "write", fw.AsAgent("writer")); err != nil {
		t.Fatal(err)
	}

	w := mustWhoami(t, alice, a)
	if w.Principal.Subject != "alice" || w.Roles["reviewer"] != "owner" || w.Roles["writer"] != "" || !w.CanCreate || w.Admin {
		t.Fatalf("alice = %+v", w)
	}
	if !strings.Contains(w.String(), "reviewer: owner") {
		t.Errorf("text = %q", w.String())
	}
	w = mustWhoami(t, bob, a)
	if w.Roles["reviewer"] != "viewer" || w.Roles["writer"] != "operator" {
		t.Fatalf("bob = %+v", w)
	}
	w = mustWhoami(t, ctx, a)
	if w.Principal.Subject != "" || w.Roles["reviewer"] != "owner" || !w.Admin || !strings.Contains(w.String(), "process itself") {
		t.Fatalf("process = %+v", w)
	}

	agents, err := Agents(bob, a)
	if err != nil || len(agents) != 2 {
		t.Fatalf("agents = %+v, %v", agents, err)
	}
	if agents[0].Name != "reviewer" || agents[0].Role != "viewer" || agents[0].Usage.Runs != 1 || agents[0].Usage.Tokens != 15 ||
		agents[1].Name != "writer" || agents[1].Role != "operator" || agents[1].Usage.Runs != 1 || agents[1].Usage.Turns != 1 {
		t.Fatalf("agents = %+v", agents)
	}
	b, _ := json.Marshal(agents[0])
	for _, want := range []string{`"name":"reviewer"`, `"role":"viewer"`, `"usage":{`, `"owners":["alice"]`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("agent JSON lacks %s: %s", want, b)
		}
	}
	if _, err := Agent(bob, a, "nope"); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("unknown agent = %v", err)
	}

}

func TestPutKeepsAccessAndNormalizes(t *testing.T) {
	ctx := context.Background()
	a := newAgent(t, fw.WithProfile(fw.AgentProfile{Name: "reviewer", Access: &fw.Access{Owners: []string{"alice"}}}))
	alice := fw.WithPrincipal(ctx, &fw.Principal{Subject: "alice", Kind: fw.KindUser})
	v, created, err := Put(alice, a, fw.AgentProfile{Name: " reviewer ", Description: "edited", Effort: "LOW", TimeZone: " America/Chicago ", Policy: &fw.Rules{Default: "Ask"}})
	if err != nil || created || v.Description != "edited" || v.Effort != "low" || v.TimeZone != "America/Chicago" || v.Policy.Default != "ask" || v.Access == nil || v.Access.Owners[0] != "alice" {
		t.Fatalf("put = %+v, created %v, %v", v, created, err)
	}
	v, created, err = Put(alice, a, fw.AgentProfile{Name: "new", Access: &fw.Access{Owners: []string{" alice ", ""}, Viewers: []string{"*"}}})
	if err != nil || !created || v.Role != "owner" || len(v.Access.Owners) != 1 || v.Access.Owners[0] != "alice" || len(v.Access.Operators) != 0 {
		t.Fatalf("create = %+v, created %v, %v", v, created, err)
	}
	if _, _, err := Put(alice, a, fw.AgentProfile{Name: "bad", Policy: &fw.Rules{Default: "maybe"}}); err == nil {
		t.Error("a bad policy default was accepted")
	}
	_, _, err = Put(alice, a, fw.AgentProfile{Name: "bad", TimeZone: "Mars/Olympus"})
	if !errors.Is(err, fw.ErrInvalidRequest) || !strings.Contains(err.Error(), "timeZone") {
		t.Errorf("a bad time zone: %v", err)
	}
}

func TestFilterAndTail(t *testing.T) {
	ctx := context.Background()
	a := newAgent(t, fw.WithProfile(fw.AgentProfile{Name: "reviewer"}))
	alice := fw.WithPrincipal(ctx, &fw.Principal{Subject: "alice"})
	r1, err := a.Run(alice, "one", fw.AsAgent("reviewer"))
	if err != nil {
		t.Fatal(err)
	}
	r2, err := a.Run(ctx, "two")
	if err != nil {
		t.Fatal(err)
	}
	runs, _ := a.List(ctx)
	if got := Filter(runs, "reviewer", "", "", 0); len(got) != 1 || got[0].ID != r1.ID {
		t.Errorf("by agent = %+v", got)
	}
	if got := Filter(runs, "", "completed", "alice", 0); len(got) != 1 || got[0].ID != r1.ID {
		t.Errorf("by state and principal = %+v", got)
	}
	if got := Filter(runs, "", "active", "", 0); len(got) != 0 {
		t.Errorf("active = %+v", got)
	}
	if got := Filter(runs, "", "", "", 1); len(got) != 1 || got[0].ID != r2.ID {
		t.Errorf("limit = %+v", got)
	}

	lines, next, err := Tail(ctx, a, r1.ID, 0, 3)
	if err != nil || len(lines) != 3 || next != 0 || lines[2].Type != fw.EvRunFinished || !strings.Contains(lines[2].Line, "completed") {
		t.Fatalf("tail = %+v, %d, %v", lines, next, err)
	}
	all, _, _ := Tail(ctx, a, r1.ID, 0, 100)
	page, next, err := Tail(ctx, a, r1.ID, 1, 2)
	if err != nil || len(page) != 2 || page[0].Seq != 1 || next != 3 {
		t.Fatalf("page = %+v, next %d, %v", page, next, err)
	}
	if !strings.Contains(all[0].Line, "queued for the agent reviewer by alice") {
		t.Errorf("first line = %q", all[0].Line)
	}
	for _, l := range all {
		if l.Line == "" || l.Seq == 0 || l.Time.IsZero() {
			t.Errorf("line = %+v", l)
		}
	}
}

// countingAuthz allows everything and counts its calls. It implements
// fw.BatchAuthz, so a batch of checks is one call.
type countingAuthz struct {
	mu     sync.Mutex
	single int
	batch  int
	checks int
	err    error
}

func (c *countingAuthz) Allow(context.Context, *fw.Principal, string, fw.Target) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.single++
	return c.err
}

func (c *countingAuthz) AllowAll(_ context.Context, _ *fw.Principal, checks []fw.AuthzCheck) []error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.batch++
	c.checks += len(checks)
	out := make([]error, len(checks))
	for i := range out {
		out[i] = c.err
	}
	return out
}

func (c *countingAuthz) calls() (single, batch, checks int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.single, c.batch, c.checks
}

func TestOneBatchPerView(t *testing.T) {
	z := &countingAuthz{}
	a := newAgent(t, fw.WithAuthz(z),
		fw.WithProfile(fw.AgentProfile{Name: "a"}),
		fw.WithProfile(fw.AgentProfile{Name: "b"}),
		fw.WithProfile(fw.AgentProfile{Name: "c"}),
	)
	ctx := fw.WithPrincipal(context.Background(), &fw.Principal{Subject: "alice", Kind: fw.KindUser})
	w := mustWhoami(t, ctx, a)
	single, batch, checks := z.calls()
	// Three agents with three checks each, plus admin and create.
	if single != 0 || batch != 1 || checks != 3*3+2 || len(w.Roles) != 3 || w.Roles["b"] != "owner" || !w.Admin || !w.CanCreate {
		t.Fatalf("whoami made %d single and %d batch calls with %d checks: %+v", single, batch, checks, w)
	}
	// The list of agents is one batch. No run exists, so the listing of
	// the runs asks nothing.
	if _, err := Agents(ctx, a); err != nil {
		t.Fatal(err)
	}
	if single, batch, _ := z.calls(); single != 0 || batch != 2 {
		t.Fatalf("agents made %d single and %d batch calls in all, want 0 and 2", single, batch)
	}
	if v, err := Agent(ctx, a, "a"); err != nil || v.Role != "owner" {
		t.Fatalf("agent = %+v, %v", v, err)
	}
	if single, batch, _ := z.calls(); single != 0 || batch != 3 {
		t.Fatalf("one agent made %d single and %d batch calls in all, want 0 and 3", single, batch)
	}
}

func TestAuthzUnavailable(t *testing.T) {
	z := &countingAuthz{err: fmt.Errorf("%w: test", fw.ErrAuthzUnavailable)}
	a := newAgent(t, fw.WithAuthz(z), fw.WithProfile(fw.AgentProfile{Name: "reviewer"}))
	ctx := fw.WithPrincipal(context.Background(), &fw.Principal{Subject: "alice", Kind: fw.KindUser})
	if _, err := WhoamiOf(ctx, a); !errors.Is(err, fw.ErrAuthzUnavailable) {
		t.Fatalf("whoami = %v", err)
	}
	if _, err := Agents(ctx, a); !errors.Is(err, fw.ErrAuthzUnavailable) {
		t.Fatalf("agents = %v", err)
	}
	if _, err := Agent(ctx, a, "reviewer"); !errors.Is(err, fw.ErrAuthzUnavailable) || errors.Is(err, fw.ErrProfileNotFound) {
		t.Fatalf("agent = %v", err)
	}
	if role := RoleOf(ctx, a, "reviewer"); role != "none" {
		t.Fatalf("role = %q", role)
	}
}

// With no profile configured, Agents lists the built-in profile "default",
// and Agent finds it too.
func TestBuiltinDefaultAgent(t *testing.T) {
	a := newAgent(t)
	ctx := fw.WithPrincipal(context.Background(), &fw.Principal{Subject: "alice", Kind: fw.KindUser})
	if _, err := a.Run(ctx, "hi"); err != nil {
		t.Fatal(err)
	}
	list, err := Agents(ctx, a)
	if err != nil || len(list) != 1 || list[0].Name != "default" {
		t.Fatalf("agents = %+v, %v", list, err)
	}
	v, err := Agent(ctx, a, "default")
	if err != nil || v.Name != "default" || v.Role != list[0].Role || v.Usage.Runs != 1 {
		t.Fatalf("agent = %+v, %v", v, err)
	}
	if _, err := Agent(ctx, a, "nope"); !errors.Is(err, fw.ErrProfileNotFound) {
		t.Fatalf("unknown agent = %v", err)
	}
}
