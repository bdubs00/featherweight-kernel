package cli

// This file is fw doctor: one line per check, ok or a problem, and exit
// code 1 when any check fails. Every check runs on its own, so one failure
// does not hide the others.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/ax"
	"github.com/bdubs00/featherweight-kernel/config"
	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/memory"
)

// Seams for the checks that reach a provider or a server. Tests replace
// them, so no test calls a real provider.
var (
	openModel       = fw.NewModel
	openToolSources = fw.NewToolSources
)

// modelLister is a model that lists the models of its provider. The openai
// adapter implements it.
type modelLister interface {
	Models(ctx context.Context) ([]string, error)
}

// namer is a tool source that knows its name, such as an MCP server.
type namer interface {
	Name() string
}

// errNotWritable marks a directory that a check cannot write to.
var errNotWritable = errors.New("not writable")

// doctor runs the checks and counts the failures.
type doctor struct {
	g       *globals
	timeout time.Duration
	failed  int
}

func (d *doctor) ok(format string, args ...any) {
	fmt.Fprintf(d.g.stdout, "ok    "+format+"\n", args...)
}

func (d *doctor) fail(format string, args ...any) {
	d.failed++
	fmt.Fprintf(d.g.stdout, "FAIL  "+format+"\n", args...)
}

func cmdDoctor(ctx context.Context, g *globals, args []string) int {
	fs := g.newFlagSet("doctor", "")
	timeout := fs.Duration("timeout", 10*time.Second, "the `limit` of each check that reaches a provider or a server")
	if _, err := parseArgs(fs, args); err != nil {
		return flagExit(err)
	}
	d := &doctor{g: g, timeout: *timeout}
	cfg := d.checkConfig()
	d.checkHome(cfg)
	d.checkToolUser(cfg)
	d.checkTime(cfg)
	d.checkAuth(ctx, cfg)
	d.checkTLS(ctx, cfg)
	d.checkAccess(cfg)
	d.checkExternal(ctx, cfg)
	secrets := d.checkCredential(ctx, cfg)
	d.checkModel(ctx, cfg, secrets)
	d.checkToolSources(ctx, cfg, secrets)
	d.checkTokens(cfg)
	d.checkTelemetry()
	d.checkBus(ctx, cfg)
	for _, c := range registeredDoctorChecks() {
		if ok, line := c(ctx, cfg, secrets, d.timeout); ok {
			d.ok("%s", line)
		} else {
			d.fail("%s", line)
		}
	}
	d.checkLedger(ctx, cfg)
	d.checkSkills(cfg)
	d.checkMemory(cfg)
	if d.failed == 1 {
		fmt.Fprintln(g.stderr, "fw doctor: 1 problem")
		return exitFailed
	}
	if d.failed > 1 {
		fmt.Fprintf(g.stderr, "fw doctor: %d problems\n", d.failed)
		return exitFailed
	}
	return exitOK
}

// checkConfig loads the configuration. On failure the other checks use
// the defaults, the FW_* variables, and the global flags, so they still
// run.
func (d *doctor) checkConfig() *fw.Config {
	path := d.g.config
	if path == "" {
		path = config.FindIn(d.g.home)
	}
	cfg, err := d.g.loadConfig()
	if err != nil {
		d.fail("configuration: %v", err)
		fmt.Fprintln(d.g.stdout, "      the checks below use the defaults and the FW_* variables")
		if cfg, err = config.LoadWith("", d.g.override); err != nil {
			cfg = fw.DefaultConfig()
			d.g.override(cfg)
		}
		if cfg.Home == "" {
			cfg.Home = fw.DefaultHome()
		}
		return cfg
	}
	if path == "" {
		d.ok("configuration: no fw.yaml; the defaults and the FW_* variables apply (fw init writes one)")
	} else {
		d.ok("configuration: %s", path)
	}
	return cfg
}

// checkHome makes the home directory and writes a file in it.
func (d *doctor) checkHome(cfg *fw.Config) {
	if err := writable(cfg.Home); err != nil {
		d.fail("home: %s is not writable: %v", cfg.Home, err)
		return
	}
	d.ok("home: %s is writable", cfg.Home)
}

// checkTime prints the configured time zone, the current time in it, and
// the gap that starts a time note. A zone that does not resolve fails the
// line. The configuration check also names such a zone.
// checkToolUser reports the user of the processes of tools (tools.user,
// docs/ARCHITECTURE.md section 7.12), with the default of ax inside a
// Task, as ax.ApplyToConfig sets it for fw serve: only for root with the
// capabilities that tools.user needs.
func (d *doctor) checkToolUser(cfg *fw.Config) {
	spec := cfg.Tools.User
	if spec == "" && ax.Detect() && os.Geteuid() == 0 && len(fw.MissingToolCaps()) == 0 {
		spec = ax.DefaultToolUser
	}
	u, err := fw.LookupToolUser(spec)
	switch {
	case err != nil:
		d.fail("tools user: %v", err)
	case u == nil:
		d.ok("tools user: none; the processes of tools run as the user of fw")
	default:
		d.ok("tools user: %s", u)
	}
}

func (d *doctor) checkTime(cfg *fw.Config) {
	loc, name, err := fw.LoadZone(cfg.Time.Zone)
	if err != nil {
		d.fail("time: time.zone (FW_TIME_ZONE) %q does not resolve: %v", cfg.Time.Zone, err)
		return
	}
	notes := "notes off"
	if cfg.Time.NoteAfter > 0 {
		notes = "notes after " + shortDuration(cfg.Time.NoteAfter)
	}
	d.ok("time: %s, %s, %s", name, fw.FormatTime(time.Now().In(loc)), notes)
}

// shortDuration writes a duration without zero tails: 10m, not 10m0s.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// writable makes dir and writes and removes one file in it.
func writable(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".fw-doctor-*")
	if err != nil {
		return fmt.Errorf("%w: %w", errNotWritable, err)
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// maxDocumentBytes bounds a discovery document or a bundle that a check
// reads.
const maxDocumentBytes = 1 << 20

// checkAuth prints the authentication modes and checks each mode that
// reaches something: oidc reads the discovery document of the issuer,
// spiffe reads the bundle or finds the Workload API socket, and token
// looks the token up without printing it.
func (d *doctor) checkAuth(ctx context.Context, cfg *fw.Config) {
	modes := cfg.Auth.Modes()
	switch {
	case slices.Contains(modes, "none"):
		d.ok("auth: modes %s (every caller is anonymous; keep the listener private)", strings.Join(modes, ", "))
	default:
		d.ok("auth: modes %s", strings.Join(modes, ", "))
	}
	for _, m := range modes {
		switch m {
		case "token":
			d.checkAuthToken(ctx, cfg)
		case "oidc":
			d.checkAuthOIDC(ctx, cfg.Auth.OIDC)
		case "spiffe":
			d.checkAuthSPIFFE(ctx, cfg.Auth.SPIFFE)
		}
	}
}

// checkAuthToken reports whether the token secret is set. A missing token
// stops fw serve at start, and fw run does not need it, so the line is
// not a failure.
func (d *doctor) checkAuthToken(ctx context.Context, cfg *fw.Config) {
	env := firstNonEmpty(cfg.Auth.TokenEnv, "FW_API_TOKEN")
	secrets, err := fw.OpenSecrets(ctx, cfg)
	if err != nil {
		secrets = fw.EnvSecrets{}
	}
	sctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	if v, err := secrets.Get(sctx, env); err == nil && v != "" {
		d.ok("auth token: %s is set", env)
		return
	}
	d.ok("auth token: %s is not set; fw serve needs it, and fw run does not", env)
}

// checkAuthOIDC fetches the discovery document of the issuer and checks
// that it names the issuer and a jwks_uri. It uses net/http alone, so the
// check works without the oidc driver.
func (d *doctor) checkAuthOIDC(ctx context.Context, o *fw.OIDCConfig) {
	if o == nil || strings.TrimSpace(o.Issuer) == "" {
		d.fail("auth oidc: auth.oidc.issuer is not set")
		return
	}
	issuer := strings.TrimRight(strings.TrimSpace(o.Issuer), "/")
	client, err := httpClient(o.CA, d.timeout)
	if err != nil {
		d.fail("auth oidc: %v", err)
		return
	}
	url := issuer + "/.well-known/openid-configuration"
	body, err := fetch(ctx, client, url)
	if err != nil {
		d.fail("auth oidc: the discovery document of %s is not reachable: %v", issuer, err)
		return
	}
	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		d.fail("auth oidc: %s is not a JSON document: %v", url, err)
		return
	}
	if doc.JWKSURI == "" {
		d.fail("auth oidc: the discovery document of %s lists no jwks_uri", issuer)
		return
	}
	if got := strings.TrimRight(doc.Issuer, "/"); got != "" && got != issuer {
		d.fail("auth oidc: the discovery document names the issuer %s, not %s; tokens carry the first", got, issuer)
		return
	}
	d.ok("auth oidc: %s serves its discovery document with jwks_uri %s", issuer, doc.JWKSURI)
}

// checkAuthSPIFFE reads the bundle, or finds the Workload API socket.
func (d *doctor) checkAuthSPIFFE(ctx context.Context, s *fw.SPIFFEConfig) {
	if s == nil || strings.TrimSpace(s.TrustDomain) == "" {
		d.fail("auth spiffe: auth.spiffe.trustDomain is not set")
		return
	}
	if s.Bundle != "" {
		d.checkSPIFFEBundle(ctx, s)
		return
	}
	socket := firstNonEmpty(s.Socket, os.Getenv("SPIFFE_ENDPOINT_SOCKET"))
	if socket == "" {
		d.fail("auth spiffe: no bundle and no Workload API socket; set auth.spiffe.bundle, auth.spiffe.socket, or SPIFFE_ENDPOINT_SOCKET")
		return
	}
	if addr, ok := strings.CutPrefix(socket, "tcp://"); ok {
		c, err := net.DialTimeout("tcp", addr, d.timeout)
		if err != nil {
			d.fail("auth spiffe: the Workload API at %s does not answer: %v", socket, err)
			return
		}
		_ = c.Close()
		d.ok("auth spiffe: the Workload API at %s answers, trust domain %s", socket, s.TrustDomain)
		return
	}
	path := strings.TrimPrefix(socket, "unix://")
	if _, err := os.Stat(path); err != nil {
		d.fail("auth spiffe: the Workload API socket %s does not exist: %v", path, err)
		return
	}
	d.ok("auth spiffe: the Workload API socket %s exists, trust domain %s", path, s.TrustDomain)
}

// checkSPIFFEBundle reads the bundle from its file or URL and checks that
// it is a JWKS document with keys.
func (d *doctor) checkSPIFFEBundle(ctx context.Context, s *fw.SPIFFEConfig) {
	var body []byte
	var err error
	if strings.HasPrefix(s.Bundle, "http://") || strings.HasPrefix(s.Bundle, "https://") {
		var client *http.Client
		if client, err = httpClient("", d.timeout); err == nil {
			body, err = fetch(ctx, client, s.Bundle)
		}
	} else {
		body, err = os.ReadFile(s.Bundle)
	}
	if err != nil {
		d.fail("auth spiffe: the bundle %s does not load: %v", s.Bundle, err)
		return
	}
	var doc struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		d.fail("auth spiffe: the bundle %s is not a JWKS document: %v", s.Bundle, err)
		return
	}
	if doc.Keys == nil {
		d.fail("auth spiffe: the bundle %s has no keys list", s.Bundle)
		return
	}
	if len(doc.Keys) == 0 {
		d.fail("auth spiffe: the bundle %s holds no key", s.Bundle)
		return
	}
	d.ok("auth spiffe: the bundle %s holds %d keys, trust domain %s", s.Bundle, len(doc.Keys), s.TrustDomain)
}

// httpClient returns a client with the timeout, trusting the PEM bundle
// at caPath when it is set.
func httpClient(caPath string, timeout time.Duration) (*http.Client, error) {
	c := &http.Client{Timeout: timeout}
	if caPath == "" {
		return c, nil
	}
	pem, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read the CA bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("the CA bundle %s holds no certificate", caPath)
	}
	c.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	return c, nil
}

// fetch reads a document of at most maxDocumentBytes with one GET.
func fetch(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxDocumentBytes))
}

// checkTLS loads the certificate of the file provider, or builds the
// configuration of a registered provider.
func (d *doctor) checkTLS(ctx context.Context, cfg *fw.Config) {
	provider := strings.ToLower(strings.TrimSpace(cfg.TLS.Provider))
	switch provider {
	case "":
		d.ok("tls: off; the listener speaks plain HTTP with unencrypted HTTP/2 (set tls.provider for HTTPS)")
		return
	case "file":
		cert, err := tls.LoadX509KeyPair(cfg.TLS.Cert, cfg.TLS.Key)
		if err != nil {
			d.fail("tls: the certificate %s and the key %s do not load: %v", cfg.TLS.Cert, cfg.TLS.Key, err)
			return
		}
		if cfg.TLS.ClientCA != "" {
			if _, err := httpClient(cfg.TLS.ClientCA, d.timeout); err != nil {
				d.fail("tls: clientCA: %v", err)
				return
			}
		}
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			d.ok("tls: the certificate %s loads", cfg.TLS.Cert)
			return
		}
		if time.Now().After(leaf.NotAfter) {
			d.fail("tls: the certificate %s expired on %s", cfg.TLS.Cert, leaf.NotAfter.Format("2006-01-02"))
			return
		}
		d.ok("tls: the certificate %s loads, for %s, until %s", cfg.TLS.Cert, strings.Join(certNames(leaf), ", "), leaf.NotAfter.Format("2006-01-02"))
		return
	}
	secrets, err := fw.OpenSecrets(ctx, cfg)
	if err != nil {
		secrets = fw.EnvSecrets{}
	}
	tctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	if _, err := fw.NewTLSConfig(tctx, cfg, secrets); err != nil {
		d.fail("tls: the provider %s does not build a configuration: %v", provider, err)
		return
	}
	d.ok("tls: the provider %s builds a configuration", provider)
}

// certNames lists the DNS names and IP addresses of a certificate, or its
// common name.
func certNames(c *x509.Certificate) []string {
	names := append([]string(nil), c.DNSNames...)
	for _, ip := range c.IPAddresses {
		names = append(names, ip.String())
	}
	if len(names) == 0 && c.Subject.CommonName != "" {
		names = append(names, c.Subject.CommonName)
	}
	if len(names) == 0 {
		names = append(names, "no name")
	}
	return names
}

// checkAccess prints the access default and the number of admins. A closed
// default with no admin locks everyone out of the agents, which is a
// failure. A closed default that does not name the local principal is a
// note, because fw run and fw agents then need a role on each agent.
func (d *doctor) checkAccess(cfg *fw.Config) {
	if accessExternal(cfg) {
		d.ok("access: %s decides for every caller; access.default, access.admins, and access.creators do not apply, and the access block of each profile goes to it as data", controlPlane(cfg))
		return
	}
	closed := accessClosed(cfg)
	word := "open"
	if closed {
		word = "closed"
	}
	n := len(cfg.Access.Admins)
	admins := fmt.Sprintf("%d admins", n)
	if n == 1 {
		admins = "1 admin"
	}
	switch {
	case closed && n == 0:
		d.fail("access: default closed, 0 admins; nobody can manage the agents until access.admins names one, for example local:*")
	case closed && !isAdmin(cfg, localPrincipal()):
		d.ok("access: default %s, %s; %s is not one, so fw run and fw agents need a role on each agent", word, admins, localPrincipal().Subject)
	default:
		d.ok("access: default %s, %s", word, admins)
	}
}

// checkExternal prints one line for each external decision point:
// policy.external, and the control plane of a linked package, such as
// workbench.access.external. It builds an agent that reaches no model and
// no tool source, and asks each decision point whether it is ready. A
// decision point on the bus has no probe, so its line says that fw doctor
// cannot make sure that it answers.
func (d *doctor) checkExternal(ctx context.Context, cfg *fw.Config) {
	if cfg.Policy.External == nil && !accessExternal(cfg) {
		return
	}
	c := *cfg // newAgent changes the bus settings, and the bus check reads them
	a, err := newAgent(ctx, &c, modeManage)
	if err != nil {
		d.fail("external decisions: %v", err)
		return
	}
	defer closeAgent(a)
	pctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	for _, st := range a.ProbeExternal(pctx) {
		key := st.Use + ".external"
		switch {
		case !st.Probed:
			d.ok("%s: decided by %s; this transport has no probe, so fw doctor cannot make sure that it answers", key, st.Target)
		case st.Err != nil:
			d.fail("%s: %s is not ready: %v", key, st.Target, st.Err)
		default:
			d.ok("%s: decided by %s, which is ready", key, st.Target)
		}
	}
}

// credentialNames returns the secrets that the provider of the configured
// model reads, in order: providers.<name>.apiKeyEnv when set, else the
// default names of the adapter.
func credentialNames(cfg *fw.Config) []string {
	provider, _ := fw.SplitSelector(cfg.Model)
	if p := cfg.ProviderNamed(provider); p.APIKeyEnv != "" {
		return []string{p.APIKeyEnv}
	}
	switch provider {
	case "anthropic":
		return []string{"ANTHROPIC_API_KEY"}
	case "gemini", "google":
		return []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}
	case "openai":
		return []string{"OPENAI_API_KEY"}
	}
	return nil
}

// checkCredential opens the secrets driver and looks the credential up.
// It never prints the value.
func (d *doctor) checkCredential(ctx context.Context, cfg *fw.Config) fw.Secrets {
	secrets, err := fw.OpenSecrets(ctx, cfg)
	if err != nil {
		d.fail("credential: the secrets driver %q does not open: %v", cfg.Secrets.Driver, err)
		return nil
	}
	if cfg.Model == "" {
		d.ok("credential: no model is configured, so there is no model credential to check")
		return secrets
	}
	names := credentialNames(cfg)
	if len(names) == 0 {
		provider, _ := fw.SplitSelector(cfg.Model)
		d.ok("credential: no known secret name for the provider %q; set providers.%s.apiKeyEnv to check one", provider, provider)
		return secrets
	}
	for _, n := range names {
		cctx, cancel := context.WithTimeout(ctx, d.timeout)
		v, err := secrets.Get(cctx, n)
		cancel()
		if err == nil && v != "" {
			d.ok("credential: %s is set", n)
			return secrets
		}
	}
	d.fail("credential: %s is not set (secrets driver %s); export it, or set providers.<name>.apiKeyEnv to the name in use", strings.Join(names, " or "), cfg.Secrets.Driver)
	return secrets
}

// checkModel opens the model and, when the adapter lists models, checks
// that the configured model is in the list.
func (d *doctor) checkModel(ctx context.Context, cfg *fw.Config, secrets fw.Secrets) {
	if secrets == nil {
		secrets = fw.EnvSecrets{}
	}
	if cfg.Model == "" {
		d.ok("model: none configured; this process serves and refuses every run until model, FW_MODEL, or the profile of a run names one")
		return
	}
	m, err := openModel(ctx, cfg.Model, cfg, secrets)
	if err != nil {
		d.fail("model: %s does not open: %v", cfg.Model, err)
		return
	}
	if c, ok := m.(io.Closer); ok {
		defer c.Close()
	}
	lister, ok := m.(modelLister)
	if !ok {
		d.ok("model: %s opens", cfg.Model)
		return
	}
	mctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	list, err := lister.Models(mctx)
	if err != nil {
		d.fail("model: %s opens, but the provider does not list its models: %v", cfg.Model, err)
		return
	}
	_, name := fw.SplitSelector(cfg.Model)
	if slices.Contains(list, name) || slices.Contains(list, cfg.Model) {
		d.ok("model: %s opens and is one of the %d models of the provider", cfg.Model, len(list))
		return
	}
	d.fail("model: %s is not one of the %d models of the provider; close names: %s", cfg.Model, len(list), strings.Join(closeNames(name, list, 5), ", "))
}

// closeNames returns up to n names from list that share a prefix with
// name, else the first n names.
func closeNames(name string, list []string, n int) []string {
	var out []string
	prefix := name
	if len(prefix) > 6 {
		prefix = prefix[:6]
	}
	for _, m := range list {
		if strings.HasPrefix(m, prefix) {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		out = list
	}
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// checkToolSources opens every tool source and lists the tools of each,
// with a timeout per source.
func (d *doctor) checkToolSources(ctx context.Context, cfg *fw.Config, secrets fw.Secrets) {
	if secrets == nil {
		secrets = fw.EnvSecrets{}
	}
	sources, err := openToolSources(ctx, cfg, secrets)
	if err != nil {
		d.fail("tool sources: %v", err)
		return
	}
	if len(sources) == 0 {
		d.ok("tool sources: none configured (tools.mcp and tools.peers are empty)")
		return
	}
	for i, s := range sources {
		name := fmt.Sprintf("source %d", i+1)
		if n, ok := s.(namer); ok && n.Name() != "" {
			name = n.Name()
		}
		tctx, cancel := context.WithTimeout(ctx, d.timeout)
		tools, err := s.Tools(tctx)
		cancel()
		if c, ok := s.(io.Closer); ok {
			_ = c.Close()
		}
		if err != nil {
			d.fail("tool source %s: tools/list failed: %v", name, err)
			continue
		}
		d.ok("tool source %s: %d tools", name, len(tools))
	}
}

// checkTokens reports where the tokens of people come from: a broker that
// tokens.url names (docs/ARCHITECTURE.md section 7.11), or nowhere.
func (d *doctor) checkTokens(cfg *fw.Config) {
	u := strings.TrimSpace(cfg.Tokens.URL)
	if u == "" {
		d.ok("tokens: off; set tokens.url (FW_TOKENS_URL) to get the tokens of people from a broker")
		return
	}
	if p, err := url.Parse(u); err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
		d.fail("tokens: tokens.url (FW_TOKENS_URL) %q is not an http or https URL", u)
		return
	}
	cred := firstNonEmpty(cfg.Tokens.Credential, cfg.Auth.TokenEnv, "FW_API_TOKEN")
	d.ok("tokens: from %s, as %s, notices on %s", u, cred, firstNonEmpty(cfg.Tokens.Subject, fw.DefaultTokensSubject))
}

// checkTelemetry reports where the metrics, the logs, and the traces go
// (docs/ARCHITECTURE.md section 10.5): an OTLP collector that the standard
// variables name, or nowhere beyond the Prometheus route.
func (d *doctor) checkTelemetry() {
	var on []string
	for _, s := range []string{"METRICS", "LOGS", "TRACES"} {
		if strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")) != "" || strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_"+s+"_ENDPOINT")) != "" {
			on = append(on, strings.ToLower(s))
		}
	}
	if len(on) == 0 {
		d.ok("telemetry: no OTLP export; set OTEL_EXPORTER_OTLP_ENDPOINT to push the metrics, the logs, and the traces to a collector")
		return
	}
	endpoint := firstNonEmpty(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"), os.Getenv("OTEL_EXPORTER_OTLP_"+strings.ToUpper(on[0])+"_ENDPOINT"))
	d.ok("telemetry: exports %s over OTLP to %s", strings.Join(on, ", "), strings.TrimSpace(endpoint))
}

// checkBus opens the bus and closes it.
func (d *doctor) checkBus(ctx context.Context, cfg *fw.Config) {
	bctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	b, err := fw.OpenBus(bctx, cfg)
	if err != nil {
		d.fail("bus: %s does not open: %v", cfg.Bus.Driver, err)
		return
	}
	if c, ok := b.(io.Closer); ok {
		_ = c.Close()
	}
	d.ok("bus: %s opens", cfg.Bus.Driver)
}

// checkLedger appends one terminal event under a temporary run id and
// reads it back. The file driver works in a hidden directory under its
// root, which the check removes, so no run is left behind.
func (d *doctor) checkLedger(ctx context.Context, cfg *fw.Config) {
	lctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	var l fw.Ledger
	var err error
	where := cfg.Ledger.Driver
	if cfg.Ledger.Driver == "file" {
		root := cfg.Ledger.Path
		if root == "" {
			root = filepath.Join(cfg.Home, "runs")
		}
		dir := filepath.Join(root, ".doctor")
		defer os.RemoveAll(dir)
		l, err = fw.OpenFileLedger(dir)
		where = "file ledger " + root
	} else {
		l, err = fw.OpenLedger(lctx, cfg)
	}
	if err != nil {
		d.fail("ledger: %s does not open: %v", where, err)
		return
	}
	if c, ok := l.(io.Closer); ok {
		defer c.Close()
	}
	id := "doctor-" + fw.NewID()
	ev := fw.MustEvent(id, fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCanceled, Error: "fw doctor check"})
	ev.Source = "fw://doctor/" + id
	seq, err := l.Append(lctx, ev)
	if err != nil {
		d.fail("ledger: %s does not append: %v", where, err)
		return
	}
	n := 0
	for e, err := range l.Read(lctx, id, 0) {
		if err != nil {
			d.fail("ledger: %s does not read: %v", where, err)
			return
		}
		if e.Seq == seq && e.Type == fw.EvRunFinished {
			n++
		}
	}
	if n != 1 {
		d.fail("ledger: %s read %d events after one append", where, n)
		return
	}
	d.ok("ledger: %s appends and reads", where)
}

// checkMemory reports the scope and the index of the memory (section 7.8),
// and the notes of the default profile.
func (d *doctor) checkMemory(cfg *fw.Config) {
	store, err := memory.New(cfg)
	if err != nil {
		d.fail("memory: %v", err)
		return
	}
	defer store.Close()
	scope := cfg.Tools.Memory.Scope
	if scope == "" {
		scope = fw.MemoryScopeAgent
	}
	notes, err := store.List(context.Background(), store.Scope(fw.RunInfo{}))
	if err != nil {
		d.fail("memory: %v", err)
		return
	}
	d.ok("memory: scope %s, index %s, %d notes of the profile default", scope, memory.IndexDriver, len(notes))
}

// checkSkills reports each skills directory. A directory that tools.skills
// names and that does not exist is a problem; the default directory is
// optional.
func (d *doctor) checkSkills(cfg *fw.Config) {
	explicit := len(cfg.Tools.Skills) > 0
	for _, dir := range cfg.SkillDirs() {
		st, err := os.Stat(dir)
		switch {
		case errors.Is(err, os.ErrNotExist) && explicit:
			d.fail("skills: %s does not exist (tools.skills names it)", dir)
		case errors.Is(err, os.ErrNotExist):
			d.ok("skills: no directory at %s yet; a skill is a directory with a SKILL.md file", dir)
		case err != nil:
			d.fail("skills: %s: %v", dir, err)
		case !st.IsDir():
			d.fail("skills: %s is not a directory", dir)
		default:
			skills, err := fw.LoadSkills(dir)
			if err != nil {
				d.fail("skills: %s: %v", dir, err)
				continue
			}
			d.ok("skills: %d in %s", len(skills), dir)
		}
	}
}
