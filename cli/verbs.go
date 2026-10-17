package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bdubs00/featherweight-kernel/config"
	"github.com/bdubs00/featherweight-kernel/fw"
)

// cmdMCP serves the MCP server over standard input and output. Logs go to
// standard error only, because standard output carries the protocol. The
// server acts as the local principal, so the user that runs it owns the
// profiles that it creates and is an admin when access.admins names
// local:* or kind:local.
func cmdMCP(ctx context.Context, g *globals, args []string) int {
	fs := g.newFlagSet("mcp", "")
	describe(fs, `Serves the MCP server to the process on the other end of standard input
and output, such as an editor or a desktop client. Every call acts as the
local principal, local:<user> of kind local (see fw whoami): a profile that
the client creates names it as the owner, and the entries local:* and
kind:local in access.admins give it the admin role.`)
	if _, err := parseArgs(fs, args); err != nil {
		return flagExit(err)
	}
	if ServeMCPStdio == nil {
		return g.fail("mcp", errors.New("the MCP server is not linked into this binary; it is part of fw-workbench, so run fw-workbench mcp"))
	}
	cfg, err := g.loadConfig()
	if err != nil {
		return g.fail("mcp", err)
	}
	a, err := newAgent(ctx, cfg, modeStdio)
	if err != nil {
		return g.fail("mcp", err)
	}
	defer closeAgent(a)
	if err := ServeMCPStdio(fw.WithPrincipal(ctx, localPrincipal()), a); err != nil && ctx.Err() == nil {
		return g.fail("mcp", err)
	}
	return exitOK
}

// cmdTools lists the tools of every source as a table or as JSON.
func cmdTools(ctx context.Context, g *globals, args []string) int {
	fs := g.newFlagSet("tools", "")
	asJSON := fs.Bool("json", false, "print the tool specifications as JSON")
	if _, err := parseArgs(fs, args); err != nil {
		return flagExit(err)
	}
	cfg, err := g.loadConfig()
	if err != nil {
		return g.fail("tools", err)
	}
	a, err := newAgent(ctx, cfg, modeInspect)
	if err != nil {
		return g.fail("tools", err)
	}
	defer closeAgent(a)
	specs, err := a.ToolSpecs(ctx)
	if err != nil {
		return g.fail("tools", err)
	}
	if *asJSON {
		if specs == nil {
			specs = []fw.ToolSpec{}
		}
		b, err := json.MarshalIndent(specs, "", "  ")
		if err != nil {
			return g.fail("tools", err)
		}
		fmt.Fprintf(g.stdout, "%s\n", b)
		return exitOK
	}
	tw := tabwriter.NewWriter(g.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tFLAGS\tSOURCE\tDESCRIPTION")
	for _, s := range specs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", s.Name, toolFlags(s), firstNonEmpty(s.Source, "-"), firstLine(s.Description))
	}
	if err := tw.Flush(); err != nil {
		return g.fail("tools", err)
	}
	return exitOK
}

// toolFlags lists the flags of a tool, or "-".
func toolFlags(s fw.ToolSpec) string {
	var f []string
	for _, x := range []struct {
		on   bool
		name string
	}{
		{s.ReadOnly, "readonly"},
		{s.Idempotent, "idempotent"},
		{s.Destructive, "destructive"},
		{s.ParallelSafe, "parallel"},
		{s.Trusted, "trusted"},
	} {
		if x.on {
			f = append(f, x.name)
		}
	}
	if len(f) == 0 {
		return "-"
	}
	return strings.Join(f, ",")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}

// cmdSkills lists the skills of the configured skill directories, or
// prints the SKILL.md file of one skill. It reads the directories without
// an agent, so it works without a model credential.
func cmdSkills(_ context.Context, g *globals, args []string) int {
	fs := g.newFlagSet("skills", "[<name>]")
	asJSON := fs.Bool("json", false, "print the skills as JSON, with the directory and the files of each")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(pos) > 1 {
		fs.Usage()
		return exitUsage
	}
	cfg, err := g.loadConfig()
	if err != nil {
		return g.fail("skills", err)
	}
	skills, err := fw.LoadSkills(cfg.SkillDirs()...)
	if err != nil {
		fmt.Fprintf(g.stderr, "fw skills: %v\n", err)
	}
	if len(pos) == 1 {
		return g.printSkill(skills, pos[0], *asJSON)
	}
	if *asJSON {
		return g.printSkillsJSON(skills)
	}
	if len(skills) == 0 {
		fmt.Fprintf(g.stderr, "fw skills: no skill under %s; a skill is a directory with a SKILL.md file\n", strings.Join(cfg.SkillDirs(), ", "))
		return exitOK
	}
	tw := tabwriter.NewWriter(g.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tDESCRIPTION")
	for _, s := range skills {
		fmt.Fprintf(tw, "%s\t%s\n", s.Name, s.Description)
	}
	if err := tw.Flush(); err != nil {
		return g.fail("skills", err)
	}
	return exitOK
}

// skillJSON is one skill in the JSON output of fw skills.
type skillJSON struct {
	fw.Skill
	Files []string `json:"files"`
	Body  string   `json:"body,omitempty"`
}

func skillWithFiles(s fw.Skill) skillJSON {
	files, _ := s.Files()
	if files == nil {
		files = []string{}
	}
	return skillJSON{Skill: s, Files: files}
}

func (g *globals) printSkillsJSON(skills []fw.Skill) int {
	out := make([]skillJSON, 0, len(skills))
	for _, s := range skills {
		out = append(out, skillWithFiles(s))
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return g.fail("skills", err)
	}
	fmt.Fprintf(g.stdout, "%s\n", b)
	return exitOK
}

// printSkill prints the body of one skill, or the skill as JSON with its
// body and files.
func (g *globals) printSkill(skills []fw.Skill, name string, asJSON bool) int {
	for _, s := range skills {
		if s.Name != name {
			continue
		}
		body, err := s.Body()
		if err != nil {
			return g.fail("skills", err)
		}
		if asJSON {
			doc := skillWithFiles(s)
			doc.Body = body
			b, err := json.MarshalIndent(doc, "", "  ")
			if err != nil {
				return g.fail("skills", err)
			}
			fmt.Fprintf(g.stdout, "%s\n", b)
			return exitOK
		}
		if !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		_, _ = io.WriteString(g.stdout, body)
		return exitOK
	}
	var names []string
	for _, s := range skills {
		names = append(names, s.Name)
	}
	if len(names) == 0 {
		return g.fail("skills", fmt.Errorf("no skill is named %q, and no skill is configured", name))
	}
	return g.fail("skills", fmt.Errorf("no skill is named %q; the skills are %s", name, strings.Join(names, ", ")))
}

// followInterval is how often fw events --follow reads the ledger again.
const followInterval = 250 * time.Millisecond

// cmdEvents prints the events of a run from the configured ledger, without
// a kernel, so it never resumes a run.
func cmdEvents(ctx context.Context, g *globals, args []string) int {
	fs := g.newFlagSet("events", "<run-id>")
	follow := fs.Bool("follow", false, "keep printing new events until the run finishes")
	from := fs.Uint64("from", 0, "the first sequence `number` to print")
	format := fs.String("format", "jsonl", "the output `format`: jsonl or text")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(pos) != 1 || (*format != "jsonl" && *format != "text") {
		fs.Usage()
		return exitUsage
	}
	id := pos[0]
	cfg, err := g.loadConfig()
	if err != nil {
		return g.fail("events", err)
	}
	l, err := fw.OpenLedger(ctx, cfg)
	if err != nil {
		return g.fail("events", err)
	}
	if c, ok := l.(io.Closer); ok {
		defer c.Close()
	}
	ctx = commandContext(ctx, cfg)
	n, err := printEvents(ctx, l, id, *from, *follow, *format, g.stdout)
	if err != nil && ctx.Err() == nil {
		return g.fail("events", err)
	}
	if n == 0 && !*follow {
		return g.fail("events", fmt.Errorf("%w: %s", fw.ErrRunNotFound, id))
	}
	return exitOK
}

// printEvents prints the events of a run from a sequence number. With
// follow, it reads the ledger again until the last event is terminal or ctx
// ends. Reading again works for every driver and across processes, which
// Ledger.Watch of the file driver does not. It returns the number of events
// it printed.
func printEvents(ctx context.Context, l fw.Ledger, id string, from uint64, follow bool, format string, w io.Writer) (int, error) {
	tools := map[string]string{}
	n := 0
	last := ""
	for {
		for e, err := range l.Read(ctx, id, from) {
			if err != nil {
				return n, err
			}
			if err := writeEvent(w, e, format, tools); err != nil {
				return n, err
			}
			n++
			from = e.Seq + 1
			last = e.Type
		}
		if !follow || fw.IsTerminal(last) {
			return n, nil
		}
		select {
		case <-ctx.Done():
			return n, nil
		case <-time.After(followInterval):
		}
	}
}

func writeEvent(w io.Writer, e fw.Event, format string, tools map[string]string) error {
	if format == "text" {
		_, err := fmt.Fprintf(w, "%s %s\n", e.Time.Format("2006-01-02T15:04:05.000Z07:00"), summarize(e, tools))
		return err
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", b)
	return err
}

// cmdReplay replays a recorded run against a model and prints the new run.
func cmdReplay(ctx context.Context, g *globals, args []string) int {
	fs := g.newFlagSet("replay", "<run-id>")
	model := fs.String("model", "", "the model `selector`, or a key of models, for the replay (default: the configured model)")
	ledgerDir := fs.String("ledger", "", "the root `dir` of the file ledger that holds the run (default: the configured ledger)")
	asJSON := fs.Bool("json", false, "print the new run as JSON instead of the output")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(pos) != 1 {
		fs.Usage()
		return exitUsage
	}
	id := pos[0]
	cfg, err := g.loadConfig()
	if err != nil {
		return g.fail("replay", err)
	}
	if *model != "" {
		// The selector replaces the model of every profile too, so the whole
		// run uses it.
		cfg.Model = cfg.ModelSelector(*model)
		for name, p := range cfg.Agents {
			p.Model = ""
			cfg.Agents[name] = p
		}
	}
	a, err := newAgent(ctx, cfg, modeOnce)
	if err != nil {
		return g.fail("replay", err)
	}
	defer closeAgent(a)
	ctx = commandContext(ctx, cfg)
	src := a.Ledger()
	if *ledgerDir != "" {
		fl, err := fw.OpenFileLedger(*ledgerDir)
		if err != nil {
			return g.fail("replay", err)
		}
		defer fl.Close()
		src = fl
	}
	r, err := fw.Replay(ctx, a, src, id, fw.WithOrigin(fw.OriginCLI))
	if r != nil && ctx.Err() != nil {
		_ = a.Cancel(context.Background(), r.ID)
		r, _ = a.Get(context.Background(), r.ID)
	} else if err != nil {
		return g.fail("replay", err)
	}
	name := id
	if title := runTitle(r); title != "" {
		name = fmt.Sprintf("%s %q", id, title)
	}
	fmt.Fprintf(g.stderr, "fw replay: run %s replays %s with %s: %s\n", r.ID, name, cfg.Model, r.State)
	if err := g.printRun(r, *asJSON); err != nil {
		return g.fail("replay", err)
	}
	return runExit(r)
}

// cmdCard prints the A2A agent card.
func cmdCard(ctx context.Context, g *globals, args []string) int {
	fs := g.newFlagSet("card", "")
	if _, err := parseArgs(fs, args); err != nil {
		return flagExit(err)
	}
	if CardJSON == nil {
		return g.fail("card", errors.New("the A2A surface is not linked into this binary"))
	}
	cfg, err := g.loadConfig()
	if err != nil {
		return g.fail("card", err)
	}
	a, err := newAgent(ctx, cfg, modeInspect)
	if err != nil {
		return g.fail("card", err)
	}
	defer closeAgent(a)
	b, err := CardJSON(a)
	if err != nil {
		return g.fail("card", err)
	}
	fmt.Fprintf(g.stdout, "%s\n", strings.TrimRight(string(b), "\n"))
	return exitOK
}

// cmdConfig prints the resolved configuration with secrets masked.
func cmdConfig(_ context.Context, g *globals, args []string) int {
	fs := g.newFlagSet("config", "")
	if _, err := parseArgs(fs, args); err != nil {
		return flagExit(err)
	}
	cfg, err := g.loadConfig()
	if err != nil {
		return g.fail("config", err)
	}
	b, err := config.Masked(cfg)
	if err != nil {
		return g.fail("config", err)
	}
	_, _ = g.stdout.Write(b)
	return exitOK
}

// cmdVersion prints the version, the commit, and the Go version.
func cmdVersion(_ context.Context, g *globals, args []string) int {
	fs := g.newFlagSet("version", "")
	if _, err := parseArgs(fs, args); err != nil {
		return flagExit(err)
	}
	fmt.Fprintf(g.stdout, "fw %s\ncommit %s\n%s %s/%s\n", Version, commit(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
	return exitOK
}

// commit returns Commit, else the VCS revision that the Go toolchain
// stamped, else "unknown".
func commit() string {
	if Commit != "" {
		return Commit
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				return s.Value
			}
		}
	}
	return "unknown"
}
