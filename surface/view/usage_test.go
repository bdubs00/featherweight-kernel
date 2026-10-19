package view

import (
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// TestUsageMarksAPartialCost sums runs with and without a price. The tokens
// without a price stay apart, and the line calls the cost a lower bound or
// unknown instead of a complete cost.
func TestUsageMarksAPartialCost(t *testing.T) {
	priced := &fw.Run{State: fw.StateCompleted, Usage: fw.RunUsage{Usage: fw.Usage{InputTokens: 100, OutputTokens: 10}, Turns: 1, CostUSD: 0.5}}
	unpriced := &fw.Run{State: fw.StateCompleted, Usage: fw.RunUsage{Usage: fw.Usage{InputTokens: 40, OutputTokens: 2}, Turns: 1, UnpricedTokens: 42}}
	for _, tc := range []struct {
		runs []*fw.Run
		line string
	}{
		{[]*fw.Run{priced}, "1 runs, 1 turns, 110 tokens, $0.5000"},
		{[]*fw.Run{priced, unpriced}, "2 runs, 2 turns, 152 tokens, at least $0.5000 (42 tokens have no price)"},
		{[]*fw.Run{unpriced}, "1 runs, 1 turns, 42 tokens, cost unknown (42 tokens have no price)"},
	} {
		var u Usage
		for _, r := range tc.runs {
			u.Add(r)
		}
		if u.String() != tc.line {
			t.Errorf("usage = %q, want %q", u.String(), tc.line)
		}
	}
	var u Usage
	u.Add(priced)
	u.Add(unpriced)
	if u.UnpricedTokens != 42 || u.CostUSD != 0.5 {
		t.Fatalf("usage = %+v", u)
	}
}
