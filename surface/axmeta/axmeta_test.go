package axmeta

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/ax"
	"github.com/bdubs00/featherweight-kernel/fw"
)

func newAgent(t *testing.T) *fw.Agent {
	t.Helper()
	model := fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
		return &fw.Response{Content: []fw.Block{fw.Text("ok")}, StopReason: fw.StopEndTurn}, nil
	}}
	a, err := fw.New(
		fw.WithoutDefaults(),
		fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()),
		fw.WithAuthn(fw.NoAuthn{}),
		fw.WithModel(model),
		fw.WithSurfaces(New()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(context.Background()) })
	return a
}

func serve(t *testing.T) *httptest.Server {
	t.Helper()
	h, err := newAgent(t).Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

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
		Metadata:   ax.ObjectMeta{Name: "reviewer", Atespace: "default"},
		Spec: ax.TaskSpec{
			Env: []ax.EnvVar{
				{Name: "FW_AGENT", Value: "reviewer"},
				{Name: "ANTHROPIC_API_KEY", Value: "sk-ant-very-secret"},
				{Name: "db_password", Value: "hunter2"},
			},
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
	if task.Metadata.Name != "reviewer" || task.Metadata.Atespace != "default" {
		t.Errorf("metadata = %+v", task.Metadata)
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
}

// The kernel can be a stub that returns fw.ErrNotImplemented, or a kernel
// with no runs. Both give zero counts.
func TestStatusEndpointWithNoRuns(t *testing.T) {
	srv := serve(t)
	resp, body := get(t, srv.URL+PathStatus)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["pendingApproval"]) != "null" {
		t.Errorf("pendingApproval = %s", raw["pendingApproval"])
	}
	var s Status
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatal(err)
	}
	if s != (Status{}) {
		t.Errorf("status = %+v, want zero", s)
	}
	for _, key := range []string{`"promptTokens":0`, `"completionTokens":0`, `"active":0`, `"inputRequired":0`, `"total":0`} {
		if !strings.Contains(body, key) {
			t.Errorf("body lacks %s: %s", key, body)
		}
	}
}

func TestComputeStatus(t *testing.T) {
	t0 := time.Date(2026, 10, 13, 9, 14, 31, 0, time.UTC)
	usage := func(in, out int) fw.RunUsage {
		return fw.RunUsage{Usage: fw.Usage{InputTokens: in, OutputTokens: out, CacheReadTokens: 999}}
	}
	runs := []*fw.Run{
		{ID: "r1", State: fw.StateCompleted, Usage: usage(1000, 100)},
		{ID: "r2", State: fw.StateInputRequired, Usage: usage(200, 20), Pending: &fw.Approval{
			ID: "ap_new", Tool: "files", Input: json.RawMessage(`{"path":"a.txt","op":"delete"}`), RequestedAt: t0.Add(time.Minute),
		}},
		nil,
		{ID: "r3", State: fw.StateInputRequired, Usage: usage(30, 3), Pending: &fw.Approval{
			ID: "ap_7f3", Tool: "bash", Input: json.RawMessage(`{"command":"git push\n  origin main"}`), RequestedAt: t0.In(time.FixedZone("x", 3600)),
		}},
		{ID: "r4", State: fw.StateRunning, Usage: usage(4, 1)},
		{ID: "r5", State: fw.StateFailed},
		{ID: "r6", State: fw.StateSuspended},
	}
	got := ComputeStatus(runs)
	want := Status{
		Usage:           Usage{PromptTokens: 1234, CompletionTokens: 124},
		PendingApproval: &PendingApproval{ID: "ap_7f3", Action: "bash: git push origin main", RequestedAt: t0},
		Runs:            RunCounts{Active: 4, InputRequired: 2, Total: 6},
	}
	if got.Usage != want.Usage || got.Runs != want.Runs {
		t.Errorf("status = %+v, want %+v", got, want)
	}
	if got.PendingApproval == nil || *got.PendingApproval != *want.PendingApproval {
		t.Errorf("pendingApproval = %+v, want %+v", got.PendingApproval, want.PendingApproval)
	}
	b, err := json.Marshal(got.PendingApproval)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"requestedAt":"2026-10-13T09:14:31Z"`) {
		t.Errorf("JSON = %s", b)
	}
}

func TestAction(t *testing.T) {
	long := strings.Repeat("x", 300)
	tests := []struct {
		ap   fw.Approval
		want string
	}{
		{fw.Approval{Tool: "bash", Input: json.RawMessage(`{"command":"rm -rf build"}`)}, "bash: rm -rf build"},
		{fw.Approval{Tool: "files", Input: json.RawMessage(`{"path":"a"}`)}, `files: {"path":"a"}`},
		{fw.Approval{Tool: "git.commit", Input: json.RawMessage(`{}`), Reason: "rule: ask git.*"}, "git.commit: rule: ask git.*"},
		{fw.Approval{Reason: "Which branch?"}, "input: Which branch?"},
		{fw.Approval{Tool: "deploy"}, "deploy"},
	}
	for _, tt := range tests {
		if got := Action(&tt.ap); got != tt.want {
			t.Errorf("Action(%+v) = %q, want %q", tt.ap, got, tt.want)
		}
	}
	got := Action(&fw.Approval{Tool: "bash", Input: json.RawMessage(`{"command":"` + long + `"}`)})
	if n := len([]rune(got)); n != len("bash: ")+maxSummary {
		t.Errorf("long action has %d runes: %q", n, got)
	}
}

func TestRegistered(t *testing.T) {
	if !slices.Contains(fw.RegisteredSurfaces(), "axmeta") {
		t.Fatalf("surfaces = %v", fw.RegisteredSurfaces())
	}
	if _, err := fw.NewSurface("axmeta", fw.DefaultConfig()); err != nil {
		t.Fatal(err)
	}
}
