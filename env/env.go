package env

import (
	"context"
	"io"
	"sync"

	"github.com/bdubs00/featherweight-kernel/cli"
	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
)

func init() {
	fw.RegisterExtension(Block)
	fw.RegisterService(Start)
	fw.RegisterTool(Name, newTool)
	fw.RegisterToolEnv(toolEnv)
	fw.RegisterPromptSection(section)
	cli.RegisterDoctorCheck(doctor)
	cli.EnvDockerfile = writeDockerfile
	native.Extend(mount)
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

// Start is the fw.Service of the environment. Without the block it starts
// nothing. Otherwise it installs the environment in the background, after
// the agent is up, and tries again with a growing pause until the install
// succeeds, for example after the egress policy of the Task lands. It
// sends the state that it finds once, as fw.env.ready or fw.env.failed.
// With placement image, it only sends the state of the image. For the
// life of the process, it also sends the events that the bus refused. A
// block that does not fit stops the start.
func Start(ctx context.Context, a *fw.Agent) (io.Closer, error) {
	s, err := load(a.Config())
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, nil
	}
	in := newInstaller(a, a.Config(), s)
	in.wake = make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if s.placement != PlacementImage {
			in.watch(ctx)
		}
	}()
	// The notifier sends the state that the start finds, and it lives as
	// long as the process: an event that the bus refused, such as the
	// start event of a golden boot without network, goes out once the bus
	// takes it.
	notifier := newInstaller(a, a.Config(), s)
	notifier.wake = in.wake
	go func() {
		defer wg.Done()
		notifier.notify(ctx)
	}()
	go func() {
		wg.Wait()
		close(done)
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

// toolEnv is the fw.ToolEnvHook of the environment. Every tool process
// gets the variables of the environment. A tool process never fails
// because of an install: until an install is ready, or after it failed,
// it gets the last environment that installed and still exists, or none.
// Only the first install, before any environment exists, holds a tool
// process of a run, and fw run, which starts no service, installs there.
func toolEnv(ctx context.Context, cfg *fw.Config, env []string) ([]string, error) {
	s, err := load(cfg)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return env, nil
	}
	if fw.RunInfoFrom(ctx).RunID == "" || fw.ToolEnvNoWait(ctx) || s.placement == PlacementImage {
		return s.apply(env, s.readMarker()), nil
	}
	st := s.state()
	if st.Kind == StateReady {
		return s.apply(env, st.Marker), nil
	}
	if last := s.lastEnv(st.Marker); last != nil || st.Kind == StateFailed {
		return s.apply(env, last), nil
	}
	in := newInstaller(fw.AgentFrom(ctx), cfg, s)
	if err := in.ready(ctx); err != nil && ctx.Err() == nil {
		in.log.Info("env: a tool process starts without the environment, because its install failed", "error", firstLine(err.Error()))
	}
	return s.apply(env, s.lastEnv(s.readMarker())), nil
}
