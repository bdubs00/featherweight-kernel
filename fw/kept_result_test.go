package fw

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCutResultKeepsTheFullTextInAFile checks that a result over the
// output limit keeps its head and its tail for the model, and that the
// full text goes to a file of the run that the note names, with every
// secret replaced as in the ledger.
func TestCutResultKeepsTheFullTextInAFile(t *testing.T) {
	ctx := testCtx(t)
	t.Setenv("FW_TEST_KEPT_TOKEN", "s3cr3t-kept-value-123456")
	full := "HEAD " + strings.Repeat("x", 6000) + " s3cr3t-kept-value-123456 " + strings.Repeat("y", 6000) + " TAIL"
	big := NewTool(ToolSpec{Name: "big", ReadOnly: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		return TextResult(full), nil
	})
	root := t.TempDir()
	cfg := DefaultConfig()
	cfg.Model = "anthropic/claude-opus-5-5"
	cfg.Tools.Files.Root = root
	on := true
	cfg.Tools.Files.PerRun = &on
	cfg.Tools.Limits.MaxOutputBytes = 1024
	s := newScript(callTools(use("call:1/x", "big", `{}`)), endTurn("done"))
	a := newTestAgent(t, s.Model(), WithConfig(cfg), WithTools(big))
	r, err := a.Run(ctx, "go")
	mustState(t, r, err, StateCompleted)

	text := TextOf(firstToolResult(t, s).Content)
	name := filepath.Join(KeptResultsDir, "call_1_x.txt")
	if !strings.HasPrefix(text, "HEAD ") || !strings.HasSuffix(text, " TAIL") || len(text) > 1024 ||
		!strings.Contains(text, "The full result of "+itoa(len(full))+" bytes is in "+name) {
		t.Fatalf("result = %q", text)
	}
	b, err := os.ReadFile(filepath.Join(root, RunsDir, r.ID, name))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); strings.Contains(got, "s3cr3t-kept-value-123456") || !strings.HasPrefix(got, "HEAD ") || !strings.HasSuffix(got, " TAIL") || len(got) < 12000 {
		t.Fatalf("kept file holds %d bytes, a secret %v", len(got), strings.Contains(got, "s3cr3t"))
	}
	var changed FileChangedData
	firstOf(t, ledgerEvents(t, a.Ledger(), r.ID), EvFileChanged, &changed)
	if changed.Path != RunsDir+"/"+r.ID+"/"+name || changed.CallID != "call:1/x" || changed.Tool != "big" {
		t.Fatalf("fw.file.changed = %+v", changed)
	}
}

// TestCutResultWithoutARunKeepsNoFile checks the plain note for a call
// outside a run.
func TestCutResultWithoutARunKeepsNoFile(t *testing.T) {
	res := boundResult(TextResult(strings.Repeat("z", 5000)), 1024, keepFull(context.Background()))
	if text := TextOf(res.Content); !strings.Contains(text, "Ask for a smaller part") || len(text) > 1024 {
		t.Fatalf("result = %q", text)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
