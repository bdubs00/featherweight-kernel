package mcp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/authn/spiffe"
	"github.com/bdubs00/featherweight-kernel/fw"
)

// secretName matches an environment-style secret name such as API_TOKEN.
var secretName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// isLocalHost reports whether a host is a loopback address or a
// cluster-local name. Plain http to such a host stays inside the machine or
// the cluster.
func isLocalHost(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	switch h {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	for _, suffix := range []string{".svc", ".svc.cluster.local", ".local"} {
		if strings.HasSuffix(h, suffix) {
			return true
		}
	}
	return false
}

// loadTLS builds the TLS settings for the MCP endpoint. It returns nil when
// the entry sets no TLS field, so the client uses the system roots.
func loadTLS(c fw.TLSConfig) (*tls.Config, error) {
	if c == (fw.TLSConfig{}) {
		return nil, nil
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.CA != "" {
		pem, err := os.ReadFile(c.CA)
		if err != nil {
			return nil, fmt.Errorf("read tls.ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("tls.ca %s holds no PEM certificate", c.CA)
		}
		tc.RootCAs = pool
	}
	if (c.Cert == "") != (c.Key == "") {
		return nil, errors.New("tls.cert and tls.key go together; set both or neither")
	}
	if c.Cert != "" {
		cert, err := tls.LoadX509KeyPair(c.Cert, c.Key)
		if err != nil {
			return nil, fmt.Errorf("load tls.cert and tls.key: %w", err)
		}
		tc.Certificates = []tls.Certificate{cert}
	}
	tc.InsecureSkipVerify = c.Insecure //nolint:gosec // development only, documented in fw.yaml
	return tc, nil
}

// baseTransport returns an http.Transport with the given TLS settings.
func baseTransport(tc *tls.Config) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	if tc != nil {
		t.TLSClientConfig = tc
	}
	return t
}

// oauthTLS returns the TLS settings for the authorization server. The
// authorization server often has a public certificate, so the pool holds the
// system roots plus the entry CA bundle. The client certificate stays with
// the MCP endpoint.
func oauthTLS(c fw.TLSConfig) (*tls.Config, error) {
	if c.CA == "" && !c.Insecure {
		return nil, nil
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: c.Insecure} //nolint:gosec // development only
	if c.CA != "" {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		pem, err := os.ReadFile(c.CA)
		if err != nil {
			return nil, fmt.Errorf("read tls.ca: %w", err)
		}
		pool.AppendCertsFromPEM(pem)
		tc.RootCAs = pool
	}
	return tc, nil
}

// authTransport adds the configured headers and credentials to each request
// that goes to the MCP endpoint, and retries a request that did not reach the
// server.
type authTransport struct {
	base     http.RoundTripper
	server   string   // entry name, for error messages
	endpoint *url.URL // credentials go to this origin only
	secrets  fw.Secrets
	bearer   string              // Secrets name of a static token
	raw      map[string]string   // cfg.Headers before resolution
	oauth    *oauthClient        // nil unless auth.oauth is set with grant client
	svid     *spiffe.TokenSource // nil unless auth.spiffe is set
	user     *personTokens       // the tokens of one person, for grant user

	retries   int           // retries after a dial error or a gateway status
	retryBase time.Duration // first backoff, doubled each retry

	mu       sync.Mutex
	resolved http.Header // headers after secret resolution; nil until first use
}

// headers resolves the static headers and the bearer token once, at first
// use. A failed resolution is not cached, so a later request tries again.
func (t *authTransport) headers(ctx context.Context) (http.Header, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.resolved != nil {
		return t.resolved, nil
	}
	h := http.Header{}
	for k, v := range t.raw {
		if secretName.MatchString(v) {
			if s, err := t.secrets.Get(ctx, v); err == nil && s != "" {
				v = s
			}
		}
		h.Set(k, v)
	}
	if t.bearer != "" {
		tok, err := t.secrets.Get(ctx, t.bearer)
		if err != nil {
			return nil, fmt.Errorf("mcp: server %s: resolve auth.bearer secret %s: %w", t.server, t.bearer, err)
		}
		if tok == "" {
			return nil, fmt.Errorf("mcp: server %s: auth.bearer secret %s is empty", t.server, t.bearer)
		}
		h.Set("Authorization", "Bearer "+tok)
	}
	t.resolved = h
	return h, nil
}

// sameOrigin reports whether u points at the MCP endpoint origin. Headers
// and credentials never follow a redirect to another origin.
func (t *authTransport) sameOrigin(u *url.URL) bool {
	return strings.EqualFold(u.Scheme, t.endpoint.Scheme) && strings.EqualFold(u.Host, t.endpoint.Host)
}

// RoundTrip implements http.RoundTripper.
func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.sameOrigin(req.URL) {
		return t.base.RoundTrip(req)
	}
	ctx := req.Context()
	hdr, err := t.headers(ctx)
	if err != nil {
		closeBody(req)
		return nil, err
	}
	resp, used, err := t.send(req, hdr, true)
	if err != nil {
		return resp, err
	}
	if t.user != nil {
		return t.retryPerson(req, hdr, resp, used)
	}
	if t.oauth == nil {
		return resp, nil
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		retry, aerr := t.oauth.unauthorized(ctx, resp.Header, used)
		if aerr != nil || !retry || !replayable(req) {
			if aerr != nil {
				return nil, discard(resp, aerr)
			}
			return resp, nil
		}
	case http.StatusForbidden:
		retry, aerr := t.oauth.forbidden(ctx, resp.Header)
		if aerr != nil || !retry || !replayable(req) {
			if aerr != nil {
				return nil, discard(resp, aerr)
			}
			return resp, nil
		}
	default:
		return resp, nil
	}
	_ = discard(resp, nil)
	resp, _, err = t.send(req, hdr, false)
	return resp, err
}

// retryPerson handles a 401 to a request with the token of a person: it
// drops the token, sends the request once more with a new one, and marks
// the person rejected when the server refuses again.
func (t *authTransport) retryPerson(req *http.Request, hdr http.Header, resp *http.Response, used string) (*http.Response, error) {
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	if used == "" || !replayable(req) {
		t.user.reject()
		return resp, nil
	}
	t.user.invalidate(req.Context(), used)
	_ = discard(resp, nil)
	resp, _, err := t.send(req, hdr, false)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		t.user.reject()
	}
	return resp, err
}

// send sends one request with headers and the current OAuth token. It
// retries a dial error and a 502, 503, or 504 when the body can be sent
// again. first tells whether req.Body is still unread. It returns the token
// that it used.
func (t *authTransport) send(req *http.Request, hdr http.Header, first bool) (*http.Response, string, error) {
	ctx := req.Context()
	for attempt := 0; ; attempt++ {
		r := req.Clone(ctx)
		if !first || attempt > 0 {
			if req.GetBody != nil {
				b, err := req.GetBody()
				if err != nil {
					return nil, "", err
				}
				r.Body = b
			}
		}
		for k, v := range hdr {
			r.Header[k] = v
		}
		var used string
		if t.user != nil {
			tok, err := t.user.token(ctx)
			if err != nil {
				closeBody(r)
				return nil, "", err
			}
			r.Header.Set("Authorization", "Bearer "+tok)
			used = tok
		}
		if t.oauth != nil {
			tok, err := t.oauth.token(ctx)
			if err != nil {
				closeBody(r)
				return nil, "", err
			}
			if tok != "" {
				r.Header.Set("Authorization", "Bearer "+tok)
				used = tok
			}
		}
		if t.svid != nil {
			tok, err := t.svid.Token(ctx)
			if err != nil {
				closeBody(r)
				return nil, "", fmt.Errorf("mcp: server %s: %w", t.server, err)
			}
			r.Header.Set("Authorization", "Bearer "+tok)
			used = tok
		}
		resp, err := t.base.RoundTrip(r)
		if attempt >= t.retries || !replayable(req) || ctx.Err() != nil {
			return resp, used, err
		}
		switch {
		case err != nil && isDialError(err):
		case err == nil && (resp.StatusCode == http.StatusBadGateway ||
			resp.StatusCode == http.StatusServiceUnavailable ||
			resp.StatusCode == http.StatusGatewayTimeout):
			_ = discard(resp, nil)
		default:
			return resp, used, err
		}
		delay := t.retryBase << attempt
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-time.After(delay):
		}
	}
}

// replayable reports whether the request body can be sent again.
func replayable(req *http.Request) bool {
	return req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
}

// isDialError reports whether err means the request never reached the
// server, so a retry cannot run the request twice.
func isDialError(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

func closeBody(req *http.Request) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
}

// discard drains and closes a response body and returns err.
func discard(resp *http.Response, err error) error {
	if resp != nil && resp.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
	}
	return err
}
