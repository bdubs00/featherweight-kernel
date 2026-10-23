//go:build !lean || propose

package routine

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// The synthetic set: twelve runs of the triage agent over three kinds of
// ticket, one run with an extra call, and one run that a person sent back.
func proposeRuns(t *testing.T) ([][]fw.Event, []ticket) {
	t.Helper()
	kinds := []string{"bug", "outage", "access"}
	var tickets []ticket
	for i := 0; i < 12; i++ {
		tickets = append(tickets, ticket{number: 30 + i, kind: kinds[i%3]})
	}
	tickets = append(tickets, ticket{number: 50, kind: "bug", extra: true}, ticket{number: 51, kind: "outage"})
	runs := recordRuns(t, t.TempDir(), tickets)
	last := runs[len(runs)-1]
	receipt := fw.MustEvent(last[0].Subject, fw.EvRunAnnotated, fw.QualityAnnotatedData{Quality: &fw.QualityObservation{ID: "r1", Completion: last[len(last)-1].Seq, Verification: "rejected"}})
	runs[len(runs)-1] = append(last, receipt)
	return runs, tickets
}

func TestPropose(t *testing.T) {
	runs, _ := proposeRuns(t)
	r, prop := Propose(runs, ProposeOptions{Name: "triage", Decider: "clef"})
	if len(prop.Runs) != 12 || len(prop.Divergent) != 1 || prop.Divergent[0].Run != runs[12][0].Subject ||
		len(prop.Excluded) != 1 || prop.Excluded[0].Reason != "the run was sent back" {
		t.Fatalf("proposal = %+v", prop)
	}
	if want := 12.0 / 13; prop.Agreement != want {
		t.Errorf("agreement = %v", prop.Agreement)
	}
	if strings.Join(prop.Sequence, " ") != "gitea.label_read gitea.issue_write gitea.issue_write" {
		t.Errorf("sequence = %v", prop.Sequence)
	}
	if err := fw.CheckRoutines(&fw.Config{Deciders: map[string]fw.DeciderConfig{"clef": {}}, Tools: fw.ToolsConfig{Builtin: []string{"decide"}}}, []fw.Routine{r}); err != nil {
		t.Fatalf("the proposal does not load: %v\n%+v", err, r)
	}
	if r.Name != "triage" || r.Version != 1 || r.Standing != fw.RoutineShadow || r.Decider != "clef" || len(r.Steps) != 3 {
		t.Fatalf("routine = %+v", r)
	}
	// The first step reads the labels, the same in every run.
	if r.Steps[0].Input != `{"method": "list", "owner": "lab", "repo": "helpdesk"}` || r.Steps[0].As != "label_read" {
		t.Errorf("step 1 = %+v", r.Steps[0])
	}
	// The label of the type is a find over the labels with the type of the
	// answer in its context; the number is a find over the input.
	step2 := r.Steps[1].Input
	for _, want := range []string{
		`"issue_number": {{ find .input`,
		"{{ find .label_read (printf `(-?\\d+(?:\\.\\d+)?),\"name\":\"%s` .type) }}, 22]",
		`"method": "add_labels"`,
	} {
		if !strings.Contains(step2, want) {
			t.Errorf("step 2 lacks %s:\n%s", want, step2)
		}
	}
	q, ok := r.Questions["type"]
	if !ok || q.Type != fw.QuestionChoice || q.Observe == nil || q.Observe.Answer == "" || len(q.Expect) != 3 {
		t.Fatalf("question type = %+v (questions %v)", q, r.QuestionNames())
	}
	if _, ok := q.Criteria.(map[string]any)["other"]; !ok {
		t.Errorf("the options lack other: %v", q.Criteria)
	}
	if !strings.HasPrefix(r.Answer, `TRIAGE {"number": {{ find .input`) || !strings.Contains(r.Answer, `"type": {{ json .type }}`) || !strings.Contains(r.Answer, `"status": "status/triaged"`) {
		t.Errorf("answer = %s", r.Answer)
	}
	// Every value that differs is a hole, with its kind.
	kinds := map[string]int{}
	for _, h := range prop.Holes {
		kinds[h.Kind]++
	}
	if kinds[HoleFind] < 3 || kinds[HoleQuestion] < 1 {
		t.Errorf("holes = %+v", prop.Holes)
	}
	// The proposal replays the runs that it came from.
	res := Replay(context.Background(), r, runs[:12], ReplayOptions{})
	if len(res.Matched) != 12 {
		t.Fatalf("replay = %+v", res)
	}
}

func TestProposeFreeHole(t *testing.T) {
	// Five runs are too few for a question, so the type is a free hole and
	// the proposal is not complete.
	var tickets []ticket
	for i := 0; i < 5; i++ {
		tickets = append(tickets, ticket{number: 60 + i, kind: []string{"bug", "outage"}[i%2]})
	}
	runs := recordRuns(t, t.TempDir(), tickets)
	r, prop := Propose(runs, ProposeOptions{Name: "triage"})
	if prop.Complete || len(r.Questions) != 0 || r.Decider != "" {
		t.Fatalf("proposal = %+v, questions %v", prop, r.QuestionNames())
	}
	free := 0
	for _, h := range prop.Holes {
		if h.Kind == HoleFree {
			free++
			if !strings.Contains(h.Template, "{{/* free ") || h.Distinct != 2 {
				t.Errorf("free hole = %+v", h)
			}
		}
	}
	if free == 0 || !strings.Contains(r.Steps[1].Input, "{{/* free step 2 labels[0] */}}") {
		t.Fatalf("holes = %+v\nstep 2 = %s", prop.Holes, r.Steps[1].Input)
	}
	if err := fw.CheckRoutines(nil, []fw.Routine{r}); err != nil {
		t.Fatalf("a proposal with free holes still loads: %v", err)
	}
	if r2, p2 := Propose(nil, ProposeOptions{}); r2.Name != "" || len(p2.Runs) != 0 {
		t.Errorf("no run = %+v %+v", r2, p2)
	}
}

// A proposal without a decider loads for replay, and a name that the
// kernel refuses makes the proposal not complete, with the problem.
func TestProposeProblems(t *testing.T) {
	runs, _ := proposeRuns(t)
	r, prop := Propose(runs, ProposeOptions{Name: "triage"})
	if r.Decider != "" || len(r.Questions) == 0 || len(prop.Problems) != 0 || !prop.Complete {
		t.Fatalf("without a decider: %+v, problems %v", prop, prop.Problems)
	}
	if res := Replay(context.Background(), r, runs[:12], ReplayOptions{}); len(res.Matched) != 12 {
		t.Fatalf("replay of a proposal without a decider = %+v", res)
	}
	_, prop = Propose(runs, ProposeOptions{Name: "Triage Desk"})
	if prop.Complete || len(prop.Problems) != 1 || !strings.Contains(prop.Problems[0], `"Triage Desk" is not the name of a routine`) {
		t.Fatalf("a bad name: %+v", prop.Problems)
	}
}

// A key that some runs leave out and the others set to one value is left
// out, a list goes element by element, a run whose list has another
// length is not used, and the number of an input is found after its word.
func TestProposeOptionalKeysAndLists(t *testing.T) {
	kinds := []string{"bug", "outage", "access"}
	var tickets []ticket
	for i := 0; i < 12; i++ {
		tickets = append(tickets, ticket{number: 30 + i, kind: kinds[i%3]})
	}
	runs := recordRuns(t, t.TempDir(), tickets)
	// One run sets per_page on the label read, as an agent can.
	runs[0] = withInput(t, runs[0], 0, `{"method": "list", "owner": "lab", "repo": "helpdesk", "per_page": 100}`)
	// One run labels one label only.
	runs[1] = withInput(t, runs[1], 1, `{"method": "add_labels", "owner": "lab", "repo": "helpdesk", "issue_number": 31, "labels": [22]}`)
	r, prop := Propose(runs, ProposeOptions{Name: "triage", Decider: "clef"})
	if len(prop.Dropped) != 1 || prop.Dropped[0] != "step 1 per_page" || r.Steps[0].Input != `{"method": "list", "owner": "lab", "repo": "helpdesk"}` {
		t.Fatalf("dropped = %v, step 1 = %s", prop.Dropped, r.Steps[0].Input)
	}
	if len(prop.Divergent) != 1 || prop.Divergent[0].Run != runs[1][0].Subject || !strings.Contains(prop.Divergent[0].Reason, "lists of another length") {
		t.Fatalf("divergent = %+v", prop.Divergent)
	}
	if !strings.Contains(r.Steps[1].Input, "`Ticket #(-?\\d+(?:\\.\\d+)?)`") || !strings.Contains(r.Steps[1].Input, `"labels": [{{`) {
		t.Errorf("step 2 = %s", r.Steps[1].Input)
	}
	for _, h := range prop.Holes {
		if strings.Contains(h.Template, "  ") {
			t.Errorf("a hole with a double space: %q", h.Template)
		}
	}
}

// withInput replaces the input of the call c of a run in its events.
func withInput(t *testing.T, events []fw.Event, c int, input string) []fw.Event {
	t.Helper()
	out := slices.Clone(events)
	n := 0
	for i, e := range out {
		if e.Type != fw.EvToolRequested {
			continue
		}
		if n == c {
			var d fw.ToolRequestedData
			if err := e.Decode(&d); err != nil {
				t.Fatal(err)
			}
			d.Input = json.RawMessage(input)
			out[i].Data, _ = json.Marshal(d)
		}
		n++
	}
	return out
}

// A proposal that holds a free value fails replay by naming the value.
func TestReplayOfAFreeValue(t *testing.T) {
	var tickets []ticket
	for i := 0; i < 5; i++ {
		tickets = append(tickets, ticket{number: 60 + i, kind: []string{"bug", "outage"}[i%2]})
	}
	runs := recordRuns(t, t.TempDir(), tickets)
	r, _ := Propose(runs, ProposeOptions{Name: "triage"})
	res := Replay(context.Background(), r, runs, ReplayOptions{})
	if len(res.Diverged) != 5 || res.Diverged[0].Reason != "the routine cannot fill the value step 2 labels[0] yet" {
		t.Fatalf("replay = %+v", res.Diverged)
	}
}
