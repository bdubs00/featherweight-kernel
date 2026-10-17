package spiffe

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/spiffe/go-spiffe/v2/bundle/spiffebundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

const (
	testTD   = "example.org"
	reviewer = "spiffe://example.org/ns/prod/sa/reviewer"
	audience = "https://reviewer.example.org"
)

// ca is a test certificate authority with a JWT signing key.
type ca struct {
	td     string
	cert   *x509.Certificate
	key    *ecdsa.PrivateKey
	jwtKey *ecdsa.PrivateKey
	kid    string
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return k
}

func newCA(t *testing.T, td, kid string) *ca {
	t.Helper()
	key := newKey(t)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test CA " + td + " " + kid},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		URIs:                  []*url.URL{{Scheme: "spiffe", Host: td}},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	return &ca{td: td, cert: cert, key: key, jwtKey: newKey(t), kid: kid}
}

// bundleJSON returns the trust bundle of the CA in JWKS form, with the
// X.509 authority and the JWT authority of every CA given.
func bundleJSON(t *testing.T, cas ...*ca) []byte {
	t.Helper()
	b := spiffebundle.New(spiffeid.RequireTrustDomainFromString(cas[0].td))
	for _, c := range cas {
		b.AddX509Authority(c.cert)
		if err := b.AddJWTAuthority(c.kid, &c.jwtKey.PublicKey); err != nil {
			t.Fatalf("AddJWTAuthority: %v", err)
		}
	}
	out, err := b.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return out
}

// writeBundle writes the bundle of the CAs to a file and returns its path.
func writeBundle(t *testing.T, cas ...*ca) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bundle.json")
	if err := os.WriteFile(path, bundleJSON(t, cas...), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// leaf issues an X.509-SVID for id. ips become IP SANs, for a server.
func (c *ca) leaf(t *testing.T, id string, ips ...net.IP) tls.Certificate {
	t.Helper()
	key := newKey(t)
	u, err := url.Parse(id)
	if err != nil {
		t.Fatalf("Parse %s: %v", id, err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		URIs:                  []*url.URL{u},
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// writePEM writes a certificate and its key to files and returns the paths.
func writePEM(t *testing.T, cert tls.Certificate) (certPath, keyPath string) {
	t.Helper()
	dir := t.TempDir()
	certPath = filepath.Join(dir, "tls.crt")
	keyPath = filepath.Join(dir, "tls.key")
	keyDER, err := x509.MarshalECPrivateKey(cert.PrivateKey.(*ecdsa.PrivateKey))
	if err != nil {
		t.Fatalf("MarshalECPrivateKey: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return certPath, keyPath
}

// jwt mints a JWT-SVID signed by the JWT key of the CA.
func (c *ca) jwt(t *testing.T, sub string, aud []string, exp time.Time) string {
	t.Helper()
	return signJWT(t, c.jwtKey, c.kid, sub, aud, exp)
}

// signJWT mints an ES256 token by hand, so the test needs no JOSE library.
func signJWT(t *testing.T, key *ecdsa.PrivateKey, kid, sub string, aud []string, exp time.Time) string {
	t.Helper()
	hdr, _ := json.Marshal(map[string]any{"alg": "ES256", "typ": "JWT", "kid": kid})
	claims, _ := json.Marshal(map[string]any{"sub": sub, "aud": aud, "exp": exp.Unix(), "iat": time.Now().Unix()})
	enc := base64.RawURLEncoding.EncodeToString
	signing := enc(hdr) + "." + enc(claims)
	sum := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + enc(sig)
}

// testConfig returns a configuration for the spiffe mode with a bundle
// source and the accepted audiences.
func testConfig(bundle string, aud ...string) *fw.Config {
	cfg := fw.DefaultConfig()
	cfg.Auth.Mode = "spiffe"
	cfg.Auth.SPIFFE = &fw.SPIFFEConfig{TrustDomain: testTD, Bundle: bundle, Audience: aud}
	return cfg
}

// newAuthn builds the mode and ends its background work with the test.
func newAuthn(t *testing.T, cfg *fw.Config) *Authn {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a, err := New(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a.(*Authn)
}

// newAgent builds an agent around the authenticator, for fw.RequireAuth.
func newAgent(t *testing.T, a fw.Authn) *fw.Agent {
	t.Helper()
	model := fw.ModelFunc{StreamFn: func(context.Context, *fw.Request, func(fw.Delta)) (*fw.Response, error) {
		return &fw.Response{}, nil
	}}
	agent, err := fw.New(
		fw.WithoutDefaults(),
		fw.WithAuthn(a),
		fw.WithModel(model),
		fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()),
		fw.WithSecrets(fw.EnvSecrets{}),
		fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if err != nil {
		t.Fatalf("fw.New: %v", err)
	}
	return agent
}

func bearerRequest(tok string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/whoami", nil)
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	return r
}

func TestJWTSVID(t *testing.T) {
	c := newCA(t, testTD, "k1")
	a := newAuthn(t, testConfig(writeBundle(t, c), audience))
	tok := c.jwt(t, reviewer, []string{audience}, time.Now().Add(5*time.Minute))
	p, err := a.Authenticate(bearerRequest(tok))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	want := &fw.Principal{Subject: reviewer, Name: "ns/prod/sa/reviewer", Kind: fw.KindWorkload, Issuer: "spiffe://example.org"}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("principal = %+v, want %+v", p, want)
	}
	if !fw.PrincipalMatches("spiffe://example.org/ns/prod/*", p) {
		t.Error("the prod glob does not match the principal")
	}
	if fw.PrincipalMatches("spiffe://example.org/ns/dev/*", p) {
		t.Error("the dev glob matches the principal")
	}
	if !fw.PrincipalMatches("kind:workload", p) || !fw.PrincipalMatches("issuer:spiffe://example.org", p) {
		t.Error("kind or issuer entry does not match the principal")
	}
}

func TestJWTSVIDRefused(t *testing.T) {
	c := newCA(t, testTD, "k1")
	a := newAuthn(t, testConfig(writeBundle(t, c), audience))
	in5 := time.Now().Add(5 * time.Minute)
	other := newKey(t)
	cases := []struct {
		name string
		req  *http.Request
	}{
		{"wrong audience", bearerRequest(c.jwt(t, reviewer, []string{"someone-else"}, in5))},
		{"other trust domain", bearerRequest(c.jwt(t, "spiffe://other.org/ns/prod/sa/reviewer", []string{audience}, in5))},
		{"expired", bearerRequest(c.jwt(t, reviewer, []string{audience}, time.Now().Add(-2*time.Minute)))},
		{"bad signature", bearerRequest(signJWT(t, other, c.kid, reviewer, []string{audience}, in5))},
		{"unknown key id", bearerRequest(signJWT(t, c.jwtKey, "k9", reviewer, []string{audience}, in5))},
		{"not a token", bearerRequest("not.a.token")},
		{"missing header", bearerRequest("")},
		{"basic scheme", func() *http.Request {
			r := bearerRequest("")
			r.SetBasicAuth("alice", c.jwt(t, reviewer, []string{audience}, in5))
			return r
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := a.Authenticate(tc.req)
			if !errors.Is(err, fw.ErrUnauthorized) || p != nil {
				t.Fatalf("Authenticate = %+v, %v; want ErrUnauthorized", p, err)
			}
		})
	}
}

func TestAudienceDefaults(t *testing.T) {
	c := newCA(t, testTD, "k1")
	bundle := writeBundle(t, c)
	in5 := time.Now().Add(5 * time.Minute)

	cfg := testConfig(bundle)
	cfg.MCP.Auth.Audience = "https://fw.example.org/mcp"
	cfg.A2A.URL = "https://fw.example.org"
	a := newAuthn(t, cfg)
	for _, aud := range []string{"https://fw.example.org/mcp", "https://fw.example.org"} {
		if _, err := a.Authenticate(bearerRequest(c.jwt(t, reviewer, []string{aud}, in5))); err != nil {
			t.Errorf("audience %s refused: %v", aud, err)
		}
	}
	if _, err := a.Authenticate(bearerRequest(c.jwt(t, reviewer, []string{"other"}, in5))); !errors.Is(err, fw.ErrUnauthorized) {
		t.Errorf("audience other accepted: %v", err)
	}

	// With no audience anywhere, any audience is accepted.
	a = newAuthn(t, testConfig(bundle))
	if _, err := a.Authenticate(bearerRequest(c.jwt(t, reviewer, []string{"anything"}, in5))); err != nil {
		t.Errorf("any audience refused: %v", err)
	}
}

func TestChallenge(t *testing.T) {
	c := newCA(t, testTD, "k1")
	a := newAuthn(t, testConfig(writeBundle(t, c)))
	w := httptest.NewRecorder()
	a.Challenge(w, bearerRequest(""))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d", w.Code)
	}
	if got := w.Header().Get("WWW-Authenticate"); got != `Bearer realm="fw", error="invalid_token"` {
		t.Errorf("WWW-Authenticate = %q", got)
	}
}

func TestX509SVID(t *testing.T) {
	c := newCA(t, testTD, "k1")
	certPath, keyPath := writePEM(t, c.leaf(t, "spiffe://example.org/ns/prod/sa/fw", net.IPv4(127, 0, 0, 1)))
	cfg := testConfig(writeBundle(t, c), audience)
	cfg.Auth.SPIFFE.X509 = true
	cfg.TLS = fw.ListenTLSConfig{Provider: "spiffe", Cert: certPath, Key: keyPath}
	a := newAuthn(t, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	tc, err := NewTLS(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("NewTLS: %v", err)
	}
	if tc.ClientAuth != tls.RequestClientCert {
		t.Fatalf("ClientAuth = %v, want RequestClientCert by default with x509", tc.ClientAuth)
	}
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(fw.PrincipalFrom(r.Context()))
	})
	ts := httptest.NewUnstartedServer(fw.RequireAuth(newAgent(t, a), echo))
	ts.TLS = tc
	ts.StartTLS()
	t.Cleanup(ts.Close)

	roots := x509.NewCertPool()
	roots.AddCert(c.cert)
	client := func(certs ...tls.Certificate) *http.Client {
		return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, Certificates: certs, MinVersion: tls.VersionTLS12}}}
	}
	get := func(t *testing.T, hc *http.Client, tok string) (*http.Response, *fw.Principal) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/whoami", nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		var p fw.Principal
		if resp.StatusCode == http.StatusOK {
			if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
				t.Fatalf("decode: %v", err)
			}
		}
		return resp, &p
	}

	t.Run("client certificate", func(t *testing.T) {
		resp, p := get(t, client(c.leaf(t, reviewer)), "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		want := &fw.Principal{Subject: reviewer, Name: "ns/prod/sa/reviewer", Kind: fw.KindWorkload, Issuer: "spiffe://example.org"}
		if !reflect.DeepEqual(p, want) {
			t.Fatalf("principal = %+v, want %+v", p, want)
		}
	})
	t.Run("no credential", func(t *testing.T) {
		resp, _ := get(t, client(), "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if got := resp.Header.Get("WWW-Authenticate"); !strings.Contains(got, "invalid_token") {
			t.Errorf("WWW-Authenticate = %q", got)
		}
	})
	t.Run("bearer over tls", func(t *testing.T) {
		tok := c.jwt(t, "spiffe://example.org/ns/prod/sa/other", []string{audience}, time.Now().Add(5*time.Minute))
		resp, p := get(t, client(), tok)
		if resp.StatusCode != http.StatusOK || p.Subject != "spiffe://example.org/ns/prod/sa/other" {
			t.Fatalf("status = %d, principal = %+v", resp.StatusCode, p)
		}
	})
	t.Run("other ca", func(t *testing.T) {
		rogue := newCA(t, testTD, "k2")
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/whoami", nil)
		resp, err := client(rogue.leaf(t, reviewer)).Do(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				t.Fatalf("a leaf from another CA was accepted")
			}
		}
	})
	t.Run("other trust domain", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/whoami", nil)
		resp, err := client(c.leaf(t, "spiffe://other.org/ns/prod/sa/reviewer")).Do(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				t.Fatalf("a leaf of another trust domain was accepted")
			}
		}
	})
}

func TestTLSClientAuthRequire(t *testing.T) {
	c := newCA(t, testTD, "k1")
	certPath, keyPath := writePEM(t, c.leaf(t, "spiffe://example.org/ns/prod/sa/fw", net.IPv4(127, 0, 0, 1)))
	cfg := testConfig(writeBundle(t, c))
	cfg.TLS = fw.ListenTLSConfig{Provider: "spiffe", Cert: certPath, Key: keyPath, ClientAuth: "require"}
	tc, err := NewTLS(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("NewTLS: %v", err)
	}
	if tc.ClientAuth != tls.RequireAnyClientCert || tc.VerifyPeerCertificate == nil {
		t.Fatalf("ClientAuth = %v, verify set = %v", tc.ClientAuth, tc.VerifyPeerCertificate != nil)
	}
	cfg.TLS.ClientAuth = ""
	if tc, err = NewTLS(context.Background(), cfg, nil); err != nil || tc.ClientAuth != tls.NoClientCert {
		t.Fatalf("without x509: ClientAuth = %v, err = %v; want NoClientCert", tc.ClientAuth, err)
	}
}

func TestBundleFileReload(t *testing.T) {
	c1 := newCA(t, testTD, "k1")
	c2 := newCA(t, testTD, "k2")
	path := writeBundle(t, c1)
	a := newAuthn(t, testConfig(path, audience))
	tok := c2.jwt(t, reviewer, []string{audience}, time.Now().Add(5*time.Minute))
	if _, err := a.Authenticate(bearerRequest(tok)); !errors.Is(err, fw.ErrUnauthorized) {
		t.Fatalf("token of k2 accepted before the bundle holds k2: %v", err)
	}
	if err := os.WriteFile(path, bundleJSON(t, c1, c2), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	if p, err := a.Authenticate(bearerRequest(tok)); err != nil || p.Subject != reviewer {
		t.Fatalf("token of k2 refused after the bundle changed: %+v, %v", p, err)
	}
	// The old key still works.
	tok1 := c1.jwt(t, reviewer, []string{audience}, time.Now().Add(5*time.Minute))
	if _, err := a.Authenticate(bearerRequest(tok1)); err != nil {
		t.Fatalf("token of k1 refused: %v", err)
	}
}

func TestBundleFileMissingAtStart(t *testing.T) {
	c := newCA(t, testTD, "k1")
	path := filepath.Join(t.TempDir(), "late.json")
	a := newAuthn(t, testConfig(path, audience))
	tok := c.jwt(t, reviewer, []string{audience}, time.Now().Add(5*time.Minute))
	if _, err := a.Authenticate(bearerRequest(tok)); !errors.Is(err, fw.ErrUnauthorized) {
		t.Fatalf("accepted without a bundle: %v", err)
	}
	if err := os.WriteFile(path, bundleJSON(t, c), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := a.Authenticate(bearerRequest(tok)); err != nil {
		t.Fatalf("refused after the bundle appeared: %v", err)
	}
}

func TestBundleURL(t *testing.T) {
	c := newCA(t, testTD, "k1")
	doc := bundleJSON(t, c)
	hits := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(doc)
	}))
	t.Cleanup(ts.Close)
	a := newAuthn(t, testConfig(ts.URL+"/bundle", audience))
	tok := c.jwt(t, reviewer, []string{audience}, time.Now().Add(5*time.Minute))
	if p, err := a.Authenticate(bearerRequest(tok)); err != nil || p.Subject != reviewer {
		t.Fatalf("Authenticate = %+v, %v", p, err)
	}
	if hits != 1 {
		t.Errorf("bundle fetched %d times at start, want 1", hits)
	}
	if _, err := New(context.Background(), testConfig("http://bundle.example.org/bundle"), nil); err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("plain http bundle outside the machine accepted: %v", err)
	}
}

func TestWorkloadAPIUnreachable(t *testing.T) {
	cfg := testConfig("")
	cfg.Auth.SPIFFE.Socket = "unix://" + filepath.Join(t.TempDir(), "missing.sock")
	a := newAuthn(t, cfg)
	c := newCA(t, testTD, "k1")
	tok := c.jwt(t, reviewer, nil, time.Now().Add(5*time.Minute))
	if _, err := a.Authenticate(bearerRequest(tok)); !errors.Is(err, fw.ErrUnauthorized) {
		t.Fatalf("accepted without a Workload API: %v", err)
	}
	if _, err := a.src.GetX509SVID(); err == nil {
		t.Error("GetX509SVID gave a certificate without a Workload API")
	}
}

func TestConfigErrors(t *testing.T) {
	c := newCA(t, testTD, "k1")
	bundle := writeBundle(t, c)
	t.Setenv("SPIFFE_ENDPOINT_SOCKET", "")
	cases := []struct {
		name string
		cfg  *fw.Config
		tls  bool
		want string
	}{
		{"no block", func() *fw.Config { cfg := fw.DefaultConfig(); cfg.Auth.Mode = "spiffe"; return cfg }(), false, "auth.spiffe.trustDomain"},
		{"no trust domain", func() *fw.Config { cfg := testConfig(bundle); cfg.Auth.SPIFFE.TrustDomain = " "; return cfg }(), false, "auth.spiffe.trustDomain"},
		{"bad trust domain", func() *fw.Config { cfg := testConfig(bundle); cfg.Auth.SPIFFE.TrustDomain = "Not A Domain"; return cfg }(), false, "auth.spiffe.trustDomain"},
		{"no source", testConfig(""), false, "SPIFFE_ENDPOINT_SOCKET"},
		{"bad socket", func() *fw.Config { cfg := testConfig(""); cfg.Auth.SPIFFE.Socket = "ftp://x"; return cfg }(), false, "auth.spiffe.socket"},
		{"tls without certificate", testConfig(bundle), true, "tls.cert"},
		{"tls bad client auth", func() *fw.Config {
			cfg := testConfig(bundle)
			certPath, keyPath := writePEM(t, c.leaf(t, "spiffe://example.org/fw"))
			cfg.TLS = fw.ListenTLSConfig{Cert: certPath, Key: keyPath, ClientAuth: "maybe"}
			return cfg
		}(), true, "tls.clientAuth"},
		{"tls no trust domain", func() *fw.Config { cfg := fw.DefaultConfig(); cfg.TLS.Provider = "spiffe"; return cfg }(), true, "auth.spiffe.trustDomain"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if tc.tls {
				_, err = NewTLS(context.Background(), tc.cfg, nil)
			} else {
				_, err = New(context.Background(), tc.cfg, nil)
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to name %s", err, tc.want)
			}
		})
	}
}

func TestTokenSource(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	n := 0
	s := NewTokenSource("", "git-mcp")
	s.now = func() time.Time { return now }
	var gotAud string
	life := 5 * time.Minute
	s.Fetch = func(_ context.Context, aud string) (string, time.Time, error) {
		n++
		gotAud = aud
		return fmt.Sprintf("t%d", n), now.Add(life), nil
	}
	ctx := context.Background()
	tok, err := s.Token(ctx)
	if err != nil || tok != "t1" || gotAud != "git-mcp" {
		t.Fatalf("Token = %q, %v, audience %q", tok, err, gotAud)
	}
	now = now.Add(4 * time.Minute)
	if tok, _ = s.Token(ctx); tok != "t1" {
		t.Fatalf("Token = %q, want the cached t1", tok)
	}
	now = now.Add(45 * time.Second) // 15 s left: inside the refresh margin
	if tok, _ = s.Token(ctx); tok != "t2" {
		t.Fatalf("Token = %q, want a new t2", tok)
	}
	// A short-lived token is replaced at half of its life.
	life = 20 * time.Second
	now = now.Add(time.Hour)
	if tok, _ = s.Token(ctx); tok != "t3" {
		t.Fatalf("Token = %q, want t3", tok)
	}
	now = now.Add(9 * time.Second)
	if tok, _ = s.Token(ctx); tok != "t3" {
		t.Fatalf("Token = %q, want the cached t3", tok)
	}
	now = now.Add(2 * time.Second)
	if tok, _ = s.Token(ctx); tok != "t4" {
		t.Fatalf("Token = %q, want t4", tok)
	}
	s.Fetch = func(context.Context, string) (string, time.Time, error) {
		return "", time.Time{}, errors.New("agent down")
	}
	now = now.Add(time.Hour)
	if _, err := s.Token(ctx); err == nil || !strings.Contains(err.Error(), "git-mcp") || !strings.Contains(err.Error(), "agent down") {
		t.Fatalf("Token error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
