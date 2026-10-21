package cli

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	v1alpha1 "github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// axFakeAPI is an ax API server that keeps Tasks and Workspaces in memory,
// as the fake of the spawner tests does. A new Task is Suspended; a resume
// makes it Pending, and the next read makes it Running.
type axFakeAPI struct {
	v1alpha1.UnimplementedAXServer
	addr string

	mu         sync.Mutex
	tasks      map[string]*v1alpha1.Task
	workspaces map[string]*v1alpha1.Workspace
	resumed    int
}

func newAXFakeAPI(t *testing.T) *axFakeAPI {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &axFakeAPI{addr: lis.Addr().String(), tasks: map[string]*v1alpha1.Task{}, workspaces: map[string]*v1alpha1.Workspace{}}
	srv := grpc.NewServer()
	v1alpha1.RegisterAXServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return f
}

func (f *axFakeAPI) CreateTask(_ context.Context, req *v1alpha1.CreateTaskRequest) (*v1alpha1.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := req.GetTask()
	k := t.GetMetadata().GetAtespace() + "/" + t.GetMetadata().GetName()
	if _, ok := f.tasks[k]; ok {
		return nil, status.Error(codes.AlreadyExists, k)
	}
	t.Status = &v1alpha1.TaskStatus{Phase: "Suspended"}
	f.tasks[k] = t
	return t, nil
}

func (f *axFakeAPI) ResumeTask(_ context.Context, req *v1alpha1.ResumeTaskRequest) (*v1alpha1.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[req.GetAtespace()+"/"+req.GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, req.GetName())
	}
	f.resumed++
	t.Status.Phase = "Pending"
	return t, nil
}

func (f *axFakeAPI) GetTask(_ context.Context, req *v1alpha1.GetTaskRequest) (*v1alpha1.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[req.GetAtespace()+"/"+req.GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, req.GetName())
	}
	if t.Status.Phase == "Pending" {
		t.Status.Phase = "Running"
		t.Status.Conditions = []*v1alpha1.Condition{{Type: "Ready", Status: "True", Reason: "TaskRunning", Message: "Task is running"}}
	}
	return t, nil
}

func (f *axFakeAPI) DeleteTask(_ context.Context, req *v1alpha1.DeleteTaskRequest) (*v1alpha1.DeleteTaskResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := req.GetAtespace() + "/" + req.GetName()
	if _, ok := f.tasks[k]; !ok {
		return nil, status.Error(codes.NotFound, k)
	}
	delete(f.tasks, k)
	return &v1alpha1.DeleteTaskResponse{}, nil
}

func (f *axFakeAPI) UpdateWorkspace(_ context.Context, req *v1alpha1.UpdateWorkspaceRequest) (*v1alpha1.Workspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ws := req.GetWorkspace()
	f.workspaces[ws.GetMetadata().GetAtespace()+"/"+ws.GetMetadata().GetName()] = ws
	return ws, nil
}

func (f *axFakeAPI) DeleteWorkspace(_ context.Context, req *v1alpha1.DeleteWorkspaceRequest) (*v1alpha1.DeleteWorkspaceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.workspaces, req.GetAtespace()+"/"+req.GetName())
	return &v1alpha1.DeleteWorkspaceResponse{}, nil
}

func (f *axFakeAPI) task(k string) *v1alpha1.Task {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tasks[k]
}

func (f *axFakeAPI) workspace(k string) *v1alpha1.Workspace {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.workspaces[k]
}

// axFakeRouter plays the atenet router in front of one kernel: /readyz
// answers 503 for the first notReady reads, and the native routes check
// the actor header and the token.
type axFakeRouter struct {
	*httptest.Server
	mu       sync.Mutex
	notReady int
	actors   []string
}

func newAXFakeRouter(t *testing.T, token string) *axFakeRouter {
	t.Helper()
	f := &axFakeRouter{notReady: 2}
	mux := http.NewServeMux()
	guard := func(w http.ResponseWriter, r *http.Request) bool {
		f.mu.Lock()
		f.actors = append(f.actors, r.Header.Get("ate-target-actor"))
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, `{"detail":"no token"}`, http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.actors = append(f.actors, r.Header.Get("ate-target-actor"))
		if f.notReady > 0 {
			f.notReady--
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		if guard(w, r) {
			_, _ = w.Write([]byte(`{"usage":{"promptTokens":120,"completionTokens":30},"pendingApproval":null,"runs":{"active":0,"inputRequired":1,"total":3},"idle":true,"idleSince":"2026-10-20T09:14:31Z"}`))
		}
	})
	mux.HandleFunc("GET /v1/runs/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		if r.PathValue("id") != "01RUN" || r.URL.Query().Get("from") != "1" || r.Header.Get("Accept") != "application/json" {
			http.Error(w, `{"detail":"run not found"}`, http.StatusNotFound)
			return
		}
		events := []fw.Event{
			fw.MustEvent("01RUN", fw.EvRunQueued, fw.RunQueuedData{Input: "hi", Agent: "reviewer"}),
			fw.MustEvent("01RUN", fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Output: "done"}),
		}
		for i := range events {
			events[i].Seq = uint64(i + 1)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"run": map[string]any{"id": "01RUN"}, "events": events})
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *axFakeRouter) host() string { return strings.TrimPrefix(f.URL, "http://") }

// axEnv prepares a caller with a configuration, a profile file, and the
// secrets of the Task in its environment, and returns the profile path.
func axEnv(t *testing.T) string {
	t.Helper()
	env(t)
	writeFile(t, "fw.yaml", `model: openai/qwen3.8-flash
effort: low
providers:
  openai:
    baseUrl: http://ax-model.fw-fleet.svc.cluster.local:8080/v1
    apiKeyEnv: OPENAI_API_KEY
bus:
  driver: nats
  url: ws://nats.fw-fleet.svc.cluster.local:8080
  intake: fw.jobs.reviewer
tokens:
  url: http://fw-front.fw-fleet.svc.cluster.local:8080
tools:
  mcp:
    - name: github
      endpoint: https://api.githubcopilot.com/mcp/
      auth:
        bearer: GITHUB_MCP_TOKEN
`)
	writeFile(t, "reviewer.yaml", `# The reviewer of the lab.
policy:
  default: allow
  ask: [github.create_pull_request]
tools:
  builtin: [bash, files]
agents:
  reviewer:
    description: Reviews changes.
    system: Run every command in $FW_RUN_DIR and cost no more than $5.
`)
	writeFile(t, "readme.txt", "Notes for the reviewer.\n")
	t.Setenv("OPENAI_API_KEY", "sk-test-model-key")
	t.Setenv("FW_API_TOKEN", "fleet-token-123456")
	t.Setenv("GITHUB_MCP_TOKEN", "")
	t.Setenv("GITHUB_MCP_TOKEN_FILE", "")
	t.Setenv("AX_SERVER", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel.otel-system.svc.cluster.local:4318")
	for _, n := range axOTLPNames[1:] {
		t.Setenv(n, "")
	}
	prev := axPoll
	axPoll = 5 * time.Millisecond
	t.Cleanup(func() { axPoll = prev })
	return "reviewer.yaml"
}

func writeFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const axGolden = `# fw ax render: the Workspace and the Task of default/reviewer, in the order of apply.
# A value <secret: NAME> is a placeholder. fw ax up sends the value of NAME from --env or from its own environment.
# Warning: GITHUB_MCP_TOKEN is not set here, so fw ax up stops until you export it or pass --env GITHUB_MCP_TOKEN=VALUE
apiVersion: ax.io/v1alpha1
kind: Workspace
metadata:
  name: reviewer-ws
  atespace: default
spec:
  files:
    - path: .fw/fw.yaml
      content: |
        # The configuration of the Task default/reviewer, from fw ax render.
        model: openai/qwen3.8-flash
        effort: low
        providers:
          openai:
            baseUrl: http://ax-model.fw-fleet.svc.cluster.local:8080/v1
            apiKeyEnv: OPENAI_API_KEY
        bus:
          driver: nats
          url: ws://nats.fw-fleet.svc.cluster.local:8080
          intake: fw.jobs.reviewer
        tokens:
          url: http://fw-front.fw-fleet.svc.cluster.local:8080
          subject: fw.connections
        tools:
          mcp:
            - name: github
              endpoint: https://api.githubcopilot.com/mcp/
              auth:
                bearer: GITHUB_MCP_TOKEN
              trusted: false
          builtin: [bash, files]
        # The reviewer of the lab.
        policy:
          default: allow
          ask: [github.create_pull_request]
        agents:
          reviewer:
            description: Reviews changes.
            system: Run every command in $FW_RUN_DIR and cost no more than $5.
    - path: notes/README.md
      content: |
        Notes for the reviewer.
---
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: reviewer
  atespace: default
spec:
  image: registry.example/fw@sha256:0123
  env:
    - name: FW_API_TOKEN
      value: '<secret: FW_API_TOKEN>'
    - name: OPENAI_API_KEY
      value: '<secret: OPENAI_API_KEY>'
    - name: GITHUB_MCP_TOKEN
      value: '<secret: GITHUB_MCP_TOKEN>'
    - name: OTEL_EXPORTER_OTLP_ENDPOINT
      value: http://otel.otel-system.svc.cluster.local:4318
    - name: FW_TOOLS_USER
      value: none
    - name: DEPLOY_TOKEN
      value: '<secret: DEPLOY_TOKEN>'
  workspaces:
    - name: reviewer-ws
      path: /workspace
  egress:
    rules:
      - http:
          hostnames:
            - ax-model.fw-fleet.svc.cluster.local
          ports:
            - 8080
      - http:
          hostnames:
            - nats.fw-fleet.svc.cluster.local
          ports:
            - 8080
      - http:
          hostnames:
            - fw-front.fw-fleet.svc.cluster.local
          ports:
            - 8080
      - http:
          hostnames:
            - otel.otel-system.svc.cluster.local
          ports:
            - 4318
      - tls:
          hostnames:
            - api.githubcopilot.com
          ports:
            - 443
      - http:
          hostnames:
            - deb.debian.org
          ports:
            - 80
      - tls:
          hostnames:
            - pypi.org
          ports:
            - 443
`

// fw ax render prints the Workspace and the Task in the order of apply,
// with a placeholder for every secret and the derived egress rules.
func TestAXRenderGolden(t *testing.T) {
	profile := axEnv(t)
	code, out, errOut := run(t, "", "ax", "render", "-f", profile, "--name", "reviewer", "--image", "registry.example/fw@sha256:0123",
		"--egress", "deb.debian.org:80", "--egress", "tls:pypi.org", "--egress", "http://ax-model.fw-fleet.svc.cluster.local:8080",
		"--env", "FW_TOOLS_USER=none", "--env", "DEPLOY_TOKEN=deploy-secret-value", "--workspace-file", "notes/README.md=readme.txt")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if out != axGolden {
		t.Errorf("render differs from the golden output:\n%s", out)
	}
	for _, secret := range []string{"sk-test-model-key", "fleet-token-123456", "deploy-secret-value"} {
		if strings.Contains(out, secret) || strings.Contains(errOut, secret) {
			t.Errorf("the output holds the secret %q", secret)
		}
	}
	// The same input gives the same output.
	if _, again, _ := run(t, "", "ax", "render", "-f", profile, "--name", "reviewer", "--image", "registry.example/fw@sha256:0123",
		"--egress", "deb.debian.org:80", "--egress", "tls:pypi.org", "--egress", "http://ax-model.fw-fleet.svc.cluster.local:8080",
		"--env", "FW_TOOLS_USER=none", "--env", "DEPLOY_TOKEN=deploy-secret-value", "--workspace-file", "notes/README.md=readme.txt"); again != out {
		t.Errorf("a second render differs:\n%s", again)
	}
}

// fw ax render refuses what a Task cannot boot with, before any call.
func TestAXRenderRefuses(t *testing.T) {
	profile := axEnv(t)
	img := "registry.example/fw@sha256:0123"
	writeFile(t, "typo.yaml", "agents:\n  reviewer:\n    sytem: hi\n")
	writeFile(t, "unknown.yaml", "workbench:\n  pools: []\n")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-f", profile, "--name", "reviewer", "--image", "registry.example/fw:latest"}, "has no digest"},
		{[]string{"-f", profile, "--name", "Not_A_Name", "--image", img}, "is not a name for ax"},
		{[]string{"-f", "typo.yaml", "--name", "reviewer", "--image", img}, "field sytem not found"},
		{[]string{"-f", "unknown.yaml", "--name", "reviewer", "--image", img}, "unknown key workbench"},
		{[]string{"-f", profile, "--name", "reviewer", "--image", img, "--workspace-file", ".fw/fw.yaml=readme.txt"}, "other than .fw/fw.yaml, which fw ax renders"},
		{[]string{"-f", profile, "--name", "reviewer", "--image", img, "--workspace-file", "../out=readme.txt"}, "path inside the workspace"},
		{[]string{"-f", profile, "--name", "reviewer", "--image", img, "--egress", "a b"}, "--egress"},
		{[]string{"-f", profile, "--name", "reviewer", "--image", img, "--env", "NOVALUE"}, "is not NAME=VALUE"},
	} {
		code, _, errOut := run(t, "", append([]string{"ax", "render"}, tc.args...)...)
		if code == 0 || !strings.Contains(errOut, tc.want) {
			t.Errorf("%v: exit %d, want %q in:\n%s", tc.args, code, tc.want, errOut)
		}
	}
	// fw ax never chooses a model.
	t.Setenv("FW_MODEL", "")
	writeFile(t, "fw.yaml", "effort: low\n")
	if code, _, errOut := run(t, "", "ax", "render", "--name", "reviewer", "--image", img); code == 0 || !strings.Contains(errOut, "names no model") {
		t.Errorf("no model: exit %d\n%s", code, errOut)
	}
	if code, _, errOut := run(t, "", "ax", "render", "--image", img); code != exitUsage || !strings.Contains(errOut, "Usage: fw ax render") {
		t.Errorf("no name: exit %d\n%s", code, errOut)
	}
}

// A header of a provider or an MCP server stays on this machine, because
// it can hold a secret, and the output says so.
func TestAXRenderDropsHeaders(t *testing.T) {
	profile := axEnv(t)
	t.Setenv("FW_PROVIDERS_OPENAI_HEADERS_X_ORG_KEY", "org-header-value")
	t.Setenv("FW_TOOLS_MCP_GITHUB_HEADERS_X_TEAM", "team-header-value")
	code, out, errOut := run(t, "", "ax", "render", "-f", profile, "--name", "reviewer", "--image", "registry.example/fw@sha256:0123")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if strings.Contains(out, "header-value") || strings.Contains(out, "headers:") {
		t.Errorf("a header reached the output:\n%s", out)
	}
	for _, want := range []string{"# Warning: providers.openai: the headers stay on this machine", "# Warning: tools.mcp github: the headers stay on this machine"} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}
}

// Each configured endpoint gets one egress rule: http for a plain URL and
// tls for an https URL, with its port. One that no rule can carry gets a
// warning instead.
func TestAXRules(t *testing.T) {
	cfg := fw.DefaultConfig()
	cfg.Model = "anthropic/claude-opus-5-5"
	cfg.Models = map[string]string{"local": "openai/qwen"}
	cfg.Providers = map[string]fw.ProviderConfig{"openai": {BaseURL: "https://vllm.ai.svc:8443/v1"}}
	cfg.Bus = fw.BusConfig{Driver: "nats", URL: "nats://nats:4222,ws://nats-ws:8080"}
	cfg.Tokens.URL = "http://front:8080"
	cfg.Tools.MCP = []fw.MCPServerConfig{{Name: "git", Endpoint: "http://localhost:9000/mcp"}, {Name: "docs", Endpoint: "wss://docs.example"}, {Name: "local", Command: "mcp-local"}}
	env := map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://10.0.0.5:4318"}
	r := &axRendered{}
	rules, err := axRules(cfg, []string{"https:registry.npmjs.org", "tls:pypi.org:8443", "front:8080"}, func(n string) string { return env[n] }, r)
	if err != nil {
		t.Fatal(err)
	}
	warnings := r.warnings
	var got []string
	for _, r := range rules {
		got = append(got, r.kind+" "+r.addr())
	}
	want := []string{
		"tls api.anthropic.com:443", "tls vllm.ai.svc:8443", "http nats-ws:8080", "http front:8080", "tls docs.example:443",
		"https registry.npmjs.org:443", "tls pypi.org:8443",
	}
	if !slices.Equal(got, want) {
		t.Errorf("rules\n got %q\nwant %q", got, want)
	}
	joined := strings.Join(warnings, "\n")
	for _, w := range []string{
		"bus nats:4222: the egress gateway carries only HTTP and TLS",
		"telemetry 10.0.0.5:4318: an egress rule names a DNS name",
		"mcp git localhost:9000: an egress rule names a DNS name",
	} {
		if !strings.Contains(joined, w) {
			t.Errorf("warnings lack %q:\n%s", w, joined)
		}
	}
	if len(warnings) != 3 {
		t.Errorf("%d warnings, want 3:\n%s", len(warnings), joined)
	}
	for _, bad := range []string{"", "tls:", "host:0", "ftp://x", "a/b:80"} {
		if _, err := axRules(fw.DefaultConfig(), []string{bad}, func(string) string { return "" }, &axRendered{}); err == nil {
			t.Errorf("--egress %q parsed", bad)
		}
	}
}

// fw ax up applies the Workspace and the Task with the values of the
// secrets, resumes the Task, and waits for /readyz through the router;
// status, events, and down then read and remove the Task.
func TestAXUpStatusEventsDown(t *testing.T) {
	profile := axEnv(t)
	t.Setenv("GITHUB_MCP_TOKEN", "gh-token-abcdef")
	api := newAXFakeAPI(t)
	router := newAXFakeRouter(t, "fleet-token-123456")
	up := []string{"ax", "up", "-f", profile, "--name", "reviewer", "--image", "registry.example/fw@sha256:0123",
		"--server", api.addr, "--router", router.host(), "--timeout", "5s", "--env", "EXTRA=1"}

	code, out, errOut := run(t, "", up...)
	if code != 0 {
		t.Fatalf("up: exit %d\n%s%s", code, out, errOut)
	}
	if want := "default/reviewer is ready at http://" + router.host() + " with the header ate-target-actor: default/reviewer\n"; out != want {
		t.Errorf("up stdout %q, want %q", out, want)
	}
	for _, want := range []string{"the ax API at " + api.addr + "\n", "the router at " + router.host() + "\n", "applied the Workspace default/reviewer-ws", "the Task is Running"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("up stderr lacks %q:\n%s", want, errOut)
		}
	}
	if strings.Contains(out+errOut, "fleet-token-123456") || strings.Contains(out+errOut, "sk-test-model-key") {
		t.Error("up printed a secret")
	}
	task := api.task("default/reviewer")
	if task == nil || api.workspace("default/reviewer-ws") == nil {
		t.Fatal("up created no Task or no Workspace")
	}
	env := map[string]string{}
	for _, e := range task.GetSpec().GetEnv() {
		env[e.GetName()] = e.GetValue()
	}
	if env["FW_API_TOKEN"] != "fleet-token-123456" || env["OPENAI_API_KEY"] != "sk-test-model-key" || env["GITHUB_MCP_TOKEN"] != "gh-token-abcdef" || env["EXTRA"] != "1" {
		t.Errorf("the Task env is %v", env)
	}
	if n := len(task.GetSpec().GetEgress().GetRules()); n != 5 {
		t.Errorf("%d egress rules, want 5", n)
	}
	if api.resumed != 1 {
		t.Errorf("%d resumes, want 1", api.resumed)
	}
	for _, a := range router.actors {
		if a != "default/reviewer" {
			t.Errorf("the router got the actor %q", a)
		}
	}

	// A Task does not change, so a second up names down.
	if code, _, errOut := run(t, "", up...); code == 0 || !strings.Contains(errOut, "fw ax down --name reviewer deletes it") {
		t.Errorf("second up: exit %d\n%s", code, errOut)
	}

	code, out, errOut = run(t, "", "ax", "status", "--name", "reviewer", "--server", api.addr, "--router", router.host())
	if code != 0 {
		t.Fatalf("status: exit %d\n%s", code, errOut)
	}
	for _, want := range []string{
		"task      default/reviewer\n", "phase     Running\n", "condition Ready is True: TaskRunning, Task is running\n",
		`kernel    {"usage":{"promptTokens":120,"completionTokens":30},"pendingApproval":null,"runs":{"active":0,"inputRequired":1,"total":3},"idle":true,"idleSince":"2026-10-20T09:14:31Z"}` + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}

	code, out, errOut = run(t, "", "ax", "events", "--name", "reviewer", "--router", router.host(), "01RUN")
	if code != 0 {
		t.Fatalf("events: exit %d\n%s", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"type":"fw.run.queued"`) || !strings.Contains(lines[1], `"type":"fw.run.finished"`) {
		t.Errorf("events:\n%s", out)
	}
	if code, out, _ := run(t, "", "ax", "events", "--name", "reviewer", "--router", router.host(), "--format", "text", "01RUN"); code != 0 || !strings.Contains(out, "fw.run.finished outcome=completed") {
		t.Errorf("text events: exit %d\n%s", code, out)
	}
	if code, _, errOut := run(t, "", "ax", "events", "--name", "reviewer", "--router", router.host(), "01OTHER"); code == 0 || !strings.Contains(errOut, "answered 404") || !strings.Contains(errOut, "run not found") {
		t.Errorf("events of an unknown run: exit %d\n%s", code, errOut)
	}

	code, out, errOut = run(t, "", "ax", "down", "--name", "reviewer", "--server", api.addr)
	if code != 0 || out != "deleted the Task default/reviewer and the Workspace default/reviewer-ws\n" {
		t.Fatalf("down: exit %d\n%s%s", code, out, errOut)
	}
	if api.task("default/reviewer") != nil || api.workspace("default/reviewer-ws") != nil {
		t.Error("down left the Task or the Workspace")
	}
	if code, _, errOut := run(t, "", "ax", "down", "--name", "reviewer", "--server", api.addr); code != 0 || !strings.Contains(errOut, "does not exist") {
		t.Errorf("second down: exit %d\n%s", code, errOut)
	}
	if code, _, errOut := run(t, "", "ax", "status", "--name", "reviewer", "--server", api.addr, "--router", router.host()); code == 0 || !strings.Contains(errOut, "read the Task default/reviewer") {
		t.Errorf("status of a deleted Task: exit %d\n%s", code, errOut)
	}
}

// closedAddr returns a loopback address on which nothing listens.
func closedAddr(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()
	return addr
}

// fw ax up stops before any call when a secret of the Task is not set, or
// when the router does not answer.
func TestAXUpRefusesBeforeApply(t *testing.T) {
	profile := axEnv(t)
	api := newAXFakeAPI(t)
	args := []string{"ax", "up", "-f", profile, "--name", "reviewer", "--image", "registry.example/fw@sha256:0123", "--server", api.addr, "--router", closedAddr(t)}
	if code, _, errOut := run(t, "", args...); code == 0 || !strings.Contains(errOut, "the Task needs GITHUB_MCP_TOKEN, not set here") {
		t.Errorf("missing secret: exit %d\n%s", code, errOut)
	}
	if code, _, errOut := run(t, "", append(args, "--env", "GITHUB_MCP_TOKEN=x")...); code == 0 || !strings.Contains(errOut, "does not answer") || !strings.Contains(errOut, "port-forward svc/atenet-router") {
		t.Errorf("router down: exit %d\n%s", code, errOut)
	}
	if api.workspace("default/reviewer-ws") != nil || api.task("default/reviewer") != nil {
		t.Error("up applied something before it refused")
	}
}

// fw ax up names the Task and the fix when /readyz does not answer in
// time, and resumes a Task that ax reports as suspended.
func TestAXUpTimesOut(t *testing.T) {
	profile := axEnv(t)
	api := newAXFakeAPI(t)
	router := newAXFakeRouter(t, "fleet-token-123456")
	router.notReady = 1 << 30
	code, _, errOut := run(t, "", "ax", "up", "-f", profile, "--name", "reviewer", "--image", "registry.example/fw@sha256:0123",
		"--server", api.addr, "--router", router.host(), "--timeout", "50ms", "--env", "GITHUB_MCP_TOKEN=x")
	if code == 0 || !strings.Contains(errOut, "the Task default/reviewer is Running and its /readyz did not answer within 50ms; ax -a default describe task reviewer shows why") {
		t.Errorf("exit %d\n%s", code, errOut)
	}

	// status reads no kernel of a Task that sleeps, because the read wakes
	// it.
	if _, err := api.CreateTask(context.Background(), &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{Metadata: &v1alpha1.ObjectMeta{Name: "sleeper", Atespace: "default"}}}); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := run(t, "", "ax", "status", "--name", "sleeper", "--server", api.addr, "--router", router.host()); code != 0 ||
		!strings.Contains(out, "kernel    not read, because a request through the router wakes a Task that is Suspended") {
		t.Errorf("status of a sleeping Task: exit %d\n%s", code, out)
	}
}

// fw ax without a command prints its usage, and an unknown command fails.
func TestAXUsage(t *testing.T) {
	env(t)
	if code, _, errOut := run(t, "", "ax"); code != exitUsage || !strings.Contains(errOut, "Usage: fw ax <render|up|status|events|down>") {
		t.Errorf("exit %d\n%s", code, errOut)
	}
	if code, _, errOut := run(t, "", "ax", "deploy"); code != exitUsage || !strings.Contains(errOut, `unknown command "deploy"`) {
		t.Errorf("exit %d\n%s", code, errOut)
	}
	if _, _, errOut := run(t, "", "help"); !strings.Contains(errOut, "  ax ") {
		t.Errorf("help lacks ax:\n%s", errOut)
	}
}
