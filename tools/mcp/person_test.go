package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeTokens is the fw.PersonTokens of a test. A connected person gets the
// token "at-<subject>-<n>", and n grows with each Reject, as a refresh
// would give a new token. A person in expired gets fw.ErrConnectionExpired
// once, as a broker does when the authorization server refuses the grant.
type fakeTokens struct {
	mu        sync.Mutex
	connected map[string]bool
	expired   map[string]bool
	n         int
	rejects   int
	forgets   int
}

func newFakeTokens() *fakeTokens {
	return &fakeTokens{connected: map[string]bool{}, expired: map[string]bool{}}
}

func (f *fakeTokens) connect(subject string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connected[subject] = true
}

func (f *fakeTokens) Token(_ context.Context, subject, server string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if server != "tickets" {
		return "", fmt.Errorf("%w: no service named %s", fw.ErrNotConnected, server)
	}
	if f.expired[subject] {
		delete(f.expired, subject)
		delete(f.connected, subject)
		return "", fw.ErrConnectionExpired
	}
	if !f.connected[subject] {
		return "", fw.ErrNotConnected
	}
	return fmt.Sprintf("at-%s-%d", subject, f.n), nil
}

func (f *fakeTokens) Reject(_ context.Context, _, _, _ string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejects++
	f.n++
}

func (f *fakeTokens) Forget(_ context.Context, subject, _ string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgets++
	delete(f.connected, subject)
}

func (f *fakeTokens) counts() (rejects, forgets int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rejects, f.forgets
}

// personMCP is an MCP server that accepts the tokens of fakeTokens. Its
// tool whoami answers the subject of the token.
type personMCP struct {
	*httptest.Server

	mu       sync.Mutex
	refuse   map[string]bool   // subjects whose tokens it refuses
	sessions map[string]string // Mcp-Session-Id to subject
	shared   bool              // a session id came with two subjects
}

// subjectOf reads the subject out of a token of fakeTokens.
func subjectOf(tok string) string {
	rest, ok := strings.CutPrefix(tok, "at-")
	if !ok {
		return ""
	}
	subject, _, _ := strings.Cut(rest, "-")
	return subject
}

func newPersonMCP(t *testing.T) *personMCP {
	t.Helper()
	pm := &personMCP{refuse: map[string]bool{}, sessions: map[string]string{}}
	srv := newTestMCPServer(nil)
	sdk.AddTool(srv, &sdk.Tool{Name: "whoami", Description: "Says who calls", Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true}},
		func(_ context.Context, req *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
			subject := subjectOf(strings.TrimPrefix(req.Extra.Header.Get("Authorization"), "Bearer "))
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "you are " + subject}}}, nil, nil
		})
	h := serveHandler(srv)
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		subject := subjectOf(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		pm.mu.Lock()
		refused := subject == "" || pm.refuse[subject]
		if sid := r.Header.Get("Mcp-Session-Id"); sid != "" && subject != "" {
			if prev, seen := pm.sessions[sid]; seen && prev != subject {
				pm.shared = true
			}
			pm.sessions[sid] = subject
		}
		pm.mu.Unlock()
		if refused {
			w.Header().Set("WWW-Authenticate", `Bearer realm="tickets"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
	pm.Server = httptest.NewServer(mux)
	t.Cleanup(pm.Close)
	return pm
}

// tokenAgent is an agent that only lends its tokens of people to the tests.
// With tokens nil, it keeps none.
func tokenAgent(t *testing.T, tokens fw.PersonTokens) *fw.Agent {
	t.Helper()
	model := fw.ModelFunc{StreamFn: func(context.Context, *fw.Request, func(fw.Delta)) (*fw.Response, error) {
		return nil, errors.New("no model in this test")
	}}
	a, err := fw.New(fw.WithoutDefaults(), fw.WithAuthn(fw.NoAuthn{}), fw.WithModel(model), fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()), fw.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}
	if tokens != nil {
		a.SetPersonTokens(tokens)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	return a
}

// newPersonServer builds an entry with grant user for the service tickets.
func newPersonServer(t *testing.T, endpoint string) *Server {
	t.Helper()
	o := fw.OAuthClientConfig{Grant: fw.GrantUser}
	return newServer(t, fw.MCPServerConfig{Name: "tickets", Title: "Tickets", Endpoint: endpoint, Auth: fw.MCPAuthConfig{OAuth: &o}}, nil)
}

// asker records the connect requests and answers each with next.
type asker struct {
	mu   sync.Mutex
	reqs []fw.ConnectRequest
}

func (k *asker) requests() []fw.ConnectRequest {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.reqs)
}

func (k *asker) connecter(next func(ctx context.Context, req fw.ConnectRequest) error) fw.Connecter {
	return func(ctx context.Context, req fw.ConnectRequest) error {
		k.mu.Lock()
		k.reqs = append(k.reqs, req)
		k.mu.Unlock()
		return next(ctx, req)
	}
}

// connects connects the person, as the end of a connection in the
// workbench does.
func connects(tokens *fakeTokens, subject string) func(ctx context.Context, _ fw.ConnectRequest) error {
	return func(context.Context, fw.ConnectRequest) error {
		tokens.connect(subject)
		return nil
	}
}

func personCtx(a *fw.Agent, subject, run string, c fw.Connecter) context.Context {
	ctx := fw.WithAgent(context.Background(), a)
	ctx = fw.WithRunInfo(ctx, fw.RunInfo{RunID: run, Agent: "default", Principal: subject, PrincipalKind: fw.KindUser})
	if c != nil {
		ctx = fw.WithConnecter(ctx, c)
	}
	return ctx
}

func whoami(t *testing.T, s *Server, ctx context.Context) fw.ToolResult {
	t.Helper()
	res, err := s.Call(ctx, "whoami", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	return res
}

func TestPersonConnectsThenCalls(t *testing.T) {
	pm := newPersonMCP(t)
	tokens := newFakeTokens()
	a := tokenAgent(t, tokens)
	s := newPersonServer(t, pm.URL+"/mcp")
	k := &asker{}
	ctx := personCtx(a, "ada", "r1", k.connecter(connects(tokens, "ada")))
	if res := whoami(t, s, ctx); res.IsError || fw.TextOf(res.Content) != "you are ada" {
		t.Fatalf("whoami = %+v", res)
	}
	if reqs := k.requests(); len(reqs) != 1 || reqs[0].Reason != fw.ConnectMissing || reqs[0].Server != "tickets" || reqs[0].Title != "Tickets" {
		t.Fatalf("connect requests = %+v", reqs)
	}
	// The second call asks nobody.
	if res := whoami(t, s, ctx); fw.TextOf(res.Content) != "you are ada" {
		t.Fatalf("whoami = %+v", res)
	}
	if len(k.requests()) != 1 {
		t.Fatalf("requests = %d", len(k.requests()))
	}
}

func TestPeopleShareNothing(t *testing.T) {
	pm := newPersonMCP(t)
	tokens := newFakeTokens()
	a := tokenAgent(t, tokens)
	s := newPersonServer(t, pm.URL+"/mcp")
	s.protocol = "2025-11-25" // sessions with Mcp-Session-Id
	k := &asker{}
	for _, who := range []string{"ada", "bob", "ada", "bob"} {
		ctx := personCtx(a, who, "r-"+who, k.connecter(connects(tokens, who)))
		if res := whoami(t, s, ctx); fw.TextOf(res.Content) != "you are "+who {
			t.Fatalf("whoami for %s = %+v", who, res)
		}
	}
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if pm.shared || len(pm.sessions) != 2 {
		t.Fatalf("sessions = %v, shared %v", pm.sessions, pm.shared)
	}
	if len(k.requests()) != 2 {
		t.Fatalf("connect requests = %d, want one per person", len(k.requests()))
	}
}

func TestPlaceholderTool(t *testing.T) {
	pm := newPersonMCP(t)
	tokens := newFakeTokens()
	a := tokenAgent(t, tokens)
	s := newPersonServer(t, pm.URL+"/mcp")
	k := &asker{}
	ctx := personCtx(a, "ada", "r1", k.connecter(connects(tokens, "ada")))
	for _, c := range []context.Context{context.Background(), ctx} {
		tools, err := s.Tools(c)
		if err != nil || len(tools) != 1 || tools[0].Spec().Name != "tickets.connect" || tools[0].Spec().Title != "Connect Tickets" {
			t.Fatalf("Tools = %v, %v", toolNames(tools), err)
		}
	}
	tools, _ := s.Tools(ctx)
	res, err := tools[0].Call(ctx, json.RawMessage(`{}`))
	if err != nil || res.IsError || !strings.Contains(fw.TextOf(res.Content), "appear in the next step") {
		t.Fatalf("connect = %+v, %v", res, err)
	}
	tools, err = s.Tools(ctx)
	if err != nil || slices.Contains(toolNames(tools), "tickets.connect") || !slices.Contains(toolNames(tools), "tickets.whoami") {
		t.Fatalf("Tools after connecting = %v, %v", toolNames(tools), err)
	}
	// The list is shared: a run without a person now sees the real tools.
	if tools, _ := s.Tools(context.Background()); !slices.Contains(toolNames(tools), "tickets.whoami") {
		t.Fatalf("Tools without a person = %v", toolNames(tools))
	}
}

func TestSkipAndNoPerson(t *testing.T) {
	pm := newPersonMCP(t)
	tokens := newFakeTokens()
	a := tokenAgent(t, tokens)
	s := newPersonServer(t, pm.URL+"/mcp")
	k := &asker{}
	skip := k.connecter(func(context.Context, fw.ConnectRequest) error {
		return fmt.Errorf("%w: denied by ada", fw.ErrConnectSkipped)
	})
	for range 2 {
		res := whoami(t, s, personCtx(a, "ada", "r1", skip))
		if !res.IsError || !strings.Contains(fw.TextOf(res.Content), "chose not to connect Tickets") {
			t.Fatalf("whoami = %+v", res)
		}
	}
	if len(k.requests()) != 1 {
		t.Fatalf("a run asked %d times after a skip", len(k.requests()))
	}
	whoami(t, s, personCtx(a, "ada", "r2", skip))
	if len(k.requests()) != 2 {
		t.Fatal("another run did not ask")
	}
	// A run of the static token has no person.
	ctx := fw.WithRunInfo(fw.WithAgent(context.Background(), a), fw.RunInfo{RunID: "r3", Principal: "token", PrincipalKind: fw.KindToken})
	if res := whoami(t, s, fw.WithConnecter(ctx, skip)); !res.IsError || !strings.Contains(fw.TextOf(res.Content), "not started by a signed-in person") {
		t.Fatalf("whoami without a person = %+v", res)
	}
	// Outside a run nobody can connect.
	ctx = personCtx(a, "ada", "r4", nil)
	if res := whoami(t, s, ctx); !res.IsError || !strings.Contains(fw.TextOf(res.Content), "under Connections") {
		t.Fatalf("whoami without an asker = %+v", res)
	}
	if len(k.requests()) != 2 {
		t.Fatal("a run without a person asked")
	}
	// A process that keeps no connections of people says so.
	bare := tokenAgent(t, nil)
	if res := whoami(t, s, personCtx(bare, "ada", "r5", skip)); !res.IsError || !strings.Contains(fw.TextOf(res.Content), "no connections of people") {
		t.Fatalf("whoami without tokens = %+v", res)
	}
	tools, _ := newPersonServer(t, pm.URL+"/docs").Tools(personCtx(bare, "ada", "r5", skip))
	if res, _ := tools[0].Call(personCtx(bare, "ada", "r5", skip), nil); !res.IsError || !strings.Contains(fw.TextOf(res.Content), "no connections of people") {
		t.Fatalf("connect without tokens = %+v", res)
	}
}

func TestStillNotConnectedAsksOnce(t *testing.T) {
	pm := newPersonMCP(t)
	a := tokenAgent(t, newFakeTokens())
	s := newPersonServer(t, pm.URL+"/mcp")
	k := &asker{}
	// The person answers without connecting: one question, not a loop.
	ctx := personCtx(a, "ada", "r1", k.connecter(func(context.Context, fw.ConnectRequest) error { return nil }))
	if res := whoami(t, s, ctx); !res.IsError || !strings.Contains(fw.TextOf(res.Content), "still not connected") || len(k.requests()) != 1 {
		t.Fatalf("whoami = %+v, requests %d", res, len(k.requests()))
	}
}

func TestExpiredGrantAsksAgain(t *testing.T) {
	pm := newPersonMCP(t)
	tokens := newFakeTokens()
	tokens.connect("ada")
	tokens.expired["ada"] = true
	a := tokenAgent(t, tokens)
	s := newPersonServer(t, pm.URL+"/mcp")
	k := &asker{}
	ctx := personCtx(a, "ada", "r1", k.connecter(connects(tokens, "ada")))
	if res := whoami(t, s, ctx); fw.TextOf(res.Content) != "you are ada" {
		t.Fatalf("whoami = %+v", res)
	}
	if reqs := k.requests(); len(reqs) != 1 || reqs[0].Reason != fw.ConnectExpired {
		t.Fatalf("connect requests = %+v", reqs)
	}
}

func TestServerKeepsRefusing(t *testing.T) {
	pm := newPersonMCP(t)
	tokens := newFakeTokens()
	a := tokenAgent(t, tokens)
	s := newPersonServer(t, pm.URL+"/mcp")
	k := &asker{}
	ctx := personCtx(a, "ada", "r1", k.connecter(connects(tokens, "ada")))
	if res := whoami(t, s, ctx); res.IsError {
		t.Fatalf("whoami = %+v", res)
	}
	pm.mu.Lock()
	pm.refuse["ada"] = true
	pm.mu.Unlock()
	s.dropPerson("ada") // a new session, so the next request carries a token
	res := whoami(t, s, ctx)
	if !res.IsError || !strings.Contains(fw.TextOf(res.Content), "refused the new connection") {
		t.Fatalf("whoami = %+v", res)
	}
	// The refused token went, then the grant, and the person was asked to
	// sign in again.
	if rejects, forgets := tokens.counts(); rejects == 0 || forgets == 0 {
		t.Fatalf("rejects %d, forgets %d", rejects, forgets)
	}
	reqs := k.requests()
	if len(reqs) != 2 || reqs[1].Reason != fw.ConnectExpired {
		t.Fatalf("connect requests = %+v", reqs)
	}
	// The circuit breaker did not count the refusals of one person.
	if !s.Up() {
		t.Fatal("the server is down after the refusals of one person")
	}
}

func TestUserGrantConfig(t *testing.T) {
	ctx := context.Background()
	cfg := fw.DefaultConfig()
	cfg.Tools.MCP = []fw.MCPServerConfig{{Name: "t", Endpoint: "https://t.example.com/mcp", Auth: fw.MCPAuthConfig{OAuth: &fw.OAuthClientConfig{Grant: "user"}}}}
	// An entry with grant user needs no client here: the package that keeps
	// the connections checks the sign-in.
	srcs, err := newSources(ctx, cfg, nil)
	if err != nil || len(srcs) != 1 || !srcs[0].(*Server).personal {
		t.Fatalf("newSources = %v, %v", srcs, err)
	}
	_ = srcs[0].(*Server).Close()
	cfg.Tools.MCP[0].Auth.OAuth = &fw.OAuthClientConfig{Grant: "person"}
	if _, err := newSources(ctx, cfg, nil); err == nil || !strings.Contains(err.Error(), "not client or user") {
		t.Fatalf("bad grant = %v", err)
	}
	// A grant client entry does not act for a person.
	s := newServer(t, fw.MCPServerConfig{Name: "plain", Endpoint: "https://p.example.com/mcp"}, nil)
	if s.personal {
		t.Fatal("a plain entry acts for a person")
	}
}

// TestRunPausesUntilConnected drives a real run: the call pauses with a
// connect request, Agent.Connected resumes it once the person connected,
// and the call acts as the person.
func TestRunPausesUntilConnected(t *testing.T) {
	pm := newPersonMCP(t)
	tokens := newFakeTokens()
	s := newPersonServer(t, pm.URL+"/mcp")
	// The list is known, so the model sees the real tool.
	if err := s.refreshWith(context.Background(), func(ctx context.Context) (*sdk.ClientSession, error) {
		return s.dial(ctx, &sdk.StreamableClientTransport{Endpoint: pm.URL + "/mcp", HTTPClient: &http.Client{Transport: s.rt.base}})
	}); err == nil {
		t.Fatal("an unauthenticated listing passed the bearer check")
	}
	s.mu.Lock()
	s.tools = []*tool{s.newTool(&sdk.Tool{Name: "whoami", Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true}})}
	s.listed = true
	s.mu.Unlock()

	var mu sync.Mutex
	var results []string
	step := 0
	model := fw.ModelFunc{StreamFn: func(_ context.Context, req *fw.Request, _ func(fw.Delta)) (*fw.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		step++
		if step == 1 {
			return &fw.Response{Content: []fw.Block{{Type: fw.BlockToolUse, ID: "c1", Name: "tickets.whoami", Input: json.RawMessage(`{}`)}}, StopReason: fw.StopToolUse}, nil
		}
		last := req.Messages[len(req.Messages)-1]
		for _, b := range last.Content {
			if b.Type == fw.BlockToolResult {
				results = append(results, fw.TextOf(b.Content))
			}
		}
		return &fw.Response{Content: []fw.Block{fw.Text("done")}, StopReason: fw.StopEndTurn}, nil
	}}
	a, err := fw.New(fw.WithoutDefaults(), fw.WithAuthn(fw.NoAuthn{}), fw.WithModel(model), fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()), fw.WithToolSources(s), fw.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}
	a.SetPersonTokens(tokens)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	defer a.Close(ctx)
	ada := &fw.Principal{Subject: "ada", Kind: fw.KindUser}
	pctx := fw.WithPrincipal(ctx, ada)
	r, err := a.Run(pctx, "who am I at Tickets?")
	if err != nil || r.State != fw.StateInputRequired || r.Pending == nil || r.Pending.Connect == nil || r.Pending.Connect.Server != "tickets" {
		t.Fatalf("Run = %+v, %v", r, err)
	}
	tokens.connect("ada")
	if n := a.Connected("ada", "tickets"); n != 1 {
		t.Fatalf("Connected resumed %d runs", n)
	}
	r, err = a.Wait(ctx, r.ID)
	if err != nil || r.State != fw.StateCompleted {
		t.Fatalf("Wait = %+v, %v", r, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(results) != 1 || results[0] != "you are ada" {
		t.Fatalf("tool results = %q", results)
	}
}
