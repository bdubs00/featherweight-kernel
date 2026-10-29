package memory

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/memory/fronttest"
)

// TestSourcesOf checks the lines of a note that name its sources.
func TestSourcesOf(t *testing.T) {
	src := func(refs ...string) []fw.MemoryLink {
		var out []fw.MemoryLink
		for _, r := range refs {
			out = append(out, fw.MemoryLink{Ref: r, Kind: "source"})
		}
		return out
	}
	for _, c := range []struct {
		text string
		want []fw.MemoryLink
	}{
		{"Launch day: 2026-11-12.\nSource: page:launch/quillwork-3.md\n", src("page:launch/quillwork-3.md")},
		{"x\n- sources: Page:/launch/a.md, file:docs/setup.md.\n", src("page:launch/a.md", "file:docs/setup.md")},
		{"x\nSource: `page:a.md` and (file:b.txt)\nSource: page:a.md\n", src("page:a.md", "file:b.txt")},
		// What a model wrote on the stack: no ref of a kind. The store
		// takes it as a page only when the front has the page
		// (TestNoteNamesItsPageLoosely).
		{"x\nSource: wiki page launch/quillwork-3.md\n", nil},
		{"The source: page:a.md is wrong\n", nil},
		{"Source: page:\nSource: run:01K1 item:gitea/issue/4\n", nil},
		{"no line\n", nil},
	} {
		if got := SourcesOf(c.text); !reflect.DeepEqual(got, c.want) {
			t.Errorf("SourcesOf(%q) = %+v, want %+v", c.text, got, c.want)
		}
	}
	var many strings.Builder
	many.WriteString("Sources:")
	for i := range MaxTextSources + 3 {
		many.WriteString(" page:p" + string(rune('a'+i)) + ".md")
	}
	if got := SourcesOf(many.String()); len(got) != MaxTextSources {
		t.Fatalf("%d sources from one note", len(got))
	}
}

// TestLooseSources checks the loose forms of a page on a line of sources:
// a path after the word page or wiki, a path of the wiki with or without
// .md, and a markdown link to a page. SourcesOf leaves them out, because
// only the front knows whether such a page exists.
func TestLooseSources(t *testing.T) {
	for _, c := range []struct {
		text  string
		loose []string
	}{
		// What Haiku wrote on the stack (stack/evidence/2a-note-written).
		{"Quillwork 3.0 launch day and webinar date (from wiki page).\nLaunch day: 2026-11-12. Webinar: 2026-11-19.\nSource: wiki page launch/quillwork-3.md\n", []string{"launch/quillwork-3.md"}},
		{"x\nSource: wiki page launch/quillwork-3.md, version 3).\n", []string{"launch/quillwork-3.md"}},
		{"x\nSource: launch/quillwork-3\n", []string{"launch/quillwork-3.md"}},
		{"x\nSource: runbook.md\n", []string{"runbook.md"}},
		{"x\nSource: the wiki page runbook\n", []string{"runbook.md"}},
		{"x\nSource: [Quillwork 3](/launch/quillwork-3.md) and [the runbook](#pages/ops/runbook.md)\n", []string{"launch/quillwork-3.md", "ops/runbook.md"}},
		{"x\nSources: page:a.md, wiki page a.md, b.md\n", []string{"b.md"}},
		// No path: a word, a URL, a ref of another kind, a path that leaves the wiki.
		{"x\nSource: the conversation with alice\n", nil},
		{"x\nSource: https://example.com/launch/plan.md run:01K1 ../secret.md\n", nil},
		{"Launch day is near, see launch/plan.md\n", nil},
	} {
		if _, got := parseSources(c.text); !reflect.DeepEqual(got, c.loose) {
			t.Errorf("parseSources(%q) loose = %q, want %q", c.text, got, c.loose)
		}
	}
	if got := SourcesOf("x\nSources: page:a.md, wiki page b.md\n"); len(got) != 1 || got[0].Ref != "page:a.md" {
		t.Fatalf("SourcesOf = %+v", got)
	}
}

// TestNoteNamesItsPageLoosely checks that a note whose line of sources
// names a page in a loose form, as Haiku wrote it on the stack, gets the
// page as its source with the hash of the page when the front has the
// page, and no source for a page that the front does not have. Without a
// front, a loose form gives no source.
func TestNoteNamesItsPageLoosely(t *testing.T) {
	s, f, _ := frontStore(t, "agent")
	ctx := asRun()
	scope := s.Scope(fw.RunInfoFrom(ctx))
	f.PutPage("launch/quillwork-3.md", "# Quillwork 3\nLaunch day: 2026-11-12\n")
	f.PutPage("ops/runbook.md", "# Runbook\n")
	page := fronttest.Hash("# Quillwork 3\nLaunch day: 2026-11-12\n")
	haiku := "Quillwork 3.0 launch day and webinar date (from wiki page).\nLaunch day: 2026-11-12. Webinar: 2026-11-19.\nSource: wiki page launch/quillwork-3.md\n"
	for _, c := range []struct {
		name, text string
		want       []fronttest.Source
	}{
		{"haiku.md", haiku, []fronttest.Source{{Ref: "page:launch/quillwork-3.md", Version: page}}},
		{"bare.md", "Launch day: 2026-11-12.\nSource: launch/quillwork-3\n", []fronttest.Source{{Ref: "page:launch/quillwork-3.md", Version: page}}},
		{"link.md", "Restart the queue first.\nSource: [the runbook](ops/runbook.md), file:notes/q.txt\n",
			[]fronttest.Source{{Ref: "file:notes/q.txt"}, {Ref: "page:ops/runbook.md", Version: fronttest.Hash("# Runbook\n")}}},
		// The front has no such page, so the note names none.
		{"gone.md", "Launch day: 2026-11-12.\nSource: wiki page launch/gone.md\n", nil},
	} {
		if _, err := s.Remember(ctx, scope, fw.MemoryRecord{Ref: NoteRef(scope, c.name), Body: c.text}, ""); err != nil {
			t.Fatal(err)
		}
		n, ok := f.Get("triage", "agent", "", c.name)
		if !ok || len(n.Sources) != len(c.want) || (len(c.want) > 0 && !reflect.DeepEqual(n.Sources, c.want)) {
			t.Errorf("front note %s = %+v, want sources %+v", c.name, n, c.want)
		}
	}

	// Without a front.
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	local, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })
	lscope := local.Scope(fw.RunInfoFrom(ctx))
	if _, err := local.Remember(ctx, lscope, fw.MemoryRecord{Ref: NoteRef(lscope, "haiku.md"), Body: haiku}, ""); err != nil {
		t.Fatal(err)
	}
	if rec, _ := local.Get(ctx, lscope, NoteRef(lscope, "haiku.md")); len(rec.Links) != 0 {
		t.Fatalf("local sources = %+v", rec.Links)
	}
}

// TestNoteNamesItsPage checks that a write of a note whose text names a
// page sends the page to the front as a source with the hash of the page,
// so the note can go out of date, that a page that the wiki does not have
// and a file go without a version, and that the change records the
// sources. A write with sources of its own keeps them. Without a front,
// the note keeps its sources without a version.
func TestNoteNamesItsPage(t *testing.T) {
	s, f, _ := frontStore(t, "agent")
	ctx := asRun()
	scope := s.Scope(fw.RunInfoFrom(ctx))
	f.PutPage("launch/quillwork-3.md", "# Quillwork 3\nLaunch day: 2026-11-12\n")
	var changes []fw.MemoryChangedData
	rctx := fw.WithMemoryRecorder(ctx, func(d fw.MemoryChangedData) { changes = append(changes, d) })
	text := "Launch day is 2026-11-12.\nSource: page:launch/quillwork-3.md, page:launch/gone.md, file:notes/launch.txt\n"
	if _, err := s.Remember(rctx, scope, fw.MemoryRecord{Ref: NoteRef(scope, "launch.md"), Body: text}, ""); err != nil {
		t.Fatal(err)
	}
	page := fronttest.Hash("# Quillwork 3\nLaunch day: 2026-11-12\n")
	want := []fronttest.Source{{Ref: "page:launch/quillwork-3.md", Version: page}, {Ref: "page:launch/gone.md"}, {Ref: "file:notes/launch.txt"}}
	if n, ok := f.Get("triage", "agent", "", "launch.md"); !ok || !reflect.DeepEqual(n.Sources, want) {
		t.Fatalf("front note = %+v", n)
	}
	if len(changes) != 1 || len(changes[0].Sources) != 3 || changes[0].Sources[0].Version != page || changes[0].Waiting {
		t.Fatalf("changes = %+v", changes)
	}
	if rec, _ := s.Get(ctx, scope, NoteRef(scope, "launch.md")); len(rec.Links) != 3 || rec.Links[0].Version != page {
		t.Fatalf("the copy keeps the sources %+v", rec.Links)
	}
	if text, replace := Answer(changes, true); replace || !strings.Contains(text, "launch/gone.md") || strings.Contains(text, "quillwork") {
		t.Fatalf("answer = %q, %v", text, replace)
	}

	// Sources of the write win over the lines of the text.
	own := []fw.MemoryLink{{Ref: "run:01K1", Kind: "source"}}
	if _, err := s.Remember(ctx, scope, fw.MemoryRecord{Ref: NoteRef(scope, "launch.md"), Body: text + "more\n", Links: own}, ""); err != nil {
		t.Fatal(err)
	}
	if n, _ := f.Get("triage", "agent", "", "launch.md"); len(n.Sources) != 1 || n.Sources[0].Ref != "run:01K1" {
		t.Fatalf("front sources = %+v", n.Sources)
	}

	// Without a front.
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	local, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })
	lscope := local.Scope(fw.RunInfoFrom(ctx))
	if _, err := local.Remember(ctx, lscope, fw.MemoryRecord{Ref: NoteRef(lscope, "launch.md"), Body: text}, ""); err != nil {
		t.Fatal(err)
	}
	if rec, _ := local.Get(ctx, lscope, NoteRef(lscope, "launch.md")); len(rec.Links) != 3 || rec.Links[0].Version != "" {
		t.Fatalf("local sources = %+v", rec.Links)
	}
}

// TestChangeWaitsForAPerson checks that a change that the front holds for
// a person, with ask first on, is recorded as waiting, that the answer to
// the model says so in place of the result of the editor, and that the
// next pull leaves the copy as the front keeps it.
func TestChangeWaitsForAPerson(t *testing.T) {
	s, f, _ := frontStore(t, "agent")
	ctx := asRun()
	scope := s.Scope(fw.RunInfoFrom(ctx))
	if _, err := s.Remember(ctx, scope, fw.MemoryRecord{Ref: NoteRef(scope, "kept.md"), Body: "Kept before.\n"}, ""); err != nil {
		t.Fatal(err)
	}
	f.SetAskFirst(true)
	var changes []fw.MemoryChangedData
	rctx := fw.WithMemoryRecorder(ctx, func(d fw.MemoryChangedData) { changes = append(changes, d) })
	if _, err := s.Remember(rctx, scope, fw.MemoryRecord{Ref: NoteRef(scope, "webinar.md"), Body: "The webinar is on 2026-11-19.\n"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Remember(rctx, scope, fw.MemoryRecord{Ref: NoteRef(scope, "kept.md"), Body: "Changed.\n"}, ""); err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 || !changes[0].Waiting || !changes[1].Waiting {
		t.Fatalf("changes = %+v", changes)
	}
	text, replace := Answer(changes[:1], true)
	if !replace || !strings.Contains(text, "Not kept yet. The new note /memories/webinar.md waits for a person") || !strings.Contains(text, "Do not say that it is saved") {
		t.Fatalf("answer = %q, %v", text, replace)
	}
	if text, _ := Answer(changes[1:], true); !strings.Contains(text, "The change of /memories/kept.md waits") {
		t.Fatalf("answer of a change = %q", text)
	}
	if n, _ := f.Get("triage", "agent", "", "webinar.md"); n.State != "waiting" {
		t.Fatalf("front note = %+v", n)
	}
	// The next pull takes what the front keeps: no waiting note, and the
	// text that a person kept.
	s.Pull(ctx, scope)
	if _, ok := readNote(t, s, scope, "webinar.md"); ok {
		t.Fatal("the copy keeps a note that waits for a person")
	}
	if got, _ := readNote(t, s, scope, "kept.md"); got != "Kept before.\n" {
		t.Fatalf("kept.md = %q", got)
	}
	// A removal that waits.
	changes = nil
	if err := s.Forget(rctx, scope, NoteRef(scope, "kept.md")); err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || !changes[0].Waiting {
		t.Fatalf("removal = %+v", changes)
	}
	if n, _ := f.Get("triage", "agent", "", "kept.md"); n.State == "put-away" {
		t.Fatal("the front put away a note while the removal waits")
	}
}

// TestNotesFromBeforeTheFront checks that the first pull of two Tasks of
// one pool, each with its own notes from before the front, keeps every
// text: the first Task sends its note under its name, and the second
// sends its other text of the same name under a free name. A note whose
// text names a page goes with the page as its source. A second pull of
// each Task sends nothing again.
func TestNotesFromBeforeTheFront(t *testing.T) {
	f := fronttest.New(frontToken)
	t.Cleanup(f.Close)
	f.PutPage("ops/runbook.md", "# Runbook\n")
	task := func() (*Store, fw.MemoryScope, string) {
		t.Setenv("FW_TEST_FRONT_TOKEN", frontToken)
		cfg := fw.DefaultConfig()
		cfg.Home = t.TempDir()
		cfg.Tools.Memory.Front = &fw.MemoryFrontConfig{URL: f.URL, TokenEnv: "FW_TEST_FRONT_TOKEN"}
		s, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		scope := s.Scope(fw.RunInfoFrom(asRun()))
		dir, _ := s.Dir(scope)
		return s, scope, dir
	}
	a, scope, dirA := task()
	b, _, dirB := task()
	writeFile(t, filepath.Join(dirA, "gotchas.md"), "Gitea refuses a label with a space.\n")
	writeFile(t, filepath.Join(dirA, "runbook.md"), "Restart the queue first.\nSource: page:ops/runbook.md\n")
	writeFile(t, filepath.Join(dirB, "gotchas.md"), "The build needs Go 1.26.\n")
	writeFile(t, filepath.Join(dirB, "same.md"), "One text in both.\n")
	writeFile(t, filepath.Join(dirA, "same.md"), "One text in both.\n")

	ctx := asRun()
	if res := a.Pull(ctx, scope); res.Sent != 3 || res.Removed != 0 {
		t.Fatalf("pull of Task a = %+v", res)
	}
	if n, ok := f.Get("triage", "agent", "", "runbook.md"); !ok || len(n.Sources) != 1 || n.Sources[0].Version != fronttest.Hash("# Runbook\n") {
		t.Fatalf("the note with a page = %+v", n)
	}
	if res := b.Pull(ctx, scope); res.Sent != 1 || res.Fetched != 2 || res.Missed != 0 {
		t.Fatalf("pull of Task b = %+v", res)
	}
	for name, want := range map[string]string{
		"gotchas.md": "Gitea refuses a label with a space.\n", "gotchas-2.md": "The build needs Go 1.26.\n", "same.md": "One text in both.\n",
	} {
		if n, ok := f.Get("triage", "agent", "", name); !ok || n.Text != want || n.By != "agent" {
			t.Errorf("front %s = %+v", name, n)
		}
		if got, ok := readNote(t, b, scope, name); !ok || got != want {
			t.Errorf("copy of b %s = %q", name, got)
		}
	}
	if _, ok := f.Get("triage", "agent", "", "same-2.md"); ok {
		t.Fatal("a text that the front has went to a free name")
	}
	puts := len(f.Changes())
	for _, s := range []*Store{a, b} {
		if res := s.Pull(ctx, scope); res.Sent != 0 || res.Removed != 0 {
			t.Fatalf("second pull = %+v", res)
		}
	}
	if len(f.Changes()) != puts {
		t.Fatalf("a second pull changed the front: %+v", f.Changes()[puts:])
	}
	// Task a learns the note of b at its next pull.
	if got, ok := readNote(t, a, scope, "gotchas-2.md"); !ok || got != "The build needs Go 1.26.\n" {
		t.Fatalf("copy of a = %q", got)
	}
}
