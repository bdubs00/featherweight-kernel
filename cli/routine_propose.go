//go:build !lean || propose

package cli

// This file is the verb fw routine propose (docs/ROUTINES.md section 3.6).
// A build with the tag lean leaves it out, and the tag propose brings it
// back (routinePropose).

import (
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

func init() { routinePropose = cmdRoutinePropose }

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
