package mcp

// This file is the side of an entry with grant user that faces runs: a
// session per person, the call path that asks the person to connect, and
// the placeholder tool. The tokens of people come from fw.PersonTokens,
// which a linked package sets on the agent (docs/ARCHITECTURE.md section
// 7.11). This package keeps no grant and runs no OAuth flow.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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

// personTokens gives the transport of one person the tokens of that person
// at this server, from the fw.PersonTokens of the agent.
type personTokens struct {
	tokens  fw.PersonTokens
	server  string
	subject string
	// rejected is set when the server refused the grant, or kept refusing
	// the tokens of the person. The call path takes it after a failure.
	rejected atomic.Bool
}

func (p *personTokens) token(ctx context.Context) (string, error) {
	tok, err := p.tokens.Token(ctx, p.subject, p.server)
	if errors.Is(err, fw.ErrConnectionExpired) {
		p.rejected.Store(true)
	}
	return tok, err
}

// invalidate drops a token that the MCP server refused.
func (p *personTokens) invalidate(ctx context.Context, used string) {
	p.tokens.Reject(ctx, p.subject, p.server, used)
}

// forget drops the grant of the person, because the server kept refusing
// its tokens.
func (p *personTokens) forget(ctx context.Context) {
	p.tokens.Forget(ctx, p.subject, p.server)
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

// tokensOf returns the tokens of people of the agent in ctx, or nil.
func tokensOf(ctx context.Context) fw.PersonTokens {
	if a := fw.AgentFrom(ctx); a != nil {
		return a.PersonTokens()
	}
	return nil
}

// personFor returns the person of a subject, and makes one when needed. It
// also closes the sessions that nobody used for personIdle.
func (s *Server) personFor(subject string, tokens fw.PersonTokens) *person {
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
	if p == nil {
		src := &personTokens{tokens: tokens, server: s.cfg.Name, subject: subject}
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
// no person or the agent keeps no tokens of people.
func (s *Server) personOf(ctx context.Context) *person {
	info := fw.RunInfoFrom(ctx)
	if info.PrincipalKind != fw.KindUser || info.Principal == "" {
		return nil
	}
	tokens := tokensOf(ctx)
	if tokens == nil {
		return nil
	}
	return s.personFor(info.Principal, tokens)
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

// noTokensResult is the answer to a call in a process that keeps no
// connections of people.
func (s *Server) noTokensResult() fw.ToolResult {
	return fw.ErrorResult(fmt.Sprintf("%s acts for a person, and this process keeps no connections of people.", s.title()))
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
	case errors.Is(err, fw.ErrNotConnected):
		return fw.ConnectMissing
	case errors.Is(err, fw.ErrConnectionExpired):
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
		return s.noTokensResult(), nil
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
		p.src.forget(ctx)
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
		return s.noTokensResult(), nil
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
