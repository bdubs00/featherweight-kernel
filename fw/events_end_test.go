package fw

import (
	"context"
	"testing"
	"time"
)

// slowLedger stores a terminal event and then waits before it returns, so
// a watcher reads the event before the kernel applies it to the run.
type slowLedger struct {
	Ledger
	wait time.Duration
}

func (l slowLedger) Append(ctx context.Context, e Event) (uint64, error) {
	seq, err := l.Ledger.Append(ctx, e)
	if err == nil && IsTerminal(e.Type) {
		time.Sleep(l.wait)
	}
	return seq, err
}

// The stream of a run ends only after the run applied its terminal event,
// so a Get after the end reads the state that the stream showed.
func TestEventsEndAfterTheRunAppliedItsEnd(t *testing.T) {
	a := newTestAgent(t, newScript(endTurn("done")).Model(), WithLedger(slowLedger{Ledger: NewMemLedger(), wait: 100 * time.Millisecond}))
	ctx := testCtx(t)
	r, err := a.Start(ctx, "go")
	if err != nil {
		t.Fatal(err)
	}
	evs, err := a.Events(ctx, r.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var last Event
	for e := range evs {
		last = e
	}
	if last.Type != EvRunFinished {
		t.Fatalf("the stream ended at %s, not at %s", last.Type, EvRunFinished)
	}
	got, err := a.Get(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateCompleted {
		t.Fatalf("Get after the end of the stream = %s; want %s", got.State, StateCompleted)
	}
}
