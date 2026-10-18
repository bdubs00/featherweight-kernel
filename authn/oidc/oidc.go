// Package oidc is the "oidc" authentication mode of fw. A bearer token from
// an OpenID Connect issuer, an ID token or a JWT access token, becomes a
// fw.Principal: the person or the workload that the token names, with the
// groups that the issuer put in the token. The access block of a profile
// then names those people and groups, so individuals and groups own agents.
//
// # Setup
//
// Set auth.mode to oidc and name the issuer. The driver reads the discovery
// document of the issuer, fetches its keys, and checks the signature, the
// issuer, the expiry, and the audience of every token.
//
//	auth:
//	  mode: oidc
//	  oidc:
//	    issuer: https://idp.example.com/realms/platform
//	    audience: [fw-dashboard, https://agent.example.com/mcp]
//	    groupsClaim: groups
//	    nameClaim: email
//	    scopes: [openid, profile, email, groups]
//	    ca: /etc/ssl/idp-ca.pem
//	access:
//	  admins: ["group:platform-admins"]
//	agents:
//	  reviewer:
//	    access:
//	      owners: ["alice@example.com"]
//	      operators: ["group:reviewers"]
//
// The keys under auth.oidc:
//
//   - issuer: the issuer URL. Required. The discovery document lives at
//     <issuer>/.well-known/openid-configuration, and the iss claim of every
//     token has to equal the issuer as that document spells it. A configured
//     issuer that differs from the document by a trailing slash only is
//     accepted.
//   - audience: the audiences that a token carries in its aud claim; one
//     match is enough. List the client id of each application that sends
//     tokens, or the resource URL that access tokens are minted for. When
//     the list is empty, the driver accepts mcp.auth.audience when that is
//     set, and otherwise accepts every audience and logs a warning at start.
//   - groupsClaim: the claim that fills Principal.Groups. The default is
//     groups, with roles as the fallback when the token has no groups claim.
//     The claim holds a list of strings or one string. A name with dots,
//     such as realm_access.roles, walks into nested claims when no claim has
//     the full name.
//   - nameClaim: the claim that fills Principal.Name. Without it, the name
//     is the first of email, preferred_username, name, and sub that is set.
//   - scopes: the scopes that the protected-resource document advertises to
//     clients and that the challenge names.
//   - jwksUrl: the keys endpoint. Set it when the issuer has no discovery
//     document; the driver then skips discovery.
//   - ca: a PEM bundle that signs the certificate of the issuer, as a file
//     path or as the PEM text itself. Without it the system roots apply.
//   - insecure: allow an http issuer. For tests only.
//
// Each key is also a variable: FW_AUTH_MODE, FW_AUTH_OIDC_ISSUER,
// FW_AUTH_OIDC_AUDIENCE (comma-separated), FW_AUTH_OIDC_GROUPS_CLAIM,
// FW_AUTH_OIDC_NAME_CLAIM, FW_AUTH_OIDC_SCOPES (comma-separated),
// FW_AUTH_OIDC_JWKS_URL, FW_AUTH_OIDC_CA, and FW_AUTH_OIDC_INSECURE. The
// access lists are FW_ACCESS_ADMINS and FW_ACCESS_CREATORS, comma-separated.
//
// # The principal
//
// Subject is the sub claim. Name is the nameClaim, or else the first of
// email, preferred_username, name, and sub that is set. Groups come from the
// groupsClaim. Issuer is the iss claim. Scopes come from the scope claim
// split on spaces, or from the scp list. Kind is user, or workload for a
// client-credentials token: a token whose client_id claim equals sub, or
// whose azp claim equals sub while the token has no email claim.
//
// An access entry matches the principal as fw.PrincipalMatches says:
// alice@example.com matches the subject or the name, group:reviewers
// matches a group, kind:workload matches every client-credentials token,
// and issuer:https://idp.example.com/* matches every token from that
// issuer. A glob with * matches any text.
//
// # Issuers
//
// Keycloak puts groups in a token through a mapper. Open the client, or a
// client scope that the client uses, and add a mapper of the type Group
// Membership with the token claim name groups. Turn Full group path off, so
// the group platform-admins arrives as platform-admins and not as
// /platform-admins. The issuer is https://<host>/realms/<realm>. Dex puts
// groups in the token when the client asks for the groups scope, and its
// connector supplies them; the issuer is the issuer of its configuration.
//
// Okta puts groups in a token only when the authorization server has a
// claim for them and the client asks for the scope. In the authorization
// server, add a claim named groups of the type Groups with a filter that
// matches the groups to include, in both the ID token and the access
// token, and add a scope named groups. The client requests that scope. The
// issuer is https://<org>.okta.com/oauth2/default for the default
// authorization server.
//
// Microsoft Entra ID names groups by their object id, not by their name.
// In the app registration, open Token configuration and add the groups
// claim. An access entry then names the id, such as
// group:0f1b2c3d-4e5f-6a7b-8c9d-0e1f2a3b4c5d. Set
// nameClaim: preferred_username, because an Entra token often has no email
// claim and its sub claim is an id that differs per application. The
// issuer is https://login.microsoftonline.com/<tenant-id>/v2.0, and the
// audience is the application (client) id for an ID token, or
// api://<client-id> for an access token minted for an exposed API.
//
// # Clients
//
// A client without a valid token gets 401 with a WWW-Authenticate header
// that names the realm, the protected-resource document (RFC 9728), and the
// scopes. The document says which authorization server to use, so an MCP
// client runs the OAuth flow by itself. ResourceMetadata builds the
// document, and the MCP surface serves it at the URL that the challenge
// names: the path /.well-known/oauth-protected-resource, with the path of
// the resource appended. The resource is mcp.auth.audience, or a2a.url
// when that is empty.
//
// # Operation
//
// Discovery runs at start. When the issuer is not reachable, the process
// starts anyway, logs one warning, answers 401 to every request, and
// retries discovery in the background with backoff until it succeeds. A
// verified token stays in a cache until it expires, so a dashboard that
// polls does not cost a verification per call, and a rejected token stays
// in the cache for a short time with its reason. No token enters a log
// line, an error, or a principal.
package oidc

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func init() { fw.RegisterAuthn(Mode, New) }

// Mode is the auth.mode value that selects this driver.
const Mode = "oidc"

const (
	realm        = "fw"
	expiryLeeway = 60 * time.Second // clock skew allowed on the exp claim
	cacheSize    = 1024             // verified and rejected tokens kept
	negativeTTL  = 30 * time.Second // how long a rejection stays cached
	httpTimeout  = 15 * time.Second // one discovery or keys request
	retryMin     = time.Second      // first wait before discovery runs again
	retryMax     = time.Minute      // longest wait between discovery runs
)

// notReady is the reason given while discovery has not succeeded.
const notReady = "OIDC discovery of the issuer has not completed; retry later"

// readyWait is how long a request waits for the first discovery before it
// gets 401 with notReady. A process that wakes in a sandbox, as a Task
// does when the router wakes it for a forwarded request, reads the
// discovery document in its first second, and the request that woke it
// must not fail for that. Tests shorten it.
var readyWait = 5 * time.Second

// allAlgs lists the signing algorithms accepted with a fixed keys
// endpoint, where no discovery document says which ones the issuer uses.
var allAlgs = []string{
	gooidc.RS256, gooidc.RS384, gooidc.RS512,
	gooidc.ES256, gooidc.ES384, gooidc.ES512,
	gooidc.PS256, gooidc.PS384, gooidc.PS512,
	gooidc.EdDSA,
}

// Authn verifies bearer tokens from one OpenID Connect issuer. It
// implements fw.Authn and is safe for concurrent use.
type Authn struct {
	issuer      string
	audience    []string // accepted audiences; empty accepts every audience
	groupsClaim string
	nameClaim   string
	scopes      []string
	metadata    string // the URL of the protected-resource document, or ""
	client      *http.Client
	log         *slog.Logger
	now         func() time.Time

	verifier  atomic.Pointer[gooidc.IDTokenVerifier] // nil until discovery succeeds
	ready     chan struct{}                          // closed when the verifier is in place
	readyOnce sync.Once
	cache     *cache
	verified  atomic.Int64 // tokens that reached the verifier, for tests and stats
}

// New builds the driver from cfg.Auth.OIDC. It returns an error for a
// configuration that cannot work. An issuer that is not reachable is not
// such an error: New logs a warning, and the driver retries discovery in
// the background while it answers 401.
func New(ctx context.Context, cfg *fw.Config, _ fw.Secrets) (fw.Authn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg == nil || cfg.Auth.OIDC == nil || strings.TrimSpace(cfg.Auth.OIDC.Issuer) == "" {
		return nil, errors.New("oidc: auth.oidc.issuer is not set; set it to the URL of the OpenID Connect issuer, or set FW_AUTH_OIDC_ISSUER")
	}
	oc := cfg.Auth.OIDC
	issuer := strings.TrimSpace(oc.Issuer)
	if err := checkURL("auth.oidc.issuer", issuer, oc.Insecure); err != nil {
		return nil, err
	}
	jwksURL := strings.TrimSpace(oc.JWKSURL)
	if jwksURL != "" {
		if err := checkURL("auth.oidc.jwksUrl", jwksURL, oc.Insecure); err != nil {
			return nil, err
		}
	}
	client, err := newHTTPClient(oc.CA)
	if err != nil {
		return nil, err
	}
	log := slog.Default().With("component", "oidc")
	a := &Authn{
		issuer:      issuer,
		groupsClaim: strings.TrimSpace(oc.GroupsClaim),
		nameClaim:   strings.TrimSpace(oc.NameClaim),
		scopes:      cleanList(oc.Scopes),
		metadata:    ResourceMetadataURL(cfg),
		client:      client,
		log:         log,
		now:         time.Now,
		cache:       newCache(cacheSize),
		ready:       make(chan struct{}),
	}
	switch {
	case len(cleanList(oc.Audience)) > 0:
		a.audience = cleanList(oc.Audience)
	case strings.TrimSpace(cfg.MCP.Auth.Audience) != "":
		a.audience = []string{strings.TrimSpace(cfg.MCP.Auth.Audience)}
		log.Info("oidc accepts the audience of mcp.auth.audience because auth.oidc.audience is empty", "issuer", issuer, "audience", a.audience[0])
	default:
		log.Warn("oidc accepts every audience because auth.oidc.audience is empty; set it to the client id or the resource URL that tokens are minted for", "issuer", issuer)
	}
	if oc.Insecure {
		log.Warn("oidc allows an http issuer because auth.oidc.insecure is set", "issuer", issuer)
	}
	if jwksURL != "" {
		keys := gooidc.NewRemoteKeySet(gooidc.ClientContext(context.Background(), client), jwksURL)
		a.verifier.Store(gooidc.NewVerifier(issuer, keys, a.verifierConfig(allAlgs)))
		a.markReady()
		log.Info("oidc verifies tokens against a fixed keys endpoint", "issuer", issuer, "jwksUrl", jwksURL)
		return a, nil
	}
	dctx, cancel := context.WithTimeout(ctx, httpTimeout)
	err = a.discover(dctx)
	cancel()
	if err == nil {
		log.Info("oidc discovery succeeded", "issuer", issuer)
		return a, nil
	}
	log.Warn("oidc discovery failed; every request gets 401 until it succeeds, and the driver retries in the background", "issuer", issuer, "err", err)
	go a.retryDiscovery(ctx)
	return a, nil
}

// checkURL rejects a URL that is not absolute, has no host, or uses http
// without insecure.
func checkURL(key, raw string, insecure bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("oidc: %s %q is not an https URL", key, raw)
	}
	if u.Scheme == "http" && !insecure {
		return fmt.Errorf("oidc: %s %q uses http; use https, or set auth.oidc.insecure for a test", key, raw)
	}
	return nil
}

// newHTTPClient builds the client for discovery and keys requests. ca is
// empty, a path to a PEM bundle, or the PEM text itself.
func newHTTPClient(ca string) (*http.Client, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if ca = strings.TrimSpace(ca); ca != "" {
		pem := []byte(ca)
		if !strings.HasPrefix(ca, "-----BEGIN") {
			b, err := os.ReadFile(ca)
			if err != nil {
				return nil, fmt.Errorf("oidc: read auth.oidc.ca: %w", err)
			}
			pem = b
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("oidc: auth.oidc.ca holds no certificate")
		}
		tc.RootCAs = pool
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Client{
		Timeout: httpTimeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           dialer.DialContext,
			TLSClientConfig:       tc,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConns:          4,
		},
	}, nil
}

// verifierConfig is the go-oidc configuration. The audience check is done
// here, not in go-oidc, because go-oidc accepts one client id and fw
// accepts a list. The clock runs expiryLeeway behind, which is the leeway
// on the exp claim.
func (a *Authn) verifierConfig(algs []string) *gooidc.Config {
	return &gooidc.Config{
		SkipClientIDCheck:    true,
		SupportedSigningAlgs: algs,
		Now:                  func() time.Time { return a.now().Add(-expiryLeeway) },
	}
}

// discover reads the discovery document and installs the verifier. An
// issuer that the document spells with or without a trailing slash, unlike
// the configuration, is used as the document spells it, because tokens
// carry that spelling.
func (a *Authn) discover(ctx context.Context) error {
	ctx = gooidc.ClientContext(ctx, a.client)
	p, err := gooidc.NewProvider(ctx, a.issuer)
	var mismatch *gooidc.IssuerMismatchError
	if errors.As(err, &mismatch) && strings.TrimSuffix(mismatch.Discovered, "/") == strings.TrimSuffix(mismatch.Provided, "/") {
		a.log.Info("oidc uses the issuer as the discovery document spells it", "configured", mismatch.Provided, "issuer", mismatch.Discovered)
		p, err = gooidc.NewProvider(ctx, mismatch.Discovered)
	}
	if err != nil {
		return fmt.Errorf("oidc: discovery at %s: %w", a.issuer, err)
	}
	keysCtx := gooidc.ClientContext(context.Background(), a.client)
	a.verifier.Store(p.VerifierContext(keysCtx, a.verifierConfig(nil)))
	a.markReady()
	return nil
}

// retryDiscovery runs discovery again with backoff until it succeeds or
// ctx ends.
func (a *Authn) retryDiscovery(ctx context.Context) {
	wait := retryMin
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		dctx, cancel := context.WithTimeout(ctx, httpTimeout)
		err := a.discover(dctx)
		cancel()
		if err == nil {
			a.log.Info("oidc discovery succeeded", "issuer", a.issuer)
			return
		}
		wait = min(wait*2, retryMax)
		a.log.Debug("oidc discovery failed again", "issuer", a.issuer, "err", err, "retryIn", wait)
	}
}

// Ready reports whether the driver can verify tokens, which is false until
// discovery succeeds.
func (a *Authn) Ready() bool { return a.verifier.Load() != nil }

// markReady wakes the requests that wait for the first discovery.
func (a *Authn) markReady() { a.readyOnce.Do(func() { close(a.ready) }) }

// waitVerifier returns the verifier, after a wait of at most readyWait for
// the first discovery when the request arrives before it finished. It
// returns nil when discovery is still running after the wait.
func (a *Authn) waitVerifier(ctx context.Context) *gooidc.IDTokenVerifier {
	if v := a.verifier.Load(); v != nil {
		return v
	}
	t := time.NewTimer(readyWait)
	defer t.Stop()
	select {
	case <-a.ready:
	case <-t.C:
	case <-ctx.Done():
	}
	return a.verifier.Load()
}

// Authenticate implements fw.Authn. It reads Authorization: Bearer <token>
// and returns fw.ErrUnauthorized, wrapped with a short reason, for a
// missing header, a token that is not a JWT, or a token that fails to
// verify. The reason never holds token material.
func (a *Authn) Authenticate(r *http.Request) (*fw.Principal, error) {
	raw, ok := bearerToken(r)
	if !ok {
		return nil, unauthorized("no bearer token")
	}
	if !looksLikeJWT(raw) {
		return nil, unauthorized("the token is not a JWT")
	}
	key := sha256.Sum256([]byte(raw))
	if e, ok := a.cache.get(key, a.now()); ok {
		if e.principal != nil {
			return clonePrincipal(e.principal), nil
		}
		return nil, unauthorized(e.reason)
	}
	v := a.waitVerifier(r.Context())
	if v == nil {
		return nil, unauthorized(notReady)
	}
	a.verified.Add(1)
	p, expiry, err := a.verify(r.Context(), v, raw)
	if err != nil {
		reason, definitive := reasonOf(err)
		if definitive {
			a.cache.put(key, entry{reason: reason, until: a.now().Add(negativeTTL)})
		}
		a.log.Debug("oidc rejected a token", "reason", reason, "err", err)
		return nil, unauthorized(reason)
	}
	a.cache.put(key, entry{principal: p, until: expiry})
	return clonePrincipal(p), nil
}

// errAudience is returned when the aud claim holds no accepted audience.
var errAudience = errors.New("oidc: audience not accepted")

// verify checks one token and maps its claims to a principal. It returns
// the expiry of the token for the cache.
func (a *Authn) verify(ctx context.Context, v *gooidc.IDTokenVerifier, raw string) (*fw.Principal, time.Time, error) {
	tok, err := v.Verify(ctx, raw)
	if err != nil {
		return nil, time.Time{}, err
	}
	if len(a.audience) > 0 && !intersects(tok.Audience, a.audience) {
		return nil, time.Time{}, errAudience
	}
	var claims map[string]any
	if err := tok.Claims(&claims); err != nil {
		return nil, time.Time{}, fmt.Errorf("oidc: read claims: %w", err)
	}
	p := principalFromClaims(claims, a.groupsClaim, a.nameClaim)
	if p.Subject == "" {
		return nil, time.Time{}, errors.New("oidc: no sub claim")
	}
	return p, tok.Expiry, nil
}

// unauthorized wraps fw.ErrUnauthorized with a reason.
func unauthorized(reason string) error {
	return fmt.Errorf("%w: oidc: %s", fw.ErrUnauthorized, reason)
}

// reasonOf maps a verification error to a short reason with no token
// material. definitive says whether the same token fails again for sure,
// so the rejection can be cached; a keys endpoint that is not reachable is
// not definitive.
func reasonOf(err error) (reason string, definitive bool) {
	var expired *gooidc.TokenExpiredError
	msg := err.Error()
	switch {
	case errors.As(err, &expired):
		return "token expired", true
	case errors.Is(err, errAudience):
		return "audience not accepted", true
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "verification interrupted", false
	case strings.Contains(msg, "issued by a different provider"):
		return "wrong issuer", true
	case strings.Contains(msg, "unexpected signature algorithm"):
		return "unsupported signing algorithm", true
	case strings.Contains(msg, "malformed jwt"), strings.Contains(msg, "not signed"), strings.Contains(msg, "multiple signatures"):
		return "malformed token", true
	case strings.Contains(msg, "fetching keys"):
		return "the keys of the issuer are not reachable", false
	case strings.Contains(msg, "failed to verify"):
		return "bad signature", true
	case strings.Contains(msg, "nbf"):
		return "token not yet valid", false
	case strings.Contains(msg, "no sub claim"):
		return "no sub claim", true
	}
	return "token rejected", false
}

// Challenge implements fw.Authn. It writes 401 with a WWW-Authenticate
// header of the form
//
//	Bearer realm="fw", resource_metadata="<url>", scope="<scopes>"
//
// where resource_metadata and scope are present when configured. When the
// request carried a token, the header also holds error="invalid_token" and
// an error_description with the reason, as RFC 6750 says. The response has
// no body, so a surface adds its own.
func (a *Authn) Challenge(w http.ResponseWriter, r *http.Request) {
	parts := []string{`Bearer realm="` + realm + `"`}
	if a.metadata != "" {
		parts = append(parts, `resource_metadata="`+a.metadata+`"`)
	}
	if len(a.scopes) > 0 {
		parts = append(parts, `scope="`+strings.Join(a.scopes, " ")+`"`)
	}
	if raw, ok := bearerToken(r); ok {
		parts = append(parts, `error="invalid_token"`)
		if desc := a.describe(raw); desc != "" {
			parts = append(parts, `error_description="`+desc+`"`)
		}
	}
	w.Header().Set("WWW-Authenticate", strings.Join(parts, ", "))
	w.WriteHeader(http.StatusUnauthorized)
}

// describe returns the reason a token was rejected, for the challenge, or
// "" when the reason is not known. It never verifies a token.
func (a *Authn) describe(raw string) string {
	if !looksLikeJWT(raw) {
		return "the token is not a JWT"
	}
	if a.verifier.Load() == nil {
		return notReady
	}
	if e, ok := a.cache.get(sha256.Sum256([]byte(raw)), a.now()); ok && e.principal == nil {
		return e.reason
	}
	return ""
}

// bearerToken returns the token of an Authorization: Bearer header. The
// scheme is matched with case ignored, as HTTP says.
func bearerToken(r *http.Request) (string, bool) {
	if r == nil {
		return "", false
	}
	scheme, rest, ok := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	tok := strings.TrimSpace(rest)
	return tok, tok != ""
}

// looksLikeJWT reports whether s has the shape of a signed JWT: three
// base64url parts, the first two not empty. The signature is checked later.
func looksLikeJWT(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		return false
	}
	for _, p := range parts {
		for _, c := range p {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '=') {
				return false
			}
		}
	}
	return true
}

// intersects reports whether any string is in both lists.
func intersects(have, want []string) bool {
	for _, h := range have {
		for _, w := range want {
			if h == w {
				return true
			}
		}
	}
	return false
}

// cleanList trims the items of a list and drops the empty ones.
func cleanList(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
