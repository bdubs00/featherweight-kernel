package fw

import (
	"testing"
	"time"
)

// TestLateQueuedKeepsState applies the events of a run out of order, as a
// bus can deliver them: the end first, then fw.run.queued, then fw.run.queued
// again. The queued event gives the run its identity and leaves its state,
// its usage, and the title that an annotation set.
func TestLateQueuedKeepsState(t *testing.T) {
	totals := RunUsage{Usage: Usage{InputTokens: 5}, Turns: 1}
	t0 := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	log := evs("R",
		MustEvent("R", EvRunQueued, RunQueuedData{Agent: "a", Input: "the input", Principal: "ada", Metadata: map[string]string{MetaBatch: "job", "k": "queued"}}),
		MustEvent("R", EvRunStarted, nil),
		MustEvent("R", EvRunAnnotated, RunAnnotatedData{Metadata: map[string]string{MetaTitle: "Named", "k": "annotated"}}),
		MustEvent("R", EvRunFinished, RunFinishedData{Outcome: OutcomeCompleted, Output: "done", Totals: &totals}),
	)
	for i := range log {
		log[i].Time = t0.Add(time.Duration(i) * time.Minute)
	}
	var r Run
	for _, i := range []int{3, 2, 0, 0} {
		r.Apply(log[i], nil)
	}
	if r.State != StateCompleted || r.Usage != totals || r.UsageSeq != 4 || r.Seq != 4 || !r.UpdatedAt.Equal(log[3].Time) {
		t.Fatalf("run = state %s, usage %+v at %d, seq %d, updated %v", r.State, r.Usage, r.UsageSeq, r.Seq, r.UpdatedAt)
	}
	if r.Agent != "a" || r.Principal != "ada" || r.Input != "the input" || !r.CreatedAt.Equal(t0) || r.Title != "Named" ||
		r.Metadata[MetaBatch] != "job" || r.Metadata["k"] != "annotated" {
		t.Fatalf("identity = %+v", r)
	}
	// In order, the same events give the same run.
	if in := ProjectRun(log); in.State != r.State || in.Title != r.Title || in.Metadata["k"] != "annotated" || in.Usage != r.Usage {
		t.Fatalf("in order = %+v", in)
	}
}
