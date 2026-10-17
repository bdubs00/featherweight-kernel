package config

import (
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// envList turns a map into the NAME=VALUE pairs that applyEnv reads.
func envList(m map[string]string) []string {
	var out []string
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func TestExpand(t *testing.T) {
	vars := map[string]string{"A": "a", "B": "b", "EMPTY": ""}
	lookup := func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
	cases := []struct{ in, want, err string }{
		{"plain", "plain", ""},
		{"${A}", "a", ""},
		{"x${A}y${B}z", "xaybz", ""},
		{"${A:-d}", "a", ""},
		{"${MISSING:-d}", "d", ""},
		{"${MISSING:-}", "", ""},
		{"${EMPTY:-d}", "d", ""},
		{"${EMPTY}", "", ""},
		{"${MISSING:-${A}}", "a", ""},
		{"${MISSING:-${ALSO_MISSING:-deep}}", "deep", ""},
		{"${MISSING:-http://h:1/p}", "http://h:1/p", ""},
		{"$$", "$", ""},
		{"$$$$", "$$", ""},
		{"$${A}", "${A}", ""},
		{"$5 and $", "$5 and $", ""},
		{"$A", "$A", ""},
		{"${MISSING}", "", "variable MISSING is not set"},
		{"${A", "", "${A has no closing brace"},
		{"${}", "", "${} is not a variable reference"},
		{"${1A}", "", "${1A} is not a variable reference"},
		{"${A B}", "", "${A B} is not a variable reference"},
		{"${A:-${MISSING}}", "a", ""}, // a default is read only when it is needed
	}
	for _, tc := range cases {
		got, err := expand(tc.in, lookup)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("expand(%q) error %v, want %q", tc.in, err, tc.err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("expand(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestEnvMapsOfStrings(t *testing.T) {
	cases := []struct {
		name string
		file func(*fw.Config)
		env  map[string]string
		get  func(*fw.Config) any
		want any
	}{
		{"models", nil, map[string]string{"FW_MODELS_FAST": "anthropic/claude-haiku-5-5"},
			func(c *fw.Config) any { return c.Models }, map[string]string{"fast": "anthropic/claude-haiku-5-5"}},
		{"models adds to the file", func(c *fw.Config) { c.Models = map[string]string{"slow": "x/y"} }, map[string]string{"FW_MODELS_FAST": "a/b"},
			func(c *fw.Config) any { return c.Models }, map[string]string{"slow": "x/y", "fast": "a/b"}},
		{"models key is lowercase", nil, map[string]string{"FW_MODELS_Code_Review": "a/b"},
			func(c *fw.Config) any { return c.Models }, map[string]string{"code_review": "a/b"}},
		{"models matches a file key loosely", func(c *fw.Config) { c.Models = map[string]string{"code-review": "x/y"} }, map[string]string{"FW_MODELS_CODE_REVIEW": "a/b"},
			func(c *fw.Config) any { return c.Models }, map[string]string{"code-review": "a/b"}},
		{"bash env keeps the key", nil, map[string]string{"FW_TOOLS_BASH_ENV_NATS_URL": "nats://nats:4222", "FW_TOOLS_BASH_ENV_Mixed": "m"},
			func(c *fw.Config) any { return c.Tools.Bash.Env }, map[string]string{"NATS_URL": "nats://nats:4222", "Mixed": "m"}},
		{"options are lower camel", nil, map[string]string{"FW_SECRETS_OPTIONS_ADDRESS": "vault:8200", "FW_BUS_OPTIONS_MAX_IN_FLIGHT": "3", "FW_LEDGER_OPTIONS_TTL": "1h"},
			func(c *fw.Config) any {
				return []map[string]string{c.Secrets.Options, c.Bus.Options, c.Ledger.Options}
			},
			[]map[string]string{{"address": "vault:8200"}, {"maxInFlight": "3"}, {"ttl": "1h"}}},
		{"empty value is unset", nil, map[string]string{"FW_MODELS_FAST": " "},
			func(c *fw.Config) any { return c.Models }, map[string]string(nil)},
		{"value is trimmed and not expanded", nil, map[string]string{"FW_MODELS_FAST": " ${X}/y "},
			func(c *fw.Config) any { return c.Models }, map[string]string{"fast": "${X}/y"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fw.DefaultConfig()
			if tc.file != nil {
				tc.file(cfg)
			}
			if err := applyEnv(cfg, envList(tc.env)); err != nil {
				t.Fatal(err)
			}
			if got := tc.get(cfg); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestEnvMapsOfStructs(t *testing.T) {
	cases := []struct {
		name string
		file func(*fw.Config)
		env  map[string]string
		get  func(*fw.Config) any
		want any
	}{
		{"provider base url", nil, map[string]string{"FW_PROVIDERS_OPENAI_BASE_URL": "http://vllm:8000/v1"},
			func(c *fw.Config) any { return c.Providers["openai"].BaseURL }, "http://vllm:8000/v1"},
		{"provider fields land in one entry", nil, map[string]string{
			"FW_PROVIDERS_OPENAI_BASE_URL": "http://vllm:8000/v1", "FW_PROVIDERS_OPENAI_API_KEY_ENV": "VLLM_KEY", "FW_PROVIDERS_OPENAI_TIMEOUT": "30s",
			"FW_PROVIDERS_OPENAI_TLS_INSECURE": "true", "FW_PROVIDERS_OPENAI_TLS_CA": "/ca.pem",
		},
			func(c *fw.Config) any { return c.Providers["openai"] },
			fw.ProviderConfig{BaseURL: "http://vllm:8000/v1", APIKeyEnv: "VLLM_KEY", Timeout: 30 * time.Second, TLS: fw.TLSConfig{CA: "/ca.pem", Insecure: true}}},
		{"provider keeps the file fields", func(c *fw.Config) {
			c.Providers = map[string]fw.ProviderConfig{"openai": {APIKeyEnv: "K", Headers: map[string]string{"X-A": "1"}}}
		}, map[string]string{"FW_PROVIDERS_OPENAI_BASE_URL": "http://x"},
			func(c *fw.Config) any { return c.Providers["openai"] },
			fw.ProviderConfig{BaseURL: "http://x", APIKeyEnv: "K", Headers: map[string]string{"X-A": "1"}}},
		{"provider headers are canonical and options lower camel", nil, map[string]string{
			"FW_PROVIDERS_OPENAI_HEADERS_X_API_KEY": "secret-name", "FW_PROVIDERS_OPENAI_HEADERS_X_TIMEOUT": "1",
			"FW_PROVIDERS_OPENAI_OPTIONS_MAX_TOKENS_FIELD": "max_tokens", "FW_PROVIDERS_OPENAI_OPTIONS_REASONING": "off", "FW_PROVIDERS_OPENAI_OPTIONS_TIMEOUT": "2",
		},
			func(c *fw.Config) any { return c.Providers["openai"] },
			fw.ProviderConfig{
				Headers: map[string]string{"X-Api-Key": "secret-name", "X-Timeout": "1"},
				Options: map[string]string{"maxTokensField": "max_tokens", "reasoning": "off", "timeout": "2"},
			}},
		{"provider name is lowercase", nil, map[string]string{"FW_PROVIDERS_Gemini_BASE_URL": "http://g"},
			func(c *fw.Config) any { return c.Providers["gemini"].BaseURL }, "http://g"},
		{"agent fields", nil, map[string]string{
			"FW_AGENTS_REVIEWER_MODEL": "fast", "FW_AGENTS_REVIEWER_TOOLS": "files, http", "FW_AGENTS_REVIEWER_EFFORT": "low",
			"FW_AGENTS_REVIEWER_SYSTEM": "You review.", "FW_AGENTS_REVIEWER_DESCRIPTION": "Reviews", "FW_AGENTS_REVIEWER_SKILLS": "review-*",
			"FW_AGENTS_REVIEWER_INSTRUCTIONS": "a,b",
		},
			func(c *fw.Config) any { return c.Agents["reviewer"] },
			fw.AgentProfile{Model: "fast", Tools: []string{"files", "http"}, Effort: "low", System: "You review.", Description: "Reviews", Skills: []string{"review-*"}, Instructions: []string{"a", "b"}}},
		{"agent pointer blocks", nil, map[string]string{
			"FW_AGENTS_REVIEWER_BUDGET_MAX_TURNS": "50", "FW_AGENTS_REVIEWER_BUDGET_DEADLINE": "10m", "FW_AGENTS_REVIEWER_POLICY_DENY": "bash,http",
		},
			func(c *fw.Config) any { return c.Agents["reviewer"] },
			fw.AgentProfile{Budget: &fw.Budget{MaxTurns: 50, Deadline: 10 * time.Minute}, Policy: &fw.Rules{Deny: []string{"bash", "http"}}}},
		{"agent from the file keeps its blocks", func(c *fw.Config) {
			c.Agents = map[string]fw.AgentProfile{"code-review": {System: "s", Budget: &fw.Budget{MaxTurns: 5}}}
		}, map[string]string{"FW_AGENTS_CODE_REVIEW_MODEL": "a/b", "FW_AGENTS_CODE_REVIEW_BUDGET_DEADLINE": "1m"},
			func(c *fw.Config) any { return c.Agents["code-review"] },
			fw.AgentProfile{System: "s", Model: "a/b", Budget: &fw.Budget{MaxTurns: 5, Deadline: time.Minute}}},
		{"per tool limit", nil, map[string]string{"FW_TOOLS_LIMITS_PER_TOOL_HTTP_MAX_OUTPUT_BYTES": "1048576", "FW_TOOLS_LIMITS_PER_TOOL_HTTP_TIMEOUT": "1m"},
			func(c *fw.Config) any { return c.Tools.Limits.PerTool },
			map[string]fw.Limit{"http": {MaxOutputBytes: 1 << 20, Timeout: time.Minute}}},
		{"price selector", nil, map[string]string{"FW_PRICES_ANTHROPIC_CLAUDE_OPUS_5_5_INPUT": "4", "FW_PRICES_ANTHROPIC_CLAUDE_OPUS_5_5_OUTPUT": "20"},
			func(c *fw.Config) any { return c.Prices },
			map[string]fw.Price{"anthropic/claude-opus-5-5": {Input: 4, Output: 20}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fw.DefaultConfig()
			if tc.file != nil {
				tc.file(cfg)
			}
			if err := applyEnv(cfg, envList(tc.env)); err != nil {
				t.Fatal(err)
			}
			if got := tc.get(cfg); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestEnvNamedLists(t *testing.T) {
	fileMCP := func(c *fw.Config) {
		c.Tools.MCP = []fw.MCPServerConfig{
			{Name: "git", Endpoint: "http://file/mcp", Timeout: time.Minute},
			{Name: "Git-MCP", Command: "npx"},
		}
	}
	cases := []struct {
		name string
		file func(*fw.Config)
		env  map[string]string
		get  func(*fw.Config) any
		want any
	}{
		{"creates an entry", nil, map[string]string{"FW_TOOLS_MCP_GIT_ENDPOINT": "http://git-mcp:8080/mcp"},
			func(c *fw.Config) any { return c.Tools.MCP },
			[]fw.MCPServerConfig{{Name: "git", Endpoint: "http://git-mcp:8080/mcp"}}},
		{"several variables fill one new entry", nil, map[string]string{
			"FW_TOOLS_MCP_GIT_ENDPOINT": "http://git-mcp:8080/mcp", "FW_TOOLS_MCP_GIT_AUTH_BEARER": "GIT_MCP_TOKEN",
			"FW_TOOLS_MCP_GIT_TRUSTED": "true", "FW_TOOLS_MCP_GIT_TIMEOUT": "30s", "FW_TOOLS_MCP_GIT_ARGS": "-y,x",
			"FW_TOOLS_MCP_GIT_HEADERS_X_TEAM": "t", "FW_TOOLS_MCP_GIT_TLS_CA": "/ca.pem",
		},
			func(c *fw.Config) any { return c.Tools.MCP },
			[]fw.MCPServerConfig{{
				Name: "git", Endpoint: "http://git-mcp:8080/mcp", Auth: fw.MCPAuthConfig{Bearer: "GIT_MCP_TOKEN"}, Trusted: true,
				Timeout: 30 * time.Second, Args: []string{"-y", "x"}, Headers: map[string]string{"X-Team": "t"}, TLS: fw.TLSConfig{CA: "/ca.pem"},
			}}},
		{"oauth pointer block", nil, map[string]string{
			"FW_TOOLS_MCP_BILLING_AUTH_OAUTH_CLIENT_ID": "fw", "FW_TOOLS_MCP_BILLING_AUTH_OAUTH_CLIENT_SECRET": "SECRET_NAME",
			"FW_TOOLS_MCP_BILLING_AUTH_OAUTH_SCOPES": "a,b", "FW_TOOLS_MCP_BILLING_AUTH_OAUTH_TOKEN_URL": "http://t",
		},
			func(c *fw.Config) any { return c.Tools.MCP[0].Auth.OAuth },
			&fw.OAuthClientConfig{ClientID: "fw", ClientSecret: "SECRET_NAME", Scopes: []string{"a", "b"}, TokenURL: "http://t"}},
		{"updates the entry from the file", fileMCP, map[string]string{"FW_TOOLS_MCP_GIT_ENDPOINT": "http://env/mcp"},
			func(c *fw.Config) any { return c.Tools.MCP[0] },
			fw.MCPServerConfig{Name: "git", Endpoint: "http://env/mcp", Timeout: time.Minute}},
		{"matches a name with case ignored and - as _", fileMCP, map[string]string{"FW_TOOLS_MCP_GIT_MCP_ENDPOINT": "http://env/mcp"},
			func(c *fw.Config) any { return []any{len(c.Tools.MCP), c.Tools.MCP[1]} },
			[]any{2, fw.MCPServerConfig{Name: "Git-MCP", Command: "npx", Endpoint: "http://env/mcp"}}},
		{"new entries follow the file in name order", fileMCP, map[string]string{"FW_TOOLS_MCP_ZED_COMMAND": "z", "FW_TOOLS_MCP_ALPHA_COMMAND": "a", "FW_TOOLS_MCP_ALPHA_TRUSTED": "true"},
			func(c *fw.Config) any {
				var names []string
				for _, s := range c.Tools.MCP {
					names = append(names, s.Name)
				}
				return []any{names, c.Tools.MCP[2].Trusted}
			},
			[]any{[]string{"git", "Git-MCP", "alpha", "zed"}, true}},
		{"longest field wins", nil, map[string]string{"FW_TOOLS_MCP_X_TLS_CA": "/ca", "FW_TOOLS_MCP_Y_HEADERS_X_TIMEOUT": "1"},
			func(c *fw.Config) any { return c.Tools.MCP },
			[]fw.MCPServerConfig{{Name: "x", TLS: fw.TLSConfig{CA: "/ca"}}, {Name: "y", Headers: map[string]string{"X-Timeout": "1"}}}},
		{"an entry named like a field", nil, map[string]string{"FW_TOOLS_MCP_ENDPOINT_ENDPOINT": "http://e", "FW_TOOLS_MCP_AUTH_AUTH_BEARER": "B"},
			func(c *fw.Config) any { return c.Tools.MCP },
			[]fw.MCPServerConfig{{Name: "auth", Auth: fw.MCPAuthConfig{Bearer: "B"}}, {Name: "endpoint", Endpoint: "http://e"}}},
		{"peers", func(c *fw.Config) { c.Tools.Peers = []fw.PeerConfig{{Name: "billing", Card: "http://card"}} },
			map[string]string{"FW_TOOLS_PEERS_REVIEWER_AX": "default/reviewer", "FW_TOOLS_PEERS_BILLING_CARD": "http://env/card.json"},
			func(c *fw.Config) any { return c.Tools.Peers },
			[]fw.PeerConfig{{Name: "billing", Card: "http://env/card.json"}, {Name: "reviewer", AX: "default/reviewer"}}},
		{"empty value is unset", nil, map[string]string{"FW_TOOLS_MCP_GIT_ENDPOINT": ""},
			func(c *fw.Config) any { return len(c.Tools.MCP) }, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fw.DefaultConfig()
			if tc.file != nil {
				tc.file(cfg)
			}
			if err := applyEnv(cfg, envList(tc.env)); err != nil {
				t.Fatal(err)
			}
			if got := tc.get(cfg); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestEnvEntryErrors(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want []string
	}{
		{"typo in a field", map[string]string{"FW_TOOLS_MCP_GIT_ENDPOITN": "x"},
			[]string{"FW_TOOLS_MCP_GIT_ENDPOITN: no field of tools.mcp matches", "FW_TOOLS_MCP_<NAME>_<FIELD>", "ENDPOINT", "AUTH_OAUTH_CLIENT_ID", "HEADERS_<NAME>", "TLS_CA"}},
		{"no entry name", map[string]string{"FW_TOOLS_MCP_ENDPOINT": "x"},
			[]string{"FW_TOOLS_MCP_ENDPOINT: no entry name before ENDPOINT; the form is FW_TOOLS_MCP_<NAME>_ENDPOINT"}},
		{"no entry name before a map", map[string]string{"FW_PROVIDERS_HEADERS_X": "x"},
			[]string{"FW_PROVIDERS_HEADERS_X: no entry name before HEADERS; the form is FW_PROVIDERS_<NAME>_HEADERS_<NAME>"}},
		{"name field is not settable", map[string]string{"FW_TOOLS_MCP_GIT_NAME": "x", "FW_AGENTS_A_NAME": "x"},
			[]string{"FW_TOOLS_MCP_GIT_NAME: no field of tools.mcp matches", "FW_AGENTS_A_NAME: no field of agents matches"}},
		{"prefix alone", map[string]string{"FW_MODELS": "x", "FW_TOOLS_MCP": "x", "FW_PROVIDERS": "x"},
			[]string{"FW_MODELS sets nothing; the form is FW_MODELS_<NAME>", "FW_TOOLS_MCP sets nothing; the form is FW_TOOLS_MCP_<NAME>_<FIELD>", "FW_PROVIDERS sets nothing"}},
		{"bad values are named", map[string]string{"FW_TOOLS_MCP_GIT_TIMEOUT": "soon", "FW_PROVIDERS_OPENAI_TLS_INSECURE": "maybe", "FW_PRICES_A_B_INPUT": "cheap"},
			[]string{`FW_TOOLS_MCP_GIT_TIMEOUT="soon": not a duration`, `FW_PROVIDERS_OPENAI_TLS_INSECURE="maybe": not a boolean`, `FW_PRICES_A_B_INPUT="cheap": not a number`}},
		{"a map key without a name", map[string]string{"FW_MODELS_": "x"},
			[]string{"FW_MODELS_"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := applyEnv(fw.DefaultConfig(), envList(tc.env))
			if err == nil {
				t.Fatal("no error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error lacks %q:\n%v", w, err)
				}
			}
		})
	}
}

// A variable under the prefix of a map that holds the whole key is not an
// error, whatever the key: FW_MODELS_ is the one exception, since its key
// is empty.
func TestEnvEmptyMapKey(t *testing.T) {
	cfg := fw.DefaultConfig()
	err := applyEnv(cfg, envList(map[string]string{"FW_MODELS_": "a/b"}))
	if err == nil || len(cfg.Models) != 0 {
		t.Fatalf("error %v, models %v", err, cfg.Models)
	}
}

func TestSplitEntry(t *testing.T) {
	routes := routesOf(reflect.TypeOf(fw.MCPServerConfig{}))
	cases := []struct {
		rest       string
		entry, key string
		suffix     string
		ok         bool
	}{
		{"GIT_ENDPOINT", "GIT", "", "ENDPOINT", true},
		{"GIT_MCP_ENDPOINT", "GIT_MCP", "", "ENDPOINT", true},
		{"GIT_AUTH_OAUTH_CLIENT_ID", "GIT", "", "AUTH_OAUTH_CLIENT_ID", true},
		{"GIT_TLS_CA", "GIT", "", "TLS_CA", true},
		{"MY_TLS_CA", "MY", "", "TLS_CA", true},
		{"GIT_HEADERS_X_API_KEY", "GIT", "X_API_KEY", "HEADERS", true},
		{"GIT_HEADERS_TIMEOUT", "GIT", "TIMEOUT", "HEADERS", true},
		{"HEADERS_ENDPOINT", "HEADERS", "", "ENDPOINT", true},
		{"ENDPOINT", "", "", "", false},
		{"GIT_HEADERS_", "", "", "", false},
		{"GIT_HEADERS", "", "", "", false},
		{"GIT_NAME", "", "", "", false},
		{"GIT_NOPE", "", "", "", false},
	}
	for _, tc := range cases {
		a, ok := splitEntry(tc.rest, routes)
		if ok != tc.ok || a.entry != tc.entry || a.key != tc.key || a.route.suffix != tc.suffix {
			t.Errorf("splitEntry(%q) = {entry %q key %q suffix %q} %v, want {entry %q key %q suffix %q} %v",
				tc.rest, a.entry, a.key, a.route.suffix, ok, tc.entry, tc.key, tc.suffix, tc.ok)
		}
	}
}

func TestKeyRules(t *testing.T) {
	for _, tc := range []struct {
		yamlKey, in, want string
	}{
		{"models", "FAST", "fast"},
		{"agents", "Code_Review", "code_review"},
		{"perTool", "HTTP", "http"},
		{"env", "NATS_URL", "NATS_URL"},
		{"env", "Mixed_case", "Mixed_case"},
		{"headers", "X_API_KEY", "X-Api-Key"},
		{"headers", "AUTHORIZATION", "Authorization"},
		{"options", "REASONING", "reasoning"},
		{"options", "MAX_TOKENS_FIELD", "maxTokensField"},
		{"options", "PROMPT_CACHE_KEY", "promptCacheKey"},
		{"prices", "ANTHROPIC_CLAUDE_OPUS_5_5", "anthropic/claude-opus-5-5"},
		{"prices", "OPENAI_GPT_6", "openai/gpt-6"},
		{"prices", "LONE", "lone"},
	} {
		if got := keyRuleFor(tc.yamlKey).convert(tc.in); got != tc.want {
			t.Errorf("rule %s: %q = %q, want %q", tc.yamlKey, tc.in, got, tc.want)
		}
	}
	if !keyRuleFor("models").loose || keyRuleFor("headers").loose || keyRuleFor("env").loose {
		t.Error("only the name rule matches loosely")
	}
}

func TestEnvNamesPatterns(t *testing.T) {
	names := EnvNames()
	for _, want := range []string{
		"FW_MODELS_<NAME>", "FW_TOOLS_BASH_ENV_<NAME>", "FW_SECRETS_OPTIONS_<NAME>", "FW_BUS_OPTIONS_<NAME>", "FW_LEDGER_OPTIONS_<NAME>",
		"FW_PROVIDERS_<NAME>_BASE_URL", "FW_PROVIDERS_<NAME>_API_KEY_ENV", "FW_PROVIDERS_<NAME>_HEADERS_<NAME>", "FW_PROVIDERS_<NAME>_OPTIONS_<NAME>", "FW_PROVIDERS_<NAME>_TLS_INSECURE",
		"FW_AGENTS_<NAME>_MODEL", "FW_AGENTS_<NAME>_TOOLS", "FW_AGENTS_<NAME>_SKILLS", "FW_AGENTS_<NAME>_BUDGET_MAX_TURNS", "FW_AGENTS_<NAME>_POLICY_DENY",
		"FW_TOOLS_LIMITS_PER_TOOL_<NAME>_MAX_OUTPUT_BYTES", "FW_PRICES_<NAME>_INPUT",
		"FW_TOOLS_MCP_<NAME>_ENDPOINT", "FW_TOOLS_MCP_<NAME>_AUTH_BEARER", "FW_TOOLS_MCP_<NAME>_AUTH_OAUTH_CLIENT_ID", "FW_TOOLS_MCP_<NAME>_HEADERS_<NAME>",
		"FW_TOOLS_PEERS_<NAME>_AX", "FW_TOOLS_PEERS_<NAME>_CARD",
	} {
		if !slices.Contains(names, want) {
			t.Errorf("EnvNames lacks %s", want)
		}
	}
	// Every listed pattern or name sets something: a pattern with its
	// placeholders filled in applies without an error.
	for _, n := range names {
		filled := strings.ReplaceAll(n, namePlaceholder, "X")
		cfg := fw.DefaultConfig()
		if err := applyEnv(cfg, []string{filled + "=1"}); err != nil && !strings.Contains(err.Error(), "not a") {
			t.Errorf("%s: %v", n, err)
		}
	}
}
