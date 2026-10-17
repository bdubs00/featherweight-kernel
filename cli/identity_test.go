package cli

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/config"
	"github.com/bdubs00/featherweight-kernel/fw"
	"gopkg.in/yaml.v3"
)

// The configuration refuses a TLS provider that is not registered, so the
// doctor test registers one whose constructor fails, which is what the
// doctor reports.
func init() {
	fw.RegisterTLSProvider("nosuch", func(context.Context, *fw.Config, fw.Secrets) (*tls.Config, error) {
		return nil, errors.New("test: no certificate")
	})
}

func TestLocalPrincipal(t *testing.T) {
	p := localPrincipal()
	if p.Kind != fw.KindLocal || p.Name == "" || p.Subject != "local:"+p.Name {
		t.Errorf("principal %+v", p)
	}
	for _, entry := range []string{"local:*", "kind:local", p.Subject, p.Name, "*"} {
		if !fw.PrincipalMatches(entry, p) {
			t.Errorf("%s does not match %+v", entry, p)
		}
	}
	if fw.PrincipalMatches("kind:user", p) || fw.PrincipalMatches("alice", p) {
		t.Error("a foreign entry matches the local principal")
	}
	cfg := fw.DefaultConfig()
	if fw.PrincipalFrom(commandContext(context.Background(), cfg)) != nil {
		t.Error("an open default puts a principal in the context")
	}
	cfg.Access.Default = "Closed"
	if got := fw.PrincipalFrom(commandContext(context.Background(), cfg)); got == nil || got.Subject != p.Subject {
		t.Errorf("a closed default: %+v", got)
	}
}

func TestWhoami(t *testing.T) {
	env(t)
	p := localPrincipal()
	code, out, errOut := run(t, "", "whoami")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	for _, want := range []string{"subject: " + p.Subject + "\n", "name: " + p.Name + "\n", "kind: local\n", "admin: false\n", "acts: in fw mcp and fw agents"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	t.Setenv("FW_ACCESS_ADMINS", "kind:local")
	t.Setenv("FW_ACCESS_DEFAULT", "closed")
	code, out, _ = run(t, "", "whoami")
	if code != 0 || !strings.Contains(out, "admin: true\n") || !strings.Contains(out, "acts: always") {
		t.Errorf("as an admin: exit %d\n%s", code, out)
	}
	code, out, _ = run(t, "", "whoami", "--json")
	var got fw.Principal
	if code != 0 || json.Unmarshal([]byte(out), &got) != nil || got.Subject != p.Subject || got.Kind != fw.KindLocal {
		t.Errorf("json: exit %d\n%s", code, out)
	}
}

// fw mcp serves as the local principal.
func TestMCPServesAsLocalPrincipal(t *testing.T) {
	env(t, fw.WithModel(&scriptModel{}))
	prev := ServeMCPStdio
	var seen atomic.Pointer[fw.Principal]
	ServeMCPStdio = func(ctx context.Context, _ *fw.Agent) error { seen.Store(fw.PrincipalFrom(ctx)); return nil }
	t.Cleanup(func() { ServeMCPStdio = prev })
	if code, _, errOut := run(t, "", "mcp"); code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if p := seen.Load(); p == nil || p.Kind != fw.KindLocal || p.Subject != localPrincipal().Subject {
		t.Errorf("principal %+v", p)
	}
}

// With no profile configured, the list names the built-in profile
// "default", and the verb prints it.
func TestAgentsBuiltinDefault(t *testing.T) {
	env(t)
	path := filepath.Join(t.TempDir(), "fw.yaml")
	if err := os.WriteFile(path, []byte("# no profiles\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FW_CONFIG", path)
	code, out, errOut := run(t, "", "agents", "default")
	if code != 0 || !strings.Contains(out, "name: default") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if code, _, errOut = run(t, "", "agents", "nope"); code == 0 || !strings.Contains(errOut, `no profile is named "nope"; the profiles are default`) {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
}

func TestAgentsVerb(t *testing.T) {
	m := &scriptModel{resps: []*fw.Response{endTurn("ok")}}
	home := env(t, fw.WithModel(m))
	path := filepath.Join(t.TempDir(), "fw.yaml")
	if err := os.WriteFile(path, []byte("agents:\n  fixed:\n    description: From fw.yaml\n    model: anthropic/claude-haiku-5-5\n    access: {owners: [alice], viewers: [\"*\"]}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FW_CONFIG", path)

	// List: the fw.yaml profile, with the access block on one line.
	code, out, errOut := run(t, "", "agents")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || strings.Join(strings.Fields(lines[0]), " ") != "NAME DESCRIPTION MODEL ACCESS" ||
		strings.Join(strings.Fields(lines[1]), " ") != "fixed From fw.yaml anthropic/claude-haiku-5-5 owners=alice viewers=*" {
		t.Errorf("table:\n%s", out)
	}

	// Put: a new profile from a file, owned by the local principal.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "prompt.md"), []byte("You write.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "writer.yaml")
	if err := os.WriteFile(file, []byte("description: Writes\nsystem: prompt.md\ntools: [files]\nskills: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = run(t, "", "agents", "put", file)
	if code != 0 {
		t.Fatalf("put: exit %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "created the profile writer under "+filepath.Join(home, "agents")) || !strings.Contains(out, "next start") {
		t.Errorf("put output: %s", out)
	}
	stored, err := os.ReadFile(filepath.Join(home, "agents", "writer.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var p fw.AgentProfile
	if err := yaml.Unmarshal(stored, &p); err != nil {
		t.Fatal(err)
	}
	if p.Name != "writer" || p.System != "You write.\n" || p.Access == nil || p.Access.Owners[0] != localPrincipal().Subject || p.Skills == nil || len(p.Skills) != 0 {
		t.Errorf("stored profile: %+v access %+v", p, p.Access)
	}

	// Get: as YAML and as JSON, and the list has both profiles.
	code, out, _ = run(t, "", "agents", "writer")
	if code != 0 || !strings.Contains(out, "name: writer\n") || !strings.Contains(out, "system: |") && !strings.Contains(out, "system: You write.") {
		t.Errorf("get: exit %d\n%s", code, out)
	}
	code, out, _ = run(t, "", "agents", "--json", "writer")
	var got fw.AgentProfile
	if code != 0 || json.Unmarshal([]byte(out), &got) != nil || got.Name != "writer" || got.Description != "Writes" {
		t.Errorf("get json: exit %d\n%s", code, out)
	}
	code, out, _ = run(t, "", "agents")
	if code != 0 || strings.Count(out, "\n") != 3 || !strings.Contains(out, "writer") {
		t.Errorf("list: exit %d\n%s", code, out)
	}

	// The new profile runs.
	code, _, errOut = run(t, "", "run", "--agent", "writer", "go")
	if code != 0 || len(m.requests()) != 1 || strings.TrimSpace(m.requests()[0].System) != "You write." {
		t.Errorf("run: exit %d\n%s", code, errOut)
	}

	// Replace with -name: the file has no access block, so the profile
	// keeps its owner. Then delete, which the owner can.
	code, out, _ = run(t, "", "agents", "put", "--name", "writer", file)
	if code != 0 || !strings.Contains(out, "replaced the profile writer") {
		t.Errorf("replace: exit %d\n%s", code, out)
	}
	code, out, _ = run(t, "", "agents", "--json", "writer")
	if code != 0 || json.Unmarshal([]byte(out), &got) != nil || got.Access == nil || got.Access.Owners[0] != localPrincipal().Subject {
		t.Errorf("after the replace: exit %d\n%s", code, out)
	}
	code, out, errOut = run(t, "", "agents", "delete", "writer")
	if code != 0 || !strings.Contains(out, "deleted the profile writer") {
		t.Errorf("delete: exit %d\n%s%s", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(home, "agents", "writer.yaml")); err == nil {
		t.Error("the file remains after the delete")
	}

	// Errors: a fw.yaml profile without and with the admin role, an
	// unknown profile, a bad file, and bad usage.
	if code, _, errOut := run(t, "", "agents", "put", "--name", "fixed", file); code != 1 || !strings.Contains(errOut, "forbidden") || !strings.Contains(errOut, "add local:* to access.admins") {
		t.Errorf("put over fw.yaml without a role: exit %d\n%s", code, errOut)
	}
	t.Setenv("FW_ACCESS_ADMINS", "local:*")
	if code, _, errOut := run(t, "", "agents", "put", "--name", "fixed", file); code != 1 || !strings.Contains(errOut, "the configuration defines it") {
		t.Errorf("put over fw.yaml: exit %d\n%s", code, errOut)
	}
	if code, _, errOut := run(t, "", "agents", "delete", "fixed"); code != 1 || !strings.Contains(errOut, "the configuration defines it") {
		t.Errorf("delete of fw.yaml: exit %d\n%s", code, errOut)
	}
	t.Setenv("FW_ACCESS_ADMINS", "")
	if code, _, errOut := run(t, "", "agents", "nope"); code != 1 || !strings.Contains(errOut, `"nope"`) || !strings.Contains(errOut, "fixed") {
		t.Errorf("unknown: exit %d\n%s", code, errOut)
	}
	if code, _, errOut := run(t, "", "agents", "delete", "nope"); code != 1 || !strings.Contains(errOut, "not found") {
		t.Errorf("delete unknown: exit %d\n%s", code, errOut)
	}
	if err := os.WriteFile(file, []byte("sytem: typo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := run(t, "", "agents", "put", file); code != 1 || !strings.Contains(errOut, "sytem") {
		t.Errorf("bad file: exit %d\n%s", code, errOut)
	}
	for _, args := range [][]string{{"agents", "put"}, {"agents", "a", "b"}, {"agents", "delete"}, {"agents", "put", "a", "b"}} {
		if code, _, _ := run(t, "", args...); code != exitUsage {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}

// With a closed default, fw agents and fw run act as the local principal:
// an admin entry opens everything, and without one the access block of
// the profile decides.
func TestAgentsAndRunUnderClosedAccess(t *testing.T) {
	m := &scriptModel{resps: []*fw.Response{endTurn("one"), endTurn("two")}}
	home := env(t, fw.WithModel(m))
	t.Setenv("FW_ACCESS_DEFAULT", "closed")
	me := localPrincipal().Subject
	if err := os.MkdirAll(filepath.Join(home, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "agents", "mine.yaml"), []byte("system: Mine.\naccess: {owners: [\""+me+"\"]}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "agents", "theirs.yaml"), []byte("system: Theirs.\naccess: {owners: [alice]}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// No admin: the list shows what the principal can view, and a run on
	// a foreign agent is refused with a hint.
	code, out, _ := run(t, "", "agents")
	if code != 0 || !strings.Contains(out, "mine") || strings.Contains(out, "theirs") {
		t.Errorf("list: exit %d\n%s", code, out)
	}
	if code, _, errOut := run(t, "", "agents", "theirs"); code != 1 || !strings.Contains(errOut, "forbidden") || !strings.Contains(errOut, "access.admins") {
		t.Errorf("get theirs: exit %d\n%s", code, errOut)
	}
	if code, _, errOut := run(t, "", "run", "--agent", "theirs", "go"); code != 1 || !strings.Contains(errOut, "forbidden") || !strings.Contains(errOut, "local:*") {
		t.Errorf("run theirs: exit %d\n%s", code, errOut)
	}
	code, out, errOut := run(t, "", "run", "--json", "--agent", "mine", "go")
	if code != 0 {
		t.Fatalf("run mine: exit %d\n%s", code, errOut)
	}
	var r fw.Run
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatal(err)
	}
	if r.Principal != me {
		t.Errorf("the run carries the principal %q, want %q", r.Principal, me)
	}
	if code, _, _ := run(t, "", "events", r.ID); code != 0 {
		t.Errorf("events: exit %d", code)
	}

	// As an admin, everything is allowed.
	t.Setenv("FW_ACCESS_ADMINS", "local:*")
	code, out, _ = run(t, "", "agents")
	if code != 0 || !strings.Contains(out, "theirs") {
		t.Errorf("admin list: exit %d\n%s", code, out)
	}
	if code, _, errOut := run(t, "", "run", "--agent", "theirs", "go"); code != 0 {
		t.Errorf("admin run: exit %d\n%s", code, errOut)
	}
	if code, _, errOut := run(t, "", "agents", "delete", "theirs"); code != 0 {
		t.Errorf("admin delete: exit %d\n%s", code, errOut)
	}
}

// writeCert writes a self-signed certificate and its key and returns their
// paths.
func writeCert(t *testing.T, dir string, notAfter time.Time) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fw-doctor"},
		DNSNames:     []string{"fw.example.com"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// fakeIssuer serves an OpenID discovery document that names itself.
func fakeIssuer(t *testing.T, jwks bool) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		doc := map[string]string{"issuer": srv.URL}
		if jwks {
			doc["jwks_uri"] = srv.URL + "/keys"
		}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDoctorIdentityChecks(t *testing.T) {
	env(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-doctor-secret")
	t.Setenv("ANTHROPIC_API_KEY_FILE", "")
	t.Setenv("SPIFFE_ENDPOINT_SOCKET", "")
	doctorSeams(t, &scriptModel{}, nil, nil)
	dir := t.TempDir()
	certPath, keyPath := writeCert(t, dir, time.Now().Add(24*time.Hour))
	bundle := filepath.Join(dir, "bundle.json")
	if err := os.WriteFile(bundle, []byte(`{"keys":[{"kty":"EC","kid":"a"},{"kty":"EC","kid":"b"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	issuer := fakeIssuer(t, true)

	// Everything in place: a chain, a reachable issuer, a bundle, a
	// certificate, and a closed default with the local admin.
	t.Setenv("FW_AUTH_MODE", "oidc,spiffe,token")
	t.Setenv("FW_AUTH_OIDC_ISSUER", issuer.URL)
	t.Setenv("FW_AUTH_OIDC_INSECURE", "true")
	t.Setenv("FW_AUTH_SPIFFE_TRUST_DOMAIN", "example.org")
	t.Setenv("FW_AUTH_SPIFFE_BUNDLE", bundle)
	t.Setenv("FW_API_TOKEN", "tok-secret")
	t.Setenv("FW_TLS_PROVIDER", "file")
	t.Setenv("FW_TLS_CERT", certPath)
	t.Setenv("FW_TLS_KEY", keyPath)
	t.Setenv("FW_ACCESS_DEFAULT", "closed")
	t.Setenv("FW_ACCESS_ADMINS", "local:*,alice")
	code, out, errOut := run(t, "", "doctor")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	for _, want := range []string{
		"ok    auth: modes oidc, spiffe, token\n",
		"ok    auth oidc: " + issuer.URL + " serves its discovery document with jwks_uri " + issuer.URL + "/keys\n",
		"ok    auth spiffe: the bundle " + bundle + " holds 2 keys, trust domain example.org\n",
		"ok    auth token: FW_API_TOKEN is set\n",
		"ok    tls: the certificate " + certPath + " loads, for fw.example.com, until ",
		"ok    access: default closed, 2 admins\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "tok-secret") || strings.Contains(out, "sk-doctor") {
		t.Error("a secret leaked")
	}

	// Problems: an issuer without jwks_uri, a bundle without keys, an
	// expired certificate, and a closed default with no admin.
	noKeys := fakeIssuer(t, false)
	t.Setenv("FW_AUTH_OIDC_ISSUER", noKeys.URL)
	if err := os.WriteFile(bundle, []byte(`{"keys":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	expiredCert, expiredKey := writeCert(t, t.TempDir(), time.Now().Add(-time.Hour))
	t.Setenv("FW_TLS_CERT", expiredCert)
	t.Setenv("FW_TLS_KEY", expiredKey)
	t.Setenv("FW_ACCESS_ADMINS", "")
	t.Setenv("FW_API_TOKEN", "")
	code, out, errOut = run(t, "", "doctor")
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s%s", code, out, errOut)
	}
	for _, want := range []string{
		"FAIL  auth oidc: the discovery document of " + noKeys.URL + " lists no jwks_uri",
		"FAIL  auth spiffe: the bundle " + bundle + " holds no key",
		"ok    auth token: FW_API_TOKEN is not set; fw serve needs it, and fw run does not",
		"FAIL  tls: the certificate " + expiredCert + " expired on ",
		"FAIL  access: default closed, 0 admins; nobody can manage the agents",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(errOut, "fw doctor: 4 problems") {
		t.Errorf("stderr:\n%s", errOut)
	}

	// An unreachable issuer, a missing bundle file, a socket that exists, a
	// missing socket, an unregistered TLS provider, and a closed default
	// whose admins do not name the local principal.
	issuer.Close()
	t.Setenv("FW_AUTH_OIDC_ISSUER", issuer.URL)
	t.Setenv("FW_AUTH_SPIFFE_BUNDLE", filepath.Join(dir, "missing.json"))
	t.Setenv("FW_TLS_PROVIDER", "nosuch")
	t.Setenv("FW_TLS_CERT", "")
	t.Setenv("FW_TLS_KEY", "")
	t.Setenv("FW_ACCESS_ADMINS", "alice")
	code, out, _ = run(t, "", "doctor")
	for _, want := range []string{
		"FAIL  auth oidc: the discovery document of " + issuer.URL + " is not reachable",
		"FAIL  auth spiffe: the bundle " + filepath.Join(dir, "missing.json") + " does not load",
		"FAIL  tls: the provider nosuch does not build a configuration",
		"ok    access: default closed, 1 admin; " + localPrincipal().Subject + " is not one, so fw run and fw agents need a role on each agent",
	} {
		if code != 1 || !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	t.Setenv("FW_AUTH_SPIFFE_BUNDLE", "")
	t.Setenv("FW_AUTH_SPIFFE_SOCKET", "unix://"+bundle) // any path that exists
	t.Setenv("FW_AUTH_MODE", "spiffe")
	t.Setenv("FW_TLS_PROVIDER", "")
	t.Setenv("FW_ACCESS_DEFAULT", "open")
	code, out, _ = run(t, "", "doctor")
	if code != 0 || !strings.Contains(out, "ok    auth spiffe: the Workload API socket "+bundle+" exists, trust domain example.org") {
		t.Errorf("socket: exit %d\n%s", code, out)
	}
	t.Setenv("FW_AUTH_SPIFFE_SOCKET", filepath.Join(dir, "nowhere.sock"))
	code, out, _ = run(t, "", "doctor")
	if code != 1 || !strings.Contains(out, "FAIL  auth spiffe: the Workload API socket "+filepath.Join(dir, "nowhere.sock")+" does not exist") {
		t.Errorf("missing socket: exit %d\n%s", code, out)
	}
	t.Setenv("FW_AUTH_SPIFFE_SOCKET", "")
	code, out, _ = run(t, "", "doctor")
	if code != 1 || !strings.Contains(out, "FAIL  auth spiffe: no bundle and no Workload API socket") {
		t.Errorf("no socket: exit %d\n%s", code, out)
	}
	t.Setenv("FW_AUTH_MODE", "none")
	code, out, _ = run(t, "", "doctor")
	if code != 0 || !strings.Contains(out, "ok    auth: modes none (every caller is anonymous") {
		t.Errorf("none: exit %d\n%s", code, out)
	}
}

// The starter fw.yaml documents access and auth as comments, and the
// blocks load once the comment markers go.
func TestInitTemplateIdentityBlocks(t *testing.T) {
	env(t)
	dir := filepath.Join(t.TempDir(), "proj")
	if code, _, errOut := run(t, "", "init", dir); code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	path := filepath.Join(dir, "fw.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{`# access: {admins: ["local:*", "alice@example.com"]}`, "#   mode: oidc,token", "#     issuer: https://idp.example.com", "#     audience: [fw]", "#     groupsClaim: groups"} {
		if !strings.Contains(s, want) {
			t.Errorf("template lacks %q:\n%s", want, s)
		}
	}
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if rest, ok := strings.CutPrefix(l, "# "); ok && (strings.HasPrefix(rest, "access:") || strings.HasPrefix(rest, "auth:") || strings.HasPrefix(rest, "  ")) {
			l = rest
		}
		lines = append(lines, l)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the uncommented template does not load: %v\n%s", err, strings.Join(lines, "\n"))
	}
	if got := cfg.Auth.Modes(); len(got) != 2 || got[0] != "oidc" || cfg.Auth.OIDC == nil || cfg.Auth.OIDC.Issuer != "https://idp.example.com" ||
		cfg.Auth.OIDC.GroupsClaim != "groups" || len(cfg.Access.Admins) != 2 || cfg.Access.Admins[0] != "local:*" {
		t.Errorf("loaded: modes %v oidc %+v access %+v", got, cfg.Auth.OIDC, cfg.Access)
	}
}
