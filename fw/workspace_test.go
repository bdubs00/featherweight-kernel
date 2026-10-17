package fw

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"testing/fstest"
)

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDirFiles(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.txt"), "alpha\n")
	mustWrite(t, filepath.Join(dir, "sub", "b.txt"), "beta\n")
	f, err := DirFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := fstest.TestFS(f, "a.txt", "sub/b.txt"); err != nil {
		t.Fatal(err)
	}
	if err := f.WriteFile("new/deep/c.txt", []byte("gamma")); err != nil {
		t.Fatal(err)
	}
	if err := f.Rename("new/deep/c.txt", "a.txt"); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("rename onto a file = %v", err)
	}
	if err := f.Rename("new/deep/c.txt", "moved/c.txt"); err != nil {
		t.Fatal(err)
	}
	if b, err := fs.ReadFile(f, "moved/c.txt"); err != nil || string(b) != "gamma" {
		t.Fatalf("moved = %q, %v", b, err)
	}
	if err := f.Remove("."); err == nil {
		t.Fatal("Remove(.) worked")
	}
	if err := f.Remove("sub"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Stat("sub/b.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("removed tree: %v", err)
	}
	for _, bad := range []string{"../x", "/etc/passwd", "a/../../x", ""} {
		if _, err := f.Open(bad); err == nil {
			t.Errorf("Open(%q) worked", bad)
		}
	}
	// A link that leaves the directory leads nowhere.
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "secret"), "s")
	if err := os.Symlink(outside, filepath.Join(dir, "out")); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.ReadFile(f, "out/secret"); err == nil {
		t.Fatal("read through a link outside the directory")
	}
	if _, err := RealName(f, "out/secret"); !errors.Is(err, ErrFileOutside) {
		t.Fatalf("RealName through a link outside = %v", err)
	}
}

func TestDirFilesKeepsPrivateModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f, _ := DirFiles(dir)
	if err := f.WriteFile("notes/a.md", []byte("x")); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]fs.FileMode{"notes": 0o700, "notes/a.md": 0o600} {
		info, err := os.Stat(filepath.Join(dir, p))
		if err != nil || info.Mode().Perm() != want {
			t.Errorf("%s: mode %v, %v, want %v", p, info.Mode().Perm(), err, want)
		}
	}
}

func TestMatchRule(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{".env", ".env", true},
		{".env", "app/.env", true},
		{".env", "app/.envrc", false},
		{".env.*", "app/.env.production", true},
		{"*.pem", "certs/web.pem", true},
		{".git", ".git/HEAD", true},
		{".git", "app/.git/refs/heads/main", true},
		{".git", "app/.gitignore", false},
		{"**/.git/config", ".git/config", true},
		{"**/.git/config", "app/.git/config", true},
		{"**/.git/config", "app/.git/config.lock", false},
		{"secrets/", "secrets/key", true},
		{"secrets/*", "secrets/key", true},
		{"secrets/*", "app/secrets/key", false},
		{"/build", "build/out.o", true},
		{"docs/**/draft.md", "docs/a/b/draft.md", true},
		{".env", ".", false},
	}
	for _, c := range cases {
		if got := matchRule(c.pattern, c.name); got != c.want {
			t.Errorf("matchRule(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func testWorkspace(t *testing.T) (*workspace, string) {
	t.Helper()
	root := t.TempDir()
	cfg := DefaultConfig()
	cfg.Home = filepath.Join(root, ".fw")
	cfg.Tools.Files.MaxFileBytes = 64
	inner, err := DirFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	w, err := newWorkspace(inner, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return w, root
}

func TestWorkspaceRules(t *testing.T) {
	w, root := testWorkspace(t)
	mustWrite(t, filepath.Join(root, ".fw", "agents", "dev.yaml"), "name: dev\n")
	mustWrite(t, filepath.Join(root, ".env"), "TOKEN=x\n")
	mustWrite(t, filepath.Join(root, "app", ".git", "config"), "[core]\n")
	mustWrite(t, filepath.Join(root, "app", ".git", "HEAD"), "ref: main\n")
	mustWrite(t, filepath.Join(root, "app", "main.go"), "package main\n")
	if err := os.Symlink(filepath.Join(root, ".fw"), filepath.Join(root, "state")); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		do   func() error
		want error
	}{
		{"read a profile", func() error { _, err := fs.ReadFile(w, ".fw/agents/dev.yaml"); return err }, ErrFileReserved},
		{"write a profile through a link", func() error { return w.WriteFile("state/agents/x.yaml", []byte("x")) }, ErrFileReserved},
		{"read .env", func() error { _, err := fs.ReadFile(w, ".env"); return err }, ErrFileDenied},
		{"read .git/config", func() error { _, err := fs.ReadFile(w, "app/.git/config"); return err }, ErrFileDenied},
		{"write .git", func() error { return w.WriteFile("app/.git/HEAD", []byte("x")) }, ErrFileReadOnly},
		{"rename into .git", func() error { return w.Rename("app/main.go", "app/.git/main.go") }, ErrFileReadOnly},
		{"remove a profile", func() error { return w.Remove(".fw/agents") }, ErrFileReserved},
		{"write a large file", func() error { return w.WriteFile("big.txt", make([]byte, 65)) }, ErrFileTooLarge},
	}
	for _, c := range cases {
		if err := c.do(); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	if b, err := fs.ReadFile(w, "app/.git/HEAD"); err != nil || string(b) != "ref: main\n" {
		t.Errorf("read .git/HEAD = %q, %v", b, err)
	}
	entries, err := w.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"app"}) {
		t.Errorf("listing = %v, want only app", names)
	}
}

func TestWorkspaceRefusesBadPatterns(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Tools.Files.Deny = []string{"[unclosed"}
	inner, _ := DirFiles(t.TempDir())
	if _, err := NewWorkspace(inner, cfg); err == nil {
		t.Fatal("a bad pattern passed")
	}
}

func TestWorkspaceRecordsChanges(t *testing.T) {
	w, root := testWorkspace(t)
	var got []FileChangedData
	b := w.bind(func(d FileChangedData) { got = append(got, d) })
	mustWrite(t, filepath.Join(root, "real", "a.txt"), "old")
	if err := os.Symlink("real", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	steps := []func() error{
		func() error { return b.WriteFile("link/a.txt", []byte("new")) },
		func() error { return b.WriteFile("b.txt", []byte("bee")) },
		func() error { return b.Rename("b.txt", "c.txt") },
		func() error { return b.Remove("c.txt") },
	}
	for _, s := range steps {
		if err := s(); err != nil {
			t.Fatal(err)
		}
	}
	sum := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	want := []FileChangedData{
		{Op: FileChanged, Path: "real/a.txt", Size: 3, SHA256: sum("new")},
		{Op: FileCreated, Path: "b.txt", Size: 3, SHA256: sum("bee")},
		{Op: FileRenamed, Path: "c.txt", OldPath: "b.txt", Size: 3, SHA256: sum("bee")},
		{Op: FileRemoved, Path: "c.txt"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("changes:\n got %+v\nwant %+v", got, want)
	}
	if err := w.WriteFile("quiet.txt", []byte("q")); err != nil || len(got) != 4 {
		t.Fatalf("the unbound workspace recorded a change: %v, %d", err, len(got))
	}
}

func TestRunFiles(t *testing.T) {
	ev := func(seq uint64, d FileChangedData) Event {
		e := MustEvent("r", EvFileChanged, d)
		e.Seq = seq
		return e
	}
	files := RunFiles([]Event{
		ev(1, FileChangedData{Op: FileCreated, Path: "a.md", Size: 1, SHA256: "1"}),
		MustEvent("r", EvToolProgress, ToolProgressData{}),
		ev(3, FileChangedData{Op: FileCreated, Path: "b.md", Size: 2, SHA256: "2"}),
		ev(4, FileChangedData{Op: FileChanged, Path: "a.md", Size: 3, SHA256: "3"}),
		ev(5, FileChangedData{Op: FileRenamed, Path: "c.md", OldPath: "b.md", Size: 2, SHA256: "2"}),
	})
	var got []string
	for _, f := range files {
		got = append(got, f.Path+" "+f.Op+" "+f.SHA256)
	}
	want := []string{"a.md created 3", "b.md removed ", "c.md renamed 2"}
	if !slices.Equal(got, want) {
		t.Fatalf("RunFiles = %q, want %q", got, want)
	}
}

func TestToolCallRecordsFileChanges(t *testing.T) {
	ctx := testCtx(t)
	root := t.TempDir()
	cfg := DefaultConfig()
	cfg.Home = filepath.Join(root, ".fw")
	cfg.Tools.Files.Root = root
	writer := NewTool(ToolSpec{Name: "writer"}, func(ctx context.Context, in json.RawMessage) (ToolResult, error) {
		f := FilesFrom(ctx)
		if f == nil {
			return ErrorResult("no workspace"), nil
		}
		if err := f.WriteFile("notes/plan.md", []byte("# Plan\n")); err != nil {
			return ErrorResult(err.Error()), nil
		}
		if err := f.WriteFile(".fw/agents/evil.yaml", []byte("x")); !errors.Is(err, ErrFileReserved) {
			return ErrorResult("the reserved write passed: " + err.Error()), nil
		}
		return TextResult("written"), nil
	})
	s := newScript(callTools(use("c1", "writer", `{}`)), endTurn("done"))
	a := newTestAgent(t, s.Model(), WithConfig(cfg), WithTools(writer))
	r, err := a.Run(ctx, "write the plan")
	mustState(t, r, err, StateCompleted)
	if got := TextOf(lastToolResults(t, s.requests()[1])[0].Content); got != "written" {
		t.Fatalf("tool result = %q", got)
	}
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	var d FileChangedData
	firstOf(t, evs, EvFileChanged, &d)
	if d.CallID != "c1" || d.Tool != "writer" || d.Op != FileCreated || d.Path != "notes/plan.md" || d.Size != 7 || d.SHA256 == "" {
		t.Fatalf("event = %+v", d)
	}
	if b, err := os.ReadFile(filepath.Join(root, "notes", "plan.md")); err != nil || string(b) != "# Plan\n" {
		t.Fatalf("file = %q, %v", b, err)
	}
	if files := RunFiles(evs); len(files) != 1 || files[0].Path != "notes/plan.md" {
		t.Fatalf("RunFiles = %+v", files)
	}
}
