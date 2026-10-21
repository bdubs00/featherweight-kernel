//go:build memory_sqlite

package memory

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// TestFTSIndex checks that the FTS5 index finds a note by a word of its
// body, follows a change and a removal of a note, and keeps the notes
// canonical: a database of another kind is built again from the notes.
func TestFTSIndex(t *testing.T) {
	ctx := context.Background()
	s, home := newStore(t, "")
	if IndexDriver != "sqlite-fts5" {
		t.Fatalf("index driver = %q", IndexDriver)
	}
	scope := s.Scope(fw.RunInfo{Agent: "reviewer"})
	dir := filepath.Join(home, "memory", "agents", "reviewer")
	write := func(name, body string, age time.Duration) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		mod := time.Now().Add(-age)
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	search := func(text string) []fw.MemoryHit {
		t.Helper()
		hits, err := s.Search(ctx, scope, fw.MemoryQuery{Text: text, Kinds: []fw.MemoryKind{fw.MemoryKindNote}})
		if err != nil {
			t.Fatal(err)
		}
		return hits
	}
	write("build.md", "How to build\nRun the zeppelin target.\n", time.Hour)
	write("sub/deploy.md", "Deploy\nUse the blimp pipeline.\n", time.Hour)

	hits := search("zeppelin")
	if len(hits) != 1 || hits[0].Why != "fts5" || hits[0].Record.Ref != "mem://agent/reviewer/build.md" || hits[0].Record.Title != "How to build" {
		t.Fatalf("hits = %+v", hits)
	}
	if hits := search(`blimp "quoted" OR NOT`); len(hits) != 1 || hits[0].Record.Ref != "mem://agent/reviewer/sub/deploy.md" {
		t.Fatalf("a query with FTS5 syntax = %+v", hits)
	}

	write("build.md", "How to build\nRun the airship target.\n", 0)
	if hits := search("zeppelin"); len(hits) != 0 {
		t.Fatalf("a changed note still matches its old text: %+v", hits)
	}
	if hits := search("airship"); len(hits) != 1 {
		t.Fatalf("a changed note does not match its new text: %+v", hits)
	}
	if err := os.Remove(filepath.Join(dir, "sub", "deploy.md")); err != nil {
		t.Fatal(err)
	}
	if hits := search("blimp"); len(hits) != 0 {
		t.Fatalf("a removed note still matches: %+v", hits)
	}

	// A database that is not of this index is replaced, never written into.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(home, "memory", "index", "agents", "reviewer", "notes.db")
	if err := os.WriteFile(db, []byte("not a database of the index"), 0o600); err != nil {
		t.Fatal(err)
	}
	if hits := search("airship"); len(hits) != 1 || hits[0].Why != "fts5" {
		t.Fatalf("after a bad database = %+v", hits)
	}
}

// TestFTSIndexForeignDatabase checks that a SQLite database that is not of
// the index, with the marks of an empty file and a table of its own, is
// built again from the notes and never written into.
func TestFTSIndexForeignDatabase(t *testing.T) {
	ctx := context.Background()
	s, home := newStore(t, "")
	scope := s.Scope(fw.RunInfo{Agent: "reviewer"})
	if _, err := s.Remember(ctx, scope, fw.MemoryRecord{Ref: NoteRef(scope, "a.md"), Body: "the zeppelin note\n"}, ""); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "memory", "index", "agents", "reviewer")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "notes.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE notes (other TEXT)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	hits, err := s.Search(ctx, scope, fw.MemoryQuery{Text: "zeppelin", Kinds: []fw.MemoryKind{fw.MemoryKindNote}})
	if err != nil || len(hits) != 1 || hits[0].Why != "fts5" {
		t.Fatalf("hits over a foreign database = %+v, %v", hits, err)
	}
}

// TestFTSIndexSeveralStores checks that several Stores of one process, as
// the tools, the native API, and a harness build, search one index at once
// without an error, and that each search uses the index.
func TestFTSIndexSeveralStores(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	cfg := &fw.Config{Home: home}
	first, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	scope := first.Scope(fw.RunInfo{Agent: "reviewer"})
	for i := range 20 {
		if _, err := first.Remember(ctx, scope, fw.MemoryRecord{Ref: NoteRef(scope, fmt.Sprintf("n%02d.md", i)), Body: fmt.Sprintf("note %d of the zeppelin\n", i)}, ""); err != nil {
			t.Fatal(err)
		}
	}
	for round := range 10 {
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for i := range 4 {
			st, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer st.Close()
				if i == 0 {
					// A writer changes a note while the others search.
					if _, err := st.Remember(ctx, scope, fw.MemoryRecord{Ref: NoteRef(scope, "n00.md"), Body: fmt.Sprintf("round %d of the zeppelin\n", round)}, ""); err != nil {
						errs <- err
					}
				}
				hits, err := st.index.Search(ctx, scope, filepath.Join(home, "memory", "agents", "reviewer"), filepath.Join(home, "memory", "index", "agents", "reviewer"), []string{"zeppelin"}, 50)
				if err != nil {
					errs <- err
					return
				}
				if len(hits) != 20 {
					errs <- fmt.Errorf("round %d: %d hits, want 20", round, len(hits))
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
	}
}

// TestFTSIndexSeesASameSizeRewrite checks that a rewrite of a note with the
// same size and time is not missed: the atomic write gives the note a new
// inode.
func TestFTSIndexSeesASameSizeRewrite(t *testing.T) {
	ctx := context.Background()
	s, home := newStore(t, "")
	scope := s.Scope(fw.RunInfo{Agent: "reviewer"})
	ref := NoteRef(scope, "a.md")
	search := func(word string) int {
		t.Helper()
		hits, err := s.Search(ctx, scope, fw.MemoryQuery{Text: word, Kinds: []fw.MemoryKind{fw.MemoryKindNote}})
		if err != nil {
			t.Fatal(err)
		}
		return len(hits)
	}
	if _, err := s.Remember(ctx, scope, fw.MemoryRecord{Ref: ref, Body: "alpha\n"}, ""); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(home, "memory", "agents", "reviewer", "a.md")
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if search("alpha") != 1 {
		t.Fatal("the first text is not indexed")
	}
	if _, err := s.Remember(ctx, scope, fw.MemoryRecord{Ref: ref, Body: "gamma\n"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if search("gamma") != 1 || search("alpha") != 0 {
		t.Fatal("a rewrite of the same size and time was missed")
	}
}

// TestFTSForgetRemovesTheText checks that Forget and a delete of the tool
// take a note out of the index at once, not at the next search, that the
// database runs with secure delete, and that the words of the note leave
// the file.
func TestFTSForgetRemovesTheText(t *testing.T) {
	ctx := context.Background()
	s, home := newStore(t, "")
	scope := s.Scope(fw.RunInfo{Agent: "reviewer"})
	for name, body := range map[string]string{
		"secret.md":     "the quokkavortex plan\n",
		"keep.md":       "the zeppelin plan\n",
		"dir/inner.md":  "the wombatcyclone plan\n",
		"dir2/other.md": "the zeppelin other plan\n",
	} {
		if _, err := s.Remember(ctx, scope, fw.MemoryRecord{Ref: NoteRef(scope, name), Body: body}, ""); err != nil {
			t.Fatal(err)
		}
	}
	if hits, err := s.Search(ctx, scope, fw.MemoryQuery{Text: "quokkavortex wombatcyclone zeppelin", Kinds: []fw.MemoryKind{fw.MemoryKindNote}}); err != nil || len(hits) != 4 || hits[0].Why != "fts5" {
		t.Fatalf("hits before forget = %+v, %v", hits, err)
	}
	if err := s.Forget(ctx, scope, NoteRef(scope, "secret.md")); err != nil {
		t.Fatal(err)
	}
	files, err := s.Notes(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if err := files.Remove("dir"); err != nil {
		t.Fatal(err)
	}
	x := s.index.(*ftsIndex)
	dir := filepath.Join(home, "memory", "index", "agents", "reviewer")
	x.mu.Lock()
	db := x.dbs[dir]
	x.mu.Unlock()
	if db == nil {
		t.Fatal("the index of the scope is not open")
	}
	var paths []string
	rows, err := db.QueryContext(ctx, `SELECT path FROM notes ORDER BY path`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	rows.Close()
	if fmt.Sprint(paths) != "[dir2/other.md keep.md]" {
		t.Fatalf("rows after forget = %v", paths)
	}
	var secure int
	if err := db.QueryRowContext(ctx, `PRAGMA secure_delete`).Scan(&secure); err != nil || secure != 1 {
		t.Fatalf("secure_delete = %d, %v", secure, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "notes.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, word := range []string{"quokkavortex", "wombatcyclone"} {
		if strings.Contains(string(raw), word) {
			t.Errorf("the database file still holds %q", word)
		}
	}
	if !strings.Contains(string(raw), "zeppelin") {
		t.Error("the database file lost the text of a kept note")
	}
}

// TestFTSRemoveCreatesNoIndex checks that a removal in a scope that has
// no index creates none.
func TestFTSRemoveCreatesNoIndex(t *testing.T) {
	ctx := context.Background()
	s, home := newStore(t, "")
	scope := s.Scope(fw.RunInfo{Agent: "reviewer"})
	ref := NoteRef(scope, "a.md")
	if _, err := s.Remember(ctx, scope, fw.MemoryRecord{Ref: ref, Body: "x"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Forget(ctx, scope, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "memory", "index", "agents", "reviewer", "notes.db")); !os.IsNotExist(err) {
		t.Fatalf("a forget created the index: %v", err)
	}
}
