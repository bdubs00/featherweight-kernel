package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func newRecallTool(t *testing.T, home, scope string) fw.Tool {
	t.Helper()
	cfg := fw.DefaultConfig()
	cfg.Home = home
	cfg.Tools.Memory.Scope = scope
	tool, err := fw.NewBuiltinTool("recall", cfg)
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

func recallCtx(agent, principal string) context.Context {
	return fw.WithRunInfo(context.Background(), fw.RunInfo{
		RunID:     "run-1",
		Agent:     agent,
		Principal: principal,
	})
}

func TestRecallNotes(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "memory", "agents", "reviewer")
	writeNote(t, dir, "build.md", "---\ndescription: How to build the project\n---\nRun make build.\n", 2*time.Hour)
	writeNote(t, dir, "prefs.md", "The user prefers short answers.\n", time.Hour)
	tool := newRecallTool(t, home, "")
	ctx := recallCtx("reviewer", "alice")

	got := mustOK(t, call(t, ctx, tool, `{"query":"build"}`))
	if !strings.Contains(got, "mem://agent/reviewer/build.md") || !strings.Contains(got, "How to build") {
		t.Fatalf("note search = %q", got)
	}
	if strings.Contains(got, "prefs.md") {
		t.Fatalf("note search returned an unrelated note: %q", got)
	}

	got = mustOK(t, call(t, ctx, tool, `{"query":"make build"}`))
	if !strings.Contains(got, "build.md") {
		t.Fatalf("body search = %q", got)
	}

	got = mustOK(t, call(t, ctx, tool, `{"ref":"mem://agent/reviewer/build.md"}`))
	if !strings.Contains(got, "Run make build.") || !strings.Contains(got, "Kind: note") {
		t.Fatalf("get note = %q", got)
	}
	mustFail(t, call(t, ctx, tool, `{"ref":"mem://agent/reviewer/../secret.md"}`), "leaves the memory directory")
	if got := mustOK(t, call(t, ctx, tool, `{"query":"nothing-matches"}`)); !strings.Contains(got, "No memory records") {
		t.Fatalf("empty search = %q", got)
	}
}

func TestRecallRespectsAgentScope(t *testing.T) {
	home := t.TempDir()
	alpha := filepath.Join(home, "memory", "agents", "alpha")
	beta := filepath.Join(home, "memory", "agents", "beta")
	writeNote(t, alpha, "secret.md", "alpha only\n", 0)
	writeNote(t, beta, "public.md", "beta note\n", 0)
	tool := newRecallTool(t, home, "")

	got := mustOK(t, call(t, recallCtx("beta", ""), tool, `{"query":"alpha only"}`))
	if strings.Contains(got, "mem://") {
		t.Fatalf("beta saw alpha's note: %q", got)
	}
	// A ref cannot widen the scope: beta cannot name a note of alpha.
	mustFail(t, call(t, recallCtx("beta", ""), tool, `{"ref":"mem://agent/alpha/secret.md"}`), "outside the memory of this run")
	mustFail(t, call(t, recallCtx("beta", ""), tool, `{"ref":"mem://shared/secret.md"}`), "outside the memory of this run")
}

func TestRecallSharedScope(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "memory", "shared")
	writeNote(t, dir, "team.md", "the shared decision\n", 0)
	tool := newRecallTool(t, home, "shared")
	got := mustOK(t, call(t, recallCtx("reviewer", ""), tool, `{"query":"shared decision"}`))
	if !strings.Contains(got, "mem://shared/team.md") {
		t.Fatalf("shared search = %q", got)
	}
}

func TestRecallRunsFilesAndSkills(t *testing.T) {
	home := t.TempDir()
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "src", "checkout.go"), []byte("package checkout\nfunc Retry() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "skills", "release-notes"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "skills", "release-notes", fw.SkillFile), []byte("---\nname: release-notes\ndescription: Write release notes\n---\nUse the changelog.\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := fw.DefaultConfig()
	cfg.Home = home
	cfg.Tools.Files.Root = work
	cfg.Tools.Skills = []string{filepath.Join(home, "skills")}
	script := newRestartScript(endTurnOf("the checkout retry now passes"))
	a, err := fw.New(
		fw.WithoutDefaults(),
		fw.WithConfig(cfg),
		fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()),
		fw.WithAuthn(fw.NoAuthn{}),
		fw.WithModel(script.Model()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(context.Background()) })
	r, err := a.Run(context.Background(), "fix the checkout retry")
	if err != nil {
		t.Fatal(err)
	}
	if r.State != fw.StateCompleted {
		t.Fatalf("run state = %s, error = %s", r.State, r.Error)
	}

	tool := newRecallTool(t, home, "")
	ctx := fw.WithAgent(recallCtx("default", ""), a)

	got := mustOK(t, call(t, ctx, tool, `{"query":"checkout retry","kinds":["run"]}`))
	if !strings.Contains(got, "run://"+r.ID) || !strings.Contains(got, "checkout retry") {
		t.Fatalf("run search = %q", got)
	}
	got = mustOK(t, call(t, ctx, tool, `{"ref":"run://`+r.ID+`"}`))
	if !strings.Contains(got, "the checkout retry now passes") {
		t.Fatalf("get run = %q", got)
	}

	got = mustOK(t, call(t, ctx, tool, `{"query":"Retry","kinds":["file"]}`))
	if !strings.Contains(got, "file://src/checkout.go") {
		t.Fatalf("file search = %q", got)
	}
	got = mustOK(t, call(t, ctx, tool, `{"ref":"file://src/checkout.go"}`))
	if !strings.Contains(got, "func Retry") {
		t.Fatalf("get file = %q", got)
	}

	got = mustOK(t, call(t, ctx, tool, `{"query":"changelog","kinds":["skill"]}`))
	if !strings.Contains(got, "skill://release-notes") {
		t.Fatalf("skill search = %q", got)
	}
	got = mustOK(t, call(t, ctx, tool, `{"ref":"skill://release-notes"}`))
	if !strings.Contains(got, "Use the changelog.") {
		t.Fatalf("get skill = %q", got)
	}
}
