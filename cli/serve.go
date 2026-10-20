package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bdubs00/featherweight-kernel/ax"
	"github.com/bdubs00/featherweight-kernel/ax/guest"
	"github.com/bdubs00/featherweight-kernel/config"
	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/axmeta"
)

// serveArgs holds the flags of fw serve. The manifest flags match the
// flags of the stock ax runner.
type serveArgs struct {
	taskFile       string
	workspaceFiles stringList
	port           int
	metadataPort   int
}

// stringList is a flag that can repeat.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// cmdServe serves every enabled surface. Under ax, or with a manifest flag,
// it runs the ax boot sequence instead.
func cmdServe(ctx context.Context, g *globals, args []string) int {
	var sa serveArgs
	fs := g.newFlagSet("serve", "")
	fs.StringVar(&sa.taskFile, "task-file", "", "read the ax Task from this `file` instead of AX_TASK_YAML")
	fs.Var(&sa.workspaceFiles, "workspace-file", "read ax Workspaces from this `file` instead of AX_WORKSPACES_YAML; repeat for more files")
	fs.IntVar(&sa.port, "port", 0, "listen on this `port` on every interface; wins over --listen")
	fs.IntVar(&sa.metadataPort, "metadata-port", axmeta.DefaultPort, "serve the ax metadata endpoints on this loopback `port`; 0 picks a free port")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(pos) > 0 {
		fs.Usage()
		return exitUsage
	}
	if ax.Detect() || sa.taskFile != "" || len(sa.workspaceFiles) > 0 {
		return serveAX(ctx, g, sa)
	}

	// Plain mode: load, build, serve. The reaper works only as PID 1, which
	// fw also is in a plain container.
	go ax.ReapZombies(ctx)
	cfg, err := g.loadConfig()
	if err != nil {
		return g.fail("serve", err)
	}
	if sa.port > 0 {
		cfg.Listen = ":" + strconv.Itoa(sa.port)
	}
	a, err := newAgent(ctx, cfg, modeServe)
	if err != nil {
		return g.fail("serve", err)
	}
	a.SetReady(true)
	err = a.Serve(ctx)
	closeAgent(a)
	if err != nil {
		return g.fail("serve", err)
	}
	return exitOK
}

// switchHandler serves the handler that was set last. The ax boot swaps
// the boot handler for the agent handler without closing the listener.
type switchHandler struct {
	h atomic.Pointer[http.Handler]
}

func newSwitchHandler(h http.Handler) *switchHandler {
	s := &switchHandler{}
	s.Set(h)
	return s
}

// Set replaces the handler.
func (s *switchHandler) Set(h http.Handler) { s.h.Store(&h) }

// ServeHTTP implements http.Handler.
func (s *switchHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	(*s.h.Load()).ServeHTTP(w, r)
}

// bootHandler answers while the workspaces are prepared: /healthz is 200,
// /readyz is 503, and every other path is 503.
func bootHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
			return
		}
		w.Header().Set("Retry-After", "5")
		if r.URL.Path == "/readyz" {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "fw is preparing the workspaces", http.StatusServiceUnavailable)
	})
}

// newHTTPServer returns a server with HTTP/1.1 and unencrypted HTTP/2 on
// one port, as fw.Agent.ServeListener builds it.
func newHTTPServer(ctx context.Context, h http.Handler) *http.Server {
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	return &http.Server{
		Handler:           h,
		Protocols:         &protocols,
		ReadHeaderTimeout: 30 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
}

// axBoot is the state of the ax boot sequence.
type axBoot struct {
	task       *ax.Task
	workspaces []*ax.Workspace
	bindings   []ax.Binding
	results    []ax.PrepareResult
	agent      *fw.Agent
}

// serveAX runs the boot sequence of docs/ARCHITECTURE.md section 11.3:
//
//  1. Start the zombie reaper, which works only as PID 1, and read the
//     manifests.
//  2. Stash the manifests for the metadata endpoints. Listen on :80 (or
//     --listen, or --port) at once with the boot handler, and serve the ax
//     metadata endpoints on 127.0.0.1:8099 (or --metadata-port). If the
//     Task sets spec.debug, the guest services of ax ssh answer from this
//     point on, so that a stuck boot can be inspected.
//  3. Compute the bindings and prepare each workspace.
//  4. Load the configuration, which a Workspace file can ship at
//     /workspace/.fw/fw.yaml, and merge the Task into it.
//  5. Build the agent, which resumes unfinished runs, and run each binding
//     goal with the bootstrap profile when a model is available.
//  6. Set ready and swap in the agent handler.
//  7. Run spec.command: none serves, "fw run" and "fw serve" run in
//     process, and anything else runs as a supervised child.
//  8. When ctx ends, shut the server down, close the agent, and exit 0.
func serveAX(ctx context.Context, g *globals, sa serveArgs) int {
	go ax.ReapZombies(ctx)

	b := &axBoot{}
	var err error
	if sa.taskFile != "" || len(sa.workspaceFiles) > 0 {
		b.task, b.workspaces, err = ax.LoadFromFiles(sa.taskFile, sa.workspaceFiles)
	} else {
		b.task, b.workspaces, err = ax.LoadFromEnv()
	}
	if err != nil {
		return g.fail("serve", err)
	}
	b.bindings = ax.Bindings(b.task, b.workspaces)
	ax.Stash(b.task, b.workspaces)

	addr := ax.Listen
	if g.listen != "" {
		addr = g.listen
	}
	if sa.port > 0 {
		addr = ":" + strconv.Itoa(sa.port)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return g.fail("serve", fmt.Errorf("listen %s: %w", addr, err))
	}
	meta, err := axmeta.Listen(ctx, sa.metadataPort)
	if err != nil {
		_ = ln.Close()
		return g.fail("serve", err)
	}
	gs := startGuest(b, meta.URL())
	sw := newSwitchHandler(bootHandler())
	srv := newHTTPServer(ctx, gs.Wrap(sw))
	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.Serve(ln) }()
	slog.Info("fw: ax boot: listening", "addr", ln.Addr().String(), "metadata", meta.URL())

	shutdown := func(a *fw.Agent) {
		sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		// A call that follows a process keeps its connection busy, so the
		// guest services stop before the server.
		gs.Close()
		_ = srv.Shutdown(sctx)
		_ = meta.Close(sctx)
		if a != nil {
			a.Close(sctx)
		}
	}

	if err := g.bootAX(ctx, b, addr); err != nil {
		shutdown(nil)
		return g.fail("serve", err)
	}
	a := b.agent
	if ctx.Err() != nil {
		// A signal arrived during the boot: do not start the task command.
		shutdown(a)
		return exitOK
	}
	h, err := a.Handler()
	if err != nil {
		shutdown(a)
		return g.fail("serve", err)
	}
	a.SetReady(true)
	sw.Set(h)
	slog.Info("fw: ax boot: ready")

	cmdCtx, stopCmd := context.WithCancel(ctx)
	cmdDone := make(chan struct{})
	go func() {
		defer close(cmdDone)
		g.runTaskCommand(cmdCtx, a, b, meta.URL())
	}()

	code := exitOK
	select {
	case <-ctx.Done():
	case err := <-srvErr:
		if !errors.Is(err, http.ErrServerClosed) {
			slog.Error("fw: serve", "error", err)
			code = exitFailed
		}
	}
	stopCmd()
	shutdown(a)
	<-cmdDone // a supervised child gets SIGTERM, then SIGKILL after its grace period
	return code
}

// startGuest starts the guest services of ax ssh when the Task sets
// spec.debug, and returns nil otherwise. A failure leaves them off and does
// not stop the boot, as in the stock runner.
func startGuest(b *axBoot, metadataURL string) *guest.Server {
	gs, err := guest.New(b.task, guest.Options{Workspace: ax.Workdir(b.bindings), MetadataURL: metadataURL})
	switch {
	case err != nil:
		slog.Error("fw: ax boot: guest services are off", "error", err)
	case gs != nil:
		slog.Warn("fw: ax boot: spec.debug is true, so the guest services of ax ssh are on. Any caller that reaches the listener can run a process in the sandbox.")
	}
	return gs
}

// bootAX runs steps 3 to 5 of the boot sequence on the manifests in b.
func (g *globals) bootAX(ctx context.Context, b *axBoot, listen string) error {
	log := slog.Default()
	var err error
	home := firstNonEmpty(g.home, ax.DefaultHome)
	b.results, err = ax.Prepare(ctx, b.bindings, home, log)
	if err != nil {
		// Prepare still prepared the other bindings; serve what is there.
		log.Error("fw: ax boot: prepare workspaces", "error", err)
	}
	for _, r := range b.results {
		if r.FirstRun {
			// The new files belong to fw, so the tool user gets the
			// workspace again (docs/ARCHITECTURE.md section 7.12).
			if err := os.Remove(fw.ToolTreeMarker(home)); err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Warn("fw: ax boot: remove the marker of the tool user", "error", err)
			}
			break
		}
	}

	path := g.config
	if path == "" {
		path = config.FindIn(home)
	}
	// ApplyToConfig below adds the MCP servers of the Workspace to
	// tools.mcp. policy.external.mcp can name one of them, so the load
	// skips that check, and CheckControlServers runs after the merge.
	cfg, err := config.LoadBeforeMerge(path, func(c *fw.Config) {
		g.override(c)
		if c.Home == "" {
			c.Home = home
		}
	})
	if err != nil {
		return err
	}
	// ApplyToConfig also sets FW_ATESPACE from the Task when it is unset.
	ax.ApplyToConfig(cfg, b.task, b.bindings)
	if err := config.CheckControlServers(cfg); err != nil {
		return err
	}
	cfg.Listen = listen
	cfg.Version = Version
	g.setLogger(cfg.Telemetry.Log)
	log = slog.Default() // the configuration can change the level
	if path != "" {
		log.Info("fw: ax boot: configuration loaded", "path", path)
	}
	b.agent, err = newAgent(ctx, cfg, modeServe)
	if err != nil {
		return err
	}
	for _, r := range b.results {
		if r.Goal == "" {
			continue
		}
		if b.agent.Model() == nil {
			log.Warn("fw: ax boot: no model credential; skipping the workspace goal", "workspace", r.Name, "path", r.Path)
			continue
		}
		// Bootstrap logs its outcome, and a failed goal does not stop the boot.
		_ = ax.Bootstrap(ctx, b.agent, r.Goal, r.Path, ax.BootstrapTimeout())
	}
	return nil
}

// runTaskCommand runs step 7 of the boot sequence and returns when the
// command ends. The caller keeps serving after that, as the stock runner
// does.
func (g *globals) runTaskCommand(ctx context.Context, a *fw.Agent, b *axBoot, metadataURL string) {
	log := a.Logger()
	var argv []string
	if b.task != nil {
		argv = b.task.Spec.Command
	}
	switch {
	case len(argv) == 0:
		if goal := os.Getenv("FW_GOAL"); strings.TrimSpace(goal) != "" {
			g.runInProcess(ctx, a, runArgs{input: goal, agent: os.Getenv("FW_AGENT")}, "ax-goal")
		}
	case filepath.Base(argv[0]) == "fw":
		verb := ""
		if len(argv) > 1 {
			verb = argv[1]
		}
		switch verb {
		case "", "serve":
			// The agent serves already.
		case "run":
			ra, err := g.parseRunArgs(argv[2:])
			if err != nil {
				if !errors.Is(err, flag.ErrHelp) {
					log.Error("fw: ax: spec.command: bad fw run arguments", "error", err)
				}
				return
			}
			g.runInProcess(ctx, a, ra, "ax-command")
		default:
			log.Warn("fw: ax: spec.command names a verb that does not run in process; serving", "verb", verb)
		}
	default:
		env := []string{ax.EnvMetadataURL + "=" + metadataURL}
		code, err := ax.Supervise(ctx, argv, ax.Workdir(b.bindings), env, log)
		if err != nil {
			log.Error("fw: ax: task command", "error", err)
			return
		}
		log.Info("fw: ax: task command finished; still serving", "exitCode", code)
	}
}

// runInProcess runs one input on the serving agent and prints the output.
// The idempotency key holds a hash of the input, so a container that ax
// resumes follows the same run instead of starting it again. Approvals
// wait for a decision through a surface.
func (g *globals) runInProcess(ctx context.Context, a *fw.Agent, ra runArgs, keyPrefix string) {
	log := a.Logger()
	sum := sha256.Sum256([]byte(ra.agent + "\x00" + ra.input))
	key := keyPrefix + "-" + hex.EncodeToString(sum[:8])
	r, err := a.Start(ctx, ra.input, fw.AsAgent(ra.agent), fw.WithOrigin(fw.OriginCLI), fw.WithIdempotencyKey(key))
	if err != nil {
		log.Error("fw: ax: start run", "error", err)
		return
	}
	final, err := followRun(ctx, a, r.ID, g.stderr, nil, false)
	if err != nil {
		log.Error("fw: ax: follow run", "run", r.ID, "error", err)
		return
	}
	if ctx.Err() != nil {
		return // the kernel suspends the run at shutdown, and the next boot resumes it
	}
	if err := g.printRun(final, ra.json); err != nil {
		log.Error("fw: ax: print run", "run", r.ID, "error", err)
	}
	log.Info("fw: ax: run finished; still serving", "run", final.ID, "state", final.State)
}
