package routine

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// stateDecider answers the question type by the state.
type stateDecider map[string]fw.Answer

func (d stateDecider) Decide(_ context.Context, state string, qs map[string]fw.Question) (fw.DecideResult, error) {
	out := fw.DecideResult{Model: "fake", Answers: map[string]fw.Answer{}}
	for id := range qs {
		if a, ok := d[state]; ok {
			out.Answers[id] = a
		}
	}
	return out, nil
}

func TestReplay(t *testing.T) {
	runs := recordRuns(t, t.TempDir(), []ticket{
		{number: 1, kind: "bug"},
		{number: 2, kind: "outage"},
		{number: 3, kind: "bug", comment: "Thanks, we are on it."},
		{number: 4, kind: "access", noReply: true},
	})
	res := Replay(context.Background(), triage(), runs, ReplayOptions{})
	if res.Runs != 4 || len(res.Matched) != 2 || len(res.Diverged) != 1 || len(res.Unreadable) != 1 {
		t.Fatalf("result = %+v", res)
	}
	d := res.Diverged[0]
	if d.Run != runs[2][0].Subject || d.Step != 3 || d.Tool != "gitea.issue_write" || d.Reason != "the key body differs" ||
		!strings.Contains(d.Rendered, `"body":"Thanks. This looks like a bug."`) || !strings.Contains(d.Recorded, `"body":"Thanks, we are on it."`) {
		t.Fatalf("divergence = %+v", d)
	}
	if len(d.Rendered) > maxShown+3 || len(d.Recorded) > maxShown+3 {
		t.Errorf("the inputs are not bounded: %d, %d", len(d.Rendered), len(d.Recorded))
	}
	if u := res.Unreadable[0]; u.Run != runs[3][0].Subject || u.Reason != "type: no answer of the agent observed" {
		t.Errorf("unreadable = %+v", u)
	}

	// A check afterward matches the next call of its tool and its result.
	// A run without the call leaves the check unchecked.
	checked := triage()
	checked.After = []fw.RoutineCheck{{Tool: "gitea.issue_read", Input: `{"method": "get", "owner": "lab", "repo": "helpdesk", "issue_number": {{ find .input "#(\\d+)" }}}`, Contains: []string{"status/triaged"}}}
	more := recordRuns(t, t.TempDir(), []ticket{{number: 5, kind: "bug", extra: true}, {number: 6, kind: "bug"}})
	res = Replay(context.Background(), checked, more, ReplayOptions{})
	if len(res.Matched) != 2 || len(res.Diverged) != 0 || len(res.Unchecked) != 1 || res.Unchecked[0] != (Extra{Step: 4, Tool: "gitea.issue_read", Runs: 1}) {
		t.Fatalf("checked = %+v", res)
	}
	checked.After[0].Contains = []string{"status/closed"}
	if res = Replay(context.Background(), checked, more[:1], ReplayOptions{}); len(res.Diverged) != 1 || res.Diverged[0].Reason != `the result lacks "status/closed"` {
		t.Fatalf("lacks = %+v", res)
	}
}

func TestReplayDecide(t *testing.T) {
	tickets := []ticket{{number: 1, kind: "bug"}, {number: 2, kind: "outage"}, {number: 3, kind: "bug"}, {number: 4, kind: "access", noReply: true}}
	runs := recordRuns(t, t.TempDir(), tickets)
	d := stateDecider{
		tickets[0].input(): {Type: fw.QuestionChoice, Choice: "bug", Probability: 0.9, Probabilities: map[string]float64{"bug": 0.9, "outage": 0.1}},
		tickets[1].input(): {Type: fw.QuestionChoice, Choice: "bug", Probability: 0.6, Probabilities: map[string]float64{"bug": 0.6, "outage": 0.4}},
		tickets[2].input(): {Type: fw.QuestionChoice, Choice: "bug", Probability: 0.97, Probabilities: map[string]float64{"bug": 0.97, "outage": 0.03}},
		tickets[3].input(): {Type: fw.QuestionChoice, Choice: "access", Probability: 0.99, Probabilities: map[string]float64{"access": 0.99}},
	}
	res := Replay(context.Background(), triage(), runs, ReplayOptions{Decider: d})
	s := res.Questions["type"]
	if s == nil || s.Compared != 3 || s.Agreed != 2 {
		t.Fatalf("score = %+v", s)
	}
	if want := (0.02 + 0.72 + 0.0018) / 3; math.Abs(s.Brier-want) > 1e-9 {
		t.Errorf("brier = %v; want %v", s.Brier, want)
	}
	want := []Band{{From: 0}, {From: 0.5, Runs: 1}, {From: 0.7}, {From: 0.85, Runs: 1, Agreed: 1}, {From: 0.95, Runs: 1, Agreed: 1}}
	for i, b := range want {
		if s.Bands[i] != b {
			t.Errorf("band %d = %+v; want %+v", i, s.Bands[i], b)
		}
	}
}

// A run whose answer the routine does not act on, or whose find finds no
// value, goes to the agent and counts as handed. A key that only the call
// of the agent holds is extra, and compare narrows the keys of a step.
func TestReplayHandedExtraAndCompare(t *testing.T) {
	runs := recordRuns(t, t.TempDir(), []ticket{
		{number: 1, kind: "bug"},
		{number: 2, kind: "bug", comment: "Thanks, we are on it."},
		{number: 3, kind: "access"},
	})
	r := triage()
	q := r.Questions["type"]
	q.Expect = fw.Expect{"bug", "outage"}
	r.Questions["type"] = q
	// The routine sets no repo on the comment, and compares the comment
	// by its issue number and method only.
	r.Steps[2].Input = `{"method": "add_comment", "owner": "lab", "issue_number": {{ find .input "#(\\d+)" }}, "body": "Thanks."}`
	r.Steps[2].Compare = []string{"method", "issue_number"}
	res := Replay(context.Background(), r, runs, ReplayOptions{})
	if len(res.Matched) != 2 || len(res.Diverged) != 0 || len(res.Handed) != 1 || !strings.Contains(res.Handed[0].Reason, "the agent answered type access") {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Extra) != 1 || res.Extra[0] != (Extra{Step: 3, Tool: "gitea.issue_write", Key: "repo", Runs: 2}) {
		t.Errorf("extra = %+v", res.Extra)
	}
	// A find that finds no value hands the run to the agent.
	r.Steps[1].Input = `{"method": "add_labels", "issue_number": {{ find .input "Issue (\\d+)" }}}`
	res = Replay(context.Background(), r, runs, ReplayOptions{})
	if len(res.Handed) != 3 || !strings.Contains(res.Handed[0].Reason, "step 2 gitea.issue_write: the routine finds no value for Issue") {
		t.Fatalf("empty find = %+v", res.Handed)
	}
}
