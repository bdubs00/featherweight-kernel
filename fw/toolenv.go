package fw

import (
	"context"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
)

// HideEnv is the variable that names more variables, with commas between
// them, that a process of a tool never gets (section 7.2).
const HideEnv = "FW_BASH_HIDE"

// axManifestEnv names the variables in which ax gives the runner the whole
// Task and every Workspace (section 11.1). The Task holds the values of
// spec.env, such as the API token, and a Workspace can hold a token in the
// URL of a repository.
var axManifestEnv = []string{"AX_TASK_YAML", "AX_WORKSPACES_YAML"}

// modelKeyEnv names the variables of the model keys that the adapters read
// when a provider names no apiKeyEnv.
var modelKeyEnv = []string{"ANTHROPIC_API_KEY", "GEMINI_API_KEY", "OPENAI_API_KEY"}

// IsSecretName reports whether the name of a variable says that it holds a
// secret, by the rule of the redactor (section 4.2): a part such as KEY,
// TOKEN, SECRET, PASSWORD, or PAT, as in GITHUB_TOKEN or DB_PASSWORD. A
// name that ends in a word such as _FILE, _PATH, or _URL names where a
// secret is, and is not one.
func IsSecretName(name string) bool { return isSecretName(name) }

// ToolEnv returns the environment of a process that a tool starts, such as
// the shell of bash or a stdio MCP server (section 7.2). From inherited,
// normally os.Environ(), it leaves out:
//
//   - AX_TASK_YAML and AX_WORKSPACES_YAML, the manifests of ax;
//   - the credentials of fw that the configuration names: auth.tokenEnv,
//     tokens.credential, the apiKeyEnv of each provider and the keys of the
//     model adapters, the bearer and the client secret of each MCP server,
//     and the client secret of auth.oidc, each also with the suffix _FILE;
//   - the names in FW_BASH_HIDE, and FW_BASH_HIDE itself;
//   - every variable with a secret name (IsSecretName);
//   - every variable whose value holds a secret by the rules of the
//     redactor: a value that fw resolved through Secrets, the value of a
//     variable with a secret name, or text with the shape of a key.
//
// Then it adds configured, the variables as NAME=value that the
// configuration gives the tool on purpose, such as tools.bash.env. A
// configured name wins over the same inherited name. A configured variable
// can have a secret name, because an operator named it, but it never has a
// hidden name or a value that fw resolved through Secrets, so a
// configuration cannot pass a credential of fw to a tool under another
// name. The redactor still replaces its value in every event.
//
// The agent in ctx knows the values that fw resolved. Without an agent,
// ToolEnv knows the secrets of inherited alone. A nil cfg takes the
// configuration of the agent in ctx, or none.
func ToolEnv(ctx context.Context, cfg *Config, inherited, configured []string) []string {
	a := AgentFrom(ctx)
	if cfg == nil && a != nil {
		cfg = a.cfg
	}
	var r *redactor
	if a != nil {
		r = a.redact
	} else {
		r = newRedactor(inherited)
	}
	hide := toolHidden(cfg)
	set := map[string]bool{}
	var keep []string
	for _, kv := range configured {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || name == "" || hide[name] || r.hasResolved(value) {
			continue
		}
		set[name] = true
		keep = append(keep, kv)
	}
	out := make([]string, 0, len(inherited)+len(keep))
	for _, kv := range inherited {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || name == "" || set[name] || hide[name] || isSecretName(name) || r.hasSecret(value) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, keep...)
}

// toolHidden returns the names that a process of a tool never gets,
// whatever their value (ToolEnv).
func toolHidden(cfg *Config) map[string]bool {
	names := slices.Concat(axManifestEnv, modelKeyEnv, []string{HideEnv, "FW_API_TOKEN"})
	for _, n := range strings.Split(os.Getenv(HideEnv), ",") {
		names = append(names, strings.TrimSpace(n))
	}
	if cfg != nil {
		names = append(names, cfg.Auth.TokenEnv, cfg.Tokens.Credential)
		if o := cfg.Auth.OIDC; o != nil {
			names = append(names, o.ClientSecret)
		}
		for _, p := range cfg.Providers {
			names = append(names, p.APIKeyEnv)
		}
		for _, s := range cfg.Tools.MCP {
			names = append(names, s.Auth.Bearer)
			if o := s.Auth.OAuth; o != nil {
				names = append(names, o.ClientSecret)
			}
		}
	}
	hide := make(map[string]bool, 2*len(names))
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			hide[n] = true
			hide[n+"_FILE"] = true
		}
	}
	return hide
}

// toolSecretNames returns the names of tools.bash.env that IsSecretName
// matches, sorted, so the agent can say at start that the shell gets them.
func toolSecretNames(env map[string]string) []string {
	var out []string
	for name := range env {
		if isSecretName(name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// A ToolEnvHook changes the environment of a tool process for a linked
// package, such as the package environment of section 18. It gets the
// environment that the tool built, normally from ToolEnv, and returns it
// with its changes, such as a directory in front of PATH. A hook can wait,
// bounded by ctx, for example until the packages are installed. An error
// keeps the tool process from starting, and the tool returns it as its
// result. A hook must not add a secret. The context carries the agent
// (AgentFrom) and the run (RunInfoFrom) when a run starts the process. A
// nil cfg means a tool without a configuration.
type ToolEnvHook func(ctx context.Context, cfg *Config, env []string) ([]string, error)

var toolEnvHooks struct {
	mu   sync.RWMutex
	list []ToolEnvHook
}

// RegisterToolEnv adds a hook that ExtendToolEnv applies to the
// environment of every tool process. A package calls it in init.
func RegisterToolEnv(h ToolEnvHook) {
	toolEnvHooks.mu.Lock()
	defer toolEnvHooks.mu.Unlock()
	toolEnvHooks.list = append(toolEnvHooks.list, h)
}

// ExtendToolEnv applies the registered hooks to env, in the order of
// registration, and returns the result. A tool that starts a process, such
// as the shell of bash or a stdio MCP server, calls it after ToolEnv. The
// first hook that fails stops it with its error.
func ExtendToolEnv(ctx context.Context, cfg *Config, env []string) ([]string, error) {
	toolEnvHooks.mu.RLock()
	hooks := slices.Clone(toolEnvHooks.list)
	toolEnvHooks.mu.RUnlock()
	for _, h := range hooks {
		next, err := h(ctx, cfg, env)
		if err != nil {
			return nil, err
		}
		env = next
	}
	return env, nil
}

// toolEnvNoWaitKey marks a context whose tool environment hooks must not
// wait for anything.
type toolEnvNoWaitKey struct{}

// WithToolEnvNoWait marks ctx so that a tool environment hook applies its
// variables and never waits, for a process that starts outside a tool
// call of a run, such as an MCP server that the tool list starts.
func WithToolEnvNoWait(ctx context.Context) context.Context {
	return context.WithValue(ctx, toolEnvNoWaitKey{}, true)
}

// ToolEnvNoWait reports whether ctx carries WithToolEnvNoWait.
func ToolEnvNoWait(ctx context.Context) bool {
	v, _ := ctx.Value(toolEnvNoWaitKey{}).(bool)
	return v
}
