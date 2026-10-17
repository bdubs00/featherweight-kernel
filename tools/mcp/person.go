package mcp

// This file is the side of an entry with grant user that faces runs and
// surfaces: a session per person, the call path that asks the person to
// connect, the placeholder tool, and fw.Connector.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	personIdle = 15 * time.Minute // a session of a person closes after this long without use
	skipTTL    = 24 * time.Hour   // how long a skip of a run is kept
)

var _ fw.Connector = (*Server)(nil)

// personTokens gives the transport of one person the tokens of that person.
type personTokens struct {
	u       *userAuth
	subject string
	store   fw.GrantStore
	// rejected is set when the server refused the grant, or kept refusing
	// the tokens of the person. The call path takes it after a failure.
	rejected atomic.Bool
}

func (p *personTokens) token(ctx context.Context) (string, error) {
	tok, err := p.u.broker.token(ctx, p.u, p.store, p.subject)
	if errors.Is(err, errGrantExpired) {
		p.rejected.Store(true)
	}
	return tok, err
}

// invalidate drops a token that the MCP server refused.
func (p *personTokens) invalidate(ctx context.Context, used string) {
	m, err := p.u.ensureMeta(ctx)
	if err != nil {
		return
	}
	p.u.broker.invalidate(tokenKey{grant: p.u.key(m, p.subject), server: p.u.server}, used)
}

func (p *personTokens) reject() { p.rejected.Store(true) }

// person is the MCP session of one person at one server. Servers bind a
// session to a user, so people never share one.
type person struct {
	subject string
	src     *personTokens
	client  *http.Client

	connMu   sync.Mutex // one connect at a time
	session  *sdk.ClientSession
	lastUsed time.Time
	active   int // calls and listings in flight
}

// title is the name of the service for a person.
func (s *Server) title() string {
	if t := strings.TrimSpace(s.cfg.Title); t != "" {
		return t
	}
	return s.cfg.Name
}

// grantsOf returns the grant store of the agent in ctx, or nil.
func grantsOf(ctx context.Context) fw.GrantStore {
	if a := fw.AgentFrom(ctx); a != nil {
		return a.Grants()
	}
	return nil
}

// personFor returns the person of a subject, and makes one when needed. It
// also closes the sessions that nobody used for personIdle.
func (s *Server) personFor(subject string, store fw.GrantStore) *person {
	s.mu.Lock()
	var idle []*sdk.ClientSession
	now := s.now()
	for k, p := range s.persons {
		if k != subject && p.active == 0 && now.Sub(p.lastUsed) > personIdle {
			if p.session != nil {
				idle = append(idle, p.session)
			}
			delete(s.persons, k)
		}
	}
	p := s.persons[subject]
	if p == nil || p.src.store != store {
		src := &personTokens{u: s.user, subject: subject, store: store}
		rt := &authTransport{
			base:      s.rt.base,
			server:    s.cfg.Name,
			endpoint:  s.rt.endpoint,
			secrets:   s.secrets,
			raw:       s.cfg.Headers,
			user:      src,
			retries:   s.rt.retries,
			retryBase: s.rt.retryBase,
		}
		if p != nil && p.session != nil {
			idle = append(idle, p.session)
		}
		p = &person{subject: subject, src: src, client: &http.Client{Transport: rt}}
		s.persons[subject] = p
	}
	p.lastUsed = now
	s.mu.Unlock()
	for _, cs := range idle {
		_ = cs.Close()
	}
	return p
}

// personOf returns the person of the run in ctx, or nil when the run has
// no person or the context has no grant store.
func (s *Server) personOf(ctx context.Context) *person {
	info := fw.RunInfoFrom(ctx)
	if info.PrincipalKind != fw.KindUser || info.Principal == "" {
		return nil
	}
	store := grantsOf(ctx)
	if store == nil {
		return nil
	}
	return s.personFor(info.Principal, store)
}

// use marks a person busy until the returned function runs.
func (s *Server) use(p *person) func() {
	s.mu.Lock()
	p.active++
	p.lastUsed = s.now()
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		p.active--
		p.lastUsed = s.now()
		s.mu.Unlock()
	}
}

// connectPerson returns the session of a person, and opens one when there
// is none.
func (s *Server) connectPerson(ctx context.Context, p *person) (*sdk.ClientSession, error) {
	p.connMu.Lock()
	defer p.connMu.Unlock()
	s.mu.Lock()
	cs, closed := p.session, s.closed
	s.mu.Unlock()
	if closed {
		return nil, fmt.Errorf("mcp: server %s is closed", s.cfg.Name)
	}
	if cs != nil {
		return cs, nil
	}
	cs, err := s.dial(ctx, &sdk.StreamableClientTransport{Endpoint: s.cfg.Endpoint, HTTPClient: p.client})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = cs.Close()
		return nil, fmt.Errorf("mcp: server %s is closed", s.cfg.Name)
	}
	p.session = cs
	s.mu.Unlock()
	go func() {
		_ = cs.Wait()
		s.forgetPerson(p, cs)
	}()
	return cs, nil
}

// forgetPerson drops a session of a person that ended.
func (s *Server) forgetPerson(p *person, cs *sdk.ClientSession) {
	s.mu.Lock()
	if p.session == cs {
		p.session = nil
	}
	s.mu.Unlock()
}

// dropPerson closes the session of a subject, so the next call opens a new
// one with new tokens.
func (s *Server) dropPerson(subject string) {
	s.mu.Lock()
	p := s.persons[subject]
	var cs *sdk.ClientSession
	if p != nil {
		cs, p.session = p.session, nil
	}
	s.mu.Unlock()
	if cs != nil {
		_ = cs.Close()
	}
}

// personTools implements Tools for an entry with grant user. The tool list
// is shared. The first person with a grant lists it. Until then, a run of a
// person sees the placeholder tool <server>.connect.
func (s *Server) personTools(ctx context.Context) ([]fw.Tool, error) {
	s.mu.Lock()
	listed, stale, down := s.listed, s.stale, s.downLocked()
	s.mu.Unlock()
	if listed && (!stale || down) {
		return s.snapshot(), nil
	}
	if p := s.personOf(ctx); p != nil && !down {
		if _, err := p.src.token(ctx); err == nil {
			done := s.use(p)
			err := s.refreshWith(ctx, func(ctx context.Context) (*sdk.ClientSession, error) { return s.connectPerson(ctx, p) })
			done()
			if err == nil {
				return s.snapshot(), nil
			}
			s.log.Warn("mcp tool list failed for a person", "subject", p.subject, "error", err)
		}
	}
	if listed {
		return s.snapshot(), nil
	}
	return []fw.Tool{&connectTool{s: s}}, nil
}

// noPersonResult is the answer to a call in a run without a person.
func (s *Server) noPersonResult() fw.ToolResult {
	return fw.ErrorResult(fmt.Sprintf("%s acts for the person who started the run, and this run was not started by a signed-in person, so %s cannot be used here.", s.title(), s.title()))
}

func (s *Server) skipResult() fw.ToolResult {
	return fw.ErrorResult(fmt.Sprintf("The person chose not to connect %s, so this call did not run.", s.title()))
}

// skipped reports whether the person of a run skipped the service.
func (s *Server) skipped(run string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.skips[run]
	return ok
}

func (s *Server) skip(run string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, at := range s.skips {
		if now.Sub(at) > skipTTL {
			delete(s.skips, k)
		}
	}
	s.skips[run] = now
}

// askConnect pauses the call until the person connects the service. It
// returns true once the person connected. Otherwise it returns the result
// for the model, or an error when the run stops.
func (s *Server) askConnect(ctx context.Context, run, reason string) (bool, fw.ToolResult, error) {
	err := fw.RequestConnect(ctx, fw.ConnectRequest{Server: s.cfg.Name, Title: s.title(), Reason: reason})
	switch {
	case err == nil:
		return true, fw.ToolResult{}, nil
	case errors.Is(err, fw.ErrConnectSkipped):
		s.skip(run)
		return false, s.skipResult(), nil
	case errors.Is(err, fw.ErrNoAsker):
		return false, fw.ErrorResult(fmt.Sprintf("%s acts for the person who started the run, and that person has not connected it. Connect %s under Connections in the dashboard, then try again.", s.title(), s.title())), nil
	case ctx.Err() != nil:
		return false, fw.ToolResult{}, fmt.Errorf("mcp: server %s: wait for a connection: %w", s.cfg.Name, ctx.Err())
	default:
		return false, fw.ToolResult{}, fmt.Errorf("mcp: server %s: wait for a connection: %w", s.cfg.Name, err)
	}
}

// reasonOf maps a token error to the reason of a connect request. It
// returns "" for an error that a connection does not fix.
func reasonOf(err error) string {
	switch {
	case errors.Is(err, errNoGrant):
		return fw.ConnectMissing
	case errors.Is(err, errGrantExpired):
		return fw.ConnectExpired
	}
	return ""
}

// callAsPerson runs a call for the person of the run. Without a grant, or
// with a grant that the server refused, the call asks the person to
// connect, then goes on. It asks at most once per call.
func (s *Server) callAsPerson(ctx context.Context, name string, arguments any) (fw.ToolResult, error) {
	info := fw.RunInfoFrom(ctx)
	if info.PrincipalKind != fw.KindUser || info.Principal == "" {
		return s.noPersonResult(), nil
	}
	if s.skipped(info.RunID) {
		return s.skipResult(), nil
	}
	p := s.personOf(ctx)
	if p == nil {
		return fw.ErrorResult(fmt.Sprintf("%s acts for a person, and this agent keeps no grants.", s.title())), nil
	}
	asked := false
	for {
		_, err := p.src.token(ctx)
		p.src.rejected.Store(false)
		if err != nil {
			reason := reasonOf(err)
			if reason == "" {
				if ctx.Err() != nil {
					return fw.ToolResult{}, fmt.Errorf("mcp: call %s.%s: %w", s.cfg.Name, name, ctx.Err())
				}
				return fw.ErrorResult(fmt.Sprintf("%s cannot get a token for the person who started the run: %v", s.title(), err)), nil
			}
			if asked {
				return fw.ErrorResult(fmt.Sprintf("%s is still not connected for the person who started the run, so this call did not run.", s.title())), nil
			}
			ok, res, err := s.askConnect(ctx, info.RunID, reason)
			if !ok {
				return res, err
			}
			asked = true
			continue
		}
		done := s.use(p)
		res, authFailed, err := s.callSession(ctx, name, arguments,
			func(ctx context.Context) (*sdk.ClientSession, error) { return s.connectPerson(ctx, p) },
			func(cs *sdk.ClientSession) { s.forgetPerson(p, cs) },
			func() bool { return p.src.rejected.Swap(false) })
		done()
		if !authFailed {
			return res, err
		}
		// The server refused the grant, or kept refusing its tokens. The
		// grant goes, the session goes, and the person connects again.
		s.user.broker.dropGrant(ctx, s.user, p.src.store, p.subject)
		s.dropPerson(p.subject)
		if asked {
			return fw.ErrorResult(fmt.Sprintf("%s refused the new connection of the person who started the run, so this call did not run.", s.title())), nil
		}
		ok, res, err := s.askConnect(ctx, info.RunID, fw.ConnectExpired)
		if !ok {
			return res, err
		}
		asked = true
	}
}

// connectTool is the placeholder of an entry with grant user before any
// person has listed its tools.
type connectTool struct{ s *Server }

// Spec implements fw.Tool.
func (t *connectTool) Spec() fw.ToolSpec {
	title := t.s.title()
	return fw.ToolSpec{
		Name: t.s.cfg.Name + ".connect",
		Description: fmt.Sprintf("Connect %s for the person who started this run, so that the tools of %s act as that person. "+
			"Call it when the task needs %s. The person signs in once, and the tools of %s appear in the next step.", title, title, title, title),
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		ReadOnly:    true,
		Idempotent:  true,
		Source:      t.s.cfg.Name,
		Trusted:     true,
		Title:       "Connect " + title,
		Action:      "Connect " + title,
	}
}

// Call implements fw.Tool.
func (t *connectTool) Call(ctx context.Context, _ json.RawMessage) (fw.ToolResult, error) {
	s := t.s
	info := fw.RunInfoFrom(ctx)
	if info.PrincipalKind != fw.KindUser || info.Principal == "" {
		return s.noPersonResult(), nil
	}
	if s.skipped(info.RunID) {
		return s.skipResult(), nil
	}
	p := s.personOf(ctx)
	if p == nil {
		return fw.ErrorResult(fmt.Sprintf("%s acts for a person, and this agent keeps no grants.", s.title())), nil
	}
	ready := func() fw.ToolResult {
		s.mu.Lock()
		s.stale = true
		s.mu.Unlock()
		return fw.TextResult(fmt.Sprintf("Connected. The tools of %s appear in the next step.", s.title()))
	}
	_, err := p.src.token(ctx)
	if err == nil {
		return ready(), nil
	}
	reason := reasonOf(err)
	if reason == "" {
		return fw.ErrorResult(fmt.Sprintf("%s cannot get a token for the person who started the run: %v", s.title(), err)), nil
	}
	ok, res, err := s.askConnect(ctx, info.RunID, reason)
	if !ok {
		return res, err
	}
	return ready(), nil
}

// ConnectName implements fw.Connector. Only an entry with grant user acts
// for a person.
func (s *Server) ConnectName() string {
	if s.user == nil {
		return ""
	}
	return s.cfg.Name
}

// errNotPerson is the error of a connector method on an entry that acts as
// the agent itself.
func (s *Server) errNotPerson() error {
	return fmt.Errorf("mcp: server %s does not act for a person", s.cfg.Name)
}

// Connection implements fw.Connector. A server whose authorization server
// cannot be found shows as not connected.
func (s *Server) Connection(ctx context.Context, subject string) (fw.Connection, error) {
	if s.user == nil {
		return fw.Connection{}, s.errNotPerson()
	}
	c := fw.Connection{Server: s.cfg.Name, Title: s.title(), SignIn: s.user.signIn}
	m, err := s.user.ensureMeta(ctx)
	if err != nil {
		s.log.Warn("mcp connection state unknown", "error", err)
		return c, nil
	}
	c.Issuer = m.issuer
	store := grantsOf(ctx)
	if store == nil {
		return c, nil
	}
	g, _, err := s.user.broker.load(ctx, store, s.user.key(m, subject))
	if err != nil {
		if !errors.Is(err, fw.ErrGrantNotFound) {
			s.log.Warn("mcp grant unreadable", "subject", subject, "error", err)
		}
		return c, nil
	}
	c.Connected, c.Account, c.Since, c.Refreshed = true, g.Account, g.Created, g.Refreshed
	return c, nil
}

// BeginConnect implements fw.Connector: an authorization request with PKCE
// (RFC 7636), the state, a nonce with openid, and the resource of every
// entry that the connection covers (RFC 8707).
func (s *Server) BeginConnect(ctx context.Context, subject, redirect string) (fw.ConnectStart, error) {
	u := s.user
	if u == nil {
		return fw.ConnectStart{}, s.errNotPerson()
	}
	if subject == "" {
		return fw.ConnectStart{}, fw.ErrNotConnectable
	}
	if _, err := url.Parse(redirect); err != nil || redirect == "" {
		return fw.ConnectStart{}, fmt.Errorf("mcp: server %s: the redirect address %q is not a URL", s.cfg.Name, redirect)
	}
	m, err := u.ensureMeta(ctx)
	if err != nil {
		return fw.ConnectStart{}, err
	}
	if m.authorize == "" {
		return fw.ConnectStart{}, fmt.Errorf("mcp: server %s: oauth: the authorization server %s has no authorization_endpoint; set auth.oauth.authorizeUrl", s.cfg.Name, m.issuer)
	}
	state, err1 := randomString(32)
	verifier, err2 := randomString(32)
	nonce, err3 := randomString(16)
	if err := errors.Join(err1, err2, err3); err != nil {
		return fw.ConnectStart{}, fmt.Errorf("mcp: server %s: random values: %w", s.cfg.Name, err)
	}
	scopes := u.authScopes(m)
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {u.clientID},
		"redirect_uri":          {redirect},
		"state":                 {state},
		"code_challenge":        {pkceChallenge(verifier)},
		"code_challenge_method": {"S256"},
	}
	if len(scopes) > 0 {
		q.Set("scope", strings.Join(scopes, " "))
	}
	start := fw.ConnectStart{Server: s.cfg.Name, State: state, Verifier: verifier, Redirect: redirect, Expires: s.now().Add(connectTTL)}
	if u.sameIdP(m) {
		q.Set("nonce", nonce)
		start.Nonce = nonce
	}
	for _, r := range u.broker.resourcesFor(u) {
		q.Add("resource", r)
	}
	for k, v := range u.params {
		if !reservedParams[k] {
			q.Set(k, v)
		}
	}
	sep := "?"
	if strings.Contains(m.authorize, "?") {
		sep = "&"
	}
	start.URL = m.authorize + sep + q.Encode()
	return start, nil
}

// FinishConnect implements fw.Connector. It exchanges the code, checks the
// person when the authorization server is the sign-in issuer, and stores
// the grant.
func (s *Server) FinishConnect(ctx context.Context, subject string, start fw.ConnectStart, code string) (fw.Connection, error) {
	u := s.user
	if u == nil {
		return fw.Connection{}, s.errNotPerson()
	}
	if subject == "" {
		return fw.Connection{}, fw.ErrNotConnectable
	}
	if !fw.SameServerName(start.Server, s.cfg.Name) {
		return fw.Connection{}, fmt.Errorf("mcp: server %s: the connection belongs to %s", s.cfg.Name, start.Server)
	}
	if !start.Expires.IsZero() && s.now().After(start.Expires) {
		return fw.Connection{}, fmt.Errorf("mcp: server %s: the connection took longer than %s; start it again", s.cfg.Name, connectTTL)
	}
	if code == "" || start.Verifier == "" {
		return fw.Connection{}, fmt.Errorf("mcp: server %s: the connection has no code", s.cfg.Name)
	}
	store := grantsOf(ctx)
	if store == nil {
		return fw.Connection{}, fmt.Errorf("mcp: server %s: the agent keeps no grants", s.cfg.Name)
	}
	m, err := u.ensureMeta(ctx)
	if err != nil {
		return fw.Connection{}, err
	}
	form := url.Values{
		"grant_type":    {grantCode},
		"code":          {code},
		"redirect_uri":  {start.Redirect},
		"code_verifier": {start.Verifier},
	}
	for _, r := range u.broker.resourcesFor(u) {
		form.Add("resource", r)
	}
	ta, err := u.post(ctx, m, m.token, form)
	if err != nil {
		return fw.Connection{}, fmt.Errorf("mcp: server %s: exchange the code: %w", s.cfg.Name, err)
	}
	if ta.RefreshToken == "" {
		return fw.Connection{}, fmt.Errorf("mcp: server %s: the authorization server gave no refresh token, so fw cannot act for the person later; allow offline access for the client, or set auth.oauth.authParams for a server without the offline_access scope", s.cfg.Name)
	}
	var account string
	if u.sameIdP(m) {
		if ta.IDToken == "" {
			return fw.Connection{}, fmt.Errorf("mcp: server %s: the sign-in issuer gave no ID token, so fw cannot check who signed in", s.cfg.Name)
		}
		c, err := idClaims(ta.IDToken)
		if err != nil {
			return fw.Connection{}, fmt.Errorf("mcp: server %s: %w", s.cfg.Name, err)
		}
		switch {
		case !sameIssuer(claimString(c, "iss"), m.issuer):
			return fw.Connection{}, fmt.Errorf("mcp: server %s: the ID token comes from another issuer", s.cfg.Name)
		case !hasAudience(c, u.clientID):
			return fw.Connection{}, fmt.Errorf("mcp: server %s: the ID token is for another client", s.cfg.Name)
		case start.Nonce != "" && claimString(c, "nonce") != start.Nonce:
			return fw.Connection{}, fmt.Errorf("mcp: server %s: the ID token belongs to another sign-in", s.cfg.Name)
		case claimString(c, "sub") != subject:
			return fw.Connection{}, fmt.Errorf("mcp: server %s: the person signed in at %s as another account than the one that uses fw; sign out there and connect again", s.cfg.Name, m.issuer)
		}
		account = accountOf(c)
	} else if ta.IDToken != "" {
		if c, err := idClaims(ta.IDToken); err == nil && (start.Nonce == "" || claimString(c, "nonce") == start.Nonce) {
			account = accountOf(c)
		}
	}
	now := s.now()
	g := fw.Grant{
		Subject:      subject,
		Issuer:       m.issuer,
		ClientID:     u.clientID,
		RefreshToken: ta.RefreshToken,
		Scopes:       strings.Fields(ta.Scope),
		Account:      account,
		Created:      now,
	}
	if len(g.Scopes) == 0 {
		g.Scopes = u.authScopes(m)
	}
	if secs := firstPositive(expiresIn(ta.RefreshExpiresIn), expiresIn(ta.RefreshTokenExpiresIn)); secs > 0 {
		g.Expires = now.Add(time.Duration(secs) * time.Second)
	}
	key := g.Key()
	l := u.broker.lockFor(key)
	l.Lock()
	_, err = store.Put(ctx, g, 0)
	u.broker.clear(key)
	if err == nil && u.exchange != nil && u.access(ta) == nil {
		// In exchange mode the access token of the code is the base token.
		u.broker.keep(tokenKey{grant: key}, ta)
	}
	l.Unlock()
	if err != nil {
		return fw.Connection{}, fmt.Errorf("mcp: server %s: store the grant: %w", s.cfg.Name, err)
	}
	s.dropPerson(subject)
	s.log.Info("mcp grant stored", "subject", subject, "issuer", m.issuer, "account", account)
	return s.Connection(ctx, subject)
}

// Disconnect implements fw.Connector. It deletes the grant, drops the
// tokens and the session of the person, and revokes the refresh token when
// the authorization server offers revocation (RFC 7009).
func (s *Server) Disconnect(ctx context.Context, subject string) error {
	u := s.user
	if u == nil {
		return s.errNotPerson()
	}
	store := grantsOf(ctx)
	if store == nil {
		return fmt.Errorf("mcp: server %s: the agent keeps no grants", s.cfg.Name)
	}
	m, err := u.ensureMeta(ctx)
	if err != nil {
		return err
	}
	key := u.key(m, subject)
	l := u.broker.lockFor(key)
	l.Lock()
	g, _, gerr := u.broker.load(ctx, store, key)
	err = store.Delete(ctx, key)
	u.broker.clear(key)
	l.Unlock()
	s.dropPerson(subject)
	if err != nil {
		return fmt.Errorf("mcp: server %s: delete the grant: %w", s.cfg.Name, err)
	}
	if gerr == nil {
		u.revoke(context.WithoutCancel(ctx), m, g.RefreshToken)
		s.log.Info("mcp grant deleted", "subject", subject, "issuer", m.issuer)
	}
	return nil
}
