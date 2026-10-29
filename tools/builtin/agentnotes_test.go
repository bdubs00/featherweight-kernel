package builtin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/memory"
	"github.com/bdubs00/featherweight-kernel/memory/fronttest"
)

// resultOf returns the result of the tool call id from the requests that
// the model saw.
func resultOf(t *testing.T, reqs []*fw.Request, id string) (string, bool) {
	t.Helper()
	for _, req := range reqs {
		for _, m := range req.Messages {
			for _, b := range m.Content {
				if b.Type == fw.BlockToolResult && b.ToolUseID == id {
					return fw.TextOf(b.Content), b.IsError
				}
			}
		}
	}
	t.Fatalf("no result for the call %s", id)
	return "", false
}

// TestPrincipalRunReadsReviewNotes drives a run of the principal scope with
// a front and a scripted model. The after-work review keeps a note in the
// agent scope. The run sees it in the block at its start and in a view of
// the memory tool, and recall reads it. Each command that would change it
// answers that the note belongs to the agent, and the note stays as it is
// at the front and in the copy, while the run keeps its own note in its
// principal scope. An out-of-date note of the agent stays out of the block
// and is counted. A held-back run of the same person gets no notes and no
// memory tools.
func TestPrincipalRunReadsReviewNotes(t *testing.T) {
	ctx := restartCtx(t)
	f := fronttest.New("builtin-front-token")
	t.Cleanup(f.Close)
	cfg := frontConfig(t, f)
	cfg.Tools.Memory.Scope = "principal"
	review := "Gitea refuses a label name with a space.\n"
	f.Put(fronttest.Note{Agent: "default", Scope: "agent", Name: "labels.md", Text: review, By: "review", State: "checked", Uses: 2})
	f.Put(fronttest.Note{Agent: "default", Scope: "agent", Name: "old-labels.md", Text: "The old label set.\n", By: "review", State: "out-of-date"})

	writes := map[string]string{
		"w1": `{"command":"create","path":"/memories/.agent/labels.md","file_text":"x\n"}`,
		"w2": `{"command":"str_replace","path":"/memories/.agent/labels.md","old_str":"space","new_str":"blank"}`,
		"w3": `{"command":"insert","path":"/memories/.agent/labels.md","insert_line":0,"insert_text":"top"}`,
		"w4": `{"command":"delete","path":"/memories/.agent/labels.md"}`,
		"w5": `{"command":"rename","old_path":"/memories/.agent/labels.md","new_path":"/memories/labels.md"}`,
		"w6": `{"command":"rename","old_path":"/memories/mine.md","new_path":"/memories/.agent/mine.md"}`,
		"w7": `{"command":"delete","path":"/memories/.agent"}`,
		"w8": `{"command":"create","path":"/memories/.agent/new.md","file_text":"x\n"}`,
	}
	calls := []fw.Block{
		toolUseOf("v1", "memory", `{"command":"view","path":"/memories/.agent/labels.md"}`),
		toolUseOf("v2", "memory", `{"command":"view","path":"/memories"}`),
	}
	for _, id := range slices.Sorted(func(yield func(string) bool) {
		for k := range writes {
			if !yield(k) {
				return
			}
		}
	}) {
		calls = append(calls, toolUseOf(id, "memory", writes[id]))
	}
	own := "Ada calls a label a tag.\n"
	calls = append(calls,
		toolUseOf("o1", "memory", `{"command":"create","path":"/memories/labels.md","file_text":"Ada calls a label a tag.\n"}`),
		toolUseOf("r1", "recall", `{"ref":"mem://agent/default/labels.md"}`),
		toolUseOf("r2", "recall", `{"query":"gitea"}`),
	)
	s := newRestartScript(&fw.Response{StopReason: fw.StopToolUse, Content: calls}, endTurnOf("done"), endTurnOf("held back"))
	a := knowhowAgent(t, cfg, s.Model())
	r, err := a.Run(ctx, "triage", fw.WithPrincipalName("ada@example.com"))
	if err != nil || r.State != fw.StateCompleted {
		t.Fatalf("run = %+v, %v", r, err)
	}
	reqs := s.requests()

	// The block at the start lists the note of the review under its line,
	// and counts the out-of-date note that it leaves out.
	first := fw.TextOf(reqs[0].Messages[0].Content)
	for _, want := range []string{
		`<context source="memory">`,
		"Notes: 1 listed, ",
		memory.AgentNotesHead + "\n- /memories/.agent/labels.md: Gitea refuses a label name with a space. (checked ",
		"1 note is out of date. View /memories to read them.",
	} {
		if !strings.Contains(first, want) {
			t.Fatalf("the first message lacks %q:\n%s", want, first)
		}
	}
	if strings.Contains(first, "old label set") {
		t.Fatalf("the block lists an out-of-date note:\n%s", first)
	}
	if !strings.Contains(reqs[0].System, memory.AgentPrefix) {
		t.Fatal("the system prompt does not say where the notes of the agent are")
	}

	// The memory tool shows the note, and a view of /memories names it.
	if text, isErr := resultOf(t, reqs, "v1"); isErr || !strings.Contains(text, "/memories/.agent/labels.md") || !strings.Contains(text, "Gitea refuses a label name with a space.") {
		t.Fatalf("view of the note of the agent = %q, %v", text, isErr)
	}
	if text, isErr := resultOf(t, reqs, "v2"); isErr || !strings.Contains(text, "\t/memories/.agent/labels.md") || !strings.Contains(text, "read-only") {
		t.Fatalf("view of /memories = %q, %v", text, isErr)
	}
	// Each change of a note of the agent answers that it belongs to the
	// agent and that the run can keep its own note.
	for id := range writes {
		text, isErr := resultOf(t, reqs, id)
		if !isErr || !strings.Contains(text, "belong to this agent") || !strings.Contains(text, "Keep your own note instead") {
			t.Fatalf("%s %s = %q, %v", id, writes[id], text, isErr)
		}
	}
	if text, isErr := resultOf(t, reqs, "o1"); isErr || text != "File created successfully at: /memories/labels.md" {
		t.Fatalf("the own note = %q, %v", text, isErr)
	}
	if text, isErr := resultOf(t, reqs, "r1"); isErr || !strings.Contains(text, "Gitea refuses a label name with a space.") {
		t.Fatalf("recall of the note of the agent = %q, %v", text, isErr)
	}
	if text, isErr := resultOf(t, reqs, "r2"); isErr || !strings.Contains(text, "mem://agent/default/labels.md") {
		t.Fatalf("recall search = %q, %v", text, isErr)
	}

	// The note of the agent stays as it was at the front and in the copy.
	if n, _ := f.Get("default", "agent", "", "labels.md"); n.Text != review || n.State != "checked" || n.By != "review" {
		t.Fatalf("the note at the front = %+v", n)
	}
	for _, c := range f.Changes() {
		if c.Principal != "ada@example.com" {
			t.Fatalf("the front got a change outside the scope of the run: %+v", c)
		}
	}
	if got := readFile(t, filepath.Join(cfg.Home, "memory", "agents", "default", "labels.md")); got != review {
		t.Fatalf("the copy of the note = %q", got)
	}
	if n, ok := f.Get("default", "principal", "ada@example.com", "labels.md"); !ok || n.Text != own {
		t.Fatalf("the own note at the front = %+v, %v", n, ok)
	}

	// fw.memory.used names the note of the agent with its ref and hash, and
	// fw.memory.changed names only the own note.
	refs := map[string][]fw.MemoryUsedRef{}
	for _, e := range ledgerOf(t, a, r.ID) {
		switch e.Type {
		case fw.EvMemoryUsed:
			var d fw.MemoryUsedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatal(err)
			}
			refs[d.How] = append(refs[d.How], d.Refs...)
		case fw.EvMemoryChanged:
			var d fw.MemoryChangedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatal(err)
			}
			if d.Ref != "mem://principal/default/labels.md" {
				t.Fatalf("fw.memory.changed = %+v", d)
			}
		}
	}
	want := fw.MemoryUsedRef{Ref: "mem://agent/default/labels.md", Hash: fronttest.Hash(review)}
	for _, how := range []string{fw.MemoryUsedBrief, fw.MemoryUsedView, fw.MemoryUsedRecall} {
		if !slices.Contains(refs[how], want) {
			t.Fatalf("fw.memory.used %s = %+v, want %+v", how, refs[how], want)
		}
	}

	// A held-back run gets no notes and no memory tools.
	h, err := a.Run(ctx, "triage", fw.WithPrincipalName("ada@example.com"), fw.WithMetadata(map[string]string{fw.MetaKnowhow: fw.KnowhowOff}))
	if err != nil || h.State != fw.StateCompleted {
		t.Fatalf("held-back run = %+v, %v", h, err)
	}
	held := s.requests()[len(s.requests())-1]
	if msg := fw.TextOf(held.Messages[0].Content); strings.Contains(msg, "<context") || strings.Contains(msg, "Gitea") {
		t.Fatalf("the held-back run got notes: %q", msg)
	}
	if strings.Contains(held.System, memory.AgentPrefix) {
		t.Fatal("the system prompt of the held-back run names the notes of the agent")
	}
	for _, spec := range held.Tools {
		if fw.KnowhowTool(spec.Name) {
			t.Fatalf("the held-back run has the tool %s", spec.Name)
		}
	}
	for _, e := range ledgerOf(t, a, h.ID) {
		if e.Type == fw.EvMemoryUsed {
			t.Fatalf("the held-back run read notes: %s", e.Data)
		}
	}
}

// TestAgentScopeKeepsItsTree checks that a run of the agent scope keeps
// its rules: /memories/.agent is a hidden name there, not the notes of an
// agent.
func TestAgentScopeKeepsItsTree(t *testing.T) {
	home := t.TempDir()
	m := newMemoryTool(t, home, "agent")
	mustFail(t, call(t, asAgent("triage"), m, `{"command":"create","path":"/memories/.agent/x.md","file_text":"x\n"}`), "hidden name")
	if _, err := os.Stat(filepath.Join(home, "memory", "agents", "triage", ".agent")); !os.IsNotExist(err) {
		t.Fatalf("the agent scope wrote under .agent: %v", err)
	}
}
