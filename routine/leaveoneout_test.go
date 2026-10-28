//go:build !lean || propose

package routine

import (
	"context"
	"testing"
)

func TestLeaveOneOut(t *testing.T) {
	runs, _ := proposeRuns(t)
	res := LeaveOneOut(context.Background(), runs[:12], ProposeOptions{Name: "triage"})
	if !res.Holds() || res.Runs != 12 || res.Matched+res.Handed != 12 {
		t.Fatalf("result = %+v", res)
	}
}

func TestLeaveOneOutOfTooFewRuns(t *testing.T) {
	// Five runs give a free hole, so no proposal is complete.
	var tickets []ticket
	for i := 0; i < 5; i++ {
		tickets = append(tickets, ticket{number: 60 + i, kind: []string{"bug", "outage"}[i%2]})
	}
	runs := recordRuns(t, t.TempDir(), tickets)
	res := LeaveOneOut(context.Background(), runs, ProposeOptions{Name: "triage"})
	if res.Holds() || res.Incomplete != 5 || res.Matched != 0 {
		t.Fatalf("result = %+v", res)
	}
	if (LeaveOneOutResult{}).Holds() {
		t.Error("no run holds")
	}
}
