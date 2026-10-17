package fw

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testKey(b byte) func() ([]byte, error) {
	return func() ([]byte, error) { return bytes.Repeat([]byte{b}, 32), nil }
}

func TestFileGrantStore(t *testing.T) {
	ctx := testCtx(t)
	dir := filepath.Join(t.TempDir(), "grants")
	s := NewFileGrantStore(dir, testKey(7))
	g := Grant{Subject: "ada", Issuer: "https://id.example", ClientID: "fw", RefreshToken: "rt-secret-1", Scopes: []string{"offline_access"}, Created: time.Now().UTC()}
	if _, _, err := s.Get(ctx, g.Key()); !errors.Is(err, ErrGrantNotFound) {
		t.Fatalf("Get before Put = %v", err)
	}
	rev, err := s.Put(ctx, g, 0)
	if err != nil || rev != 1 {
		t.Fatalf("Put = %d, %v", rev, err)
	}
	got, rev2, err := s.Get(ctx, g.Key())
	if err != nil || rev2 != 1 || got.RefreshToken != "rt-secret-1" || got.Scopes[0] != "offline_access" {
		t.Fatalf("Get = %+v %d %v", got, rev2, err)
	}
	// The token never appears in clear on disk, and no name gives the person away.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		if bytes.Contains(b, []byte("rt-secret")) || strings.Contains(e.Name(), "ada") {
			t.Fatalf("file %s leaks the grant", e.Name())
		}
		if info, _ := e.Info(); info.Mode().Perm() != 0o600 {
			t.Fatalf("file %s has mode %v", e.Name(), info.Mode().Perm())
		}
	}
	// Compare-and-set.
	g.RefreshToken = "rt-secret-2"
	if _, err := s.Put(ctx, g, 5); !errors.Is(err, ErrGrantConflict) {
		t.Fatalf("Put with a stale revision = %v", err)
	}
	if rev, err = s.Put(ctx, g, 1); err != nil || rev != 2 {
		t.Fatalf("Put with the revision = %d, %v", rev, err)
	}
	// Another store with the same key reads it, one with another key does not.
	if got, _, err := NewFileGrantStore(dir, testKey(7)).Get(ctx, g.Key()); err != nil || got.RefreshToken != "rt-secret-2" {
		t.Fatalf("reopen = %+v %v", got, err)
	}
	if _, _, err := NewFileGrantStore(dir, testKey(8)).Get(ctx, g.Key()); err == nil || strings.Contains(err.Error(), "rt-secret") {
		t.Fatalf("wrong key = %v", err)
	}
	other := Grant{Subject: "bob", Issuer: "https://id.example", ClientID: "fw", RefreshToken: "rt-bob"}
	if _, err := s.Put(ctx, other, 0); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(ctx, "ada")
	if err != nil || len(list) != 1 || list[0].Subject != "ada" {
		t.Fatalf("List = %+v %v", list, err)
	}
	if err := s.Delete(ctx, g.Key()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get(ctx, g.Key()); !errors.Is(err, ErrGrantNotFound) {
		t.Fatalf("Get after Delete = %v", err)
	}
}

func TestGrantKeyFile(t *testing.T) {
	dir := t.TempDir()
	load := grantKeyLoader(EnvSecrets{}, "FW_TEST_GRANT_KEY_UNSET", filepath.Join(dir, grantKeyFile))
	k1, err := load()
	if err != nil || len(k1) != 32 {
		t.Fatalf("key = %d bytes, %v", len(k1), err)
	}
	info, err := os.Stat(filepath.Join(dir, grantKeyFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file = %v, %v", info, err)
	}
	k2, _ := grantKeyLoader(EnvSecrets{}, "FW_TEST_GRANT_KEY_UNSET", filepath.Join(dir, grantKeyFile))()
	if !bytes.Equal(k1, k2) {
		t.Fatal("a second loader read another key")
	}
	t.Setenv("FW_TEST_GRANT_KEY", "from-secret")
	k3, _ := grantKeyLoader(EnvSecrets{}, "FW_TEST_GRANT_KEY", filepath.Join(dir, grantKeyFile))()
	if bytes.Equal(k1, k3) {
		t.Fatal("the secret did not win over the key file")
	}
}

func TestGrantNeverFormatsTheToken(t *testing.T) {
	g := Grant{Subject: "ada", RefreshToken: "rt-secret"}
	for _, s := range []string{g.String(), g.GoString(), g.LogValue().String()} {
		if strings.Contains(s, "rt-secret") {
			t.Fatalf("%q shows the token", s)
		}
	}
}

// fakeConnector is a tool source with one tool that acts for the person of
// the run. It connects anybody whose code is "good".
type fakeConnector struct {
	mu    sync.Mutex
	infos []RunInfo
}

func (f *fakeConnector) ConnectName() string { return "tickets" }

func (f *fakeConnector) key(subject string) GrantKey {
	return GrantKey{Subject: subject, Issuer: "https://id.example", ClientID: "fw"}
}

func (f *fakeConnector) Connection(ctx context.Context, subject string) (Connection, error) {
	c := Connection{Server: "tickets", Title: "Tickets"}
	if _, _, err := AgentFrom(ctx).Grants().Get(ctx, f.key(subject)); err == nil {
		c.Connected = true
	}
	return c, nil
}

func (f *fakeConnector) BeginConnect(_ context.Context, subject, redirect string) (ConnectStart, error) {
	return ConnectStart{Server: "tickets", URL: "https://id.example/authorize?state=s1", State: "s1", Verifier: "v", Redirect: redirect}, nil
}

func (f *fakeConnector) FinishConnect(ctx context.Context, subject string, start ConnectStart, code string) (Connection, error) {
	if code != "good" {
		return Connection{}, errors.New("bad code")
	}
	g := Grant{Subject: subject, Issuer: "https://id.example", ClientID: "fw", RefreshToken: "rt"}
	if _, err := AgentFrom(ctx).Grants().Put(ctx, g, 0); err != nil {
		return Connection{}, err
	}
	return f.Connection(ctx, subject)
}

func (f *fakeConnector) Disconnect(ctx context.Context, subject string) error {
	return AgentFrom(ctx).Grants().Delete(ctx, f.key(subject))
}

func (f *fakeConnector) Tools(context.Context) ([]Tool, error) {
	return []Tool{NewTool(ToolSpec{Name: "tickets.list", Source: "tickets"}, func(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
		info := RunInfoFrom(ctx)
		f.mu.Lock()
		f.infos = append(f.infos, info)
		f.mu.Unlock()
		if info.PrincipalKind != KindUser {
			return ErrorResult("tickets acts for a person"), nil
		}
		g := AgentFrom(ctx).Grants()
		if _, _, err := g.Get(ctx, f.key(info.Principal)); err != nil {
			if err := RequestConnect(ctx, ConnectRequest{Server: "tickets", Title: "Tickets", Reason: ConnectMissing}); err != nil {
				return ToolResult{}, err
			}
		}
		return TextResult("tickets of " + info.Principal), nil
	})}, nil
}

func TestConnectPausesAndResumes(t *testing.T) {
	ctx := testCtx(t)
	ada := &Principal{Subject: "ada", Kind: KindUser}
	pctx := WithPrincipal(ctx, ada)
	fc := &fakeConnector{}
	s := newScript(callTools(use("c1", "tickets.list", `{}`)), endTurn("done"))
	a := newTestAgent(t, s.Model(), WithToolSources(fc), WithGrantStore(NewMemGrantStore()))

	r, err := a.Run(pctx, "list my tickets")
	mustState(t, r, err, StateInputRequired)
	if r.Pending.Connect == nil || r.Pending.Connect.Server != "tickets" || r.Pending.Reason != (ConnectRequest{Title: "Tickets"}).Message() {
		t.Fatalf("pending = %+v", r.Pending)
	}
	// A click is not a grant.
	err = a.Decide(pctx, r.ID, ApprovalDecision{ApprovalID: r.Pending.ID, Decision: "allow"})
	if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "connect Tickets") {
		t.Fatalf("Decide allow = %v", err)
	}
	conns, err := a.Connections(pctx, ada)
	if err != nil || len(conns) != 1 || conns[0].Connected {
		t.Fatalf("Connections = %+v %v", conns, err)
	}
	start, err := a.BeginConnect(pctx, ada, "tickets", "https://fw.example/ui/connect/callback")
	if err != nil || start.Redirect != "https://fw.example/ui/connect/callback" {
		t.Fatalf("BeginConnect = %+v %v", start, err)
	}
	if _, err := a.FinishConnect(pctx, ada, start, "bad"); err == nil {
		t.Fatal("FinishConnect took a bad code")
	}
	conns, err = a.FinishConnect(pctx, ada, start, "good")
	if err != nil || !conns[0].Connected {
		t.Fatalf("FinishConnect = %+v %v", conns, err)
	}
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)
	if got := TextOf(lastToolResults(t, s.requests()[1])[0].Content); got != "tickets of ada" {
		t.Fatalf("tool result = %q", got)
	}
	var d ApprovalDecidedData
	firstOf(t, ledgerEvents(t, a.Ledger(), r.ID), EvApprovalDecided, &d)
	if d.Decision != "allow" || d.Decider != "ada" || d.Note != "connected" {
		t.Fatalf("decision = %+v", d)
	}
	// The ledger holds no grant.
	for _, e := range ledgerEvents(t, a.Ledger(), r.ID) {
		if bytes.Contains(e.Data, []byte(`"rt"`)) {
			t.Fatalf("event %s holds the refresh token", e.Type)
		}
	}
	if err := a.Disconnect(pctx, ada, "tickets"); err != nil {
		t.Fatal(err)
	}
	if conns, _ := a.Connections(pctx, ada); conns[0].Connected {
		t.Fatal("still connected after Disconnect")
	}
}

func TestConnectSkip(t *testing.T) {
	ctx := testCtx(t)
	pctx := WithPrincipal(ctx, &Principal{Subject: "ada", Kind: KindUser})
	s := newScript(callTools(use("c1", "tickets.list", `{}`)), endTurn("done"))
	a := newTestAgent(t, s.Model(), WithToolSources(&fakeConnector{}), WithGrantStore(NewMemGrantStore()))
	r, err := a.Run(pctx, "list my tickets")
	mustState(t, r, err, StateInputRequired)
	if err := a.Decide(pctx, r.ID, ApprovalDecision{ApprovalID: r.Pending.ID, Decision: "deny", Note: "not now"}); err != nil {
		t.Fatal(err)
	}
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)
	res := lastToolResults(t, s.requests()[1])[0]
	if !res.IsError || !strings.Contains(TextOf(res.Content), "did not connect") {
		t.Fatalf("tool result = %+v", res)
	}
}

func TestConnectNeedsAPerson(t *testing.T) {
	ctx := testCtx(t)
	a := newTestAgent(t, newScript().Model(), WithToolSources(&fakeConnector{}), WithGrantStore(NewMemGrantStore()))
	for _, p := range []*Principal{nil, {Subject: "token", Kind: KindToken}, {Subject: "spiffe://td/x", Kind: KindWorkload}} {
		if _, err := a.BeginConnect(ctx, p, "tickets", "https://fw.example/cb"); !errors.Is(err, ErrNotConnectable) {
			t.Fatalf("BeginConnect for %+v = %v", p, err)
		}
	}
	if _, err := a.BeginConnect(ctx, &Principal{Subject: "ada", Kind: KindUser}, "docs", "https://fw.example/cb"); !errors.Is(err, ErrConnectorNotFound) {
		t.Fatalf("BeginConnect for an unknown service = %v", err)
	}
}

func TestRunInfoCarriesThePerson(t *testing.T) {
	ctx := testCtx(t)
	fc := &fakeConnector{}
	s := newScript(callTools(use("c1", "tickets.list", `{}`)), endTurn("done"))
	a := newTestAgent(t, s.Model(), WithToolSources(fc), WithGrantStore(NewMemGrantStore()))
	r, err := a.Run(WithPrincipal(ctx, &Principal{Subject: "token", Kind: KindToken}), "list")
	mustState(t, r, err, StateCompleted)
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.infos) != 1 || fc.infos[0].Principal != "token" || fc.infos[0].PrincipalKind != KindToken || fc.infos[0].RunID != r.ID {
		t.Fatalf("run info = %+v", fc.infos)
	}
}

// seedConnectWait leaves a run as a process leaves it when it stops while
// a call of ada waits for a connection.
func seedConnectWait(t *testing.T, l Ledger, id string) {
	seed(t, l, id,
		MustEvent(id, EvRunQueued, RunQueuedData{Agent: "default", Input: "hi", Origin: OriginCLI, Principal: "ada", PrincipalKind: KindUser}),
		MustEvent(id, EvRunStarted, RunStartedData{}),
		MustEvent(id, EvTurnStarted, TurnData{Turn: 1}),
		MustEvent(id, EvModelRequested, ModelRequestedData{}),
		MustEvent(id, EvModelResponded, ModelRespondedData{Content: []Block{use("c1", "tickets.list", `{}`)}, StopReason: StopToolUse}),
		MustEvent(id, EvToolRequested, ToolRequestedData{Tool: "tickets.list", CallID: "c1", Input: json.RawMessage(`{}`)}),
		MustEvent(id, EvApprovalRequested, ApprovalRequestedData{ApprovalID: "ap1", Tool: "tickets.list", CallID: "c1",
			Reason: "Connect Tickets", Connect: &ConnectRequest{Server: "tickets", Title: "Tickets", Reason: ConnectMissing}}),
	)
}

func TestConnectSurvivesARestart(t *testing.T) {
	ctx := testCtx(t)
	ada := &Principal{Subject: "ada", Kind: KindUser}
	t.Run("connect after the restart runs the call once", func(t *testing.T) {
		l := NewMemLedger()
		seedConnectWait(t, l, "r1")
		s := newScript(endTurn("done"))
		a := newTestAgent(t, s.Model(), WithLedger(l), WithToolSources(&fakeConnector{}), WithGrantStore(NewMemGrantStore()))
		r, err := a.Get(ctx, "r1")
		mustState(t, r, err, StateInputRequired)
		if r.Pending.Connect == nil || r.Pending.ID != "ap1" {
			t.Fatalf("pending = %+v", r.Pending)
		}
		start, _ := a.BeginConnect(ctx, ada, "tickets", "https://fw.example/cb")
		if _, err := a.FinishConnect(ctx, ada, start, "good"); err != nil {
			t.Fatal(err)
		}
		r, err = a.Wait(ctx, "r1")
		mustState(t, r, err, StateCompleted)
		evs := ledgerEvents(t, a.Ledger(), "r1")
		// No "rerun after resume" question: the service never saw the call.
		if n := countType(evs, EvApprovalRequested); n != 1 {
			t.Fatalf("approvals = %d, want 1", n)
		}
		if got := TextOf(lastToolResults(t, s.requests()[0])[0].Content); got != "tickets of ada" {
			t.Fatalf("tool result = %q", got)
		}
	})
	t.Run("a skip before the restart ends the call", func(t *testing.T) {
		l := NewMemLedger()
		seedConnectWait(t, l, "r2")
		seed(t, l, "r2", MustEvent("r2", EvApprovalDecided, ApprovalDecidedData{ApprovalID: "ap1", Decision: "deny", Decider: "ada"}))
		s := newScript(endTurn("done"))
		a := newTestAgent(t, s.Model(), WithLedger(l), WithToolSources(&fakeConnector{}), WithGrantStore(NewMemGrantStore()))
		r, err := a.Wait(ctx, "r2")
		mustState(t, r, err, StateCompleted)
		res := lastToolResults(t, s.requests()[0])[0]
		if !res.IsError || !strings.Contains(TextOf(res.Content), "chose not to") {
			t.Fatalf("tool result = %+v", res)
		}
	})
}
