package axmeta

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/ax"
)

func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(b)
}

func stashExample(t *testing.T) {
	t.Helper()
	task := &ax.Task{
		APIVersion: ax.APIVersion,
		Kind:       ax.KindTask,
		Metadata:   ax.ObjectMeta{Name: "reviewer", Atespace: "default", CreateTime: "2026-10-17T12:00:00Z"},
		Spec: ax.TaskSpec{
			Env: []ax.EnvVar{
				{Name: "FW_AGENT", Value: "reviewer"},
				{Name: "ANTHROPIC_API_KEY", Value: "sk-ant-very-secret"},
				{Name: "db_password", Value: "hunter2"},
			},
			Resources:  ax.ResourcesSpec{Limits: ax.ResourceList{CPU: "2", Memory: "4Gi"}},
			Workspaces: []ax.WorkspaceRef{{Name: "code", Path: "/workspace"}, {Name: "missing"}, {Name: "tools"}},
		},
	}
	tools := &ax.Workspace{Kind: ax.KindWorkspace, Metadata: ax.ObjectMeta{Name: "tools"}}
	code := &ax.Workspace{Kind: ax.KindWorkspace, Metadata: ax.ObjectMeta{Name: "code"}, Spec: ax.WorkspaceSpec{
		Files: []ax.File{{Path: "AGENTS.md", Content: "# Rules\n- one\n"}},
	}}
	ax.Stash(task, []*ax.Workspace{tools, code})
	t.Cleanup(func() { ax.Stash(nil, nil) })
}

func serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(Handler())
	t.Cleanup(srv.Close)
	return srv
}

func TestTaskEndpoint(t *testing.T) {
	stashExample(t)
	srv := serve(t)
	resp, body := get(t, srv.URL+PathTask)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/yaml" {
		t.Errorf("Content-Type = %q", ct)
	}
	for _, secret := range []string{"sk-ant-very-secret", "hunter2"} {
		if strings.Contains(body, secret) {
			t.Errorf("task YAML leaks %q:\n%s", secret, body)
		}
	}
	if strings.Contains(body, "status") {
		t.Errorf("task YAML has a status:\n%s", body)
	}
	// The YAML keys follow ax commit f0f16ff.
	if !strings.Contains(body, "createTime: ") || strings.Contains(body, "creationTimestamp") {
		t.Errorf("task YAML has the wrong time key:\n%s", body)
	}
	task, err := ax.ParseTask([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range task.Spec.Env {
		got[e.Name] = e.Value
	}
	want := map[string]string{"FW_AGENT": "reviewer", "ANTHROPIC_API_KEY": "***", "db_password": "***"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if task.Metadata.Name != "reviewer" || task.Metadata.Atespace != "default" || task.Spec.Resources.Limits.Memory != "4Gi" {
		t.Errorf("task = %+v", task)
	}
	// The stash keeps the real values.
	st, _ := ax.Stashed()
	if st.Spec.Env[1].Value != "sk-ant-very-secret" {
		t.Error("serving the task changed the stash")
	}
}

func TestWorkspacesEndpoint(t *testing.T) {
	stashExample(t)
	srv := serve(t)
	resp, body := get(t, srv.URL+PathWorkspaces)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/yaml" {
		t.Errorf("Content-Type = %q", ct)
	}
	if n := strings.Count(body, "\n---\n"); n != 1 {
		t.Errorf("got %d separators, want 1:\n%s", n, body)
	}
	wss, err := ax.ParseWorkspaces([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, w := range wss {
		names = append(names, w.Metadata.Name)
	}
	if !slices.Equal(names, []string{"code", "tools"}) {
		t.Errorf("order = %v, want binding order [code tools]", names)
	}
	if wss[0].Spec.Files[0].Content != "# Rules\n- one\n" {
		t.Errorf("file content = %q", wss[0].Spec.Files[0].Content)
	}
}

func TestEndpointsWithoutStash(t *testing.T) {
	ax.Stash(nil, nil)
	srv := serve(t)
	for _, p := range []string{PathTask, PathWorkspaces} {
		if resp, _ := get(t, srv.URL+p); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", p, resp.StatusCode)
		}
	}
	// The handler serves nothing else.
	if resp, _ := get(t, srv.URL+"/v1/runs"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("/v1/runs: status %d, want 404", resp.StatusCode)
	}
}

// Listen binds the loopback address only, and port 0 picks a free port.
func TestListenLoopback(t *testing.T) {
	stashExample(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := Listen(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	if !strings.HasPrefix(s.URL(), "http://127.0.0.1:") || strings.HasSuffix(s.URL(), ":0") {
		t.Fatalf("URL = %q", s.URL())
	}
	resp, body := get(t, s.URL()+PathTask)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "name: reviewer") {
		t.Errorf("status %d: %s", resp.StatusCode, body)
	}
	if _, err := Listen(ctx, 70000); err == nil {
		t.Error("a port out of range was accepted")
	}
}
