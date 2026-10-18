package oidc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// issuer is a fake OpenID Connect issuer: a discovery document and a JWKS
// with one RSA key, plus a signer for tokens.
type issuer struct {
	srv      *httptest.Server
	key      *rsa.PrivateKey
	kid      string
	healthy  atomic.Bool
	jwksHits atomic.Int64
}

func newIssuer(t *testing.T, tls bool) *issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	is := &issuer{key: key, kid: "test-key"}
	is.healthy.Store(true)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		if !is.healthy.Load() {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                is.srv.URL,
			"authorization_endpoint":                is.srv.URL + "/auth",
			"token_endpoint":                        is.srv.URL + "/token",
			"jwks_uri":                              is.srv.URL + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, r *http.Request) {
		is.jwksHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: &key.PublicKey, KeyID: is.kid, Algorithm: "RS256", Use: "sig",
		}}})
	})
	if tls {
		is.srv = httptest.NewTLSServer(mux)
	} else {
		is.srv = httptest.NewServer(mux)
	}
	t.Cleanup(is.srv.Close)
	return is
}

// token mints a token signed by the issuer key. Claims that the caller
// does not set get defaults: iss, aud "fw-test", sub "u1", iat, and exp an
// hour ahead.
func (is *issuer) token(t *testing.T, claims map[string]any) string {
	t.Helper()
	return mint(t, is.key, is.kid, is.srv.URL, claims)
}

func mint(t *testing.T, key *rsa.PrivateKey, kid, iss string, claims map[string]any) string {
	t.Helper()
	all := map[string]any{
		"iss": iss,
		"aud": "fw-test",
		"sub": "u1",
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	for k, v := range claims {
		if v == nil {
			delete(all, k)
			continue
		}
		all[k] = v
	}
	opts := (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, opts)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(signer).Claims(all).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// config returns a configuration for the issuer with the metadata set.
func config(is *issuer) *fw.Config {
	cfg := fw.DefaultConfig()
	cfg.Auth.Mode = "oidc"
	cfg.Auth.OIDC = &fw.OIDCConfig{Issuer: is.srv.URL, Audience: []string{"fw-test"}, Insecure: true}
	cfg.MCP.Auth.Audience = "https://agent.example.com/mcp"
	return cfg
}

var noSecrets = fw.SecretsFunc(func(context.Context, string) (string, error) { return "", nil })

// newAuthn builds the driver. edit changes the configuration first.
func newAuthn(t *testing.T, is *issuer, edit func(cfg *fw.Config)) *Authn {
	t.Helper()
	cfg := config(is)
	if edit != nil {
		edit(cfg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a, err := New(ctx, cfg, noSecrets)
	if err != nil {
		t.Fatal(err)
	}
	return a.(*Authn)
}

func request(token string) *http.Request {
	r := httptest.NewRequest("GET", "/v1/runs", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

const metadataURL = "https://agent.example.com/.well-known/oauth-protected-resource/mcp"

func TestGoodToken(t *testing.T) {
	is := newIssuer(t, false)
	a := newAuthn(t, is, nil)
	tok := is.token(t, map[string]any{
		"sub": "alice-id", "email": "alice@example.com", "name": "Alice",
		"groups": []string{"reviewers", "platform-admins"}, "scope": "openid profile email",
	})
	p, err := a.Authenticate(request(tok))
	if err != nil {
		t.Fatal(err)
	}
	want := &fw.Principal{
		Subject: "alice-id", Name: "alice@example.com", Kind: fw.KindUser, Issuer: is.srv.URL,
		Groups: []string{"reviewers", "platform-admins"}, Scopes: []string{"openid", "profile", "email"},
	}
	if got, _ := json.Marshal(p); string(got) != string(must(json.Marshal(want))) {
		t.Fatalf("principal = %s, want %s", got, must(json.Marshal(want)))
	}
	if !fw.PrincipalMatches("group:platform-*", p) || !fw.PrincipalMatches("alice@example.com", p) || !fw.PrincipalMatches("issuer:"+is.srv.URL, p) {
		t.Fatal("the principal does not match its access entries")
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

func TestNameFallbacks(t *testing.T) {
	is := newIssuer(t, false)
	a := newAuthn(t, is, nil)
	cases := []struct {
		claims map[string]any
		want   string
	}{
		{map[string]any{"preferred_username": "alice", "name": "Alice"}, "alice"},
		{map[string]any{"name": "Alice"}, "Alice"},
		{map[string]any{}, "u1"},
	}
	for _, c := range cases {
		p, err := a.Authenticate(request(is.token(t, c.claims)))
		if err != nil {
			t.Fatal(err)
		}
		if p.Name != c.want {
			t.Errorf("claims %v: name = %q, want %q", c.claims, p.Name, c.want)
		}
	}
	// A configured claim wins, and falls back when it is absent.
	b := newAuthn(t, is, func(cfg *fw.Config) { cfg.Auth.OIDC.NameClaim = "preferred_username" })
	p, err := b.Authenticate(request(is.token(t, map[string]any{"email": "a@x", "preferred_username": "alice"})))
	if err != nil || p.Name != "alice" {
		t.Fatalf("nameClaim: name = %q, err %v", p.Name, err)
	}
	p, err = b.Authenticate(request(is.token(t, map[string]any{"email": "a@x"})))
	if err != nil || p.Name != "a@x" {
		t.Fatalf("nameClaim absent: name = %q, err %v", p.Name, err)
	}
}

func TestGroupsClaim(t *testing.T) {
	is := newIssuer(t, false)
	// The default reads groups, then roles; a string is one group.
	a := newAuthn(t, is, nil)
	p, err := a.Authenticate(request(is.token(t, map[string]any{"roles": "admin"})))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Groups) != 1 || p.Groups[0] != "admin" {
		t.Fatalf("roles fallback: groups = %v", p.Groups)
	}
	p, err = a.Authenticate(request(is.token(t, map[string]any{"groups": []string{}, "roles": []string{"admin"}})))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Groups) != 0 {
		t.Fatalf("an empty groups claim wins over roles: groups = %v", p.Groups)
	}
	// An override names another claim, with a dotted path into an object.
	b := newAuthn(t, is, func(cfg *fw.Config) { cfg.Auth.OIDC.GroupsClaim = "realm_access.roles" })
	p, err = b.Authenticate(request(is.token(t, map[string]any{
		"groups":       []string{"ignored"},
		"realm_access": map[string]any{"roles": []string{"reviewer", "", "ops"}},
	})))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(p.Groups, ",") != "reviewer,ops" {
		t.Fatalf("groupsClaim override: groups = %v", p.Groups)
	}
	c := newAuthn(t, is, func(cfg *fw.Config) { cfg.Auth.OIDC.GroupsClaim = "team" })
	p, err = c.Authenticate(request(is.token(t, map[string]any{"team": "core"})))
	if err != nil || strings.Join(p.Groups, ",") != "core" {
		t.Fatalf("string group: groups = %v, err %v", p.Groups, err)
	}
}

func TestScopesClaim(t *testing.T) {
	is := newIssuer(t, false)
	a := newAuthn(t, is, nil)
	p, err := a.Authenticate(request(is.token(t, map[string]any{"scp": []string{"read", "write"}})))
	if err != nil || strings.Join(p.Scopes, ",") != "read,write" {
		t.Fatalf("scp: scopes = %v, err %v", p.Scopes, err)
	}
}

func TestWorkloadKind(t *testing.T) {
	is := newIssuer(t, false)
	a := newAuthn(t, is, nil)
	cases := []struct {
		name   string
		claims map[string]any
		want   string
	}{
		{"client_id equals sub", map[string]any{"sub": "svc", "client_id": "svc"}, fw.KindWorkload},
		{"azp equals sub without email", map[string]any{"sub": "svc", "azp": "svc"}, fw.KindWorkload},
		{"azp equals sub with email", map[string]any{"sub": "svc", "azp": "svc", "email": "svc@example.com"}, fw.KindUser},
		{"a person", map[string]any{"sub": "u1", "azp": "dashboard", "email": "u@example.com"}, fw.KindUser},
	}
	for _, c := range cases {
		p, err := a.Authenticate(request(is.token(t, c.claims)))
		if err != nil {
			t.Fatal(c.name, err)
		}
		if p.Kind != c.want {
			t.Errorf("%s: kind = %q, want %q", c.name, p.Kind, c.want)
		}
	}
}

func TestRejects(t *testing.T) {
	is := newIssuer(t, false)
	a := newAuthn(t, is, func(cfg *fw.Config) { cfg.Auth.OIDC.Scopes = []string{"openid", "groups"} })
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		token  string
		reason string
	}{
		{"wrong audience", is.token(t, map[string]any{"aud": "someone-else"}), "audience not accepted"},
		{"expired", is.token(t, map[string]any{"exp": time.Now().Add(-2 * time.Minute).Unix()}), "token expired"},
		{"wrong issuer", mint(t, is.key, is.kid, "https://evil.example.com", nil), "wrong issuer"},
		{"bad signature", mint(t, other, is.kid, is.srv.URL, nil), "bad signature"},
		{"not a JWT", "not-a-jwt", "the token is not a JWT"},
		{"missing header", "", "no bearer token"},
	}
	for _, c := range cases {
		r := request(c.token)
		p, err := a.Authenticate(r)
		if !errors.Is(err, fw.ErrUnauthorized) || p != nil {
			t.Fatalf("%s: got %v, %v; want ErrUnauthorized", c.name, p, err)
		}
		if !strings.Contains(err.Error(), c.reason) {
			t.Errorf("%s: err %q does not name %q", c.name, err, c.reason)
		}
		if strings.Contains(err.Error(), c.token) && c.token != "" {
			t.Errorf("%s: the error holds the token", c.name)
		}
		w := httptest.NewRecorder()
		a.Challenge(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s: challenge status %d", c.name, w.Code)
		}
		h := w.Header().Get("WWW-Authenticate")
		prefix := `Bearer realm="fw", resource_metadata="` + metadataURL + `", scope="openid groups"`
		if !strings.HasPrefix(h, prefix) {
			t.Errorf("%s: header %q does not start with %q", c.name, h, prefix)
		}
		if c.token == "" {
			if strings.Contains(h, "error") {
				t.Errorf("%s: header %q holds an error for a request with no token", c.name, h)
			}
			continue
		}
		if !strings.Contains(h, `error="invalid_token"`) {
			t.Errorf("%s: header %q lacks error=\"invalid_token\"", c.name, h)
		}
		if !strings.Contains(h, `error_description="`+c.reason+`"`) {
			t.Errorf("%s: header %q lacks the reason %q", c.name, h, c.reason)
		}
		if strings.Contains(h, c.token) {
			t.Errorf("%s: the header holds the token", c.name)
		}
	}
}

func TestExpiryLeeway(t *testing.T) {
	is := newIssuer(t, false)
	a := newAuthn(t, is, nil)
	tok := is.token(t, map[string]any{"exp": time.Now().Add(-30 * time.Second).Unix()})
	if _, err := a.Authenticate(request(tok)); err != nil {
		t.Fatalf("a token 30 s past its expiry is inside the leeway: %v", err)
	}
}

func TestCache(t *testing.T) {
	is := newIssuer(t, false)
	a := newAuthn(t, is, nil)
	tok := is.token(t, map[string]any{"groups": []string{"g"}})
	first, err := a.Authenticate(request(tok))
	if err != nil {
		t.Fatal(err)
	}
	first.Groups[0] = "changed" // a caller cannot change the cached principal
	second, err := a.Authenticate(request(tok))
	if err != nil {
		t.Fatal(err)
	}
	if second.Groups[0] != "g" || second.Subject != first.Subject {
		t.Fatalf("second principal = %+v", second)
	}
	if n := a.verified.Load(); n != 1 {
		t.Fatalf("verifications = %d, want 1", n)
	}
	if n := is.jwksHits.Load(); n != 1 {
		t.Fatalf("jwks hits = %d, want 1", n)
	}
	if a.cache.len() != 1 {
		t.Fatalf("cache holds %d entries", a.cache.len())
	}
	// The entry ends with the token.
	a.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if _, err := a.Authenticate(request(tok)); !errors.Is(err, fw.ErrUnauthorized) {
		t.Fatalf("after expiry: %v", err)
	}
	if n := a.verified.Load(); n != 2 {
		t.Fatalf("verifications after expiry = %d, want 2", n)
	}
}

func TestCacheBound(t *testing.T) {
	c := newCache(2)
	now := time.Now()
	keys := [][32]byte{{1}, {2}, {3}}
	for _, k := range keys {
		c.put(k, entry{reason: "x", until: now.Add(time.Minute)})
	}
	if c.len() != 2 {
		t.Fatalf("len = %d", c.len())
	}
	if _, ok := c.get(keys[0], now); ok {
		t.Fatal("the oldest entry stays")
	}
	if _, ok := c.get(keys[2], now); !ok {
		t.Fatal("the newest entry is gone")
	}
	if _, ok := c.get(keys[2], now.Add(2*time.Minute)); ok {
		t.Fatal("an entry past its time stays")
	}
}

func TestConcurrent(t *testing.T) {
	is := newIssuer(t, false)
	a := newAuthn(t, is, nil)
	tokens := []string{is.token(t, nil), is.token(t, map[string]any{"sub": "u2"}), "bad.token.x"}
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok := tokens[i%len(tokens)]
			p, err := a.Authenticate(request(tok))
			if tok == "bad.token.x" {
				if !errors.Is(err, fw.ErrUnauthorized) {
					t.Error("bad token accepted")
				}
				return
			}
			if err != nil || p == nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

func TestResourceMetadata(t *testing.T) {
	is := newIssuer(t, false)
	cfg := config(is)
	cfg.Auth.OIDC.Scopes = []string{"openid", "groups"}
	doc := ResourceMetadata(cfg)
	if doc["resource"] != "https://agent.example.com/mcp" {
		t.Errorf("resource = %v", doc["resource"])
	}
	if got := doc["authorization_servers"].([]string); len(got) != 1 || got[0] != is.srv.URL {
		t.Errorf("authorization_servers = %v, want the issuer", got)
	}
	if got := doc["scopes_supported"].([]string); strings.Join(got, " ") != "openid groups" {
		t.Errorf("scopes_supported = %v", got)
	}
	if got := doc["bearer_methods_supported"].([]string); len(got) != 1 || got[0] != "header" {
		t.Errorf("bearer_methods_supported = %v", got)
	}
	if got := ResourceMetadataURL(cfg); got != metadataURL {
		t.Errorf("metadata URL = %q", got)
	}

	cfg.MCP.Auth.AuthorizationServers = []string{"https://as.example.com"}
	cfg.MCP.Auth.Audience = ""
	cfg.A2A.URL = "https://agent.example.com/"
	doc = ResourceMetadata(cfg)
	if doc["resource"] != "https://agent.example.com/" {
		t.Errorf("resource from a2a.url = %v", doc["resource"])
	}
	if got := doc["authorization_servers"].([]string); got[0] != "https://as.example.com" {
		t.Errorf("configured authorization_servers = %v", got)
	}
	if got := ResourceMetadataURL(cfg); got != "https://agent.example.com/.well-known/oauth-protected-resource" {
		t.Errorf("metadata URL from a2a.url = %q", got)
	}

	cfg.A2A.URL = ""
	if _, ok := ResourceMetadata(cfg)["resource"]; ok {
		t.Error("resource is set with no audience and no a2a.url")
	}
	if ResourceMetadataURL(cfg) != "" {
		t.Error("metadata URL is set with no audience and no a2a.url")
	}
}

func TestChallengeWithoutMetadata(t *testing.T) {
	is := newIssuer(t, false)
	a := newAuthn(t, is, func(cfg *fw.Config) { cfg.MCP.Auth.Audience = ""; cfg.A2A.URL = "" })
	w := httptest.NewRecorder()
	a.Challenge(w, request(""))
	if h := w.Header().Get("WWW-Authenticate"); h != `Bearer realm="fw"` {
		t.Fatalf("header = %q", h)
	}
}

func TestDiscoveryRecovers(t *testing.T) {
	// A request waits readyWait for the first discovery; the test keeps it
	// short, because the issuer stays down for a while here.
	prev := readyWait
	readyWait = 50 * time.Millisecond
	t.Cleanup(func() { readyWait = prev })
	is := newIssuer(t, false)
	is.healthy.Store(false)
	a := newAuthn(t, is, nil)
	if a.Ready() {
		t.Fatal("ready before discovery succeeded")
	}
	tok := is.token(t, nil)
	if _, err := a.Authenticate(request(tok)); !errors.Is(err, fw.ErrUnauthorized) {
		t.Fatalf("before recovery: %v", err)
	}
	w := httptest.NewRecorder()
	a.Challenge(w, request(tok))
	if h := w.Header().Get("WWW-Authenticate"); !strings.Contains(h, `error="invalid_token", error_description="`+notReady+`"`) {
		t.Fatalf("header while not ready = %q", h)
	}
	is.healthy.Store(true)
	deadline := time.Now().Add(10 * time.Second)
	for {
		p, err := a.Authenticate(request(tok))
		if err == nil && p.Subject == "u1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("discovery did not recover: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestJWKSURLSkipsDiscovery(t *testing.T) {
	is := newIssuer(t, false)
	is.healthy.Store(false) // discovery is down, and not needed
	a := newAuthn(t, is, func(cfg *fw.Config) { cfg.Auth.OIDC.JWKSURL = is.srv.URL + "/keys" })
	if !a.Ready() {
		t.Fatal("not ready with a fixed keys endpoint")
	}
	if _, err := a.Authenticate(request(is.token(t, nil))); err != nil {
		t.Fatal(err)
	}
}

func TestAudienceDefaults(t *testing.T) {
	is := newIssuer(t, false)
	// With no auth.oidc.audience, mcp.auth.audience is the audience.
	a := newAuthn(t, is, func(cfg *fw.Config) { cfg.Auth.OIDC.Audience = nil })
	if _, err := a.Authenticate(request(is.token(t, map[string]any{"aud": "https://agent.example.com/mcp"}))); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate(request(is.token(t, nil))); !errors.Is(err, fw.ErrUnauthorized) {
		t.Fatalf("another audience accepted: %v", err)
	}
	// With neither, every audience is accepted, and a list in aud works.
	b := newAuthn(t, is, func(cfg *fw.Config) { cfg.Auth.OIDC.Audience = nil; cfg.MCP.Auth.Audience = "" })
	if _, err := b.Authenticate(request(is.token(t, map[string]any{"aud": []string{"x", "y"}}))); err != nil {
		t.Fatal(err)
	}
	c := newAuthn(t, is, func(cfg *fw.Config) { cfg.Auth.OIDC.Audience = []string{"other", "y"} })
	if _, err := c.Authenticate(request(is.token(t, map[string]any{"aud": []string{"x", "y"}}))); err != nil {
		t.Fatal(err)
	}
}

func TestInsecureIssuerRefused(t *testing.T) {
	cfg := fw.DefaultConfig()
	cfg.Auth.OIDC = &fw.OIDCConfig{Issuer: "http://127.0.0.1:1/realms/x"}
	_, err := New(context.Background(), cfg, noSecrets)
	if err == nil || !strings.Contains(err.Error(), "auth.oidc.insecure") {
		t.Fatalf("err = %v, want a refusal that names auth.oidc.insecure", err)
	}
	cfg.Auth.OIDC = &fw.OIDCConfig{Issuer: "https://idp.example.com", JWKSURL: "http://127.0.0.1:1/keys"}
	_, err = New(context.Background(), cfg, noSecrets)
	if err == nil || !strings.Contains(err.Error(), "auth.oidc.jwksUrl") {
		t.Fatalf("err = %v, want a refusal that names auth.oidc.jwksUrl", err)
	}
}

func TestMissingIssuer(t *testing.T) {
	cfg := fw.DefaultConfig()
	_, err := New(context.Background(), cfg, noSecrets)
	if err == nil || !strings.Contains(err.Error(), "auth.oidc.issuer") {
		t.Fatalf("err = %v, want one that names auth.oidc.issuer", err)
	}
	cfg.Auth.OIDC = &fw.OIDCConfig{Issuer: "not a url"}
	if _, err := New(context.Background(), cfg, noSecrets); err == nil {
		t.Fatal("a bad issuer URL is accepted")
	}
}

func TestTLSIssuerWithCA(t *testing.T) {
	is := newIssuer(t, true)
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: is.srv.Certificate().Raw})
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, cert, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, ca := range []string{path, string(cert)} {
		a := newAuthn(t, is, func(cfg *fw.Config) { cfg.Auth.OIDC.Insecure = false; cfg.Auth.OIDC.CA = ca })
		if !a.Ready() {
			t.Fatalf("discovery over TLS failed with ca %q", ca[:16])
		}
		if _, err := a.Authenticate(request(is.token(t, nil))); err != nil {
			t.Fatal(err)
		}
	}
	// Without the CA, discovery fails and the driver answers 401.
	a := newAuthn(t, is, func(cfg *fw.Config) { cfg.Auth.OIDC.Insecure = false })
	if a.Ready() {
		t.Fatal("an unknown certificate is accepted")
	}
}

func TestBearerHeaderForms(t *testing.T) {
	r := request("")
	r.Header.Set("Authorization", "bearer  abc ")
	if tok, ok := bearerToken(r); !ok || tok != "abc" {
		t.Fatalf("lower-case scheme: %q %v", tok, ok)
	}
	r.Header.Set("Authorization", "Basic abc")
	if _, ok := bearerToken(r); ok {
		t.Fatal("Basic accepted as Bearer")
	}
	r.Header.Set("Authorization", "Bearer ")
	if _, ok := bearerToken(r); ok {
		t.Fatal("an empty token accepted")
	}
	if looksLikeJWT("a.b") || looksLikeJWT("a.b.c.d") || looksLikeJWT("a b.c.d") || !looksLikeJWT("eyJ.eyJ.sig") {
		t.Fatal("looksLikeJWT")
	}
}
