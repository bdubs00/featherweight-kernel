package fw

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
)

// Constructors that drivers register at init.
type (
	ModelCtor      func(ctx context.Context, model string, cfg *Config, secrets Secrets) (Model, error)
	BusCtor        func(ctx context.Context, cfg *Config) (Bus, error)
	LedgerCtor     func(ctx context.Context, cfg *Config) (Ledger, error)
	SecretsCtor    func(ctx context.Context, cfg *Config) (Secrets, error)
	SurfaceCtor    func(cfg *Config) (Surface, error)
	ToolCtor       func(cfg *Config) (Tool, error)
	ToolSourceCtor func(ctx context.Context, cfg *Config, secrets Secrets) ([]ToolSource, error)
	AuthnCtor      func(ctx context.Context, cfg *Config, secrets Secrets) (Authn, error)
	TLSCtor        func(ctx context.Context, cfg *Config, secrets Secrets) (*tls.Config, error)
)

var registry = struct {
	mu          sync.RWMutex
	models      map[string]ModelCtor
	buses       map[string]BusCtor
	ledgers     map[string]LedgerCtor
	secrets     map[string]SecretsCtor
	surfaces    map[string]SurfaceCtor
	tools       map[string]ToolCtor
	toolSources map[string]ToolSourceCtor
	authns      map[string]AuthnCtor
	tlsProvs    map[string]TLSCtor
	defaults    []func(*Config)
}{
	models:      map[string]ModelCtor{},
	buses:       map[string]BusCtor{},
	ledgers:     map[string]LedgerCtor{},
	secrets:     map[string]SecretsCtor{},
	surfaces:    map[string]SurfaceCtor{},
	tools:       map[string]ToolCtor{},
	toolSources: map[string]ToolSourceCtor{},
	authns:      map[string]AuthnCtor{},
	tlsProvs:    map[string]TLSCtor{},
}

// RegisterModel registers a model provider, for example "anthropic".
func RegisterModel(provider string, c ModelCtor) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.models[provider] = c
}

// RegisterBus registers a bus driver, for example "nats".
func RegisterBus(name string, c BusCtor) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.buses[name] = c
}

// RegisterLedger registers a ledger driver, for example "file".
func RegisterLedger(name string, c LedgerCtor) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.ledgers[name] = c
}

// RegisterSecrets registers a secrets driver, for example "env".
func RegisterSecrets(name string, c SecretsCtor) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.secrets[name] = c
}

// RegisterSurface registers a surface by name, for example "native".
func RegisterSurface(name string, c SurfaceCtor) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.surfaces[name] = c
}

// RegisterDefaults registers a change to the default configuration. A
// package that a program links in calls it in init, for example to turn on
// its surface or its tool by default. DefaultConfig applies the changes
// after the kernel defaults, in the order of registration. A file or a
// variable still overrides the result.
func RegisterDefaults(fn func(*Config)) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.defaults = append(registry.defaults, fn)
}

// RegisterTool registers a built-in tool by name, for example "bash".
func RegisterTool(name string, c ToolCtor) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.tools[name] = c
}

// RegisterToolSource registers a tool source provider, for example "mcp"
// (reads cfg.Tools.MCP) or "a2a" (reads cfg.Tools.Peers).
func RegisterToolSource(name string, c ToolSourceCtor) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.toolSources[name] = c
}

// RegisterAuthn registers an authentication mode, for example "oauth".
func RegisterAuthn(mode string, c AuthnCtor) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.authns[mode] = c
}

// RegisterTLSProvider registers a builder of the listener TLS configuration
// by name, for example "spiffe". The name "file" is built in.
func RegisterTLSProvider(name string, c TLSCtor) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.tlsProvs[name] = c
}

// RegisteredTLSProviders lists the registered TLS providers, without the
// built-in "file".
func RegisteredTLSProviders() []string {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	out := make([]string, 0, len(registry.tlsProvs))
	for n := range registry.tlsProvs {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// NewTLSConfig builds the listener TLS configuration that cfg.TLS names.
// An empty provider returns nil, which means plain HTTP.
func NewTLSConfig(ctx context.Context, cfg *Config, secrets Secrets) (*tls.Config, error) {
	name := strings.ToLower(strings.TrimSpace(cfg.TLS.Provider))
	switch name {
	case "":
		return nil, nil
	case "file":
		return fileTLSConfig(cfg.TLS)
	}
	registry.mu.RLock()
	c, ok := registry.tlsProvs[name]
	registry.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("fw: no TLS provider %q registered", name)
	}
	tc, err := c(ctx, cfg, secrets)
	if err != nil {
		return nil, err
	}
	return tc, nil
}

// fileTLSConfig builds the "file" provider: a certificate and a key from
// files, with optional client authentication against a CA bundle.
func fileTLSConfig(c ListenTLSConfig) (*tls.Config, error) {
	if c.Cert == "" || c.Key == "" {
		return nil, fmt.Errorf("fw: tls.provider file needs tls.cert and tls.key")
	}
	cert, err := tls.LoadX509KeyPair(c.Cert, c.Key)
	if err != nil {
		return nil, fmt.Errorf("fw: tls: load certificate: %w", err)
	}
	tc := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	if c.ClientCA != "" {
		pem, err := os.ReadFile(c.ClientCA)
		if err != nil {
			return nil, fmt.Errorf("fw: tls: read clientCA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("fw: tls: clientCA %s holds no certificate", c.ClientCA)
		}
		tc.ClientCAs = pool
	}
	switch strings.ToLower(strings.TrimSpace(c.ClientAuth)) {
	case "", "none":
		tc.ClientAuth = tls.NoClientCert
	case "request":
		tc.ClientAuth = tls.VerifyClientCertIfGiven
	case "require":
		tc.ClientAuth = tls.RequireAndVerifyClientCert
	default:
		return nil, fmt.Errorf("fw: tls.clientAuth %q is not none, request, or require", c.ClientAuth)
	}
	return tc, nil
}

// NewModel opens a model from a "provider/model" selector.
func NewModel(ctx context.Context, selector string, cfg *Config, secrets Secrets) (Model, error) {
	provider, model := SplitSelector(selector)
	registry.mu.RLock()
	c, ok := registry.models[provider]
	registry.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("fw: no model provider %q registered (known: %v)", provider, RegisteredModels())
	}
	return c(ctx, model, cfg, secrets)
}

// OpenBus opens the bus driver named in cfg.
func OpenBus(ctx context.Context, cfg *Config) (Bus, error) {
	registry.mu.RLock()
	c, ok := registry.buses[cfg.Bus.Driver]
	registry.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("fw: no bus driver %q registered", cfg.Bus.Driver)
	}
	return c(ctx, cfg)
}

// OpenLedger opens the ledger driver named in cfg.
func OpenLedger(ctx context.Context, cfg *Config) (Ledger, error) {
	registry.mu.RLock()
	c, ok := registry.ledgers[cfg.Ledger.Driver]
	registry.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("fw: no ledger driver %q registered", cfg.Ledger.Driver)
	}
	return c(ctx, cfg)
}

// OpenSecrets opens the secrets driver named in cfg.
func OpenSecrets(ctx context.Context, cfg *Config) (Secrets, error) {
	registry.mu.RLock()
	c, ok := registry.secrets[cfg.Secrets.Driver]
	registry.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("fw: no secrets driver %q registered", cfg.Secrets.Driver)
	}
	return c(ctx, cfg)
}

// NewSurface builds a registered surface by name.
func NewSurface(name string, cfg *Config) (Surface, error) {
	registry.mu.RLock()
	c, ok := registry.surfaces[name]
	registry.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("fw: no surface %q registered", name)
	}
	return c(cfg)
}

// NewBuiltinTool builds a registered tool by name.
func NewBuiltinTool(name string, cfg *Config) (Tool, error) {
	registry.mu.RLock()
	c, ok := registry.tools[name]
	registry.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("fw: no built-in tool %q registered", name)
	}
	return c(cfg)
}

// NewToolSources builds every registered tool source provider.
func NewToolSources(ctx context.Context, cfg *Config, secrets Secrets) ([]ToolSource, error) {
	registry.mu.RLock()
	names := make([]string, 0, len(registry.toolSources))
	for n := range registry.toolSources {
		names = append(names, n)
	}
	sort.Strings(names)
	ctors := make([]ToolSourceCtor, 0, len(names))
	for _, n := range names {
		ctors = append(ctors, registry.toolSources[n])
	}
	registry.mu.RUnlock()
	var out []ToolSource
	for i, c := range ctors {
		s, err := c(ctx, cfg, secrets)
		if err != nil {
			return nil, fmt.Errorf("fw: tool source %s: %w", names[i], err)
		}
		out = append(out, s...)
	}
	return out, nil
}

// NewAuthn builds the authentication of cfg: one mode, or a ChainAuthn of
// the modes that auth.mode lists, in order.
func NewAuthn(ctx context.Context, cfg *Config, secrets Secrets) (Authn, error) {
	modes := cfg.Auth.Modes()
	if len(modes) == 1 {
		return newAuthnMode(ctx, modes[0], cfg, secrets)
	}
	chain := make(ChainAuthn, 0, len(modes))
	for _, m := range modes {
		a, err := newAuthnMode(ctx, m, cfg, secrets)
		if err != nil {
			return nil, err
		}
		chain = append(chain, a)
	}
	return chain, nil
}

func newAuthnMode(ctx context.Context, mode string, cfg *Config, secrets Secrets) (Authn, error) {
	switch mode {
	case "", "token":
		env := cfg.Auth.TokenEnv
		if env == "" {
			env = "FW_API_TOKEN"
		}
		// A secret from a file often ends in a newline, which no client sends.
		tok, err := secrets.Get(ctx, env)
		tok = strings.TrimSpace(tok)
		if err != nil || tok == "" {
			return nil, fmt.Errorf("fw: auth.mode is token but %s is not set; set it or set auth.mode: none", env)
		}
		return TokenAuthn{Token: tok}, nil
	case "none":
		return NoAuthn{}, nil
	}
	registry.mu.RLock()
	c, ok := registry.authns[mode]
	registry.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("fw: no auth mode %q registered (known: token, none, %s)", mode, strings.Join(RegisteredAuthns(), ", "))
	}
	return c(ctx, cfg, secrets)
}

// RegisteredAuthns lists the registered authentication modes.
func RegisteredAuthns() []string {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	out := make([]string, 0, len(registry.authns))
	for n := range registry.authns {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// RegisteredModels lists the model providers.
func RegisteredModels() []string {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	out := make([]string, 0, len(registry.models))
	for n := range registry.models {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// RegisteredTools lists the built-in tool names.
func RegisteredTools() []string {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	out := make([]string, 0, len(registry.tools))
	for n := range registry.tools {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// RegisteredSurfaces lists the surface names.
func RegisteredSurfaces() []string {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	out := make([]string, 0, len(registry.surfaces))
	for n := range registry.surfaces {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
