package fw

import (
	"context"
	"os"
	"slices"
	"sort"
	"strings"
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
