package ax

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// The expected names come from TestMarkerName in internal/workspace of ax.
func TestMarkerName(t *testing.T) {
	tests := map[string]string{
		"/workspace":        "initialized-workspace",
		"/workspace/":       "initialized-workspace",
		"":                  "initialized-workspace",
		"/":                 "initialized-root",
		"/workspace/app":    "initialized-workspace-app",
		"/workspace/tools/": "initialized-workspace-tools",
		"/workspace/a/b":    "initialized-workspace-a-b",
		"/srv/data":         "initialized-srv-data",
	}
	for path, want := range tests {
		if got := MarkerName(path); got != want {
			t.Errorf("MarkerName(%q) = %q, want %q", path, got, want)
		}
	}
}

// needGit skips the test when the go-git file transport cannot run, because
// it starts git-upload-pack for file:// URLs.
func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed; the go-git file transport needs git-upload-pack")
	}
}

// makeRepo creates a repository with one commit on branch main.
func makeRepo(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	repo, err := git.PlainInitWithOptions(dir, &git.PlainInitOptions{
		InitOptions: git.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName("main")},
	})
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		write(t, filepath.Join(dir, name), content)
		if _, err := wt.Add(name); err != nil {
			t.Fatal(err)
		}
	}
	sig := &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}
	if _, err := wt.Commit("initial", &git.CommitOptions{Author: sig, Committer: sig}); err != nil {
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

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestPrepare(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	src := filepath.Join(root, "src")
	makeRepo(t, src, map[string]string{"README.md": "hello\n"})
	home := filepath.Join(root, "home", ".fw")
	wsPath := filepath.Join(root, "workspace")
	absSkills := filepath.Join(root, "abs-skills")

	ws := &Workspace{
		Metadata: ObjectMeta{Name: "code"},
		Spec: WorkspaceSpec{
			Files: []File{
				{Path: ".fw/fw.yaml", Content: "model: test/model\n"},
				{Path: "AGENTS.md", Content: "# Rules\n"},
			},
			Git: []GitRepo{
				{Name: "app", Repo: "file://" + src, Branch: "main", Depth: 1},
				{Name: "origin", Repo: "file://" + src + "/"},
				{Repo: "file://" + src, Dir: "nested/copy"},
			},
			Skills: SkillsConfig{Path: ".agents/skills"},
		},
	}
	extra := &Workspace{
		Metadata: ObjectMeta{Name: "tools"},
		Spec:     WorkspaceSpec{Skills: SkillsConfig{Path: absSkills}},
	}
	bindings := []Binding{
		{Ref: WorkspaceRef{Name: "code", Goal: "run the tests"}, Workspace: ws, Path: wsPath},
		{Ref: WorkspaceRef{Name: "tools"}, Workspace: extra, Path: filepath.Join(wsPath, "tools")},
	}

	res, err := Prepare(context.Background(), bindings, home, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("got %d results, want 2", len(res))
	}
	r := res[0]
	if !r.FirstRun || !r.Marked || r.Path != wsPath || r.Name != "code" || r.Goal != "run the tests" {
		t.Errorf("result = %+v", r)
	}
	wantCloned := []string{"file://" + src, "file://" + src + "/", "file://" + src}
	if !reflect.DeepEqual(r.Cloned, wantCloned) {
		t.Errorf("cloned = %v, want %v", r.Cloned, wantCloned)
	}
	if got := readFile(t, filepath.Join(wsPath, ".fw/fw.yaml")); got != "model: test/model\n" {
		t.Errorf("fw.yaml = %q", got)
	}
	if got := readFile(t, filepath.Join(wsPath, "AGENTS.md")); got != "# Rules\n" {
		t.Errorf("AGENTS.md = %q", got)
	}
	st, err := os.Stat(filepath.Join(wsPath, "AGENTS.md"))
	if err != nil || st.Mode().Perm() != 0o644 {
		t.Errorf("AGENTS.md mode = %v, %v", st, err)
	}
	for _, dir := range []string{"app", "src", "nested/copy"} {
		if got := readFile(t, filepath.Join(wsPath, dir, "README.md")); got != "hello\n" {
			t.Errorf("%s/README.md = %q", dir, got)
		}
		if !exists(filepath.Join(wsPath, dir, ".git")) {
			t.Errorf("%s has no .git", dir)
		}
	}
	// The branch and the depth reach the clone.
	repo, err := git.PlainOpen(filepath.Join(wsPath, "app"))
	if err != nil {
		t.Fatal(err)
	}
	head, err := repo.Head()
	if err != nil || head.Name() != plumbing.NewBranchReferenceName("main") {
		t.Errorf("app HEAD = %v, %v", head, err)
	}
	if !exists(filepath.Join(wsPath, "app", ".git", "shallow")) {
		t.Error("app is not a shallow clone")
	}
	if st, err := os.Stat(filepath.Join(wsPath, ".agents/skills")); err != nil || !st.IsDir() {
		t.Errorf("relative skills dir: %v", err)
	}
	if st, err := os.Stat(absSkills); err != nil || !st.IsDir() {
		t.Errorf("absolute skills dir: %v", err)
	}
	st, err = os.Stat(MarkerDir(home))
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("marker dir mode = %v, %v", st, err)
	}
	marker := filepath.Join(home, "ax", MarkerName(wsPath))
	if got := readFile(t, marker); !strings.HasPrefix(got, "workspace: code\ninitialized_at: ") {
		t.Errorf("marker = %q", got)
	}
	if !res[1].FirstRun || !res[1].Marked || res[1].Goal != "" {
		t.Errorf("second binding = %+v", res[1])
	}
	if !exists(filepath.Join(home, "ax", MarkerName(filepath.Join(wsPath, "tools")))) {
		t.Error("no marker for the second binding")
	}

	// A second start finds the markers and changes nothing.
	write(t, filepath.Join(wsPath, "AGENTS.md"), "edited by the agent\n")
	if err := os.RemoveAll(filepath.Join(wsPath, "app")); err != nil {
		t.Fatal(err)
	}
	res, err = Prepare(context.Background(), bindings, home, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range res {
		if r.FirstRun || r.Marked || r.Goal != "" || len(r.Cloned) != 0 {
			t.Errorf("second Prepare result %d = %+v", i, r)
		}
	}
	if got := readFile(t, filepath.Join(wsPath, "AGENTS.md")); got != "edited by the agent\n" {
		t.Errorf("AGENTS.md rewritten: %q", got)
	}
	if exists(filepath.Join(wsPath, "app")) {
		t.Error("app cloned again")
	}
}

func TestPrepareSkipsExistingCheckout(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	src := filepath.Join(root, "src")
	makeRepo(t, src, map[string]string{"README.md": "upstream\n"})
	wsPath := filepath.Join(root, "workspace")
	// The repository is already there, as after a restore without a marker.
	makeRepo(t, filepath.Join(wsPath, "app"), map[string]string{"README.md": "local\n"})
	b := Binding{Ref: WorkspaceRef{Name: "w"}, Path: wsPath, Workspace: &Workspace{
		Spec: WorkspaceSpec{Git: []GitRepo{{Name: "app", Repo: "file://" + src}}},
	}}
	res, err := Prepare(context.Background(), []Binding{b}, filepath.Join(root, "home"), quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if !res[0].Marked || len(res[0].Cloned) != 0 {
		t.Errorf("result = %+v", res[0])
	}
	if got := readFile(t, filepath.Join(wsPath, "app", "README.md")); got != "local\n" {
		t.Errorf("README.md = %q", got)
	}
}

func TestPrepareFailedCloneLeavesNoMarker(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	wsPath := filepath.Join(root, "workspace")
	b := Binding{
		Ref:  WorkspaceRef{Name: "w", Goal: "build"},
		Path: wsPath,
		Workspace: &Workspace{Spec: WorkspaceSpec{
			Files: []File{{Path: "notes.txt", Content: "x"}},
			Git:   []GitRepo{{Name: "app", Repo: "file://" + filepath.Join(root, "does-not-exist")}},
		}},
	}
	for i := 0; i < 2; i++ {
		res, err := Prepare(context.Background(), []Binding{b}, home, quietLog())
		if err != nil {
			t.Fatal(err)
		}
		r := res[0]
		if !r.FirstRun || r.Marked || len(r.Cloned) != 0 || r.Goal != "build" {
			t.Errorf("attempt %d: result = %+v", i, r)
		}
		if exists(filepath.Join(home, "ax", MarkerName(wsPath))) {
			t.Fatalf("attempt %d: marker written after a failed clone", i)
		}
		if exists(filepath.Join(wsPath, "app", ".git")) {
			t.Errorf("attempt %d: partial clone left behind", i)
		}
		if got := readFile(t, filepath.Join(wsPath, "notes.txt")); got != "x" {
			t.Errorf("notes.txt = %q", got)
		}
	}
}

func TestPrepareDefaultBinding(t *testing.T) {
	root := t.TempDir()
	wsPath := filepath.Join(root, "workspace")
	res, err := Prepare(context.Background(), []Binding{{Path: wsPath}}, filepath.Join(root, "home"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res[0].FirstRun || !res[0].Marked || res[0].Goal != "" {
		t.Errorf("result = %+v", res[0])
	}
	if got := readFile(t, filepath.Join(root, "home", "ax", MarkerName(wsPath))); !strings.HasPrefix(got, "workspace: unknown\n") {
		t.Errorf("marker = %q", got)
	}
	if st, err := os.Stat(wsPath); err != nil || !st.IsDir() {
		t.Errorf("workspace dir: %v", err)
	}
}

func TestPrepareUnwritablePath(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "file")
	write(t, blocker, "x")
	ok := filepath.Join(root, "ok")
	res, err := Prepare(context.Background(), []Binding{{Path: filepath.Join(blocker, "ws")}, {Path: ok}}, filepath.Join(root, "home"), quietLog())
	if err == nil {
		t.Error("no error for a path under a file")
	}
	if len(res) != 2 || !res[1].Marked {
		t.Errorf("the next binding was not prepared: %+v", res)
	}
}

func TestCloneDir(t *testing.T) {
	tests := []struct {
		repo GitRepo
		want string
	}{
		{GitRepo{Name: "app", Repo: "https://h/x/y.git"}, "/w/app"},
		{GitRepo{Repo: "https://h/x/chalk.git"}, "/w/chalk"},
		{GitRepo{Name: "origin", Repo: "git@github.com:org/tool.git"}, "/w/tool"},
		{GitRepo{Name: "repo", Repo: "https://h/x/lib/"}, "/w/lib"},
		{GitRepo{Name: "app", Dir: ".", Repo: "u"}, "/w"},
		{GitRepo{Name: "app", Dir: "src/app", Repo: "u"}, "/w/src/app"},
		{GitRepo{Name: "app", Dir: "/opt/app", Repo: "u"}, "/opt/app"},
	}
	for _, tt := range tests {
		if got := CloneDir(tt.repo, "/w"); got != tt.want {
			t.Errorf("CloneDir(%+v) = %q, want %q", tt.repo, got, tt.want)
		}
	}
}

func TestRedactURL(t *testing.T) {
	tests := map[string]string{
		"https://github.com/o/r.git":                     "https://github.com/o/r.git",
		"https://x-access-token:ghs_abc123@github.com/o": "https://***@github.com/o",
		"https://ghp_secret99@github.com/o/r.git":        "https://***@github.com/o/r.git",
		"git@github.com:o/r.git":                         "git@github.com:o/r.git",
	}
	for in, want := range tests {
		if got := RedactURL(in); got != want {
			t.Errorf("RedactURL(%q) = %q, want %q", in, got, want)
		}
	}
	err := &os.PathError{Op: "clone", Path: "https://ghp_secret99@github.com/o/r.git", Err: os.ErrNotExist}
	if got := redactErr(err, "https://ghp_secret99@github.com/o/r.git"); strings.Contains(got, "ghp_secret99") {
		t.Errorf("redactErr kept the token: %q", got)
	}
}

func TestPrepareForgetsCloneCredentials(t *testing.T) {
	target := t.TempDir()
	home := filepath.Join(target, ".fw")
	repoURL := "https://x-access-token:ghs_cloned123@github.com/o/app.git"
	ws := &Workspace{}
	ws.Spec.Git = []GitRepo{{Name: "app", Repo: repoURL}, {Name: "tools", Repo: "ssh://git@github.com/o/tools.git"}}
	// A repository that an earlier version cloned, with the token in .git/config.
	for _, r := range ws.Spec.Git {
		repo, err := git.PlainInit(CloneDir(r, target), false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repo.CreateRemote(&gitconfig.RemoteConfig{Name: "origin", URLs: []string{r.Repo}}); err != nil {
			t.Fatal(err)
		}
	}
	// The marker exists, so the setup itself does not run again.
	if err := writeMarker(filepath.Join(MarkerDir(home), MarkerName(target)), ws); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(context.Background(), []Binding{{Path: target, Workspace: ws}}, home, quietLog()); err != nil {
		t.Fatal(err)
	}
	app, err := os.ReadFile(filepath.Join(target, "app", ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(app), "ghs_cloned123") || !strings.Contains(string(app), "https://github.com/o/app.git") {
		t.Fatalf(".git/config of app:\n%s", app)
	}
	tools, err := os.ReadFile(filepath.Join(target, "tools", ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(tools), "ssh://git@github.com/o/tools.git") {
		t.Fatalf("the ssh user changed:\n%s", tools)
	}
}
