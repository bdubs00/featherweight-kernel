package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/memory/fronttest"
)

// frontToken is the static token of the fake front. It is a test value.
const frontToken = "test-front-token-9f3a"

// frontStore returns a store whose notes the fake front keeps.
func frontStore(t *testing.T, scope string) (*Store, *fronttest.Front, string) {
	t.Helper()
	f := fronttest.New(frontToken)
	t.Cleanup(f.Close)
	t.Setenv("FW_TEST_FRONT_TOKEN", frontToken)
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Tools.Memory.Scope = scope
	cfg.Tools.Memory.Front = &fw.MemoryFrontConfig{URL: f.URL, TokenEnv: "FW_TEST_FRONT_TOKEN"}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, f, cfg.Home
}

// asRun returns a context of the run r1 of the agent triage.
func asRun() context.Context {
	return fw.WithRunInfo(context.Background(), fw.RunInfo{RunID: "r1", Agent: "triage", Principal: "ada@example.com"})
}

func readNote(t *testing.T, s *Store, scope fw.MemoryScope, name string) (string, bool) {
	t.Helper()
	dir, err := s.Dir(scope)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
	if errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b), true
}

// TestFrontPush checks that each change through the store goes to the
// front with the hash that the copy had, who made it, the run, and the
// sources, and that the note keeps its sources and its fields.
func TestFrontPush(t *testing.T) {
	s, f, _ := frontStore(t, "principal")
	ctx := asRun()
	scope := s.Scope(fw.RunInfoFrom(ctx))
	ref := NoteRef(scope, "gotchas/labels.md")
	links := []fw.MemoryLink{{Ref: "run:01K1", Kind: "source"}, {Ref: "page:agents/triage/labels.md", Kind: "source", Version: "abc"}}
	meta := []fw.MemoryField{{Name: "origin", Value: "review"}}
	rec, err := s.Remember(ctx, scope, fw.MemoryRecord{Ref: ref, Body: "Gitea refuses a label name with a space.\n", Links: links, Metadata: meta}, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, scope, ref)
	if err != nil || len(got.Links) != 2 || got.Links[1].Version != "abc" || len(got.Metadata) != 1 {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if _, err := s.Remember(ctx, scope, fw.MemoryRecord{Ref: ref, Body: "Gitea refuses a space in a label name.\n"}, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, scope, ref); len(got.Links) != 0 {
		t.Fatalf("a new text kept the sources of the old one: %+v", got.Links)
	}
	changes := f.Changes()
	if len(changes) != 2 {
		t.Fatalf("changes = %+v", changes)
	}
	first, second := changes[0], changes[1]
	if first.IfHash != "absent" || first.By != "agent" || first.Run != "r1" || first.Principal != "ada@example.com" ||
		len(first.Sources) != 2 || first.Sources[1] != (fronttest.Source{Ref: "page:agents/triage/labels.md", Version: "abc"}) {
		t.Fatalf("first push = %+v", first)
	}
	if second.IfHash != rec.Hash || second.Sources == nil || len(second.Sources) != 0 {
		t.Fatalf("second push = %+v", second)
	}
	if n, ok := f.Get("triage", "principal", "ada@example.com", "gotchas/labels.md"); !ok || n.Text != "Gitea refuses a space in a label name.\n" {
		t.Fatalf("front note = %+v", n)
	}

	// A person through the native API: no run, a principal.
	pctx := fw.WithPrincipal(context.Background(), &fw.Principal{Subject: "ada@example.com", Kind: fw.KindUser})
	if _, err := s.Remember(pctx, scope, fw.MemoryRecord{Ref: NoteRef(scope, "prefs.md"), Body: "Short answers.\n"}, "absent"); err != nil {
		t.Fatal(err)
	}
	last := f.Changes()[2]
	if last.By != "person:ada@example.com" || last.Run != "" {
		t.Fatalf("push of a person = %+v", last)
	}

	// A removal puts the note away with the hash of the copy.
	if err := s.Forget(ctx, scope, NoteRef(scope, "prefs.md")); err != nil {
		t.Fatal(err)
	}
	del := f.Changes()[3]
	if del.Method != "DELETE" || del.IfHash != fronttest.Hash("Short answers.\n") {
		t.Fatalf("removal = %+v", del)
	}
	if n, _ := f.Get("triage", "principal", "ada@example.com", "prefs.md"); n.State != "put-away" {
		t.Fatalf("the front did not put the note away: %+v", n)
	}
}

// TestFrontConflict checks that a 412 of the front puts the text of the
// front in the copy and returns the error of the contract.
func TestFrontConflict(t *testing.T) {
	s, f, _ := frontStore(t, "agent")
	ctx := asRun()
	scope := s.Scope(fw.RunInfoFrom(ctx))
	ref := NoteRef(scope, "a.md")
	if _, err := s.Remember(ctx, scope, fw.MemoryRecord{Ref: ref, Body: "one\n"}, ""); err != nil {
		t.Fatal(err)
	}
	// Another run changed the note at the front.
	f.Put(fronttest.Note{Agent: "triage", Scope: "agent", Name: "a.md", Text: "two from elsewhere\n", State: "unchecked", By: "agent"})
	_, err := s.Remember(ctx, scope, fw.MemoryRecord{Ref: ref, Body: "three\n"}, "")
	if !errors.Is(err, ErrFrontConflict) || !errors.Is(err, fw.ErrMemoryConflict) || err.Error() != "This note changed in another run. View it again before you change it." {
		t.Fatalf("err = %v", err)
	}
	if text, _ := readNote(t, s, scope, "a.md"); text != "two from elsewhere\n" {
		t.Fatalf("copy = %q, want the text of the front", text)
	}

	// The edit of the memory tool goes through the same path.
	err = s.Locked(ctx, scope, func(files fw.Files) error {
		f.Put(fronttest.Note{Agent: "triage", Scope: "agent", Name: "a.md", Text: "four\n", State: "unchecked", By: "agent"})
		return files.WriteFile("a.md", []byte("five\n"))
	})
	if !errors.Is(err, ErrFrontConflict) {
		t.Fatalf("locked write err = %v", err)
	}
	if text, _ := readNote(t, s, scope, "a.md"); text != "four\n" {
		t.Fatalf("copy = %q", text)
	}
}

// TestFrontNoAnswer checks that a front that does not answer gets the
// change undone in the copy, with an error that says the note was not
// kept and holds neither the token nor the principal.
func TestFrontNoAnswer(t *testing.T) {
	s, f, _ := frontStore(t, "principal")
	ctx := asRun()
	scope := s.Scope(fw.RunInfoFrom(ctx))
	ref := NoteRef(scope, "a.md")
	if _, err := s.Remember(ctx, scope, fw.MemoryRecord{Ref: ref, Body: "kept\n"}, ""); err != nil {
		t.Fatal(err)
	}
	f.SetDown(true)
	check := func(err error) {
		t.Helper()
		if !errors.Is(err, ErrNotKept) || strings.Contains(err.Error(), frontToken) || strings.Contains(err.Error(), "ada@example.com") || strings.Contains(err.Error(), f.URL) {
			t.Fatalf("err = %v", err)
		}
	}
	_, err := s.Remember(ctx, scope, fw.MemoryRecord{Ref: ref, Body: "lost\n"}, "")
	check(err)
	if text, _ := readNote(t, s, scope, "a.md"); text != "kept\n" {
		t.Fatalf("copy after a failed change = %q", text)
	}
	_, err = s.Remember(ctx, scope, fw.MemoryRecord{Ref: NoteRef(scope, "new.md"), Body: "new\n"}, "")
	check(err)
	if _, ok := readNote(t, s, scope, "new.md"); ok {
		t.Fatal("a new note that the front did not take stays in the copy")
	}
	check(s.Forget(ctx, scope, ref))
	if text, _ := readNote(t, s, scope, "a.md"); text != "kept\n" {
		t.Fatalf("copy after a failed removal = %q", text)
	}
	err = s.Locked(ctx, scope, func(files fw.Files) error { return files.Rename("a.md", "b.md") })
	check(err)
	if _, ok := readNote(t, s, scope, "b.md"); ok {
		t.Fatal("a rename that the front did not take stays in the copy")
	}

	// A closed front is no answer too.
	f.SetDown(false)
	f.Close()
	_, err = s.Remember(ctx, scope, fw.MemoryRecord{Ref: ref, Body: "lost\n"}, "")
	check(err)
}

// TestFrontRename checks that a rename sends the note under the new name
// and puts the old name away, and that a directory is refused.
func TestFrontRename(t *testing.T) {
	s, f, _ := frontStore(t, "agent")
	ctx := asRun()
	scope := s.Scope(fw.RunInfoFrom(ctx))
	if _, err := s.Remember(ctx, scope, fw.MemoryRecord{Ref: NoteRef(scope, "dir/a.md"), Body: "one\n"}, ""); err != nil {
		t.Fatal(err)
	}
	err := s.Locked(ctx, scope, func(files fw.Files) error { return files.Rename("dir/a.md", "b.md") })
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := f.Get("triage", "agent", "", "b.md"); !ok || n.Text != "one\n" {
		t.Fatalf("front b.md = %+v", n)
	}
	if n, _ := f.Get("triage", "agent", "", "dir/a.md"); n.State != "put-away" {
		t.Fatalf("front dir/a.md = %+v", n)
	}
	if _, err := s.Remember(ctx, scope, fw.MemoryRecord{Ref: NoteRef(scope, "dir/c.md"), Body: "c\n"}, ""); err != nil {
		t.Fatal(err)
	}
	for _, op := range []func(fw.Files) error{
		func(files fw.Files) error { return files.Remove("dir") },
		func(files fw.Files) error { return files.Rename("dir", "other") },
	} {
		if err := s.Locked(ctx, scope, op); err == nil || !strings.Contains(err.Error(), "one note at a time") {
			t.Fatalf("a change of a directory = %v", err)
		}
	}
}

// TestFrontPull checks that the start of a run takes the notes of the
// front into the copy: a new note, a changed note, a note that the front
// put away, a note that the front no longer lists, a note that the front
// never had, and a name that no note can have.
func TestFrontPull(t *testing.T) {
	s, f, _ := frontStore(t, "agent")
	ctx := asRun()
	scope := s.Scope(fw.RunInfoFrom(ctx))
	// The copy before the front: a note from before the front was set.
	dir, _ := s.Dir(scope)
	writeFile(t, filepath.Join(dir, "old.md"), "From before the front.\n")
	// Notes that went through the front.
	for _, n := range []string{"changed.md", "gone.md", "away.md"} {
		if _, err := s.Remember(ctx, scope, fw.MemoryRecord{Ref: NoteRef(scope, n), Body: n + " v1\n"}, ""); err != nil {
			t.Fatal(err)
		}
	}
	f.Put(fronttest.Note{Agent: "triage", Scope: "agent", Name: "new.md", Text: "A person wrote this.\n", By: "person:ada", State: "unchecked",
		Sources: []fronttest.Source{{Ref: "item:gitea/issue/42", Version: "v7"}}})
	f.Put(fronttest.Note{Agent: "triage", Scope: "agent", Name: "changed.md", Text: "changed.md v2\n", By: "review", State: "checked"})
	f.Put(fronttest.Note{Agent: "triage", Scope: "agent", Name: "away.md", Text: "away.md v1\n", State: "put-away"})
	f.Put(fronttest.Note{Agent: "triage", Scope: "agent", Name: "../escape.md", Text: "x\n"})
	// The front no longer lists gone.md.
	f.Drop("triage", "agent", "", "gone.md")

	res := s.Pull(ctx, scope)
	if !res.Answered || res.Fetched != 2 || res.Removed != 2 || res.Sent != 1 {
		t.Fatalf("pull = %+v", res)
	}
	for name, want := range map[string]string{"new.md": "A person wrote this.\n", "changed.md": "changed.md v2\n", "old.md": "From before the front.\n"} {
		if got, ok := readNote(t, s, scope, name); !ok || got != want {
			t.Errorf("%s = %q, %v", name, got, ok)
		}
	}
	for _, name := range []string{"gone.md", "away.md"} {
		if _, ok := readNote(t, s, scope, name); ok {
			t.Errorf("%s stays in the copy", name)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape.md")); err == nil {
		t.Fatal("a name of the front left the directory of the scope")
	}
	if n, ok := f.Get("triage", "agent", "", "old.md"); !ok || n.By != "agent" || n.Text != "From before the front.\n" {
		t.Fatalf("the note from before the front = %+v", n)
	}
	if rec, _ := s.Get(ctx, scope, NoteRef(scope, "new.md")); len(rec.Links) != 1 || rec.Links[0].Version != "v7" {
		t.Fatalf("the sources of a pulled note = %+v", rec.Links)
	}
	// A second pull changes nothing.
	if res := s.Pull(ctx, scope); res != (PullResult{Answered: true}) {
		t.Fatalf("second pull = %+v", res)
	}
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestFrontBrief checks the block of the notes with a front: the first
// line, the order and the labels of the groups, the count of the notes
// left out, and the line of a front that does not answer.
func TestFrontBrief(t *testing.T) {
	s, f, _ := frontStore(t, "agent")
	ctx := asRun()
	scope := s.Scope(fw.RunInfoFrom(ctx))
	day := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	put := func(name, text, by, state string, uses int) {
		f.Put(fronttest.Note{Agent: "triage", Scope: "agent", Name: name, Text: text, By: by, State: state, Uses: uses, At: day})
	}
	put("plain.md", "A note without sources.\n", "agent", "unchecked", 9)
	put("checked-a.md", "Checked, used once.\n", "review", "checked", 1)
	put("checked-b.md", "Checked, used often.\n", "review", "checked", 5)
	put("person.md", "The user wants short answers.\n", "person:ada@example.com", "unchecked", 0)
	put("stale.md", "Out of date.\n", "review", "out-of-date", 3)
	put("quiet.md", "Resting.\n", "review", "resting", 0)
	put("away.md", "Put away.\n", "review", "put-away", 0)

	b, err := s.Brief(ctx, scope, BriefHint)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(b.Text, "\n")
	want := []string{
		"Notes: 4 listed, X of 4 KB shown.",
		"- /memories/person.md: The user wants short answers. (from a person)",
		"- /memories/checked-b.md: Checked, used often. (checked 2026-10-27)",
		"- /memories/checked-a.md: Checked, used once. (checked 2026-10-27)",
		"- /memories/plain.md: A note without sources.",
		"1 note is out of date, and 1 note is resting. View /memories to read them.",
	}
	// The first line counts the four notes that the block lists, not the
	// six of the copy: the lines after the list count the others.
	if len(lines) != len(want) || !strings.HasPrefix(lines[0], "Notes: 4 listed, 0.") || !strings.HasSuffix(lines[0], " of 4 KB shown.") {
		t.Fatalf("block =\n%s", b.Text)
	}
	for i := 1; i < len(want); i++ {
		if lines[i] != want[i] {
			t.Fatalf("line %d = %q, want %q\nblock =\n%s", i, lines[i], want[i], b.Text)
		}
	}
	if len(b.Used) != 4 || b.Used[0].Ref != "mem://agent/triage/person.md" || b.Used[0].Hash != fronttest.Hash("The user wants short answers.\n") {
		t.Fatalf("used = %+v", b.Used)
	}
	if !strings.Contains(s.Section(scope), "the notes that a person wrote first") {
		t.Fatal("the system section names the order of newest first")
	}
	if sec := s.Section(scope); !strings.Contains(sec, `"Source: page:launch/plan.md"`) || !strings.Contains(sec, "goes out of date when that page changes") {
		t.Fatalf("the system section does not say how a note names its page:\n%s", sec)
	}

	// The front does not answer: the copy and the labels of the last
	// answer, with one line that says so.
	f.SetDown(true)
	b, err = s.Brief(ctx, scope, BriefHint)
	if err != nil {
		t.Fatal(err)
	}
	lines = strings.Split(b.Text, "\n")
	if lines[1] != NoAnswerNote || lines[2] != want[1] || len(lines) != len(want)+1 {
		t.Fatalf("block without an answer =\n%s", b.Text)
	}
}

// TestFrontRecallRuns checks that recall merges the runs of the archive of
// the front with the runs of this Task, one hit per run id, and that a ref
// of a run of the front reads it.
func TestFrontRecallRuns(t *testing.T) {
	s, f, _ := frontStore(t, "principal")
	ctx := asRun()
	scope := s.Scope(fw.RunInfoFrom(ctx))
	f.AddRun(fronttest.Run{ID: "01KOLD", Title: "Printer queue fixed", Input: "the printer is broken", Output: "restarted the spooler", Outcome: "completed", Agent: "triage", Principal: "ada@example.com"})
	f.AddRun(fronttest.Run{ID: "r1", Title: "This run", Input: "printer", Agent: "triage", Principal: "ada@example.com"})
	f.AddRun(fronttest.Run{ID: "01KBOB", Title: "printer of bob", Agent: "triage", Principal: "bob@example.com"})
	res, err := s.Recall(ctx, scope, fw.MemoryQuery{Text: "printer", Kinds: []fw.MemoryKind{fw.MemoryKindRun}, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 || res.Hits[0].Record.Ref != "run://01KOLD" || res.Hits[0].Record.Trust != "untrusted" {
		t.Fatalf("hits = %+v", res.Hits)
	}
	if q := f.Queries(); len(q) != 1 || !strings.Contains(q[0], "agent=triage") || !strings.Contains(q[0], "limit=20") {
		t.Fatalf("queries = %v", q)
	}
	rec, err := s.Get(ctx, scope, "run://01KOLD")
	if err != nil || !strings.Contains(rec.Body, "restarted the spooler") {
		t.Fatalf("Get = %+v, %v", rec, err)
	}
	other := scope
	other.Principal = "bob@example.com"
	if _, err := s.Get(ctx, other, "run://01KOLD"); err == nil {
		t.Fatal("another principal read a run that a search of ada found")
	}
	f.SetDown(true)
	res, err = s.Recall(ctx, scope, fw.MemoryQuery{Text: "printer", Kinds: []fw.MemoryKind{fw.MemoryKindRun}})
	if err != nil || len(res.Hits) != 0 || len(res.Notes) != 1 || res.Notes[0] != FrontRunsNote {
		t.Fatalf("recall without the front = %+v, %v", res, err)
	}
}

// TestRemovalDropsEditRecords checks that the removal of a note drops the
// records of its edits, which hold lines of its text.
func TestRemovalDropsEditRecords(t *testing.T) {
	s, _ := newStore(t, "agent")
	ctx := context.Background()
	scope := s.Scope(fw.RunInfo{Agent: "triage"})
	err := s.Locked(ctx, scope, func(files fw.Files) error {
		if err := files.WriteFile("a.md", []byte("SECRET LINE\n")); err != nil {
			return err
		}
		return RecordEdit(files, CallEdit{CallID: "c1", Name: "a.md", Result: "SECRET LINE"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Forget(ctx, scope, NoteRef(scope, "a.md")); err != nil {
		t.Fatal(err)
	}
	dir, _ := s.Dir(scope)
	if b, err := os.ReadFile(filepath.Join(dir, stateName)); err == nil && strings.Contains(string(b), "SECRET LINE") {
		t.Fatalf("the state keeps the text of a removed note: %s", b)
	}
}

// TestFrontWriteAfterPutAway checks that a note that the run deleted, which
// the front put away, can get a new text under the same name.
func TestFrontWriteAfterPutAway(t *testing.T) {
	s, f, _ := frontStore(t, "agent")
	ctx := asRun()
	scope := s.Scope(fw.RunInfoFrom(ctx))
	ref := NoteRef(scope, "a.md")
	if _, err := s.Remember(ctx, scope, fw.MemoryRecord{Ref: ref, Body: "one\n"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Forget(ctx, scope, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Remember(ctx, scope, fw.MemoryRecord{Ref: ref, Body: "two\n"}, ""); err != nil {
		t.Fatalf("a new text after a removal = %v", err)
	}
	if n, _ := f.Get("triage", "agent", "", "a.md"); n.Text != "two\n" || n.State == "put-away" {
		t.Fatalf("front = %+v", n)
	}
	if res := s.Pull(ctx, scope); !res.Answered || res.Removed != 0 {
		t.Fatalf("pull = %+v", res)
	}
	if text, ok := readNote(t, s, scope, "a.md"); !ok || text != "two\n" {
		t.Fatalf("copy = %q, %v", text, ok)
	}
}
