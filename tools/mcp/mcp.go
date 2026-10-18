// Package mcp is the MCP client of fw. It connects to the servers under
// tools.mcp in fw.yaml and turns each server tool into an fw.Tool named
// "<server>.<tool>".
//
// An entry with an endpoint uses the Streamable HTTP transport. An entry with
// a command uses the stdio transport. The client library is
// github.com/modelcontextprotocol/go-sdk, which speaks revision 2026-07-28
// and falls back to the initialize handshake for older servers.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os/exec"
	"runtime/debug"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/bdubs00/featherweight-kernel/authn/spiffe"
	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultTimeout = 60 * time.Second
	failureLimit   = 5                // failures in a row before the server is down
	downTime       = 30 * time.Second // how long the server stays down
	oauthTimeout   = 30 * time.Second // limit for one metadata or token request
)

func init() {
	fw.RegisterToolSource("mcp", newSources)
}

// newSources builds one Server per entry under tools.mcp. A configuration
// error fails the whole set. A server that is down does not, because each
// Server connects on first use. An entry that a control plane or
// policy.external names is not a tool source: the decision point has a
// connection of its own, and the model never sees a decision tool.
func newSources(ctx context.Context, cfg *fw.Config, secrets fw.Secrets) ([]fw.ToolSource, error) {
	log := slog.Default().With("component", "mcp")
	seen := map[string]bool{}
	out := make([]fw.ToolSource, 0, len(cfg.Tools.MCP))
	for i, c := range cfg.Tools.MCP {
		if c.Name == "" {
			return nil, fmt.Errorf("mcp: tools.mcp[%d]: name is required", i)
		}
		if seen[c.Name] {
			return nil, fmt.Errorf("mcp: tools.mcp[%d]: server name %q is used twice", i, c.Name)
		}
		seen[c.Name] = true
		if cfg.IsControlServer(c.Name) {
			log.Info("mcp server used for decisions; the model does not see its tools", "mcpServer", c.Name)
			continue
		}
		s, err := New(ctx, c, secrets, log)
		if err != nil {
			return nil, err
		}
		s.useSPIFFESocket(cfg)
		out = append(out, s)
	}
	return out, nil
}

// useSPIFFESocket points the outbound JWT-SVID at the Workload API socket
// that auth.spiffe names, when it names one.
func (s *Server) useSPIFFESocket(cfg *fw.Config) {
	if s.rt != nil && s.rt.svid != nil && cfg.Auth.SPIFFE != nil {
		s.rt.svid.Socket = cfg.Auth.SPIFFE.Socket
	}
}

// Server is one connection to one MCP server. It implements fw.ToolSource.
// It is safe for concurrent use.
type Server struct {
	cfg     fw.MCPServerConfig
	secrets fw.Secrets
	log     *slog.Logger
	timeout time.Duration
	client  *http.Client   // nil for a stdio server
	rt      *authTransport // nil for a stdio server
	now     func() time.Time
	// personal is true for an entry with auth.oauth.grant user. Its tokens
	// come from the fw.PersonTokens of the agent (person.go).
	personal bool
	// protocol pins the protocol version for tests. Empty means the SDK
	// default: 2026-07-28 with a fallback to initialize.
	protocol string

	connMu    sync.Mutex // one connect at a time
	refreshMu sync.Mutex // one tools/list at a time

	mu        sync.Mutex
	session   *sdk.ClientSession
	tools     []*tool // in the order the model sees them
	listed    bool    // tools holds a list from the server
	stale     bool    // the server reported tools/list_changed
	failures  int     // failures in a row
	downUntil time.Time
	calls     map[string]*call // in-flight calls by progress token
	seq       uint64
	closed    bool
	persons   map[string]*person   // the sessions of people, for grant user
	skips     map[string]time.Time // runs whose person skipped the service
}

// New validates an entry and returns a Server for it. It does not connect:
// the first call to Tools or Call does, so a server that is down does not
// stop fw from starting. A nil secrets reads the environment. A nil log
// uses slog.Default.
func New(ctx context.Context, cfg fw.MCPServerConfig, secrets fw.Secrets, log *slog.Logger) (*Server, error) {
	_ = ctx // New does no I/O; the first Tools or Call connects.
	if secrets == nil {
		secrets = fw.EnvSecrets{}
	}
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		cfg:     cfg,
		secrets: secrets,
		log:     log.With("mcpServer", cfg.Name),
		timeout: cfg.Timeout,
		now:     time.Now,
		calls:   map[string]*call{},
		persons: map[string]*person{},
		skips:   map[string]time.Time{},
	}
	if s.timeout == 0 {
		s.timeout = defaultTimeout
	}
	if err := s.configure(); err != nil {
		return nil, fmt.Errorf("mcp: server %q: %w", cfg.Name, err)
	}
	return s, nil
}

// configure checks the entry and builds the HTTP client for an endpoint.
func (s *Server) configure() error {
	c := s.cfg
	switch {
	case c.Name == "":
		return errors.New("name is required")
	case strings.ContainsAny(c.Name, " \t\n"):
		return errors.New("name must not contain spaces")
	case c.Endpoint != "" && c.Command != "":
		return errors.New("set endpoint or command, not both")
	case c.Endpoint == "" && c.Command == "":
		return errors.New("set endpoint or command")
	case c.Timeout < 0:
		return errors.New("timeout must not be negative")
	}
	if c.Command != "" {
		if c.TLS != (fw.TLSConfig{}) || c.Auth.Bearer != "" || c.Auth.OAuth != nil || c.Auth.SPIFFE != nil || len(c.Headers) > 0 {
			return errors.New("tls, auth, and headers apply to an endpoint only, not to a command")
		}
		return nil
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil {
		return fmt.Errorf("endpoint is not a URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("endpoint scheme %q is not http or https", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("endpoint has no host")
	}
	if u.Scheme == "http" && !isLocalHost(u.Hostname()) {
		s.log.Warn("mcp endpoint uses plain http outside the cluster; use https", "host", u.Hostname())
	}
	if c.Auth.Bearer != "" && c.Auth.OAuth != nil {
		return errors.New("set auth.bearer or auth.oauth, not both")
	}
	if c.Auth.SPIFFE != nil && (c.Auth.Bearer != "" || c.Auth.OAuth != nil) {
		return errors.New("set auth.spiffe alone, not with auth.bearer or auth.oauth")
	}
	if o := c.Auth.OAuth; o != nil {
		if g := strings.ToLower(strings.TrimSpace(o.Grant)); g != "" && g != fw.GrantUser && g != fw.GrantClient {
			return fmt.Errorf("auth.oauth.grant %q is not client or user", o.Grant)
		}
		s.personal = o.GrantOf() == fw.GrantUser
		if !s.personal && (o.ClientID == "" || o.ClientSecret == "") {
			return errors.New("auth.oauth needs clientId and clientSecret")
		}
		if o.TokenURL != "" {
			if err := safeTokenURL(o.TokenURL); err != nil {
				return fmt.Errorf("auth.oauth.tokenUrl: %w", err)
			}
		}
		if o.AuthorizeURL != "" {
			if err := safeTokenURL(o.AuthorizeURL); err != nil {
				return fmt.Errorf("auth.oauth.authorizeUrl: %w", err)
			}
		}
	}
	tc, err := loadTLS(c.TLS)
	if err != nil {
		return err
	}
	s.rt = &authTransport{
		base:      baseTransport(tc),
		server:    c.Name,
		endpoint:  u,
		secrets:   s.secrets,
		bearer:    c.Auth.Bearer,
		raw:       c.Headers,
		retries:   3,
		retryBase: 100 * time.Millisecond,
	}
	if o := c.Auth.OAuth; o != nil && !s.personal {
		otc, err := oauthTLS(c.TLS)
		if err != nil {
			return err
		}
		hc := &http.Client{Transport: baseTransport(otc), Timeout: oauthTimeout}
		s.rt.oauth = newOAuthClient(c.Name, o, s.secrets, u, hc, s.log)
	}
	if sp := c.Auth.SPIFFE; sp != nil {
		// Every request carries a JWT-SVID of this workload for the audience
		// named, or for the endpoint URL. The Workload API is not touched
		// before the first request.
		aud := sp.Audience
		if aud == "" {
			aud = c.Endpoint
		}
		s.rt.svid = spiffe.NewTokenSource("", aud)
	}
	s.client = &http.Client{Transport: s.rt}
	return nil
}

// Name returns the entry name, which prefixes every tool of the server.
func (s *Server) Name() string { return s.cfg.Name }

// Up reports whether the server is not marked down. A metrics gauge reads it.
func (s *Server) Up() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.downLocked()
}

// Instructions returns the instructions that the server sent at discovery,
// or "" before the first connection.
func (s *Server) Instructions() string {
	s.mu.Lock()
	cs := s.session
	for _, p := range s.persons {
		if cs == nil && p.session != nil {
			cs = p.session
		}
	}
	s.mu.Unlock()
	if cs == nil {
		return ""
	}
	if r := cs.InitializeResult(); r != nil {
		return r.Instructions
	}
	return ""
}

// Close ends the session. For a stdio server it stops the process. Tools and
// Call fail after Close.
func (s *Server) Close() error {
	s.mu.Lock()
	cs := s.session
	s.session = nil
	s.closed = true
	var others []*sdk.ClientSession
	for _, p := range s.persons {
		if p.session != nil {
			others = append(others, p.session)
			p.session = nil
		}
	}
	s.mu.Unlock()
	if s.rt != nil && s.rt.svid != nil {
		_ = s.rt.svid.Close()
	}
	for _, o := range others {
		_ = o.Close()
	}
	if cs != nil {
		return cs.Close()
	}
	return nil
}

func (s *Server) downLocked() bool { return s.now().Before(s.downUntil) }

// succeeded records an answer from the server.
func (s *Server) succeeded() {
	s.mu.Lock()
	s.failures = 0
	s.mu.Unlock()
}

// failed records a failure. After failureLimit failures in a row the server
// is down for downTime. The count is not reset when the server goes down, so
// the first failure after the down time marks it down again.
func (s *Server) failed(op string, err error) {
	s.mu.Lock()
	s.failures++
	n := s.failures
	if n >= failureLimit {
		s.downUntil = s.now().Add(downTime)
	}
	s.mu.Unlock()
	if n == failureLimit {
		s.log.Warn("mcp server marked down", "op", op, "failures", n, "for", downTime, "error", err)
	} else {
		s.log.Debug("mcp server request failed", "op", op, "failures", n, "error", err)
	}
}

// downResult is the error result for a call while the server is down.
func (s *Server) downResult() fw.ToolResult {
	s.mu.Lock()
	wait := s.downUntil.Sub(s.now()).Round(time.Second)
	s.mu.Unlock()
	return fw.ErrorResult(fmt.Sprintf("MCP server %s is down after %d failed requests in a row. Calls resume in %s.",
		s.cfg.Name, failureLimit, wait))
}

// connect returns the session, and opens one when there is none. The
// session does not take values from ctx: the SDK hands its context to the
// handlers of server requests, and those must not see the first caller.
func (s *Server) connect(ctx context.Context) (*sdk.ClientSession, error) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	s.mu.Lock()
	cs, closed := s.session, s.closed
	s.mu.Unlock()
	if closed {
		return nil, fmt.Errorf("mcp: server %s is closed", s.cfg.Name)
	}
	if cs != nil {
		return cs, nil
	}
	cs, err := s.dial(ctx, s.transport())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = cs.Close()
		return nil, fmt.Errorf("mcp: server %s is closed", s.cfg.Name)
	}
	s.session = cs
	s.mu.Unlock()
	// Forget the session when its connection ends, so the next use
	// reconnects.
	go func() {
		_ = cs.Wait()
		s.forget(cs)
	}()
	return cs, nil
}

// dial opens a session over a transport. The session does not take values
// from ctx, but ctx can cancel the dial.
func (s *Server) dial(ctx context.Context, tr sdk.Transport) (*sdk.ClientSession, error) {
	cctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	client := sdk.NewClient(&sdk.Implementation{Name: "featherweight-kernel", Version: version()}, &sdk.ClientOptions{
		Logger: s.log,
		// fw answers form and URL elicitation. It does not offer roots or
		// sampling, which revision 2026-07-28 deprecates.
		Capabilities: &sdk.ClientCapabilities{
			Elicitation: &sdk.ElicitationCapabilities{
				Form: &sdk.FormElicitationCapabilities{},
				URL:  &sdk.URLElicitationCapabilities{},
			},
		},
		ElicitationHandler:          s.elicit,
		ToolListChangedHandler:      s.listChanged,
		ProgressNotificationHandler: s.progress,
	})
	var opts *sdk.ClientSessionOptions
	if s.protocol != "" {
		opts = &sdk.ClientSessionOptions{ProtocolVersion: s.protocol}
	}
	cs, err := client.Connect(cctx, tr, opts)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("mcp: connect to server %s: %w", s.cfg.Name, err)
	}
	if r := cs.InitializeResult(); r != nil {
		s.log.Debug("mcp server connected", "protocol", r.ProtocolVersion)
	}
	return cs, nil
}

// forget drops a session that has ended.
func (s *Server) forget(cs *sdk.ClientSession) {
	s.mu.Lock()
	if s.session == cs {
		s.session = nil
	}
	s.mu.Unlock()
}

// transport returns a new SDK transport for the entry.
func (s *Server) transport() sdk.Transport {
	if s.cfg.Command != "" {
		cmd := exec.Command(s.cfg.Command, s.cfg.Args...)
		cmd.Stderr = &logWriter{log: s.log}
		return &sdk.CommandTransport{Command: cmd}
	}
	return &sdk.StreamableClientTransport{Endpoint: s.cfg.Endpoint, HTTPClient: s.client}
}

// listChanged handles notifications/tools/list_changed. The next Tools call
// lists the tools again.
func (s *Server) listChanged(context.Context, *sdk.ToolListChangedRequest) {
	s.mu.Lock()
	s.stale = true
	s.mu.Unlock()
	s.log.Debug("mcp tool list changed")
}

// Tools implements fw.ToolSource. It lists the tools on first use and after
// the server reports a change. A new list keeps the old order and appends
// new tools, so the prompt cache prefix stays valid. While the server is
// down, or when a new list fails, Tools returns the cached list.
func (s *Server) Tools(ctx context.Context) ([]fw.Tool, error) {
	if s.personal {
		return s.personTools(ctx)
	}
	s.mu.Lock()
	listed, stale, down := s.listed, s.stale, s.downLocked()
	s.mu.Unlock()
	if listed && (!stale || down) {
		return s.snapshot(), nil
	}
	if down {
		return nil, fmt.Errorf("mcp: server %s is down", s.cfg.Name)
	}
	if err := s.refresh(ctx); err != nil {
		if listed {
			s.log.Warn("mcp tool list refresh failed; using the cached list", "error", err)
			return s.snapshot(), nil
		}
		return nil, err
	}
	return s.snapshot(), nil
}

// snapshot returns the cached tools as fw.Tool values.
func (s *Server) snapshot() []fw.Tool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]fw.Tool, len(s.tools))
	for i, t := range s.tools {
		out[i] = t
	}
	return out
}

// refresh lists the tools from the server, following every page.
func (s *Server) refresh(ctx context.Context) error {
	return s.refreshWith(ctx, s.connect)
}

// refreshWith lists the tools over the session that open returns.
func (s *Server) refreshWith(ctx context.Context, open func(context.Context) (*sdk.ClientSession, error)) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	s.mu.Lock()
	if s.listed && !s.stale {
		// Another caller refreshed while this one waited.
		s.mu.Unlock()
		return nil
	}
	s.stale = false
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	// fail keeps the list stale, so the next Tools call tries again. A
	// protocol error means the server answered, so it does not count
	// toward the circuit breaker.
	fail := func(err error) error {
		s.mu.Lock()
		s.stale = true
		s.mu.Unlock()
		var werr *jsonrpc.Error
		switch {
		case errors.As(err, &werr):
			s.succeeded()
		case ctx.Err() == nil || errors.Is(ctx.Err(), context.DeadlineExceeded):
			s.failed("tools/list", err)
		}
		return err
	}
	cs, err := open(ctx)
	if err != nil {
		return fail(err)
	}
	var fresh []*tool
	for t, err := range cs.Tools(ctx, nil) {
		if err != nil {
			return fail(fmt.Errorf("mcp: server %s: tools/list: %w", s.cfg.Name, err))
		}
		fresh = append(fresh, s.newTool(t))
	}
	s.succeeded()
	s.mu.Lock()
	s.tools = mergeTools(s.tools, fresh)
	s.listed = true
	n := len(s.tools)
	s.mu.Unlock()
	s.log.Debug("mcp tools listed", "count", n)
	return nil
}

// mergeTools keeps the tools of old that are still in fresh, in their old
// order and with their new specs, then appends the new tools sorted by name.
func mergeTools(old, fresh []*tool) []*tool {
	byName := make(map[string]*tool, len(fresh))
	for _, t := range fresh {
		byName[t.name] = t
	}
	out := make([]*tool, 0, len(fresh))
	for _, t := range old {
		if f, ok := byName[t.name]; ok {
			out = append(out, f)
			delete(byName, t.name)
		}
	}
	added := make([]*tool, 0, len(byName))
	for _, t := range fresh {
		if f, ok := byName[t.name]; ok {
			// A name the server lists twice keeps its last definition.
			added = append(added, f)
			delete(byName, t.name)
		}
	}
	sort.Slice(added, func(i, j int) bool { return added[i].spec.Name < added[j].spec.Name })
	return append(out, added...)
}

// newTool turns an MCP tool definition into an fw tool.
func (s *Server) newTool(t *sdk.Tool) *tool {
	schema := json.RawMessage(`{"type":"object"}`)
	if t.InputSchema != nil {
		if b, err := json.Marshal(t.InputSchema); err == nil && string(b) != "null" {
			schema = b
		}
	}
	desc := t.Description
	if desc == "" {
		desc = t.Title
	}
	var readOnly, idempotent bool
	destructive := true // the MCP default when destructiveHint is absent
	if a := t.Annotations; a != nil {
		readOnly = a.ReadOnlyHint
		idempotent = a.IdempotentHint
		if a.DestructiveHint != nil {
			destructive = *a.DestructiveHint
		}
	}
	if readOnly {
		// MCP reads destructiveHint only when readOnlyHint is false.
		destructive = false
	}
	var action string
	if s.cfg.Trusted {
		action = actionOf(t)
	}
	return &tool{
		server: s,
		name:   t.Name,
		spec: fw.ToolSpec{
			Name:         s.cfg.Name + "." + t.Name,
			Description:  desc,
			InputSchema:  schema,
			ReadOnly:     readOnly,
			Idempotent:   idempotent,
			Destructive:  destructive,
			ParallelSafe: readOnly,
			Source:       s.cfg.Name,
			Trusted:      s.cfg.Trusted,
			Action:       action,
		},
	}
}

// maxActionRunes caps the allow-button label that a server title gives.
const maxActionRunes = 40

// actionOf returns the allow-button label of a tool from the title that
// its server gives: Title, else Annotations.Title. The caller uses it only
// for a trusted server, because the label speaks for the call. Spaces
// collapse, characters that do not print go away, and a long title is cut
// at a word to maxActionRunes with an ellipsis. Without a title, the label
// is empty and the surfaces show a neutral one. The tool name never
// becomes a label.
func actionOf(t *sdk.Tool) string {
	title := t.Title
	if strings.TrimSpace(title) == "" && t.Annotations != nil {
		title = t.Annotations.Title
	}
	title = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		if !unicode.IsPrint(r) {
			return -1
		}
		return r
	}, title)
	title = strings.Join(strings.Fields(title), " ")
	r := []rune(title)
	if len(r) <= maxActionRunes {
		return title
	}
	// The ellipsis takes the last rune. The cut keeps a whole word when the
	// rune after it is a space.
	cut := string(r[:maxActionRunes-1])
	if r[maxActionRunes-1] != ' ' {
		if i := strings.LastIndexByte(cut, ' '); i >= len(cut)/2 {
			cut = cut[:i]
		}
	}
	if cut = strings.TrimRight(cut, " ,;:-"); cut == "" {
		return ""
	}
	return cut + "…"
}

// tool is one MCP tool as an fw.Tool.
type tool struct {
	server *Server
	name   string // the name on the server, without the prefix
	spec   fw.ToolSpec
}

// Spec implements fw.Tool.
func (t *tool) Spec() fw.ToolSpec { return t.spec }

// Call implements fw.Tool.
func (t *tool) Call(ctx context.Context, input json.RawMessage) (fw.ToolResult, error) {
	return t.server.Call(ctx, t.name, input)
}

// call is the state of one in-flight tools/call.
type call struct {
	ctx     context.Context // the caller context, without the call timeout
	timeout time.Duration
	timer   *time.Timer // cancels the call when the time limit passes

	mu     sync.Mutex
	paused int // input requests that wait for the caller
}

// errTimeout is the cause of a call context that ran out of time.
var errTimeout = errors.New("mcp: call timed out")

// pause stops the call timer while fw waits for the caller. A human answer
// does not count against the time limit of the call.
func (c *call) pause() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paused == 0 {
		c.timer.Stop()
	}
	c.paused++
}

// resume restarts the call timer with the full time limit.
func (c *call) resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paused--
	if c.paused == 0 {
		c.timer.Reset(c.timeout)
	}
}

type callKey struct{}

// Call runs tools/call for a tool, by its name on the server, with the time
// limit of the entry. It returns an error result, not an error, when the
// server fails or is down, so the model reads what happened. It returns an
// error only when ctx ends.
func (s *Server) Call(ctx context.Context, name string, args json.RawMessage) (fw.ToolResult, error) {
	s.mu.Lock()
	down := s.downLocked()
	s.mu.Unlock()
	if down {
		return s.downResult(), nil
	}
	var arguments any
	if len(args) > 0 && string(args) != "null" {
		if !json.Valid(args) {
			return fw.ErrorResult(fmt.Sprintf("arguments for %s.%s are not valid JSON", s.cfg.Name, name)), nil
		}
		arguments = args
	}
	if s.personal {
		return s.callAsPerson(ctx, name, arguments)
	}
	res, _, err := s.callSession(ctx, name, arguments, s.connect, s.forget, nil)
	return res, err
}

// callSession runs one tools/call over the session that open returns,
// with the time limit of the entry. forget drops a session that closed.
// authFailed, when set, reports whether a failure came from the grant of a
// person: such a failure does not count toward the circuit breaker, and
// callSession returns true with no result, so the caller asks the person
// to connect again.
func (s *Server) callSession(ctx context.Context, name string, arguments any,
	open func(context.Context) (*sdk.ClientSession, error), forget func(*sdk.ClientSession), authFailed func() bool,
) (fw.ToolResult, bool, error) {
	cctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	c := &call{ctx: ctx, timeout: s.timeout}
	c.timer = time.AfterFunc(s.timeout, func() { cancel(errTimeout) })
	defer c.timer.Stop()
	cctx = context.WithValue(cctx, callKey{}, c)
	auth := func() bool { return authFailed != nil && ctx.Err() == nil && authFailed() }

	cs, err := open(cctx)
	if err != nil {
		if auth() {
			return fw.ToolResult{}, true, nil
		}
		res, err := s.callFailed(ctx, cctx, name, "connect", err)
		return res, false, err
	}
	token := s.track(c)
	defer s.untrack(token)
	params := &sdk.CallToolParams{Name: name, Arguments: arguments}
	params.SetProgressToken(token)
	res, err := cs.CallTool(cctx, params)
	if err != nil {
		var werr *jsonrpc.Error
		if errors.As(err, &werr) && ctx.Err() == nil && !errors.Is(context.Cause(cctx), errTimeout) {
			// The server answered with a protocol error.
			s.succeeded()
			if werr.Code == sdk.CodeURLElicitationRequired {
				res, err := s.urlElicitation(ctx, cs, cctx, params, werr)
				return res, false, err
			}
			if unknownTool(werr) {
				s.mu.Lock()
				s.stale = true
				s.mu.Unlock()
				if rerr := s.refreshWith(ctx, open); rerr != nil {
					s.log.Warn("mcp tool list refresh failed", "error", rerr)
				}
			}
			return fw.ErrorResult(fmt.Sprintf("MCP server %s rejected the call to %s: %s", s.cfg.Name, name, werr.Message)), false, nil
		}
		if auth() {
			forget(cs)
			return fw.ToolResult{}, true, nil
		}
		if errors.Is(err, sdk.ErrConnectionClosed) {
			forget(cs)
		}
		res, err := s.callFailed(ctx, cctx, name, "tools/call", err)
		return res, false, err
	}
	s.succeeded()
	return convertResult(res), false, nil
}

// callFailed turns a failed call into an error result and records the
// failure. When the caller context ended, it returns that error instead.
func (s *Server) callFailed(ctx, cctx context.Context, name, op string, err error) (fw.ToolResult, error) {
	if ctx.Err() != nil {
		return fw.ToolResult{}, fmt.Errorf("mcp: call %s.%s: %w", s.cfg.Name, name, ctx.Err())
	}
	if errors.Is(context.Cause(cctx), errTimeout) {
		s.failed(op, errTimeout)
		return fw.ErrorResult(fmt.Sprintf("MCP server %s did not answer the call to %s within %s.", s.cfg.Name, name, s.timeout)), nil
	}
	s.failed(op, err)
	return fw.ErrorResult(fmt.Sprintf("MCP server %s failed the call to %s: %v", s.cfg.Name, name, err)), nil
}

// unknownTool reports whether a protocol error means that the server does
// not know the tool.
func unknownTool(e *jsonrpc.Error) bool {
	if e.Code != jsonrpc.CodeInvalidParams && e.Code != jsonrpc.CodeMethodNotFound {
		return false
	}
	m := strings.ToLower(e.Message)
	return strings.Contains(m, "unknown tool") || (strings.Contains(m, "tool") && strings.Contains(m, "not found"))
}

// track registers an in-flight call and returns its progress token.
func (s *Server) track(c *call) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	token := "fw-" + strconv.FormatUint(s.seq, 10)
	s.calls[token] = c
	return token
}

func (s *Server) untrack(token string) {
	s.mu.Lock()
	delete(s.calls, token)
	s.mu.Unlock()
}

// progress forwards notifications/progress to the progress reporter of the
// call that owns the token.
func (s *Server) progress(_ context.Context, req *sdk.ProgressNotificationClientRequest) {
	p := req.Params
	if p == nil {
		return
	}
	s.mu.Lock()
	c := s.calls[fmt.Sprint(p.ProgressToken)]
	s.mu.Unlock()
	if c != nil {
		fw.ReportProgress(c.ctx, p.Progress, p.Total, p.Message)
	}
}

// callFor finds the call that a server request belongs to. Revision
// 2026-07-28 returns input requests inside the tools/call result, so the SDK
// passes the call context. An older server sends elicitation/create on its
// own, with no link to a call; then fw uses the only in-flight call, if
// there is exactly one.
func (s *Server) callFor(ctx context.Context) *call {
	if c, ok := ctx.Value(callKey{}).(*call); ok {
		return c
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) != 1 {
		return nil
	}
	for _, c := range s.calls {
		return c
	}
	return nil
}

// ask asks the caller of the run through fw.AskCaller. The call timer stops
// while fw waits for the answer.
func (s *Server) ask(ctx context.Context, question string, schema []byte) ([]byte, error) {
	c := s.callFor(ctx)
	if c == nil {
		return nil, fw.ErrNoAsker
	}
	c.pause()
	defer c.resume()
	return fw.AskCaller(c.ctx, question, schema)
}

// elicit answers elicitation/create. Form mode asks the caller with the
// requested schema. URL mode asks the caller to open the URL and confirm.
func (s *Server) elicit(ctx context.Context, req *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
	p := req.Params
	mode := p.Mode
	if mode == "" {
		mode = "form"
		if p.URL != "" {
			mode = "url"
		}
	}
	question := p.Message
	var schema []byte
	if mode == "url" {
		question = urlQuestion(p.Message, p.URL)
	} else if p.RequestedSchema != nil {
		b, err := json.Marshal(p.RequestedSchema)
		if err != nil {
			return nil, fmt.Errorf("mcp: encode requested schema: %w", err)
		}
		schema = b
	}
	answer, err := s.ask(ctx, question, schema)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		if errors.Is(err, fw.ErrNoAsker) {
			s.log.Warn("mcp server asked for input but the run has no caller to ask", "mode", mode)
			return &sdk.ElicitResult{Action: "cancel"}, nil
		}
		s.log.Info("mcp elicitation declined", "mode", mode, "error", err)
		return &sdk.ElicitResult{Action: "decline"}, nil
	}
	if mode == "url" {
		return &sdk.ElicitResult{Action: "accept"}, nil
	}
	var content map[string]any
	if len(answer) > 0 {
		if err := json.Unmarshal(answer, &content); err != nil {
			s.log.Warn("mcp elicitation answer is not a JSON object; declining", "error", err)
			return &sdk.ElicitResult{Action: "decline"}, nil
		}
	}
	return &sdk.ElicitResult{Action: "accept", Content: content}, nil
}

func urlQuestion(message, u string) string {
	if message == "" {
		return "Open this URL to continue, then confirm: " + u
	}
	return message + "\n\nOpen this URL to continue, then confirm: " + u
}

// urlElicitation handles the error -32042 of revision 2025-11-25: the server
// needs the caller to visit one or more URLs. fw asks the caller for each
// URL, then retries the call once.
func (s *Server) urlElicitation(ctx context.Context, cs *sdk.ClientSession, cctx context.Context, params *sdk.CallToolParams, werr *jsonrpc.Error) (fw.ToolResult, error) {
	var data struct {
		Elicitations []*sdk.ElicitParams `json:"elicitations"`
	}
	if len(werr.Data) > 0 {
		if err := json.Unmarshal(werr.Data, &data); err != nil {
			return fw.ErrorResult(fmt.Sprintf("MCP server %s asked for a URL visit with unreadable data: %v", s.cfg.Name, err)), nil
		}
	}
	if len(data.Elicitations) == 0 {
		return fw.ErrorResult(fmt.Sprintf("MCP server %s asked for a URL visit but gave no URL", s.cfg.Name)), nil
	}
	for _, e := range data.Elicitations {
		if _, err := s.ask(cctx, urlQuestion(e.Message, e.URL), nil); err != nil {
			if ctx.Err() != nil {
				return fw.ToolResult{}, fmt.Errorf("mcp: call %s.%s: %w", s.cfg.Name, params.Name, ctx.Err())
			}
			return fw.ErrorResult(fmt.Sprintf("MCP server %s needs the caller to open %s, and the caller did not confirm", s.cfg.Name, e.URL)), nil
		}
	}
	res, err := cs.CallTool(cctx, params)
	if err != nil {
		var werr *jsonrpc.Error
		if errors.As(err, &werr) && ctx.Err() == nil {
			s.succeeded()
			return fw.ErrorResult(fmt.Sprintf("MCP server %s rejected the call to %s: %s", s.cfg.Name, params.Name, werr.Message)), nil
		}
		return s.callFailed(ctx, cctx, params.Name, "tools/call", err)
	}
	s.succeeded()
	return convertResult(res), nil
}

// convertResult maps an MCP tool result to an fw tool result.
func convertResult(res *sdk.CallToolResult) fw.ToolResult {
	out := fw.ToolResult{IsError: res.IsError, Content: []fw.Block{}}
	for _, c := range res.Content {
		if b, ok := convertContent(c); ok {
			out.Content = append(out.Content, b)
		}
	}
	if res.StructuredContent != nil {
		if b, err := json.Marshal(res.StructuredContent); err == nil {
			out.Structured = b
		}
	}
	return out
}

// convertContent maps one MCP content block to an fw block.
func convertContent(c sdk.Content) (fw.Block, bool) {
	switch v := c.(type) {
	case *sdk.TextContent:
		return fw.Text(v.Text), true
	case *sdk.ImageContent:
		return fw.Block{Type: fw.BlockImage, MediaType: v.MIMEType, Data: v.Data}, true
	case *sdk.AudioContent:
		return fw.Text(fmt.Sprintf("[audio %s, %d bytes, not shown]", v.MIMEType, len(v.Data))), true
	case *sdk.ResourceLink:
		return fw.Text(resourceLinkText(v)), true
	case *sdk.EmbeddedResource:
		r := v.Resource
		if r == nil {
			return fw.Block{}, false
		}
		if r.Text != "" || len(r.Blob) == 0 {
			return fw.Text(r.Text), true
		}
		if strings.HasPrefix(r.MIMEType, "image/") {
			return fw.Block{Type: fw.BlockImage, MediaType: r.MIMEType, Data: r.Blob}, true
		}
		return fw.Text(fmt.Sprintf("[resource %s (%s), %d bytes, not shown]", r.URI, r.MIMEType, len(r.Blob))), true
	}
	if b, err := json.Marshal(c); err == nil {
		return fw.Text(string(b)), true
	}
	return fw.Block{}, false
}

func resourceLinkText(r *sdk.ResourceLink) string {
	parts := []string{"Resource: " + r.URI}
	if r.Name != "" {
		parts = append(parts, "name: "+r.Name)
	}
	if r.MIMEType != "" {
		parts = append(parts, "type: "+r.MIMEType)
	}
	if r.Description != "" {
		parts = append(parts, "description: "+r.Description)
	}
	return strings.Join(parts, "\n")
}

// version returns the module version of the running binary.
func version() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

// logWriter writes the stderr of a stdio server to the log, one record per
// line.
type logWriter struct {
	log *slog.Logger
	mu  sync.Mutex
	buf []byte
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := slices.Index(w.buf, '\n')
		if i < 0 {
			break
		}
		if line := strings.TrimSpace(string(w.buf[:i])); line != "" {
			w.log.Info("mcp server stderr", "line", line)
		}
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) > 64<<10 {
		w.log.Info("mcp server stderr", "line", string(w.buf))
		w.buf = w.buf[:0]
	}
	return len(p), nil
}
