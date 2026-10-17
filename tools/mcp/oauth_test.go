package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// authServerStub is an OAuth authorization server with RFC 8414 metadata and
// a token endpoint. Each token is "tok-<n>:<scope>".
type authServerStub struct {
	*httptest.Server
	t         *testing.T
	expiresIn int

	mu    sync.Mutex
	forms []url.Values
}

func newAuthServer(t *testing.T) *authServerStub {
	t.Helper()
	as := &authServerStub{t: t, expiresIn: 3600}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                as.URL,
			"token_endpoint":        as.URL + "/token",
			"grant_types_supported": []string{"client_credentials"},
		})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		as.mu.Lock()
		as.forms = append(as.forms, r.PostForm)
		n := len(as.forms)
		as.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.PostForm.Get("client_secret") != "hunter2" {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client", "error_description": "bad secret"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": fmt.Sprintf("tok-%d:%s", n, r.PostForm.Get("scope")),
			"token_type":   "Bearer",
			"expires_in":   as.expiresIn,
		})
	})
	as.Server = httptest.NewServer(mux)
	t.Cleanup(as.Close)
	return as
}

func (as *authServerStub) requests() []url.Values {
	as.mu.Lock()
	defer as.mu.Unlock()
	return append([]url.Values(nil), as.forms...)
}

// resourceServer serves the test MCP server at /mcp behind a bearer check,
// with RFC 9728 metadata. need lists scopes that every token must carry.
type resourceServer struct {
	*httptest.Server
	need []string

	mu           sync.Mutex
	unauthorized int
	forbidden    int
	tokens       []string
}

func newResourceServer(t *testing.T, as *authServerStub, need ...string) *resourceServer {
	t.Helper()
	rs := &resourceServer{need: need}
	mcpHandler := serveHandler(newTestMCPServer(nil))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resource":              rs.URL + "/mcp",
			"authorization_servers": []string{as.URL},
		})
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		tok, ok := strings.CutPrefix(auth, "Bearer tok-")
		if !ok {
			rs.mu.Lock()
			rs.unauthorized++
			rs.mu.Unlock()
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource/mcp", scope="ignored"`, rs.URL))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		rs.mu.Lock()
		rs.tokens = append(rs.tokens, auth)
		rs.mu.Unlock()
		_, scope, _ := strings.Cut(tok, ":")
		have := strings.Fields(scope)
		for _, s := range rs.need {
			if !containsString(have, s) {
				rs.mu.Lock()
				rs.forbidden++
				rs.mu.Unlock()
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer error="insufficient_scope", scope="%s"`, s))
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		mcpHandler.ServeHTTP(w, r)
	})
	rs.Server = httptest.NewServer(mux)
	t.Cleanup(rs.Close)
	return rs
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func oauthSecrets() fw.Secrets { return mapSecrets(map[string]string{"OAUTH_SECRET": "hunter2"}) }

// newLoggedServer builds a Server whose debug log goes to buf.
func newLoggedServer(t *testing.T, cfg fw.MCPServerConfig, buf *bytes.Buffer) *Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s, err := New(context.Background(), cfg, oauthSecrets(), log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOAuthDiscovery(t *testing.T) {
	as := newAuthServer(t)
	rs := newResourceServer(t, as)
	var logs bytes.Buffer
	s := newLoggedServer(t, fw.MCPServerConfig{
		Name:     "secure",
		Endpoint: rs.URL + "/mcp",
		Auth: fw.MCPAuthConfig{OAuth: &fw.OAuthClientConfig{
			ClientID:     "fw-client",
			ClientSecret: "OAUTH_SECRET",
			Scopes:       []string{"mcp.read", "mcp.call"},
		}},
	}, &logs)

	ctx := context.Background()
	tools, err := s.Tools(ctx)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(tools) != 5 {
		t.Fatalf("tools = %v", toolNames(tools))
	}
	res, err := s.Call(ctx, "echo", json.RawMessage(`{"msg":"authorized"}`))
	if err != nil || fw.TextOf(res.Content) != "echo: authorized" {
		t.Fatalf("Call = %+v, %v", res, err)
	}

	forms := as.requests()
	if len(forms) != 1 {
		t.Fatalf("token requests = %d, want 1 (the token is cached)", len(forms))
	}
	f := forms[0]
	want := map[string]string{
		"grant_type":    "client_credentials",
		"client_id":     "fw-client",
		"client_secret": "hunter2",
		"scope":         "mcp.read mcp.call",
		"resource":      rs.URL + "/mcp",
	}
	for k, v := range want {
		if got := f.Get(k); got != v {
			t.Errorf("token form %s = %q, want %q", k, got, v)
		}
	}
	rs.mu.Lock()
	unauthorized := rs.unauthorized
	rs.mu.Unlock()
	if unauthorized != 1 {
		t.Errorf("401 responses = %d, want 1 (the first request only)", unauthorized)
	}
	if strings.Contains(logs.String(), "tok-") || strings.Contains(logs.String(), "hunter2") {
		t.Errorf("a token or a secret reached the log:\n%s", logs.String())
	}
}

func TestOAuthTokenURL(t *testing.T) {
	as := newAuthServer(t)
	rs := newResourceServer(t, as)
	var logs bytes.Buffer
	s := newLoggedServer(t, fw.MCPServerConfig{
		Name:     "secure",
		Endpoint: rs.URL + "/mcp",
		Auth: fw.MCPAuthConfig{OAuth: &fw.OAuthClientConfig{
			ClientID:     "fw-client",
			ClientSecret: "OAUTH_SECRET",
			TokenURL:     as.URL + "/token",
		}},
	}, &logs)
	if _, err := s.Tools(context.Background()); err != nil {
		t.Fatalf("Tools: %v", err)
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.unauthorized != 0 {
		t.Errorf("401 responses = %d, want 0: the token comes before the first request", rs.unauthorized)
	}
	if n := len(as.requests()); n != 1 {
		t.Errorf("token requests = %d, want 1", n)
	}
	if as.requests()[0].Has("scope") {
		t.Errorf("scope sent without configured scopes: %v", as.requests()[0])
	}
}

func TestOAuthStepUp(t *testing.T) {
	as := newAuthServer(t)
	rs := newResourceServer(t, as, "mcp.admin")
	var logs bytes.Buffer
	s := newLoggedServer(t, fw.MCPServerConfig{
		Name:     "secure",
		Endpoint: rs.URL + "/mcp",
		Auth: fw.MCPAuthConfig{OAuth: &fw.OAuthClientConfig{
			ClientID:     "fw-client",
			ClientSecret: "OAUTH_SECRET",
			Scopes:       []string{"mcp.read"},
			TokenURL:     as.URL + "/token",
		}},
	}, &logs)
	if _, err := s.Tools(context.Background()); err != nil {
		t.Fatalf("Tools: %v", err)
	}
	forms := as.requests()
	if len(forms) != 2 {
		t.Fatalf("token requests = %d, want 2", len(forms))
	}
	if got := forms[1].Get("scope"); got != "mcp.read mcp.admin" {
		t.Errorf("step-up scope = %q, want the union", got)
	}
}

func TestOAuthBadSecret(t *testing.T) {
	as := newAuthServer(t)
	rs := newResourceServer(t, as)
	cfg := fw.MCPServerConfig{
		Name:     "secure",
		Endpoint: rs.URL + "/mcp",
		Auth: fw.MCPAuthConfig{OAuth: &fw.OAuthClientConfig{
			ClientID:     "fw-client",
			ClientSecret: "WRONG_SECRET",
			TokenURL:     as.URL + "/token",
		}},
	}
	s, err := New(context.Background(), cfg, mapSecrets(map[string]string{"WRONG_SECRET": "letmein"}), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.rt.retryBase = time.Millisecond
	res, err := s.Call(context.Background(), "echo", nil)
	if err != nil {
		t.Fatal(err)
	}
	text := fw.TextOf(res.Content)
	if !res.IsError || !strings.Contains(text, "invalid_client") {
		t.Errorf("result = %+v, want the token endpoint error", res)
	}
	if strings.Contains(text, "letmein") {
		t.Errorf("the client secret reached the tool result: %s", text)
	}
}

func TestOAuthRefreshBeforeExpiry(t *testing.T) {
	as := newAuthServer(t)
	as.expiresIn = 60
	u, _ := url.Parse("https://mcp.example.com/mcp")
	o := newOAuthClient("x", &fw.OAuthClientConfig{ClientID: "fw-client", ClientSecret: "OAUTH_SECRET", TokenURL: as.URL + "/token"},
		oauthSecrets(), u, as.Client(), slog.New(slog.DiscardHandler))
	t0 := time.Now()
	now := t0
	o.now = func() time.Time { return now }
	ctx := context.Background()

	first, err := o.token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now = t0.Add(20 * time.Second) // 40s left: keep the token
	if tok, _ := o.token(ctx); tok != first {
		t.Errorf("token at 40s left = %q, want the cached %q", tok, first)
	}
	now = t0.Add(31 * time.Second) // 29s left: refresh
	second, err := o.token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Error("token not refreshed within 30 seconds of expiry")
	}
	if n := len(as.requests()); n != 2 {
		t.Errorf("token requests = %d, want 2", n)
	}
	if got := as.requests()[0].Get("resource"); got != "https://mcp.example.com/mcp" {
		t.Errorf("resource = %q", got)
	}
}

func TestBearerChallenge(t *testing.T) {
	h := http.Header{}
	h.Add("WWW-Authenticate", `Basic realm="x", Bearer resource_metadata="https://rs.example.com/.well-known/oauth-protected-resource", scope="a b"`)
	c := bearerChallenge(h)
	if c["resource_metadata"] != "https://rs.example.com/.well-known/oauth-protected-resource" || c["scope"] != "a b" {
		t.Errorf("challenge = %v", c)
	}
	if c := bearerChallenge(http.Header{}); len(c) != 0 {
		t.Errorf("empty header challenge = %v", c)
	}
}

func TestCanonicalResource(t *testing.T) {
	cases := map[string]string{
		"HTTPS://MCP.Example.com/mcp":      "https://mcp.example.com/mcp",
		"https://mcp.example.com:443/mcp":  "https://mcp.example.com/mcp",
		"https://mcp.example.com:8443/mcp": "https://mcp.example.com:8443/mcp",
		"https://mcp.example.com/":         "https://mcp.example.com",
		"https://mcp.example.com/mcp#frag": "https://mcp.example.com/mcp",
		"http://[::1]:80/mcp":              "http://[::1]/mcp",
	}
	for in, want := range cases {
		u, err := url.Parse(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := canonicalResource(u); got != want {
			t.Errorf("canonicalResource(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestMetadataURLs(t *testing.T) {
	u, _ := url.Parse("https://rs.example.com/v1/mcp")
	got := wellKnownResourceURLs(u)
	want := []string{
		"https://rs.example.com/.well-known/oauth-protected-resource/v1/mcp",
		"https://rs.example.com/.well-known/oauth-protected-resource",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("wellKnownResourceURLs = %v", got)
	}
	as, err := authServerMetadataURLs("https://auth.example.com/tenant")
	if err != nil {
		t.Fatal(err)
	}
	if as[0] != "https://auth.example.com/.well-known/oauth-authorization-server/tenant" {
		t.Errorf("authServerMetadataURLs = %v", as)
	}
	if _, err := authServerMetadataURLs("http://auth.example.com"); err == nil {
		t.Error("plain http authorization server outside the cluster accepted")
	}
}
