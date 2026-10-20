package env

import (
	"context"
	"io"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func init() {
	fw.RegisterExtension(Block)
	fw.RegisterService(Start)
	fw.RegisterTool(Name, newTool)
	fw.RegisterToolEnv(toolEnv)
	fw.RegisterPromptSection(section)
	fw.RegisterDefaults(func(c *fw.Config) {
		c.Tools.Builtin = append(c.Tools.Builtin, Name)
		// A change of the packages asks a person first. A profile that
		// must keep its packages denies the tool: deny: ["env: *"].
		c.Policy.Rules.Ask = append(c.Policy.Rules.Ask, AskRules...)
	})
}

// AskRules are the rules that the default policy adds: a person allows
// each add and each remove of a package. The tool env is one tool, and
// its field command names the action, so these rules use the field form
// of the policy (section 7.5 of the architecture).
var AskRules = []string{"env: command=add", "env: command=remove"}

// Start is the fw.Service of the environment. Without the block, or with
// placement image, it starts nothing. Otherwise it installs the
// environment in the background, after the agent is up, and tries again
// with a growing pause until the install succeeds, for example after the
// egress policy of the Task lands. A block that does not fit stops the
// start.
func Start(ctx context.Context, a *fw.Agent) (io.Closer, error) {
	s, err := load(a.Config())
	if err != nil {
		return nil, err
	}
	if s == nil || s.placement == PlacementImage {
		return nil, nil
	}
	in := newInstaller(a, a.Config(), s)
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		in.serve(ctx)
	}()
	return service{cancel: cancel, done: done}, nil
}

// service stops the background install and waits for it. The manager
// that runs gets killed, and the next start goes on from the cache.
type service struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (s service) Close() error {
	s.cancel()
	<-s.done
	return nil
}

// toolEnv is the fw.ToolEnvHook of the environment. A tool process of a
// run waits until the environment is ready, and fails with the reason
// when its install failed. Every tool process gets the variables of the
// environment.
func toolEnv(ctx context.Context, cfg *fw.Config, env []string) ([]string, error) {
	s, err := load(cfg)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return env, nil
	}
	if fw.RunInfoFrom(ctx).RunID != "" {
		if err := newInstaller(fw.AgentFrom(ctx), cfg, s).ready(ctx); err != nil {
			return nil, err
		}
	}
	return s.apply(env, s.readMarker()), nil
}
