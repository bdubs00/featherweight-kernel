package mcp

// This file acts for the person who started a run: an entry with
// auth.oauth.grant user (docs/ARCHITECTURE.md section 7.10).
//
// The person connects once. fw keeps the refresh token in the grant store of
// the agent (fw.GrantStore) and never in a log line or an error. Before a
// call, the broker turns the grant into an access token for the MCP server:
// a refresh with the resource parameter (RFC 8707), or a refresh and then a
// token exchange (RFC 8693) when the entry sets exchange. The broker keeps
// these rules for every authorization server, the strictest included:
//
//   - One refresh of a grant runs at a time in the process, across every
//     entry that shares the grant.
//   - The new refresh token is stored before the call uses the new access
//     token.
//   - A grant that the server refuses is deleted, so the next call asks the
//     person to connect again.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Grant types and token types of the token requests.
const (
	grantRefresh       = "refresh_token"
	grantCode          = "authorization_code"
	grantTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"
	tokenTypeAccess    = "urn:ietf:params:oauth:token-type:access_token"
)

const (
	connectTTL   = 10 * time.Minute // the life of a connection in progress
	metaRetry    = 30 * time.Second // how long a failed discovery is not tried again
	revokeTimout = 10 * time.Second
)

// Errors of the token of a person.
var (
	// errNoGrant means that the person never connected the service, or
	// disconnected it.
	errNoGrant = errors.New("mcp: the person has not connected the service")
	// errGrantExpired means that the authorization server refused the
	// grant, which the broker then deleted.
	errGrantExpired = errors.New("mcp: the authorization server refused the grant")
)

// userAuth is the OAuth setup of one entry with grant user.
type userAuth struct {
	server   string // entry name, for errors and logs
	clientID string
	secret   string // Secrets name of the client secret; "" for a public client
	// signIn is true when the entry uses the client of the browser sign-in
	// (auth.oidc), so every such entry shares the grant of the person.
	signIn bool
	// issuer is the configured authorization server, or the sign-in issuer.
	// Empty means that the protected resource metadata of the MCP server
	// (RFC 9728) names it.
	issuer string
	// oidcIssuer is auth.oidc.issuer. When the authorization server is the
	// sign-in issuer, a connection asks for openid and checks that the
	// person who signs in is the person in fw.
	oidcIssuer   string
	authorizeURL string // configured; discovery fills the rest
	tokenURL     string
	scopes       []string
	offline      bool
	params       map[string]string
	exchange     *fw.TokenExchangeConfig
	resource     string // canonical MCP endpoint URL
	endpoint     *url.URL
	allowHTTP    bool // the sign-in issuer is http and auth.oidc.insecure is set
	http         *http.Client
	secrets      fw.Secrets
	broker       *broker
	log          *slog.Logger
	now          func() time.Time

	mu          sync.Mutex
	meta        *userMeta
	metaErr     error
	metaRetryAt time.Time
}

// userMeta is what discovery found about the authorization server.
type userMeta struct {
	issuer     string // the key of the grants; the token endpoint when no issuer is known
	authorize  string
	token      string
	revocation string
	basicAuth  bool
}

// newUserAuth builds the OAuth setup of an entry with grant user. oidc is
// auth.oidc, or nil.
func newUserAuth(c fw.MCPServerConfig, oidc *fw.OIDCConfig, b *broker, secrets fw.Secrets, log *slog.Logger) (*userAuth, error) {
	o := c.Auth.OAuth
	endpoint, err := url.Parse(c.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("endpoint is not a URL: %w", err)
	}
	u := &userAuth{
		server:       c.Name,
		clientID:     o.ClientID,
		secret:       o.ClientSecret,
		issuer:       strings.TrimSpace(o.Issuer),
		authorizeURL: o.AuthorizeURL,
		tokenURL:     o.TokenURL,
		scopes:       slices.Clone(o.Scopes),
		offline:      o.OfflineOf(),
		params:       o.AuthParams,
		exchange:     o.Exchange,
		resource:     canonicalResource(endpoint),
		endpoint:     endpoint,
		secrets:      secrets,
		broker:       b,
		log:          log,
		now:          time.Now,
	}
	if oidc != nil {
		u.oidcIssuer = strings.TrimSpace(oidc.Issuer)
	}
	if u.clientID == "" {
		if oidc == nil || strings.TrimSpace(oidc.ClientID) == "" {
			return nil, errors.New("auth.oauth.grant user without auth.oauth.clientId uses the client of the browser sign-in, so auth.oidc.clientId must be set")
		}
		u.signIn = true
		u.clientID = strings.TrimSpace(oidc.ClientID)
		u.secret = oidc.ClientSecret
		u.issuer = u.oidcIssuer
		u.allowHTTP = oidc.Insecure
		hc, err := oidcHTTP(oidc)
		if err != nil {
			return nil, err
		}
		u.http = hc
	} else {
		tc, err := oauthTLS(c.TLS)
		if err != nil {
			return nil, err
		}
		u.http = &http.Client{Transport: baseTransport(tc), Timeout: oauthTimeout}
	}
	for _, raw := range []string{u.authorizeURL, u.tokenURL} {
		if raw == "" {
			continue
		}
		if err := safeURL(raw, u.allowHTTP); err != nil {
			return nil, fmt.Errorf("auth.oauth: %w", err)
		}
	}
	if b != nil {
		b.register(u)
	}
	return u, nil
}

// oidcHTTP returns the client for the sign-in issuer, with the CA and the
// insecure setting of auth.oidc.
func oidcHTTP(o *fw.OIDCConfig) (*http.Client, error) {
	var tc *tls.Config
	if o.CA != "" || o.Insecure {
		tc = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: o.Insecure} //nolint:gosec // development only
		if o.CA != "" {
			pool, err := x509.SystemCertPool()
			if err != nil || pool == nil {
				pool = x509.NewCertPool()
			}
			pem, err := os.ReadFile(o.CA)
			if err != nil {
				return nil, fmt.Errorf("read auth.oidc.ca: %w", err)
			}
			pool.AppendCertsFromPEM(pem)
			tc.RootCAs = pool
		}
	}
	return &http.Client{Transport: baseTransport(tc), Timeout: oauthTimeout}, nil
}

// sameIssuer compares two issuer URLs without a trailing slash.
func sameIssuer(a, b string) bool {
	return a != "" && strings.TrimSuffix(a, "/") == strings.TrimSuffix(b, "/")
}

// staticIssuer returns the issuer that the configuration names, without a
// trailing slash, or "".
func (u *userAuth) staticIssuer() string { return strings.TrimSuffix(u.issuer, "/") }

// sameIdP reports whether the authorization server is the sign-in issuer.
func (u *userAuth) sameIdP(m *userMeta) bool {
	return u.signIn || sameIssuer(m.issuer, u.oidcIssuer)
}

// key returns the grant key of a person.
func (u *userAuth) key(m *userMeta, subject string) fw.GrantKey {
	return fw.GrantKey{Subject: subject, Issuer: m.issuer, ClientID: u.clientID}
}

// ensureMeta discovers the endpoints of the authorization server once. A
// failure is kept for metaRetry, so a server that is down does not slow
// every step of every run.
func (u *userAuth) ensureMeta(ctx context.Context) (*userMeta, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.meta != nil {
		return u.meta, nil
	}
	if u.metaErr != nil && u.now().Before(u.metaRetryAt) {
		return nil, u.metaErr
	}
	m, err := u.discover(ctx)
	if err != nil {
		u.metaErr, u.metaRetryAt = err, u.now().Add(metaRetry)
		return nil, err
	}
	u.meta, u.metaErr = m, nil
	return m, nil
}

// asMetadata is the part of RFC 8414 and OpenID metadata that a connection
// reads.
type asMetadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	RevocationEndpoint    string   `json:"revocation_endpoint"`
	TokenAuthMethods      []string `json:"token_endpoint_auth_methods_supported"`
}

// discover finds the endpoints. The caller holds u.mu.
func (u *userAuth) discover(ctx context.Context) (*userMeta, error) {
	issuer := u.issuer
	configured := u.authorizeURL != "" && u.tokenURL != ""
	if issuer == "" && !configured {
		var prm protectedResource
		var err error
		for _, mu := range wellKnownResourceURLs(u.endpoint) {
			if err = fetchJSON(ctx, u.http, mu, &prm); err == nil {
				break
			}
		}
		if err != nil {
			return nil, fmt.Errorf("mcp: server %s: oauth: read protected resource metadata: %w", u.server, err)
		}
		if prm.Resource != "" && !resourceCovers(prm.Resource, u.resource) {
			return nil, fmt.Errorf("mcp: server %s: oauth: protected resource metadata names resource %q, not %q", u.server, prm.Resource, u.resource)
		}
		if len(prm.AuthorizationServers) == 0 {
			return nil, fmt.Errorf("mcp: server %s: oauth: protected resource metadata lists no authorization server", u.server)
		}
		issuer = prm.AuthorizationServers[0]
	}
	var as asMetadata
	if issuer != "" {
		urls, err := metadataURLs(issuer, u.allowHTTP)
		if err != nil {
			return nil, fmt.Errorf("mcp: server %s: oauth: %w", u.server, err)
		}
		for _, mu := range urls {
			if err = fetchJSON(ctx, u.http, mu, &as); err == nil {
				break
			}
		}
		if err != nil && !configured {
			return nil, fmt.Errorf("mcp: server %s: oauth: read authorization server metadata for %s: %w", u.server, issuer, err)
		}
		if err == nil && as.Issuer != "" && !sameIssuer(as.Issuer, issuer) {
			return nil, fmt.Errorf("mcp: server %s: oauth: authorization server metadata names issuer %q, not %q", u.server, as.Issuer, issuer)
		}
	}
	m := &userMeta{
		issuer:     strings.TrimSuffix(issuer, "/"),
		authorize:  firstOf(u.authorizeURL, as.AuthorizationEndpoint),
		token:      firstOf(u.tokenURL, as.TokenEndpoint),
		revocation: as.RevocationEndpoint,
		basicAuth: len(as.TokenAuthMethods) > 0 &&
			!slices.Contains(as.TokenAuthMethods, "client_secret_post") &&
			slices.Contains(as.TokenAuthMethods, "client_secret_basic"),
	}
	if m.token == "" {
		return nil, fmt.Errorf("mcp: server %s: oauth: the authorization server %s has no token_endpoint", u.server, issuer)
	}
	if m.issuer == "" {
		m.issuer = m.token
	}
	for _, raw := range []string{m.authorize, m.token, m.revocation} {
		if raw == "" {
			continue
		}
		if err := safeURL(raw, u.allowHTTP); err != nil {
			return nil, fmt.Errorf("mcp: server %s: oauth: %w", u.server, err)
		}
	}
	u.log.Info("mcp oauth discovered the authorization server of a person grant", "issuer", m.issuer)
	return m, nil
}

func firstOf(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// metadataURLs returns the metadata URLs of an issuer. allowHTTP accepts a
// plain http issuer outside the machine, for auth.oidc.insecure.
func metadataURLs(issuer string, allowHTTP bool) ([]string, error) {
	u, err := url.Parse(issuer)
	if err == nil && allowHTTP && u.Scheme == "http" {
		u.Scheme = "https"
		urls, err := authServerMetadataURLs(u.String())
		for i := range urls {
			urls[i] = "http" + strings.TrimPrefix(urls[i], "https")
		}
		return urls, err
	}
	return authServerMetadataURLs(issuer)
}

// safeURL refuses an endpoint that would carry credentials over plain http
// outside the machine or the cluster.
func safeURL(raw string, allowHTTP bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("endpoint %q is not a URL", raw)
	}
	if u.Scheme == "https" || (u.Scheme == "http" && (allowHTTP || isLocalHost(u.Hostname()))) {
		return nil
	}
	return fmt.Errorf("endpoint %s must use https", u.Host)
}

// fetchJSON fetches a metadata document.
func fetchJSON(ctx context.Context, hc *http.Client, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", req.URL.Redacted(), resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}

// tokenAnswer is a token endpoint reply. refresh_expires_in is the name
// that Keycloak uses, refresh_token_expires_in the name of others.
type tokenAnswer struct {
	AccessToken           string `json:"access_token"`
	TokenType             string `json:"token_type"`
	ExpiresIn             any    `json:"expires_in"`
	RefreshToken          string `json:"refresh_token"`
	RefreshExpiresIn      any    `json:"refresh_expires_in"`
	RefreshTokenExpiresIn any    `json:"refresh_token_expires_in"`
	IDToken               string `json:"id_token"`
	Scope                 string `json:"scope"`
	Error                 string `json:"error"`
	ErrorDescription      string `json:"error_description"`
}

// oauthError is a refusal of the token endpoint. It holds the status and
// the OAuth error code and description, never a token.
type oauthError struct {
	status int
	code   string
	desc   string
}

func (e *oauthError) Error() string {
	if e.code == "" {
		return fmt.Sprintf("token endpoint replied %d %s", e.status, http.StatusText(e.status))
	}
	if e.desc == "" {
		return "token endpoint refused: " + e.code
	}
	return "token endpoint refused: " + e.code + ": " + e.desc
}

// grantDead reports whether a refused refresh means that the grant is gone:
// invalid_grant, or a 400 or 401 without an error code. A refusal of the
// client, such as invalid_client, is a configuration error and keeps the
// grant.
func grantDead(err error) bool {
	var oe *oauthError
	if !errors.As(err, &oe) {
		return false
	}
	if oe.code == "invalid_grant" {
		return true
	}
	return oe.code == "" && (oe.status == http.StatusBadRequest || oe.status == http.StatusUnauthorized)
}

// send posts a form to an endpoint of the authorization server with the
// client credentials, and returns the status and at most 1 MiB of the body.
func (u *userAuth) send(ctx context.Context, m *userMeta, endpoint string, form url.Values) (int, []byte, error) {
	var secret string
	if u.secret != "" {
		s, err := u.secrets.Get(ctx, u.secret)
		if err != nil {
			return 0, nil, fmt.Errorf("mcp: server %s: oauth: resolve client secret %s: %w", u.server, u.secret, err)
		}
		secret = s
	}
	basic := secret != "" && m.basicAuth
	if !basic {
		form.Set("client_id", u.clientID)
		if secret != "" {
			form.Set("client_secret", secret)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, fmt.Errorf("mcp: server %s: oauth: request: %w", u.server, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if basic {
		req.SetBasicAuth(url.QueryEscape(u.clientID), url.QueryEscape(secret))
	}
	resp, err := u.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("mcp: server %s: oauth: request: %w", u.server, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("mcp: server %s: oauth: read answer: %w", u.server, err)
	}
	return resp.StatusCode, body, nil
}

// post sends one token request and decodes the answer.
func (u *userAuth) post(ctx context.Context, m *userMeta, endpoint string, form url.Values) (*tokenAnswer, error) {
	status, body, err := u.send(ctx, m, endpoint, form)
	if err != nil {
		return nil, err
	}
	var ta tokenAnswer
	derr := json.Unmarshal(body, &ta)
	if status != http.StatusOK {
		oe := &oauthError{status: status}
		if derr == nil {
			oe.code, oe.desc = ta.Error, ta.ErrorDescription
		}
		return nil, fmt.Errorf("mcp: server %s: oauth: %w", u.server, oe)
	}
	if derr != nil {
		return nil, fmt.Errorf("mcp: server %s: oauth: decode token response: %w", u.server, derr)
	}
	return &ta, nil
}

// access checks the access token of an answer.
func (u *userAuth) access(ta *tokenAnswer) error {
	if ta.AccessToken == "" {
		return fmt.Errorf("mcp: server %s: oauth: token response has no access_token", u.server)
	}
	if ta.TokenType != "" && !strings.EqualFold(ta.TokenType, "bearer") {
		return fmt.Errorf("mcp: server %s: oauth: token type %q is not Bearer", u.server, ta.TokenType)
	}
	return nil
}

// exchangeFor turns a base token into a token for the MCP server with RFC
// 8693. The resource parameter goes along only when the entry names no
// audience, because a server that needs exchange often rejects resource.
func (u *userAuth) exchangeFor(ctx context.Context, m *userMeta, base string) (*tokenAnswer, error) {
	form := url.Values{
		"grant_type":           {grantTokenExchange},
		"subject_token":        {base},
		"subject_token_type":   {tokenTypeAccess},
		"requested_token_type": {tokenTypeAccess},
	}
	if a := u.exchange.Audience; a != "" {
		form.Set("audience", a)
	} else {
		form.Set("resource", u.resource)
	}
	if len(u.exchange.Scopes) > 0 {
		form.Set("scope", strings.Join(u.exchange.Scopes, " "))
	}
	ta, err := u.post(ctx, m, m.token, form)
	if err != nil {
		return nil, err
	}
	if err := u.access(ta); err != nil {
		return nil, err
	}
	return ta, nil
}

// broker holds the access tokens of people in memory and runs every
// refresh. newSources makes one per process and gives it to every entry
// with grant user, so entries that share a grant share its lock.
type broker struct {
	now func() time.Time

	mu        sync.Mutex
	locks     map[fw.GrantKey]*sync.Mutex
	tokens    map[tokenKey]cachedToken
	pending   map[fw.GrantKey]fw.Grant // refreshed grants that the store did not take
	resources map[clientKey][]string   // the MCP URLs of the entries that share an issuer and a client
}

// tokenKey names one access token: a grant and the entry that uses it.
// The base token of exchange mode has an empty server.
type tokenKey struct {
	grant  fw.GrantKey
	server string
}

// clientKey names the entries that share one grant of a person.
type clientKey struct{ issuer, clientID string }

type cachedToken struct {
	tok string
	exp time.Time // zero when the server gave no lifetime
}

func newBroker() *broker {
	return &broker{
		now:       time.Now,
		locks:     map[fw.GrantKey]*sync.Mutex{},
		tokens:    map[tokenKey]cachedToken{},
		pending:   map[fw.GrantKey]fw.Grant{},
		resources: map[clientKey][]string{},
	}
}

// register records the MCP URL of an entry under its issuer and client.
func (b *broker) register(u *userAuth) {
	b.mu.Lock()
	defer b.mu.Unlock()
	k := clientKey{u.staticIssuer(), u.clientID}
	if !slices.Contains(b.resources[k], u.resource) {
		b.resources[k] = append(b.resources[k], u.resource)
		sort.Strings(b.resources[k])
	}
}

// resourcesFor returns the MCP URLs that one connection of u covers: every
// entry with the same configured issuer and client.
func (b *broker) resourcesFor(u *userAuth) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if u.staticIssuer() == "" {
		return []string{u.resource}
	}
	out := slices.Clone(b.resources[clientKey{u.staticIssuer(), u.clientID}])
	if !slices.Contains(out, u.resource) {
		out = append(out, u.resource)
	}
	return out
}

func (b *broker) lockFor(key fw.GrantKey) *sync.Mutex {
	b.mu.Lock()
	defer b.mu.Unlock()
	l := b.locks[key]
	if l == nil {
		l = &sync.Mutex{}
		b.locks[key] = l
	}
	return l
}

// cached returns a token that stays good for refreshMargin more.
func (b *broker) cached(k tokenKey) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.tokens[k]
	if !ok {
		return "", false
	}
	if !t.exp.IsZero() && !b.now().Add(refreshMargin).Before(t.exp) {
		delete(b.tokens, k)
		return "", false
	}
	return t.tok, true
}

func (b *broker) keep(k tokenKey, ta *tokenAnswer) {
	t := cachedToken{tok: ta.AccessToken}
	if secs := expiresIn(ta.ExpiresIn); secs > 0 {
		t.exp = b.now().Add(time.Duration(secs) * time.Second)
	}
	b.mu.Lock()
	b.tokens[k] = t
	b.mu.Unlock()
}

// invalidate drops a token that the MCP server refused, unless another
// request already replaced it.
func (b *broker) invalidate(k tokenKey, used string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if t, ok := b.tokens[k]; ok && t.tok == used {
		delete(b.tokens, k)
	}
}

// clear drops every token and the pending grant of a grant key.
func (b *broker) clear(key fw.GrantKey) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for k := range b.tokens {
		if k.grant == key {
			delete(b.tokens, k)
		}
	}
	delete(b.pending, key)
}

// load returns the grant and its revision. A refreshed grant that the store
// did not take comes first, with the revision of the store.
func (b *broker) load(ctx context.Context, store fw.GrantStore, key fw.GrantKey) (fw.Grant, uint64, error) {
	g, rev, err := store.Get(ctx, key)
	b.mu.Lock()
	p, ok := b.pending[key]
	b.mu.Unlock()
	if ok {
		if errors.Is(err, fw.ErrGrantNotFound) {
			rev = 0
		}
		return p, rev, nil
	}
	return g, rev, err
}

// token returns an access token of a person for the MCP server of u. It
// returns errNoGrant when the person has no grant and errGrantExpired when
// the server refused the grant.
func (b *broker) token(ctx context.Context, u *userAuth, store fw.GrantStore, subject string) (string, error) {
	m, err := u.ensureMeta(ctx)
	if err != nil {
		return "", err
	}
	key := u.key(m, subject)
	tk := tokenKey{grant: key, server: u.server}
	if t, ok := b.cached(tk); ok {
		return t, nil
	}
	l := b.lockFor(key)
	l.Lock()
	defer l.Unlock()
	if t, ok := b.cached(tk); ok {
		return t, nil
	}
	if u.exchange == nil {
		g, rev, err := b.load(ctx, store, key)
		if err != nil {
			return "", noGrant(err)
		}
		ta, err := b.refresh(ctx, u, m, store, g, rev, u.resource)
		if err != nil {
			return "", err
		}
		b.keep(tk, ta)
		return ta.AccessToken, nil
	}
	bk := tokenKey{grant: key}
	for attempt := 0; ; attempt++ {
		base, ok := b.cached(bk)
		if !ok {
			g, rev, err := b.load(ctx, store, key)
			if err != nil {
				return "", noGrant(err)
			}
			ta, err := b.refresh(ctx, u, m, store, g, rev, "")
			if err != nil {
				return "", err
			}
			b.keep(bk, ta)
			base = ta.AccessToken
		}
		ta, err := u.exchangeFor(ctx, m, base)
		if err == nil {
			b.keep(tk, ta)
			return ta.AccessToken, nil
		}
		var oe *oauthError
		if attempt == 0 && errors.As(err, &oe) && (oe.code == "invalid_grant" || oe.code == "invalid_token" || oe.status == http.StatusUnauthorized) {
			// The base token ended early. Refresh once more.
			b.invalidate(bk, base)
			continue
		}
		return "", err
	}
}

func noGrant(err error) error {
	if errors.Is(err, fw.ErrGrantNotFound) {
		return errNoGrant
	}
	return err
}

// refresh redeems the refresh token of g and stores the new one before it
// returns the answer. The caller holds the lock of the grant key.
//
// The request and the write do not end with ctx. Once the server rotates
// the refresh token, only the new one is good, so a canceled call must not
// leave the new token unwritten. oauthTimeout bounds the work instead.
func (b *broker) refresh(ctx context.Context, u *userAuth, m *userMeta, store fw.GrantStore, g fw.Grant, rev uint64, resource string) (*tokenAnswer, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), oauthTimeout)
	defer cancel()
	form := url.Values{"grant_type": {grantRefresh}, "refresh_token": {g.RefreshToken}}
	if resource != "" {
		form.Set("resource", resource)
	}
	ta, err := u.post(ctx, m, m.token, form)
	if err == nil {
		err = u.access(ta)
	}
	if err != nil {
		if grantDead(err) {
			b.forget(ctx, store, g)
			u.log.Info("mcp grant refused; the person connects again", "subject", g.Subject, "error", err)
			return nil, fmt.Errorf("%w: %w", errGrantExpired, err)
		}
		return nil, err
	}
	now := b.now()
	if ta.RefreshToken != "" {
		g.RefreshToken = ta.RefreshToken
	}
	g.Refreshed = now
	if secs := firstPositive(expiresIn(ta.RefreshExpiresIn), expiresIn(ta.RefreshTokenExpiresIn)); secs > 0 {
		g.Expires = now.Add(time.Duration(secs) * time.Second)
	}
	key := g.Key()
	if _, err := store.Put(ctx, g, rev); err != nil {
		if errors.Is(err, fw.ErrGrantConflict) {
			// Another writer, such as a new connection, changed the grant.
			// The stored grant wins, and this access token is still good.
			b.mu.Lock()
			delete(b.pending, key)
			b.mu.Unlock()
			u.log.Info("mcp grant changed during a refresh; the stored grant wins", "subject", g.Subject)
			return ta, nil
		}
		// The new refresh token stays in memory, so the next refresh uses
		// it and tries to store it again.
		b.mu.Lock()
		b.pending[key] = g
		b.mu.Unlock()
		return nil, fmt.Errorf("mcp: server %s: store the refreshed grant: %w", u.server, err)
	}
	b.mu.Lock()
	delete(b.pending, key)
	b.mu.Unlock()
	u.log.Debug("mcp grant refreshed", "subject", g.Subject, "expiresIn", expiresIn(ta.ExpiresIn))
	return ta, nil
}

func firstPositive(vals ...int64) int64 {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}

// forget deletes a grant that the server refused, unless a new connection
// replaced it in the meantime. The caller holds the lock of the grant key.
func (b *broker) forget(ctx context.Context, store fw.GrantStore, g fw.Grant) {
	key := g.Key()
	if cur, _, err := store.Get(ctx, key); err == nil && cur.RefreshToken == g.RefreshToken {
		_ = store.Delete(ctx, key)
	}
	b.clear(key)
}

// dropGrant deletes the grant of a person whose tokens the MCP server keeps
// refusing.
func (b *broker) dropGrant(ctx context.Context, u *userAuth, store fw.GrantStore, subject string) {
	m, err := u.ensureMeta(ctx)
	if err != nil {
		return
	}
	key := u.key(m, subject)
	l := b.lockFor(key)
	l.Lock()
	defer l.Unlock()
	if g, _, err := b.load(ctx, store, key); err == nil {
		b.forget(ctx, store, g)
	} else {
		b.clear(key)
	}
}

// randomString returns n random bytes in base64url without padding.
func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// pkceChallenge is the S256 challenge of a verifier (RFC 7636).
func pkceChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// authScopes returns the scopes of the authorization request.
func (u *userAuth) authScopes(m *userMeta) []string {
	out := slices.Clone(u.scopes)
	add := func(s string) {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	if u.sameIdP(m) {
		add("openid")
	}
	if u.offline {
		add("offline_access")
	}
	return out
}

// reservedParams are the parameters of an authorization request that
// authParams cannot change.
var reservedParams = map[string]bool{
	"response_type": true, "client_id": true, "redirect_uri": true, "scope": true, "state": true,
	"code_challenge": true, "code_challenge_method": true, "nonce": true, "resource": true,
}

// idClaims reads the claims of an ID token without a signature check. OIDC
// Core 3.1.3.7 allows that for a token that came from the token endpoint
// over TLS.
func idClaims(raw string) (map[string]any, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errors.New("the ID token is not a JWT")
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil, errors.New("the ID token is not a JWT")
	}
	var c map[string]any
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, errors.New("the ID token is not a JWT")
	}
	return c, nil
}

func claimString(c map[string]any, name string) string {
	s, _ := c[name].(string)
	return s
}

// hasAudience reports whether the aud claim names the client.
func hasAudience(c map[string]any, clientID string) bool {
	switch v := c["aud"].(type) {
	case string:
		return v == clientID
	case []any:
		for _, a := range v {
			if s, ok := a.(string); ok && s == clientID {
				return true
			}
		}
	}
	return false
}

// accountOf is the name of the person at the service, for display.
func accountOf(c map[string]any) string {
	for _, k := range []string{"preferred_username", "email", "name"} {
		if s := claimString(c, k); s != "" {
			return s
		}
	}
	return ""
}

// revoke asks the server to revoke a refresh token (RFC 7009). It is best
// effort: a failure is logged without the token.
func (u *userAuth) revoke(ctx context.Context, m *userMeta, refreshToken string) {
	if m.revocation == "" || refreshToken == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, revokeTimout)
	defer cancel()
	form := url.Values{"token": {refreshToken}, "token_type_hint": {"refresh_token"}}
	status, _, err := u.send(ctx, m, m.revocation, form)
	switch {
	case err != nil:
		u.log.Warn("mcp grant revocation failed", "error", err)
	case status != http.StatusOK:
		u.log.Warn("mcp grant revocation refused", "status", status)
	}
}
