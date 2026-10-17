package ax

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

const (
	// Listen is the address that ax probes and that the atenet router reaches.
	Listen = ":80"
	// DefaultHome is the fw home on the durable volume.
	DefaultHome = "/workspace/.fw"
	// BootstrapProfile is the name of the profile that runs a binding goal.
	BootstrapProfile = "bootstrap"
	// DefaultBootstrapTimeout is the time limit of a goal when
	// AX_BOOTSTRAP_TIMEOUT is not set.
	DefaultBootstrapTimeout = 10 * time.Minute
	// EnvAtespace carries the atespace of the Task to the kernel.
	EnvAtespace = "FW_ATESPACE"
)

// BootstrapSystem is the system prompt of the bootstrap profile.
const BootstrapSystem = `You prepare a workspace directory for the goal in the message. The message names the directory.
Run every command from that directory, and change files only inside it.
Install the tools and dependencies that the goal needs, then check that the goal works.
Do not push, publish, deploy, or change anything outside the directory.
When you finish, report in a few lines what you installed, what you ran, and what failed.`

// BootstrapTimeout returns the duration in AX_BOOTSTRAP_TIMEOUT, or ten
// minutes when the variable is unset or not a positive Go duration.
func BootstrapTimeout() time.Duration {
	raw := os.Getenv(EnvBootstrapTimeout)
	if raw == "" {
		return DefaultBootstrapTimeout
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		slog.Warn("ax: invalid bootstrap timeout; using the default", "env", EnvBootstrapTimeout, "value", raw, "default", DefaultBootstrapTimeout)
		return DefaultBootstrapTimeout
	}
	return d
}

// ApplyToConfig merges the Task and its Workspaces into cfg:
//
//   - Listen is ":80".
//   - Home is /workspace/.fw when it is empty.
//   - Tools.Files.Root is the first binding path when it is empty.
//   - Every spec.mcp.servers entry joins Tools.MCP unless an entry with the
//     same name exists. The fw.yaml entry wins. spec.mcp.registries entries
//     are logged and skipped.
//   - A "bootstrap" profile joins Agents unless one exists. Its empty
//     access block shows it to access.admins only.
//   - FW_ATESPACE is set to the atespace of the Task when it is unset.
//   - Node is set to "ax:<atespace>/<task>" when it is empty, so every
//     ledger event names this Task.
//
// The caller sets Listen again after this call to honor a --port flag.
func ApplyToConfig(cfg *fw.Config, task *Task, bindings []Binding) {
	log := slog.Default()
	cfg.Listen = Listen
	if cfg.Home == "" {
		cfg.Home = DefaultHome
	}
	if cfg.Tools.Files.Root == "" {
		cfg.Tools.Files.Root = Workdir(bindings)
	}

	have := make(map[string]bool, len(cfg.Tools.MCP))
	for _, s := range cfg.Tools.MCP {
		have[s.Name] = true
	}
	for _, b := range bindings {
		ws := b.Workspace
		if ws == nil {
			continue
		}
		for _, s := range ws.Spec.MCP.Servers {
			if s.Name == "" {
				log.Warn("ax: workspace MCP server has no name; skipping", "workspace", ws.Metadata.Name)
				continue
			}
			if have[s.Name] {
				log.Info("ax: fw.yaml defines the workspace MCP server; keeping fw.yaml", "workspace", ws.Metadata.Name, "server", s.Name)
				continue
			}
			have[s.Name] = true
			cfg.Tools.MCP = append(cfg.Tools.MCP, fw.MCPServerConfig{
				Name:     s.Name,
				Endpoint: s.Endpoint,
				Command:  s.Command,
				Args:     slices.Clone(s.Args),
			})
		}
		for _, r := range ws.Spec.MCP.Registries {
			log.Warn("ax: MCP registries are not supported; skipping", "workspace", ws.Metadata.Name, "provider", r.Provider, "project", r.Project, "query", r.Query)
		}
		for _, r := range ws.Spec.Skills.Registries {
			log.Warn("ax: skill registries are not supported; skipping", "workspace", ws.Metadata.Name, "provider", r.Provider, "project", r.Project, "query", r.Query)
		}
	}

	if cfg.Agents == nil {
		cfg.Agents = map[string]fw.AgentProfile{}
	}
	if _, ok := cfg.Agents[BootstrapProfile]; !ok {
		cfg.Agents[BootstrapProfile] = fw.AgentProfile{
			Name:   BootstrapProfile,
			System: BootstrapSystem,
			Tools:  []string{"bash", "files"},
			Budget: &fw.Budget{Deadline: BootstrapTimeout()},
			// The process runs this profile itself. An access block with no
			// entry keeps it out of every list but the one of the admins, so
			// nobody starts it by mistake from the dashboard.
			Access: &fw.Access{},
		}
	}

	atespace, name := Identity(task)
	if atespace != "" {
		if _, set := os.LookupEnv(EnvAtespace); !set {
			_ = os.Setenv(EnvAtespace, atespace)
		}
	}
	if cfg.Node == "" && atespace != "" && name != "" {
		cfg.Node = "ax:" + atespace + "/" + name
	}
}

// BootstrapInput returns the run input for a goal in dir.
func BootstrapInput(goal, dir string) string {
	return fmt.Sprintf("Prepare the workspace at %s for this goal:\n\n%s", dir, goal)
}

// Bootstrap runs the bootstrap profile with a binding goal, under a time
// limit, and logs the outcome. A timeout of zero or less means
// BootstrapTimeout(). The run needs a model credential: when the agent has
// none, the caller skips Bootstrap and logs a warning, as the stock runner
// does. Bootstrap does not check for a credential itself.
//
// Bootstrap returns an error when the run cannot start or ends in a state
// other than completed. A run that waits for an approval stays in the ledger,
// and a decision through any surface continues it.
func Bootstrap(ctx context.Context, a *fw.Agent, goal, dir string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = BootstrapTimeout()
	}
	log := a.Logger().With("dir", dir, "timeout", timeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	log.Info("ax: bootstrap started")
	start := time.Now()
	r, err := a.Run(ctx, BootstrapInput(goal, dir), fw.AsAgent(BootstrapProfile), fw.WithOrigin("cli"))
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			log.Warn("ax: bootstrap timed out", "error", err)
		} else {
			log.Warn("ax: bootstrap failed", "error", err)
		}
		return fmt.Errorf("ax: bootstrap: %w", err)
	}
	log = log.With("run", r.ID, "state", r.State, "elapsed", time.Since(start).Round(time.Second))
	switch r.State {
	case fw.StateCompleted:
		log.Info("ax: bootstrap completed")
		return nil
	case fw.StateInputRequired:
		pending := ""
		if r.Pending != nil {
			pending = r.Pending.ID
		}
		log.Warn("ax: bootstrap waits for an approval; continuing without it", "approval", pending)
		return fmt.Errorf("ax: bootstrap run %s waits for approval %s", r.ID, pending)
	default:
		log.Warn("ax: bootstrap did not complete", "error", r.Error)
		return fmt.Errorf("ax: bootstrap run %s ended %s: %s", r.ID, r.State, r.Error)
	}
}
