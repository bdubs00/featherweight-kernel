// Package harness hosts a third-party agent harness, opencode or Claude
// Code, inside a run of the kernel (docs/ARCHITECTURE.md section 19).
//
// The block harness of fw.yaml turns it on. Every run of the process then
// becomes a session of the harness instead of a run of the turn loop. The
// kernel keeps what makes a run durable: one ledger, one policy, one
// approval surface, and an idle run while a person decides. The harness
// asks the kernel about each tool call. The kernel answers from the policy
// of the run, and for a call that the policy asks a person about, it
// records fw.approval.requested and lets the harness wait or exit. After
// the decision, possibly after a cold boot, the kernel continues the same
// session of the harness, whose home is on the durable volume.
//
// The package registers itself in init: it claims the block harness, it
// gives the agent a fw.Runner, it stops the harness processes as a
// fw.Service, and it serves the hook route of Claude Code and the memory
// route of both kinds on the native API (memory.go).
package harness

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/memory"
	"github.com/bdubs00/featherweight-kernel/surface/native"
)

// The kinds of harness.
const (
	KindOpencode   = "opencode"
	KindClaudeCode = "claude-code"
)

// EvEvent is the type of the ledger event that holds one event of the
// harness, or a step of the kernel in the session (EventData).
const EvEvent = "fw.harness.event"

// Config is the block harness of fw.yaml.
//
// Kind names the harness. Home is the home directory of the harness: its
// HOME, and for Claude Code also CLAUDE_CONFIG_DIR, where the sessions,
// the transcripts, and the credentials of the harness live. It must be on
// the durable volume, so a session continues after a cold boot. The
// default is the directory .harness beside the home of fw, which is
// /workspace/.harness under ax. Command is the program of the harness and
// its first arguments; the kernel adds its own flags after them. Port is
// the loopback port of opencode serve; 0 picks a free port.
type Config struct {
	Kind    string   `yaml:"kind" json:"kind"`
	Home    string   `yaml:"home,omitempty" json:"home,omitempty"`
	Command []string `yaml:"command,omitempty" json:"command,omitempty"`
	Port    int      `yaml:"port,omitempty" json:"port,omitempty"`
}

func init() {
	fw.RegisterExtension("harness")
	fw.RegisterRunner(newRunner)
	fw.RegisterService(service)
	native.Extend(extend)
}

// driver is one kind of harness.
type driver interface {
	// drive does the work of the run that is left (fw.Runner).
	drive(ctx context.Context, r *run) error
	// canceled stops what the harness does for a run that was canceled
	// while no driver worked on it, such as a run that waited for a
	// person. It must return at once.
	canceled(id string)
	io.Closer
}

// host is the Runner of one agent. It holds the state of the harness in
// memory: the processes and the sessions that run now. The ledger holds
// everything that a cold boot needs.
type host struct {
	a    *fw.Agent
	cfg  Config
	home string
	log  *slog.Logger
	drv  driver
	// tick is the period of the checks of the budget while a driver
	// follows the harness, also when the harness sends nothing. quiet is
	// the longest time without an event of the harness, after which the
	// driver counts the harness as lost.
	tick, quiet time.Duration
	// private holds the paths that no tool call of a harness reaches: the
	// home of the harness, the home of fw, and the reserved paths, also
	// with their links resolved.
	private []string
	// mem is the memory of the agent, or nil when tools.builtin names
	// neither memory nor recall. memHTTP serves the MCP server of the
	// memory route (memory.go).
	mem     *memory.Store
	memHTTP http.Handler

	// noServerOnce logs once that the memory server cannot be served
	// (host.noServer).
	noServerOnce sync.Once

	mu       sync.Mutex
	hooks    map[string]*claudeProc // the running Claude Code processes, by the secret of their hook URL
	memBinds map[string]*memBinding // the memory servers of the processes of the harness, by the secret of their URL
	mounts   int                    // the native surfaces that serve the routes of the host
	serving  bool                   // the process runs its services, as fw serve does (service)
}

// newRunner builds the host when the configuration has the block harness.
func newRunner(_ context.Context, a *fw.Agent) (fw.Runner, error) {
	var c Config
	ok, err := a.Config().Extension("harness", &c)
	if err != nil {
		return nil, fmt.Errorf("harness: %w", err)
	}
	if !ok {
		return nil, nil
	}
	h, err := newHost(a, c)
	if err != nil {
		return nil, err
	}
	return h, nil
}

func newHost(a *fw.Agent, c Config) (*host, error) {
	c.Kind = strings.TrimSpace(c.Kind)
	if c.Port < 0 || c.Port > 65535 {
		return nil, fmt.Errorf("harness: port %d is not a TCP port; set harness.port to 0 or a port", c.Port)
	}
	home, err := homeOf(a.Config(), c.Home)
	if err != nil {
		return nil, err
	}
	h := &host{a: a, cfg: c, home: home, log: a.Logger().With("harness", c.Kind), hooks: map[string]*claudeProc{}, memBinds: map[string]*memBinding{}, tick: checkEvery}
	switch c.Kind {
	case KindOpencode:
		h.drv, h.quiet = newOpencode(h), opencodeQuiet
	case KindClaudeCode:
		if c.Port != 0 {
			return nil, errors.New("harness: port is a setting of opencode; remove harness.port for claude-code")
		}
		h.drv, h.quiet = &claude{h: h}, claudeQuiet
	case "":
		return nil, errors.New("harness: the block harness needs kind: opencode or claude-code")
	default:
		return nil, fmt.Errorf("harness: unknown kind %q; use opencode or claude-code", c.Kind)
	}
	for _, p := range append([]string{home, a.Config().Home}, a.Config().Reserved()...) {
		if abs, err := filepath.Abs(p); err == nil && p != "" {
			h.private = append(h.private, abs)
			if real := realPath(abs); real != abs {
				h.private = append(h.private, real)
			}
		}
	}
	if cfg := a.Config(); builtin(cfg, "memory") || builtin(cfg, "recall") {
		// The turn loop goes on without a tool that it cannot build, and
		// so does the harness without the memory.
		store, err := memory.New(cfg)
		if err != nil {
			h.log.Warn("harness: the harness gets no memory", "error", err)
		} else {
			h.mem, h.memHTTP = store, newMemHandler()
			store.GuardToken(a)
		}
	}
	a.Observe(fw.ObserverFunc(h.observe))
	return h, nil
}

// The checks while a driver follows the harness (host.tick, host.quiet).
// opencode sends a heartbeat on its event stream every 30 seconds, so five
// minutes without an event means a lost server or stream. Claude writes
// nothing while a tool call runs, and a call of the Bash tool can take ten
// minutes, so its bound is longer.
const (
	checkEvery    = 15 * time.Second
	opencodeQuiet = 5 * time.Minute
	claudeQuiet   = 30 * time.Minute
)

// observe tells the driver about a run that was canceled, and removes the
// armed memory calls of a run that finished, so an armed call never
// outlives its run.
func (h *host) observe(e fw.Event) {
	if e.Type != fw.EvRunFinished {
		return
	}
	h.dropArms(e.Subject)
	var d fw.RunFinishedData
	if e.Decode(&d) == nil && d.Outcome == fw.OutcomeCanceled {
		h.drv.canceled(e.Subject)
	}
}

// homeOf resolves and checks the home of the harness. The home is a
// sibling of the home of fw by default. It must not be inside the home of
// fw, which the tool user cannot reach, and not inside a directory of a
// run, which belongs to one run.
func homeOf(cfg *fw.Config, home string) (string, error) {
	fwHome, err := filepath.Abs(cfg.Home)
	if err != nil {
		return "", fmt.Errorf("harness: the home of fw: %w", err)
	}
	if strings.TrimSpace(home) == "" {
		home = filepath.Join(filepath.Dir(fwHome), ".harness")
	}
	abs, err := filepath.Abs(home)
	if err != nil {
		return "", fmt.Errorf("harness: home %q: %w", home, err)
	}
	if within(abs, fwHome) {
		return "", fmt.Errorf("harness: home %s is inside the home of fw (%s); put it beside it, such as %s", abs, fwHome, filepath.Join(filepath.Dir(fwHome), ".harness"))
	}
	for _, r := range cfg.Reserved() {
		if r, err := filepath.Abs(r); err == nil && within(abs, r) {
			return "", fmt.Errorf("harness: home %s is inside %s, which fw keeps for itself", abs, r)
		}
	}
	if cfg.Tools.Files.PerRunOn() {
		root, err := filepath.Abs(cfg.WorkspaceRoot())
		if err == nil && within(abs, filepath.Join(root, "runs")) {
			return "", fmt.Errorf("harness: home %s is inside the directories of the runs (%s); a session must outlive one run", abs, filepath.Join(root, "runs"))
		}
	}
	return abs, nil
}

// within reports whether p is dir or inside it.
func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// Drive implements fw.Runner.
func (h *host) Drive(ctx context.Context, hr *fw.Hosted) (bool, error) {
	st := scan(hr.Events())
	if st.started && !st.hosted {
		// The turn loop started this run before the block harness was
		// set; it keeps the run.
		return false, nil
	}
	r := &run{h: h, hr: hr, st: st}
	if !st.started {
		if _, err := hr.Emit(ctx, fw.EvRunStarted, fw.RunStartedData{Model: hr.Model()}); err != nil {
			return true, err
		}
		if err := r.mark(ctx, EventData{Kind: KindStart}); err != nil {
			return true, err
		}
		r.rescan()
	}
	if q := hr.Queued(); !r.st.terminal && q.Metadata[fw.MetaForkOf] != "" && r.st.session != "" && r.st.sessSeq <= q.Kept {
		// The branch kept the session of its source. The harness holds one
		// session for both runs, so the branch would drive the session of
		// the source.
		return true, r.fail(ctx, errBranch)
	}
	// A run that waits has nothing to do before the decision. A finished
	// run has nothing left: a follow-up reopens it, so the run is then no
	// longer terminal. A run that failed before the harness got its input
	// stays finished.
	if r.st.pending != nil || r.st.terminal {
		return true, nil
	}
	if err := h.prepare(); err != nil {
		return true, r.fail(ctx, err.Error())
	}
	if abs, _, err := hr.Dir(); err == nil {
		r.dir = abs
	}
	return true, h.drv.drive(ctx, r)
}

// errBranch is the error of a branch of a run whose session started.
const errBranch = "harness: a branch of a hosted run is not supported"

// prepare creates the home of the harness for the tool user.
func (h *host) prepare() error {
	if err := os.MkdirAll(h.home, 0o700); err != nil {
		return fmt.Errorf("harness: create the home %s: %w", h.home, err)
	}
	if u := h.a.ToolUser(); u != nil {
		if err := os.Chown(h.home, int(u.UID), int(u.GID)); err != nil {
			return fmt.Errorf("harness: give the home %s to the tool user %s: %w", h.home, u, err)
		}
	}
	return nil
}

// Close stops every process of the harness and closes the memory.
func (h *host) Close() error {
	err := h.drv.Close()
	if h.mem != nil {
		if cerr := h.mem.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("harness: close the memory: %w", cerr)
		}
	}
	return err
}

// service stops the processes of the harness when the agent shuts down.
// Only a process that serves, such as fw serve, runs the services, so the
// host then knows that the native API can mount its routes (memBase).
func service(_ context.Context, a *fw.Agent) (io.Closer, error) {
	h, ok := a.Runner().(*host)
	if !ok {
		return nil, nil
	}
	h.mu.Lock()
	h.serving = true
	h.mu.Unlock()
	h.log.Info("harness on", "home", h.home)
	return h, nil
}

// extend mounts the hook route of Claude Code and the memory route of
// both kinds on the native API. The memory route names its methods, so it
// cannot clash with a route of another extension for every path, such as
// GET /. A stateless MCP server answers 405 to GET and DELETE.
func extend(k *native.Kit) error {
	h, ok := k.Agent().Runner().(*host)
	if !ok || (h.cfg.Kind != KindClaudeCode && h.mem == nil) {
		return nil
	}
	if h.cfg.Kind == KindClaudeCode {
		k.HandlePublic("POST "+hookPath+"{secret}", h.hook)
	}
	if h.mem != nil {
		for _, m := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
			k.HandlePublic(m+" "+memPath+"{secret}", h.memRoute)
		}
	}
	h.mu.Lock()
	h.mounts++
	h.mu.Unlock()
	return nil
}

// secret returns a new random secret of 32 bytes in hex.
func secret() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
