package fw

import "testing"

// TestBudgetHoldsAfterACompaction proves that the budget is checked again
// after a compaction, which is a model call of its own, before the call of
// the turn.
func TestBudgetHoldsAfterACompaction(t *testing.T) {
	ctx := testCtx(t)
	s := &stepsModel{steps: []any{callTools(use("c1", "echo", `{"text":"one"}`)), endTurn("done")}}
	a := newTestAgent(t, s.Model(), WithTools(echoTool(nil)))
	a.k.compactAt = 1
	r, err := a.Run(ctx, "work", WithRunBudget(Budget{MaxTokens: 20}))
	mustState(t, r, err, StateFailed)
	if len(s.summaries) != 1 || len(s.turns) != 1 {
		t.Fatalf("summaries = %d, turns = %d: the turn after the compaction called the model", len(s.summaries), len(s.turns))
	}
	if r.Error != "budget exceeded: maxTokens" {
		t.Fatalf("error = %q", r.Error)
	}
}
