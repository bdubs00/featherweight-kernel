package builtin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func TestMemory(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	cfg := fw.DefaultConfig()
	cfg.Home = home
	m, err := fw.NewBuiltinTool("memory", cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	got := mustOK(t, call(t, ctx, m, `{"command":"view","path":"/memories"}`))
	if !strings.Contains(got, "\t/memories") {
		t.Fatalf("empty view: %q", got)
	}
	info, err := os.Stat(filepath.Join(home, "memory", "agents", "default"))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("memory directory: %v, mode %v", err, info.Mode())
	}

	got = mustOK(t, call(t, ctx, m, `{"command":"create","path":"/memories/notes/todo.md","file_text":"- first\n- second\n"}`))
	if got != "File created successfully at: /memories/notes/todo.md" {
		t.Fatalf("create: %q", got)
	}
	if s := readFile(t, filepath.Join(home, "memory", "agents", "default", "notes", "todo.md")); s != "- first\n- second\n" {
		t.Fatalf("host file: %q", s)
	}
	got = mustOK(t, call(t, ctx, m, `{"command":"view","path":"/memories/notes/todo.md"}`))
	if got != "Here's the content of /memories/notes/todo.md with line numbers:\n     1\t- first\n     2\t- second" {
		t.Fatalf("view file: %q", got)
	}
	got = mustOK(t, call(t, ctx, m, `{"command":"view","path":"/memories"}`))
	if !strings.Contains(got, "\t/memories/notes/todo.md") || strings.Contains(got, home) {
		t.Fatalf("view dir: %q", got)
	}

	mustOK(t, call(t, ctx, m, `{"command":"str_replace","path":"/memories/notes/todo.md","old_str":"first","new_str":"done"}`))
	mustOK(t, call(t, ctx, m, `{"command":"insert","path":"/memories/notes/todo.md","insert_line":2,"insert_text":"- third"}`))
	if s := readFile(t, filepath.Join(home, "memory", "agents", "default", "notes", "todo.md")); s != "- done\n- second\n- third\n" {
		t.Fatalf("after edits: %q", s)
	}

	got = mustOK(t, call(t, ctx, m, `{"command":"rename","old_path":"/memories/notes/todo.md","new_path":"/memories/archive/todo.md"}`))
	if got != "Successfully renamed /memories/notes/todo.md to /memories/archive/todo.md" {
		t.Fatalf("rename: %q", got)
	}
	mustOK(t, call(t, ctx, m, `{"command":"delete","path":"/memories/archive"}`))
	mustFail(t, call(t, ctx, m, `{"command":"view","path":"/memories/archive/todo.md"}`), "does not exist")

	mustFail(t, call(t, ctx, m, `{"command":"view","path":"/memories/../x"}`), "leaves /memories")
	mustFail(t, call(t, ctx, m, `{"command":"view","path":"/etc/passwd"}`), "outside /memories")
	mustFail(t, call(t, ctx, m, `{"command":"view","path":"/memoriesX"}`), "outside /memories")
	mustFail(t, call(t, ctx, m, `{"command":"delete","path":"/memories"}`), "cannot be deleted")
}

// newMemoryTool builds the memory tool with a home directory and a scope.
func newMemoryTool(t *testing.T, home, scope string) fw.Tool {
	t.Helper()
	cfg := fw.DefaultConfig()
	cfg.Home = home
	cfg.Tools.Memory.Scope = scope
	m, err := fw.NewBuiltinTool("memory", cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// asAgent returns a context of a tool call in a run of the agent.
func asAgent(agent string) context.Context {
	return fw.WithRunInfo(context.Background(), fw.RunInfo{RunID: "r1", Agent: agent})
}

func TestMemoryAgentScope(t *testing.T) {
	home := t.TempDir()
	m := newMemoryTool(t, home, "")
	mustOK(t, call(t, asAgent("alpha"), m, `{"command":"create","path":"/memories/a.md","file_text":"alpha note\n"}`))
	if s := readFile(t, filepath.Join(home, "memory", "agents", "alpha", "a.md")); s != "alpha note\n" {
		t.Fatalf("host file: %q", s)
	}
	mustFail(t, call(t, asAgent("beta"), m, `{"command":"view","path":"/memories/a.md"}`), "does not exist")
	mustOK(t, call(t, asAgent("beta"), m, `{"command":"create","path":"/memories/a.md","file_text":"beta note\n"}`))
	got := mustOK(t, call(t, asAgent("alpha"), m, `{"command":"view","path":"/memories/a.md"}`))
	if !strings.Contains(got, "alpha note") {
		t.Fatalf("alpha sees: %q", got)
	}
	for _, bad := range []string{"../x", ".hidden", "a/b", strings.Repeat("x", 65)} {
		mustFail(t, call(t, asAgent(bad), m, `{"command":"view","path":"/memories"}`), "cannot name a memory directory")
	}
	if _, err := os.Stat(filepath.Join(home, "memory", "x")); !os.IsNotExist(err) {
		t.Fatalf("a bad agent name created a directory: %v", err)
	}
}

func TestMemorySharedScope(t *testing.T) {
	home := t.TempDir()
	m := newMemoryTool(t, home, "Shared")
	mustOK(t, call(t, asAgent("alpha"), m, `{"command":"create","path":"/memories/team.md","file_text":"shared note\n"}`))
	got := mustOK(t, call(t, asAgent("beta"), m, `{"command":"view","path":"/memories/team.md"}`))
	if !strings.Contains(got, "shared note") {
		t.Fatalf("beta sees: %q", got)
	}
	if s := readFile(t, filepath.Join(home, "memory", "shared", "team.md")); s != "shared note\n" {
		t.Fatalf("host file: %q", s)
	}
}

func TestMemoryUnknownScope(t *testing.T) {
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Tools.Memory.Scope = "team"
	_, err := fw.NewBuiltinTool("memory", cfg)
	if err == nil || !strings.Contains(err.Error(), "tools.memory.scope") {
		t.Fatalf("err = %v", err)
	}
}

// writeNote writes a file under dir with a modification time age ago.
func writeNote(t *testing.T, dir, name, content string, age time.Duration) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	mod := time.Now().Add(-age)
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
}

// brief calls the Briefer of the memory tool for an agent.
func brief(t *testing.T, m fw.Tool, agent string) string {
	t.Helper()
	b, ok := m.(fw.Briefer)
	if !ok {
		t.Fatal("the memory tool is not an fw.Briefer")
	}
	s, err := b.Brief(context.Background(), fw.RunInfo{RunID: "r1", Agent: agent})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMemoryBriefEmpty(t *testing.T) {
	home := t.TempDir()
	m := newMemoryTool(t, home, "")
	got := brief(t, m, "")
	want := briefHeading + briefIntro + briefEmpty + "\n\n" + briefRules
	if got != want {
		t.Fatalf("brief = %q", got)
	}
	if _, err := os.Stat(filepath.Join(home, "memory")); !os.IsNotExist(err) {
		t.Fatalf("Brief created the memory directory: %v", err)
	}
}

func TestMemoryBrief(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "memory", "agents", "reviewer")
	writeNote(t, dir, "old.md", "# Old note\nbody\n", 5*time.Hour)
	writeNote(t, dir, "front.md", "---\nname: front\ndescription: \"The front matter line\"\n---\n# Heading\n", 4*time.Hour)
	writeNote(t, dir, "plain-front.md", "---\nname: x\n---\n\n## After the block  \n", 3*time.Hour)
	writeNote(t, dir, "sub/deep.md", "\n\n  ## Deep line\t\n", 2*time.Hour)
	writeNote(t, dir, "long.md", strings.Repeat("é", 200)+"\n", time.Hour)
	writeNote(t, dir, "empty.md", "\n\n", 30*time.Minute)
	writeNote(t, dir, "b.md", "B\n", 0)
	writeNote(t, dir, "a.md", "A\n", 0)
	writeNote(t, dir, ".hidden.md", "hidden\n", 0)
	writeNote(t, dir, ".git/config", "hidden\n", 0)
	outside := filepath.Join(home, "secret.md")
	if err := os.WriteFile(outside, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link.md")); err != nil {
		t.Fatal(err)
	}

	m := newMemoryTool(t, home, "agent")
	got := brief(t, m, "reviewer")
	list := strings.Join([]string{
		"- /memories/a.md: A",
		"- /memories/b.md: B",
		"- /memories/empty.md",
		"- /memories/long.md: " + strings.Repeat("é", briefLineMax) + "...",
		"- /memories/sub/deep.md: Deep line",
		"- /memories/plain-front.md: After the block",
		"- /memories/front.md: The front matter line",
		"- /memories/old.md: Old note",
	}, "\n")
	want := briefHeading + briefIntro + briefList + "\n\n" + list + "\n\n" + briefRules
	if got != want {
		t.Fatalf("brief:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(got, "link.md") || strings.Contains(got, ".hidden") || strings.Contains(got, ".git") {
		t.Fatalf("brief shows a hidden or outside file: %q", got)
	}
}

func TestMemoryBriefCaps(t *testing.T) {
	home := t.TempDir()
	m := newMemoryTool(t, home, "")

	dir := filepath.Join(home, "memory", "agents", "many")
	for i := range briefMaxFiles + 5 {
		writeNote(t, dir, fmt.Sprintf("n%02d.md", i), "note\n", time.Duration(i)*time.Minute)
	}
	got := brief(t, m, "many")
	if n := strings.Count(got, "\n- /memories/"); n != briefMaxFiles {
		t.Fatalf("listed %d files, want %d", n, briefMaxFiles)
	}
	if !strings.Contains(got, "\n5 more files are not listed. View /memories to see every file.\n") {
		t.Fatalf("no count of the files left out:\n%s", got)
	}
	if !strings.Contains(got, "- /memories/n00.md: note\n- /memories/n01.md: note") {
		t.Fatalf("not newest first:\n%s", got)
	}

	dir = filepath.Join(home, "memory", "agents", "wide")
	for i := range 30 {
		writeNote(t, dir, fmt.Sprintf("w%02d.md", i), strings.Repeat("x", briefLineMax)+"\n", time.Duration(i)*time.Minute)
	}
	got = brief(t, m, "wide")
	var size, n int
	for _, l := range strings.Split(got, "\n") {
		if strings.HasPrefix(l, "- /memories/") {
			size += len(l) + 1
			n++
		}
	}
	if size > briefMaxBytes || n == 0 || n == 30 {
		t.Fatalf("listed %d files in %d bytes", n, size)
	}
	if !strings.Contains(got, fmt.Sprintf("\n%d more files are not listed.", 30-n)) {
		t.Fatalf("no count of the files left out:\n%s", got)
	}
}
