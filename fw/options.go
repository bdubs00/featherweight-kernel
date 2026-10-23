package fw

import (
	"crypto/tls"
	"log/slog"
	"time"
)

// Option configures New.
type Option func(*options)

type options struct {
	runner     Runner
	cfg        *Config
	configPath string
	model      Model
	tools      []Tool
	sources    []ToolSource
	policy     Policy
	ledger     Ledger
	bus        Bus
	secrets    Secrets
	compactor  Compactor
	surfaces   []Surface
	observers  []Observer
	modelMW    []ModelMiddleware
	toolMW     []ToolMiddleware
	profiles   []AgentProfile
	authn      Authn
	authz      Authz
	authzFrom  func(*Agent) (Authz, string, error)
	tlsConfig  *tls.Config
	prices     Prices
	logger     *slog.Logger
	clock      Clock
	files      Files
	noDefaults bool
	noResume   bool
	annotators []RunAnnotator
	services   []Service
	noServices bool
	spawner    Spawner
	deciders   map[string]Decider
	// followEvery and recordEvery replace the intervals of tree.go; only a
	// test of this package sets them.
	followEvery time.Duration
	recordEvery time.Duration
	giveUp      time.Duration
}

// WithServices adds services to the registered ones (see Service).
// WithSpawner runs the children of spawn_task through s, in place of the
// driver that spawn.driver names or the children in this process.
func WithSpawner(s Spawner) Option { return func(o *options) { o.spawner = s } }

// WithDecider answers the calls to the decider name with d, in place of
// the client that the driver of deciders.<name> builds (see
// Agent.DeciderNamed). A
// test gives a fake decider this way. A routine still names a key of
// deciders, so the configuration lists the name too.
func WithDecider(name string, d Decider) Option {
	return func(o *options) {
		if o.deciders == nil {
			o.deciders = map[string]Decider{}
		}
		o.deciders[name] = d
	}
}

// WithRunner drives the runs of the agent through r, in place of the
// registered runners and of the turn loop (see Runner).
func WithRunner(r Runner) Option { return func(o *options) { o.runner = r } }

func WithServices(s ...Service) Option {
	return func(o *options) { o.services = append(o.services, s...) }
}

// WithoutServices starts no service, registered or given. A verb that runs
// one piece of work and exits uses it, so the process never joins a fleet.
func WithoutServices() Option { return func(o *options) { o.noServices = true } }

// WithRunAnnotators adds annotators to the registered ones (see
// RunAnnotator).
func WithRunAnnotators(fns ...RunAnnotator) Option {
	return func(o *options) { o.annotators = append(o.annotators, fns...) }
}

// WithFiles uses another store as the workspace in place of the directory
// tools.files.root, such as a store in memory. The kernel applies the rules
// of tools.files to it and records each change (section 7.9).
func WithFiles(f Files) Option { return func(o *options) { o.files = f } }

// WithConfig uses a resolved configuration.
func WithConfig(c *Config) Option { return func(o *options) { o.cfg = c } }

// WithConfigFile loads fw.yaml from a path. The loader is registered by the
// config package; see LoadConfig.
func WithConfigFile(path string) Option { return func(o *options) { o.configPath = path } }

// WithModelName selects the model by "provider/model".
func WithModelName(selector string) Option {
	return func(o *options) {
		if o.cfg == nil {
			o.cfg = DefaultConfig()
		}
		o.cfg.Model = selector
	}
}

// WithModel uses a model implementation.
func WithModel(m Model) Option { return func(o *options) { o.model = m } }

// WithTools adds tools.
func WithTools(t ...Tool) Option { return func(o *options) { o.tools = append(o.tools, t...) } }

// WithToolSources adds tool sources.
func WithToolSources(s ...ToolSource) Option {
	return func(o *options) { o.sources = append(o.sources, s...) }
}

// WithPolicy uses a policy.
func WithPolicy(p Policy) Option { return func(o *options) { o.policy = p } }

// WithLedger uses a ledger.
func WithLedger(l Ledger) Option { return func(o *options) { o.ledger = l } }

// WithBus uses a bus.
func WithBus(b Bus) Option { return func(o *options) { o.bus = b } }

// WithSecrets uses a secrets resolver.
func WithSecrets(s Secrets) Option { return func(o *options) { o.secrets = s } }

// WithCompactor uses a compactor.
func WithCompactor(c Compactor) Option { return func(o *options) { o.compactor = c } }

// WithSurfaces adds surfaces beyond those named in the configuration.
func WithSurfaces(s ...Surface) Option {
	return func(o *options) { o.surfaces = append(o.surfaces, s...) }
}

// WithObservers adds ledger observers.
func WithObservers(obs ...Observer) Option {
	return func(o *options) { o.observers = append(o.observers, obs...) }
}

// WithModelMiddleware wraps the model, outermost first.
func WithModelMiddleware(m ...ModelMiddleware) Option {
	return func(o *options) { o.modelMW = append(o.modelMW, m...) }
}

// WithToolMiddleware wraps every tool, outermost first.
func WithToolMiddleware(m ...ToolMiddleware) Option {
	return func(o *options) { o.toolMW = append(o.toolMW, m...) }
}

// WithProfile adds or replaces a profile.
func WithProfile(p AgentProfile) Option {
	return func(o *options) { o.profiles = append(o.profiles, p) }
}

// WithBudget sets the default budget.
func WithBudget(b Budget) Option {
	return func(o *options) {
		if o.cfg == nil {
			o.cfg = DefaultConfig()
		}
		o.cfg.Budget = b
	}
}

// WithListen sets the listen address.
func WithListen(addr string) Option {
	return func(o *options) {
		if o.cfg == nil {
			o.cfg = DefaultConfig()
		}
		o.cfg.Listen = addr
	}
}

// WithAuthn uses an authenticator.
func WithAuthn(a Authn) Option { return func(o *options) { o.authn = a } }

// WithAuthz uses an authorization plug in place of the rules from access.
func WithAuthz(z Authz) Option { return func(o *options) { o.authz = z } }

// WithAuthzFrom builds the authorization plug from the agent, once the
// agent has its configuration, its secrets, and its bus, for a plug that
// needs them, such as the control-plane authorization of the workbench. The
// function returns the plug and the mode that AccessMode reports,
// AccessModeExternal or "" for AccessModeCustom; a nil plug keeps the rules
// from access. WithAuthz wins over it.
func WithAuthzFrom(build func(a *Agent) (Authz, string, error)) Option {
	return func(o *options) { o.authzFrom = build }
}

// WithTLS serves the listener with this TLS configuration, in place of
// the one that Config.TLS builds.
func WithTLS(c *tls.Config) Option { return func(o *options) { o.tlsConfig = c } }

// WithPrices uses a price table.
func WithPrices(p Prices) Option { return func(o *options) { o.prices = p } }

// WithLogger uses a logger.
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// WithoutDefaults skips the built-in tools and the registered tool sources
// and surfaces, so only what the options give is used.
func WithoutDefaults() Option { return func(o *options) { o.noDefaults = true } }

// WithoutResume skips resuming unfinished runs at start. One-shot commands use
// it so that they do not drive the runs of a server that shares the ledger.
func WithoutResume() Option { return func(o *options) { o.noResume = true } }

// ConfigLoader loads a configuration file and the FW_* variables. The config
// package sets it. An empty path loads the defaults and the variables only.
var ConfigLoader func(path string) (*Config, error)

// ConfigFinder returns the path of the configuration file to load when no
// path is given, or "". The config package sets it.
var ConfigFinder func() string
