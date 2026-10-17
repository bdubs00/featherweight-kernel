package builtin

import (
	"os"
	"sort"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// hiddenEnv names the variables that the shell never gets, because a model
// that runs "env" would put them into a tool result and the ledger.
var hiddenEnv = map[string]bool{
	"FW_API_TOKEN":      true,
	"ANTHROPIC_API_KEY": true,
	"GEMINI_API_KEY":    true,
	"OPENAI_API_KEY":    true,
}

// hiddenNames returns the hidden variables: hiddenEnv plus the names listed
// in FW_BASH_HIDE (comma-separated).
func hiddenNames() map[string]bool {
	hide := map[string]bool{"FW_BASH_HIDE": true}
	for k := range hiddenEnv {
		hide[k] = true
	}
	for _, n := range strings.Split(os.Getenv("FW_BASH_HIDE"), ",") {
		if n = strings.TrimSpace(n); n != "" {
			hide[n] = true
		}
	}
	return hide
}

// shellEnv returns the process environment without the hidden variables.
func shellEnv() []string {
	hide := hiddenNames()
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if hide[name] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// configEnv returns the variables of tools.bash.env, sorted by name, plus
// FW_HOME set to the home directory. The configuration loader resolved the
// values already. A hidden name stays hidden, so a configuration cannot put
// the API token or a model key into the shell under its own name.
func configEnv(cfg *fw.Config) []string {
	hide := hiddenNames()
	names := make([]string, 0, len(cfg.Tools.Bash.Env))
	for name := range cfg.Tools.Bash.Env {
		if name != "" && !hide[name] && !strings.Contains(name, "=") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := make([]string, 0, len(names)+1)
	for _, name := range names {
		out = append(out, name+"="+cfg.Tools.Bash.Env[name])
	}
	home := cfg.Home
	if home == "" {
		home = fw.DefaultHome()
	}
	return append(out, "FW_HOME="+home)
}
