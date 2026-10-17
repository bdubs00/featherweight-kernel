package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeIdP is an authorization server with refresh token rotation. A
// refresh token that comes back after it was rotated revokes its whole
// family, as the strictest servers do.
type fakeIdP struct {
	*httptest.Server
	t        *testing.T
	clientID string
	secret   string

	refreshDelay   time.Duration
	noRefreshToken bool

	mu          sync.Mutex
	n           int
	codes       map[string]codeGrant
	families    map[string]*family
	rts         map[string]string // refresh token to family id
	access      map[string]accessInfo
	refreshes   int
	exchanges   int
	revocations int
	forms       []url.Values
}

type codeGrant struct {
	subject, challenge, redirect, nonce string
}

type family struct {
	subject string
	current string
	revoked bool
}

type accessInfo struct{ subject, aud string }

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	idp := &fakeIdP{t: t, clientID: "fw", secret: "oidc-secret",
		codes: map[string]codeGrant{}, families: map[string]*family{}, rts: map[string]string{}, access: map[string]accessInfo{}}
	mux := http.NewServeMux()
	meta := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                idp.URL,
			"authorization_endpoint":                idp.URL + "/authorize",
			"token_endpoint":                        idp.URL + "/token",
			"revocation_endpoint":                   idp.URL + "/revoke",
			"token_endpoint_auth_methods_supported": []string{"client_secret_post", "client_secret_basic"},
		})
	}
	mux.HandleFunc("GET /.well-known/openid-configuration", meta)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", meta)
	mux.HandleFunc("POST /token", idp.token)
	mux.HandleFunc("POST /revoke", idp.revoke)
	idp.Server = httptest.NewServer(mux)
	t.Cleanup(idp.Close)
	return idp
}

func (idp *fakeIdP) fail(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": desc})
}

// issue makes an access token for a subject and an audience. The caller
// holds idp.mu.
func (idp *fakeIdP) issue(subject, aud string) string {
	idp.n++
	tok := fmt.Sprintf("at-%d-%s", idp.n, subject)
	idp.access[tok] = accessInfo{subject: subject, aud: aud}
	return tok
}

// rotate gives a family a new refresh token. The caller holds idp.mu.
func (idp *fakeIdP) rotate(id string) string {
	idp.n++
	rt := fmt.Sprintf("rt-%d", idp.n)
	idp.families[id].current = rt
	idp.rts[rt] = id
	return rt
}

func unsignedJWT(claims map[string]any) string {
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	b, _ := json.Marshal(claims)
	return h + "." + base64.RawURLEncoding.EncodeToString(b) + ".sig"
}

func (idp *fakeIdP) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		idp.fail(w, 400, "invalid_request", err.Error())
		return
	}
	f := r.PostForm
	id, secret := f.Get("client_id"), f.Get("client_secret")
	if u, p, ok := r.BasicAuth(); ok {
		id, secret = u, p
	}
	idp.mu.Lock()
	idp.forms = append(idp.forms, f)
	idp.mu.Unlock()
	if id != idp.clientID || secret != idp.secret {
		idp.fail(w, 401, "invalid_client", "bad client")
		return
	}
	aud := "base"
	if res := f["resource"]; len(res) == 1 {
		aud = res[0]
	}
	answer := map[string]any{"token_type": "Bearer", "expires_in": 300}
	switch f.Get("grant_type") {
	case grantCode:
		idp.mu.Lock()
		cg, ok := idp.codes[f.Get("code")]
		delete(idp.codes, f.Get("code"))
		if !ok || pkceChallenge(f.Get("code_verifier")) != cg.challenge || f.Get("redirect_uri") != cg.redirect {
			idp.mu.Unlock()
			idp.fail(w, 400, "invalid_grant", "bad code")
			return
		}
		idp.n++
		fid := fmt.Sprintf("f%d", idp.n)
		idp.families[fid] = &family{subject: cg.subject}
		if !idp.noRefreshToken {
			answer["refresh_token"] = idp.rotate(fid)
		}
		answer["access_token"] = idp.issue(cg.subject, aud)
		answer["id_token"] = unsignedJWT(map[string]any{"iss": idp.URL, "aud": idp.clientID, "sub": cg.subject, "nonce": cg.nonce, "preferred_username": cg.subject + "@example.com"})
		answer["scope"] = "openid offline_access"
		idp.mu.Unlock()
	case grantRefresh:
		if idp.refreshDelay > 0 {
			time.Sleep(idp.refreshDelay)
		}
		idp.mu.Lock()
		idp.refreshes++
		rt := f.Get("refresh_token")
		fid, ok := idp.rts[rt]
		fam := idp.families[fid]
		switch {
		case !ok || fam.revoked:
			idp.mu.Unlock()
			idp.fail(w, 400, "invalid_grant", "Token is not active")
			return
		case fam.current != rt:
			fam.revoked = true
			idp.mu.Unlock()
			idp.fail(w, 400, "invalid_grant", "Maximum allowed refresh token reuse exceeded")
			return
		}
		answer["refresh_token"] = idp.rotate(fid)
		answer["access_token"] = idp.issue(fam.subject, aud)
		idp.mu.Unlock()
	case grantTokenExchange:
		idp.mu.Lock()
		idp.exchanges++
		ai, ok := idp.access[f.Get("subject_token")]
		if !ok || ai.aud != "base" || f.Get("subject_token_type") != tokenTypeAccess {
			idp.mu.Unlock()
			idp.fail(w, 400, "invalid_grant", "bad subject token")
			return
		}
		answer["access_token"] = idp.issue(ai.subject, f.Get("audience"))
		answer["issued_token_type"] = tokenTypeAccess
		idp.mu.Unlock()
	default:
		idp.fail(w, 400, "unsupported_grant_type", f.Get("grant_type"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(answer)
}

func (idp *fakeIdP) revoke(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	idp.mu.Lock()
	defer idp.mu.Unlock()
	idp.revocations++
	if fid, ok := idp.rts[r.PostForm.Get("token")]; ok {
		idp.families[fid].revoked = true
	}
}

// authorize plays the browser: the person signs in as subject and the
// server answers with a code.
func (idp *fakeIdP) authorize(t *testing.T, raw, subject string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil || !strings.HasPrefix(raw, idp.URL+"/authorize?") {
		t.Fatalf("authorization URL %q", raw)
	}
	q := u.Query()
	if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("state") == "" || q.Get("client_id") != idp.clientID {
		t.Fatalf("authorization request %v", q)
	}
	idp.mu.Lock()
	defer idp.mu.Unlock()
	idp.n++
	code := fmt.Sprintf("code-%d", idp.n)
	idp.codes[code] = codeGrant{subject: subject, challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri"), nonce: q.Get("nonce")}
	return code
}

func (idp *fakeIdP) counts() (refreshes, exchanges, revocations int) {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	return idp.refreshes, idp.exchanges, idp.revocations
}

func (idp *fakeIdP) lookup(tok string) (accessInfo, bool) {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	ai, ok := idp.access[tok]
	return ai, ok
}

// personMCP is an MCP server that accepts a token of the fake IdP for one
// audience. Its tool whoami answers the subject of the token.
type personMCP struct {
	*httptest.Server
	idp *fakeIdP
	aud string // the audience that a token needs; "" means the URL of /mcp

	mu       sync.Mutex
	refuse   map[string]bool   // subjects whose tokens it refuses
	sessions map[string]string // Mcp-Session-Id to subject
	shared   bool              // a session id came with two subjects
}

func newPersonMCP(t *testing.T, idp *fakeIdP, metaIssuer string) *personMCP {
	t.Helper()
	pm := &personMCP{idp: idp, refuse: map[string]bool{}, sessions: map[string]string{}}
	srv := newTestMCPServer(nil)
	sdk.AddTool(srv, &sdk.Tool{Name: "whoami", Description: "Says who calls", Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true}},
		func(_ context.Context, req *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
			tok := strings.TrimPrefix(req.Extra.Header.Get("Authorization"), "Bearer ")
			ai, _ := idp.lookup(tok)
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "you are " + ai.subject}}}, nil, nil
		})
	h := serveHandler(srv)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"resource": pm.URL + "/mcp", "authorization_servers": []string{metaIssuer}})
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		ai, ok := idp.lookup(tok)
		want := pm.aud
		if want == "" {
			want = pm.URL + "/mcp"
		}
		pm.mu.Lock()
		refused := pm.refuse[ai.subject]
		if sid := r.Header.Get("Mcp-Session-Id"); sid != "" && ok {
			if prev, seen := pm.sessions[sid]; seen && prev != ai.subject {
				pm.shared = true
			}
			pm.sessions[sid] = ai.subject
		}
		pm.mu.Unlock()
		if !ok || ai.aud != want || refused {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource/mcp"`, pm.URL))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
	pm.Server = httptest.NewServer(mux)
	t.Cleanup(pm.Close)
	return pm
}

// grantAgent is an agent that only lends its grant store to the tests.
func grantAgent(t *testing.T, store fw.GrantStore) *fw.Agent {
	t.Helper()
	model := fw.ModelFunc{StreamFn: func(context.Context, *fw.Request, func(fw.Delta)) (*fw.Response, error) {
		return nil, errors.New("no model in this test")
	}}
	a, err := fw.New(fw.WithoutDefaults(), fw.WithAuthn(fw.NoAuthn{}), fw.WithModel(model), fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()), fw.WithGrantStore(store), fw.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	return a
}

func userSecrets() fw.Secrets {
	return mapSecrets(map[string]string{"OIDC_SECRET": "oidc-secret", "OWN_SECRET": "oidc-secret"})
}

// newPersonServer builds an entry with grant user against the fake IdP.
// Without a clientId in o, the entry uses the sign-in client.
func newPersonServer(t *testing.T, name, endpoint string, idp *fakeIdP, o fw.OAuthClientConfig, b *broker) *Server {
	t.Helper()
	o.Grant = fw.GrantUser
	s := newServer(t, fw.MCPServerConfig{Name: name, Title: strings.ToUpper(name[:1]) + name[1:], Endpoint: endpoint, Auth: fw.MCPAuthConfig{OAuth: &o}}, userSecrets())
	if err := s.useUserGrant(&fw.OIDCConfig{Issuer: idp.URL, ClientID: "fw", ClientSecret: "OIDC_SECRET"}, b); err != nil {
		t.Fatal(err)
	}
	return s
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

// browse connects the person as signedInAs, as the callback of a surface
// does.
func browse(t *testing.T, s *Server, idp *fakeIdP, subject, signedInAs string) func(ctx context.Context, _ fw.ConnectRequest) error {
	return func(ctx context.Context, _ fw.ConnectRequest) error {
		start, err := s.BeginConnect(ctx, subject, "https://fw.example/ui/connect/callback")
		if err != nil {
			return err
		}
		_, err = s.FinishConnect(ctx, subject, start, idp.authorize(t, start.URL, signedInAs))
		return err
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
	idp := newFakeIdP(t)
	pm := newPersonMCP(t, idp, idp.URL)
	store := fw.NewMemGrantStore()
	a := grantAgent(t, store)
	s := newPersonServer(t, "tickets", pm.URL+"/mcp", idp, fw.OAuthClientConfig{}, newBroker())
	k := &asker{}
	ctx := personCtx(a, "ada", "r1", k.connecter(browse(t, s, idp, "ada", "ada")))

	if s.ConnectName() != "tickets" {
		t.Fatalf("ConnectName = %q", s.ConnectName())
	}
	c, err := s.Connection(ctx, "ada")
	if err != nil || c.Connected || !c.SignIn || c.Issuer != idp.URL || c.Title != "Tickets" {
		t.Fatalf("Connection before = %+v, %v", c, err)
	}
	if res := whoami(t, s, ctx); res.IsError || fw.TextOf(res.Content) != "you are ada" {
		t.Fatalf("whoami = %+v", res)
	}
	if reqs := k.requests(); len(reqs) != 1 || reqs[0].Reason != fw.ConnectMissing || reqs[0].Server != "tickets" || reqs[0].Title != "Tickets" {
		t.Fatalf("connect requests = %+v", reqs)
	}
	c, _ = s.Connection(ctx, "ada")
	if !c.Connected || c.Account != "ada@example.com" || c.Since.IsZero() {
		t.Fatalf("Connection after = %+v", c)
	}
	// The second call uses the cached token: no refresh, no question.
	refreshes, _, _ := idp.counts()
	if res := whoami(t, s, ctx); fw.TextOf(res.Content) != "you are ada" {
		t.Fatalf("whoami = %+v", res)
	}
	if r2, _, _ := idp.counts(); r2 != refreshes || len(k.requests()) != 1 {
		t.Fatalf("refreshes %d -> %d, requests %d", refreshes, r2, len(k.requests()))
	}
	// Each token request carried the resource of the MCP server.
	for _, f := range idp.forms {
		if f.Get("grant_type") == grantRefresh && f.Get("resource") != pm.URL+"/mcp" {
			t.Fatalf("refresh without the resource: %v", f)
		}
	}
}

func TestAuthorizationRequest(t *testing.T) {
	idp := newFakeIdP(t)
	pm := newPersonMCP(t, idp, idp.URL)
	a := grantAgent(t, fw.NewMemGrantStore())
	b := newBroker()
	s := newPersonServer(t, "tickets", pm.URL+"/mcp", idp, fw.OAuthClientConfig{Scopes: []string{"tickets.read"}, AuthParams: map[string]string{"prompt": "consent", "state": "evil"}}, b)
	newPersonServer(t, "docs", pm.URL+"/docs", idp, fw.OAuthClientConfig{}, b)
	start, err := s.BeginConnect(personCtx(a, "ada", "r1", nil), "ada", "https://fw.example/ui/connect/callback")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(start.URL)
	q := u.Query()
	if q.Get("scope") != "tickets.read openid offline_access" || q.Get("prompt") != "consent" || q.Get("state") != start.State ||
		q.Get("code_challenge") != pkceChallenge(start.Verifier) || q.Get("nonce") != start.Nonce || q.Get("redirect_uri") != start.Redirect {
		t.Fatalf("authorization request = %v", q)
	}
	// One connection of the sign-in client covers both entries.
	if res := q["resource"]; len(res) != 2 {
		t.Fatalf("resources = %v", res)
	}
	if start.Server != "tickets" || time.Until(start.Expires) < 9*time.Minute {
		t.Fatalf("start = %+v", start)
	}
}

func TestPeopleShareNothing(t *testing.T) {
	idp := newFakeIdP(t)
	pm := newPersonMCP(t, idp, idp.URL)
	a := grantAgent(t, fw.NewMemGrantStore())
	s := newPersonServer(t, "tickets", pm.URL+"/mcp", idp, fw.OAuthClientConfig{}, newBroker())
	s.protocol = "2025-11-25" // sessions with Mcp-Session-Id
	k := &asker{}
	for _, who := range []string{"ada", "bob", "ada", "bob"} {
		ctx := personCtx(a, who, "r-"+who, k.connecter(browse(t, s, idp, who, who)))
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

func TestConcurrentCallsRefreshOnce(t *testing.T) {
	idp := newFakeIdP(t)
	pm := newPersonMCP(t, idp, idp.URL)
	a := grantAgent(t, fw.NewMemGrantStore())
	b := newBroker()
	s := newPersonServer(t, "tickets", pm.URL+"/mcp", idp, fw.OAuthClientConfig{}, b)
	ctx := personCtx(a, "ada", "r1", (&asker{}).connecter(browse(t, s, idp, "ada", "ada")))
	if res := whoami(t, s, ctx); res.IsError {
		t.Fatalf("whoami = %+v", res)
	}
	// Forget the access tokens, so every call needs a refresh.
	b.mu.Lock()
	clear(b.tokens)
	b.mu.Unlock()
	before, _, _ := idp.counts()
	idp.refreshDelay = 50 * time.Millisecond
	var wg sync.WaitGroup
	var bad atomic.Int32
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := s.Call(ctx, "whoami", json.RawMessage(`{}`))
			if err != nil || fw.TextOf(res.Content) != "you are ada" {
				bad.Add(1)
			}
		}()
	}
	wg.Wait()
	after, _, _ := idp.counts()
	if bad.Load() != 0 || after-before != 1 {
		t.Fatalf("bad calls %d, refreshes %d, want 1", bad.Load(), after-before)
	}
	// The store holds the rotated refresh token, the one the server knows.
	gs, _ := a.Grants().List(ctx, "ada")
	idp.mu.Lock()
	defer idp.mu.Unlock()
	if len(gs) != 1 || idp.families[idp.rts[gs[0].RefreshToken]].current != gs[0].RefreshToken || gs[0].Refreshed.IsZero() {
		t.Fatalf("stored grant is not the current one")
	}
}

// flakyStore loses the next Put, as a process that stops between the
// answer of the server and the write does.
type flakyStore struct {
	*fw.MemGrantStore
	lose atomic.Bool
}

func (f *flakyStore) Put(ctx context.Context, g fw.Grant, prev uint64) (uint64, error) {
	if f.lose.Swap(false) {
		return 0, errors.New("disk full")
	}
	return f.MemGrantStore.Put(ctx, g, prev)
}

func TestLostRefreshAsksAgainOnce(t *testing.T) {
	idp := newFakeIdP(t)
	pm := newPersonMCP(t, idp, idp.URL)
	store := &flakyStore{MemGrantStore: fw.NewMemGrantStore()}
	a := grantAgent(t, store)
	b := newBroker()
	s := newPersonServer(t, "tickets", pm.URL+"/mcp", idp, fw.OAuthClientConfig{}, b)
	k := &asker{}
	ctx := personCtx(a, "ada", "r1", k.connecter(browse(t, s, idp, "ada", "ada")))
	if res := whoami(t, s, ctx); res.IsError {
		t.Fatalf("whoami = %+v", res)
	}
	// The refresh succeeds at the server, but the new refresh token never
	// reaches the store.
	b.mu.Lock()
	clear(b.tokens)
	b.mu.Unlock()
	store.lose.Store(true)
	if res := whoami(t, s, ctx); !res.IsError || !strings.Contains(fw.TextOf(res.Content), "store the refreshed grant") {
		t.Fatalf("whoami with a lost write = %+v", res)
	}
	// A new process starts with the old refresh token on disk. The server
	// sees it again, revokes the grant, and the person connects once more.
	s2 := newPersonServer(t, "tickets", pm.URL+"/mcp", idp, fw.OAuthClientConfig{}, newBroker())
	k2 := &asker{}
	ctx2 := personCtx(a, "ada", "r1", k2.connecter(browse(t, s2, idp, "ada", "ada")))
	if res := whoami(t, s2, ctx2); fw.TextOf(res.Content) != "you are ada" {
		t.Fatalf("whoami after the restart = %+v", res)
	}
	if reqs := k2.requests(); len(reqs) != 1 || reqs[0].Reason != fw.ConnectExpired {
		t.Fatalf("connect requests = %+v", reqs)
	}
	// A person who answers without connecting gets one question, not a loop.
	b3 := newBroker()
	s3 := newPersonServer(t, "tickets", pm.URL+"/mcp", idp, fw.OAuthClientConfig{}, b3)
	if err := s3.Disconnect(personCtx(a, "ada", "r2", nil), "ada"); err != nil {
		t.Fatal(err)
	}
	k3 := &asker{}
	ctx3 := personCtx(a, "ada", "r2", k3.connecter(func(context.Context, fw.ConnectRequest) error { return nil }))
	if res := whoami(t, s3, ctx3); !res.IsError || !strings.Contains(fw.TextOf(res.Content), "still not connected") || len(k3.requests()) != 1 {
		t.Fatalf("whoami = %+v, requests %d", res, len(k3.requests()))
	}
}

func TestExchangeMode(t *testing.T) {
	idp := newFakeIdP(t)
	tickets := newPersonMCP(t, idp, idp.URL)
	tickets.aud = "tickets-mcp"
	docs := newPersonMCP(t, idp, idp.URL)
	docs.aud = "docs-mcp"
	a := grantAgent(t, fw.NewMemGrantStore())
	b := newBroker()
	st := newPersonServer(t, "tickets", tickets.URL+"/mcp", idp, fw.OAuthClientConfig{Exchange: &fw.TokenExchangeConfig{Audience: "tickets-mcp"}}, b)
	sd := newPersonServer(t, "docs", docs.URL+"/mcp", idp, fw.OAuthClientConfig{Exchange: &fw.TokenExchangeConfig{Audience: "docs-mcp"}}, b)
	k := &asker{}
	ctx := personCtx(a, "ada", "r1", k.connecter(browse(t, st, idp, "ada", "ada")))
	if res := whoami(t, st, ctx); fw.TextOf(res.Content) != "you are ada" {
		t.Fatalf("tickets whoami = %+v", res)
	}
	if res := whoami(t, sd, ctx); fw.TextOf(res.Content) != "you are ada" {
		t.Fatalf("docs whoami = %+v", res)
	}
	// One connection serves both, the code gave the base token, and each
	// entry exchanged it once.
	refreshes, exchanges, _ := idp.counts()
	if len(k.requests()) != 1 || refreshes != 0 || exchanges != 2 {
		t.Fatalf("requests %d, refreshes %d, exchanges %d", len(k.requests()), refreshes, exchanges)
	}
	for _, f := range idp.forms {
		if f.Get("grant_type") == grantTokenExchange && f.Get("resource") != "" {
			t.Fatalf("an exchange with an audience sent resource: %v", f)
		}
	}
}

func TestPlaceholderTool(t *testing.T) {
	idp := newFakeIdP(t)
	pm := newPersonMCP(t, idp, idp.URL)
	a := grantAgent(t, fw.NewMemGrantStore())
	s := newPersonServer(t, "tickets", pm.URL+"/mcp", idp, fw.OAuthClientConfig{}, newBroker())
	k := &asker{}
	ctx := personCtx(a, "ada", "r1", k.connecter(browse(t, s, idp, "ada", "ada")))
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
	idp := newFakeIdP(t)
	pm := newPersonMCP(t, idp, idp.URL)
	a := grantAgent(t, fw.NewMemGrantStore())
	s := newPersonServer(t, "tickets", pm.URL+"/mcp", idp, fw.OAuthClientConfig{}, newBroker())
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
}

func TestServerKeepsRefusing(t *testing.T) {
	idp := newFakeIdP(t)
	pm := newPersonMCP(t, idp, idp.URL)
	store := fw.NewMemGrantStore()
	a := grantAgent(t, store)
	s := newPersonServer(t, "tickets", pm.URL+"/mcp", idp, fw.OAuthClientConfig{}, newBroker())
	k := &asker{}
	ctx := personCtx(a, "ada", "r1", k.connecter(browse(t, s, idp, "ada", "ada")))
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
	reqs := k.requests()
	if len(reqs) != 2 || reqs[1].Reason != fw.ConnectExpired {
		t.Fatalf("connect requests = %+v", reqs)
	}
	// The circuit breaker did not count the refusals of one person.
	if !s.Up() {
		t.Fatal("the server is down after the refusals of one person")
	}
}

func TestFinishConnectChecks(t *testing.T) {
	idp := newFakeIdP(t)
	pm := newPersonMCP(t, idp, idp.URL)
	store := fw.NewMemGrantStore()
	a := grantAgent(t, store)
	s := newPersonServer(t, "tickets", pm.URL+"/mcp", idp, fw.OAuthClientConfig{}, newBroker())
	ctx := personCtx(a, "ada", "r1", nil)
	err := browse(t, s, idp, "ada", "mallory")(ctx, fw.ConnectRequest{})
	if err == nil || !strings.Contains(err.Error(), "another account") {
		t.Fatalf("FinishConnect as another person = %v", err)
	}
	if gs, _ := store.List(ctx, "ada"); len(gs) != 0 {
		t.Fatalf("grants = %v", gs)
	}
	start, _ := s.BeginConnect(ctx, "ada", "https://fw.example/cb")
	start.Expires = time.Now().Add(-time.Second)
	if _, err := s.FinishConnect(ctx, "ada", start, idp.authorize(t, start.URL, "ada")); err == nil || !strings.Contains(err.Error(), "longer than") {
		t.Fatalf("FinishConnect after the expiry = %v", err)
	}
	start, _ = s.BeginConnect(ctx, "ada", "https://fw.example/cb")
	start.Verifier = "wrong"
	if _, err := s.FinishConnect(ctx, "ada", start, idp.authorize(t, start.URL, "ada")); err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("FinishConnect with a wrong verifier = %v", err)
	}
	idp.noRefreshToken = true
	if err := browse(t, s, idp, "ada", "ada")(ctx, fw.ConnectRequest{}); err == nil || !strings.Contains(err.Error(), "no refresh token") {
		t.Fatalf("FinishConnect without a refresh token = %v", err)
	}
}

func TestDisconnectRevokes(t *testing.T) {
	idp := newFakeIdP(t)
	pm := newPersonMCP(t, idp, idp.URL)
	store := fw.NewMemGrantStore()
	a := grantAgent(t, store)
	s := newPersonServer(t, "tickets", pm.URL+"/mcp", idp, fw.OAuthClientConfig{}, newBroker())
	ctx := personCtx(a, "ada", "r1", nil)
	if err := browse(t, s, idp, "ada", "ada")(ctx, fw.ConnectRequest{}); err != nil {
		t.Fatal(err)
	}
	gs, _ := store.List(ctx, "ada")
	if len(gs) != 1 {
		t.Fatalf("grants = %v", gs)
	}
	rt := gs[0].RefreshToken
	if err := s.Disconnect(ctx, "ada"); err != nil {
		t.Fatal(err)
	}
	if gs, _ := store.List(ctx, "ada"); len(gs) != 0 {
		t.Fatalf("grants after Disconnect = %v", gs)
	}
	_, _, revocations := idp.counts()
	idp.mu.Lock()
	revoked := idp.families[idp.rts[rt]].revoked
	idp.mu.Unlock()
	if revocations != 1 || !revoked {
		t.Fatalf("revocations %d, revoked %v", revocations, revoked)
	}
}

func TestInvalidClientKeepsTheGrant(t *testing.T) {
	idp := newFakeIdP(t)
	pm := newPersonMCP(t, idp, idp.URL)
	store := fw.NewMemGrantStore()
	a := grantAgent(t, store)
	b := newBroker()
	s := newPersonServer(t, "tickets", pm.URL+"/mcp", idp, fw.OAuthClientConfig{}, b)
	ctx := personCtx(a, "ada", "r1", nil)
	if err := browse(t, s, idp, "ada", "ada")(ctx, fw.ConnectRequest{}); err != nil {
		t.Fatal(err)
	}
	idp.secret = "rotated"
	res := whoami(t, s, ctx)
	if !res.IsError || !strings.Contains(fw.TextOf(res.Content), "invalid_client") {
		t.Fatalf("whoami = %+v", res)
	}
	if gs, _ := store.List(ctx, "ada"); len(gs) != 1 {
		t.Fatal("a refused client deleted the grant of the person")
	}
}

func TestOwnClientDiscovered(t *testing.T) {
	signIn := newFakeIdP(t)
	other := newFakeIdP(t)
	other.clientID, other.secret = "tickets-client", "oidc-secret"
	pm := newPersonMCP(t, other, other.URL)
	a := grantAgent(t, fw.NewMemGrantStore())
	s := newPersonServer(t, "tickets", pm.URL+"/mcp", signIn, fw.OAuthClientConfig{ClientID: "tickets-client", ClientSecret: "OWN_SECRET"}, newBroker())
	k := &asker{}
	ctx := personCtx(a, "ada", "r1", k.connecter(browse(t, s, other, "ada", "ada-at-tickets")))
	if res := whoami(t, s, ctx); fw.TextOf(res.Content) != "you are ada-at-tickets" {
		t.Fatalf("whoami = %+v", res)
	}
	c, _ := s.Connection(ctx, "ada")
	if !c.Connected || c.SignIn || c.Issuer != other.URL {
		t.Fatalf("Connection = %+v", c)
	}
	start, _ := s.BeginConnect(ctx, "ada", "https://fw.example/cb")
	if q, _ := url.Parse(start.URL); strings.Contains(q.Query().Get("scope"), "openid") || start.Nonce != "" {
		t.Fatalf("a foreign server got openid: %s", start.URL)
	}
}

func TestUserGrantConfig(t *testing.T) {
	ctx := context.Background()
	base := func() *fw.Config {
		cfg := fw.DefaultConfig()
		cfg.Auth.Mode = "oidc"
		cfg.Auth.OIDC = &fw.OIDCConfig{Issuer: "https://id.example.com", ClientID: "fw"}
		cfg.Tools.MCP = []fw.MCPServerConfig{{Name: "t", Endpoint: "https://t.example.com/mcp", Auth: fw.MCPAuthConfig{OAuth: &fw.OAuthClientConfig{Grant: "user"}}}}
		return cfg
	}
	srcs, err := newSources(ctx, base(), nil)
	if err != nil || len(srcs) != 1 || srcs[0].(*Server).ConnectName() != "t" {
		t.Fatalf("newSources = %v, %v", srcs, err)
	}
	_ = srcs[0].(*Server).Close()

	cfg := base()
	cfg.Auth.Mode = "token"
	if _, err := newSources(ctx, cfg, nil); err == nil || !strings.Contains(err.Error(), "oidc") {
		t.Fatalf("token mode = %v", err)
	}
	cfg = base()
	cfg.Auth.OIDC.ClientID = ""
	if _, err := newSources(ctx, cfg, nil); err == nil || !strings.Contains(err.Error(), "auth.oidc.clientId") {
		t.Fatalf("no sign-in client = %v", err)
	}
	cfg = base()
	cfg.Tools.MCP[0].Auth.OAuth = &fw.OAuthClientConfig{Grant: "person"}
	if _, err := newSources(ctx, cfg, nil); err == nil || !strings.Contains(err.Error(), "not client or user") {
		t.Fatalf("bad grant = %v", err)
	}
	// An entry of its own needs no client secret: a public client is fine.
	cfg = base()
	cfg.Tools.MCP[0].Auth.OAuth = &fw.OAuthClientConfig{Grant: "user", ClientID: "pub", Issuer: "https://other.example.com"}
	srcs, err = newSources(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("public client = %v", err)
	}
	_ = srcs[0].(*Server).Close()
	// A grant client entry is not a connector.
	s := newServer(t, fw.MCPServerConfig{Name: "plain", Endpoint: "https://p.example.com/mcp"}, nil)
	if s.ConnectName() != "" {
		t.Fatal("a plain entry acts for a person")
	}
}

func TestNoTokenInLogsOrErrors(t *testing.T) {
	idp := newFakeIdP(t)
	pm := newPersonMCP(t, idp, idp.URL)
	store := &flakyStore{MemGrantStore: fw.NewMemGrantStore()}
	a := grantAgent(t, store)
	var logs strings.Builder
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return logs.Write(p)
	}), &slog.HandlerOptions{Level: slog.LevelDebug}))
	o := fw.OAuthClientConfig{Grant: fw.GrantUser}
	s, err := New(context.Background(), fw.MCPServerConfig{Name: "tickets", Endpoint: pm.URL + "/mcp", Auth: fw.MCPAuthConfig{OAuth: &o}}, userSecrets(), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	b := newBroker()
	if err := s.useUserGrant(&fw.OIDCConfig{Issuer: idp.URL, ClientID: "fw", ClientSecret: "OIDC_SECRET"}, b); err != nil {
		t.Fatal(err)
	}
	ctx := personCtx(a, "ada", "r1", (&asker{}).connecter(browse(t, s, idp, "ada", "ada")))
	var texts []string
	texts = append(texts, fw.TextOf(whoami(t, s, ctx).Content))
	b.mu.Lock()
	clear(b.tokens)
	b.mu.Unlock()
	store.lose.Store(true)
	texts = append(texts, fw.TextOf(whoami(t, s, ctx).Content))
	_ = s.Disconnect(ctx, "ada")
	mu.Lock()
	texts = append(texts, logs.String())
	mu.Unlock()
	for _, text := range texts {
		for _, secret := range []string{"rt-", "at-", "oidc-secret", "code-"} {
			if strings.Contains(text, secret) {
				t.Fatalf("%q shows %q", text, secret)
			}
		}
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

var _ io.Writer = writerFunc(nil)

// TestRunPausesUntilConnected drives a real run: the call pauses with a
// connect request, Agent.FinishConnect resumes it, and the call acts as
// the person.
func TestRunPausesUntilConnected(t *testing.T) {
	idp := newFakeIdP(t)
	pm := newPersonMCP(t, idp, idp.URL)
	s := newPersonServer(t, "tickets", pm.URL+"/mcp", idp, fw.OAuthClientConfig{}, newBroker())
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
		fw.WithBus(fw.NewMemBus()), fw.WithGrantStore(fw.NewMemGrantStore()), fw.WithToolSources(s),
		fw.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	defer a.Close(ctx)
	ada := &fw.Principal{Subject: "ada", Kind: fw.KindUser}
	pctx := fw.WithPrincipal(ctx, ada)
	r, err := a.Run(pctx, "who am I at Tickets?")
	if err != nil || r.State != fw.StateInputRequired || r.Pending == nil || r.Pending.Connect == nil || r.Pending.Connect.Server != "tickets" {
		t.Fatalf("Run = %+v, %v", r, err)
	}
	conns, err := a.Connections(pctx, ada)
	if err != nil || len(conns) != 1 || conns[0].Connected {
		t.Fatalf("Connections = %+v, %v", conns, err)
	}
	start, err := a.BeginConnect(pctx, ada, "tickets", "https://fw.example/ui/connect/callback")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.FinishConnect(pctx, ada, start, idp.authorize(t, start.URL, "ada")); err != nil {
		t.Fatal(err)
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
