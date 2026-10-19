package fw

import (
	"context"
	"testing"
	"time"
)

// TestChildReportedLeavesAFinishedParentFinished proves that the report of
// a child that ends after its parent leaves the parent finished: its file
// in the ledger closes again, and its stream of events ends.
func TestChildReportedLeavesAFinishedParentFinished(t *testing.T) {
	ctx := testCtx(t)
	l := openFileLedgerAt(t, t.TempDir())
	sp := newTreeSpawner()
	a := newTestAgent(t, newScript(endTurn("parent done")).Model(), WithSpawner(sp), WithLedger(l), withFollow(10*time.Millisecond, time.Hour))
	parent, err := a.Run(ctx, "parent")
	mustState(t, parent, err, StateCompleted)
	// The child starts and ends after the parent, as a branch or a child
	// that a look found does.
	child, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("background"))
	if err != nil {
		t.Fatal(err)
	}
	sp.report(child.ID, StateCompleted, tokens(40), tokens(40))
	eventually(t, "the report", func() bool { return countType(ledgerEvents(t, l, parent.ID), EvChildReported) == 1 })
	l.mu.Lock()
	r := l.runs[parent.ID]
	l.mu.Unlock()
	r.mu.Lock()
	open, closed := r.f != nil, r.done
	r.mu.Unlock()
	if open || !closed {
		t.Fatalf("the file of the finished parent: open %v, done %v", open, closed)
	}
	if got, _ := a.Get(ctx, parent.ID); got.State != StateCompleted {
		t.Fatalf("parent = %s", got.State)
	}
	// The stream of the events of the parent ends after the notes that
	// follow its end.
	sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	evs, err := a.Events(sctx, parent.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	for range evs {
	}
	if sctx.Err() != nil || time.Since(began) > time.Second {
		t.Fatalf("the stream of a finished run ended after %s", time.Since(began))
	}
}
