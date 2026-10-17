package ax

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const reviewerPath = "../examples/ax/reviewer.yaml"

// splitDocs splits a YAML stream on "---" lines.
func splitDocs(t *testing.T, data string) []string {
	t.Helper()
	var docs []string
	var cur strings.Builder
	for _, line := range strings.SplitAfter(data, "\n") {
		if strings.TrimSpace(line) == "---" {
			docs = append(docs, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteString(line)
	}
	docs = append(docs, cur.String())
	return docs
}

func TestParseReviewerExample(t *testing.T) {
	data, err := os.ReadFile(reviewerPath)
	if err != nil {
		t.Fatal(err)
	}
	docs := splitDocs(t, string(data))
	if len(docs) != 3 {
		t.Fatalf("got %d documents, want 3", len(docs))
	}

	wss, err := ParseWorkspaces([]byte(docs[0]))
	if err != nil {
		t.Fatal(err)
	}
	if len(wss) != 1 {
		t.Fatalf("got %d workspaces, want 1", len(wss))
	}
	ws := wss[0]
	if ws.APIVersion != APIVersion || ws.Kind != KindWorkspace {
		t.Errorf("apiVersion/kind = %q/%q", ws.APIVersion, ws.Kind)
	}
	if ws.Metadata.Name != "reviewer-ws" || ws.Metadata.Atespace != "default" {
		t.Errorf("metadata = %+v", ws.Metadata)
	}
	wantGit := []GitRepo{{Name: "app", Repo: "https://github.com/example/app.git", Branch: "main"}}
	if !reflect.DeepEqual(ws.Spec.Git, wantGit) {
		t.Errorf("git = %+v, want %+v", ws.Spec.Git, wantGit)
	}
	if len(ws.Spec.Files) != 2 {
		t.Fatalf("got %d files, want 2", len(ws.Spec.Files))
	}
	if ws.Spec.Files[0].Path != ".fw/fw.yaml" || !strings.Contains(ws.Spec.Files[0].Content, "mode: none") {
		t.Errorf("files[0] = %+v", ws.Spec.Files[0])
	}
	if ws.Spec.Files[1].Path != ".fw/agents/reviewer.yaml" || !strings.HasPrefix(ws.Spec.Files[1].Content, "name: reviewer\n") {
		t.Errorf("files[1] = %+v", ws.Spec.Files[1])
	}
	wantMCP := MCPConfig{Servers: []MCPServer{{Name: "git", Endpoint: "http://git-mcp.default.svc.cluster.local:8080"}}}
	if !reflect.DeepEqual(ws.Spec.MCP, wantMCP) {
		t.Errorf("mcp = %+v", ws.Spec.MCP)
	}
	if ws.Spec.Skills.Path != "/workspace/.agents/skills" {
		t.Errorf("skills.path = %q", ws.Spec.Skills.Path)
	}

	task, err := ParseTask([]byte(docs[1]))
	if err != nil {
		t.Fatal(err)
	}
	if task == nil {
		t.Fatal("task is nil")
	}
	if task.APIVersion != APIVersion || task.Kind != KindTask {
		t.Errorf("apiVersion/kind = %q/%q", task.APIVersion, task.Kind)
	}
	if task.Metadata.Name != "reviewer" || task.Metadata.Atespace != "default" {
		t.Errorf("metadata = %+v", task.Metadata)
	}
	if task.Spec.Image != "ghcr.io/bdubs00/featherweight-kernel:toolbox" {
		t.Errorf("image = %q", task.Spec.Image)
	}
	if len(task.Spec.Command) != 0 {
		t.Errorf("command = %v, want none", task.Spec.Command)
	}
	wantEnv := []EnvVar{{Name: "FW_AGENT", Value: "reviewer"}, {Name: "ANTHROPIC_API_KEY", Value: "replace-me"}}
	if !reflect.DeepEqual(task.Spec.Env, wantEnv) {
		t.Errorf("env = %+v", task.Spec.Env)
	}
	wantRes := ResourceReqs{
		Requests: ResourceList{CPU: "500m", Memory: "1Gi"},
		Limits:   ResourceList{CPU: "2", Memory: "4Gi"},
	}
	if task.Spec.Resources != wantRes {
		t.Errorf("resources = %+v", task.Spec.Resources)
	}
	wantRefs := []WorkspaceRef{{
		Name: "reviewer-ws",
		Path: "/workspace",
		Goal: "Install the toolchain that the repository under app/ needs and run its test suite once.",
	}}
	if !reflect.DeepEqual(task.Spec.Workspaces, wantRefs) {
		t.Errorf("workspaces = %+v", task.Spec.Workspaces)
	}
	if task.Spec.Debug {
		t.Error("debug = true")
	}
	if ns, n := Identity(task); ns != "default" || n != "reviewer" {
		t.Errorf("Identity = %q, %q", ns, n)
	}

	// The third document is a Model. Neither parser takes it.
	if got, err := ParseWorkspaces([]byte(docs[2])); err != nil || len(got) != 0 {
		t.Errorf("ParseWorkspaces(model) = %v, %v", got, err)
	}
	if got, err := ParseTask([]byte(docs[2])); err != nil || got != nil {
		t.Errorf("ParseTask(model) = %v, %v", got, err)
	}
}

func TestLoadFromFilesCombinedManifest(t *testing.T) {
	task, wss, err := LoadFromFiles(reviewerPath, []string{reviewerPath})
	if err != nil {
		t.Fatal(err)
	}
	if task == nil || task.Metadata.Name != "reviewer" {
		t.Fatalf("task = %+v", task)
	}
	if len(wss) != 1 || wss[0].Metadata.Name != "reviewer-ws" {
		t.Fatalf("workspaces = %+v", wss)
	}
}

func TestLoadFromFilesSeveralWorkspaceFiles(t *testing.T) {
	dir := t.TempDir()
	taskFile := filepath.Join(dir, "task.yaml")
	a := filepath.Join(dir, "a.yaml")
	b := filepath.Join(dir, "b.yaml")
	write(t, taskFile, "apiVersion: ax.io/v1alpha1\nkind: Task\nmetadata: {name: t}\nspec:\n  workspaces: [{name: a}, {name: b}]\n")
	write(t, a, "kind: Workspace\nmetadata: {name: a}\n---\n---\nkind: Workspace\nmetadata: {name: c}\n")
	write(t, b, "metadata: {name: b}\n")
	task, wss, err := LoadFromFiles(taskFile, []string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if task.Metadata.Name != "t" {
		t.Errorf("task = %+v", task)
	}
	var names []string
	for _, w := range wss {
		names = append(names, w.Metadata.Name)
	}
	if want := []string{"a", "c", "b"}; !reflect.DeepEqual(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
	if _, _, err := LoadFromFiles(filepath.Join(dir, "missing.yaml"), nil); err == nil {
		t.Error("missing task file: no error")
	}
}

func TestLoadFromEnv(t *testing.T) {
	t.Setenv(EnvTaskYAML, "")
	t.Setenv(EnvWorkspacesYAML, "")
	if Detect() {
		t.Error("Detect() = true with AX_TASK_YAML empty")
	}
	task, wss, err := LoadFromEnv()
	if err != nil || task != nil || len(wss) != 0 {
		t.Fatalf("empty env: %v %v %v", task, wss, err)
	}

	t.Setenv(EnvTaskYAML, "apiVersion: ax.io/v1alpha1\nkind: Task\nmetadata:\n  name: t1\n  atespace: team\nspec:\n  command: [sh, -c, 'echo hi']\n  debug: true\n")
	t.Setenv(EnvWorkspacesYAML, "---\n\n---\napiVersion: ax.io/v1alpha1\nkind: Workspace\nmetadata:\n  name: w1\n---\n# only a comment\n---\nkind: Workspace\nmetadata:\n  name: w2\nspec:\n  mcp:\n    - name: legacy\n      command: mcp-server\n      args: [--stdio]\n---\n")
	if !Detect() {
		t.Error("Detect() = false with AX_TASK_YAML set")
	}
	task, wss, err = LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if task.Metadata.Name != "t1" || task.Metadata.Atespace != "team" || !task.Spec.Debug {
		t.Errorf("task = %+v", task)
	}
	if want := []string{"sh", "-c", "echo hi"}; !reflect.DeepEqual(task.Spec.Command, want) {
		t.Errorf("command = %v", task.Spec.Command)
	}
	if len(wss) != 2 || wss[0].Metadata.Name != "w1" || wss[1].Metadata.Name != "w2" {
		t.Fatalf("workspaces = %+v", wss)
	}
	// spec.mcp as a bare list is the legacy form that ax accepts.
	want := []MCPServer{{Name: "legacy", Command: "mcp-server", Args: []string{"--stdio"}}}
	if !reflect.DeepEqual(wss[1].Spec.MCP.Servers, want) {
		t.Errorf("legacy mcp = %+v", wss[1].Spec.MCP)
	}

	t.Setenv(EnvTaskYAML, "spec: [unclosed")
	if _, _, err := LoadFromEnv(); err == nil {
		t.Error("bad YAML: no error")
	}
}

func TestManifestRoundTrip(t *testing.T) {
	task, wss, err := LoadFromFiles(reviewerPath, []string{reviewerPath})
	if err != nil {
		t.Fatal(err)
	}
	out, err := yaml.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseTask(out)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, task) {
		t.Errorf("task round trip:\n%s", out)
	}
	if strings.Contains(string(out), "status") {
		t.Errorf("task YAML has a status:\n%s", out)
	}
	out, err = yaml.Marshal(wss[0])
	if err != nil {
		t.Fatal(err)
	}
	wback, err := ParseWorkspaces(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(wback) != 1 || !reflect.DeepEqual(wback[0], wss[0]) {
		t.Errorf("workspace round trip:\n%s", out)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
