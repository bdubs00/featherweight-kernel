package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/ax"
	"github.com/bdubs00/featherweight-kernel/fw"
)

// cleanEnv unsets every configuration variable for the test, so the
// environment of the developer does not leak in. An empty value is unset.
func cleanEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, EnvPrefix+"_") {
			t.Setenv(name, "")
		}
	}
	for _, n := range EnvNames() {
		if !strings.Contains(n, namePlaceholder) {
			t.Setenv(n, "")
		}
	}
	for _, alias := range envAliases {
		t.Setenv(alias, "")
	}
	t.Setenv("FW_CONFIG", "")
	t.Setenv("FW_HOME", t.TempDir())
}

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigLoaderIsSet(t *testing.T) {
	if fw.ConfigLoader == nil {
		t.Fatal("fw.ConfigLoader is nil after importing config")
	}
}

func TestEnvOverrides(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		get  func(*fw.Config) any
		want any
	}{
		{"string", map[string]string{"FW_BUS_URL": "nats://nats:4222"}, func(c *fw.Config) any { return c.Bus.URL }, "nats://nats:4222"},
		{"top-level string", map[string]string{"FW_LISTEN": ":9090"}, func(c *fw.Config) any { return c.Listen }, ":9090"},
		{"model", map[string]string{"FW_MODEL": "openai/gpt-6"}, func(c *fw.Config) any { return c.Model }, "openai/gpt-6"},
		{"auth mode", map[string]string{"FW_AUTH_MODE": "none"}, func(c *fw.Config) any { return c.Auth.Mode }, "none"},
		{"camel case key", map[string]string{"FW_AUTH_TOKEN_ENV": "MY_TOKEN"}, func(c *fw.Config) any { return c.Auth.TokenEnv }, "MY_TOKEN"},
		{"mcp path", map[string]string{"FW_MCP_PATH": "/m"}, func(c *fw.Config) any { return c.MCP.Path }, "/m"},
		{"int", map[string]string{"FW_TOOLS_SEARCH_THRESHOLD": "40"}, func(c *fw.Config) any { return c.Tools.SearchThreshold }, 40},
		{"max runs", map[string]string{"FW_MAX_RUNS": "4"}, func(c *fw.Config) any { return c.MaxRuns }, 4},
		{"duration", map[string]string{"FW_BUDGET_DEADLINE": "30m"}, func(c *fw.Config) any { return c.Budget.Deadline }, 30 * time.Minute},
		{"acronym duration", map[string]string{"FW_MCP_TASK_TTL": "2h"}, func(c *fw.Config) any { return c.MCP.TaskTTL }, 2 * time.Hour},
		{"bool", map[string]string{"FW_MCP_TASKS": "false"}, func(c *fw.Config) any { return c.MCP.Tasks }, false},
		{"float", map[string]string{"FW_BUDGET_MAX_COST_USD": "2.5"}, func(c *fw.Config) any { return c.Budget.MaxCostUSD }, 2.5},
		{"list surfaces", map[string]string{"FW_SURFACES": "native, metrics,,"}, func(c *fw.Config) any { return c.Surfaces }, []string{"native", "metrics"}},
		{"list builtin", map[string]string{"FW_TOOLS_BUILTIN": "bash,files"}, func(c *fw.Config) any { return c.Tools.Builtin }, []string{"bash", "files"}},
		{"list egress types", map[string]string{"FW_BUS_EGRESS_TYPES": "fw.run.finished"}, func(c *fw.Config) any { return c.Bus.EgressTypes }, []string{"fw.run.finished"}},
		{"intake retries", map[string]string{"FW_BUS_INTAKE_RETRIES": "2"}, func(c *fw.Config) any { return c.Bus.IntakeRetries }, 2},
		{"dead letter off", map[string]string{"FW_BUS_DEAD_LETTER": "false"}, func(c *fw.Config) any { return c.Bus.DeadLetterOn() }, false},
		{"dead letter on by default", map[string]string{"FW_BUS_URL": "x"}, func(c *fw.Config) any { return c.Bus.DeadLetterOn() }, true},
		{"list http allow", map[string]string{"FW_TOOLS_HTTP_ALLOW": "https://a/*,https://b/*"}, func(c *fw.Config) any { return c.Tools.HTTP.Allow }, []string{"https://a/*", "https://b/*"}},
		{"list skills", map[string]string{"FW_TOOLS_SKILLS": "/skills,/more"}, func(c *fw.Config) any { return c.Tools.Skills }, []string{"/skills", "/more"}},
		{"telemetry log", map[string]string{"FW_TELEMETRY_LOG": "debug"}, func(c *fw.Config) any { return c.Telemetry.Log }, "debug"},
		{"log alias", map[string]string{"FW_LOG": "warn"}, func(c *fw.Config) any { return c.Telemetry.Log }, "warn"},
		{"full name wins over alias", map[string]string{"FW_LOG": "warn", "FW_TELEMETRY_LOG": "error"}, func(c *fw.Config) any { return c.Telemetry.Log }, "error"},
		{"inline struct", map[string]string{"FW_POLICY_DEFAULT": "ask", "FW_POLICY_DENY": "bash"}, func(c *fw.Config) any { return []any{c.Policy.Default, c.Policy.Deny} }, []any{"ask", []string{"bash"}}},
		{"inline limit", map[string]string{"FW_TOOLS_LIMITS_TIMEOUT": "1m", "FW_TOOLS_LIMITS_MAX_INPUT_BYTES": "10"}, func(c *fw.Config) any { return c.Tools.Limits.Limit }, fw.Limit{MaxInputBytes: 10, MaxOutputBytes: fw.DefaultMaxOutputBytes, Timeout: time.Minute}},
		{"pointer struct", map[string]string{"FW_POLICY_EXTERNAL_BUS": "fw.policy", "FW_POLICY_EXTERNAL_TIMEOUT": "5s"}, func(c *fw.Config) any { return *c.Policy.External }, fw.ExternalPolicyConfig{Bus: "fw.policy", Timeout: 5 * time.Second}},
		{"absent pointer stays nil", map[string]string{"FW_BUS_URL": "x"}, func(c *fw.Config) any { return c.Policy.External == nil }, true},
		{"empty value is unset", map[string]string{"FW_LISTEN": "  "}, func(c *fw.Config) any { return c.Listen }, ":8080"},
		{"ax server", map[string]string{"FW_AX_SERVER": "ax:1"}, func(c *fw.Config) any { return c.AX.Server }, "ax:1"},
		{"spawn", map[string]string{"FW_SPAWN_MAX_DEPTH": "1", "FW_SPAWN_MAX_CHILDREN": "2", "FW_SPAWN_MAX_DESCENDANTS": "5", "FW_SPAWN_MAX_RUNNING": "3"}, func(c *fw.Config) any { return c.Spawn }, fw.SpawnConfig{MaxDepth: 1, MaxChildren: 2, MaxDescendants: 5, MaxRunning: 3}},
		{"spawn exhausted", map[string]string{"FW_SPAWN_DISABLED": "true", "FW_SPAWN_MAX_DEPTH": "0"}, func(c *fw.Config) any { return c.Spawn }, fw.SpawnConfig{Disabled: true, MaxDepth: 0, MaxChildren: 16, MaxDescendants: 32, MaxRunning: 8}},
		{"budget shape", map[string]string{"FW_BUDGET_MAX_TOOL_CALLS": "9", "FW_BUDGET_MAX_CALLS_PER_TURN": "3", "FW_BUDGET_MAX_REPEATS": "2"}, func(c *fw.Config) any {
			return []int{c.Budget.MaxToolCalls, c.Budget.MaxCallsPerTurn, c.Budget.MaxRepeats}
		}, []int{9, 3, 2}},
		{"bash timeout", map[string]string{"FW_TOOLS_BASH_TIMEOUT": "90s"}, func(c *fw.Config) any { return c.Tools.Bash.Timeout }, 90 * time.Second},
		{"tool user", map[string]string{"FW_TOOLS_USER": "65532:65532"}, func(c *fw.Config) any { return c.Tools.User }, "65532:65532"},
		{"files per run", map[string]string{"FW_TOOLS_FILES_PER_RUN": "true"}, func(c *fw.Config) any { return c.Tools.Files.PerRunOn() }, true},
		{"files per run off", map[string]string{"FW_TOOLS_FILES_PER_RUN": "false"}, func(c *fw.Config) any { return c.Tools.Files.PerRun != nil && !*c.Tools.Files.PerRun }, true},
		{"files per run unset", map[string]string{"FW_BUS_URL": "x"}, func(c *fw.Config) any { return c.Tools.Files.PerRun == nil }, true},
		{"value is not expanded", map[string]string{"FW_BUS_URL": "${NATS_URL}", "NATS_URL": "nats://x"}, func(c *fw.Config) any { return c.Bus.URL }, "${NATS_URL}"},
		{"time zone", map[string]string{"FW_TIME_ZONE": "America/Chicago"}, func(c *fw.Config) any { return c.Time.Zone }, "America/Chicago"},
		{"time note after", map[string]string{"FW_TIME_NOTE_AFTER": "30m"}, func(c *fw.Config) any { return c.Time.NoteAfter }, 30 * time.Minute},
		{"time notes off", map[string]string{"FW_TIME_NOTE_AFTER": "0"}, func(c *fw.Config) any { return c.Time.NoteAfter }, time.Duration(0)},
		{"profile time zone", map[string]string{"FW_AGENTS_REVIEWER_TIME_ZONE": "Europe/Berlin"}, func(c *fw.Config) any { return c.Agents["reviewer"].TimeZone }, "Europe/Berlin"},
		{"policy external mcp", map[string]string{"FW_POLICY_EXTERNAL_MCP": "control", "FW_POLICY_EXTERNAL_TOOL": "decide"}, func(c *fw.Config) any { return *c.Policy.External }, fw.ExternalPolicyConfig{MCP: "control", Tool: "decide"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fw.DefaultConfig()
			if err := applyEnv(cfg, envList(tc.env)); err != nil {
				t.Fatal(err)
			}
			if got := tc.get(cfg); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestEnvBadValuesAreNamed(t *testing.T) {
	env := map[string]string{"FW_MAX_RUNS": "many", "FW_BUDGET_DEADLINE": "soon", "FW_MCP_TASKS": "maybe"}
	err := applyEnv(fw.DefaultConfig(), envList(env))
	if err == nil {
		t.Fatal("no error for bad values")
	}
	for k := range env {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("error does not name %s: %v", k, err)
		}
	}
}

func TestEnvNames(t *testing.T) {
	names := EnvNames()
	for _, want := range []string{
		"FW_BUS_URL", "FW_TOOLS_SEARCH_THRESHOLD", "FW_AUTH_MODE", "FW_MCP_PATH", "FW_TELEMETRY_LOG", "FW_MODEL", "FW_HOME", "FW_MAX_RUNS", "FW_BUDGET_MAX_COST_USD",
		"FW_TOOLS_LIMITS_TIMEOUT", "FW_TOOLS_BASH_TIMEOUT", "FW_TOOLS_SKILLS", "FW_SPAWN_MAX_DEPTH", "FW_BUDGET_MAX_REPEATS",
		"FW_TOOLS_USER", "FW_TOOLS_FILES_PER_RUN",
		"FW_TIME_ZONE", "FW_TIME_NOTE_AFTER", "FW_AGENTS_<NAME>_TIME_ZONE",
		"FW_POLICY_EXTERNAL_MCP", "FW_POLICY_EXTERNAL_TOOL", "FW_POLICY_EXTERNAL_BUS", "FW_POLICY_EXTERNAL_TIMEOUT",
		"FW_BUS_INTAKE_RETRIES", "FW_BUS_DEAD_LETTER",
	} {
		if !slices.Contains(names, want) {
			t.Errorf("EnvNames lacks %s", want)
		}
	}
	for _, fileOnly := range []string{"FW_AGENTS", "FW_MODELS", "FW_PRICES", "FW_TOOLS_MCP", "FW_TOOLS_PEERS", "FW_VERSION", "FW_TOOLS_MCP_<NAME>_NAME", "FW_AGENTS_<NAME>_NAME"} {
		if slices.Contains(names, fileOnly) {
			t.Errorf("EnvNames has %s", fileOnly)
		}
	}
	if len(slices.Compact(slices.Clone(names))) != len(names) {
		t.Errorf("two keys map to the same variable: %v", names)
	}
	if !slices.IsSorted(names) {
		t.Error("EnvNames is not sorted")
	}
}

func TestUpperSnake(t *testing.T) {
	for in, want := range map[string]string{
		"url": "URL", "searchThreshold": "SEARCH_THRESHOLD", "maxCostUSD": "MAX_COST_USD",
		"taskTTL": "TASK_TTL", "a2a": "A2A", "tokenEnv": "TOKEN_ENV", "clientId": "CLIENT_ID",
		"baseUrl": "BASE_URL", "apiKeyEnv": "API_KEY_ENV", "perTool": "PER_TOOL",
	} {
		if got := upperSnake(in); got != want {
			t.Errorf("upperSnake(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadFileThenEnv(t *testing.T) {
	cleanEnv(t)
	dir := t.TempDir()
	path := writeFile(t, filepath.Join(dir, "fw.yaml"), `
listen: ":7000"
model: anthropic/claude-haiku-5-5
bus:
  driver: nats
  url: nats://file:4222
budget:
  maxTurns: 50
policy:
  deny: [bash]
`)
	t.Setenv("FW_BUS_URL", "nats://env:4222")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":7000" || cfg.Model != "anthropic/claude-haiku-5-5" || cfg.Bus.Driver != "nats" {
		t.Errorf("file values not applied: %+v", cfg)
	}
	if cfg.Bus.URL != "nats://env:4222" {
		t.Errorf("variable does not win over the file: %q", cfg.Bus.URL)
	}
	// Keys absent from the file keep their defaults.
	if cfg.Budget.MaxTurns != 50 || cfg.Budget.Deadline != 2*time.Hour || cfg.Budget.MaxTokens != 2_000_000 {
		t.Errorf("budget overlay: %+v", cfg.Budget)
	}
	if cfg.Policy.Default != "allow" || !slices.Equal(cfg.Policy.Ask, []string{"destructive"}) || !slices.Equal(cfg.Policy.Deny, []string{"bash"}) {
		t.Errorf("policy overlay: %+v", cfg.Policy)
	}
	if cfg.Effort != "high" || cfg.MaxRuns != 0 {
		t.Errorf("defaults lost: effort %q, maxRuns %d", cfg.Effort, cfg.MaxRuns)
	}
}

func TestLoadUnknownKey(t *testing.T) {
	cleanEnv(t)
	dir := t.TempDir()
	for _, tc := range []struct{ yaml, key, line string }{
		{"listen: \":1\"\nbogus: 1\n", "bogus", "line 2"},
		{"bus:\n  driver: mem\n  urll: x\n", "urll", "line 3"},
		{"tools:\n  limits:\n    maxInputBytes: 1\n    perTool:\n      http: {maxOutputBytez: 1}\n", "maxOutputBytez", "line 5"},
		{"policy:\n  defualt: ask\n", "defualt", "line 2"},
		{"agents:\n  a:\n    budget: {maxTurn: 1}\n", "maxTurn", "line 3"},
		{"tools:\n  mcp:\n    - name: x\n      auth: {oauth: {clientid: a}}\n", "clientid", "line 4"},
	} {
		path := writeFile(t, filepath.Join(dir, "fw.yaml"), tc.yaml)
		_, err := Load(path)
		if err == nil || !strings.Contains(err.Error(), tc.key) || !strings.Contains(err.Error(), tc.line) {
			t.Errorf("Load(%q) error %v, want one that names %q on %s", tc.yaml, err, tc.key, tc.line)
		}
	}
}

func TestLoadTypeError(t *testing.T) {
	cleanEnv(t)
	path := writeFile(t, filepath.Join(t.TempDir(), "fw.yaml"), "maxRuns: many\n")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "line 1") || !strings.Contains(err.Error(), "many") {
		t.Errorf("type error: %v", err)
	}
}

func TestLoadNoFile(t *testing.T) {
	cleanEnv(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	want := fw.DefaultConfig()
	if cfg.Listen != want.Listen || cfg.Model != want.Model {
		t.Errorf("defaults: %+v", cfg)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("no error for a missing file")
	}
}

// exampleRefs lists the variables that examples/fw.yaml references.
func exampleRefs(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile("../examples/fw.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)`).FindAllStringSubmatch(string(data), -1) {
		if !slices.Contains(names, m[1]) {
			names = append(names, m[1])
		}
	}
	return names
}

func TestLoadExample(t *testing.T) {
	cleanEnv(t)
	for _, name := range exampleRefs(t) {
		t.Setenv(name, "")
	}
	cfg, err := Load("../examples/fw.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Tools.MCP) != 4 || cfg.Tools.MCP[0].Timeout != time.Minute || cfg.Tools.MCP[1].Auth.OAuth == nil {
		t.Errorf("tools.mcp: %+v", cfg.Tools.MCP)
	}
	if s := cfg.Tools.MCP[3].Auth.SPIFFE; s == nil || s.Audience != "planner" {
		t.Errorf("tools.mcp[3].auth.spiffe: %+v", s)
	}
	if got := cfg.Auth.Modes(); !slices.Equal(got, []string{"oidc", "spiffe", "token"}) {
		t.Errorf("auth.mode: %v", got)
	}
	if cfg.Auth.OIDC == nil || cfg.Auth.OIDC.Issuer != "https://idp.example.com" || cfg.Auth.SPIFFE == nil || cfg.Auth.SPIFFE.TrustDomain != "example.org" {
		t.Errorf("auth blocks: oidc %+v spiffe %+v", cfg.Auth.OIDC, cfg.Auth.SPIFFE)
	}
	if cfg.TLS.ClientAuth != "none" || cfg.TLS.Provider != "" {
		t.Errorf("tls: %+v", cfg.TLS)
	}
	if cfg.Access.Default != "open" || !slices.Equal(cfg.Access.Admins, []string{"local:*"}) {
		t.Errorf("access: %+v", cfg.Access)
	}
	if acc := cfg.Agents["reviewer"].Access; acc == nil || !slices.Equal(acc.Owners, []string{"alice@example.com"}) || len(acc.Viewers) != 2 {
		t.Errorf("reviewer access: %+v", acc)
	}
	// Every reference in the example has a default, so the file loads
	// with no variable set.
	if got := cfg.Tools.MCP[0].Endpoint; got != "https://git-mcp.default.svc.cluster.local:8443/mcp" {
		t.Errorf("default of the endpoint reference: %q", got)
	}
	if got := cfg.Providers["openai"].BaseURL; got != "https://api.openai.com/v1" {
		t.Errorf("default of the base URL reference: %q", got)
	}
	if got := cfg.Tools.Bash.Env["NATS_URL"]; got != "nats://nats:4222" {
		t.Errorf("default of the bash env reference: %q", got)
	}
	if p := cfg.Agents["reviewer"]; p.Model != "fast" || !strings.Contains(p.System, "review") || len(p.Instructions) != 1 || !slices.Equal(p.Skills, []string{"review-*"}) || p.Description == "" {
		t.Errorf("reviewer profile: %+v", p)
	}
	if p := cfg.Agents["default"]; strings.HasSuffix(p.System, ".md") || p.Skills != nil {
		t.Errorf("default profile: %+v", p)
	}
	if l := cfg.Tools.Limits.For("http"); l.MaxOutputBytes != 1<<20 || l.Timeout != 10*time.Minute {
		t.Errorf("limits.For(http): %+v", l)
	}
	if l := cfg.Tools.Limits.For("git.push"); l.Timeout != 2*time.Minute {
		t.Errorf("limits.For(git.push): %+v", l)
	}
	if cfg.Spawn != (fw.SpawnConfig{MaxDepth: 3, MaxChildren: 16, MaxDescendants: 32, MaxRunning: 8}) || cfg.Budget.MaxRepeats != 5 {
		t.Errorf("spawn %+v, budget %+v", cfg.Spawn, cfg.Budget)
	}
	if o := cfg.Providers["openai"].Options; o["idleTimeout"] != "120s" || o["parallelToolCalls"] != "true" {
		t.Errorf("openai options: %v", o)
	}
	if !slices.Equal(cfg.Tools.Builtin, fw.DefaultConfig().Tools.Builtin) {
		t.Errorf("tools.builtin %v differs from the default %v", cfg.Tools.Builtin, fw.DefaultConfig().Tools.Builtin)
	}
}

// The example loads with every variable that it references set to a dummy
// value, and the values land.
func TestLoadExampleWithVariables(t *testing.T) {
	cleanEnv(t)
	refs := exampleRefs(t)
	if len(refs) < 2 {
		t.Fatalf("the example references %d variables, want at least 2", len(refs))
	}
	for _, name := range refs {
		t.Setenv(name, "dummy-"+name)
	}
	cfg, err := Load("../examples/fw.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Tools.MCP[0].Endpoint; got != "dummy-GIT_MCP_ENDPOINT" {
		t.Errorf("endpoint: %q", got)
	}
	if got := cfg.Providers["openai"].BaseURL; got != "dummy-OPENAI_BASE_URL" {
		t.Errorf("base URL: %q", got)
	}
}

// The fw.yaml and the profile that the ax example ships in its Workspace
// files load as they are.
func TestLoadAXExampleWorkspaceFiles(t *testing.T) {
	cleanEnv(t)
	_, wss, err := ax.LoadFromFiles("", []string{"../examples/ax/reviewer.yaml"})
	if err != nil || len(wss) == 0 {
		t.Fatalf("workspaces %d, error %v", len(wss), err)
	}
	ws := t.TempDir()
	for _, f := range wss[0].Spec.Files {
		writeFile(t, filepath.Join(ws, f.Path), f.Content)
	}
	home := filepath.Join(ws, ".fw")
	t.Setenv("FW_HOME", home)
	if got := FindIn(""); got != filepath.Join(home, "fw.yaml") {
		t.Fatalf("FindIn = %q", got)
	}
	cfg, err := Load(filepath.Join(home, "fw.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.Mode != "none" || cfg.Bus.Driver != "redis" {
		t.Errorf("config: %+v", cfg)
	}
	p, ok := cfg.Agents["reviewer"]
	if !ok || !strings.Contains(p.System, "pull requests") || p.Budget == nil || p.Budget.Deadline != 30*time.Minute {
		t.Errorf("reviewer profile from the agents directory: %+v", p)
	}
}

func TestExpandInFile(t *testing.T) {
	cleanEnv(t)
	t.Setenv("NATS_URL", "nats://nats:4222")
	t.Setenv("PORT", "9000")
	t.Setenv("EMPTY", "")
	t.Setenv("INNER", "inner")
	path := writeFile(t, filepath.Join(t.TempDir(), "fw.yaml"), `
bus:
  url: ${NATS_URL}
  intake: "${NATS_URL}/jobs"
  egress: ${MISSING:-fw.events}
  options:
    a: ${EMPTY:-fallback}
    b: ${MISSING:-${INNER}}
    c: "cost $$5 or $5 for a $ sign"
maxRuns: ${PORT}
mcp:
  tasks: ${MISSING:-false}
  path: "${PORT}"
tools:
  mcp:
    - name: git
      endpoint: ${NATS_URL:-x}
agents:
  a: {system: "Use $${HOME} as the home of ${INNER}."}
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"url":      "nats://nats:4222",
		"intake":   "nats://nats:4222/jobs",
		"egress":   "fw.events",
		"a":        "fallback",
		"b":        "inner",
		"c":        "cost $5 or $5 for a $ sign",
		"endpoint": "nats://nats:4222",
		"system":   "Use ${HOME} as the home of inner.",
	}
	got := map[string]string{
		"url": cfg.Bus.URL, "intake": cfg.Bus.Intake, "egress": cfg.Bus.Egress,
		"a": cfg.Bus.Options["a"], "b": cfg.Bus.Options["b"], "c": cfg.Bus.Options["c"],
		"endpoint": cfg.Tools.MCP[0].Endpoint, "system": cfg.Agents["a"].System,
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
	}
	// A plain scalar takes the type of the expanded text, and a quoted
	// scalar stays a string.
	if cfg.MaxRuns != 9000 || cfg.MCP.Tasks || cfg.MCP.Path != "9000" {
		t.Errorf("maxRuns %d, mcp.tasks %v, mcp.path %q", cfg.MaxRuns, cfg.MCP.Tasks, cfg.MCP.Path)
	}
}

func TestExpandErrorsNameTheVariableAndTheFile(t *testing.T) {
	cleanEnv(t)
	t.Setenv("EMPTY", "")
	dir := t.TempDir()
	for _, tc := range []struct {
		yaml string
		want []string
	}{
		{"bus:\n  url: ${NOT_SET}\n", []string{"fw.yaml", "line 2", "bus.url", "variable NOT_SET is not set"}},
		// An empty value counts as unset, as it does for an FW_* variable.
		{"bus:\n  url: ${EMPTY}\n", []string{"bus.url", "variable EMPTY is not set; an empty value counts as unset"}},
		{"tools:\n  mcp:\n    - name: a\n      endpoint: ${NOT_SET\n", []string{"tools.mcp[0].endpoint", "no closing brace"}},
		{"listen: ${1BAD}\n", []string{"listen", "${1BAD} is not a variable reference"}},
		{"listen: ${}\n", []string{"listen", "${} is not a variable reference"}},
		{"listen: ${NOT_SET:-${ALSO_NOT_SET}}\n", []string{"listen", "variable ALSO_NOT_SET is not set"}},
		// Unknown keys and unset variables are reported together.
		{"bogus: 1\nlisten: ${NOT_SET}\n", []string{"field bogus not found", "variable NOT_SET is not set"}},
	} {
		path := writeFile(t, filepath.Join(dir, "fw.yaml"), tc.yaml)
		_, err := Load(path)
		if err == nil {
			t.Errorf("Load(%q): no error", tc.yaml)
			continue
		}
		for _, w := range tc.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("Load(%q) error lacks %q:\n%v", tc.yaml, w, err)
			}
		}
	}
}

// Profile files expand their values, and a prompt read from a file does
// not.
func TestExpandInProfileFiles(t *testing.T) {
	cleanEnv(t)
	home := os.Getenv("FW_HOME")
	t.Setenv("REVIEW_MODEL", "anthropic/claude-haiku-5-5")
	writeFile(t, filepath.Join(home, "agents", "reviewer.yaml"), "model: ${REVIEW_MODEL}\nsystem: prompt.md\ninstructions: [\"Prefer ${REVIEW_MODEL}.\", notes.md]\n")
	writeFile(t, filepath.Join(home, "agents", "prompt.md"), "Literal ${REVIEW_MODEL} stays.")
	writeFile(t, filepath.Join(home, "agents", "notes.md"), "Notes with ${REVIEW_MODEL}.")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Agents["reviewer"]
	if p.Model != "anthropic/claude-haiku-5-5" || p.System != "Literal ${REVIEW_MODEL} stays." {
		t.Errorf("profile: %+v", p)
	}
	if want := []string{"Prefer anthropic/claude-haiku-5-5.", "Notes with ${REVIEW_MODEL}."}; !slices.Equal(p.Instructions, want) {
		t.Errorf("instructions %q, want %q", p.Instructions, want)
	}

	writeFile(t, filepath.Join(home, "agents", "bad.yaml"), "model: ${NOT_SET}\n")
	_, err = Load("")
	if err == nil || !strings.Contains(err.Error(), "bad.yaml") || !strings.Contains(err.Error(), "NOT_SET") {
		t.Errorf("unset variable in a profile file: %v", err)
	}
}

func TestProfileSystemFiles(t *testing.T) {
	cleanEnv(t)
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("FW_HOME", home)
	writeFile(t, filepath.Join(dir, "prompts", "a.md"), "from the config directory")
	writeFile(t, filepath.Join(home, "prompts", "b.md"), "from home")
	writeFile(t, filepath.Join(dir, "prompts", "both.md"), "config directory first")
	writeFile(t, filepath.Join(home, "prompts", "both.md"), "home second")
	abs := writeFile(t, filepath.Join(t.TempDir(), "abs.md"), "absolute")
	path := writeFile(t, filepath.Join(dir, "fw.yaml"), `
agents:
  a: {system: prompts/a.md}
  b: {system: prompts/b.md}
  both: {system: prompts/both.md}
  abs: {system: `+abs+`}
  inline: {system: "You are terse."}
  missing: {system: prompts/missing.md}
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"a": "from the config directory", "b": "from home", "both": "config directory first",
		"abs": "absolute", "inline": "You are terse.", "missing": "prompts/missing.md",
	}
	for name, sys := range want {
		p := cfg.Agents[name]
		if p.System != sys || p.Name != name {
			t.Errorf("%s: name %q system %q, want %q", name, p.Name, p.System, sys)
		}
	}
}

func TestProfileInstructionsAndDescription(t *testing.T) {
	cleanEnv(t)
	dir := t.TempDir()
	home := os.Getenv("FW_HOME")
	writeFile(t, filepath.Join(dir, "prompts", "rules.md"), "house rules")
	writeFile(t, filepath.Join(home, "prompts", "notes.md"), "home notes")
	path := writeFile(t, filepath.Join(dir, "fw.yaml"), `
agents:
  a:
    description: prompts/rules.md
    system: prompts/rules.md
    instructions: [prompts/rules.md, "Be brief.", prompts/notes.md, prompts/missing.md]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Agents["a"]
	// A description is text, never a file.
	if p.Description != "prompts/rules.md" || p.System != "house rules" {
		t.Errorf("profile: %+v", p)
	}
	if want := []string{"house rules", "Be brief.", "home notes", "prompts/missing.md"}; !slices.Equal(p.Instructions, want) {
		t.Errorf("instructions %q, want %q", p.Instructions, want)
	}
}

func TestAgentsDirectory(t *testing.T) {
	cleanEnv(t)
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("FW_HOME", home)
	writeFile(t, filepath.Join(home, "agents", "writer.yaml"), "system: You write.\ntools: [files]\n")
	writeFile(t, filepath.Join(home, "agents", "file-name.yml"), "name: named\nsystem: prompt.md\n")
	writeFile(t, filepath.Join(home, "agents", "prompt.md"), "next to the profile")
	writeFile(t, filepath.Join(home, "agents", "reviewer.yaml"), "system: from the directory\n")
	writeFile(t, filepath.Join(home, "agents", "notes.txt"), "not a profile")
	path := writeFile(t, filepath.Join(dir, "fw.yaml"), "agents:\n  reviewer:\n    system: from fw.yaml\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if p := cfg.Agents["writer"]; p.Name != "writer" || p.System != "You write." || !slices.Equal(p.Tools, []string{"files"}) {
		t.Errorf("writer: %+v", p)
	}
	if p, ok := cfg.Agents["named"]; !ok || p.System != "next to the profile" {
		t.Errorf("named: %+v (ok %v)", p, ok)
	}
	if _, ok := cfg.Agents["file-name"]; ok {
		t.Error("the name field does not win over the file name")
	}
	if p := cfg.Agents["reviewer"]; p.System != "from fw.yaml" {
		t.Errorf("a profile file overrides fw.yaml: %+v", p)
	}
	if len(cfg.Agents) != 3 {
		t.Errorf("agents: %v", sortedKeys(cfg.Agents))
	}

	writeFile(t, filepath.Join(home, "agents", "bad.yaml"), "sytem: typo\n")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "sytem") {
		t.Errorf("unknown key in a profile file: %v", err)
	}
}

func TestValidateListsEveryProblem(t *testing.T) {
	cleanEnv(t)
	path := writeFile(t, filepath.Join(t.TempDir(), "fw.yaml"), `
listen: ""
auth: {mode: password}
effort: extreme
model: claude
models: {fast: haiku}
providers:
  OpenAI: {baseUrl: x}
  "my-vllm": {timeout: -1s}
surfaces: [native, ""]
telemetry: {log: loud}
maxRuns: -1
bus: {intakeRetries: -1}
budget: {maxTurns: -1, maxTokens: -2, maxCostUSD: -0.5, deadline: -1s, maxToolCalls: -1, maxCallsPerTurn: -1, maxRepeats: -1}
spawn: {maxDepth: -1, maxChildren: -1}
compaction: {at: -1, model: nosuch}
time: {zone: Mars/Olympus_Mons, noteAfter: -1m}
tools:
  bash: {timeout: -1s}
  memory: {scope: team}
  limits:
    maxInputBytes: -1
    maxOutputBytes: -1
    timeout: -1s
    perTool:
      "git.[": {timeout: 1s}
      http: {maxInputBytes: -1}
agents:
  a: {model: nosuch}
  b: {model: fast}
  c: {model: openai/gpt-6, effort: huge}
  d: {}
  e: {skills: ["", "x"], tools: [files, " "], budget: {maxTurns: -1}}
  f: {timeZone: Atlantis/Capital}
`)
	_, err := Load(path)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error %v is not a ValidationError", err)
	}
	wants := []string{
		"auth.mode", "effort \"extreme\"", "listen is empty", "model \"claude\"", "models.fast",
		"providers.OpenAI: a provider name", "providers.my-vllm: a provider name", "providers.my-vllm.timeout -1s is negative",
		"surfaces[1]", "telemetry.log", "maxRuns -1 is negative", "bus.intakeRetries -1 is negative",
		"budget.maxTurns -1", "budget.maxTokens -2", "budget.maxCostUSD -0.5", "budget.deadline -1s", "budget.maxToolCalls -1", "budget.maxCallsPerTurn -1", "budget.maxRepeats -1",
		"spawn.maxDepth -1", "spawn.maxChildren -1", "tools.bash.timeout -1s",
		"compaction.at -1 is negative", "compaction.model \"nosuch\"", "tools.memory.scope \"team\"",
		"time.zone \"Mars/Olympus_Mons\"", "time.noteAfter -1m0s is negative", "agents.f.timeZone \"Atlantis/Capital\"",
		"tools.limits.maxInputBytes -1", "tools.limits.maxOutputBytes -1", "tools.limits.timeout -1s",
		"tools.limits.perTool.git.[ is not a glob", "tools.limits.perTool.http.maxInputBytes -1",
		"agents.a.model", "agents.b.model", "agents.c.effort", "agents.e.skills[0] is empty", "agents.e.tools[1] is empty", "agents.e.budget.maxTurns -1",
	}
	for _, w := range wants {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error lacks %q:\n%v", w, err)
		}
	}
	if len(ve.Problems) != len(wants) {
		t.Errorf("%d problems, want %d:\n%v", len(ve.Problems), len(wants), err)
	}
	if strings.Contains(err.Error(), "agents.d") {
		t.Errorf("a profile without a model is valid:\n%v", err)
	}
}

func TestValidateAcceptsTheDefaults(t *testing.T) {
	cfg := fw.DefaultConfig()
	cfg.Providers = map[string]fw.ProviderConfig{"openai": {BaseURL: "http://vllm:8000/v1"}, "vllm2": {}}
	cfg.Tools.Limits.PerTool = map[string]fw.Limit{"git.*": {Timeout: time.Minute}, "http": {}}
	cfg.Agents = map[string]fw.AgentProfile{"a": {Effort: "XHigh", Skills: []string{}, Tools: nil}}
	cfg.Models = map[string]string{"fast": "anthropic/claude-haiku-5-5"}
	cfg.Compaction.Model = "fast"
	cfg.Tools.Memory.Scope = "Shared"
	cfg.Time.Zone = "Europe/Berlin"
	cfg.Agents["b"] = fw.AgentProfile{TimeZone: "UTC"}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestMasked(t *testing.T) {
	c := fw.DefaultConfig()
	c.Tools.MCP = []fw.MCPServerConfig{{
		Name:     "billing",
		Endpoint: "https://billing/mcp",
		TLS:      fw.TLSConfig{Cert: "/etc/cert.pem", Key: "/etc/key.pem"},
		Auth:     fw.MCPAuthConfig{Bearer: "BILLING_TOKEN", OAuth: &fw.OAuthClientConfig{ClientID: "fw", ClientSecret: "literal-secret"}},
		Headers:  map[string]string{"X-Api-Key": "literal-key", "X-Trace": "trace-value"},
	}}
	c.Providers = map[string]fw.ProviderConfig{"openai": {
		APIKeyEnv: "OPENAI_API_KEY",
		Headers:   map[string]string{"X-Team": "team-value"},
		Options:   map[string]string{"reasoning": "on"},
	}}
	c.Tools.Bash.Env = map[string]string{"NATS_URL": "nats://nats:4222", "GITHUB_TOKEN": "literal-token"}
	c.Secrets.Options = map[string]string{"password": "hunter2", "address": "vault:8200"}
	out, err := Masked(c)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, hidden := range []string{"literal-secret", "literal-key", "hunter2", "/etc/key.pem", "trace-value", "team-value", "literal-token"} {
		if strings.Contains(s, hidden) {
			t.Errorf("%q is not masked:\n%s", hidden, s)
		}
	}
	// tokenEnv and apiKeyEnv hold the name of a variable, not a value, so
	// they stay visible. Header values are masked, and header names stay.
	for _, shown := range []string{"driver: env", "vault:8200", "/etc/cert.pem", "X-Trace: '***'", "X-Team: '***'", "clientId: fw", "https://billing/mcp", "deadline: 2h0m0s", "tokenEnv: FW_API_TOKEN", "apiKeyEnv: OPENAI_API_KEY", "reasoning: \"on\"", "NATS_URL: nats://nats:4222"} {
		if !strings.Contains(s, shown) {
			t.Errorf("%q is missing:\n%s", shown, s)
		}
	}
	if strings.Count(s, "'***'") < 7 {
		t.Errorf("too few masks:\n%s", s)
	}
}

func TestFind(t *testing.T) {
	cleanEnv(t)
	t.Chdir(t.TempDir())
	home := os.Getenv("FW_HOME")
	if got := Find(); got != "" {
		t.Fatalf("Find with no file = %q", got)
	}
	homeFile := writeFile(t, filepath.Join(home, "fw.yaml"), "listen: \":1\"\n")
	if got := Find(); got != homeFile {
		t.Errorf("Find = %q, want %q", got, homeFile)
	}
	other := t.TempDir()
	otherFile := writeFile(t, filepath.Join(other, "fw.yaml"), "listen: \":2\"\n")
	if got := FindIn(other); got != otherFile {
		t.Errorf("FindIn = %q, want %q", got, otherFile)
	}
	writeFile(t, "fw.yaml", "listen: \":3\"\n")
	if got := Find(); got != "fw.yaml" {
		t.Errorf("Find = %q, want ./fw.yaml", got)
	}
	t.Setenv("FW_CONFIG", "/nonexistent/fw.yaml")
	if got := Find(); got != "/nonexistent/fw.yaml" {
		t.Errorf("Find = %q, want $FW_CONFIG", got)
	}
}

// A top-level key that a linked package claimed with fw.RegisterExtension
// lands in Extensions, and Extension decodes it. Every other unknown key is
// still an error, no variable sets a block, and the masked print keeps it.
func TestLoadExtensionBlock(t *testing.T) {
	cleanEnv(t)
	fw.RegisterExtension("testext")
	path := writeFile(t, filepath.Join(t.TempDir(), "fw.yaml"), "listen: \":1\"\ntestext:\n  teams: [team-*, ops]\n  depth: 3\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	var block struct {
		Teams []string `yaml:"teams"`
		Depth int      `yaml:"depth"`
	}
	ok, err := cfg.Extension("testext", &block)
	if err != nil || !ok || len(block.Teams) != 2 || block.Teams[0] != "team-*" || block.Depth != 3 {
		t.Fatalf("Extension = %v, %v: %+v", ok, err, block)
	}
	if ok, err := cfg.Extension("absent", &block); ok || err != nil {
		t.Fatalf("absent block = %v, %v", ok, err)
	}
	var wrong struct {
		Teams int `yaml:"teams"`
	}
	if ok, err := cfg.Extension("testext", &wrong); !ok || err == nil || !strings.Contains(err.Error(), "testext") {
		t.Fatalf("block of the wrong shape = %v, %v", ok, err)
	}
	out, err := Masked(cfg)
	if err != nil || !strings.Contains(string(out), "testext:") || !strings.Contains(string(out), "team-*") {
		t.Fatalf("Masked = %v:\n%s", err, out)
	}
	for _, name := range EnvNames() {
		if strings.Contains(name, "TESTEXT") {
			t.Fatalf("a variable names the block: %s", name)
		}
	}
	// An unclaimed key is still a typo.
	path = writeFile(t, filepath.Join(t.TempDir(), "fw.yaml"), "listen: \":1\"\nworkbenchh:\n  teams: [a]\n")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "workbenchh") || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("unclaimed block: %v", err)
	}
}
