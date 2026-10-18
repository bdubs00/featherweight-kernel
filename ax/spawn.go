package ax

// This file is the ax Spawner: with spawn.driver "ax", each child of
// spawn_task runs in a child Task of its own (docs/ARCHITECTURE.md section
// 11.9). The spawner creates the Task through the ax.v1alpha1.AX gRPC
// service at ax.server with the image, the environment, the resources, and
// the workspaces of this Task, waits until the Task runs, starts the child
// run through the atenet router with the static token of the fleet and the
// person of the parent, and follows the run through the native API of the
// child. The child Task gets FW_PARENT_RUN, FW_PARENT_MEMBER, and the depth
// that spawn.maxDepth leaves it in FW_SPAWN_MAX_DEPTH.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	v1alpha1 "github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func init() {
	fw.RegisterSpawner("ax", func(_ context.Context, cfg *fw.Config) (fw.Spawner, error) { return NewSpawner(cfg) })
}

// Variables that a child Task gets beyond the ones of its parent.
const (
	EnvParentRun     = "FW_PARENT_RUN"      // the id of the parent run
	EnvParentMember  = "FW_PARENT_MEMBER"   // the parent process, "ax:<atespace>/<task>"
	EnvSpawnMaxDepth = "FW_SPAWN_MAX_DEPTH" // the levels below a child that spawn.maxDepth still allows
	EnvSpawnDisabled = "FW_SPAWN_DISABLED"  // a leaf has no remaining delegation depth
)

// DefaultServer is the ax API server inside the cluster.
const DefaultServer = "ax-server.ax-system.svc.cluster.local:8080"

// Limits of the spawner.
const (
	readyTimeout = 10 * time.Minute // how long a child Task gets to run
	startTimeout = 5 * time.Minute  // how long the first request to a child gets
	maxTaskName  = 63
	maxAnswer    = 4 << 20
	phaseRunning = "Running"
)

// droppedEnv names the variables of the parent that a child does not get:
// the ones that this file sets, under either spelling of the depth variable
// that earlier revisions used, and the fleet identity, which the child
// derives from its own Task. fw reads the depth from FW_SPAWN_MAX_DEPTH,
// so an inherited value of either name would name the remaining depth of
// the parent, not of the child, and would be a lie.
var droppedEnv = map[string]bool{
	EnvParentRun: true, EnvParentMember: true, EnvSpawnMaxDepth: true, EnvSpawnDisabled: true,
	"FW_SPAWN_MAXDEPTH":       true,
	"FW_WORKBENCH_FLEET_NAME": true, "FW_WORKBENCH_FLEET_URL": true,
}

// Spawner runs child runs in child Tasks. It implements fw.Spawner.
type Spawner struct {
	cfg      *fw.Config
	server   string // the ax API server
	router   string // the atenet router, host[:port]
	tokenEnv string // the Secrets name of the static token of the fleet
	http     *http.Client
	log      *slog.Logger

	// readyPoll and runPoll are the intervals of the waits; tests shorten them.
	readyPoll time.Duration
	runPoll   time.Duration

	mu     sync.Mutex
	conn   *grpc.ClientConn
	client v1alpha1.AXClient
}

var _ fw.Spawner = (*Spawner)(nil)

// NewSpawner returns the spawner of the configuration. It needs ax.router,
// because the native API of a child Task is reached through the router.
func NewSpawner(cfg *fw.Config) (*Spawner, error) {
	router := strings.TrimSpace(cfg.AX.Router)
	if router == "" {
		return nil, errors.New("ax: spawn.driver ax needs ax.router (FW_AX_ROUTER), the atenet router that reaches a child Task")
	}
	if u, err := url.Parse("http://" + router); err != nil || u.Host == "" || u.Path != "" {
		return nil, fmt.Errorf("ax: ax.router %q is not a host", router)
	}
	server := strings.TrimSpace(cfg.AX.Server)
	if server == "" {
		server = DefaultServer
	}
	tokenEnv := strings.TrimSpace(cfg.Auth.TokenEnv)
	if tokenEnv == "" {
		tokenEnv = "FW_API_TOKEN"
	}
	return &Spawner{
		cfg:       cfg,
		server:    server,
		router:    router,
		tokenEnv:  tokenEnv,
		http:      &http.Client{Timeout: 2 * time.Minute},
		log:       slog.Default().With("component", "ax.spawn"),
		readyPoll: 2 * time.Second,
		runPoll:   time.Second,
	}, nil
}

// ax returns the gRPC client of the ax API, and opens it on first use.
func (s *Spawner) ax() (v1alpha1.AXClient, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil {
		return s.client, nil
	}
	conn, err := grpc.NewClient(s.server, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("ax: connect to the ax server %s: %w", s.server, err)
	}
	s.conn, s.client = conn, v1alpha1.NewAXClient(conn)
	return s.client, nil
}

// Close closes the connection to the ax API.
func (s *Spawner) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return nil
	}
	err := s.conn.Close()
	s.conn, s.client = nil, nil
	return err
}

// Spawn implements fw.Spawner.
func (s *Spawner) Spawn(ctx context.Context, parent *fw.Run, req fw.RunRequest) (*fw.Run, string, error) {
	task, _ := Stashed()
	if task == nil {
		return nil, "", errors.New("ax: this process is not an ax Task, so it cannot start a child Task")
	}
	atespace, name := Identity(task)
	if atespace == "" || name == "" {
		return nil, "", errors.New("ax: the Task of this process has no atespace or no name")
	}
	child, err := childName(name)
	if err != nil {
		return nil, "", err
	}
	if req.Metadata == nil {
		req.Metadata = map[string]string{}
	}
	req.Metadata[fw.MetaParentMember] = "ax:" + atespace + "/" + name
	client, err := s.ax()
	if err != nil {
		return nil, "", err
	}
	spec := s.childTask(task, atespace, child, parent)
	if _, err := client.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: spec}); err != nil {
		return nil, "", fmt.Errorf("ax: create the child Task %s/%s: %w", atespace, child, err)
	}
	s.log.Info("child Task created", "atespace", atespace, "task", child, "parent", parent.ID)
	if err := s.awaitRunning(ctx, client, atespace, child); err != nil {
		s.deleteTask(client, atespace, child)
		return nil, "", err
	}
	actor := atespace + "/" + child
	run, err := s.startRun(ctx, actor, parent, req)
	if err != nil {
		s.deleteTask(client, atespace, child)
		return nil, "", err
	}
	return run, "ax:" + actor, nil
}

// childName makes the name of a child Task from the name of its parent and
// a random suffix, within the length of a DNS label.
func childName(parent string) (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("ax: random name: %w", err)
	}
	suffix := fmt.Sprintf("%x", b)
	parent = strings.ToLower(parent)
	if n := maxTaskName - len(suffix) - 1; len(parent) > n {
		parent = strings.TrimRight(parent[:n], "-")
	}
	return parent + "-" + suffix, nil
}

// childTask is the Task of a child: the parent Task with a new name, no
// goals, no debug services, and the variables of a child.
func (s *Spawner) childTask(task *Task, atespace, name string, parent *fw.Run) *v1alpha1.Task {
	spec := &v1alpha1.TaskSpec{Image: task.Spec.Image, Command: append([]string(nil), task.Spec.Command...)}
	for _, e := range task.Spec.Env {
		if !droppedEnv[e.Name] {
			spec.Env = append(spec.Env, &v1alpha1.EnvVar{Name: e.Name, Value: e.Value})
		}
	}
	spec.Env = append(spec.Env,
		&v1alpha1.EnvVar{Name: EnvParentRun, Value: parent.ID},
		&v1alpha1.EnvVar{Name: EnvParentMember, Value: "ax:" + atespace + "/" + task.Metadata.Name},
	)
	// Zero maxDepth means unlimited, so a separate flag represents an
	// exhausted allowance. It also protects direct Go child creation and
	// custom delegation tools, independently of spawn_task policy rules.
	disabled := s.cfg.Spawn.Disabled
	if max := s.cfg.Spawn.MaxDepth; max > 0 {
		if left := max - parent.Depth - 1; left >= 1 {
			spec.Env = append(spec.Env, &v1alpha1.EnvVar{Name: EnvSpawnMaxDepth, Value: fmt.Sprint(left)})
		} else {
			disabled = true
		}
	} else {
		// Preserve an explicitly unlimited tree instead of picking up the
		// child's default finite maxDepth from its configuration file.
		spec.Env = append(spec.Env, &v1alpha1.EnvVar{Name: EnvSpawnMaxDepth, Value: "0"})
	}
	spec.Env = append(spec.Env, &v1alpha1.EnvVar{Name: EnvSpawnDisabled, Value: fmt.Sprint(disabled)})
	r := task.Spec.Resources
	if r.Requests != (ResourceList{}) || r.Limits != (ResourceList{}) {
		spec.Resources = &v1alpha1.ResourcesSpec{
			Requests: &v1alpha1.ResourceList{Cpu: r.Requests.CPU, Memory: r.Requests.Memory},
			Limits:   &v1alpha1.ResourceList{Cpu: r.Limits.CPU, Memory: r.Limits.Memory},
		}
	}
	for _, w := range task.Spec.Workspaces {
		// No goal: a child works on the run that its parent starts, not on
		// a bootstrap run of its own.
		spec.Workspaces = append(spec.Workspaces, &v1alpha1.WorkspaceRef{Name: w.Name, Path: w.Path})
	}
	apiVersion, kind := task.APIVersion, task.Kind
	if apiVersion == "" {
		apiVersion = "ax.io/v1alpha1"
	}
	if kind == "" {
		kind = "Task"
	}
	return &v1alpha1.Task{
		ApiVersion: apiVersion,
		Kind:       kind,
		Metadata:   &v1alpha1.ObjectMeta{Name: name, Atespace: atespace},
		Spec:       spec,
	}
}

// awaitRunning waits until the Task runs.
func (s *Spawner) awaitRunning(ctx context.Context, client v1alpha1.AXClient, atespace, name string) error {
	deadline := time.Now().Add(readyTimeout)
	for {
		t, err := client.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: atespace, Name: name})
		if err != nil {
			return fmt.Errorf("ax: read the child Task %s/%s: %w", atespace, name, err)
		}
		if t.GetStatus().GetPhase() == phaseRunning {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ax: the child Task %s/%s is %q after %s", atespace, name, t.GetStatus().GetPhase(), readyTimeout)
		}
		if err := sleep(ctx, s.readyPoll); err != nil {
			return err
		}
	}
}

// deleteTask removes a child Task that never got its run.
func (s *Spawner) deleteTask(client v1alpha1.AXClient, atespace, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := client.DeleteTask(ctx, &v1alpha1.DeleteTaskRequest{Atespace: atespace, Name: name}); err != nil {
		s.log.Warn("delete the child Task", "atespace", atespace, "task", name, "error", err)
	}
}

// runBody is the body of POST /v1/runs of the child, in the shape that the
// native API takes.
type runBody struct {
	Agent         string            `json:"agent,omitempty"`
	Input         string            `json:"input"`
	Instructions  string            `json:"instructions,omitempty"`
	Profile       *profileBody      `json:"profile,omitempty"`
	Budget        *budgetBody       `json:"budget,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	Principal     string            `json:"principal,omitempty"`
	PrincipalKind string            `json:"principalKind,omitempty"`
}

type profileBody struct {
	System       string      `json:"system,omitempty"`
	Instructions []string    `json:"instructions,omitempty"`
	Skills       []string    `json:"skills,omitempty"`
	Tools        []string    `json:"tools,omitempty"`
	Model        string      `json:"model,omitempty"`
	Effort       string      `json:"effort,omitempty"`
	TimeZone     string      `json:"timeZone,omitempty"`
	Policy       *rulesBody  `json:"policy,omitempty"`
	Budget       *budgetBody `json:"budget,omitempty"`
}

// rulesBody is fw.Rules in the shape that the native API takes.
type rulesBody struct {
	Default string      `json:"default,omitempty"`
	Allow   []string    `json:"allow,omitempty"`
	Ask     []string    `json:"ask,omitempty"`
	Deny    []string    `json:"deny,omitempty"`
	All     []rulesBody `json:"all,omitempty"`
}

// rulesOf converts the rules of a profile. It returns nil for nil, so
// a profile without rules stays a profile without rules and the rules of
// the child process alone hold. The kernel of the parent composes the rules
// of a child with the base rules of this process before Spawn travels it;
// this conversion states them the same words that the children read.
func rulesOf(r *fw.Rules) *rulesBody {
	if r == nil {
		return nil
	}
	out := &rulesBody{Default: r.Default, Allow: r.Allow, Ask: r.Ask, Deny: r.Deny}
	for i := range r.All {
		out.All = append(out.All, *rulesOf(&r.All[i]))
	}
	return out
}

type budgetBody struct {
	MaxTurns        int     `json:"maxTurns,omitempty"`
	MaxTokens       int     `json:"maxTokens,omitempty"`
	MaxCostUSD      float64 `json:"maxCostUSD,omitempty"`
	Deadline        string  `json:"deadline,omitempty"`
	MaxToolCalls    int     `json:"maxToolCalls,omitempty"`
	MaxCallsPerTurn int     `json:"maxCallsPerTurn,omitempty"`
	MaxRepeats      int     `json:"maxRepeats,omitempty"`
}

func budgetOf(b *fw.Budget) *budgetBody {
	if b == nil {
		return nil
	}
	out := &budgetBody{MaxTurns: b.MaxTurns, MaxTokens: b.MaxTokens, MaxCostUSD: b.MaxCostUSD,
		MaxToolCalls: b.MaxToolCalls, MaxCallsPerTurn: b.MaxCallsPerTurn, MaxRepeats: b.MaxRepeats}
	if b.Deadline > 0 {
		out.Deadline = b.Deadline.String()
	}
	return out
}

func bodyOf(req fw.RunRequest) runBody {
	body := runBody{Agent: req.Agent, Input: req.Input, Instructions: req.Instructions, Budget: budgetOf(req.Budget),
		Metadata: req.Metadata, Principal: req.Principal, PrincipalKind: req.PrincipalKind}
	if p := req.Profile; p != nil {
		body.Profile = &profileBody{System: p.System, Instructions: p.Instructions, Skills: p.Skills, Tools: p.Tools,
			Model: p.Model, Effort: p.Effort, TimeZone: p.TimeZone, Policy: rulesOf(p.Policy), Budget: budgetOf(p.Budget)}
	}
	return body
}

// startRun starts the child run through the router. The child boots after
// its Task runs, so the request is sent again on a gateway error, with the
// same idempotency key.
func (s *Spawner) startRun(ctx context.Context, actor string, parent *fw.Run, req fw.RunRequest) (*fw.Run, error) {
	body, err := json.Marshal(bodyOf(req))
	if err != nil {
		return nil, fmt.Errorf("ax: encode the child run: %w", err)
	}
	key := "spawn:" + parent.ID + ":" + actor
	deadline := time.Now().Add(startTimeout)
	wait := time.Second
	for {
		status, answer, err := s.do(ctx, http.MethodPost, actor, "/v1/runs", body, key)
		switch {
		case err == nil && (status == http.StatusCreated || status == http.StatusOK):
			var run fw.Run
			if err := json.Unmarshal(answer, &run); err != nil || run.ID == "" {
				return nil, fmt.Errorf("ax: the child %s answered with no run", actor)
			}
			return &run, nil
		case err == nil && status != http.StatusBadGateway && status != http.StatusServiceUnavailable && status != http.StatusGatewayTimeout:
			return nil, problemOf(actor, status, answer)
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case time.Now().After(deadline):
			if err != nil {
				return nil, fmt.Errorf("ax: the child %s did not take its run within %s: %w", actor, startTimeout, err)
			}
			return nil, fmt.Errorf("ax: the child %s did not take its run within %s: %s", actor, startTimeout, http.StatusText(status))
		}
		if err := sleep(ctx, wait); err != nil {
			return nil, err
		}
		if wait < 10*time.Second {
			wait *= 2
		}
	}
}

// actorOf returns the atenet actor of a member name, "ax:<atespace>/<task>".
func actorOf(member string) (string, error) {
	actor, ok := strings.CutPrefix(member, "ax:")
	if space, task, found := strings.Cut(actor, "/"); !ok || !found || space == "" || task == "" || strings.Contains(task, "/") {
		return "", fmt.Errorf("%w: %q is not a child Task of this process", fw.ErrRunNotFound, member)
	}
	return actor, nil
}

// Get implements fw.Spawner.
func (s *Spawner) Get(ctx context.Context, member, id string) (*fw.Run, error) {
	actor, err := actorOf(member)
	if err != nil {
		return nil, err
	}
	status, answer, err := s.do(ctx, http.MethodGet, actor, "/v1/runs/"+url.PathEscape(id), nil, "")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, problemOf(actor, status, answer)
	}
	var run fw.Run
	if err := json.Unmarshal(answer, &run); err != nil || run.ID == "" {
		return nil, fmt.Errorf("ax: the child %s answered with no run", actor)
	}
	return &run, nil
}

// WaitFor implements fw.Spawner: it reads the run until done reports true.
func (s *Spawner) WaitFor(ctx context.Context, member, id string, done func(*fw.Run) bool) (*fw.Run, error) {
	var last *fw.Run
	for {
		r, err := s.Get(ctx, member, id)
		if err != nil {
			if ctx.Err() != nil {
				return last, ctx.Err()
			}
			return nil, err
		}
		if done(r) {
			return r, nil
		}
		last = r
		if err := sleep(ctx, s.runPoll); err != nil {
			return last, err
		}
	}
}

// Decide implements fw.Spawner.
func (s *Spawner) Decide(ctx context.Context, member, id string, d fw.ApprovalDecision) error {
	actor, err := actorOf(member)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"decision": d.Decision, "note": d.Note, "content": d.Content})
	status, answer, err := s.do(ctx, http.MethodPost, actor, "/v1/runs/"+url.PathEscape(id)+"/approvals/"+url.PathEscape(d.ApprovalID), body, "")
	if err != nil {
		return err
	}
	if status >= 300 {
		return problemOf(actor, status, answer)
	}
	return nil
}

// Cancel implements fw.Spawner.
func (s *Spawner) Cancel(ctx context.Context, member, id string) error {
	actor, err := actorOf(member)
	if err != nil {
		return err
	}
	status, answer, err := s.do(ctx, http.MethodPost, actor, "/v1/runs/"+url.PathEscape(id)+"/cancel", nil, "")
	if err != nil {
		return err
	}
	if status >= 300 {
		return problemOf(actor, status, answer)
	}
	return nil
}

// do sends one request to a child through the router, with the static
// token of the fleet, and returns the status and the body.
func (s *Spawner) do(ctx context.Context, method, actor, path string, body []byte, idempotencyKey string) (int, []byte, error) {
	token, err := fw.EnvSecrets{}.Get(ctx, s.tokenEnv)
	if err != nil || strings.TrimSpace(token) == "" {
		return 0, nil, fmt.Errorf("ax: the static token %s is not set, so this process cannot reach a child Task", s.tokenEnv)
	}
	var rd io.Reader = http.NoBody
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+s.router+path, rd)
	if err != nil {
		return 0, nil, fmt.Errorf("ax: build the request to %s: %w", actor, err)
	}
	req.Header.Set("ate-target-actor", actor)
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("ax: the child %s did not answer: %w", actor, err)
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return 0, nil, fmt.Errorf("ax: read the answer of the child %s: %w", actor, err)
	}
	return resp.StatusCode, answer, nil
}

// problemOf turns a refused answer of a child into an error. A 404 is
// fw.ErrRunNotFound and a 409 is fw.ErrNoPendingApproval, so the tools of
// the parent read them as they read the errors of this process.
func problemOf(actor string, status int, answer []byte) error {
	var p struct {
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal(answer, &p)
	detail := strings.TrimSpace(p.Detail)
	if detail == "" {
		detail = http.StatusText(status)
	}
	switch status {
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s: %s", fw.ErrRunNotFound, actor, detail)
	case http.StatusConflict:
		return fmt.Errorf("%w: %s: %s", fw.ErrNoPendingApproval, actor, detail)
	}
	return fmt.Errorf("ax: the child %s answered %d: %s", actor, status, detail)
}

// sleep waits for d or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
