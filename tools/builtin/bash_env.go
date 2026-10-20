package builtin

import (
	"context"
	"os"
	"sort"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// configEnv returns the variables of tools.bash.env as NAME=value, sorted
// by name. The configuration loader resolved the values already. fw.ToolEnv
// still drops a hidden name and a credential of fw, so a configuration
// cannot put the API token or a model key into the shell.
func configEnv(cfg *fw.Config) []string {
	names := make([]string, 0, len(cfg.Tools.Bash.Env))
	for name := range cfg.Tools.Bash.Env {
		if name != "" && !strings.Contains(name, "=") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, name+"="+cfg.Tools.Bash.Env[name])
	}
	return out
}

// shellEnv returns the environment of a new shell: the process environment
// and the variables of tools.bash.env through the rules of fw.ToolEnv, so
// a model that runs "env" finds no secret of fw, then FW_HOME.
func (t *bashTool) shellEnv(ctx context.Context) []string {
	env := fw.ToolEnv(ctx, t.cfg, os.Environ(), t.env)
	return append(env, "FW_HOME="+t.home)
}
