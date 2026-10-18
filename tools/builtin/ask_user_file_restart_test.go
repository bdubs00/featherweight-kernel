package builtin

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Exercise real shutdown and file-ledger reopen, rather than only seeding
// an in-memory history. Neither the original waiter nor the Agent survives.
func TestAskUserFileLedgerRestart(t *testing.T) {
	ctx := restartCtx(t)
	root := filepath.Join(t.TempDir(), "nested", "runs")
	open := func() *fw.FileLedger {
		t.Helper()
		l, err := fw.OpenFileLedger(root)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close() })
		return l
	}
	ask, err := fw.NewBuiltinTool("ask_user", fw.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	first := newRestartScript(callAskUser(`{"question":"` + restartQuestion + `"}`))
	a := restartAgent(t, open(), first.Model(), ask)
	r, err := a.Run(ctx, "ask my name")
	if err != nil || r.State != fw.StateInputRequired || r.Pending == nil {
		t.Fatalf("first run = %+v, err = %v", r, err)
	}
	id, approval := r.ID, r.Pending.ID
	a.Close(ctx)

	second := newRestartScript(endTurnOf("Hello, Ada."))
	b := restartAgent(t, open(), second.Model(), ask)
	r, err = b.Get(ctx, id)
	if err != nil || r.State != fw.StateInputRequired || r.Pending == nil || r.Pending.ID != approval {
		t.Fatalf("restored wait = %+v, err = %v", r, err)
	}
	if err := b.Decide(ctx, id, fw.ApprovalDecision{ApprovalID: approval, Content: json.RawMessage(`{"answer":"Ada"}`)}); err != nil {
		t.Fatal(err)
	}
	r, err = b.Wait(ctx, id)
	if err != nil || r.State != fw.StateCompleted {
		t.Fatalf("answered run = %+v, err = %v", r, err)
	}
	reqs := second.requests()
	if len(reqs) != 1 || resultText(reqs[0]) != `{"answer":"Ada"}` {
		t.Fatalf("restored model requests = %+v", reqs)
	}
	b.Close(ctx)

	third := newRestartScript()
	c := restartAgent(t, open(), third.Model(), ask)
	r, err = c.Wait(ctx, id)
	if err != nil || r.State != fw.StateCompleted || len(third.requests()) != 0 {
		t.Fatalf("completed run after another restart = %+v, err = %v", r, err)
	}
	evs := ledgerOf(t, c, id)
	for _, typ := range []string{fw.EvApprovalRequested, fw.EvApprovalDecided, fw.EvToolResponded} {
		if n := countOfType(evs, typ); n != 1 {
			t.Fatalf("%s count = %d, want 1", typ, n)
		}
	}
}
