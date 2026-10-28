package builtin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// knowhowAgent builds an agent with the memory tool and the recall tool
// over cfg.
func knowhowAgent(t *testing.T, cfg *fw.Config, model fw.Model, extra ...fw.Tool) *fw.Agent {
	t.Helper()
	var tools []fw.Tool
	for _, name := range []string{"memory", "recall"} {
		tl, err := fw.NewBuiltinTool(name, cfg)
		if err != nil {
			t.Fatal(err)
		}
		tools = append(tools, tl)
	}
	a, err := fw.New(
		fw.WithConfig(cfg),
		fw.WithoutDefaults(),
		fw.WithAuthn(fw.NoAuthn{}),
		fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()),
		fw.WithModel(model),
		fw.WithTools(append(tools, extra...)...),
		fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeMemoryRecoveryAgent(a) })
	return a
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestMemoryUsed checks that a run records fw.memory.used for the notes of
// its start, for a view of a note, and for a note that recall reads, with
// the ref and the hash and never the text, and that a view of the
// directory and a search record nothing.
func TestMemoryUsed(t *testing.T) {
	ctx := restartCtx(t)
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Model = "fake/test"
	dir := filepath.Join(cfg.Home, "memory", "agents", "default")
	writeNote(t, dir, "build.md", "How to build: SECRET-TEXT\n", 0)
	writeNote(t, dir, "other.md", "Another note\n", 0)
	s := newRestartScript(
		&fw.Response{StopReason: fw.StopToolUse, Content: []fw.Block{
			toolUseOf("v1", "memory", `{"command":"view","path":"/memories/build.md"}`),
			toolUseOf("v2", "memory", `{"command":"view","path":"/memories"}`),
			toolUseOf("r1", "recall", `{"query":"build"}`),
			toolUseOf("r2", "recall", `{"ref":"mem://agent/default/other.md"}`),
		}},
		endTurnOf("done"),
	)
	a := knowhowAgent(t, cfg, s.Model())
	r, err := a.Run(ctx, "build it")
	if err != nil || r.State != fw.StateCompleted {
		t.Fatalf("run = %+v, %v", r, err)
	}
	var used []fw.MemoryUsedData
	started := -1
	for i, e := range ledgerOf(t, a, r.ID) {
		switch e.Type {
		case fw.EvRunStarted:
			started = i
		case fw.EvMemoryUsed:
			if started < 0 || i < started {
				t.Fatal("fw.memory.used came before fw.run.started")
			}
			if strings.Contains(string(e.Data), "SECRET-TEXT") {
				t.Fatalf("fw.memory.used holds the text of a note: %s", e.Data)
			}
			var d fw.MemoryUsedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatal(err)
			}
			used = append(used, d)
		}
	}
	if len(used) != 3 {
		t.Fatalf("fw.memory.used = %+v, want brief, view, and recall", used)
	}
	if used[0].How != fw.MemoryUsedBrief || len(used[0].Refs) != 2 {
		t.Fatalf("brief = %+v", used[0])
	}
	if used[1].How != fw.MemoryUsedView || len(used[1].Refs) != 1 ||
		used[1].Refs[0] != (fw.MemoryUsedRef{Ref: "mem://agent/default/build.md", Hash: sha("How to build: SECRET-TEXT\n")}) {
		t.Fatalf("view = %+v", used[1])
	}
	if used[2].How != fw.MemoryUsedRecall || used[2].Refs[0] != (fw.MemoryUsedRef{Ref: "mem://agent/default/other.md", Hash: sha("Another note\n")}) {
		t.Fatalf("recall = %+v", used[2])
	}
}
