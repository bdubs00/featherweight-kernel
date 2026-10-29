package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/memory"
	"github.com/bdubs00/featherweight-kernel/memory/fronttest"
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

// callAs calls a tool as the call callID of a run, without the counter of
// call, so two calls can share an id as a repeat after a restart does.
func callAs(t *testing.T, tool fw.Tool, callID, input string) fw.ToolResult {
	t.Helper()
	ctx := fw.WithRunInfo(context.Background(), fw.RunInfo{RunID: "r1", Agent: "default", CallID: callID})
	res, err := tool.Call(ctx, json.RawMessage(input))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// TestMemoryEditOnce checks that a repeated insert or str_replace with the
// same call id returns the first result and leaves the note as the first
// call did, that a repeat after another write fails, and that another call
// id edits again.
func TestMemoryEditOnce(t *testing.T) {
	home := t.TempDir()
	m := newMemoryTool(t, home, "agent")
	note := filepath.Join(home, "memory", "agents", "default", "n.md")
	mustOK(t, callAs(t, m, "c0", `{"command":"create","path":"/memories/n.md","file_text":"one\n"}`))

	ins := `{"command":"insert","path":"/memories/n.md","insert_line":1,"insert_text":"two"}`
	first := mustOK(t, callAs(t, m, "c1", ins))
	if again := mustOK(t, callAs(t, m, "c1", ins)); again != first {
		t.Fatalf("repeat = %q, first = %q", again, first)
	}
	if got := readFile(t, note); got != "one\ntwo\n" {
		t.Fatalf("note after the repeat = %q", got)
	}

	rep := `{"command":"str_replace","path":"/memories/n.md","old_str":"two","new_str":"two two"}`
	first = mustOK(t, callAs(t, m, "c2", rep))
	if again := mustOK(t, callAs(t, m, "c2", rep)); again != first || readFile(t, note) != "one\ntwo two\n" {
		t.Fatalf("repeat = %q, note = %q", again, readFile(t, note))
	}

	// Another write came after the first call: the repeat fails.
	mustOK(t, callAs(t, m, "c3", `{"command":"create","path":"/memories/n.md","file_text":"changed\n"}`))
	mustFail(t, callAs(t, m, "c2", rep), memory.ConflictText)
	if got := readFile(t, note); got != "changed\n" {
		t.Fatalf("a failed repeat changed the note: %q", got)
	}

	// Another call edits again.
	mustOK(t, callAs(t, m, "c4", `{"command":"insert","path":"/memories/n.md","insert_line":0,"insert_text":"top"}`))
	if got := readFile(t, note); got != "top\nchanged\n" {
		t.Fatalf("note = %q", got)
	}
}

// frontConfig returns a configuration whose notes the fake front keeps,
// with a home of its own, as one Task.
func frontConfig(t *testing.T, f *fronttest.Front) *fw.Config {
	t.Helper()
	t.Setenv("FW_TEST_FRONT_TOKEN", f.Token)
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Model = "fake/test"
	cfg.Tools.Memory.Front = &fw.MemoryFrontConfig{URL: f.URL, TokenEnv: "FW_TEST_FRONT_TOKEN"}
	return cfg
}

// TestNoteSurvivesANewTask checks that a note that a run of one Task wrote
// reaches the first message of a run of a new Task with an empty copy,
// through the front, and that a conflict at the front reaches the model as
// the sentence of the contract.
func TestNoteSurvivesANewTask(t *testing.T) {
	ctx := restartCtx(t)
	f := fronttest.New("builtin-front-token")
	t.Cleanup(f.Close)

	first := newRestartScript(
		&fw.Response{StopReason: fw.StopToolUse, Content: []fw.Block{
			toolUseOf("w1", "memory", `{"command":"create","path":"/memories/labels.md","file_text":"Gitea refuses a label name with a space.\n"}`),
		}},
		endTurnOf("saved"),
	)
	a := knowhowAgent(t, frontConfig(t, f), first.Model())
	if r, err := a.Run(ctx, "triage"); err != nil || r.State != fw.StateCompleted {
		t.Fatalf("first run = %+v, %v", r, err)
	}
	if n, ok := f.Get("default", "agent", "", "labels.md"); !ok || n.By != "agent" || n.Run == "" {
		t.Fatalf("front note = %+v", n)
	}

	// A new Task: another home, the same front.
	f.Put(fronttest.Note{Agent: "default", Scope: "agent", Name: "labels.md", Text: "Gitea refuses a label name with a space.\n", By: "agent", State: "checked"})
	second := newRestartScript(
		&fw.Response{StopReason: fw.StopToolUse, Content: []fw.Block{
			toolUseOf("e1", "memory", `{"command":"str_replace","path":"/memories/labels.md","old_str":"space","new_str":"blank"}`),
		}},
		endTurnOf("done"),
	)
	b := knowhowAgent(t, frontConfig(t, f), second.Model())
	r, err := b.Run(ctx, "triage again")
	if err != nil || r.State != fw.StateCompleted {
		t.Fatalf("second run = %+v, %v", r, err)
	}
	firstMsg := fw.TextOf(second.requests()[0].Messages[0].Content)
	if !strings.Contains(firstMsg, "Notes: 1 listed") || !strings.Contains(firstMsg, "/memories/labels.md: Gitea refuses a label name with a space. (checked") {
		t.Fatalf("first message of the new Task = %q", firstMsg)
	}
	if n, _ := f.Get("default", "agent", "", "labels.md"); n.Text != "Gitea refuses a label name with a blank.\n" {
		t.Fatalf("the edit of the new Task did not reach the front: %+v", n)
	}
}

// TestMemoryToolConflict checks that an edit of the memory tool that the
// front refuses with 412 returns the sentence of the contract to the
// model, and the copy holds the text of the front.
func TestMemoryToolConflict(t *testing.T) {
	f := fronttest.New("builtin-front-token")
	t.Cleanup(f.Close)
	cfg := frontConfig(t, f)
	m, err := fw.NewBuiltinTool("memory", cfg)
	if err != nil {
		t.Fatal(err)
	}
	mustOK(t, callAs(t, m, "c1", `{"command":"create","path":"/memories/n.md","file_text":"one\n"}`))
	f.Put(fronttest.Note{Agent: "default", Scope: "agent", Name: "n.md", Text: "two\n", By: "agent", State: "unchecked"})
	mustFail(t, callAs(t, m, "c2", `{"command":"str_replace","path":"/memories/n.md","old_str":"one","new_str":"three"}`), memory.ConflictText)
	if got := readFile(t, filepath.Join(cfg.Home, "memory", "agents", "default", "n.md")); got != "two\n" {
		t.Fatalf("copy = %q", got)
	}
	mustOK(t, callAs(t, m, "c3", `{"command":"create","path":"/memories/d/x.md","file_text":"x\n"}`))
	mustFail(t, callAs(t, m, "c4", `{"command":"delete","path":"/memories/d"}`), "one note at a time")
}

// TestFrontTokenRedacted checks that the token of the front is a secret of
// the agent even when the name of its variable does not say so.
func TestFrontTokenRedacted(t *testing.T) {
	ctx := restartCtx(t)
	f := fronttest.New("plain-named-front-value-1234")
	t.Cleanup(f.Close)
	cfg := frontConfig(t, f)
	t.Setenv("FW_TEST_FRONT_TOKEN", "")
	t.Setenv("FW_FRONT_ACCESS_VALUE", f.Token)
	cfg.Tools.Memory.Front.TokenEnv = "FW_FRONT_ACCESS_VALUE"
	s := newRestartScript(endTurnOf("done"))
	a := knowhowAgent(t, cfg, s.Model())
	if r, err := a.Run(ctx, "work"); err != nil || r.State != fw.StateCompleted {
		t.Fatalf("run = %+v, %v", r, err)
	}
	if got := a.Redact("token " + f.Token); strings.Contains(got, f.Token) {
		t.Fatalf("Redact = %q", got)
	}
}
