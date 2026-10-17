package fw

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFleetToken(t *testing.T) {
	for in, want := range map[string]string{
		"default/reviewer": "default_reviewer",
		"node7:8080":       "node7_8080",
		"a.b c*>":          "a_b_c__",
		"ok-name_1":        "ok-name_1",
		"":                 "_",
	} {
		if got := FleetToken(in); got != want {
			t.Errorf("FleetToken(%q) = %q, want %q", in, got, want)
		}
	}
	if got := FleetRecordSubject("fw.fleet", "default/reviewer"); got != "fw.fleet.members.default_reviewer" {
		t.Errorf("record subject = %q", got)
	}
	if got := FleetAliveSubject("fw.fleet", "node7:8080"); got != "fw.fleet.alive.node7_8080" {
		t.Errorf("alive subject = %q", got)
	}
}

// collect gathers the events that a watch delivers.
type collect struct {
	mu     sync.Mutex
	events []Event
}

func (c *collect) add(e Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
	return nil
}

func (c *collect) types(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mu.Lock()
		got := len(c.events)
		var out []string
		if got >= n {
			for _, e := range c.events {
				out = append(out, e.Type)
			}
		}
		c.mu.Unlock()
		if got >= n {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("got %d events, want %d", got, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestMemBusRetainer(t *testing.T) {
	ctx := context.Background()
	b := NewMemBus()
	var _ Retainer = b
	ev := func(typ string) Event { return MustEvent("", typ, nil) }
	if err := b.Retain(ctx, "f.members.a", ev("old-a")); err != nil {
		t.Fatal(err)
	}
	_ = b.Retain(ctx, "f.members.a", ev("a"))
	_ = b.Retain(ctx, "f.members.b", ev("b"))
	_ = b.Retain(ctx, "f.alive.a", ev("alive"))

	var c collect
	stop, err := b.Watch(ctx, "f.members.*", c.add)
	if err != nil {
		t.Fatal(err)
	}
	// The kept events come first, sorted by subject, and only the newest
	// event of each subject is kept.
	if got := strings.Join(c.types(t, 2), ","); got != "a,b" {
		t.Fatalf("kept = %s", got)
	}
	_ = b.Retain(ctx, "f.members.c", ev("c"))
	_ = b.Retain(ctx, "f.alive.c", ev("alive-c"))
	if got := strings.Join(c.types(t, 3), ","); got != "a,b,c" {
		t.Fatalf("after retain = %s", got)
	}
	// A retained event does not reach a plain subscriber.
	var plain collect
	unsub, _ := b.Subscribe(ctx, "f.members.*", plain.add)
	defer unsub()
	_ = b.Retain(ctx, "f.members.d", ev("d"))
	c.types(t, 4)
	plain.mu.Lock()
	n := len(plain.events)
	plain.mu.Unlock()
	if n != 0 {
		t.Fatalf("a subscriber got %d retained events", n)
	}
	stop()
	stop()
	_ = b.Retain(ctx, "f.members.e", ev("e"))
	time.Sleep(20 * time.Millisecond)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.events) != 4 {
		t.Fatalf("a stopped watch got %d events", len(c.events))
	}
}

func TestRulesRefuseAnotherMember(t *testing.T) {
	ctx := context.Background()
	z := NewRulesAuthz(AccessConfig{Admins: []string{"alice"}}, func(string) (AgentProfile, bool) { return AgentProfile{}, false })
	run := &Run{ID: "r1", Agent: "reviewer", Principal: "alice"}
	if err := z.Allow(ctx, alice, ActionRunView, Target{Agent: "reviewer", Run: run}); err != nil {
		t.Fatalf("a local run = %v", err)
	}
	// An admin of this process is not an admin of another one, and the
	// starter of a run is judged by its owner.
	err := z.Allow(ctx, alice, ActionRunView, Target{Agent: "reviewer", Run: run, Member: "team/other"})
	if !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "team/other") {
		t.Fatalf("a run of another member = %v", err)
	}
}

func TestExternalAuthzSendsMemberFacts(t *testing.T) {
	ctx := testCtx(t)
	bus := NewMemBus()
	cp := &controlPlane{decide: func(Principal, AuthzQuery) AuthzDecision { return AuthzDecision{Allow: true} }}
	cp.serve(t, bus, "fw.authz")
	a := externalAgent(t, newScript(endTurn("hi")).Model(), bus, nil)
	if a.AccessMode() != AccessModeExternal {
		t.Fatalf("access mode = %q", a.AccessMode())
	}
	run := &Run{ID: "r2", Agent: "reviewer", Principal: "carol", State: StateInputRequired, Parent: "r1"}
	errs := a.AuthorizeAll(WithPrincipal(ctx, bob), []AuthzCheck{{Action: ActionRunView, Target: Target{
		Agent: "reviewer", Run: run, Member: "team/other",
		Access:  &Access{Viewers: []string{"bob"}},
		Lineage: []RunRef{{ID: "r1", Agent: "planner", Principal: "carol"}},
	}}})
	if errs[0] != nil {
		t.Fatalf("decision = %v", errs[0])
	}
	reqs := cp.authzRequests()
	if len(reqs) != 1 || len(reqs[0].Checks) != 1 {
		t.Fatalf("requests = %+v", reqs)
	}
	q := reqs[0].Checks[0]
	if q.Member != "team/other" || q.Agent != "reviewer" || q.Access == nil || q.Access.Viewers[0] != "bob" ||
		q.Run == nil || q.Run.ID != "r2" || q.Run.Principal != "carol" || len(q.Run.Lineage) != 1 || q.Run.Lineage[0].ID != "r1" {
		t.Fatalf("check = %+v run = %+v", q, q.Run)
	}
}

func TestAccessMode(t *testing.T) {
	a := newTestAgent(t, newScript(endTurn("hi")).Model())
	if a.AccessMode() != AccessModeRules {
		t.Fatalf("default = %q", a.AccessMode())
	}
	b := newTestAgent(t, newScript(endTurn("hi")).Model(), WithAuthz(AuthzFunc(func(context.Context, *Principal, string, Target) error { return nil })))
	if b.AccessMode() != AccessModeCustom {
		t.Fatalf("WithAuthz = %q", b.AccessMode())
	}
}

func TestRequireAuthKeepsCredential(t *testing.T) {
	a := newTestAgent(t, newScript(endTurn("hi")).Model(), WithAuthn(TokenAuthn{Token: "s3cret"}))
	var got string
	h := RequireAuth(a, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = CredentialFrom(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/runs", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got != "Bearer s3cret" {
		t.Fatalf("credential = %q", got)
	}
	if CredentialFrom(context.Background()) != "" {
		t.Fatal("an empty context carries a credential")
	}
}

// fakeFleet is a Fleet with nothing in it.
type fakeFleet struct {
	name   string
	closed bool
}

func (f *fakeFleet) Name() string                                   { return f.name }
func (f *fakeFleet) Members(context.Context) ([]FleetMember, error) { return nil, nil }
func (f *fakeFleet) Runs(context.Context) ([]*Run, error)           { return nil, nil }
func (f *fakeFleet) Run(context.Context, string) (*Run, error)      { return nil, ErrRunNotFound }
func (f *fakeFleet) Forward(context.Context, string, *http.Request) (*http.Response, error) {
	return nil, ErrFleetNoRoute
}
func (f *fakeFleet) Close() error { f.closed = true; return nil }

func TestOpenFleet(t *testing.T) {
	// Without fleet.subject, no fleet opens.
	a := newTestAgent(t, newScript(endTurn("hi")).Model())
	if a.Fleet() != nil {
		t.Fatal("a fleet without fleet.subject")
	}

	opened := &fakeFleet{name: "here"}
	RegisterFleet(func(_ context.Context, got *Agent) (Fleet, error) {
		if got.Config().Fleet.Subject != "fw.fleet" {
			t.Errorf("subject = %q", got.Config().Fleet.Subject)
		}
		return opened, nil
	})
	t.Cleanup(func() { RegisterFleet(nil) })
	cfg := DefaultConfig()
	cfg.Fleet.Subject = "fw.fleet"
	b, err := New(WithConfig(cfg), WithModel(newScript(endTurn("hi")).Model()), WithLedger(NewMemLedger()), WithBus(NewMemBus()), WithAuthn(NoAuthn{}), WithoutDefaults())
	if err != nil {
		t.Fatal(err)
	}
	if b.Fleet() != opened {
		t.Fatalf("fleet = %v", b.Fleet())
	}
	b.Close(context.Background())
	if !opened.closed {
		t.Fatal("the fleet was not closed at shutdown")
	}

	// WithFleet wins over the registered driver.
	mine := &fakeFleet{name: "mine"}
	c, err := New(WithConfig(cfg), WithModel(newScript(endTurn("hi")).Model()), WithLedger(NewMemLedger()), WithBus(NewMemBus()), WithAuthn(NoAuthn{}), WithoutDefaults(), WithFleet(mine))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	if c.Fleet() != mine {
		t.Fatalf("fleet = %v", c.Fleet())
	}

	// A driver that fails stops New.
	RegisterFleet(func(context.Context, *Agent) (Fleet, error) { return nil, errors.New("fleet: no bus") })
	if _, err := New(WithConfig(cfg), WithModel(newScript(endTurn("hi")).Model()), WithLedger(NewMemLedger()), WithBus(NewMemBus()), WithAuthn(NoAuthn{}), WithoutDefaults()); err == nil || !strings.Contains(err.Error(), "no bus") {
		t.Fatalf("a failing driver = %v", err)
	}
}

func TestTokenFromAFileWithANewline(t *testing.T) {
	t.Setenv("FW_API_TOKEN", "s3cret\n")
	cfg := DefaultConfig()
	authn, err := NewAuthn(context.Background(), cfg, EnvSecrets{})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/runs", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	if _, err := authn.Authenticate(req); err != nil {
		t.Fatalf("a token with a trailing newline in its secret = %v", err)
	}
}

func TestIDTime(t *testing.T) {
	before := time.Now().Add(-time.Millisecond)
	at, ok := IDTime(NewID())
	if !ok || at.Before(before.Truncate(time.Millisecond)) || at.After(time.Now()) {
		t.Fatalf("IDTime(NewID()) = %v, %v", at, ok)
	}
	// Too short, a first character past 7, and a letter outside the alphabet.
	for _, bad := range []string{"", "short", "8ZZZZZZZZZZZZZZZZZZZZZZZZZ", "0IJA3Q9Z6K2M4N8P0R1S2T3U4V"} {
		if _, ok := IDTime(bad); ok {
			t.Errorf("IDTime(%q) reports a time", bad)
		}
	}
}
