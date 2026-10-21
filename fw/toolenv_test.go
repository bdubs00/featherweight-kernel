package fw

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// envNames returns the names of an environment as NAME=value pairs.
func envNames(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		out = append(out, name)
	}
	return out
}

// envValue returns the value of the last entry of name, as exec takes it.
func envValue(env []string, name string) (string, bool) {
	v, ok := "", false
	for _, kv := range env {
		if n, val, _ := strings.Cut(kv, "="); n == name {
			v, ok = val, true
		}
	}
	return v, ok
}

func TestIsSecretName(t *testing.T) {
	for name, want := range map[string]bool{
		"GITHUB_TOKEN":      true,
		"OPENAI_API_KEY":    true,
		"DB_PASSWORD":       true,
		"db_pass":           true,
		"GH_PAT":            true,
		"AWS_CREDENTIALS":   true,
		"FW_API_TOKEN_FILE": false,
		"TOKEN_URL":         false,
		"KEYBOARD":          false,
		"PATH":              false,
		"MONKEY":            false,
	} {
		if got := IsSecretName(name); got != want {
			t.Errorf("IsSecretName(%q) = %v, want %v", name, got, want)
		}
	}
}

// A tool process gets no manifest of ax, no credential that the
// configuration names, no variable with a secret name, and no variable
// whose value holds a secret.
func TestToolEnvWithoutAgent(t *testing.T) {
	t.Setenv(HideEnv, "EXTRA_HIDDEN, ALSO_HIDDEN")
	cfg := DefaultConfig()
	cfg.Auth.TokenEnv = "FLEET_CRED"
	cfg.Providers = map[string]ProviderConfig{"openai": {APIKeyEnv: "LOCAL_LLM"}}
	cfg.Tools.MCP = []MCPServerConfig{{Name: "git", Auth: MCPAuthConfig{Bearer: "GIT_BEARER", OAuth: &OAuthClientConfig{ClientSecret: "GIT_CLIENT"}}}}
	cfg.Tools.MCP = append(cfg.Tools.MCP, MCPServerConfig{Name: "plain"})
	inherited := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/root",
		"AX_TASK_YAML=spec:\n  env:\n  - name: FW_API_TOKEN\n    value: fleet-token-123456",
		"AX_WORKSPACES_YAML=spec:\n  git:\n  - repo: https://x:ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa@github.com/o/r",
		"FLEET_CRED=fleet-token-123456",
		"FLEET_CRED_FILE=/run/secrets/fleet",
		"FW_API_TOKEN=other-token-123456",
		"LOCAL_LLM=llm-key-1234567",
		"GIT_BEARER=bearer-1234567",
		"GIT_CLIENT=client-1234567",
		"GITHUB_TOKEN=ghp_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"DB_PASSWORD=hunter2hunter2",
		"MIRROR=hunter2hunter2",
		"SESSION=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJhbGljZSJ9.c2lnbmF0dXJlLXZhbHVl",
		"PROXY=https://user:pw@proxy.example.com",
		"EXTRA_HIDDEN=plain",
		"ALSO_HIDDEN=plain",
		"FW_TOOLS_BASH_ENV_NATS_URL=nats://nats:4222",
		"KEEP=yes",
		HideEnv + "=EXTRA_HIDDEN, ALSO_HIDDEN",
	}
	got := ToolEnv(context.Background(), cfg, inherited, nil)
	want := []string{"PATH", "HOME", "FW_TOOLS_BASH_ENV_NATS_URL", "KEEP"}
	if names := envNames(got); !slices.Equal(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	for _, kv := range got {
		if strings.Contains(kv, "token-123456") || strings.Contains(kv, "hunter2") {
			t.Fatalf("a secret reached the tool: %q", kv)
		}
	}
}

// A configured variable wins over an inherited one and can have a secret
// name, but never a hidden name or a value that fw resolved through
// Secrets.
func TestToolEnvConfigured(t *testing.T) {
	a := newTestAgent(t, newScript().Model(), WithSecrets(SecretsFunc(func(_ context.Context, name string) (string, error) {
		return "resolved-" + strings.ToLower(name) + "-12345", nil
	})))
	ctx := WithAgent(context.Background(), a)
	if _, err := a.secrets.Get(ctx, "SVC_CRED"); err != nil {
		t.Fatal(err)
	}
	inherited := []string{"KEEP=process", "MIRROR=resolved-svc_cred-12345", "OVERRIDE=process"}
	configured := []string{
		"OVERRIDE=config",
		"NPM_TOKEN=npm-operator-choice",
		"SMUGGLED=resolved-svc_cred-12345",
		"FW_API_TOKEN=leak-leak-leak",
		"ANTHROPIC_API_KEY=leak-leak-leak",
		"AX_TASK_YAML=leak",
		"broken",
	}
	got := ToolEnv(ctx, nil, inherited, configured)
	if names := envNames(got); !slices.Equal(names, []string{"KEEP", "OVERRIDE", "NPM_TOKEN"}) {
		t.Fatalf("names = %v", names)
	}
	if v, _ := envValue(got, "OVERRIDE"); v != "config" {
		t.Errorf("OVERRIDE = %q, want the configured value", v)
	}
	if v, _ := envValue(got, "NPM_TOKEN"); v != "npm-operator-choice" {
		t.Errorf("NPM_TOKEN = %q, want the configured value", v)
	}
}

func TestToolSecretNames(t *testing.T) {
	got := toolSecretNames(map[string]string{"NATS_URL": "x", "NPM_TOKEN": "y", "DB_PASS": "z"})
	if !slices.Equal(got, []string{"DB_PASS", "NPM_TOKEN"}) {
		t.Fatalf("got %v", got)
	}
}

// withToolEnvHook registers h for the length of a test.
func withToolEnvHook(t *testing.T, h ToolEnvHook) {
	t.Helper()
	toolEnvHooks.mu.Lock()
	saved := slices.Clone(toolEnvHooks.list)
	toolEnvHooks.mu.Unlock()
	RegisterToolEnv(h)
	t.Cleanup(func() {
		toolEnvHooks.mu.Lock()
		toolEnvHooks.list = saved
		toolEnvHooks.mu.Unlock()
	})
}

// The hooks change the environment in the order of registration, and the
// first error stops ExtendToolEnv.
func TestExtendToolEnv(t *testing.T) {
	cfg := DefaultConfig()
	withToolEnvHook(t, func(_ context.Context, c *Config, env []string) ([]string, error) {
		if c != cfg {
			return env, nil
		}
		return append(env, "STEP=one"), nil
	})
	withToolEnvHook(t, func(ctx context.Context, c *Config, env []string) ([]string, error) {
		if c != cfg {
			return env, nil
		}
		if RunInfoFrom(ctx).RunID == "broken" {
			return nil, errors.New("env: the environment failed")
		}
		return append(env, "STEP=two"), nil
	})
	env, err := ExtendToolEnv(context.Background(), cfg, []string{"PATH=/bin"})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := envValue(env, "STEP"); v != "two" || !slices.Contains(env, "STEP=one") || !slices.Contains(env, "PATH=/bin") {
		t.Fatalf("env = %v", env)
	}
	ctx := WithRunInfo(context.Background(), RunInfo{RunID: "broken"})
	if _, err := ExtendToolEnv(ctx, cfg, nil); err == nil || !strings.Contains(err.Error(), "environment failed") {
		t.Fatalf("err = %v", err)
	}
	// Another configuration gets the environment as it is.
	env, err = ExtendToolEnv(ctx, DefaultConfig(), []string{"A=1"})
	if err != nil || !slices.Equal(env, []string{"A=1"}) {
		t.Fatalf("env = %v, err = %v", env, err)
	}
}
