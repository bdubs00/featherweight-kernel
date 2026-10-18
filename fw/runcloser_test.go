package fw

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// statefulTool keeps state per run and records the runs that the kernel
// released.
type statefulTool struct {
	Tool
	mu     sync.Mutex
	closed []string
}

func (t *statefulTool) CloseRun(runID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = append(t.closed, runID)
}

// The kernel tells a RunCloser when a run ends, once, with the id of the
// run, also behind the limits middleware.
func TestRunCloserHearsTheEndOfARun(t *testing.T) {
	st := &statefulTool{Tool: NewTool(ToolSpec{Name: "stateful", Idempotent: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		return TextResult("ok"), nil
	})}
	a := newTestAgent(t, newScript(endTurn("done")).Model(), WithTools(st))
	ctx := testCtx(t)
	r, err := a.Start(ctx, "go")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.WaitFor(ctx, r.ID, func(r *Run) bool { return r.State.Terminal() }); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st.mu.Lock()
		n := len(st.closed)
		st.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.closed) != 1 || st.closed[0] != r.ID {
		t.Fatalf("CloseRun calls = %v; want one with %s", st.closed, r.ID)
	}
	if _, ok := runCloserOf(LimitTools(ToolLimits{Limit: Limit{MaxOutputBytes: 10}})(st)); !ok {
		t.Fatal("the RunCloser is not found behind the limits middleware")
	}
}
