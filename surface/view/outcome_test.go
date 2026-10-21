package view

import (
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func TestOutcomeLine(t *testing.T) {
	for _, tc := range []struct {
		d    fw.OutcomeEvaluatedData
		want string
	}{
		{fw.OutcomeEvaluatedData{Verdict: fw.GradeNeedsRevision, Notes: "Add tests.", Iteration: 1, Iterations: 2, Continue: true},
			"graded needs_revision in pass 1 of 2; the run revises its answer: Add tests."},
		{fw.OutcomeEvaluatedData{Verdict: fw.GradeSkipped, Reason: "the budget is used up", Iteration: 1, Iterations: 1},
			"graded skipped in pass 1 of 1 (the budget is used up)"},
	} {
		if got := Line(fw.MustEvent("r1", fw.EvOutcomeEvaluated, tc.d)).Line; got != tc.want {
			t.Errorf("line = %q, want %q", got, tc.want)
		}
	}
}
