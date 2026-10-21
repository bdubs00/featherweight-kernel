package fw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// A remote fork whose record of the branch does not reach the ledger
// starts nothing in the other process: the process that Place made holds no
// run and is abandoned, and the caller learns of the failure.
func TestForkRemoteIntentFailureAborts(t *testing.T) {
	ctx := testCtx(t)
	cause := errors.New("injected intent write failure")
	l := &eventFaultLedger{Ledger: NewMemLedger(), typ: EvRunChild, cause: cause}
	other := newTestAgent(t, newScript(endTurn("child done")).Model())
	sp := &otherSpawner{other: other}
	a := newTestAgent(t, newScript(endTurn("source done")).Model(), WithLedger(l), WithSpawner(sp))
	source, err := a.Run(ctx, "source work")
	mustState(t, source, err, StateCompleted)

	if _, err := a.Fork(ctx, source.ID, ForkRequest{Spawn: true, Input: "go on"}); !errors.Is(err, ErrStorageUnavailable) || !errors.Is(err, cause) {
		t.Fatalf("fork = %v", err)
	}
	if runs, _ := other.List(ctx); len(runs) != 0 {
		t.Fatalf("the other process holds %d runs: %+v", len(runs), runs)
	}
	eventually(t, "the placed process is abandoned", func() bool { return len(sp.abandonedMembers()) == 1 && sp.abandonedMembers()[0] == "other" })
	if kids, err := a.Children(ctx, source.ID); err != nil || len(kids) != 0 {
		t.Fatalf("the source counts the aborted branch: %+v, %v", kids, err)
	}
}

// branchCopyGateLedger stops the copy of a branch at its at-th write for the
// run id, as a process that dies mid-copy would leave it.
type branchCopyGateLedger struct {
	Ledger
	id      string
	at      int32
	entered chan struct{}
	release chan struct{}
	fail    error
	writes  atomic.Int32
}

func (l *branchCopyGateLedger) Append(ctx context.Context, e Event) (uint64, error) {
	if e.Subject == l.id && l.writes.Add(1) == l.at {
		close(l.entered)
		<-l.release
		if l.fail != nil {
			return 0, l.fail
		}
	}
	return l.Ledger.Append(ctx, e)
}

// A retry of the same branch waits for the copy of the first send, and
// holds the same result: the branch that the first send wrote, or the same
// failure. A caller that receives a run holds a branch whose events are in
// the ledger.
func TestForkEventsRetryWaitsForAcceptance(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("failure=%v", fail), func(t *testing.T) {
			ctx := testCtx(t)
			id := NewID()
			l := &branchCopyGateLedger{Ledger: NewMemLedger(), id: id, at: 3, entered: make(chan struct{}), release: make(chan struct{})}
			cause := errors.New("injected copy write failure")
			if fail {
				l.fail = cause
			}
			a := newTestAgent(t, newScript(endTurn("source done")).Model(), WithLedger(l))
			source, err := a.Run(ctx, "source work")
			mustState(t, source, err, StateCompleted)
			kept := ledgerEvents(t, l, source.ID)
			req := ForkRequest{ID: id}
			type result struct {
				r   *Run
				err error
			}
			first := make(chan result, 1)
			go func() { r, err := a.ForkEvents(ctx, kept, req); first <- result{r, err} }()
			<-l.entered
			retry := make(chan result, 1)
			go func() { r, err := a.ForkEvents(ctx, kept, req); retry <- result{r, err} }()
			select {
			case res := <-retry:
				t.Fatalf("a retry returned before the copy ended: %+v", res)
			case <-time.After(20 * time.Millisecond):
			}
			close(l.release)
			one, two := <-first, <-retry
			if fail {
				for _, res := range []result{one, two} {
					if res.r != nil || !errors.Is(res.err, ErrStorageUnavailable) || !errors.Is(res.err, cause) || res.err != one.err {
						t.Fatalf("retry failure = %+v, first = %+v", res, one)
					}
				}
				return
			}
			if one.err != nil || two.err != nil || one.r.ID != id || two.r.ID != id || two.r.ID != one.r.ID {
				t.Fatalf("first = %+v, %v; retry = %+v, %v", one.r, one.err, two.r, two.err)
			}
			if got := len(ledgerEvents(t, l, id)); got != len(kept) {
				t.Fatalf("the branch holds %d events of %d copies", got, len(kept))
			}
			a.k.slotMu.Lock()
			drivers := a.k.rootDrivers
			a.k.slotMu.Unlock()
			if drivers != 0 {
				t.Fatalf("a finished branch held %d root admissions", drivers)
			}
		})
	}
}

// A branch whose kept events wait for a decision starts no driver, so it
// holds no place of the backlog while it waits. The decision starts the
// driver within the backlog.
func TestPendingApprovalBranchHoldsNoAdmission(t *testing.T) {
	ctx := testCtx(t)
	a := newTestAgent(t, newScript(endTurn("done")).Model())
	id := NewID()
	kept := []Event{
		cursorEvent(t, 1, EvRunQueued, RunQueuedData{Agent: "default", Input: "ask me", Origin: OriginHTTP}),
		cursorEvent(t, 2, EvRunStarted, RunStartedData{Model: "anthropic/claude-opus-5-5"}),
		cursorEvent(t, 3, EvTurnStarted, TurnData{Turn: 1}),
		cursorEvent(t, 4, EvModelRequested, ModelRequestedData{}),
		cursorEvent(t, 5, EvModelResponded, ModelRespondedData{StopReason: StopToolUse, Content: []Block{{Type: BlockToolUse, ID: "c1", Name: "ask", Input: json.RawMessage(`{}`)}}}),
		cursorEvent(t, 6, EvToolRequested, ToolRequestedData{Tool: "ask", CallID: "c1", Input: json.RawMessage(`{}`)}),
		cursorEvent(t, 7, EvApprovalRequested, ApprovalRequestedData{ApprovalID: "ap1", CallID: "c1", Tool: "ask", Input: json.RawMessage(`{}`)}),
	}
	r, err := a.ForkEvents(ctx, kept, ForkRequest{ID: id})
	if err != nil || r.State != StateInputRequired || r.Pending == nil {
		t.Fatalf("branch = %+v, %v", r, err)
	}
	a.k.slotMu.Lock()
	drivers := a.k.rootDrivers
	a.k.slotMu.Unlock()
	if drivers != 0 {
		t.Fatalf("a branch that waits for a decision held %d root admissions", drivers)
	}
	if err := a.Decide(ctx, id, ApprovalDecision{ApprovalID: "ap1", Decision: "allow"}); err != nil {
		t.Fatal(err)
	}
	done, err := a.WaitFor(ctx, id, func(r *Run) bool { return r.State.Terminal() })
	mustState(t, done, err, StateCompleted)
	if done.Output != "done" {
		t.Fatalf("branch output = %q", done.Output)
	}
}
