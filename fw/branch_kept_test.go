package fw

import (
	"context"
	"testing"
)

// TestBranchKeptCountsTheCopies proves that Kept of a branch is the number
// of the events that it copies, in the fw.run.queued of the copies too,
// when the branch leaves out the records of the children of its source.
func TestBranchKeptCountsTheCopies(t *testing.T) {
	a := newTestAgent(t, newScript().Model())
	src := []Event{
		MustEvent("S", EvRunQueued, RunQueuedData{Agent: "default", Input: "go"}),
		MustEvent("S", EvRunStarted, RunStartedData{}),
		MustEvent("S", EvRunChild, RunChildData{Child: "c1", Member: "remote", Kind: childSpawn}),
		MustEvent("S", EvChildReported, ChildReportedData{Child: "c1", State: StateRunning}),
		MustEvent("S", EvChildReported, ChildReportedData{Child: "c1", State: StateCompleted}),
		MustEvent("S", EvRunFinished, RunFinishedData{Outcome: OutcomeCompleted}),
	}
	for i := range src {
		src[i].Seq = uint64(i + 1)
	}
	req := ForkRequest{}
	kept, q, err := a.branchOf(context.Background(), "S", RunQueuedData{Agent: "default", Input: "go"}, src, &req)
	if err != nil {
		t.Fatal(err)
	}
	var head RunQueuedData
	if err := kept[0].Decode(&head); err != nil {
		t.Fatal(err)
	}
	if len(kept) != 3 || q.Kept != 3 || head.Kept != 3 {
		t.Fatalf("kept %d events; Kept = %d, and %d in the copy", len(kept), q.Kept, head.Kept)
	}
}
