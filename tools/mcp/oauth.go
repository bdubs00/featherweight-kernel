package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// refreshMargin is how long before expiry a token is replaced.
const refreshMargin = 30 * time.Second

// oauthClient gets access tokens with the OAuth 2.1 client_credentials
// grant. A sandbox has no browser, so this is the only grant fw uses.
//
// When the entry sets tokenUrl, the client asks that endpoint for a token
// before the first request. Otherwise it waits for the first 401 from the MCP
// server, then reads the protected resource metadata (RFC 9728), the
// authorization server metadata (RFC 8414), and asks the token endpoint
// there. Every token request carries the resource parameter (RFC 8707). The
// token stays in memory only.
type oauthClient struct {
	server   string // entry name, for errors and logs
	clientID string
	secret   string // Secrets name of the client secret
	secrets  fw.Secrets
	resource string // canonical MCP endpoint URL
	endpoint *url.URL
	http     *http.Client // for metadata and token requests; it carries no MCP credentials
	log      *slog.Logger
	now      func() time.Time

	mu         sync.Mutex
	tokenURL   string
	basicAuth  bool     // send the client secret with HTTP Basic instead of in the body
	scopes     []string // the scopes of the next token request
	configured bool     // the entry sets scopes, so a challenge does not replace them
	tok        string
	expiry     time.Time // zero means the server gave no lifetime
}

func newOAuthClient(server string, c *fw.OAuthClientConfig, secrets fw.Secrets, endpoint *url.URL, hc *http.Client, log *slog.Logger) *oauthClient {
	return &oauthClient{
		server:     server,
		clientID:   c.ClientID,
		secret:     c.ClientSecret,
		secrets:    secrets,
		resource:   canonicalResource(endpoint),
		endpoint:   endpoint,
		http:       hc,
		log:        log,
		now:        time.Now,
		tokenURL:   c.TokenURL,
		scopes:     slices.Clone(c.Scopes),
		configured: len(c.Scopes) > 0,
	}
}

// canonicalResource returns the canonical URI of the MCP server for the
// resource parameter: lower-case scheme and host, no default port, no
// fragment, and no trailing slash on an empty path.
func canonicalResource(u *url.URL) string {
	c := *u
	c.Scheme = strings.ToLower(c.Scheme)
	host := strings.ToLower(c.Hostname())
	port := c.Port()
	if (c.Scheme == "https" && port == "443") || (c.Scheme == "http" && port == "80") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	c.Host = host
	c.Fragment = ""
	c.RawFragment = ""
	c.User = nil
	if c.Path == "/" {
		c.Path = ""
		c.RawPath = ""
	}
	return c.String()
}

// valid reports whether the cached token is usable for at least
// refreshMargin more. The caller holds o.mu.
func (o *oauthClient) valid() bool {
	if o.tok == "" {
		return false
	}
	return o.expiry.IsZero() || o.now().Add(refreshMargin).Before(o.expiry)
}

// token returns an access token for the next request. It returns "" before
// discovery, when the token endpoint is not known yet.
func (o *oauthClient) token(ctx context.Context) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.valid() {
		return o.tok, nil
	}
	if o.tokenURL == "" {
		return "", nil
	}
	if err := o.fetch(ctx); err != nil {
		return "", err
	}
	return o.tok, nil
}

// unauthorized handles a 401 from the MCP server. used is the token that the
// failed request carried. It reports whether a retry with a new token can
// succeed.
func (o *oauthClient) unauthorized(ctx context.Context, h http.Header, used string) (bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if used != "" && o.tok != "" && o.tok != used && o.valid() {
		// Another request already replaced the token.
		return true, nil
	}
	challenge := bearerChallenge(h)
	if o.tokenURL == "" {
		if err := o.discover(ctx, challenge); err != nil {
			return false, err
		}
	} else if used == "" && o.valid() {
		return true, nil
	}
	if !o.configured && len(o.scopes) == 0 {
		if s := challenge["scope"]; s != "" {
			o.scopes = strings.Fields(s)
		}
	}
	o.tok = ""
	if err := o.fetch(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// forbidden handles a 403 from the MCP server. On error="insufficient_scope"
// it asks once for a token with the union of the old and the challenged
// scopes. It reports whether a retry can succeed.
func (o *oauthClient) forbidden(ctx context.Context, h http.Header) (bool, error) {
	challenge := bearerChallenge(h)
	if challenge["error"] != "insufficient_scope" {
		return false, nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.tokenURL == "" {
		return false, nil
	}
	union := slices.Clone(o.scopes)
	for _, s := range strings.Fields(challenge["scope"]) {
		if !slices.Contains(union, s) {
			union = append(union, s)
		}
	}
	if len(union) == len(o.scopes) {
		return false, nil
	}
	o.log.Info("mcp oauth step-up", "server", o.server, "scopes", strings.Join(union, " "))
	o.scopes = union
	o.tok = ""
	if err := o.fetch(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// bearerChallenge returns the parameters of the Bearer challenge in the
// WWW-Authenticate headers, with lower-case keys. It returns an empty map
// when there is no such challenge.
func bearerChallenge(h http.Header) map[string]string {
	cs, err := oauthex.ParseWWWAuthenticate(h.Values("WWW-Authenticate"))
	if err != nil {
		return map[string]string{}
	}
	for _, c := range cs {
		if c.Scheme == "bearer" && c.Params != nil {
			return c.Params
		}
	}
	return map[string]string{}
}

// protectedResource is the part of RFC 9728 metadata that fw reads.
type protectedResource struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
	ScopesSupported      []string `json:"scopes_supported"`
}

// authServer is the part of RFC 8414 metadata that fw reads.
type authServer struct {
	Issuer            string   `json:"issuer"`
	TokenEndpoint     string   `json:"token_endpoint"`
	TokenAuthMethods  []string `json:"token_endpoint_auth_methods_supported"`
	GrantTypesAllowed []string `json:"grant_types_supported"`
}

// discover finds the token endpoint from a 401 challenge. The caller holds
// o.mu.
func (o *oauthClient) discover(ctx context.Context, challenge map[string]string) error {
	var candidates []string
	if u := challenge["resource_metadata"]; u != "" {
		candidates = []string{u}
	} else {
		candidates = wellKnownResourceURLs(o.endpoint)
	}
	var prm protectedResource
	var err error
	for _, u := range candidates {
		if err = o.getJSON(ctx, u, &prm); err == nil {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("mcp: server %s: oauth: read protected resource metadata: %w", o.server, err)
	}
	if prm.Resource != "" && !resourceCovers(prm.Resource, o.resource) {
		return fmt.Errorf("mcp: server %s: oauth: protected resource metadata names resource %q, not %q", o.server, prm.Resource, o.resource)
	}
	if len(prm.AuthorizationServers) == 0 {
		return fmt.Errorf("mcp: server %s: oauth: protected resource metadata lists no authorization server", o.server)
	}
	issuer := prm.AuthorizationServers[0]
	var as authServer
	urls, err := authServerMetadataURLs(issuer)
	if err != nil {
		return fmt.Errorf("mcp: server %s: oauth: %w", o.server, err)
	}
	for _, u := range urls {
		if err = o.getJSON(ctx, u, &as); err == nil {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("mcp: server %s: oauth: read authorization server metadata for %s: %w", o.server, issuer, err)
	}
	if as.Issuer != "" && strings.TrimSuffix(as.Issuer, "/") != strings.TrimSuffix(issuer, "/") {
		return fmt.Errorf("mcp: server %s: oauth: authorization server metadata names issuer %q, not %q", o.server, as.Issuer, issuer)
	}
	if as.TokenEndpoint == "" {
		return fmt.Errorf("mcp: server %s: oauth: authorization server %s has no token_endpoint", o.server, issuer)
	}
	if err := safeTokenURL(as.TokenEndpoint); err != nil {
		return fmt.Errorf("mcp: server %s: oauth: %w", o.server, err)
	}
	if len(as.GrantTypesAllowed) > 0 && !slices.Contains(as.GrantTypesAllowed, "client_credentials") {
		return fmt.Errorf("mcp: server %s: oauth: authorization server %s does not offer the client_credentials grant", o.server, issuer)
	}
	o.basicAuth = len(as.TokenAuthMethods) > 0 &&
		!slices.Contains(as.TokenAuthMethods, "client_secret_post") &&
		slices.Contains(as.TokenAuthMethods, "client_secret_basic")
	if !o.configured && len(o.scopes) == 0 && challenge["scope"] == "" {
		o.scopes = slices.Clone(prm.ScopesSupported)
	}
	o.tokenURL = as.TokenEndpoint
	o.log.Info("mcp oauth discovered token endpoint", "server", o.server, "issuer", issuer)
	return nil
}

// wellKnownResourceURLs returns the RFC 9728 metadata URLs for an endpoint:
// the path-specific one first, then the root one.
func wellKnownResourceURLs(endpoint *url.URL) []string {
	base := url.URL{Scheme: endpoint.Scheme, Host: endpoint.Host}
	var out []string
	if p := strings.TrimSuffix(endpoint.Path, "/"); p != "" {
		u := base
		u.Path = "/.well-known/oauth-protected-resource" + p
		out = append(out, u.String())
	}
	u := base
	u.Path = "/.well-known/oauth-protected-resource"
	return append(out, u.String())
}

// authServerMetadataURLs returns the metadata URLs to try for an issuer:
// RFC 8414 first, then OpenID Connect discovery.
func authServerMetadataURLs(issuer string) ([]string, error) {
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("authorization server %q is not a URL", issuer)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLocalHost(u.Hostname())) {
		return nil, fmt.Errorf("authorization server %q must use https", issuer)
	}
	base := url.URL{Scheme: u.Scheme, Host: u.Host}
	p := strings.TrimSuffix(u.Path, "/")
	at := func(path string) string { v := base; v.Path = path; return v.String() }
	if p == "" {
		return []string{
			at("/.well-known/oauth-authorization-server"),
			at("/.well-known/openid-configuration"),
		}, nil
	}
	return []string{
		at("/.well-known/oauth-authorization-server" + p),
		at("/.well-known/openid-configuration" + p),
		at(p + "/.well-known/openid-configuration"),
	}, nil
}

// resourceCovers reports whether the resource in the metadata is the MCP
// endpoint or a prefix of it.
func resourceCovers(meta, endpoint string) bool {
	u, err := url.Parse(meta)
	if err != nil {
		return false
	}
	m := canonicalResource(u)
	if m == endpoint {
		return true
	}
	return strings.HasPrefix(endpoint, strings.TrimSuffix(m, "/")+"/")
}

// safeTokenURL refuses to send client credentials over plain http to a host
// outside the machine or the cluster.
func safeTokenURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("token endpoint %q is not a URL", raw)
	}
	if u.Scheme == "https" || (u.Scheme == "http" && isLocalHost(u.Hostname())) {
		return nil
	}
	return fmt.Errorf("token endpoint %s must use https", u.Host)
}

// getJSON fetches a metadata document.
func (o *oauthClient) getJSON(ctx context.Context, u string, v any) error {
	return fetchJSON(ctx, o.http, u, v)
}

// fetchJSON fetches a metadata document of at most 1 MiB.
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

// tokenResponse is a token endpoint reply, success or error.
type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        any    `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// fetch asks the token endpoint for a new token. The caller holds o.mu.
func (o *oauthClient) fetch(ctx context.Context) error {
	secret, err := o.secrets.Get(ctx, o.secret)
	if err != nil {
		return fmt.Errorf("mcp: server %s: oauth: resolve client secret %s: %w", o.server, o.secret, err)
	}
	form := url.Values{
		"grant_type": {"client_credentials"},
		"resource":   {o.resource},
	}
	if len(o.scopes) > 0 {
		form.Set("scope", strings.Join(o.scopes, " "))
	}
	if !o.basicAuth {
		form.Set("client_id", o.clientID)
		form.Set("client_secret", secret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("mcp: server %s: oauth: token request: %w", o.server, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if o.basicAuth {
		req.SetBasicAuth(url.QueryEscape(o.clientID), url.QueryEscape(secret))
	}
	resp, err := o.http.Do(req)
	if err != nil {
		return fmt.Errorf("mcp: server %s: oauth: token request: %w", o.server, err)
	}
	defer resp.Body.Close()
	var tr tokenResponse
	derr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tr)
	if resp.StatusCode != http.StatusOK {
		if derr == nil && tr.Error != "" {
			return fmt.Errorf("mcp: server %s: oauth: token endpoint refused: %s: %s", o.server, tr.Error, tr.ErrorDescription)
		}
		return fmt.Errorf("mcp: server %s: oauth: token endpoint replied %s", o.server, resp.Status)
	}
	if derr != nil {
		return fmt.Errorf("mcp: server %s: oauth: decode token response: %w", o.server, derr)
	}
	if tr.AccessToken == "" {
		return fmt.Errorf("mcp: server %s: oauth: token response has no access_token", o.server)
	}
	if tr.TokenType != "" && !strings.EqualFold(tr.TokenType, "bearer") {
		return fmt.Errorf("mcp: server %s: oauth: token type %q is not Bearer", o.server, tr.TokenType)
	}
	o.tok = tr.AccessToken
	o.expiry = time.Time{}
	if secs := expiresIn(tr.ExpiresIn); secs > 0 {
		o.expiry = o.now().Add(time.Duration(secs) * time.Second)
	}
	o.log.Debug("mcp oauth token issued", "server", o.server, "expiresIn", expiresIn(tr.ExpiresIn))
	return nil
}

// expiresIn reads expires_in as a number or a numeric string.
func expiresIn(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case string:
		n, err := strconv.ParseInt(x, 10, 64)
		if err == nil {
			return n
		}
	}
	return 0
}
