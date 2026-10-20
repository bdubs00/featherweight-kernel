package fw

import (
	"context"
	"encoding/json"
	"testing"
)

// TestToolRespondedRecordsTheExitCode checks that fw.tool.responded holds
// the exit code of a command next to its result, and leaves it out for a
// tool that runs no command.
func TestToolRespondedRecordsTheExitCode(t *testing.T) {
	ctx := testCtx(t)
	code := 3
	cmd := NewTool(ToolSpec{Name: "cmd", ReadOnly: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		return ToolResult{Content: []Block{Text("exit code 3")}, ExitCode: &code}, nil
	})
	s := newScript(callTools(use("c1", "cmd", `{}`), use("c2", "echo", `{"Text":"x"}`)), endTurn("done"))
	a := newTestAgent(t, s.Model(), WithTools(cmd, echoTool(nil)))
	r, err := a.Run(ctx, "go")
	mustState(t, r, err, StateCompleted)
	got := map[string]*int{}
	for _, e := range ledgerEvents(t, a.Ledger(), r.ID) {
		if e.Type != EvToolResponded {
			continue
		}
		var d ToolRespondedData
		if err := e.Decode(&d); err != nil {
			t.Fatal(err)
		}
		got[d.CallID] = d.ExitCode
	}
	if got["c1"] == nil || *got["c1"] != 3 || got["c2"] != nil || len(got) != 2 {
		t.Fatalf("exit codes = %v", got)
	}
}
