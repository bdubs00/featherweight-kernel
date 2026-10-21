package cli

// This file holds the verbs that set up and inspect an installation: fw
// init writes a starter layout, and fw env shows the variables.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bdubs00/featherweight-kernel/config"
	"github.com/bdubs00/featherweight-kernel/fw"
)

// starterConfig is the fw.yaml that fw init writes. It holds the keys that
// most installations change, with one comment each, and it is valid for the
// config loader as it is.
const starterConfig = `# fw.yaml: the configuration of one fw process.
#
# Every key has a default, and a FW_* variable overrides the file: model is
# FW_MODEL, and budget.maxTurns is FW_BUDGET_MAX_TURNS. Run fw env to see
# the variables and fw doctor to check this file.
#
# Paths in this file are relative to the directory that fw runs in, which
# is the directory of this file in the common case. The global flag -home
# overrides home.

home: .                          # runs, memory, agents, and skills live under this directory

# A model must be set: without model here, FW_MODEL, or the model of a
# profile, fw refuses every run. There is no default model, and each model
# costs what its provider charges. Remove the # of one line, or write
# another provider/model selector.
# model: anthropic/claude-opus-5-5  # the credential comes from ANTHROPIC_API_KEY
# model: gemini/gemini-3.8-flash    # the credential comes from GEMINI_API_KEY
# model: openai/<model>             # the credential comes from OPENAI_API_KEY; providers.openai.baseUrl names a local server
effort: high                     # low, medium, high, xhigh, or max

tools:
  builtin: [bash, files, http, memory, recall, skill, clock, ask_user, spawn_task, await_task] # the built-in tools the model can call
  files:
    root: .                      # the files tool cannot leave this directory
  http:
    allow: []                    # URL globs the http tool can fetch, for example https://api.github.com/*
  memory:
    scope: ""                    # empty is agent here and principal in an ax Task
                                 # agent: every person who runs a profile reads its notes
                                 # principal: each person keeps their own notes in each profile
                                 # shared: every profile reads the same notes

budget:                          # limits of every run; a profile or a request can lower them
  maxTurns: 200                  # model turns
  maxTokens: 2000000             # input and output tokens in total
  deadline: 2h                   # working time; time spent waiting for a person or a follow-up does not count
  maxToolCalls: 1000             # tool calls in the whole run
  maxCallsPerTurn: 32            # tool calls in one model response
  maxRepeats: 5                  # the same tool call in a row before the run fails

compaction:                      # a long run replaces its history with a summary before the context fills
  at: 150000                     # estimated tokens that start a summary; lower it for a model with a small window, 0 is off

time:
  zone: ""                       # an IANA name such as America/Chicago; empty is the zone of this host, UTC in a sandbox
  noteAfter: 10m                 # after a gap this long, the model reads the new time before its next turn; 0 is off

agents:                          # profiles; fw run -agent <name> selects one, and agents/<name>.yaml adds one
  default:
    system: prompts/default.md   # the system prompt: the text itself, or a file next to this one
    instructions: []             # more texts that follow the system prompt
    tools: ["*"]                 # globs over the tool names the profile can call

# Who manages the agents. An admin owns every profile; local:* is the user
# that runs fw on this host (see fw whoami). A profile adds owners,
# operators, and viewers in its own access block.
# access: {admins: ["local:*", "alice@example.com"]}

# How fw serve knows who calls. Modes are tried in order: oidc accepts a
# bearer token from the issuer, and token accepts FW_API_TOKEN.
# auth:
#   mode: oidc,token
#   oidc:
#     issuer: https://idp.example.com  # its discovery document gives the keys
#     audience: [fw]                   # accepted aud claims: a client id, or the URL of this server
#     groupsClaim: groups              # the claim that group:<name> entries match

# fw serve needs an API token: export FW_API_TOKEN, or set auth.mode: none
# on a listener that nothing outside the host can reach.
`

// modelNeeded is the sentence of fw init about the model, which the
// starter fw.yaml leaves unset.
const modelNeeded = "A model must be set: without model in fw.yaml, FW_MODEL, or the model of a profile, fw refuses every run."

// cmdInit writes a starter fw.yaml, prompts/default.md, and the agents and
// skills directories into a directory. It never overwrites a file. The
// starter names no model, because no paid model is a built-in choice.
func cmdInit(_ context.Context, g *globals, args []string) int {
	fs := g.newFlagSet("init", "[<dir>]")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(pos) > 1 {
		fs.Usage()
		return exitUsage
	}
	dir := "."
	if len(pos) == 1 {
		dir = pos[0]
	}
	files := []struct {
		path, content string
	}{
		{"fw.yaml", starterConfig},
		{filepath.Join("prompts", "default.md"), fw.DefaultSystemPrompt + "\n"},
	}
	for _, f := range files {
		p := filepath.Join(dir, f.path)
		if _, err := os.Stat(p); err == nil {
			return g.fail("init", fmt.Errorf("%s exists; fw init does not overwrite it", p))
		}
	}
	for _, sub := range []string{"prompts", "agents", "skills"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return g.fail("init", err)
		}
	}
	for _, f := range files {
		p := filepath.Join(dir, f.path)
		if err := os.WriteFile(p, []byte(f.content), 0o644); err != nil {
			return g.fail("init", err)
		}
	}
	// The file must load, or the starter is wrong and the user finds out now.
	if _, err := config.Load(filepath.Join(dir, "fw.yaml")); err != nil {
		return g.fail("init", fmt.Errorf("the starter configuration does not load: %w", err))
	}
	fmt.Fprintf(g.stdout, "wrote %s\n", filepath.Join(dir, "fw.yaml"))
	fmt.Fprintf(g.stdout, "wrote %s\n", filepath.Join(dir, "prompts", "default.md"))
	fmt.Fprintf(g.stdout, "made  %s%c\n", filepath.Join(dir, "agents"), filepath.Separator)
	fmt.Fprintf(g.stdout, "made  %s%c\n", filepath.Join(dir, "skills"), filepath.Separator)
	fmt.Fprintln(g.stdout, modelNeeded)
	fmt.Fprintf(g.stdout, "next: set model in %s or export FW_MODEL, export the credential of its provider, then run fw doctor and fw run \"hello\"\n", filepath.Join(dir, "fw.yaml"))
	return exitOK
}

// otherEnvNames lists the variables that fw reads outside the
// configuration keys: the command line, the authentication token, the
// model credentials, the MCP state key, and the SPIFFE Workload API socket.
var otherEnvNames = []string{
	"FW_CONFIG", "FW_LOG", "FW_AGENT", "FW_ATESPACE", "FW_API_TOKEN", "FW_MCP_STATE_KEY",
	"ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY", "OPENAI_API_KEY", "SPIFFE_ENDPOINT_SOCKET",
}

// cmdEnv prints every configuration variable, one per line, with "=" and
// its value when it is set. A secret value is masked.
func cmdEnv(ctx context.Context, g *globals, args []string) int {
	if len(args) > 0 && args[0] == "dockerfile" {
		return cmdEnvDockerfile(ctx, g, args[1:])
	}
	fs := g.newFlagSet("env", "[dockerfile]")
	onlySet := fs.Bool("set", false, "print only the variables that are set")
	if _, err := parseArgs(fs, args); err != nil {
		return flagExit(err)
	}
	print := func(names []string) {
		for _, n := range names {
			v, ok := os.LookupEnv(n)
			switch {
			case ok && v != "":
				fmt.Fprintf(g.stdout, "%s=%s\n", n, maskEnv(n, v))
			case !*onlySet:
				fmt.Fprintln(g.stdout, n)
			}
		}
	}
	names := config.EnvNames()
	print(names)
	others := append([]string(nil), otherEnvNames...)
	sort.Strings(others)
	if !*onlySet {
		fmt.Fprintln(g.stdout, "# other variables that fw reads")
	}
	print(others)
	// A map or a named list entry, such as FW_TOOLS_MCP_GIT_ENDPOINT, is set
	// under a pattern, so it is found in the environment itself.
	known := map[string]bool{}
	for _, n := range append(names, others...) {
		known[n] = true
	}
	var entries []string
	for _, kv := range os.Environ() {
		n, v, ok := strings.Cut(kv, "=")
		if ok && v != "" && strings.HasPrefix(n, config.EnvPrefix+"_") && !known[n] {
			entries = append(entries, n)
		}
	}
	if len(entries) > 0 {
		sort.Strings(entries)
		fmt.Fprintln(g.stdout, "# map and list entries that are set")
		print(entries)
	}
	return exitOK
}

// maskEnv masks the value of a variable whose last segment names a secret.
func maskEnv(name, value string) string {
	last := name
	if i := strings.LastIndexByte(name, '_'); i >= 0 {
		last = name[i+1:]
	}
	if config.SecretKey(last) {
		return "***"
	}
	return value
}
