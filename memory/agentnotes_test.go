package memory

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/memory/fronttest"
)

// TestPrincipalBriefReadsTheAgent checks the block of a run of the
// principal scope with a front: the store pulls the notes of the agent
// scope beside the notes of the principal, the block lists the notes of
// the agent first under their line and the notes of the run under
// theirs, an out-of-date note of the agent stays out and is counted, and
// fw.memory.used names the notes of the agent with their refs and hashes.
func TestPrincipalBriefReadsTheAgent(t *testing.T) {
	s, f, _ := frontStore(t, "principal")
	ctx := asRun()
	scope := s.Scope(fw.RunInfoFrom(ctx))
	day := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	review := "Gitea refuses a label name with a space.\n"
	f.Put(fronttest.Note{Agent: "triage", Scope: "agent", Name: "gotchas/labels.md", Text: review, By: "review", State: "checked", Uses: 4, At: day})
	f.Put(fronttest.Note{Agent: "triage", Scope: "agent", Name: "rules.md", Text: "Tickets close after 7 days.\n", By: "person:owner@example.com", State: "unchecked", At: day})
	f.Put(fronttest.Note{Agent: "triage", Scope: "agent", Name: "stale.md", Text: "The old label set.\n", By: "review", State: "out-of-date", At: day})
	f.Put(fronttest.Note{Agent: "triage", Scope: "principal", Principal: "ada@example.com", Name: "prefs.md", Text: "Ada wants short answers.\n", By: "agent", State: "unchecked", At: day})
	// A note of another person stays with that person.
	f.Put(fronttest.Note{Agent: "triage", Scope: "principal", Principal: "bob@example.com", Name: "bob.md", Text: "Bob's note.\n", By: "agent", State: "unchecked", At: day})

	b, err := s.Brief(ctx, scope, BriefHint)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(b.Text, "\n")
	want := []string{
		"Notes: 3 listed, X of 4 KB shown.",
		AgentNotesHead,
		"- /memories/.agent/rules.md: Tickets close after 7 days. (from a person)",
		"- /memories/.agent/gotchas/labels.md: Gitea refuses a label name with a space. (checked 2026-10-27)",
		OwnNotesHead,
		"- /memories/prefs.md: Ada wants short answers.",
		"1 note is out of date. View /memories to read them.",
	}
	if len(lines) != len(want) || !strings.HasPrefix(lines[0], "Notes: 3 listed, 0.") {
		t.Fatalf("block =\n%s", b.Text)
	}
	for i := 1; i < len(want); i++ {
		if lines[i] != want[i] {
			t.Fatalf("line %d = %q, want %q\nblock =\n%s", i, lines[i], want[i], b.Text)
		}
	}
	if strings.Contains(b.Text, "Bob") {
		t.Fatalf("the block holds the note of another person:\n%s", b.Text)
	}
	wantUsed := []fw.MemoryUsedRef{
		{Ref: "mem://agent/triage/rules.md", Hash: fronttest.Hash("Tickets close after 7 days.\n")},
		{Ref: "mem://agent/triage/gotchas/labels.md", Hash: fronttest.Hash(review)},
		{Ref: "mem://principal/triage/prefs.md", Hash: fronttest.Hash("Ada wants short answers.\n")},
	}
	if fmt.Sprint(b.Used) != fmt.Sprint(wantUsed) {
		t.Fatalf("used = %+v, want %+v", b.Used, wantUsed)
	}
	// The pull kept the notes of the agent in the copy of the agent scope,
	// and the run reads one by its ref.
	agent, _ := AgentScope(scope)
	if got, ok := readNote(t, s, agent, "gotchas/labels.md"); !ok || got != review {
		t.Fatalf("copy of the agent note = %q, %v", got, ok)
	}
	if rec, err := s.Get(ctx, scope, "mem://agent/triage/gotchas/labels.md"); err != nil || rec.Body != review || rec.Hash != fronttest.Hash(review) {
		t.Fatalf("Get of the agent note = %+v, %v", rec, err)
	}
	// The run cannot write the note of the agent through the store.
	if _, err := s.Remember(ctx, scope, fw.MemoryRecord{Ref: "mem://agent/triage/gotchas/labels.md", Body: "x\n"}, ""); err == nil {
		t.Fatal("a run of the principal scope wrote a note of the agent")
	}
	if err := s.Forget(ctx, scope, "mem://agent/triage/gotchas/labels.md"); err == nil {
		t.Fatal("a run of the principal scope removed a note of the agent")
	}
	for _, c := range f.Changes() {
		if c.Principal == "" {
			t.Fatalf("the front got a change of the agent scope: %+v", c)
		}
	}
	if n, _ := f.Get("triage", "agent", "", "gotchas/labels.md"); n.Text != review || n.State != "checked" {
		t.Fatalf("the note at the front = %+v", n)
	}
	// Recall finds the note of the agent beside the notes of the run.
	res, err := s.Recall(ctx, scope, fw.MemoryQuery{Text: "gitea", Kinds: []fw.MemoryKind{fw.MemoryKindNote}})
	if err != nil || len(res.Hits) != 1 || res.Hits[0].Record.Ref != "mem://agent/triage/gotchas/labels.md" {
		t.Fatalf("recall = %+v, %v", res, err)
	}
	if sec := s.Section(scope); !strings.Contains(sec, AgentSection) {
		t.Fatalf("the system section does not name the notes of the agent:\n%s", sec)
	}
	if sec := s.Section(agent); strings.Contains(sec, AgentPrefix) {
		t.Fatalf("the section of the agent scope names %s:\n%s", AgentPrefix, sec)
	}

	// The front does not answer: the block comes from the copies of both
	// scopes, with one line that says so.
	f.SetDown(true)
	b, err = s.Brief(ctx, scope, BriefHint)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Split(b.Text, "\n"); len(got) != len(want)+1 || got[1] != NoAnswerNote || got[2] != AgentNotesHead || got[4] != want[3] {
		t.Fatalf("block without an answer =\n%s", b.Text)
	}
}

// TestPrincipalBriefWithoutAgentNotes checks that a run of the principal
// scope whose agent has no note to list gets the block as it was: no line
// before a group. An out-of-date note of the agent still counts.
func TestPrincipalBriefWithoutAgentNotes(t *testing.T) {
	s, f, _ := frontStore(t, "principal")
	ctx := asRun()
	scope := s.Scope(fw.RunInfoFrom(ctx))
	f.Put(fronttest.Note{Agent: "triage", Scope: "principal", Principal: "ada@example.com", Name: "prefs.md", Text: "Short answers.\n", By: "agent", State: "unchecked"})
	b, err := s.Brief(ctx, scope, BriefHint)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(b.Text, "\n"); len(lines) != 2 || lines[1] != "- /memories/prefs.md: Short answers." {
		t.Fatalf("block =\n%s", b.Text)
	}
	f.Put(fronttest.Note{Agent: "triage", Scope: "agent", Name: "stale.md", Text: "Old.\n", By: "review", State: "out-of-date"})
	b, err = s.Brief(ctx, scope, BriefHint)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.Text, AgentNotesHead) || strings.Contains(b.Text, OwnNotesHead) || !strings.HasSuffix(b.Text, "\n1 note is out of date. View /memories to read them.") {
		t.Fatalf("block with only an out-of-date note of the agent =\n%s", b.Text)
	}

	// No note at all gives no block.
	s2, _, _ := frontStore(t, "principal")
	if b, err := s2.Brief(ctx, s2.Scope(fw.RunInfoFrom(ctx)), BriefHint); err != nil || b.Text != "" || len(b.Used) != 0 {
		t.Fatalf("empty block = %+v, %v", b, err)
	}
}

// TestPrincipalBriefWithoutAFront checks that without a front a run of
// the principal scope reads the local agent scope of the same home: the
// notes that runs without a principal keep there. fw.memory.used names
// them without a hash.
func TestPrincipalBriefWithoutAFront(t *testing.T) {
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Tools.Memory.Scope = "principal"
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := asRun()
	scope := s.Scope(fw.RunInfoFrom(ctx))
	// A run without a principal keeps a note in the agent scope.
	local := fw.WithRunInfo(context.Background(), fw.RunInfo{RunID: "r0", Agent: "triage"})
	agent := s.Scope(fw.RunInfoFrom(local))
	if agent.Kind != fw.MemoryAgentScope {
		t.Fatalf("scope without a principal = %+v", agent)
	}
	if _, err := s.Remember(local, agent, fw.MemoryRecord{Ref: NoteRef(agent, "build.md"), Body: "Run make test.\n"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Remember(ctx, scope, fw.MemoryRecord{Ref: NoteRef(scope, "prefs.md"), Body: "Short answers.\n"}, ""); err != nil {
		t.Fatal(err)
	}
	b, err := s.Brief(ctx, scope, BriefHint)
	if err != nil {
		t.Fatal(err)
	}
	want := ContextList + "\n" + AgentNotesHead + "\n- /memories/.agent/build.md: Run make test.\n" + OwnNotesHead + "\n- /memories/prefs.md: Short answers."
	if b.Text != want {
		t.Fatalf("block =\n%s\nwant\n%s", b.Text, want)
	}
	if len(b.Used) != 2 || b.Used[0] != (fw.MemoryUsedRef{Ref: "mem://agent/triage/build.md"}) || b.Used[1] != (fw.MemoryUsedRef{Ref: "mem://principal/triage/prefs.md"}) {
		t.Fatalf("used = %+v", b.Used)
	}
	// The run of the agent scope keeps its block: no notes of a principal,
	// no line before a group.
	b, err = s.Brief(local, agent, BriefHint)
	if err != nil || b.Text != ContextList+"\n- /memories/build.md: Run make test." {
		t.Fatalf("block of the agent scope = %q, %v", b.Text, err)
	}
	// The scope shared keeps its behavior too.
	cfg.Tools.Memory.Scope = "shared"
	sh, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := AgentScope(sh.Scope(fw.RunInfoFrom(ctx))); ok {
		t.Fatal("the shared scope reads the notes of an agent")
	}
}

// TestPrincipalBriefSharesTheBound checks that the two groups of the block
// keep the bound of 4 KB and 40 notes together, that each group first has
// half of it, so neither group pushes all the notes of the other out, and
// that the room that one group leaves goes to the other.
func TestPrincipalBriefSharesTheBound(t *testing.T) {
	s, f, _ := frontStore(t, "principal")
	ctx := asRun()
	scope := s.Scope(fw.RunInfoFrom(ctx))
	long := strings.Repeat("x", 120)
	put := func(scope, principal, name string, n int) {
		for i := range n {
			f.Put(fronttest.Note{Agent: "triage", Scope: scope, Principal: principal, Name: fmt.Sprintf("%s-%02d.md", name, i),
				Text: fmt.Sprintf("%s %02d %s\n", name, i, long), By: "agent", State: "unchecked"})
		}
	}
	put("agent", "", "review", 30)
	put("principal", "ada@example.com", "own", 30)
	b, err := s.Brief(ctx, scope, BriefHint)
	if err != nil {
		t.Fatal(err)
	}
	agentLines, ownLines, size := 0, 0, 0
	for _, l := range strings.Split(b.Text, "\n") {
		switch {
		case strings.HasPrefix(l, "- "+AgentPrefix+"/"):
			agentLines++
			size += len(l) + 1
		case strings.HasPrefix(l, "- "+Prefix+"/"):
			ownLines++
			size += len(l) + 1
		}
	}
	// Each line is about 150 bytes, so each half of 2 KB holds 13 of them.
	if size > IndexMaxBytes || agentLines < 12 || ownLines < 12 || agentLines+ownLines != len(b.Used) {
		t.Fatalf("agent %d, own %d, %d bytes:\n%s", agentLines, ownLines, size, b.Text)
	}
	if more := 60 - len(b.Used); !strings.Contains(b.Text, fmt.Sprintf("\n%d more notes are not listed. ", more)) ||
		!strings.HasPrefix(b.Text, fmt.Sprintf("Notes: %d listed, ", len(b.Used))) {
		t.Fatalf("the counts of the block are wrong:\n%s", b.Text)
	}

	// The agent has two notes: the notes of the run take the rest of the
	// room.
	for i := 2; i < 30; i++ {
		f.Drop("triage", "agent", "", fmt.Sprintf("review-%02d.md", i))
	}
	b, err = s.Brief(ctx, scope, BriefHint)
	if err != nil {
		t.Fatal(err)
	}
	agentLines, ownLines = strings.Count(b.Text, "\n- "+AgentPrefix+"/"), strings.Count(b.Text, "\n- "+Prefix+"/own-")
	if agentLines != 2 || ownLines < 20 {
		t.Fatalf("agent %d, own %d:\n%s", agentLines, ownLines, b.Text)
	}
}

// TestShowRef checks the paths of the refs that a run of the principal
// scope reads.
func TestShowRef(t *testing.T) {
	p := fw.MemoryScope{Kind: fw.MemoryPrincipalScope, Agent: "triage", Principal: "ada"}
	a := fw.MemoryScope{Kind: fw.MemoryAgentScope, Agent: "triage"}
	for _, c := range []struct {
		scope    fw.MemoryScope
		ref, out string
	}{
		{p, "mem://principal/triage/x.md", "/memories/x.md"},
		{p, "mem://agent/triage/d/x.md", "/memories/.agent/d/x.md"},
		{p, "mem://agent/other/x.md", "mem://agent/other/x.md"},
		{a, "mem://agent/triage/x.md", "/memories/x.md"},
		{a, "mem://principal/triage/x.md", "mem://principal/triage/x.md"},
	} {
		if got := ShowRef(c.scope, c.ref); got != c.out {
			t.Errorf("ShowRef(%s, %s) = %s, want %s", c.scope.Kind, c.ref, got, c.out)
		}
	}
	for name, want := range map[string]string{".agent": ".", ".agent/x.md": "x.md", ".agents/x.md": "", "x/.agent/y.md": ""} {
		got, ok := AgentName(name)
		if got != want || ok != (want != "") {
			t.Errorf("AgentName(%q) = %q, %v", name, got, ok)
		}
	}
	if err := CheckName(filepath.ToSlash(filepath.Join(AgentDir, "x.md"))); err == nil {
		t.Fatal("a note of a scope can take the path of the notes of the agent")
	}
}
