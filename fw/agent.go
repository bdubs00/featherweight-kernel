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
	holds     atomic.Int32 // open holds of background work (Hold)
	cfg       *Config
	log       *slog.Logger
	model     Model
	modelMW   []ModelMiddleware
	toolMW    []ToolMiddleware
	tools     []Tool
	sources   []ToolSource
	live      liveHub // the live streams of the runs (live.go)
	policy    Policy
	ledger    Ledger
	bus       Bus
	secrets   Secrets
	redact    *redactor  // removes secrets from each event, see redact.go
	files     *workspace // the workspace of the file tools, see workspace.go
	toolUser  *ToolUser  // the user of the processes of tools, or nil, see tooluser.go
	compactor Compactor
	surfaces  []Surface
	authn     Authn
	authz     Authz
	tlsConfig *tls.Config
	prices    Prices
	clock     Clock
	spawner   Spawner // the children in other processes, see spawn.go
	// followEvery and recordEvery are the intervals of the reads of the
	// children in other processes and of their records, see tree.go.
	followEvery time.Duration
	recordEvery time.Duration
	giveUp      time.Duration // how long a child that does not answer still counts as reachable
	node        string        // the name of this process, see Config.Node
	started     time.Time     // when New built this agent, see Started

	tokMu        sync.RWMutex
	personTokens PersonTokens // the tokens of people from a linked package, see connect.go
	ready        atomic.Bool
	noResume     bool

	// rulesPolicy is true when policy comes from cfg.Policy.Rules, so
	// ProfileGrants can name its pattern rules.
	rulesPolicy bool

	profMu sync.RWMutex    // guards cfg.Agents, which is replaced, never changed in place
	fixed  map[string]bool // the profiles that the configuration defines, see ProfileLocked

	obsMu      sync.RWMutex
	observers  []Observer
	annotators []RunAnnotator

	modelsMu sync.Mutex
	models   map[string]Model
	slots    map[string]chan struct{} // the model slots of each provider with maxConcurrent

	zoneMu sync.Mutex
	zones  map[string]zoneEntry // time zones by name, see clock.go

	decisionPoints []external // policy.external and the points of linked packages, see external.go

	services   []io.Closer        // what stops the services, see service.go
	runner     Runner             // drives the runs in place of the turn loop, see runner.go
	deciders   map[string]Decider // the deciders that WithDecider gave, see decider.go
	accessMode string             // AccessModeRules, AccessModeExternal, or AccessModeCustom

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
	if cfg.Home == "" {
		cfg.Home = DefaultHome()
	}
	if cfg.Agents == nil {
		cfg.Agents = map[string]AgentProfile{}
	}
	for _, p := range o.profiles {
		cfg.Agents[p.Name] = p
	}
	// The configuration defines every profile but the ones that the loader
	// read from files under <home>/agents.
	fixed := make(map[string]bool, len(cfg.Agents))
	for name := range cfg.Agents {
		if !cfg.FileProfiles[name] {
			fixed[name] = true
		}
	}
	toolMW := o.toolMW
	if cfg.Tools.Limits.Limit != (Limit{}) || len(cfg.Tools.Limits.PerTool) > 0 {
		// The limits wrap every tool outermost, so the kernel never records a
		// result that the configuration does not allow.
		toolMW = append([]ToolMiddleware{LimitTools(cfg.Tools.Limits)}, o.toolMW...)
	}
	a := &Agent{
		cfg:        cfg,
		fixed:      fixed,
		log:        o.logger,
		modelMW:    o.modelMW,
		toolMW:     toolMW,
		policy:     o.policy,
		ledger:     o.ledger,
		bus:        o.bus,
		secrets:    o.secrets,
		compactor:  o.compactor,
		authn:      o.authn,
		authz:      o.authz,
		tlsConfig:  o.tlsConfig,
		prices:     o.prices,
		clock:      o.clock,
		observers:  o.observers,
		annotators: o.annotators,
		models:     map[string]Model{},
		slots:      modelSlots(cfg),
		noResume:   o.noResume,
		deciders:   o.deciders,
	}
	// Before the kernel starts, only resources opened by this constructor
	// belong to it. In particular, a failed setup must release the file
	// ledger's writer lock without closing a caller-supplied ledger.
	var setupClosers []io.Closer
	keepSetup := func(v any) {
		if c, ok := v.(io.Closer); ok {
			setupClosers = append(setupClosers, c)
		}
	}
	complete, kernelStarted := false, false
	defer func() {
		if complete {
			return
		}
		if kernelStarted {
			stop, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			a.shutdownKernel(stop)
			return
		}
		a.closeDecisionPoints()
		for i := len(setupClosers) - 1; i >= 0; i-- {
			if err := setupClosers[i].Close(); err != nil {
				a.log.Warn("close resource after setup failure", "error", err)
			}
		}
	}()
	a.followEvery, a.recordEvery, a.giveUp = defaultFollowEvery, defaultRecordEvery, followGiveUp
	if o.giveUp > 0 {
		a.giveUp = o.giveUp
	}
	if o.followEvery > 0 {
		a.followEvery = o.followEvery
	}
	if o.recordEvery > 0 {
		a.recordEvery = o.recordEvery
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
	if names := toolSecretNames(cfg.Tools.Bash.Env); len(names) > 0 {
		a.log.Warn("tools.bash.env gives the shell variables with a secret name; every command of the shell can read them", "names", names)
	}
	if a.toolUser, err = LookupToolUser(cfg.Tools.User); err != nil {
		return nil, err
	}
	files := o.files
	if files == nil {
		if files, err = DirFiles(cfg.WorkspaceRoot()); err != nil {
			return nil, err
		}
		if u := a.toolUser; u != nil {
			// The kernel writes as fw, so a file that it creates for a
			// tool, such as an attachment, goes to the tool user.
			files.(*dirFiles).owner = &fileOwner{uid: int(u.UID), gid: int(u.GID)}
		}
	}
	if a.files, err = newWorkspace(files, cfg); err != nil {
		return nil, err
	}
	if a.toolUser != nil {
		if err := a.useToolUser(a.toolUser); err != nil {
			return nil, err
		}
		a.log.Info("the processes of tools run as the tool user", "user", a.toolUser.String())
	}
	if a.ledger == nil {
		if a.ledger, err = OpenLedger(ctx, cfg); err != nil {
			return nil, err
		}
		keepSetup(a.ledger)
	}
	if a.bus == nil {
		if a.bus, err = OpenBus(ctx, cfg); err != nil {
			return nil, err
		}
		keepSetup(a.bus)
	}
	if a.authn == nil {
		if a.authn, err = NewAuthn(ctx, cfg, a.secrets); err != nil {
			return nil, err
		}
	}
	a.accessMode = AccessModeCustom
	if a.authz == nil && o.authzFrom != nil {
		z, mode, err := o.authzFrom(a)
		if err != nil {
			return nil, err
		}
		if z != nil {
			a.authz = z
			if mode != "" {
				a.accessMode = mode
			}
		}
	}
	if err := a.setupExternal(ctx, a.policy != nil); err != nil {
		return nil, err
	}
	if a.authz == nil {
		a.authz = NewRulesAuthz(cfg.Access, a.ProfileNamed)
		a.accessMode = AccessModeRules
	}
	if a.tlsConfig == nil && cfg.TLS.Provider != "" {
		if a.tlsConfig, err = NewTLSConfig(ctx, cfg, a.secrets); err != nil {
			return nil, err
		}
	}
	if o.model != nil {
		a.model = a.wrapModel(limitModel(a.slots, cfg.Model, o.model))
	} else if cfg.Model == "" {
		// No default model. The process serves, and a run request fails with
		// ErrNoModel unless its profile names a model.
		a.log.Info("no model configured; a run needs model in the configuration or on its profile")
	} else {
		// Open the default model now when possible. A missing credential is a
		// warning, not a failure, so that a process boots and serves health and
		// metadata before the credential arrives; the first run reports the error.
		m, err := NewModel(ctx, cfg.Model, cfg, a.secrets)
		if err != nil {
			a.log.Warn("default model not available yet", "model", cfg.Model, "error", err)
		} else {
			a.model = a.wrapModel(limitModel(a.slots, cfg.Model, m))
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
			if t == nil {
				// The configuration turns the tool off, as no decider
				// turns off decide.
				continue
			}
			t = a.wrapTool(t)
			a.tools = append(a.tools, t)
			keepSetup(t)
		}
		srcs, err := NewToolSources(ctx, cfg, a.secrets)
		if err != nil {
			return nil, err
		}
		a.sources = append(a.sources, srcs...)
		for _, source := range srcs {
			keepSetup(source)
		}
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
	a.started = time.Now()
	a.node = strings.TrimSpace(cfg.Node)
	if a.node == "" {
		a.node = DefaultNode(cfg.Listen)
	}
	a.spawner = o.spawner
	if a.spawner == nil && !o.noDefaults {
		if a.spawner, err = newSpawner(ctx, cfg); err != nil {
			return nil, err
		}
	}
	// The runner exists before the kernel resumes the runs of the ledger,
	// so a resumed run goes to it.
	a.runner = o.runner
	if a.runner == nil && !o.noDefaults {
		if a.runner, err = newRunner(ctx, a); err != nil {
			return nil, err
		}
	}
	for name, p := range cfg.Agents {
		if err := a.checkHosted(p); err != nil {
			return nil, fmt.Errorf("fw: agents.%s: %w", name, err)
		}
	}
	if err := a.startKernel(ctx); err != nil {
		return nil, err
	}
	kernelStarted = true
	if err := a.startServices(ctx, &o); err != nil {
		return nil, err
	}
	complete = true
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

// ToolUser returns the user that runs the processes of tools (tools.user,
// section 7.12), or nil when they run as the user of fw.
func (a *Agent) ToolUser() *ToolUser { return a.toolUser }

// Node returns the name of this process among the processes of a fleet
// (Config.Node).
func (a *Agent) Node() string { return a.node }

// DefaultNode returns the default name of a process outside ax:
// "<host>:<port>" from the host name and the port of listen, or the host
// name alone when listen names no port.
func DefaultNode(listen string) string {
	host, _ := os.Hostname()
	if host == "" {
		host = "fw"
	}
	if _, port, err := net.SplitHostPort(strings.TrimSpace(listen)); err == nil && port != "" {
		return host + ":" + port
	}
	return host
}

// Logger returns the logger.
func (a *Agent) Logger() *slog.Logger { return a.log }

// Started returns when New built this agent. The status body uses it as
// the start of an idle period when no run ever worked.
func (a *Agent) Started() time.Time { return a.started }

// Model returns the default model, or nil when the configuration names none.
func (a *Agent) Model() Model { return a.model }

// ModelFor returns the model for a selector or a name from Config.Models,
// opening and caching it on first use.
func (a *Agent) ModelFor(ctx context.Context, name string) (Model, error) {
	return a.openModel(ctx, a.cfg.ModelSelector(name))
}

// openModel returns the model for a selector as it stands, opening and
// caching it on first use. A run opens the selector of its resolved
// profile this way, so a name in Config.Models never maps it to another
// model.
func (a *Agent) openModel(ctx context.Context, selector string) (Model, error) {
	a.modelsMu.Lock()
	defer a.modelsMu.Unlock()
	if m, ok := a.models[selector]; ok {
		return m, nil
	}
	if selector == "" {
		return nil, ErrNoModel
	}
	m, err := NewModel(ctx, selector, a.cfg, a.secrets)
	if err != nil {
		return nil, err
	}
	m = a.wrapModel(limitModel(a.slots, selector, m))
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
func (a *Agent) Ready() bool { return a.ready.Load() && a.RuntimeError() == nil }

// Handler builds the shared mux with every surface mounted.
func (a *Agent) Handler() (http.Handler, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if a.RuntimeError() != nil {
			http.Error(w, "storage failed; execution blocked until restart", http.StatusServiceUnavailable)
			return
		}
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
	// The services go first, because a last message of one needs the bus.
	a.closeServices()
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

// Hold marks the process as busy with background work, such as the install
// of an environment, until the returned function is called. The status of
// the process is not idle while a hold is open, so a controller does not
// put the process to sleep in the middle of the work (StatusOf in
// surface/view). The function can be called more than once.
func (a *Agent) Hold() (release func()) {
	a.holds.Add(1)
	var once sync.Once
	return func() { once.Do(func() { a.holds.Add(-1) }) }
}

// Holds counts the open holds of background work.
func (a *Agent) Holds() int { return int(a.holds.Load()) }
