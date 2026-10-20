package ax

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	v1alpha1 "github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeAX is an ax API server that keeps Tasks in memory. A new Task is
// Pending for pendingReads reads of its status, then Running.
type fakeAX struct {
	v1alpha1.UnimplementedAXServer
	addr string

	mu           sync.Mutex
	tasks        map[string]*v1alpha1.Task
	reads        map[string]int
	pendingReads int
	deleted      []string
	resumed      []string
}

func newFakeAX(t *testing.T) *fakeAX {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeAX{addr: lis.Addr().String(), tasks: map[string]*v1alpha1.Task{}, reads: map[string]int{}, pendingReads: 1}
	srv := grpc.NewServer()
	v1alpha1.RegisterAXServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return f
}

func key(atespace, name string) string { return atespace + "/" + name }

func (f *fakeAX) CreateTask(_ context.Context, req *v1alpha1.CreateTaskRequest) (*v1alpha1.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := req.GetTask()
	k := key(t.GetMetadata().GetAtespace(), t.GetMetadata().GetName())
	if _, ok := f.tasks[k]; ok {
		return nil, status.Error(codes.AlreadyExists, k)
	}
	// ax creates every Task suspended until a caller resumes it.
	t.Status = &v1alpha1.TaskStatus{Phase: "Suspended"}
	f.tasks[k] = t
	return t, nil
}

func (f *fakeAX) ResumeTask(_ context.Context, req *v1alpha1.ResumeTaskRequest) (*v1alpha1.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := key(req.GetAtespace(), req.GetName())
	t, ok := f.tasks[k]
	if !ok {
		return nil, status.Error(codes.NotFound, k)
	}
	f.resumed = append(f.resumed, k)
	t.Status.Phase = "Pending"
	return t, nil
}

func (f *fakeAX) GetTask(_ context.Context, req *v1alpha1.GetTaskRequest) (*v1alpha1.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := key(req.GetAtespace(), req.GetName())
	t, ok := f.tasks[k]
	if !ok {
		return nil, status.Error(codes.NotFound, k)
	}
	f.reads[k]++
	// A Task runs only after a resume, as on ax.
	if f.reads[k] > f.pendingReads && t.Status.Phase == "Pending" {
		t.Status.Phase = "Running"
	}
	return t, nil
}

func (f *fakeAX) DeleteTask(_ context.Context, req *v1alpha1.DeleteTaskRequest) (*v1alpha1.DeleteTaskResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := key(req.GetAtespace(), req.GetName())
	if _, ok := f.tasks[k]; !ok {
		return nil, status.Error(codes.NotFound, k)
	}
	delete(f.tasks, k)
	f.deleted = append(f.deleted, k)
	return &v1alpha1.DeleteTaskResponse{}, nil
}

func (f *fakeAX) created() []*v1alpha1.Task {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*v1alpha1.Task
	for _, t := range f.tasks {
		out = append(out, t)
	}
	return out
}

// fakeRouter plays the atenet router and the native API of one child: it
// checks the actor header and the token, starts one run under the id of
// the body, 01CHILD without one, lets it finish after two reads, and
// records the decisions and the cancel.
type fakeRouter struct {
	*httptest.Server
	token string

	mu         sync.Mutex
	runID      string
	ignoreID   bool // start the run as 01CHILD whatever the body names, as a child of an older version
	actors     []string
	body       map[string]any
	keys       []string
	failStarts int // how many POST /v1/runs answer 503 first
	reads      int
	decisions  []map[string]any
	canceled   bool
}

func newFakeRouter(t *testing.T) *fakeRouter {
	t.Helper()
	f := &fakeRouter{token: "fleet-secret"}
	mux := http.NewServeMux()
	guard := func(w http.ResponseWriter, r *http.Request) bool {
		f.mu.Lock()
		f.actors = append(f.actors, r.Header.Get("ate-target-actor"))
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+f.token {
			w.WriteHeader(http.StatusUnauthorized)
			return false
		}
		return true
	}
	run := func(state string) map[string]any {
		f.mu.Lock()
		defer f.mu.Unlock()
		principal, _ := f.body["principal"].(string)
		out := map[string]any{"id": f.runID, "agent": "default", "state": state, "principal": principal, "usage": map[string]any{}}
		if state == "completed" {
			out["output"] = "child done"
		}
		return out
	}
	mux.HandleFunc("POST /v1/runs", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		f.mu.Lock()
		f.keys = append(f.keys, r.Header.Get("Idempotency-Key"))
		if f.failStarts > 0 {
			f.failStarts--
			f.mu.Unlock()
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.body = body
		f.runID, _ = body["id"].(string)
		if f.runID == "" || f.ignoreID {
			f.runID = "01CHILD"
		}
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(run("running"))
	})
	mux.HandleFunc("POST /v1/runs/fork", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.keys = append(f.keys, r.Header.Get("Idempotency-Key"))
		f.body = body
		f.mu.Unlock()
		id, _ := body["id"].(string)
		if id == "" {
			id = "01BRANCH"
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "agent": "default", "state": "running", "usage": map[string]any{}})
	})
	mux.HandleFunc("GET /v1/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		f.mu.Lock()
		have := f.runID
		f.mu.Unlock()
		if r.PathValue("id") != have || have == "" {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":"no run has that id"}`))
			return
		}
		f.mu.Lock()
		f.reads++
		state := "running"
		if f.reads > 2 {
			state = "completed"
		}
		if f.canceled {
			state = "canceled"
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(run(state))
	})
	mux.HandleFunc("POST /v1/runs/{id}/approvals/{approval}", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		var d map[string]any
		_ = json.NewDecoder(r.Body).Decode(&d)
		d["approval"] = r.PathValue("approval")
		f.mu.Lock()
		f.decisions = append(f.decisions, d)
		f.mu.Unlock()
		if r.PathValue("approval") == "stale" {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"detail":"no pending approval"}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/runs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		f.mu.Lock()
		f.canceled = true
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

// newSpawnerUnderTest stashes a parent Task and returns a spawner against
// the fakes.
func newSpawnerUnderTest(t *testing.T, ax *fakeAX, router *fakeRouter) *Spawner {
	t.Helper()
	return newSpawnerAt(t, ax, strings.TrimPrefix(router.URL, "http://"))
}

// newSpawnerAt stashes a parent Task and returns a spawner against the ax
// fake and a router URL of its caller, which a test decides at will.
func newSpawnerAt(t *testing.T, ax *fakeAX, routerURL string) *Spawner {
	t.Helper()
	prevTask, prevWS := Stashed()
	t.Cleanup(func() { Stash(prevTask, prevWS) })
	Stash(&Task{
		APIVersion: "ax.io/v1alpha1", Kind: "Task",
		Metadata: ObjectMeta{Name: "migrator", Atespace: "team"},
		Spec: TaskSpec{
			Image:   "ghcr.io/example/fw:toolbox",
			Command: []string{"fw", "serve"},
			Env: []EnvVar{{Name: "FW_MODEL", Value: "openai/x"}, {Name: "FW_WORKBENCH_FLEET_NAME", Value: "old"}, {Name: "FW_WORKBENCH_FLEET_URL", Value: "http://old"},
				{Name: EnvParentRun, Value: "stale"}, {Name: "FW_SPAWN_MAX_DEPTH", Value: "9"}, {Name: "FW_SPAWN_MAXDEPTH", Value: "9"}, {Name: EnvSpawnDisabled, Value: "true"}, {Name: "FW_API_TOKEN", Value: "fleet-secret"}},
			Resources:  ResourcesSpec{Requests: ResourceList{CPU: "500m", Memory: "1Gi"}},
			Workspaces: []WorkspaceRef{{Name: "repo", Path: "/workspace/repo", Goal: "migrate everything"}},
			Debug:      true,
		},
	}, nil)
	t.Setenv("FW_TEST_FLEET_TOKEN", "fleet-secret")
	cfg := fw.DefaultConfig()
	cfg.AX.Server = ax.addr
	cfg.AX.Router = routerURL
	cfg.Auth.TokenEnv = "FW_TEST_FLEET_TOKEN"
	cfg.Spawn.MaxDepth = 3
	s, err := NewSpawner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.readyPoll, s.runPoll = 5*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSpawnCreatesAChildTaskAndStartsItsRun(t *testing.T) {
	ctx := context.Background()
	ax := newFakeAX(t)
	router := newFakeRouter(t)
	router.failStarts = 1 // the child boots after its Task runs
	s := newSpawnerUnderTest(t, ax, router)
	parent := &fw.Run{ID: "01PARENT", Depth: 0}
	req := fw.RunRequest{Input: "migrate module x", Principal: "ada", PrincipalKind: fw.KindUser,
		Metadata: map[string]string{fw.MetaParent: parent.ID}, Budget: &fw.Budget{MaxTurns: 3, Deadline: 5 * time.Minute},
		Profile: &fw.AgentProfile{Tools: []string{"files", "bash"}, TimeZone: "America/Chicago",
			Policy: &fw.Rules{Default: "deny", Deny: []string{"bash"}}}}
	run, member, err := s.Spawn(ctx, parent, req)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if run.ID != "01CHILD" || run.Principal != "ada" || !strings.HasPrefix(member, "ax:team/migrator-") {
		t.Fatalf("run = %+v, member %q", run, member)
	}
	// The Task is the parent Task with a new name, no goal, no debug, and
	// the variables of a child.
	tasks := ax.created()
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d", len(tasks))
	}
	task := tasks[0]
	if task.GetMetadata().GetAtespace() != "team" || "ax:team/"+task.GetMetadata().GetName() != member || task.GetSpec().GetImage() != "ghcr.io/example/fw:toolbox" ||
		len(task.GetSpec().GetCommand()) != 2 || task.GetSpec().GetDebug() || task.GetSpec().GetResources().GetRequests().GetCpu() != "500m" {
		t.Fatalf("task = %+v", task)
	}
	if ws := task.GetSpec().GetWorkspaces(); len(ws) != 1 || ws[0].GetName() != "repo" || ws[0].GetGoal() != "" {
		t.Fatalf("workspaces = %+v", ws)
	}
	env := map[string]string{}
	for _, e := range task.GetSpec().GetEnv() {
		env[e.GetName()] = e.GetValue()
	}
	if env["FW_MODEL"] != "openai/x" || env[EnvParentRun] != "01PARENT" || env[EnvParentMember] != "ax:team/migrator" || env[EnvSpawnMaxDepth] != "2" || env["FW_API_TOKEN"] != "fleet-secret" {
		t.Fatalf("env = %v", env)
	}
	// An inherited depth variable under either name is out, and the real
	// depth variable appears once.
	depths := 0
	for _, e := range task.GetSpec().GetEnv() {
		if e.GetName() == "FW_SPAWN_MAXDEPTH" || e.GetName() == EnvSpawnMaxDepth {
			depths++
		}
	}
	if depths != 1 {
		t.Fatalf("the child kept %d depth variables", depths)
	}
	disabledVars := 0
	for _, e := range task.GetSpec().GetEnv() {
		if e.GetName() == EnvSpawnDisabled {
			disabledVars++
		}
	}
	if disabledVars != 1 || env[EnvSpawnDisabled] != "false" {
		t.Fatalf("child kept stale disabled setting: %v", task.GetSpec().GetEnv())
	}
	if _, ok := env["FW_WORKBENCH_FLEET_NAME"]; ok {
		t.Fatal("the child kept the fleet name of the parent")
	}
	// The run went to the child through the router, as the person, with
	// the budget, the tools, and the parent in the metadata, and the retry
	// carried the same idempotency key.
	router.mu.Lock()
	body, keys, actors := router.body, router.keys, router.actors
	router.mu.Unlock()
	if body["principal"] != "ada" || body["principalKind"] != "user" || body["input"] != "migrate module x" {
		t.Fatalf("body = %v", body)
	}
	meta, _ := body["metadata"].(map[string]any)
	if meta[fw.MetaParent] != "01PARENT" || meta[fw.MetaParentMember] != "ax:team/migrator" {
		t.Fatalf("metadata = %v", meta)
	}
	budget, _ := body["budget"].(map[string]any)
	if budget["maxTurns"] != float64(3) || budget["deadline"] != "5m0s" {
		t.Fatalf("budget = %v", budget)
	}
	profile, _ := body["profile"].(map[string]any)
	if tools, _ := profile["tools"].([]any); len(tools) != 2 {
		t.Fatalf("profile = %v", profile)
	}
	// The time zone and the rules of the profile travel with the run.
	if profile["timeZone"] != "America/Chicago" {
		t.Fatalf("the time zone of the profile did not travel: %v", profile)
	}
	policy, _ := profile["policy"].(map[string]any)
	if policy == nil || policy["default"] != "deny" || joinAny(policy["deny"], "deny") != "bash" {
		t.Fatalf("the policy of the profile did not travel: %v", profile)
	}
	if len(keys) != 2 || keys[0] != keys[1] || !strings.HasPrefix(keys[0], "spawn:01PARENT:team/") {
		t.Fatalf("idempotency keys = %v", keys)
	}
	for _, a := range actors {
		if "ax:"+a != member {
			t.Fatalf("actor = %q, want the child", a)
		}
	}

	// The run methods reach the child.
	got, err := s.Get(ctx, member, "01CHILD")
	if err != nil || got.State != fw.StateRunning {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	done, err := s.WaitFor(ctx, member, "01CHILD", func(r *fw.Run) bool { return r.State.Terminal() })
	if err != nil || done.State != fw.StateCompleted || done.Output != "child done" {
		t.Fatalf("WaitFor = %+v, %v", done, err)
	}
	if err := s.Decide(ctx, member, "01CHILD", fw.ApprovalDecision{ApprovalID: "ap1", Decision: "allow", Note: "go"}); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if err := s.Decide(ctx, member, "01CHILD", fw.ApprovalDecision{ApprovalID: "stale", Decision: "allow"}); !errors.Is(err, fw.ErrNoPendingApproval) {
		t.Fatalf("Decide on a stale approval = %v", err)
	}
	if _, err := s.Get(ctx, member, "01NOPE"); !errors.Is(err, fw.ErrRunNotFound) {
		t.Fatalf("Get unknown = %v", err)
	}
	if _, err := s.Get(ctx, "http://elsewhere", "01CHILD"); !errors.Is(err, fw.ErrRunNotFound) {
		t.Fatalf("Get at a member that is not a Task = %v", err)
	}
	if err := s.Cancel(ctx, member, "01CHILD"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	router.mu.Lock()
	decisions, canceled := router.decisions, router.canceled
	router.mu.Unlock()
	if len(decisions) != 2 || decisions[0]["approval"] != "ap1" || decisions[0]["decision"] != "allow" || decisions[0]["note"] != "go" || !canceled {
		t.Fatalf("decisions = %v, canceled %v", decisions, canceled)
	}
}

func TestSpawnDeletesTheTaskWhenTheRunDoesNotStart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ax := newFakeAX(t)
	router := newFakeRouter(t)
	router.token = "another-secret" // the child refuses the token, so the run never starts
	s := newSpawnerUnderTest(t, ax, router)
	_, _, err := s.Spawn(ctx, &fw.Run{ID: "01PARENT"}, fw.RunRequest{Input: "x"})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("Spawn = %v", err)
	}
	ax.mu.Lock()
	deleted := ax.deleted
	ax.mu.Unlock()
	if len(deleted) != 1 || !strings.HasPrefix(deleted[0], "team/migrator-") {
		t.Fatalf("deleted = %v", deleted)
	}
}

func TestNewSpawnerNeedsTheRouter(t *testing.T) {
	cfg := fw.DefaultConfig()
	cfg.AX.Router = ""
	if _, err := NewSpawner(cfg); err == nil || !strings.Contains(err.Error(), "ax.router") {
		t.Fatalf("NewSpawner without a router = %v", err)
	}
	cfg.AX.Router = "atenet-router.ate-system.svc.cluster.local"
	s, err := NewSpawner(cfg)
	if err != nil || s.server != DefaultServer || s.tokenEnv != "FW_API_TOKEN" {
		t.Fatalf("NewSpawner = %+v, %v", s, err)
	}
	prevTask, prevWS := Stashed()
	t.Cleanup(func() { Stash(prevTask, prevWS) })
	Stash(nil, nil)
	if _, _, err := s.Spawn(context.Background(), &fw.Run{ID: "p"}, fw.RunRequest{Input: "x"}); err == nil || !strings.Contains(err.Error(), "not an ax Task") {
		t.Fatalf("Spawn outside ax = %v", err)
	}
	for _, name := range []string{"migrator", strings.Repeat("a", 80)} {
		child, err := childName(name, "")
		if err != nil || len(child) > maxTaskName || !strings.HasPrefix(child, name[:min(len(name), 20)]) {
			t.Fatalf("childName(%d) = %q, %v", len(name), child, err)
		}
	}
}

// joinAny flattens a JSON list of strings from a decoded body for one
// assertion.
func joinAny(v any, field string) string {
	list, _ := v.([]any)
	var out []string
	for _, e := range list {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return strings.Join(out, ",")
}

// TestChildTaskCarriesTheDepthTheConfigExpects walks the depth remnant that
// the child Task carries as the configuration expects it. A child past the
// A child Task carries the egress of its parent, so it reaches what the
// parent reaches and nothing more; a parent without egress gives none.
func TestChildTaskInheritsTheEgress(t *testing.T) {
	ax := newFakeAX(t)
	router := newFakeRouter(t)
	s := newSpawnerUnderTest(t, ax, router)
	base := &Task{Metadata: ObjectMeta{Name: "migrator", Atespace: "team"}, Spec: TaskSpec{Image: "img", Egress: &EgressSpec{Rules: []EgressRule{
		{HTTP: &EgressHosts{Hostnames: []string{"nats.agents.svc"}, Ports: []int32{8080}}},
		{HTTPS: &EgressHosts{Hostnames: []string{"api.example.com"}}},
		{TLS: &EgressHosts{Hostnames: []string{"github.com", "*.github.com"}, Ports: []int32{443}}},
		{},
	}}}}
	child := s.childTask(base, "team", "c", &fw.Run{ID: "01P"}, -1)
	rules := child.Spec.GetEgress().GetRules()
	if len(rules) != 3 {
		t.Fatalf("rules = %v, want 3", rules)
	}
	if h := rules[0].GetHttp(); h == nil || h.GetHostnames()[0] != "nats.agents.svc" || h.GetPorts()[0] != 8080 {
		t.Fatalf("http rule = %v", rules[0])
	}
	if h := rules[1].GetHttps(); h == nil || h.GetHostnames()[0] != "api.example.com" || len(h.GetPorts()) != 0 {
		t.Fatalf("https rule = %v", rules[1])
	}
	if h := rules[2].GetTls(); h == nil || len(h.GetHostnames()) != 2 || h.GetPorts()[0] != 443 {
		t.Fatalf("tls rule = %v", rules[2])
	}
	plain := s.childTask(&Task{Metadata: ObjectMeta{Name: "m", Atespace: "team"}, Spec: TaskSpec{Image: "img"}}, "team", "c", &fw.Run{ID: "01P"}, -1)
	if plain.Spec.GetEgress() != nil {
		t.Fatalf("a parent without egress gave the child %v", plain.Spec.GetEgress())
	}
}

// depth of the tree gets no depth variable, because FW_SPAWN_MAX_DEPTH = 0
// means unlimited in the configuration; FW_SPAWN_DISABLED represents an
// exhausted allowance independently of the tool policy.
func TestChildTaskCarriesTheDepthTheConfigExpects(t *testing.T) {
	ax := newFakeAX(t)
	router := newFakeRouter(t)
	s := newSpawnerUnderTest(t, ax, router)
	base := &Task{Metadata: ObjectMeta{Name: "migrator", Atespace: "team"}, Spec: TaskSpec{Image: "img"}}
	for depth, want := range map[int]string{0: "2", 1: "1", 2: "", 3: ""} {
		child := s.childTask(base, "team", "c", &fw.Run{ID: "01P", Depth: depth}, -1)
		env := map[string]string{}
		for _, e := range child.Spec.Env {
			env[e.Name] = e.Value
		}
		if got := env[EnvSpawnMaxDepth]; got != want {
			t.Fatalf("depth %d leaves FW_SPAWN_MAX_DEPTH = %q, want %q", depth, got, want)
		}
		if disabled := env[EnvSpawnDisabled] == "true"; disabled != (depth >= 2) {
			t.Fatalf("depth %d disabled=%v", depth, disabled)
		}
	}
	s.cfg.Spawn.MaxDepth = 0
	child := s.childTask(base, "team", "c", &fw.Run{ID: "01P", Depth: 20}, -1)
	for _, e := range child.Spec.Env {
		if (e.Name == EnvSpawnMaxDepth && e.Value != "0") || (e.Name == EnvSpawnDisabled && e.Value != "false") {
			t.Fatalf("unlimited depth changed meaning: %v", child.Spec.Env)
		}
	}
}

// TestChildTaskCarriesItsShareOfTheTree proves that a child Task gets the
// runs that its share of spawn.maxDescendants leaves under it, that a share
// without room disables spawning there, because zero means unlimited, and
// that an inherited value never survives. A branch has no share and gets
// the limit of this process.
func TestChildTaskCarriesItsShareOfTheTree(t *testing.T) {
	ax := newFakeAX(t)
	router := newFakeRouter(t)
	s := newSpawnerUnderTest(t, ax, router)
	base := &Task{Metadata: ObjectMeta{Name: "migrator", Atespace: "team"}, Spec: TaskSpec{Image: "img",
		Env: []EnvVar{{Name: EnvSpawnMaxDescendants, Value: "999"}}}}
	for _, tc := range []struct {
		share    int
		want     string
		disabled bool
	}{{2, "2", false}, {0, "", true}, {-1, fmt.Sprint(fw.DefaultSpawnDescendants), false}} {
		child := s.childTask(base, "team", "c", &fw.Run{ID: "01P"}, tc.share)
		env, n := map[string]string{}, 0
		for _, e := range child.Spec.Env {
			env[e.Name] = e.Value
			if e.Name == EnvSpawnMaxDescendants {
				n++
			}
		}
		if got := env[EnvSpawnMaxDescendants]; got != tc.want || n > 1 || (env[EnvSpawnDisabled] == "true") != tc.disabled {
			t.Fatalf("share %d: env = %v", tc.share, env)
		}
	}
	// Spawn reads the share from the request that the kernel filled.
	parent := &fw.Run{ID: "01PARENT"}
	req := fw.RunRequest{ID: "01CHILD", Input: "x", Metadata: map[string]string{fw.MetaParent: parent.ID, fw.MetaMaxDescendants: "3"}}
	if _, _, err := s.Spawn(context.Background(), parent, req); err != nil {
		t.Fatal(err)
	}
	got := ""
	for _, e := range ax.created()[0].GetSpec().GetEnv() {
		if e.GetName() == EnvSpawnMaxDescendants {
			got = e.GetValue()
		}
	}
	if got != "3" {
		t.Fatalf("the child Task got %s=%q", EnvSpawnMaxDescendants, got)
	}
}

// TestSpawnCarriesTheComposedProfileThroughTheRoundTrip runs a child Task
// run against a real native surface: the ax encoding carries the policy and
// the time zone of the profile, the native surface decodes them, and the
// child run keeps them in its ledger.
func TestSpawnCarriesTheComposedProfileThroughTheRoundTrip(t *testing.T) {
	ax := newFakeAX(t)
	child, ledger := childProcessAgent(t)
	srv, err := child.Handler()
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(srv)
	t.Cleanup(httpServer.Close)
	s := newSpawnerAt(t, ax, strings.TrimPrefix(httpServer.URL, "http://"))

	parent := &fw.Run{ID: "01PARENT", Depth: 0}
	policy := fw.MergeRules(fw.Rules{Default: "deny", Allow: []string{"files", "bash: git status"}}, fw.Rules{Ask: []string{"files"}})
	req := fw.RunRequest{Input: "collect the summaries", Principal: "ada", PrincipalKind: fw.KindUser,
		Metadata: map[string]string{fw.MetaParent: parent.ID},
		Profile: &fw.AgentProfile{System: "Be careful.", Tools: []string{"files", "bash*"}, TimeZone: "America/Chicago",
			Policy: &policy}}
	run, member, err := s.Spawn(context.Background(), parent, req)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if !strings.HasPrefix(member, "ax:team/migrator-") {
		t.Fatalf("member = %q", member)
	}
	done, err := s.WaitFor(context.Background(), member, run.ID, func(r *fw.Run) bool { return r.State.Terminal() })
	if err != nil || done.State != fw.StateCompleted || done.Output != "child done" {
		t.Fatalf("child run = %+v, %v", done, err)
	}
	var queued fw.RunQueuedData
	for ev, err := range ledger.Read(context.Background(), run.ID, 1) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Type == fw.EvRunQueued {
			if err := ev.Decode(&queued); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if queued.Profile == nil || queued.Profile.Policy == nil {
		t.Fatalf("queued profile = %+v", queued.Profile)
	}
	p := queued.Profile
	if p.TimeZone != "America/Chicago" || joinAnyListOf(p.Tools) != "files,bash*" || p.System != "Be careful." {
		t.Fatalf("the profile of the child run lost its shape: %+v", p)
	}
	for _, tc := range []struct {
		name, input string
		want        fw.Verdict
	}{
		{"files", `{}`, fw.VerdictAsk},
		{"bash", `{"command":"git status"}`, fw.VerdictAllow},
		{"bash", `{"command":"git push"}`, fw.VerdictDeny},
		{"http", `{}`, fw.VerdictDeny},
	} {
		got := fw.NewRulesPolicy(*p.Policy).Decide(context.Background(), fw.ToolCall{Spec: fw.ToolSpec{Name: tc.name}, Input: json.RawMessage(tc.input)})
		if got.Verdict != tc.want {
			t.Fatalf("policy after AX/native roundtrip for %s: %+v, want %v", tc.name, got, tc.want)
		}
	}
	if queued.Metadata[fw.MetaParent] != "01PARENT" {
		t.Fatalf("the child run lost its parent: %v", queued.Metadata)
	}
}

// childProcessAgent is the agent that a child Task runs: one run that a
// fake model finishes at once, its in-process ledger, and the native
// surface.
func childProcessAgent(t *testing.T) (*fw.Agent, fw.Ledger) {
	t.Helper()
	m := fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
		return &fw.Response{Content: []fw.Block{fw.Text("child done")}, StopReason: fw.StopEndTurn, Usage: fw.Usage{InputTokens: 1, OutputTokens: 1}}, nil
	}}
	ledger := fw.NewMemLedger()
	a, err := fw.New(fw.WithoutDefaults(), fw.WithAuthn(fw.NoAuthn{}),
		fw.WithModelName("anthropic/claude-opus-5-5"), fw.WithModel(m), fw.WithLedger(ledger),
		fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		fw.WithSurfaces(native.New()))
	if err != nil {
		t.Fatalf("fw.New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	return a, ledger
}

// joinAnyListOf joins a list of strings for one assertion.
func joinAnyListOf(list []string) string { return strings.Join(list, ",") }

// Place creates a child Task as Spawn does and names it at once; Branch
// then starts the branch there from the kept events, under the id of the
// request, with an idempotency key of its own.
func TestForkCreatesAChildTaskAndStartsTheBranch(t *testing.T) {
	ctx := context.Background()
	ax := newFakeAX(t)
	router := newFakeRouter(t)
	s := newSpawnerUnderTest(t, ax, router)
	parent := &fw.Run{ID: "01PARENT", Depth: 0}
	member, err := s.Place(ctx, parent)
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	if !strings.HasPrefix(member, "ax:team/migrator-") {
		t.Fatalf("member %q", member)
	}
	if tasks := ax.created(); len(tasks) != 1 {
		t.Fatalf("tasks = %d", len(tasks))
	}
	ax.mu.Lock()
	resumed := append([]string(nil), ax.resumed...)
	ax.mu.Unlock()
	if len(resumed) != 1 || !strings.HasPrefix(resumed[0], "team/migrator-") {
		t.Fatalf("the child Task was not resumed: %v", resumed)
	}
	events := []fw.Event{{Type: fw.EvRunQueued, Subject: "01PARENT", Seq: 1, Data: json.RawMessage(`{"agent":"default","input":"hello"}`)}, {Type: fw.EvRunStarted, Subject: "01PARENT", Seq: 2, Data: json.RawMessage(`{}`)}}
	run, err := s.Branch(ctx, member, events, fw.ForkRequest{ID: "01BRANCHID", Input: "try another way"})
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}
	if run.ID != "01BRANCHID" {
		t.Fatalf("run = %+v", run)
	}
	router.mu.Lock()
	defer router.mu.Unlock()
	evs, _ := router.body["events"].([]any)
	if len(evs) != 2 || router.body["input"] != "try another way" || router.body["id"] != "01BRANCHID" || len(router.keys) != 1 || router.keys[0] != "fork:01BRANCHID" {
		t.Fatalf("the child got body %v with keys %v", router.body, router.keys)
	}
}

// TestSpawnWithoutTheTokenFailsBeforeTheTask checks that a parent without
// the static token of the fleet fails at once, names the variable, and
// creates no child Task (scenario1 D8).
func TestSpawnWithoutTheTokenFailsBeforeTheTask(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ax := newFakeAX(t)
	router := newFakeRouter(t)
	s := newSpawnerUnderTest(t, ax, router)
	t.Setenv("FW_TEST_FLEET_TOKEN", "")
	start := time.Now()
	if _, err := s.Plan(&fw.Run{ID: "01PARENT"}, fw.RunRequest{ID: fw.NewID()}); err == nil || !strings.Contains(err.Error(), "FW_TEST_FLEET_TOKEN is not set") {
		t.Fatalf("Plan = %v", err)
	}
	_, _, err := s.Spawn(ctx, &fw.Run{ID: "01PARENT"}, fw.RunRequest{ID: fw.NewID(), Input: "x"})
	if err == nil || !strings.Contains(err.Error(), "FW_TEST_FLEET_TOKEN is not set") {
		t.Fatalf("Spawn = %v", err)
	}
	if _, err := s.Place(ctx, &fw.Run{ID: "01PARENT"}); err == nil || !strings.Contains(err.Error(), "FW_TEST_FLEET_TOKEN is not set") {
		t.Fatalf("Place = %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("the refusal took %s", d)
	}
	if tasks := ax.created(); len(tasks) != 0 {
		t.Fatalf("created %d child Tasks", len(tasks))
	}
	// A token that goes away after the Task exists ends the start at once.
	_, err = s.startOn(ctx, "team/migrator-x", "/v1/runs", []byte(`{}`), "k")
	var noToken *noTokenError
	if !errors.As(err, &noToken) {
		t.Fatalf("startOn = %v", err)
	}
}
