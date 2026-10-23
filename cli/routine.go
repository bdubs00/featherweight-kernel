package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/routine"
)

// routinePropose is the verb fw routine propose. routine_propose.go sets
// it in a build without the tag lean, or with the tag propose. A proposal
// reads many runs, which is the work of the workbench or of a librarian
// over a ledger, so a worker built lean does not carry it. A replay stays
// in every build, because it needs the rendering of the kernel.
var routinePropose func(ctx context.Context, g *globals, args []string) int

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
		if routinePropose == nil {
			return g.fail("routine propose", errors.New("this binary does not carry fw routine propose; build fw without the tag lean, or with the tag propose"))
		}
		return routinePropose(ctx, g, args)
	case "", "help", "-h", "-help", "--help":
		usage := "Usage: fw routine replay --routine <r.yaml> [--ledger <dir>] [--agent <name>] [--decide] [--json]\n"
		about := "replay checks a routine against the runs of a ledger: for each run, it\n" +
			"renders the steps with the answers of the agent and compares each input\n" +
			"with the call that the agent made."
		if routinePropose != nil {
			usage += "       fw routine propose --agent <name> [--ledger <dir>] [--runs <id,id>] [--out <r.yaml>] [--name <name>] [--decider <name>] [--json]\n"
			about += " propose makes a routine from the runs\nof an agent that completed and that no one sent back."
		}
		fmt.Fprintf(g.stderr, "%s\n%s\n", usage, about)
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
