package fw

import (
	"testing"
	"time"
)

// TestRefusedCancelsDoNotLoseAChildThatAnswers proves that a child whose
// cancels fail, but that answers its reads, never counts as lost: the
// kernel goes on reading it and counting its spend, and goes on trying its
// cancel while its tree is spent.
func TestRefusedCancelsDoNotLoseAChildThatAnswers(t *testing.T) {
	ctx := testCtx(t)
	sp := newReadCounter()
	a := newTestAgent(t, newScript(endTurn("parent done")).Model(), WithSpawner(sp),
		withFollow(10*time.Millisecond, time.Hour), withGiveUp(100*time.Millisecond))
	parent, err := a.Run(ctx, "parent", WithRunBudget(Budget{MaxTokens: 100}))
	mustState(t, parent, err, StateCompleted)
	child, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("spends"))
	if err != nil {
		t.Fatal(err)
	}
	sp.mu.Lock()
	sp.failCancel[child.ID] = 1 << 20 // every cancel fails, and every read works
	sp.mu.Unlock()
	sp.report(child.ID, StateRunning, tokens(200), tokens(200))
	if _, err := a.Get(ctx, child.ID); err != nil {
		t.Fatal(err)
	}
	// Well past the grace of the reads.
	time.Sleep(400 * time.Millisecond)
	sp.report(child.ID, StateRunning, tokens(900), tokens(900))
	eventually(t, "the parent counts the new spend", func() bool {
		r, _ := a.Get(ctx, parent.ID)
		return r.TreeUsage != nil && r.TreeUsage.InputTokens == 10+900
	})
	if a.Following() != 1 {
		t.Fatalf("Following = %d for a child that answers", a.Following())
	}
	if n := sp.readsIn(100 * time.Millisecond); n < 3 {
		t.Fatalf("a child that answers was read %d times in 100ms", n)
	}
	before := sp.cancelsOf(child.ID)
	time.Sleep(400 * time.Millisecond)
	if sp.cancelsOf(child.ID) <= before {
		t.Fatalf("the cancel of the child is no longer tried: %d cancels", sp.cancelsOf(child.ID))
	}
}
