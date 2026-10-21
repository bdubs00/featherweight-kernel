package memory

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func newStore(t *testing.T, scope string) (*Store, string) {
	t.Helper()
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Tools.Memory.Scope = scope
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, cfg.Home
}

func TestScopeAndDir(t *testing.T) {
	s, home := newStore(t, "principal")
	alice := s.Scope(fw.RunInfo{Agent: "reviewer", Principal: "alice@example.com", RunID: "r1"})
	local := s.Scope(fw.RunInfo{})
	if alice.Kind != fw.MemoryPrincipalScope || alice.Agent != "reviewer" || alice.Task != "r1" {
		t.Fatalf("alice scope = %+v", alice)
	}
	if local.Kind != fw.MemoryAgentScope || local.Agent != "default" {
		t.Fatalf("local scope = %+v", local)
	}
	dir, err := s.Dir(alice)
	if err != nil || !strings.HasPrefix(dir, filepath.Join(home, "memory", "principals", "reviewer")+string(filepath.Separator)) || strings.Contains(dir, "alice") {
		t.Fatalf("alice dir = %q, %v", dir, err)
	}
	if _, err := s.Dir(fw.MemoryScope{Kind: fw.MemoryAgentScope, Agent: "../x"}); err == nil {
		t.Fatal("a bad agent name named a directory")
	}
	if _, err := New(&fw.Config{Tools: fw.ToolsConfig{Memory: fw.MemoryToolConfig{Scope: "team"}}}); err == nil {
		t.Fatal("an unknown scope was accepted")
	}
}

func TestCheckName(t *testing.T) {
	for _, ok := range []string{"a.md", "dir/a.md", "a..b.md"} {
		if err := CheckName(ok); err != nil {
			t.Errorf("CheckName(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", ".", ".a", "dir/.a", ".dir/a", "../a", "/a", "a//b"} {
		if err := CheckName(bad); err == nil {
			t.Errorf("CheckName(%q) accepted a bad name", bad)
		}
	}
}

// TestNoteRefStaysInScope checks that a ref cannot reach a note of another
// agent, another scope, or outside the directory.
func TestNoteRefStaysInScope(t *testing.T) {
	scope := fw.MemoryScope{Kind: fw.MemoryAgentScope, Agent: "beta"}
	if name, err := noteName(scope, "mem://agent/beta/dir/a.md"); err != nil || name != "dir/a.md" {
		t.Fatalf("own ref = %q, %v", name, err)
	}
	for _, ref := range []string{
		"mem://agent/alpha/a.md",
		"mem://shared/a.md",
		"mem://principal/beta/a.md",
		"mem://agent/beta/../alpha/a.md",
		"mem://agent/beta/.hidden",
		"mem://agent/beta",
		"run://x",
	} {
		if _, err := noteName(scope, ref); err == nil {
			t.Errorf("noteName(%q) left the scope", ref)
		}
	}
}

func TestRememberGetForget(t *testing.T) {
	ctx := context.Background()
	s, home := newStore(t, "")
	scope := s.Scope(fw.RunInfo{Agent: "reviewer"})
	ref := NoteRef(scope, "build.md")

	rec, err := s.Remember(ctx, scope, fw.MemoryRecord{Ref: ref, Body: "How to build\nmake\n"}, "absent")
	if err != nil || rec.Hash == "" || rec.Title != "How to build" || rec.Size != 18 {
		t.Fatalf("remember = %+v, %v", rec, err)
	}
	if _, err := s.Remember(ctx, scope, fw.MemoryRecord{Ref: ref, Body: "x"}, "absent"); !errors.Is(err, fw.ErrMemoryConflict) {
		t.Fatalf("a second absent write = %v", err)
	}
	if _, err := s.Remember(ctx, scope, fw.MemoryRecord{Ref: ref, Body: "x"}, "wrong"); !errors.Is(err, fw.ErrMemoryConflict) {
		t.Fatalf("a stale write = %v", err)
	}
	next, err := s.Remember(ctx, scope, fw.MemoryRecord{Ref: ref, Body: "How to build\nmake build\n"}, rec.Hash)
	if err != nil || next.Hash == rec.Hash {
		t.Fatalf("a current write = %+v, %v", next, err)
	}
	got, err := s.Get(ctx, scope, ref)
	if err != nil || got.Body != "How to build\nmake build\n" || got.Hash != next.Hash {
		t.Fatalf("get = %+v, %v", got, err)
	}
	// The atomic write leaves no temporary file behind.
	dir := filepath.Join(home, "memory", "agents", "reviewer")
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "build.md" {
		t.Fatalf("directory = %v", entries)
	}
	info, _ := os.Stat(dir)
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode = %v", info.Mode())
	}
	list, err := s.List(ctx, scope)
	if err != nil || len(list) != 1 || list[0].Ref != ref || list[0].Body != "" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if err := s.Forget(ctx, scope, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, scope, ref); !errors.Is(err, fw.ErrMemoryNotFound) {
		t.Fatalf("get after forget = %v", err)
	}
	if err := s.Forget(ctx, scope, ref); !errors.Is(err, fw.ErrMemoryNotFound) {
		t.Fatalf("a second forget = %v", err)
	}
}

// TestNotesRecordsChanges checks that each write through Notes records one
// change without the content.
func TestNotesRecordsChanges(t *testing.T) {
	s, _ := newStore(t, "")
	scope := s.Scope(fw.RunInfo{Agent: "reviewer"})
	var got []fw.MemoryChangedData
	ctx := fw.WithMemoryRecorder(context.Background(), func(d fw.MemoryChangedData) { got = append(got, d) })
	files, err := s.Notes(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if err := files.WriteFile("a.md", []byte("secret text")); err != nil {
		t.Fatal(err)
	}
	if err := files.WriteFile("a.md", []byte("other text")); err != nil {
		t.Fatal(err)
	}
	if err := files.Rename("a.md", "b.md"); err != nil {
		t.Fatal(err)
	}
	if err := files.Remove("b.md"); err != nil {
		t.Fatal(err)
	}
	ops := []string{}
	for _, d := range got {
		ops = append(ops, d.Op)
		if strings.Contains(fmt.Sprint(d), "secret") {
			t.Fatalf("a change holds the content: %+v", d)
		}
	}
	if strings.Join(ops, ",") != "created,changed,renamed,removed" {
		t.Fatalf("ops = %v", ops)
	}
	if got[0].Ref != "mem://agent/reviewer/a.md" || got[0].Size != 11 || len(got[0].SHA256) != 64 || got[2].OldRef != "mem://agent/reviewer/a.md" {
		t.Fatalf("changes = %+v", got)
	}
	if err := files.WriteFile(".x", []byte("x")); err == nil {
		t.Fatal("a hidden note was written")
	}
	if err := files.WriteFile("big", make([]byte, MaxNoteBytes+1)); !errors.Is(err, ErrLimit) {
		t.Fatalf("an oversize note = %v", err)
	}
}

func TestMaxNotes(t *testing.T) {
	s, home := newStore(t, "")
	scope := s.Scope(fw.RunInfo{})
	dir := filepath.Join(home, "memory", "agents", "default")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range MaxNotes {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("n%04d.md", i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files, err := s.Notes(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if err := files.WriteFile("one-more.md", []byte("x")); !errors.Is(err, ErrLimit) {
		t.Fatalf("a note past the limit = %v", err)
	}
	if err := files.WriteFile("n0000.md", []byte("updated")); err != nil {
		t.Fatalf("an update at the limit = %v", err)
	}
}

func TestSearchAndContext(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t, "")
	scope := s.Scope(fw.RunInfo{Agent: "reviewer"})
	if text, err := s.ContextText(ctx, scope); err != nil || text != "" {
		t.Fatalf("empty context = %q, %v", text, err)
	}
	for name, body := range map[string]string{
		"build.md": "---\ndescription: How to build\n---\nRun make build.\n",
		"prefs.md": "The user prefers short answers.\n",
	} {
		if _, err := s.Remember(ctx, scope, fw.MemoryRecord{Ref: NoteRef(scope, name), Body: body}, ""); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := s.Search(ctx, scope, fw.MemoryQuery{Text: "make", Kinds: []fw.MemoryKind{fw.MemoryKindNote}})
	if err != nil || len(hits) != 1 || hits[0].Record.Ref != "mem://agent/reviewer/build.md" || !strings.Contains(hits[0].Snippet, "make build") {
		t.Fatalf("hits = %+v, %v", hits, err)
	}
	if hits[0].Why != "scan" && hits[0].Why != "fts5" {
		t.Fatalf("why = %q", hits[0].Why)
	}
	if _, err := s.Search(ctx, scope, fw.MemoryQuery{Text: "  "}); err == nil {
		t.Fatal("an empty search was accepted")
	}
	text, err := s.ContextText(ctx, scope)
	if err != nil || !strings.HasPrefix(text, ContextList+"\n") || !strings.Contains(text, "- /memories/build.md: How to build") {
		t.Fatalf("context = %q, %v", text, err)
	}
	other := s.Scope(fw.RunInfo{Agent: "other"})
	if hits, _ := s.Search(ctx, other, fw.MemoryQuery{Text: "make"}); len(hits) != 0 {
		t.Fatalf("another agent found %+v", hits)
	}
	if _, err := ParseKinds([]string{"note", "files"}); err == nil {
		t.Fatal("an unknown kind was accepted")
	}
	var _ fs.FS = (fw.Files)(nil)
}

// TestRememberIsAtomic checks that two writers with the same condition never
// both succeed: the lock of the scope holds from the read to the write.
func TestRememberIsAtomic(t *testing.T) {
	ctx := context.Background()
	s, home := newStore(t, "")
	scope := s.Scope(fw.RunInfo{Agent: "reviewer"})
	ref := NoteRef(scope, "race.md")
	for round := range 20 {
		_ = s.Forget(ctx, scope, ref)
		// Another Store over the same home contends on the same lock.
		other, err := New(&fw.Config{Home: home})
		if err != nil {
			t.Fatal(err)
		}
		var wins atomic.Int32
		var wg sync.WaitGroup
		for i := range 8 {
			st := s
			if i%2 == 1 {
				st = other
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := st.Remember(ctx, scope, fw.MemoryRecord{Ref: ref, Body: fmt.Sprint("writer ", i)}, "absent"); err == nil {
					wins.Add(1)
				} else if !errors.Is(err, fw.ErrMemoryConflict) {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		other.Close()
		if n := wins.Load(); n != 1 {
			t.Fatalf("round %d: %d writers won an absent write, want 1", round, n)
		}
	}
	// "present" writes only a note that exists.
	if _, err := s.Remember(ctx, scope, fw.MemoryRecord{Ref: NoteRef(scope, "none.md"), Body: "x"}, "present"); !errors.Is(err, fw.ErrMemoryConflict) {
		t.Fatalf("present write of a missing note = %v", err)
	}
	if _, err := s.Remember(ctx, scope, fw.MemoryRecord{Ref: ref, Body: "y"}, "present"); err != nil {
		t.Fatalf("present write of a note = %v", err)
	}
	// Forget refuses a directory.
	if _, err := s.Remember(ctx, scope, fw.MemoryRecord{Ref: NoteRef(scope, "dir/a.md"), Body: "x"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Forget(ctx, scope, NoteRef(scope, "dir")); !errors.Is(err, fw.ErrInvalidRequest) {
		t.Fatalf("forget of a directory = %v", err)
	}
}

// TestRecallRunsOfThePrincipal checks that a search over runs never shows a
// run of another principal.
func TestRecallRunsOfThePrincipal(t *testing.T) {
	ctx := context.Background()
	s, home := newStore(t, "principal")
	cfg := fw.DefaultConfig()
	cfg.Home = home
	cfg.Tools.Files.Root = t.TempDir()
	model := fw.ModelFunc{StreamFn: func(_ context.Context, req *fw.Request, _ func(fw.Delta)) (*fw.Response, error) {
		return &fw.Response{Content: []fw.Block{fw.Text("done: " + fw.TextOf(req.Messages[len(req.Messages)-1].Content))}, StopReason: fw.StopEndTurn}, nil
	}}
	a, err := fw.New(fw.WithoutDefaults(), fw.WithConfig(cfg), fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()),
		fw.WithAuthn(fw.NoAuthn{}), fw.WithModel(model))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	for _, who := range []string{"alice", "bob"} {
		pctx := fw.WithPrincipal(ctx, &fw.Principal{Subject: who, Kind: fw.KindUser})
		if _, err := a.Run(pctx, "the zeppelin plan of "+who); err != nil {
			t.Fatal(err)
		}
	}
	actx := fw.WithAgent(ctx, a)
	alice := s.Scope(fw.RunInfo{Agent: "default", Principal: "alice"})
	hits, err := s.Search(actx, alice, fw.MemoryQuery{Text: "zeppelin", Kinds: []fw.MemoryKind{fw.MemoryKindRun}})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || !strings.Contains(hits[0].Snippet, "alice") {
		t.Fatalf("alice sees %+v", hits)
	}
	if _, err := s.Get(actx, alice, hits[0].Record.Ref); err != nil {
		t.Fatalf("alice reads her run: %v", err)
	}
	bobHits, _ := s.Search(actx, s.Scope(fw.RunInfo{Agent: "default", Principal: "bob"}), fw.MemoryQuery{Text: "zeppelin", Kinds: []fw.MemoryKind{fw.MemoryKindRun}})
	if len(bobHits) != 1 || bobHits[0].Record.Ref == hits[0].Record.Ref {
		t.Fatalf("bob sees %+v", bobHits)
	}
	if _, err := s.Get(actx, s.Scope(fw.RunInfo{Agent: "default", Principal: "bob"}), hits[0].Record.Ref); !errors.Is(err, fw.ErrMemoryNotFound) {
		t.Fatalf("bob reads alice's run: %v", err)
	}
}
