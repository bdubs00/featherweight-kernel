package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/routine"
)

// cmdRoutine is the verb routine: replay a routine over recorded runs, or
// propose one from them (docs/ROUTINES.md section 3.6).
func cmdRoutine(ctx context.Context, g *globals, args []string) int {
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "replay":
		return cmdRoutineReplay(ctx, g, args)
	case "propose":
		return cmdRoutinePropose(ctx, g, args)
	case "", "help", "-h", "-help", "--help":
		fmt.Fprint(g.stderr, `Usage: fw routine replay --routine <r.yaml> [--ledger <dir>] [--agent <name>] [--decide] [--json]
       fw routine propose --agent <name> [--ledger <dir>] [--runs <id,id>] [--out <r.yaml>] [--name <name>] [--decider <name>] [--json]

replay checks a routine against the runs of a ledger: for each run, it
renders the steps with the answers of the agent and compares each input
with the call that the agent made. propose makes a routine from the runs
of an agent that completed and that no one sent back.
`)
		if sub == "" {
			return exitUsage
		}
		return exitOK
	}
	fmt.Fprintf(g.stderr, "fw routine: unknown command %q; run fw routine help\n", sub)
	return exitUsage
}

func cmdRoutineReplay(ctx context.Context, g *globals, args []string) int {
	fs := g.newFlagSet("routine replay", "")
	file := fs.String("routine", "", "the routine `file`, YAML or JSON")
	dir := fs.String("ledger", "", "the root `dir` of a file ledger (default: the configured ledger)")
	agent := fs.String("agent", "", "read only the runs of this agent `name`")
	decide := fs.Bool("decide", false, "also ask the questions of the routine on the input of each run, and score the answers")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(pos) != 0 || *file == "" {
		fs.Usage()
		return exitUsage
	}
	r, err := readRoutine(*file)
	if err != nil {
		return g.fail("routine replay", err)
	}
	var cfg *fw.Config
	if *dir == "" || *decide {
		if cfg, err = g.loadConfig(); err != nil {
			return g.fail("routine replay", err)
		}
	}
	check := cfg
	if !*decide {
		check = nil // no model is asked, so its name needs no entry
	}
	if p := fw.RoutineProblems(check, []fw.Routine{r}); len(p) > 0 {
		return g.fail("routine replay", fmt.Errorf("%s: %s", *file, strings.Join(p, "; ")))
	}
	var opts routine.ReplayOptions
	if *decide {
		if len(r.Questions) == 0 {
			return g.fail("routine replay", fmt.Errorf("%s: the routine has no question to decide", *file))
		}
		secrets, err := fw.OpenSecrets(ctx, cfg)
		if err != nil {
			return g.fail("routine replay", err)
		}
		if opts.Decider, err = fw.NewDecider(ctx, cfg, secrets, r.Decider); err != nil {
			return g.fail("routine replay", err)
		}
	}
	runs, err := readRuns(ctx, cfg, *dir, *agent, nil)
	if err != nil {
		return g.fail("routine replay", err)
	}
	res := routine.Replay(ctx, r, runs, opts)
	if *asJSON {
		enc := json.NewEncoder(g.stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			return g.fail("routine replay", err)
		}
		return exitOK
	}
	writeReplay(g.stdout, r, res)
	return exitOK
}

// readRoutine reads a routine from a YAML or a JSON file.
func readRoutine(path string) (fw.Routine, error) {
	var r fw.Routine
	b, err := os.ReadFile(path)
	if err != nil {
		return r, err
	}
	// A key that a routine does not have, such as lack: for lacks, is an
	// error, so a typo never turns a check off.
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&r); err != nil {
		return r, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}

// readRuns reads the runs of a ledger, oldest first: the file ledger at
// dir, or the configured ledger. With agent, only the runs of that agent;
// with ids, only those runs.
func readRuns(ctx context.Context, cfg *fw.Config, dir, agent string, ids []string) ([][]fw.Event, error) {
	var l fw.Ledger
	var err error
	if dir != "" {
		l, err = fw.OpenFileLedgerReadOnly(dir)
	} else {
		l, err = openReadLedger(ctx, cfg)
	}
	if err != nil {
		return nil, err
	}
	if c, ok := l.(io.Closer); ok {
		defer c.Close()
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var out [][]fw.Event
	for id, err := range l.Runs(ctx) {
		if err != nil {
			return nil, err
		}
		if len(want) > 0 && !want[id] {
			continue
		}
		var evs []fw.Event
		for e, err := range l.Read(ctx, id, 0) {
			if err != nil {
				return nil, err
			}
			evs = append(evs, e)
		}
		if len(evs) == 0 {
			continue
		}
		if agent != "" {
			var q fw.RunQueuedData
			if evs[0].Type != fw.EvRunQueued || evs[0].Decode(&q) != nil || q.Agent != agent {
				continue
			}
		}
		out = append(out, evs)
	}
	return out, nil
}

// writeReplay prints the result of a replay for a person.
func writeReplay(w io.Writer, r fw.Routine, res routine.ReplayResult) {
	compared := len(res.Matched) + len(res.Diverged)
	fmt.Fprintf(w, "%s, version %d: %d runs\n", res.Routine, res.Version, res.Runs)
	fmt.Fprintf(w, "  matched     %d of %d\n  diverged    %d\n", len(res.Matched), compared, len(res.Diverged))
	fmt.Fprintf(w, "  handed      %d (the routine leaves these runs to the agent)\n  unreadable  %d\n", len(res.Handed), len(res.Unreadable))
	if len(res.Diverged) > 0 {
		fmt.Fprintln(w, "\nDiverged:")
		for _, d := range res.Diverged {
			fmt.Fprintf(w, "  %s  step %d %s: %s\n", d.Run, d.Step, d.Tool, d.Reason)
			if d.Rendered != "" {
				fmt.Fprintf(w, "    routine: %s\n", d.Rendered)
			}
			if d.Recorded != "" {
				fmt.Fprintf(w, "    agent:   %s\n", d.Recorded)
			}
		}
	}
	for _, list := range []struct {
		title string
		runs  []routine.Unreadable
	}{{"Handed to the agent", res.Handed}, {"Unreadable", res.Unreadable}} {
		if len(list.runs) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s:\n", list.title)
		for _, u := range list.runs {
			fmt.Fprintf(w, "  %s  %s\n", u.Run, u.Reason)
		}
	}
	if len(res.Extra) > 0 {
		fmt.Fprintln(w, "\nKeys that only the agent sets, not compared:")
		for _, x := range res.Extra {
			fmt.Fprintf(w, "  step %d %s %s: %d runs\n", x.Step, x.Tool, x.Key, x.Runs)
		}
	}
	if len(res.Unchecked) > 0 {
		fmt.Fprintln(w, "\nChecks afterward that the agent does not make, not compared:")
		for _, x := range res.Unchecked {
			fmt.Fprintf(w, "  step %d %s: %d runs\n", x.Step, x.Tool, x.Runs)
		}
	}
	if res.Questions == nil {
		return
	}
	fmt.Fprintf(w, "\nAnswers of the model %s on the input of each run:\n", r.Decider)
	if res.DeciderErrors > 0 {
		fmt.Fprintf(w, "  %d runs failed. The first error: %s\n", res.DeciderErrors, res.DeciderError)
	}
	for _, name := range r.QuestionNames() {
		s := res.Questions[name]
		if s == nil || s.Compared == 0 {
			fmt.Fprintf(w, "  %s: no run to compare\n", name)
			continue
		}
		fmt.Fprintf(w, "  %s: agreed %d of %d (%.0f%%), Brier score %.3f\n", name, s.Agreed, s.Compared, 100*float64(s.Agreed)/float64(s.Compared), s.Brier)
		fmt.Fprintln(w, "    probability    runs  agreed")
		for i, b := range s.Bands {
			span := fmt.Sprintf("below %.2f", s.Bands[min(i+1, len(s.Bands)-1)].From)
			switch {
			case i == len(s.Bands)-1:
				span = fmt.Sprintf("%.2f and up", b.From)
			case i > 0:
				span = fmt.Sprintf("%.2f to %.2f", b.From, s.Bands[i+1].From)
			}
			fmt.Fprintf(w, "    %-13s %5d %7d\n", span, b.Runs, b.Agreed)
		}
	}
}

func cmdRoutinePropose(ctx context.Context, g *globals, args []string) int {
	fs := g.newFlagSet("routine propose", "")
	agent := fs.String("agent", "", "the agent `name` whose runs the routine comes from")
	dir := fs.String("ledger", "", "the root `dir` of a file ledger (default: the configured ledger)")
	ids := fs.String("runs", "", "read only these runs: `ids` separated by commas")
	out := fs.String("out", "", "write the routine to this `file` (default: standard output)")
	name := fs.String("name", "", "the `name` of the routine (default: the agent name)")
	decider := fs.String("decider", "", "the `name` of the model that answers the questions, a key of the configuration")
	asJSON := fs.Bool("json", false, "print the routine and the proposal as JSON")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(pos) != 0 || *agent == "" {
		fs.Usage()
		return exitUsage
	}
	var cfg *fw.Config
	if *dir == "" {
		if cfg, err = g.loadConfig(); err != nil {
			return g.fail("routine propose", err)
		}
	}
	var list []string
	for _, id := range strings.Split(*ids, ",") {
		if id = strings.TrimSpace(id); id != "" {
			list = append(list, id)
		}
	}
	runs, err := readRuns(ctx, cfg, *dir, *agent, list)
	if err != nil {
		return g.fail("routine propose", err)
	}
	n := *name
	if n == "" {
		n = routineName(*agent)
	}
	r, prop := routine.Propose(runs, routine.ProposeOptions{Name: n, Decider: *decider})
	if len(prop.Runs) == 0 {
		return g.fail("routine propose", fmt.Errorf("the ledger holds no completed run of the agent %s", *agent))
	}
	body, err := yaml.Marshal(r)
	if err != nil {
		return g.fail("routine propose", err)
	}
	head := fmt.Sprintf("# Proposed by fw routine propose from %d runs of the agent %s.\n", len(prop.Runs), *agent)
	if !prop.Complete {
		head += "# Not complete: write each value that the routine cannot fill yet, or hand that step to the agent.\n"
	}
	body = append([]byte(head), body...)
	if *out != "" {
		if err := os.WriteFile(*out, body, 0o644); err != nil {
			return g.fail("routine propose", err)
		}
	}
	switch {
	case *asJSON:
		enc := json.NewEncoder(g.stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]any{"routine": r, "proposal": prop}); err != nil {
			return g.fail("routine propose", err)
		}
	case *out != "":
		writeProposal(g.stdout, r, prop)
	default:
		_, _ = g.stdout.Write(body)
		writeProposal(g.stderr, r, prop)
	}
	return exitOK
}

// routineName makes a valid routine name from an agent name: lower case,
// and every run of other characters than a-z and 0-9 a single -.
func routineName(agent string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(agent) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	n := strings.TrimRight(b.String(), "-")
	if len(n) > 64 {
		n = strings.TrimRight(n[:64], "-")
	}
	if n == "" {
		n = "proposed"
	}
	return n
}

// writeProposal prints a proposal for a person.
func writeProposal(w io.Writer, r fw.Routine, p routine.Proposal) {
	total := len(p.Runs) + len(p.Divergent)
	fmt.Fprintf(w, "%s: %d of %d completed runs make the calls %s (%.0f%%)\n", r.Name, len(p.Runs), total, strings.Join(p.Sequence, ", "), 100*p.Agreement)
	for _, x := range p.Divergent {
		fmt.Fprintf(w, "  not used: %s, %s\n", x.Run, x.Reason)
	}
	for _, x := range p.Excluded {
		fmt.Fprintf(w, "  left out: %s, %s\n", x.Run, x.Reason)
	}
	for _, x := range p.Dropped {
		fmt.Fprintf(w, "  optional, left out: %s\n", x)
	}
	for _, x := range p.Problems {
		fmt.Fprintf(w, "  to fix: %s\n", x)
	}
	if len(p.Holes) > 0 {
		fmt.Fprintln(w, "Values that differ between runs:")
	}
	kinds := map[string]string{
		routine.HoleFind:     "found in %s",
		routine.HoleQuestion: "the question %s",
		routine.HoleFree:     "a value the routine cannot fill yet",
	}
	for _, h := range p.Holes {
		where := "answer"
		if h.Step > 0 {
			where = fmt.Sprintf("step %d %s", h.Step, h.Tool)
		}
		kind := kinds[h.Kind]
		if strings.Contains(kind, "%s") {
			kind = fmt.Sprintf(kind, h.Name)
		}
		fmt.Fprintf(w, "  %s %s: %s, %d values\n", where, h.Path, kind, h.Distinct)
	}
	if p.Complete {
		fmt.Fprintln(w, "The routine is complete. Run fw routine replay on it before it rehearses.")
	} else {
		fmt.Fprintln(w, "The routine is not complete: write each value that it cannot fill yet, or hand that step to the agent.")
	}
}
