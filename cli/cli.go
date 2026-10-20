// Package cli holds the verbs of the fw command line. The main package
// links in the drivers and passes the arguments to Run. See
// docs/ARCHITECTURE.md section 12.9.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/ax"
	"github.com/bdubs00/featherweight-kernel/config"
	"github.com/bdubs00/featherweight-kernel/fw"
)

// Build information. The main package sets it from its link flags.
var (
	Version = "dev"
	Commit  = ""
)

// Functions of packages that the binary links in. The main package sets
// them in drivers.go, so this package builds and tests without those
// packages. A nil function disables what needs it.
var (
	// NewLogger returns the process logger: JSON lines on standard error.
	NewLogger func(level string) *slog.Logger
	// SetBuildInfo records the version for metrics and traces.
	SetBuildInfo func(version, commit string)
	// ServeMCPStdio serves the MCP server over standard input and output.
	ServeMCPStdio func(ctx context.Context, a *fw.Agent) error
	// CardJSON returns the A2A agent card.
	CardJSON func(a *fw.Agent) ([]byte, error)
)

// A DoctorCheck is one line of fw doctor from a linked package. It reports
// whether the check passed and the line, without the ok or FAIL mark.
// Secrets are the secrets of the process, and timeout bounds its work, as
// it bounds the checks of the kernel.
type DoctorCheck func(ctx context.Context, cfg *fw.Config, secrets fw.Secrets, timeout time.Duration) (ok bool, line string)

var doctorChecks struct {
	mu   sync.Mutex
	list []DoctorCheck
}

// RegisterDoctorCheck adds a check that fw doctor runs after the checks of
// the kernel. A package calls it in init, or a binary in its own init.
func RegisterDoctorCheck(c DoctorCheck) {
	doctorChecks.mu.Lock()
	defer doctorChecks.mu.Unlock()
	doctorChecks.list = append(doctorChecks.list, c)
}

func registeredDoctorChecks() []DoctorCheck {
	doctorChecks.mu.Lock()
	defer doctorChecks.mu.Unlock()
	return append([]DoctorCheck(nil), doctorChecks.list...)
}

// AgentsPut and AgentsDelete create, replace, and remove an agent profile
// for the verb agents. The kernel writes no profile, so they are nil in fw,
// and the verb says so. The workbench sets them.
var (
	AgentsPut    func(ctx context.Context, a *fw.Agent, p fw.AgentProfile) error
	AgentsDelete func(ctx context.Context, a *fw.Agent, name string) error
)

// AgentOptions returns options that every verb adds last when it builds an
// agent. Tests set it to inject a fake model. It is nil in the binary.
var AgentOptions func() []fw.Option

// Exit codes.
const (
	exitOK       = 0
	exitFailed   = 1 // an error, or a run that failed
	exitCanceled = 2 // a canceled run
	exitUsage    = 2 // a bad command line
)

// shutdownTimeout bounds the drain of the kernel at exit, as in
// fw.Agent.ServeListener.
const shutdownTimeout = 8 * time.Second

// globals holds the global flags and the standard streams.
type globals struct {
	config string
	log    string
	home   string
	listen string

	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer

	level string // the level of the current default logger
}

func (g *globals) addFlags(fs *flag.FlagSet) {
	fs.StringVar(&g.config, "config", g.config, "the configuration `file` (default: $FW_CONFIG, ./fw.yaml, or $FW_HOME/fw.yaml)")
	fs.StringVar(&g.log, "log", g.log, "the log `level`: debug, info, warn, or error")
	fs.StringVar(&g.home, "home", g.home, "the fw home `dir` (default: $FW_HOME, /workspace/.fw, or ./.fw)")
	fs.StringVar(&g.listen, "listen", g.listen, "the listen `address`, for example :8080")
}

type verb struct {
	name    string
	summary string
	run     func(ctx context.Context, g *globals, args []string) int
}

func verbs() []verb {
	return []verb{
		{"run", `run "<input>" once and print the output`, cmdRun},
		{"serve", "serve every enabled surface on one listener", cmdServe},
		{"mcp", "serve the MCP server over standard input and output, as the local principal", cmdMCP},
		{"init", "write a starter fw.yaml, a default prompt, and the agents and skills directories", cmdInit},
		{"doctor", "check the configuration, the identity, the credential, the model, the tool sources, the bus, the ledger, and the skills", cmdDoctor},
		{"agents", "list the agent profiles, print one, or put and delete one", cmdAgents},
		{"whoami", "print the local principal that the commands act as", cmdWhoami},
		{"tools", "list the tools of every source", cmdTools},
		{"skills", "list the skills, or print one skill", cmdSkills},
		{"events", "print or follow the ledger of a run", cmdEvents},
		{"replay", "replay a recorded run against a model", cmdReplay},
		{"card", "print the A2A agent card", cmdCard},
		{"config", "print the resolved configuration with secrets masked", cmdConfig},
		{"env", "print every configuration variable, with its value when it is set", cmdEnv},
		{"ax", "render, start, inspect, and delete one ax Task for a profile file", cmdAX},
		{"version", "print the version", cmdVersion},
	}
}

// Run runs the command line args, without the program name, and returns
// the exit code. Global flags come before the verb, and every verb also
// accepts them.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	g := &globals{stdin: stdin, stdout: stdout, stderr: stderr}
	fs := flag.NewFlagSet("fw", flag.ContinueOnError)
	fs.SetOutput(stderr)
	g.addFlags(fs)
	fs.Usage = func() { usage(fs) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	g.setLogger(firstNonEmpty(g.log, os.Getenv("FW_TELEMETRY_LOG"), os.Getenv("FW_LOG"), "info"))
	// The process holds the secrets of the agent in its memory and its
	// environment, so no tool process can read them through /proc.
	if err := undumpable(); err != nil {
		slog.Warn("cli: the process stays dumpable", "error", err)
	}
	if SetBuildInfo != nil {
		SetBuildInfo(Version, Commit)
	}

	rest := fs.Args()
	name := ""
	if len(rest) > 0 {
		name, rest = rest[0], rest[1:]
	} else if ax.Detect() {
		name = "serve" // ax starts the runner with no arguments
	}
	if name == "help" {
		usage(fs)
		return exitOK
	}
	for _, v := range verbs() {
		if v.name == name {
			return v.run(ctx, g, rest)
		}
	}
	if name == "" {
		usage(fs)
	} else {
		fmt.Fprintf(stderr, "fw: unknown command %q; run fw help\n", name)
	}
	return exitUsage
}

func usage(fs *flag.FlagSet) {
	w := fs.Output()
	fmt.Fprintf(w, "Usage: fw [global flags] <command> [flags] [arguments]\n\nCommands:\n")
	for _, v := range verbs() {
		if v.name == "mcp" && ServeMCPStdio == nil {
			continue // the binary has no MCP server, such as fw without the workbench
		}
		fmt.Fprintf(w, "  %-8s %s\n", v.name, v.summary)
	}
	fmt.Fprintf(w, `
Start here:
  fw init                 write fw.yaml and prompts/default.md in this directory
  fw doctor               check that the credential, the model, and the drivers work
  fw run "task"           run once; shape it with -system, -i, -tools, -skills, -model,
                          -effort, -time-zone, -max-turns, -deadline, and -max-tool-calls
  fw agents               list the agent profiles; fw agents put <file.yaml> adds one
  fw whoami               the local principal; list it in access.admins as local:*
  fw run -h               the flags of a command

Identity:
  fw mcp and fw agents act as the local principal, local:<user>. The other
  commands act as it when access.default is closed, and are trusted when it
  is open. fw serve authenticates callers with auth.mode: token, oidc, spiffe,
  or a chain such as oidc,token.

Global flags:
`)
	fs.PrintDefaults()
}

// newFlagSet returns the flag set of a verb, with the global flags.
func (g *globals) newFlagSet(name, args string) *flag.FlagSet {
	fs := flag.NewFlagSet("fw "+name, flag.ContinueOnError)
	fs.SetOutput(g.stderr)
	g.addFlags(fs)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: fw %s [flags] %s\n\nFlags:\n", name, args)
		fs.PrintDefaults()
	}
	return fs
}

// describe adds a paragraph to the usage of a flag set, after the usage
// line and before the flags.
func describe(fs *flag.FlagSet, text string) {
	usage := fs.Usage
	fs.Usage = func() {
		usage()
		fmt.Fprintf(fs.Output(), "\n%s\n", strings.TrimSpace(text))
	}
}

// parseArgs parses flags that come before, between, or after the
// positional arguments, and returns the positional arguments. Everything
// after "--" is positional.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		if n := len(args) - len(rest); n > 0 && args[n-1] == "--" {
			return append(pos, rest...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// flagExit maps a flag parse error to an exit code.
func flagExit(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	return exitUsage
}

// fail prints an error and returns exitFailed.
func (g *globals) fail(verb string, err error) int {
	fmt.Fprintf(g.stderr, "fw %s: %v\n", verb, err)
	return exitFailed
}

// setLogger makes a logger at level the default, once per level.
func (g *globals) setLogger(level string) {
	level = strings.ToLower(level)
	if level == "" || level == g.level {
		return
	}
	g.level = level
	if NewLogger != nil {
		slog.SetDefault(NewLogger(level))
		return
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(g.stderr, &slog.HandlerOptions{Level: l})))
}

// loadConfig resolves the configuration: the file, the variables, and then
// the global flags.
func (g *globals) loadConfig() (*fw.Config, error) {
	path := g.config
	if path == "" {
		path = config.FindIn(g.home)
	}
	cfg, err := config.LoadWith(path, g.override)
	if err != nil {
		return nil, err
	}
	if cfg.Home == "" {
		cfg.Home = fw.DefaultHome()
	}
	cfg.Version = Version
	g.setLogger(cfg.Telemetry.Log)
	if path != "" {
		slog.Debug("fw: configuration loaded", "path", path)
	}
	return cfg, nil
}

// override applies the global flags to a configuration.
func (g *globals) override(c *fw.Config) {
	if g.home != "" {
		c.Home = g.home
	}
	if g.listen != "" {
		c.Listen = g.listen
	}
	if g.log != "" {
		c.Telemetry.Log = g.log
	}
}

// agentMode selects how a verb builds its agent.
type agentMode int

const (
	// modeServe uses the configured ledger, bus, and authentication, and
	// resumes the unfinished runs of the ledger.
	modeServe agentMode = iota
	// modeStdio is modeServe without authentication, for a server on
	// standard input and output.
	modeStdio
	// modeOnce runs one piece of work: the configured ledger without its
	// old runs, so none is resumed, the configured bus without intake, and
	// no authentication, because nothing listens.
	modeOnce
	// modeInspect only reads the tools or the card: an in-memory ledger and
	// bus, and no authentication.
	modeInspect
	// modeManage changes the profiles: modeInspect without the tools, the
	// tool sources, and the surfaces, so no MCP server is contacted.
	modeManage
)

// newAgent builds the agent of a verb.
func newAgent(ctx context.Context, cfg *fw.Config, mode agentMode) (*fw.Agent, error) {
	opts := []fw.Option{fw.WithConfig(cfg)}
	var opened fw.Ledger
	if mode != modeServe {
		// Only a process that serves runs the services, such as a fleet
		// member that others can reach.
		opts = append(opts, fw.WithoutServices())
	}
	switch mode {
	case modeStdio:
		opts = append(opts, fw.WithAuthn(fw.NoAuthn{}))
	case modeOnce:
		cfg.Bus.Intake = ""
		l, err := fw.OpenLedger(ctx, cfg)
		if err != nil {
			return nil, err
		}
		opened = l
		opts = append(opts, fw.WithLedger(newRunsLedger{l}), fw.WithAuthn(fw.NoAuthn{}))
	case modeInspect:
		cfg.Bus.Intake = ""
		opts = append(opts, fw.WithLedger(fw.NewMemLedger()), fw.WithAuthn(fw.NoAuthn{}))
		opts = append(opts, localBus(cfg)...)
	case modeManage:
		cfg.Bus.Intake = ""
		// A profile change calls no model, so a stand-in model keeps a
		// missing credential from logging a warning here.
		noModel := fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
			return nil, errors.New("cli: this command calls no model")
		}}
		opts = append(opts, fw.WithLedger(fw.NewMemLedger()), fw.WithAuthn(fw.NoAuthn{}), fw.WithoutDefaults(), fw.WithModel(noModel))
		opts = append(opts, localBus(cfg)...)
	}
	if AgentOptions != nil {
		opts = append(opts, AgentOptions()...)
	}
	a, err := fw.New(opts...)
	if err != nil {
		if c, ok := opened.(io.Closer); ok {
			_ = c.Close()
		}
		return nil, err
	}
	return a, nil
}

// localBus returns the option of an in-memory bus for a verb that only
// reads or manages, unless an external decision point (policy.external, or
// the control plane of a linked package) answers on the bus. Then the verb
// keeps the configured bus, without intake and egress, so that the control
// plane hears it.
func localBus(cfg *fw.Config) []fw.Option {
	for _, e := range []*fw.ExternalPolicyConfig{externalAccess(cfg), cfg.Policy.External} {
		if e != nil && e.Transport() == "bus" {
			cfg.Bus.Egress = ""
			return nil
		}
	}
	return []fw.Option{fw.WithBus(fw.NewMemBus())}
}

// closeAgent drains the kernel and closes the resources of the agent.
func closeAgent(a *fw.Agent) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	a.Close(ctx)
}

// newRunsLedger hides the runs that a ledger already holds from the kernel,
// so a verb that runs one piece of work does not resume the runs of a
// server that shares the ledger. Reads, watches, and appends go through.
type newRunsLedger struct{ fw.Ledger }

// Runs implements fw.Ledger with an empty list.
func (newRunsLedger) Runs(context.Context) iter.Seq2[string, error] {
	return func(func(string, error) bool) {}
}

// Flush flushes the ledger when it can.
func (l newRunsLedger) Flush() error {
	if f, ok := l.Ledger.(interface{ Flush() error }); ok {
		return f.Flush()
	}
	return nil
}

// Close closes the ledger when it can.
func (l newRunsLedger) Close() error {
	if c, ok := l.Ledger.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
