package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func newTestFiles(t *testing.T) (fw.Tool, string) {
	t.Helper()
	root := t.TempDir()
	cfg := fw.DefaultConfig()
	cfg.Tools.Files.Root = root
	tool, err := fw.NewBuiltinTool("files", cfg)
	if err != nil {
		t.Fatal(err)
	}
	return tool, root
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestFilesView(t *testing.T) {
	f, root := newTestFiles(t)
	ctx := context.Background()
	writeFile(t, filepath.Join(root, "a.txt"), "one\ntwo\nthree\n")

	got := mustOK(t, call(t, ctx, f, `{"command":"view","path":"a.txt"}`))
	want := "     1\tone\n     2\ttwo\n     3\tthree"
	if !strings.HasSuffix(got, want) {
		t.Fatalf("view: got %q", got)
	}
	got = mustOK(t, call(t, ctx, f, `{"command":"view","path":"`+filepath.Join(root, "a.txt")+`","view_range":[2,-1]}`))
	if !strings.HasSuffix(got, "     2\ttwo\n     3\tthree") || strings.Contains(got, "one") {
		t.Fatalf("view_range: got %q", got)
	}
	mustFail(t, call(t, ctx, f, `{"command":"view","path":"a.txt","view_range":[5,6]}`), "view_range")
	mustFail(t, call(t, ctx, f, `{"command":"view","path":"missing.txt"}`), "does not exist")
}

func TestFilesViewDirectory(t *testing.T) {
	f, root := newTestFiles(t)
	writeFile(t, filepath.Join(root, "top.txt"), "x")
	writeFile(t, filepath.Join(root, "d1", "mid.txt"), "x")
	writeFile(t, filepath.Join(root, "d1", "d2", "deep.txt"), "x")
	writeFile(t, filepath.Join(root, ".hidden", "h.txt"), "x")
	writeFile(t, filepath.Join(root, "node_modules", "m.js"), "x")

	got := mustOK(t, call(t, context.Background(), f, `{"command":"view","path":"."}`))
	_, got, _ = strings.Cut(got, "\n") // skip the header line
	for _, want := range []string{"top.txt", "d1/mid.txt", filepath.Join("d1", "d2")} {
		if !strings.Contains(got, want) {
			t.Errorf("listing lacks %s:\n%s", want, got)
		}
	}
	for _, bad := range []string{"deep.txt", ".hidden", "node_modules"} {
		if strings.Contains(got, bad) {
			t.Errorf("listing shows %s:\n%s", bad, got)
		}
	}
}

func TestFilesCreate(t *testing.T) {
	f, root := newTestFiles(t)
	ctx := context.Background()
	got := mustOK(t, call(t, ctx, f, `{"command":"create","path":"sub/dir/new.txt","file_text":"hello\n"}`))
	if !strings.Contains(got, "created") {
		t.Fatalf("create: got %q", got)
	}
	if s := readFile(t, filepath.Join(root, "sub", "dir", "new.txt")); s != "hello\n" {
		t.Fatalf("content %q", s)
	}
	abs := filepath.Join(root, "abs.txt")
	mustOK(t, call(t, ctx, f, `{"command":"create","path":"`+abs+`","file_text":"x"}`))
	if s := readFile(t, abs); s != "x" {
		t.Fatalf("absolute create: content %q", s)
	}
	mustFail(t, call(t, ctx, f, `{"command":"create","path":"nofield.txt"}`), "file_text")
}

func TestFilesStrReplace(t *testing.T) {
	f, root := newTestFiles(t)
	ctx := context.Background()
	p := filepath.Join(root, "code.go")
	writeFile(t, p, "alpha\nbeta\ngamma\nbeta\n")

	mustOK(t, call(t, ctx, f, `{"command":"str_replace","path":"code.go","old_str":"alpha","new_str":"ALPHA"}`))
	if s := readFile(t, p); s != "ALPHA\nbeta\ngamma\nbeta\n" {
		t.Fatalf("unique replace: %q", s)
	}
	mustFail(t, call(t, ctx, f, `{"command":"str_replace","path":"code.go","old_str":"beta","new_str":"x"}`), "Multiple occurrences of old_str `beta` in lines: [2, 4]")
	mustFail(t, call(t, ctx, f, `{"command":"str_replace","path":"code.go","old_str":"delta","new_str":"x"}`), "did not appear verbatim")
	if s := readFile(t, p); s != "ALPHA\nbeta\ngamma\nbeta\n" {
		t.Fatalf("failed replace changed the file: %q", s)
	}
}

func TestFilesInsert(t *testing.T) {
	f, root := newTestFiles(t)
	ctx := context.Background()
	p := filepath.Join(root, "list.txt")
	writeFile(t, p, "one\ntwo\n")

	mustOK(t, call(t, ctx, f, `{"command":"insert","path":"list.txt","insert_line":1,"insert_text":"one and a half\n"}`))
	if s := readFile(t, p); s != "one\none and a half\ntwo\n" {
		t.Fatalf("insert: %q", s)
	}
	mustOK(t, call(t, ctx, f, `{"command":"insert","path":"list.txt","insert_line":0,"new_str":"zero"}`))
	if s := readFile(t, p); s != "zero\none\none and a half\ntwo\n" {
		t.Fatalf("insert at top with new_str: %q", s)
	}
	mustOK(t, call(t, ctx, f, `{"command":"insert","path":"list.txt","insert_line":4,"insert_text":"end"}`))
	if s := readFile(t, p); s != "zero\none\none and a half\ntwo\nend\n" {
		t.Fatalf("insert at end: %q", s)
	}
	mustFail(t, call(t, ctx, f, `{"command":"insert","path":"list.txt","insert_line":9,"insert_text":"x"}`), "[0, 5]")
}

func TestFilesJail(t *testing.T) {
	f, root := newTestFiles(t)
	ctx := context.Background()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret.txt"), "secret")
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	for _, input := range []string{
		`{"command":"view","path":"../secret.txt"}`,
		`{"command":"view","path":"sub/../../secret.txt"}`,
		`{"command":"view","path":"` + filepath.Join(outside, "secret.txt") + `"}`,
		`{"command":"view","path":"/etc/passwd"}`,
		`{"command":"view","path":"link/secret.txt"}`,
		`{"command":"create","path":"link/evil.txt","file_text":"x"}`,
		`{"command":"create","path":"../evil.txt","file_text":"x"}`,
	} {
		res := call(t, ctx, f, input)
		if !res.IsError || strings.Contains(text(res), "secret\n") || strings.HasSuffix(text(res), "\tsecret") {
			t.Errorf("%s: want a rejection, got %q", input, text(res))
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "evil.txt")); err == nil {
		t.Fatal("create wrote through a symlink outside the root")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "evil.txt")); err == nil {
		t.Fatal("create wrote above the root")
	}
	mustFail(t, call(t, ctx, f, `{"command":"delete","path":"a.txt"}`), "unknown command")
}

func TestFilesRefusesReservedPaths(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	cfg := fw.DefaultConfig()
	cfg.Tools.Files.Root = root
	cfg.Home = filepath.Join(root, ".fw")
	cfg.ConfigFile = filepath.Join(root, "fw.yaml")
	writeFile(t, filepath.Join(root, ".fw", "agents", "dev.yaml"), "name: dev\n")
	writeFile(t, filepath.Join(root, "fw.yaml"), "model: x\n")
	writeFile(t, filepath.Join(root, "app", "main.go"), "package main\n")
	if err := os.Symlink(filepath.Join(root, ".fw"), filepath.Join(root, "app", "state")); err != nil {
		t.Fatal(err)
	}
	f, err := fw.NewBuiltinTool("files", cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{
		`{"command":"view","path":".fw/agents/dev.yaml"}`,
		`{"command":"view","path":"` + filepath.Join(root, ".fw") + `"}`,
		`{"command":"create","path":".fw/agents/evil.yaml","file_text":"access: {}\n"}`,
		`{"command":"create","path":"./app/../.fw/fw.yaml","file_text":"x"}`,
		`{"command":"create","path":"app/state/agents/evil.yaml","file_text":"x"}`,
		`{"command":"str_replace","path":"fw.yaml","old_str":"x","new_str":"y"}`,
	} {
		mustFail(t, call(t, ctx, f, in), "holds the state or the configuration of fw")
	}
	if _, err := os.Stat(filepath.Join(root, ".fw", "agents", "evil.yaml")); err == nil {
		t.Fatal("the files tool wrote a profile")
	}
	if readFile(t, filepath.Join(root, "fw.yaml")) != "model: x\n" {
		t.Fatal("the files tool changed fw.yaml")
	}
	got := mustOK(t, call(t, ctx, f, `{"command":"view","path":"."}`))
	if strings.Contains(got, "fw.yaml") || strings.Contains(got, "state") || !strings.Contains(got, "main.go") {
		t.Fatalf("listing:\n%s", got)
	}
	mustOK(t, call(t, ctx, f, `{"command":"create","path":"app/notes.md","file_text":"ok\n"}`))
}

func TestFilesResolve(t *testing.T) {
	f, root := newTestFiles(t)
	ctx := context.Background()
	writeFile(t, filepath.Join(root, "secrets", "key"), "k\n")
	if err := os.Symlink(filepath.Join(root, "secrets"), filepath.Join(root, "docs")); err != nil {
		t.Fatal(err)
	}
	r, ok := f.(fw.Resolver)
	if !ok {
		t.Fatal("the files tool is not a resolver")
	}
	for in, want := range map[string]string{
		`{"command":"view","path":"./secrets/key"}`:                              "secrets/key",
		`{"command":"view","path":"` + filepath.Join(root, "secrets/key") + `"}`: "secrets/key",
		`{"command":"view","path":"docs/key"}`:                                   "secrets/key",
		`{"command":"create","path":"docs/../new.txt","file_text":"x"}`:          "new.txt",
		`{"command":"view","path":"/"}`:                                          "",
	} {
		out, err := r.Resolve(ctx, []byte(in))
		if want == "" {
			if err == nil {
				t.Errorf("Resolve(%s) = %s, want an error", in, out)
			}
			continue
		}
		if err != nil {
			t.Fatalf("Resolve(%s): %v", in, err)
		}
		var got struct{ Path, Command string }
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		if got.Path != want || got.Command == "" {
			t.Errorf("Resolve(%s) = %s, want path %q", in, out, want)
		}
	}
}
