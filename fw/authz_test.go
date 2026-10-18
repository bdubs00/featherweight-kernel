package fw

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPrincipalMatches(t *testing.T) {
	alice := &Principal{Subject: "sub-1", Name: "alice@example.com", Kind: KindUser, Issuer: "https://idp.example.com", Groups: []string{"platform-team", "oncall"}}
	wl := &Principal{Subject: "spiffe://example.org/ns/prod/sa/reviewer", Kind: KindWorkload}
	cases := []struct {
		entry string
		p     *Principal
		want  bool
	}{
		{"*", alice, true},
		{"sub-1", alice, true},
		{"user:sub-1", alice, true},
		{"alice@example.com", alice, true},
		{"*@example.com", alice, true},
		{"bob@example.com", alice, false},
		{"group:platform-team", alice, true},
		{"group:platform-*", alice, true},
		{"group:admins", alice, false},
		{"kind:user", alice, true},
		{"kind:workload", alice, false},
		{"issuer:https://idp.example.com", alice, true},
		{"spiffe://example.org/ns/prod/*", wl, true},
		{"spiffe://example.org/ns/dev/*", wl, false},
		{"kind:workload", wl, true},
		{"", alice, false},
		{"*", nil, false},
	}
	for _, c := range cases {
		if got := PrincipalMatches(c.entry, c.p); got != c.want {
			t.Errorf("PrincipalMatches(%q, %+v) = %v, want %v", c.entry, c.p, got, c.want)
		}
	}
}

func TestRulesAuthz(t *testing.T) {
	profiles := map[string]AgentProfile{
		"open":    {Name: "open"},
		"private": {Name: "private", Access: &Access{Owners: []string{"alice"}, Operators: []string{"group:ops"}, Viewers: []string{"kind:workload"}}},
	}
	lookup := func(name string) (AgentProfile, bool) { p, ok := profiles[name]; return p, ok }
	alice := &Principal{Subject: "alice", Kind: KindUser}
	bob := &Principal{Subject: "bob", Kind: KindUser, Groups: []string{"ops"}}
	carol := &Principal{Subject: "carol", Kind: KindUser}
	wl := &Principal{Subject: "spiffe://td/x", Kind: KindWorkload}
	root := &Principal{Subject: "root", Kind: KindUser}
	ctx := context.Background()

	z := NewRulesAuthz(AccessConfig{Default: "open", Admins: []string{"root"}}, lookup)
	allowed := func(z Authz, p *Principal, action string, t Target) bool { return z.Allow(ctx, p, action, t) == nil }
	run := &Run{ID: "r1", Agent: "private", Principal: "carol"}
	cases := []struct {
		name   string
		p      *Principal
		action string
		target Target
		want   bool
	}{
		{"admin does everything", root, ActionAgentDelete, Target{Agent: "private"}, true},
		{"admin action", root, ActionAdmin, Target{}, true},
		{"not admin", alice, ActionAdmin, Target{}, false},
		{"open agent: anyone operates", carol, ActionAgentRun, Target{Agent: "open"}, true},
		{"open agent: nobody owns", carol, ActionAgentEdit, Target{Agent: "open"}, false},
		{"owner edits", alice, ActionAgentEdit, Target{Agent: "private"}, true},
		{"owner runs", alice, ActionAgentRun, Target{Agent: "private"}, true},
		{"operator by group runs", bob, ActionAgentRun, Target{Agent: "private"}, true},
		{"operator cannot edit", bob, ActionAgentEdit, Target{Agent: "private"}, false},
		{"viewer views", wl, ActionAgentView, Target{Agent: "private"}, true},
		{"viewer cannot run", wl, ActionAgentRun, Target{Agent: "private"}, false},
		{"stranger cannot view", carol, ActionAgentView, Target{Agent: "private"}, false},
		{"own run is visible", carol, ActionRunView, Target{Run: run}, true},
		{"own run is operable", carol, ActionRunOperate, Target{Run: run}, true},
		{"viewer sees a run", wl, ActionRunView, Target{Run: run}, true},
		{"viewer cannot operate a run", wl, ActionRunOperate, Target{Run: run}, false},
		{"operator operates a run", bob, ActionRunOperate, Target{Run: run}, true},
		{"anyone creates when open", carol, ActionAgentCreate, Target{}, true},
		{"unknown agent follows the default", carol, ActionAgentRun, Target{Agent: "nope"}, true},
	}
	for _, c := range cases {
		if got := allowed(z, c.p, c.action, c.target); got != c.want {
			t.Errorf("%s: allowed = %v, want %v", c.name, got, c.want)
		}
	}
	if err := z.Allow(ctx, carol, ActionAgentEdit, Target{Agent: "private"}); !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("error = %v", err)
	}

	closed := NewRulesAuthz(AccessConfig{Default: "closed", Admins: []string{"root"}, Creators: []string{"group:ops"}}, lookup)
	if allowed(closed, carol, ActionAgentRun, Target{Agent: "open"}) {
		t.Error("a closed default lets a stranger run an agent without an access block")
	}
	if !allowed(closed, root, ActionAgentRun, Target{Agent: "open"}) {
		t.Error("a closed default stops the admin")
	}
	if allowed(closed, carol, ActionAgentCreate, Target{}) || !allowed(closed, bob, ActionAgentCreate, Target{}) {
		t.Error("creators are not honored")
	}
	if !allowed(closed, alice, ActionAgentRun, Target{Agent: "private"}) {
		t.Error("an owner is stopped by the closed default")
	}
}

func TestKernelEnforcesAccess(t *testing.T) {
	ctx := testCtx(t)
	cfg := DefaultConfig()
	cfg.Access = AccessConfig{Default: "closed", Admins: []string{"root"}}
	a := newTestAgent(t, newScript(endTurn("a"), endTurn("b"), endTurn("c")).Model(), WithConfig(cfg),
		WithProfile(AgentProfile{Name: "team", Access: &Access{Owners: []string{"alice"}, Viewers: []string{"group:readers"}}}))
	alice := WithPrincipal(ctx, &Principal{Subject: "alice", Kind: KindUser})
	bob := WithPrincipal(ctx, &Principal{Subject: "bob", Kind: KindUser, Groups: []string{"readers"}})
	carol := WithPrincipal(ctx, &Principal{Subject: "carol", Kind: KindUser})
	root := WithPrincipal(ctx, &Principal{Subject: "root", Kind: KindUser})

	if _, err := a.Start(carol, "x", AsAgent("team")); !errors.Is(err, ErrForbidden) {
		t.Fatalf("stranger start = %v", err)
	}
	r, err := a.Run(alice, "x", AsAgent("team"))
	mustState(t, r, err, StateCompleted)
	if r.Principal != "alice" {
		t.Fatalf("principal = %q", r.Principal)
	}
	if _, err := a.Get(carol, r.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("stranger get = %v", err)
	}
	if _, err := a.Get(bob, r.ID); err != nil {
		t.Fatalf("viewer get = %v", err)
	}
	if err := a.Cancel(bob, r.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer cancel = %v", err)
	}
	if err := a.Send(bob, r.ID, UserText("more"), OriginHTTP); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer send = %v", err)
	}
	if _, err := a.Events(carol, r.ID, 0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("stranger events = %v", err)
	}
	list, _ := a.List(carol)
	if len(list) != 0 {
		t.Fatalf("stranger list = %d runs", len(list))
	}
	list, _ = a.List(bob)
	if len(list) != 1 {
		t.Fatalf("viewer list = %d runs", len(list))
	}
	// The one who started a run on an open agent in a closed deployment is
	// the admin; the run owner always sees the run.
	r2, err := a.Run(root, "y")
	mustState(t, r2, err, StateCompleted)
	if _, err := a.Get(root, r2.ID); err != nil {
		t.Fatal(err)
	}
	// A child inherits the principal of its parent.
	c, err := a.Run(ctx, "child", WithParent(r.ID))
	mustState(t, c, err, StateCompleted)
	if c.Principal != "alice" {
		t.Fatalf("child principal = %q", c.Principal)
	}
	kids, err := a.Children(bob, r.ID)
	if err != nil || len(kids) != 1 {
		t.Fatalf("viewer children = %v, %v", kids, err)
	}
	// A call without a principal is the process itself.
	if _, err := a.Get(ctx, r.ID); err != nil {
		t.Fatalf("internal get = %v", err)
	}
	// Profiles are filtered by view.
	if names := profileNames(a.ProfilesFor(carol)); len(names) != 0 {
		t.Fatalf("stranger profiles = %v", names)
	}
	if names := profileNames(a.ProfilesFor(bob)); strings.Join(names, ",") != "team" {
		t.Fatalf("viewer profiles = %v", names)
	}
}

func profileNames(ps []AgentProfile) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}

func TestChainAuthnAndModes(t *testing.T) {
	deny := AuthnDeny{}
	tok := TokenAuthn{Token: "s3cret"}
	chain := ChainAuthn{deny, tok}
	req := httptest.NewRequest("GET", "/", nil)
	if _, err := chain.Authenticate(req); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("no credential = %v", err)
	}
	req.Header.Set("Authorization", "Bearer s3cret")
	p, err := chain.Authenticate(req)
	if err != nil || p.Subject != KindToken || p.Kind != KindToken {
		t.Fatalf("principal = %+v, %v", p, err)
	}
	rec := httptest.NewRecorder()
	chain.Challenge(rec, req)
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != "deny" {
		t.Fatalf("challenge = %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	if got := (AuthConfig{Mode: " OIDC, spiffe ,token"}).Modes(); strings.Join(got, "|") != "oidc|spiffe|token" {
		t.Fatalf("modes = %v", got)
	}
	if got := (AuthConfig{}).Modes(); strings.Join(got, "|") != "token" {
		t.Fatalf("default modes = %v", got)
	}
	t.Setenv("FW_API_TOKEN", "x")
	cfg := DefaultConfig()
	cfg.Auth.Mode = "none,token"
	a, err := NewAuthn(context.Background(), cfg, EnvSecrets{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.(ChainAuthn); !ok {
		t.Fatalf("NewAuthn = %T, want a chain", a)
	}
	cfg.Auth.Mode = "nope"
	if _, err := NewAuthn(context.Background(), cfg, EnvSecrets{}); err == nil || !strings.Contains(err.Error(), "known: token, none") {
		t.Fatalf("unknown mode = %v", err)
	}
}

// AuthnDeny refuses every request, for tests of the chain.
type AuthnDeny struct{}

func (AuthnDeny) Authenticate(*http.Request) (*Principal, error) { return nil, ErrUnauthorized }
func (AuthnDeny) Challenge(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("WWW-Authenticate", "deny")
	w.WriteHeader(http.StatusUnauthorized)
}

func TestServeWithTLS(t *testing.T) {
	ctx, cancel := context.WithCancel(testCtx(t))
	defer cancel()
	srv := httptest.NewUnstartedServer(nil)
	srv.StartTLS()
	cert := srv.TLS.Certificates[0]
	srv.Close()
	a := newTestAgent(t, newScript().Model(), WithTLS(&tls.Config{Certificates: []tls.Certificate{cert}}))
	ln, err := newLocalListener()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- a.ServeListener(ctx, ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.TLS == nil {
				http.Error(w, "no tls", 500)
				return
			}
			_, _ = w.Write([]byte(r.Proto))
		}))
	}()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, ForceAttemptHTTP2: true}}
	resp, err := client.Get("https://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 32)
	n, _ := resp.Body.Read(body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(string(body[:n]), "HTTP/2") {
		t.Fatalf("status %d, proto %q", resp.StatusCode, body[:n])
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestFileTLSConfig(t *testing.T) {
	if _, err := fileTLSConfig(ListenTLSConfig{Provider: "file"}); err == nil {
		t.Fatal("missing cert and key accepted")
	}
	cfg := DefaultConfig()
	cfg.TLS.Provider = "nope"
	if _, err := NewTLSConfig(context.Background(), cfg, EnvSecrets{}); err == nil {
		t.Fatal("unknown provider accepted")
	}
	RegisterTLSProvider("test-static", func(context.Context, *Config, Secrets) (*tls.Config, error) {
		return &tls.Config{ServerName: "static"}, nil
	})
	cfg.TLS.Provider = "test-static"
	tc, err := NewTLSConfig(context.Background(), cfg, EnvSecrets{})
	if err != nil || tc.ServerName != "static" {
		t.Fatalf("provider = %+v, %v", tc, err)
	}
}

func newLocalListener() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }

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

// A check about a run of another member carries the member, the access
// block, and the lineage to the Authz unchanged, in one batch, so the
// control-plane authorization of the workbench can send them on. A
// LineageAuthz is not asked again for the ancestors.
func TestAuthorizeAllPassesMemberFacts(t *testing.T) {
	ctx := testCtx(t)
	var got []AuthzCheck
	z := &recordingAuthz{allow: func(p *Principal, c AuthzCheck) error {
		got = append(got, c)
		return fmt.Errorf("%w: no", ErrForbidden)
	}}
	a := newTestAgent(t, newScript(endTurn("hi")).Model(), WithAuthzFrom(func(*Agent) (Authz, string, error) { return z, AccessModeExternal, nil }))
	if a.AccessMode() != AccessModeExternal {
		t.Fatalf("access mode = %q", a.AccessMode())
	}
	run := &Run{ID: "r2", Agent: "reviewer", Principal: "carol", State: StateInputRequired, Parent: "r1"}
	errs := a.AuthorizeAll(WithPrincipal(ctx, bob), []AuthzCheck{{Action: ActionRunView, Target: Target{
		Agent: "reviewer", Run: run, Member: "team/other",
		Access:  &Access{Viewers: []string{"bob"}},
		Lineage: []RunRef{{ID: "r1", Agent: "planner", Principal: "carol"}},
	}}, {Action: ActionAgentView, Target: Target{Agent: "reviewer"}}})
	if len(errs) != 2 || !errors.Is(errs[0], ErrForbidden) || !errors.Is(errs[1], ErrForbidden) {
		t.Fatalf("decisions = %v", errs)
	}
	if len(got) != 2 || got[0].Target.Member != "team/other" || got[0].Target.Access == nil || got[0].Target.Access.Viewers[0] != "bob" ||
		got[0].Target.Run == nil || got[0].Target.Run.ID != "r2" || len(got[0].Target.Lineage) != 1 || got[0].Target.Lineage[0].ID != "r1" {
		t.Fatalf("checks = %+v", got)
	}
}

// recordingAuthz is a batch Authz that decides about the lineage itself.
type recordingAuthz struct {
	allow func(p *Principal, c AuthzCheck) error
}

func (*recordingAuthz) DecidesLineage() {}

func (z *recordingAuthz) Allow(ctx context.Context, p *Principal, action string, t Target) error {
	return z.allow(p, AuthzCheck{Action: action, Target: t})
}

func (z *recordingAuthz) AllowAll(ctx context.Context, p *Principal, checks []AuthzCheck) []error {
	out := make([]error, len(checks))
	for i, c := range checks {
		out[i] = z.allow(p, c)
	}
	return out
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
