package fw

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrNotImplemented marks a kernel method that is not written yet.
var ErrNotImplemented = errors.New("fw: not implemented")

// ErrRunNotFound is returned for an unknown run id.
var ErrRunNotFound = errors.New("fw: run not found")

// Agent is a configured featherweight agent. Build one with New.
type Agent struct {
	cfg       *Config
	log       *slog.Logger
	model     Model
	modelMW   []ModelMiddleware
	toolMW    []ToolMiddleware
	tools     []Tool
	sources   []ToolSource
	policy    Policy
	ledger    Ledger
	bus       Bus
	secrets   Secrets
	redact    *redactor  // removes secrets from each event, see redact.go
	files     *workspace // the workspace of the file tools, see workspace.go
	compactor Compactor
	surfaces  []Surface
	authn     Authn
	authz     Authz
	tlsConfig *tls.Config
	prices    Prices
	clock     Clock
	grants    GrantStore // the grants of people, see connect.go
	ready     atomic.Bool
	noResume  bool

	// rulesPolicy is true when policy comes from cfg.Policy.Rules, so
	// ProfileGrants can name its pattern rules.
	rulesPolicy bool

	profMu       sync.RWMutex // guards cfg.Agents, which is replaced, never changed in place
	profileStore ProfileStore
	storeWarn    sync.Once

	obsMu      sync.RWMutex
	observers  []Observer
	annotators []RunAnnotator

	modelsMu sync.Mutex
	models   map[string]Model

	zoneMu sync.Mutex
	zones  map[string]zoneEntry // time zones by name, see clock.go

	decisionPoints []external // access.external and policy.external, see external.go

	fleet      Fleet  // nil when the fleet is off, see fleet.go
	accessMode string // AccessModeRules, AccessModeExternal, or AccessModeCustom

	k *kernel // runtime state, see kernel.go
}

// New builds an agent from options, then the configuration file, then the
// FW_* variables, in that order of precedence.
func New(opts ...Option) (*Agent, error) {
	ctx := context.Background()
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	cfg := o.cfg
	if cfg == nil {
		path := o.configPath
		if path == "" && ConfigFinder != nil {
			path = ConfigFinder()
		}
		switch {
		case ConfigLoader != nil:
			// Options, then fw.yaml, then FW_* variables.
			c, err := ConfigLoader(path)
			if err != nil {
				return nil, err
			}
			cfg = c
		case o.configPath != "":
			return nil, errors.New("fw: WithConfigFile needs the config package imported")
		default:
			cfg = DefaultConfig()
		}
	}
	if o.titles != "" {
		cfg.Titles = o.titles
	}
	if cfg.Home == "" {
		cfg.Home = DefaultHome()
	}
	if cfg.Agents == nil {
		cfg.Agents = map[string]AgentProfile{}
	}
	for _, p := range o.profiles {
		cfg.Agents[p.Name] = p
	}
	toolMW := o.toolMW
	if cfg.Tools.Limits.Limit != (Limit{}) || len(cfg.Tools.Limits.PerTool) > 0 {
		// The limits wrap every tool outermost, so the kernel never records a
		// result that the configuration does not allow.
		toolMW = append([]ToolMiddleware{LimitTools(cfg.Tools.Limits)}, o.toolMW...)
	}
	a := &Agent{
		cfg:          cfg,
		log:          o.logger,
		modelMW:      o.modelMW,
		toolMW:       toolMW,
		policy:       o.policy,
		ledger:       o.ledger,
		bus:          o.bus,
		secrets:      o.secrets,
		compactor:    o.compactor,
		authn:        o.authn,
		authz:        o.authz,
		tlsConfig:    o.tlsConfig,
		profileStore: o.store,
		grants:       o.grants,
		prices:       o.prices,
		clock:        o.clock,
		observers:    o.observers,
		annotators:   o.annotators,
		fleet:        o.fleet,
		models:       map[string]Model{},
		noResume:     o.noResume,
	}
	a.ready.Store(true)
	if a.log == nil {
		a.log = slog.Default()
	}
	if a.clock == nil {
		a.clock = SystemClock{}
	}
	if err := checkZones(cfg); err != nil {
		return nil, err
	}
	var err error
	if a.secrets == nil {
		if a.secrets, err = OpenSecrets(ctx, cfg); err != nil {
			return nil, err
		}
	}
	// Every value that a driver resolves becomes a secret that no event
	// holds.
	a.redact = newRedactor(os.Environ())
	a.secrets = recordSecrets{Secrets: a.secrets, r: a.redact}
	files := o.files
	if files == nil {
		if files, err = DirFiles(cfg.WorkspaceRoot()); err != nil {
			return nil, err
		}
	}
	if a.files, err = newWorkspace(files, cfg); err != nil {
		return nil, err
	}
	if a.ledger == nil {
		if a.ledger, err = OpenLedger(ctx, cfg); err != nil {
			return nil, err
		}
	}
	if a.bus == nil {
		if a.bus, err = OpenBus(ctx, cfg); err != nil {
			return nil, err
		}
	}
	if a.authn == nil {
		if a.authn, err = NewAuthn(ctx, cfg, a.secrets); err != nil {
			return nil, err
		}
	}
	switch {
	case a.authz != nil:
		a.accessMode = AccessModeCustom
	case cfg.Access.External != nil:
		a.accessMode = AccessModeExternal
	default:
		a.accessMode = AccessModeRules
	}
	if err := a.setupExternal(ctx, a.authz != nil, a.policy != nil); err != nil {
		return nil, err
	}
	if a.authz == nil {
		a.authz = NewRulesAuthz(cfg.Access, a.ProfileNamed)
	}
	if a.profileStore == nil && ProfileStoreOpener != nil {
		if a.profileStore, err = ProfileStoreOpener(cfg); err != nil {
			return nil, err
		}
	}
	if a.tlsConfig == nil && cfg.TLS.Provider != "" {
		if a.tlsConfig, err = NewTLSConfig(ctx, cfg, a.secrets); err != nil {
			return nil, err
		}
	}
	if o.model != nil {
		a.model = a.wrapModel(o.model)
	} else {
		// Open the default model now when possible. A missing credential is a
		// warning, not a failure, so that a process boots and serves health and
		// metadata before the credential arrives; the first run reports the error.
		m, err := NewModel(ctx, cfg.Model, cfg, a.secrets)
		if err != nil {
			a.log.Warn("default model not available yet", "model", cfg.Model, "error", err)
		} else {
			a.model = a.wrapModel(m)
		}
	}
	if a.model != nil {
		a.models[cfg.Model] = a.model
	}
	if !o.noDefaults {
		for _, name := range cfg.Tools.Builtin {
			t, err := NewBuiltinTool(name, cfg)
			if err != nil {
				a.log.Warn("built-in tool not available", "tool", name, "error", err)
				continue
			}
			a.tools = append(a.tools, a.wrapTool(t))
		}
		srcs, err := NewToolSources(ctx, cfg, a.secrets)
		if err != nil {
			return nil, err
		}
		a.sources = append(a.sources, srcs...)
		for _, name := range cfg.Surfaces {
			s, err := NewSurface(name, cfg)
			if err != nil {
				a.log.Warn("surface not available", "surface", name, "error", err)
				continue
			}
			a.surfaces = append(a.surfaces, s)
		}
	}
	for _, t := range o.tools {
		a.tools = append(a.tools, a.wrapTool(t))
	}
	a.sources = append(a.sources, o.sources...)
	a.surfaces = append(a.surfaces, o.surfaces...)
	if a.policy == nil {
		a.policy = NewRulesPolicy(cfg.Policy.Rules)
		a.rulesPolicy = true
	}
	if a.prices == nil {
		a.prices = PriceTable(cfg.Prices)
	}
	if a.grants == nil {
		// The file store does no I/O until the first connection.
		g, err := NewGrantStore(ctx, cfg, a.secrets)
		if err != nil {
			return nil, err
		}
		a.grants = g
	}
	if err := a.startKernel(ctx); err != nil {
		return nil, err
	}
	if err := a.openFleet(ctx); err != nil {
		a.shutdownKernel(ctx)
		return nil, err
	}
	return a, nil
}

// DefaultHome is /workspace/.fw when /workspace exists, else ./.fw.
func DefaultHome() string {
	if st, err := os.Stat("/workspace"); err == nil && st.IsDir() {
		return "/workspace/.fw"
	}
	return filepath.Join(".", ".fw")
}

func (a *Agent) wrapModel(m Model) Model {
	for i := len(a.modelMW) - 1; i >= 0; i-- {
		m = a.modelMW[i](m)
	}
	return m
}

func (a *Agent) wrapTool(t Tool) Tool {
	for i := len(a.toolMW) - 1; i >= 0; i-- {
		t = a.toolMW[i](t)
	}
	return t
}

// Files returns the workspace (section 7.9). It applies the rules of
// tools.files and records no change. A tool call gets a workspace that
// records its changes through FilesFrom.
func (a *Agent) Files() Files {
	if a.files == nil {
		return nil
	}
	return a.files
}

// Config returns the resolved configuration.
func (a *Agent) Config() *Config { return a.cfg }

// Logger returns the logger.
func (a *Agent) Logger() *slog.Logger { return a.log }

// Model returns the default model.
func (a *Agent) Model() Model { return a.model }

// ModelFor returns the model for a selector or a name from Config.Models,
// opening and caching it on first use.
func (a *Agent) ModelFor(ctx context.Context, name string) (Model, error) {
	selector := a.cfg.ModelSelector(name)
	a.modelsMu.Lock()
	defer a.modelsMu.Unlock()
	if m, ok := a.models[selector]; ok {
		return m, nil
	}
	m, err := NewModel(ctx, selector, a.cfg, a.secrets)
	if err != nil {
		return nil, err
	}
	m = a.wrapModel(m)
	a.models[selector] = m
	return m, nil
}

// Tools returns every tool: the static tools, then the tools of every source.
func (a *Agent) Tools(ctx context.Context) ([]Tool, error) {
	out := append([]Tool(nil), a.tools...)
	for _, s := range a.sources {
		ts, err := s.Tools(ctx)
		if err != nil {
			a.log.Warn("tool source failed", "error", err)
			continue
		}
		for _, t := range ts {
			out = append(out, a.wrapTool(t))
		}
	}
	// Keep insertion order: a stable order keeps the prompt cache prefix valid,
	// and a tool source that appends new tools keeps the old order.
	return out, nil
}

// ToolSpecs returns the specifications of every tool.
func (a *Agent) ToolSpecs(ctx context.Context) ([]ToolSpec, error) {
	ts, err := a.Tools(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ToolSpec, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Spec())
	}
	return out, nil
}

// Policy returns the policy.
func (a *Agent) Policy() Policy { return a.policy }

// Ledger returns the ledger.
func (a *Agent) Ledger() Ledger { return a.ledger }

// Bus returns the bus.
func (a *Agent) Bus() Bus { return a.bus }

// Secrets returns the secrets resolver.
func (a *Agent) Secrets() Secrets { return a.secrets }

// Compactor returns the compactor, or nil for the kernel default.
func (a *Agent) Compactor() Compactor { return a.compactor }

// Authn returns the authenticator.
func (a *Agent) Authn() Authn { return a.authn }

// Prices returns the price table.
func (a *Agent) Prices() Prices { return a.prices }

// Observe adds an observer. Observers run in registration order.
func (a *Agent) Observe(o Observer) {
	a.obsMu.Lock()
	defer a.obsMu.Unlock()
	a.observers = append(a.observers, o)
}

func (a *Agent) notify(e Event) {
	a.obsMu.RLock()
	obs := append([]Observer(nil), a.observers...)
	a.obsMu.RUnlock()
	for _, o := range obs {
		o.Observe(e)
	}
}

// storeObservers returns the observers that measure the stores.
func (a *Agent) storeObservers() []StoreObserver {
	a.obsMu.RLock()
	defer a.obsMu.RUnlock()
	var out []StoreObserver
	for _, o := range a.observers {
		if so, ok := o.(StoreObserver); ok {
			out = append(out, so)
		}
	}
	return out
}

// observeAppend reports the time of one ledger append.
func (a *Agent) observeAppend(d time.Duration) {
	for _, so := range a.storeObservers() {
		so.ObserveLedgerAppend(driverName(a.cfg.Ledger.Driver), d)
	}
}

// countBus reports one bus message in a direction, "in" or "out".
func (a *Agent) countBus(direction string) {
	for _, so := range a.storeObservers() {
		so.CountBusMessage(driverName(a.cfg.Bus.Driver), direction)
	}
}

// driverName is the driver of a store as a metric reports it.
func driverName(d string) string {
	if d = strings.TrimSpace(d); d != "" {
		return d
	}
	return "custom"
}

// ToolSources returns the tool sources of the agent, such as its MCP
// servers.
func (a *Agent) ToolSources() []ToolSource {
	return append([]ToolSource(nil), a.sources...)
}

// Surfaces returns the surfaces.
func (a *Agent) Surfaces() []Surface { return a.surfaces }

// SetReady sets the readiness that /readyz reports.
func (a *Agent) SetReady(ready bool) { a.ready.Store(ready) }

// Ready reports readiness.
func (a *Agent) Ready() bool { return a.ready.Load() }

// Handler builds the shared mux with every surface mounted.
func (a *Agent) Handler() (http.Handler, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !a.Ready() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	})
	for _, s := range a.surfaces {
		if err := s.Mount(mux, a); err != nil {
			return nil, fmt.Errorf("fw: mount surface %T: %w", s, err)
		}
	}
	return mux, nil
}

// Serve listens on Config.Listen, serves every surface, and blocks until
// ctx ends or the listener fails. Without TLS the listener speaks HTTP/1.1
// and unencrypted HTTP/2 on one port. With TLS from Config.TLS or WithTLS,
// it speaks HTTP/1.1 and HTTP/2 over TLS, and a client certificate reaches
// the authenticator through the request.
func (a *Agent) Serve(ctx context.Context) error {
	h, err := a.Handler()
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", a.cfg.Listen)
	if err != nil {
		return fmt.Errorf("fw: listen %s: %w", a.cfg.Listen, err)
	}
	return a.ServeListener(ctx, ln, h)
}

// TLSConfig returns the TLS configuration of the listener, or nil for
// plain HTTP.
func (a *Agent) TLSConfig() *tls.Config { return a.tlsConfig }

// ServeListener serves a handler on a listener with graceful shutdown. It
// wraps the listener with TLS when the agent has a TLS configuration.
func (a *Agent) ServeListener(ctx context.Context, ln net.Listener, h http.Handler) error {
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	if a.tlsConfig != nil {
		tc := a.tlsConfig.Clone()
		if len(tc.NextProtos) == 0 {
			tc.NextProtos = []string{"h2", "http/1.1"}
		}
		ln = tls.NewListener(ln, tc)
		protocols.SetHTTP2(true)
	} else {
		protocols.SetUnencryptedHTTP2(true)
	}
	srv := &http.Server{
		Handler:           h,
		Protocols:         &protocols,
		ReadHeaderTimeout: 30 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	a.log.Info("fw serving", "addr", ln.Addr().String(), "tls", a.tlsConfig != nil)
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		a.shutdownKernel(shutdownCtx)
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Close releases the kernel without serving.
func (a *Agent) Close(ctx context.Context) { a.shutdownKernel(ctx) }

// Ask runs one input on a fresh agent and returns the output text.
func Ask(ctx context.Context, input string, opts ...Option) (string, error) {
	a, err := New(opts...)
	if err != nil {
		return "", err
	}
	defer a.Close(ctx)
	r, err := a.Run(ctx, input)
	if err != nil {
		return "", err
	}
	if r.State == StateFailed {
		return r.Output, fmt.Errorf("fw: run failed: %s", r.Error)
	}
	return r.Output, nil
}

// ErrNoAsker is returned by AskCaller when the context has no asker.
var ErrNoAsker = errors.New("fw: no asker in context")

// ErrDeclined is returned by an asker when the caller declined to answer.
var ErrDeclined = errors.New("fw: declined by the caller")

// closeResources closes every tool, tool source, the bus, and the ledger that
// implements io.Closer. The kernel calls it at the end of shutdown.
func (a *Agent) closeResources() {
	// The fleet goes first, because its last record needs the bus.
	if c, ok := a.fleet.(io.Closer); ok {
		if err := c.Close(); err != nil {
			a.log.Warn("close fleet", "error", err)
		}
	}
	a.closeDecisionPoints()
	for _, t := range a.tools {
		if c, ok := t.(io.Closer); ok {
			if err := c.Close(); err != nil {
				a.log.Warn("close tool", "tool", t.Spec().Name, "error", err)
			}
		}
	}
	for _, s := range a.sources {
		if c, ok := s.(io.Closer); ok {
			if err := c.Close(); err != nil {
				a.log.Warn("close tool source", "error", err)
			}
		}
	}
	if c, ok := a.bus.(io.Closer); ok {
		if err := c.Close(); err != nil {
			a.log.Warn("close bus", "error", err)
		}
	}
	if c, ok := a.ledger.(io.Closer); ok {
		if err := c.Close(); err != nil {
			a.log.Warn("close ledger", "error", err)
		}
	}
}
