package fw

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
)

// perRunAgent returns an agent with tools.files.perRun on, a workspace at
// the returned root, its home outside the root, and a tool "touch" that
// writes the file of its input through the workspace of its call and
// records the directory that the kernel names.
func perRunAgent(t *testing.T, s *scriptModel, dirs *sync.Map) (*Agent, string) {
	t.Helper()
	root := t.TempDir()
	cfg := DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Tools.Files.Root = root
	on := true
	cfg.Tools.Files.PerRun = &on
	touch := NewTool(ToolSpec{Name: "touch", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(ctx context.Context, input json.RawMessage) (ToolResult, error) {
		var in struct{ Path string }
		_ = json.Unmarshal(input, &in)
		info := RunInfoFrom(ctx)
		dirs.Store(info.RunID, info.Dir)
		files := FilesFrom(ctx)
		if _, err := files.Stat("from-the-other-run"); err == nil {
			return ErrorResult("a run sees the file of another run"), nil
		}
		if err := files.WriteFile(in.Path, []byte(info.RunID)); err != nil {
			return ErrorResult(err.Error()), nil
		}
		return TextResult("ok"), nil
	})
	return newTestAgent(t, s.Model(), WithConfig(cfg), WithTools(touch)), root
}

// Each run gets a directory of its own under the root, and a file that one
// run writes is not in the workspace of the next run. The ledger records
// the path under the root, so the files of a run read through Files.
func TestPerRunDirectories(t *testing.T) {
	ctx := testCtx(t)
	var dirs sync.Map
	s := newScript(
		callTools(use("c1", "touch", `{"path":"from-the-other-run"}`)), endTurn("one"),
		callTools(use("c2", "touch", `{"path":"out/second.txt"}`)), endTurn("two"),
	)
	a, root := perRunAgent(t, s, &dirs)
	first, err := a.Run(ctx, "first")
	mustState(t, first, err, StateCompleted)
	second, err := a.Run(ctx, "second")
	mustState(t, second, err, StateCompleted)
	if res := lastToolResults(t, s.requests()[3]); res[0].IsError {
		t.Fatalf("second run: %s", TextOf(res[0].Content))
	}
	for _, r := range []*Run{first, second} {
		got, _ := dirs.Load(r.ID)
		if want := filepath.Join(root, RunsDir, r.ID); got != want {
			t.Errorf("run %s worked in %v, want %s", r.ID, got, want)
		}
	}
	b, err := os.ReadFile(filepath.Join(root, RunsDir, second.ID, "out", "second.txt"))
	if err != nil || string(b) != second.ID {
		t.Fatalf("the file of the second run = %q, %v", b, err)
	}
	var changed FileChangedData
	firstOf(t, ledgerEvents(t, a.ledger, second.ID), EvFileChanged, &changed)
	if want := path.Join(RunsDir, second.ID, "out/second.txt"); changed.Path != want || changed.Op != FileCreated {
		t.Fatalf("fw.file.changed = %+v, want the path %s", changed, want)
	}
	if b, err := fs.ReadFile(a.Files(), changed.Path); err != nil || string(b) != second.ID {
		t.Fatalf("Files reads %q, %v", b, err)
	}
}

// A child run and a branch work in the directory of their tree; a run
// with FilesRoot works in the root; metadata that names a source without a
// fork gets a directory of its own.
func TestRunDirOfChildBranchAndRoot(t *testing.T) {
	ctx := testCtx(t)
	var dirs sync.Map
	s := newScript(endTurn("parent"), endTurn("child"), endTurn("root"), endTurn("forged"), endTurn("branch"))
	a, _ := perRunAgent(t, s, &dirs)
	parent, err := a.Run(ctx, "parent")
	mustState(t, parent, err, StateCompleted)
	child, err := a.Run(ctx, "child", WithParent(parent.ID))
	mustState(t, child, err, StateCompleted)
	root, err := a.Run(ctx, "root", InFilesRoot())
	mustState(t, root, err, StateCompleted)
	forged, err := a.Run(ctx, "forged", WithMetadata(map[string]string{MetaForkOf: parent.ID}))
	mustState(t, forged, err, StateCompleted)
	branch, err := a.Fork(ctx, parent.ID, ForkRequest{Input: "again"})
	if err != nil {
		t.Fatal(err)
	}
	dir := func(id string) string {
		rs, err := a.k.lookup(id)
		if err != nil {
			t.Fatal(err)
		}
		return a.dirOf(rs)
	}
	want := path.Join(RunsDir, parent.ID)
	for name, got := range map[string]string{"parent": dir(parent.ID), "child": dir(child.ID), "branch": dir(branch.ID)} {
		if got != want {
			t.Errorf("%s works in %q, want %q", name, got, want)
		}
	}
	if got := dir(root.ID); got != "" {
		t.Errorf("a run with FilesRoot works in %q", got)
	}
	if got := dir(forged.ID); got != path.Join(RunsDir, forged.ID) {
		t.Errorf("a run that names a source in its metadata works in %q", got)
	}
	a.cfg.Tools.Files.PerRun = nil
	rs, _ := a.k.lookup(forged.ID)
	rs.mu.Lock()
	rs.dirSet = false
	rs.mu.Unlock()
	if got := a.dirOf(rs); got != "" {
		t.Errorf("without perRun a run works in %q", got)
	}
}

// An attachment of a run lands in the directory of the run, and the label
// names its path in that directory, where the tools of the run find it.
func TestPerRunAttachments(t *testing.T) {
	ctx := testCtx(t)
	var dirs sync.Map
	s := newScript(endTurn("seen"))
	a, root := perRunAgent(t, s, &dirs)
	r, err := a.Run(ctx, "look", WithAttachments(Attachment("notes.txt", []byte("hello\n"))))
	mustState(t, r, err, StateCompleted)
	if _, err := os.Stat(filepath.Join(root, RunsDir, r.ID, AttachmentsDir, "notes.txt")); err != nil {
		t.Fatal(err)
	}
	label := s.requests()[0].Messages[0].Content[1].Text
	if !strings.Contains(label, "It is in the workspace at attachments/notes.txt.") {
		t.Fatalf("label = %q", label)
	}
	files := RunFiles(ledgerEvents(t, a.ledger, r.ID))
	if len(files) != 1 || files[0].Path != path.Join(RunsDir, r.ID, AttachmentsDir, "notes.txt") {
		t.Fatalf("files of the run = %+v", files)
	}
}

// The workspace of a directory keeps the rules of the workspace, maps the
// reserved paths, and records changes under the root.
func TestWorkspaceSub(t *testing.T) {
	root := t.TempDir()
	cfg := DefaultConfig()
	cfg.Home = filepath.Join(root, "runs", "r1", "state")
	dir, err := DirFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	w, err := newWorkspace(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var got []FileChangedData
	sub := w.sub("runs/r1")
	if !slices.Equal(sub.reserved, []string{"state"}) || sub.prefix != "runs/r1" {
		t.Fatalf("reserved %v, prefix %q", sub.reserved, sub.prefix)
	}
	if w.sub("").prefix != "" || len(w.sub("").reserved) != len(w.reserved) {
		t.Fatal("sub of the root changed the workspace")
	}
	if inner := w.sub("runs/r1/state/x").reserved; !slices.Equal(inner, []string{"."}) {
		t.Fatalf("a directory in a reserved path = %v", inner)
	}
	if err := (&dirFiles{dir: root}).makeSub("runs/r1", nil); err != nil {
		t.Fatal(err)
	}
	bound := sub.bind(func(d FileChangedData) { got = append(got, d) })
	if err := bound.WriteFile("a.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := bound.Rename("a.txt", "b.txt"); err != nil {
		t.Fatal(err)
	}
	if err := bound.WriteFile("state/x", []byte("x")); !errors.Is(err, ErrFileReserved) {
		t.Fatalf("a write into the home = %v", err)
	}
	if err := bound.WriteFile(".env", []byte("x")); !errors.Is(err, ErrFileDenied) {
		t.Fatalf("a denied name = %v", err)
	}
	if len(got) != 2 || got[0].Path != "runs/r1/a.txt" || got[1].Path != "runs/r1/b.txt" || got[1].OldPath != "runs/r1/a.txt" {
		t.Fatalf("changes = %+v", got)
	}
	if err := os.Mkdir(filepath.Join(root, "outside"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../outside", filepath.Join(root, "runs", "r1", "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := bound.Open("link/x"); err == nil {
		t.Fatal("a link out of the directory of the run opened")
	}
	if _, err := bound.RealName("link/x"); !errors.Is(err, ErrFileOutside) {
		t.Fatalf("RealName of a link out = %v", err)
	}
	if shown("runs/r1/attachments/a.png", "runs/r1") != "attachments/a.png" || shown("other/a.png", "runs/r1") != "other/a.png" || shown("a.png", "") != "a.png" {
		t.Fatal("shown is wrong")
	}
}

// makeSub refuses a part of the directory that is a link or a file.
func TestMakeSubRefusesLinks(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "elsewhere"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere", filepath.Join(root, "runs")); err != nil {
		t.Fatal(err)
	}
	d := &dirFiles{dir: root}
	if err := d.makeSub("runs/r1", nil); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("makeSub through a link = %v", err)
	}
	mustWrite(t, filepath.Join(root, "file"), "x")
	if err := d.makeSub("file/r1", nil); err == nil {
		t.Fatal("makeSub through a file worked")
	}
}

// A store that is not on the disk gets the directory of a run by its
// prefix.
func TestPrefixFiles(t *testing.T) {
	inner := &mapFiles{m: fstest.MapFS{"runs/r1/a.txt": {Data: []byte("a")}, "top.txt": {Data: []byte("t")}}}
	p := &prefixFiles{inner: inner, dir: "runs/r1"}
	if b, err := fs.ReadFile(p, "a.txt"); err != nil || string(b) != "a" {
		t.Fatalf("read = %q, %v", b, err)
	}
	if _, err := p.Stat("../top.txt"); err == nil {
		t.Fatal("a name left the directory")
	}
	if err := p.WriteFile("b.txt", []byte("b")); err != nil {
		t.Fatal(err)
	}
	if _, ok := inner.m["runs/r1/b.txt"]; !ok {
		t.Fatal("the write missed the prefix")
	}
	if real, err := p.RealName("b.txt"); err != nil || real != "b.txt" {
		t.Fatalf("RealName = %q, %v", real, err)
	}
	if err := p.Remove("."); err == nil {
		t.Fatal("Remove(.) worked")
	}
}

// mapFiles is a Files in memory for TestPrefixFiles.
type mapFiles struct{ m fstest.MapFS }

func (f *mapFiles) Open(name string) (fs.File, error)          { return f.m.Open(name) }
func (f *mapFiles) Stat(name string) (fs.FileInfo, error)      { return f.m.Stat(name) }
func (f *mapFiles) ReadDir(name string) ([]fs.DirEntry, error) { return f.m.ReadDir(name) }
func (f *mapFiles) WriteFile(name string, data []byte) error {
	f.m[name] = &fstest.MapFile{Data: data}
	return nil
}
func (f *mapFiles) Remove(name string) error { delete(f.m, name); return nil }
func (f *mapFiles) Rename(o, n string) error {
	f.m[n] = f.m[o]
	delete(f.m, o)
	return nil
}
