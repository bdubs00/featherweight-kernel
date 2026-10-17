// Package tokens gives a process the access tokens of people from a broker
// outside it (docs/ARCHITECTURE.md section 7.11). The broker keeps the
// grants of people and runs the OAuth flows; the front of the workbench is
// one. This package is the fw.PersonTokens of a process that names a broker
// in tokens.url, and it defines the protocol of the broker, which the
// workbench serves. A refresh token never enters this process. An access
// token does, and the process keeps it until shortly before the broker says
// it expires, or for five minutes at most.
package tokens

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Routes of the broker, under the authenticated listener of the front.
const (
	PathToken  = "/v1/tokens"        // POST a TokenRequest; 200 with a TokenAnswer, or a Refusal
	PathReject = "/v1/tokens/reject" // POST a RejectRequest; 204
	PathForget = "/v1/tokens/forget" // POST a ForgetRequest; 204
)

// EvConnected is the type of the bus event with which a broker announces a
// finished connection. Its data is a Connected. The subject is
// fw.Config.Tokens.Subject.
const EvConnected = "fw.connection.connected"

// Reasons of a Refusal.
const (
	ReasonForbidden      = "forbidden"       // the caller is a person, or the run is not of that person
	ReasonUnknownRun     = "unknown-run"     // the fleet of the broker does not know the run
	ReasonUnknownService = "unknown-service" // no service of that name acts for a person
	ReasonNotConnected   = "not-connected"   // the person never connected the service
	ReasonExpired        = "expired"         // the authorization server refused the grant of the person
	ReasonNoFleet        = "no-fleet"        // the broker has no fleet, so it cannot check the run
	ReasonInvalid        = "invalid-request" // the request lacks the subject, the server, or the run
	ReasonError          = "error"           // the broker failed; Detail says why, without a token
)

// TokenRequest asks for the access token of the person Subject at the
// service Server, for the run Run of that person.
type TokenRequest struct {
	Subject string `json:"subject"`
	Server  string `json:"server"`
	Run     string `json:"run"`
}

// TokenAnswer is the token. ExpiresAt is zero when the broker does not know
// the lifetime.
type TokenAnswer struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt,omitzero"`
}

// RejectRequest says that the service refused Token, the token of Subject.
type RejectRequest struct {
	Subject string `json:"subject"`
	Server  string `json:"server"`
	Token   string `json:"token"`
}

// ForgetRequest says that the service kept refusing the tokens of Subject,
// so the broker drops the grant.
type ForgetRequest struct {
	Subject string `json:"subject"`
	Server  string `json:"server"`
}

// Refusal is the body of a refused request. Detail never holds a token.
type Refusal struct {
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

// Connected is the data of EvConnected: the person connected Servers.
type Connected struct {
	Subject string   `json:"subject"`
	Servers []string `json:"servers"`
}

func init() {
	fw.RegisterService(Start)
}

// Limits of the client.
const (
	requestTimeout = 30 * time.Second
	// maxCache is how long a token is used without asking the broker
	// again, even when the broker gave a later expiry.
	maxCache = 5 * time.Minute
	// margin is how long before its expiry a token counts as expired.
	margin = 30 * time.Second
)

// Start is the fw.Service of the tokens. Without tokens.url, it starts
// nothing. Otherwise it makes the Client the PersonTokens of the agent and
// turns each notice of the broker on the bus into Agent.Connected.
func Start(ctx context.Context, a *fw.Agent) (io.Closer, error) {
	cfg := a.Config()
	if strings.TrimSpace(cfg.Tokens.URL) == "" {
		return nil, nil
	}
	c, err := New(a, cfg.Tokens.URL, cfg.Tokens.Credential)
	if err != nil {
		return nil, err
	}
	a.SetPersonTokens(c)
	s := service{a: a, c: c}
	if b := a.Bus(); b != nil {
		subject := strings.TrimSpace(cfg.Tokens.Subject)
		if subject == "" {
			subject = fw.DefaultTokensSubject
		}
		stop, err := b.Subscribe(context.WithoutCancel(ctx), subject, func(e fw.Event) error {
			if e.Type != EvConnected {
				return nil
			}
			var n Connected
			if err := e.Decode(&n); err != nil || n.Subject == "" || len(n.Servers) == 0 {
				return nil
			}
			c.forgetSubject(n.Subject)
			a.Connected(n.Subject, n.Servers...)
			return nil
		})
		if err != nil {
			a.SetPersonTokens(nil)
			return nil, fmt.Errorf("tokens: subscribe to %s: %w", subject, err)
		}
		s.stop = stop
	}
	return s, nil
}

// service removes the client from the agent and ends its subscription.
type service struct {
	a    *fw.Agent
	c    *Client
	stop func()
}

func (s service) Close() error {
	if s.stop != nil {
		s.stop()
	}
	s.a.SetPersonTokens(nil)
	return nil
}

// Client asks a broker for the tokens of people. It implements
// fw.PersonTokens.
type Client struct {
	a    *fw.Agent
	base string // the address of the broker, without a trailing slash
	cred string // the Secrets name of the bearer token for the broker
	http *http.Client
	log  *slog.Logger

	mu     sync.Mutex
	bearer string // the resolved credential
	cache  map[cacheKey]cached
}

var _ fw.PersonTokens = (*Client)(nil)

type cacheKey struct{ subject, server string }

type cached struct {
	tok string
	exp time.Time
}

// New returns a client of the broker at base. credential is the Secrets
// name of the bearer token for the broker; empty uses auth.tokenEnv of the
// configuration of a.
func New(a *fw.Agent, base, credential string) (*Client, error) {
	base = strings.TrimSpace(base)
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("tokens: tokens.url %q is not an http or https URL", base)
	}
	credential = strings.TrimSpace(credential)
	if credential == "" {
		credential = strings.TrimSpace(a.Config().Auth.TokenEnv)
	}
	if credential == "" {
		credential = "FW_API_TOKEN"
	}
	return &Client{
		a:     a,
		base:  strings.TrimRight(base, "/"),
		cred:  credential,
		http:  &http.Client{Timeout: requestTimeout},
		log:   a.Logger().With("component", "tokens"),
		cache: map[cacheKey]cached{},
	}, nil
}

// credential resolves the bearer token for the broker once.
func (c *Client) credential(ctx context.Context) (string, error) {
	c.mu.Lock()
	b := c.bearer
	c.mu.Unlock()
	if b != "" {
		return b, nil
	}
	var secrets fw.Secrets = c.a.Secrets()
	if secrets == nil {
		secrets = fw.EnvSecrets{}
	}
	v, err := secrets.Get(ctx, c.cred)
	if err != nil || strings.TrimSpace(v) == "" {
		return "", fmt.Errorf("tokens: the credential %s for the broker is not set", c.cred)
	}
	c.mu.Lock()
	c.bearer = strings.TrimSpace(v)
	c.mu.Unlock()
	return c.bearer, nil
}

// Token implements fw.PersonTokens. It asks the broker for the token of
// the person for the run in ctx, and keeps the answer until shortly before
// it expires.
func (c *Client) Token(ctx context.Context, subject, server string) (string, error) {
	k := cacheKey{subject, server}
	c.mu.Lock()
	t, ok := c.cache[k]
	c.mu.Unlock()
	if ok && time.Now().Before(t.exp) {
		return t.tok, nil
	}
	var ans TokenAnswer
	status, ref, err := c.post(ctx, PathToken, TokenRequest{Subject: subject, Server: server, Run: fw.RunInfoFrom(ctx).RunID}, &ans)
	if err != nil {
		return "", err
	}
	if status == http.StatusOK {
		if ans.Token == "" {
			return "", errors.New("tokens: the broker gave an empty token")
		}
		exp := time.Now().Add(maxCache)
		if !ans.ExpiresAt.IsZero() && ans.ExpiresAt.Add(-margin).Before(exp) {
			exp = ans.ExpiresAt.Add(-margin)
		}
		c.mu.Lock()
		c.cache[k] = cached{tok: ans.Token, exp: exp}
		c.mu.Unlock()
		return ans.Token, nil
	}
	switch ref.Reason {
	case ReasonNotConnected:
		return "", fmt.Errorf("%w: %s", fw.ErrNotConnected, server)
	case ReasonExpired:
		return "", fmt.Errorf("%w: %s", fw.ErrConnectionExpired, server)
	}
	detail := ref.Detail
	if detail == "" {
		detail = http.StatusText(status)
	}
	if ref.Reason == "" {
		return "", fmt.Errorf("tokens: the broker answered %d for the token of the person at %s: %s", status, server, detail)
	}
	return "", fmt.Errorf("tokens: the broker refused the token of the person at %s (%s): %s", server, ref.Reason, detail)
}

// Reject implements fw.PersonTokens.
func (c *Client) Reject(ctx context.Context, subject, server, used string) {
	k := cacheKey{subject, server}
	c.mu.Lock()
	if t, ok := c.cache[k]; ok && t.tok == used {
		delete(c.cache, k)
	}
	c.mu.Unlock()
	if _, _, err := c.post(ctx, PathReject, RejectRequest{Subject: subject, Server: server, Token: used}, nil); err != nil {
		c.log.Warn("the broker did not take a rejected token", "server", server, "error", err)
	}
}

// Forget implements fw.PersonTokens.
func (c *Client) Forget(ctx context.Context, subject, server string) {
	c.mu.Lock()
	delete(c.cache, cacheKey{subject, server})
	c.mu.Unlock()
	if _, _, err := c.post(ctx, PathForget, ForgetRequest{Subject: subject, Server: server}, nil); err != nil {
		c.log.Warn("the broker did not forget a grant", "server", server, "error", err)
	}
}

// forgetSubject drops the cached tokens of a person, so a new connection
// gives new tokens at once.
func (c *Client) forgetSubject(subject string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.cache {
		if k.subject == subject {
			delete(c.cache, k)
		}
	}
}

// post sends one request to the broker. A 2xx answer is decoded into out
// when out is not nil, and any other answer into the Refusal.
func (c *Client) post(ctx context.Context, path string, body, out any) (int, Refusal, error) {
	bearer, err := c.credential(ctx)
	if err != nil {
		return 0, Refusal{}, err
	}
	b, err := json.Marshal(body)
	if err != nil {
		return 0, Refusal{}, fmt.Errorf("tokens: encode the request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(b))
	if err != nil {
		return 0, Refusal{}, fmt.Errorf("tokens: build the request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, Refusal{}, fmt.Errorf("tokens: the broker did not answer: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, Refusal{}, fmt.Errorf("tokens: read the answer of the broker: %w", err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out != nil && len(bytes.TrimSpace(data)) > 0 {
			if err := json.Unmarshal(data, out); err != nil {
				return resp.StatusCode, Refusal{}, fmt.Errorf("tokens: the answer of the broker is not readable: %w", err)
			}
		}
		return resp.StatusCode, Refusal{}, nil
	}
	var ref Refusal
	_ = json.Unmarshal(data, &ref)
	if resp.StatusCode == http.StatusUnauthorized {
		return resp.StatusCode, ref, fmt.Errorf("tokens: the broker did not accept the credential %s", c.cred)
	}
	return resp.StatusCode, ref, nil
}
