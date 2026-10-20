package fw

import (
	"context"
	"encoding/json"
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
	// The wait between two tries doubles from cancelRetryFirst, so at this
	// point the next try can be 800 ms away or more. A fixed window shorter
	// than that wait made this test fail now and then.
	before := sp.cancelsOf(child.ID)
	eventually(t, "the cancel of the child is tried again", func() bool { return sp.cancelsOf(child.ID) > before })
}

// TestCancelCutByAShutdownIsSentOnTheNextStart proves that a cancel of a
// run whose cancels of children in other processes did not run, because
// the shutdown began, reaches those children on the next start of the
// process, and that the start after that sends nothing again.
func TestCancelCutByAShutdownIsSentOnTheNextStart(t *testing.T) {
	ctx := testCtx(t)
	l := NewMemLedger()
	sp := newReadCounter()
	started := make(chan struct{}, 1)
	block := NewTool(ToolSpec{Name: "block", ReadOnly: true}, func(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
		started <- struct{}{}
		<-ctx.Done()
		return ToolResult{}, ctx.Err()
	})
	m := ModelFunc{StreamFn: func(context.Context, *Request, func(Delta)) (*Response, error) {
		return callTools(use("b1", "block", `{}`)), nil
	}}
	a := newTestAgent(t, m, WithSpawner(sp), WithLedger(l), WithTools(block), withFollow(time.Hour, time.Hour))
	parent, err := a.Start(ctx, "parent")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	child, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("works"))
	if err != nil {
		t.Fatal(err)
	}
	// The cancel comes once the shutdown began, so its cancels of other
	// processes cannot run.
	a.k.lifeMu.Lock()
	a.k.draining = true
	a.k.lifeMu.Unlock()
	if err := a.Cancel(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if sp.wasCanceled(child.ID) {
		t.Fatal("the cancel reached the child during the shutdown; the test proves nothing")
	}
	a.k.lifeMu.Lock()
	a.k.draining = false
	a.k.lifeMu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	a.Close(cctx)
	cancel()

	b := newTestAgent(t, newScript().Model(), WithSpawner(sp), WithLedger(l), withFollow(time.Hour, time.Hour))
	eventually(t, "the next start cancels the child", func() bool { return sp.wasCanceled(child.ID) })
	eventually(t, "the parent records the end of the child", func() bool {
		d, ok := lastReport(t, l, parent.ID, child.ID)
		return ok && d.State == StateCanceled
	})
	cctx, cancel = context.WithTimeout(ctx, 5*time.Second)
	b.Close(cctx)
	cancel()
	n := sp.cancelsOf(child.ID)
	newTestAgent(t, newScript().Model(), WithSpawner(sp), WithLedger(l), withFollow(time.Hour, time.Hour))
	time.Sleep(100 * time.Millisecond)
	if got := sp.cancelsOf(child.ID); got != n {
		t.Fatalf("the start after the cancel sent it %d more times", got-n)
	}
}
