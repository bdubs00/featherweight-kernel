package fw

import (
	"context"
	"encoding/json"
	"testing"
)

// TestToolRequestedRecordsTheSource checks that fw.tool.requested names
// the source of the tool, which the traces turn into gen_ai.tool.type.
func TestToolRequestedRecordsTheSource(t *testing.T) {
	ctx := testCtx(t)
	mcp := NewTool(ToolSpec{Name: "github.get_me", Source: "github", ReadOnly: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		return TextResult("me"), nil
	})
	s := newScript(callTools(use("c1", "github.get_me", `{}`)), endTurn("done"))
	a := newTestAgent(t, s.Model(), WithTools(mcp))
	r, err := a.Run(ctx, "go")
	mustState(t, r, err, StateCompleted)
	var d ToolRequestedData
	firstOf(t, ledgerEvents(t, a.Ledger(), r.ID), EvToolRequested, &d)
	if d.Source != "github" || d.Tool != "github.get_me" {
		t.Fatalf("fw.tool.requested = %+v", d)
	}
}
