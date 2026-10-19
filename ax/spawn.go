package ax

// This file is the ax Spawner: with spawn.driver "ax", each child of
// spawn_task runs in a child Task of its own (docs/ARCHITECTURE.md section
// 11.9). The spawner creates the Task through the ax.v1alpha1.AX gRPC
// service at ax.server with the image, the environment, the resources, and
// the workspaces of this Task, waits until the Task runs, starts the child
// run through the atenet router with the static token of the fleet and the
// person of the parent, and follows the run through the native API of the
// child. The child Task gets FW_PARENT_RUN, FW_PARENT_MEMBER, the depth
// that spawn.maxDepth leaves it in FW_SPAWN_MAX_DEPTH, and the runs that
// its share of spawn.maxDescendants leaves it in FW_SPAWN_MAX_DESCENDANTS.
// The name of a child
// Task ends with the id of its run, so the kernel records the name before
// the Task exists, and a restart of this process finds the Task again.

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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	v1alpha1 "github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func init() {
	fw.RegisterSpawner("ax", func(_ context.Context, cfg *fw.Config) (fw.Spawner, error) { return NewSpawner(cfg) })
}

// Variables that a child Task gets beyond the ones of its parent.
const (
	EnvParentRun     = "FW_PARENT_RUN"      // the id of the parent run
	EnvParentMember  = "FW_PARENT_MEMBER"   // the parent process, "ax:<atespace>/<task>"
	EnvSpawnMaxDepth = "FW_SPAWN_MAX_DEPTH" // the levels below a child that spawn.maxDepth still allows
	EnvSpawnDisabled = "FW_SPAWN_DISABLED"  // a leaf has no remaining delegation depth or runs
	// EnvSpawnMaxDescendants is the number of runs that a child can have
	// under it: the share of spawn.maxDescendants that its parent reserved
	// for it (fw.MetaMaxDescendants), less the child itself.
	EnvSpawnMaxDescendants = "FW_SPAWN_MAX_DESCENDANTS"
)

// DefaultServer is the ax API server inside the cluster.
const DefaultServer = "ax-server.ax-system.svc.cluster.local:8080"

// Limits of the spawner.
const (
	readyTimeout   = 10 * time.Minute // how long a child Task gets to run
	startTimeout   = 5 * time.Minute  // how long the first request to a child gets
	maxTaskName    = 63
	maxAnswer      = 4 << 20
	phaseRunning   = "Running"
	phaseSuspended = "Suspended"
	phaseFailed    = "Failed"
)

// droppedEnv names the variables of the parent that a child does not get:
// the ones that this file sets, under either spelling of the depth variable
// that earlier revisions used, and the fleet identity, which the child
// derives from its own Task. fw reads the depth from FW_SPAWN_MAX_DEPTH,
// so an inherited value of either name would name the remaining depth of
// the parent, not of the child, and would be a lie.
var droppedEnv = map[string]bool{
	EnvParentRun: true, EnvParentMember: true, EnvSpawnMaxDepth: true, EnvSpawnDisabled: true, EnvSpawnMaxDescendants: true,
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
var _ fw.Forker = (*Spawner)(nil)
var _ fw.Planner = (*Spawner)(nil)

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

// Spawn implements fw.Spawner. The child Task has the name that Plan
// gives req, and the child takes its run under req.ID.
func (s *Spawner) Spawn(ctx context.Context, parent *fw.Run, req fw.RunRequest) (*fw.Run, string, error) {
	task, _ := Stashed()
	if task == nil {
		return nil, "", errors.New("ax: this process is not an ax Task, so it cannot start a child Task")
	}
	atespace, name := Identity(task)
	if atespace == "" || name == "" {
		return nil, "", errors.New("ax: the Task of this process has no atespace or no name")
	}
	child, err := childName(name, req.ID)
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
	spec := s.childTask(task, atespace, child, parent, maxDescendantsOf(req.Metadata))
	if err := s.createTask(ctx, client, spec); err != nil {
		return nil, "", err
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
	if req.ID != "" && run.ID != req.ID {
		s.deleteTask(client, atespace, child)
		return nil, "", fmt.Errorf("ax: the child %s took the run as %s, not %s", actor, run.ID, req.ID)
	}
	return run, "ax:" + actor, nil
}

// Plan implements fw.Planner: it returns the member name of the child Task
// that Spawn creates for req, from the name of this Task and req.ID, and
// creates nothing.
func (s *Spawner) Plan(_ *fw.Run, req fw.RunRequest) (string, error) {
	task, _ := Stashed()
	if task == nil {
		return "", errors.New("ax: this process is not an ax Task, so it cannot start a child Task")
	}
	atespace, name := Identity(task)
	if atespace == "" || name == "" {
		return "", errors.New("ax: the Task of this process has no atespace or no name")
	}
	if _, ok := fw.IDTime(req.ID); !ok {
		return "", fmt.Errorf("ax: the child run needs an id from fw.NewID to name its Task, not %q", req.ID)
	}
	child, err := childName(name, req.ID)
	if err != nil {
		return "", err
	}
	return "ax:" + atespace + "/" + child, nil
}

// Find implements fw.Planner: after a restart of this process, it looks for
// the child run id in the child Task at member. A Task that does not exist,
// that ax deletes, or that failed holds no run that answers. A Task that
// does not run, because this process went away before it resumed the Task,
// runs again for the look, as Spawn would have made it run, so a Task
// without the run answers that it does not have it.
func (s *Spawner) Find(ctx context.Context, member, id string) (*fw.Run, error) {
	actor, err := actorOf(member)
	if err != nil {
		return nil, err
	}
	atespace, child, _ := strings.Cut(actor, "/")
	client, err := s.ax()
	if err != nil {
		return nil, err
	}
	t, err := client.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: atespace, Name: child})
	if status.Code(err) == codes.NotFound {
		return nil, fmt.Errorf("%w: the child Task %s does not exist", fw.ErrRunNotFound, actor)
	}
	if err != nil {
		return nil, fmt.Errorf("ax: read the child Task %s: %w", actor, err)
	}
	switch t.GetStatus().GetPhase() {
	case v1alpha1.PhaseTerminating:
		return nil, fmt.Errorf("%w: the child Task %s is being deleted", fw.ErrRunNotFound, actor)
	case phaseFailed:
		return nil, fmt.Errorf("%w: the child Task %s failed", fw.ErrRunNotFound, actor)
	}
	if t.GetStatus().GetPhase() != phaseRunning {
		if err := s.awaitRunning(ctx, client, atespace, child); err != nil {
			return nil, err
		}
	}
	return s.Get(ctx, member, id)
}

// Place implements fw.Forker: it creates a child Task for a branch, as
// Spawn does, and returns its member name at once. The branch has a
// workspace of its own, the one of the new Task, so two branches never see
// each other's files.
func (s *Spawner) Place(ctx context.Context, parent *fw.Run) (string, error) {
	task, _ := Stashed()
	if task == nil {
		return "", errors.New("ax: this process is not an ax Task, so it cannot start a child Task")
	}
	atespace, name := Identity(task)
	if atespace == "" || name == "" {
		return "", errors.New("ax: the Task of this process has no atespace or no name")
	}
	child, err := childName(name, "")
	if err != nil {
		return "", err
	}
	client, err := s.ax()
	if err != nil {
		return "", err
	}
	spec := s.childTask(task, atespace, child, parent, -1)
	if err := s.createTask(ctx, client, spec); err != nil {
		return "", err
	}
	s.log.Info("child Task created for a branch", "atespace", atespace, "task", child, "parent", parent.ID)
	return "ax:" + atespace + "/" + child, nil
}

// Branch implements fw.Forker: it waits until the child Task of Place runs
// and starts the branch there with the kept events, through POST
// /v1/runs/fork of the child, under the id of the request. A Task that does
// not run, or that does not take the branch, is deleted.
func (s *Spawner) Branch(ctx context.Context, member string, events []fw.Event, req fw.ForkRequest) (*fw.Run, error) {
	actor, err := actorOf(member)
	if err != nil {
		return nil, err
	}
	atespace, child, _ := strings.Cut(actor, "/")
	task, _ := Stashed()
	if task == nil {
		return nil, errors.New("ax: this process is not an ax Task, so it cannot start a child Task")
	}
	_, name := Identity(task)
	client, err := s.ax()
	if err != nil {
		return nil, err
	}
	if err := s.awaitRunning(ctx, client, atespace, child); err != nil {
		s.deleteTask(client, atespace, child)
		return nil, err
	}
	if req.Metadata == nil {
		req.Metadata = map[string]string{}
	}
	req.Metadata[fw.MetaParentMember] = "ax:" + atespace + "/" + name
	body, err := json.Marshal(map[string]any{"id": req.ID, "events": events, "input": req.Input, "model": req.Model, "metadata": req.Metadata})
	if err != nil {
		s.deleteTask(client, atespace, child)
		return nil, fmt.Errorf("ax: encode the branch: %w", err)
	}
	run, err := s.startOn(ctx, actor, "/v1/runs/fork", body, "fork:"+req.ID)
	if err != nil {
		s.deleteTask(client, atespace, child)
		return nil, err
	}
	if run.ID != req.ID {
		s.deleteTask(client, atespace, child)
		return nil, fmt.Errorf("ax: the child %s took the branch as the run %s, not %s", actor, run.ID, req.ID)
	}
	return run, nil
}

// Abandon implements fw.Forker and fw.Planner: it deletes the child Task of
// a branch or a child run that never reached it, after a restart of this
// process. A Task that does not exist is gone already.
func (s *Spawner) Abandon(ctx context.Context, member string) error {
	actor, err := actorOf(member)
	if err != nil {
		return err
	}
	atespace, child, _ := strings.Cut(actor, "/")
	client, err := s.ax()
	if err != nil {
		return err
	}
	_, err = client.DeleteTask(ctx, &v1alpha1.DeleteTaskRequest{Atespace: atespace, Name: child})
	if err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("ax: delete the child Task %s: %w", actor, err)
	}
	s.log.Info("child Task of a lost child deleted", "atespace", atespace, "task", child)
	return nil
}

// egressOf converts the egress of a Task manifest to the API type. A Task
// without egress gives nil, and the child then has no network, as the
// parent has none.
func egressOf(e *EgressSpec) *v1alpha1.EgressSpec {
	if e == nil {
		return nil
	}
	out := &v1alpha1.EgressSpec{}
	hosts := func(h *EgressHosts) *v1alpha1.EgressHosts {
		return &v1alpha1.EgressHosts{Hostnames: append([]string(nil), h.Hostnames...), Ports: append([]int32(nil), h.Ports...)}
	}
	for _, r := range e.Rules {
		rule := &v1alpha1.EgressRule{}
		switch {
		case r.HTTP != nil:
			rule.Http = hosts(r.HTTP)
		case r.HTTPS != nil:
			rule.Https = hosts(r.HTTPS)
		case r.TLS != nil:
			rule.Tls = hosts(r.TLS)
		default:
			continue
		}
		out.Rules = append(out.Rules, rule)
	}
	return out
}

// childName makes the name of a child Task from the name of its parent and
// the id of the child run, within the length of a DNS label. The id is a
// ULID, which has only digits and letters, so the name is the same for
// the same id and differs for another id. Without such an id, as for a
// branch, the suffix is random.
func childName(parent, id string) (string, error) {
	suffix := strings.ToLower(id)
	if _, ok := fw.IDTime(id); !ok {
		b := make([]byte, 4)
		if _, err := rand.Read(b); err != nil {
			return "", fmt.Errorf("ax: random name: %w", err)
		}
		suffix = fmt.Sprintf("%x", b)
	}
	parent = strings.ToLower(parent)
	if n := maxTaskName - len(suffix) - 1; len(parent) > n {
		parent = strings.TrimRight(parent[:n], "-")
	}
	return parent + "-" + suffix, nil
}

// maxDescendantsOf reads fw.MetaMaxDescendants from the metadata of a child
// request, and returns -1 when the kernel set none.
func maxDescendantsOf(meta map[string]string) int {
	v, ok := meta[fw.MetaMaxDescendants]
	if !ok {
		return -1
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return -1
	}
	return n
}

// childTask is the Task of a child: the parent Task with a new name, no
// goals, no debug services, and the variables of a child. maxDesc is the
// number of runs that the child can have under it, or -1 when its parent
// set no share, as for a branch, which is a tree of its own.
func (s *Spawner) childTask(task *Task, atespace, name string, parent *fw.Run, maxDesc int) *v1alpha1.Task {
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
	// The process of the child holds its tree to the share that its parent
	// reserved, as the kernel there does for the run from its metadata. Zero
	// means unlimited, so a share without room for more runs is the flag.
	// Without a share, the child takes the limit of this process, and an
	// explicitly unlimited tree stays unlimited.
	switch {
	case maxDesc == 0:
		disabled = true
	case maxDesc > 0:
		spec.Env = append(spec.Env, &v1alpha1.EnvVar{Name: EnvSpawnMaxDescendants, Value: strconv.Itoa(maxDesc)})
	default:
		spec.Env = append(spec.Env, &v1alpha1.EnvVar{Name: EnvSpawnMaxDescendants, Value: strconv.Itoa(s.cfg.Spawn.MaxDescendants)})
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
	// The child reaches what the parent reaches and nothing more: ax writes
	// the egress policy of the child actor from this before the child runs.
	spec.Egress = egressOf(task.Spec.Egress)
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

// createTask creates a child Task and resumes it. ax creates every Task
// suspended until a caller resumes it, so a Task that nobody resumes never
// runs. A create that fails can still have made the Task, for example when
// the call ends while the server works on it, so the Task is deleted, with
// a context of its own. A Task of that name that exists is not one that
// this call made, and it stays.
func (s *Spawner) createTask(ctx context.Context, client v1alpha1.AXClient, spec *v1alpha1.Task) error {
	atespace, name := spec.GetMetadata().GetAtespace(), spec.GetMetadata().GetName()
	if _, err := client.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: spec}); err != nil {
		if status.Code(err) != codes.AlreadyExists {
			s.deleteTask(client, atespace, name)
		}
		return fmt.Errorf("ax: create the child Task %s/%s: %w", atespace, name, err)
	}
	if _, err := client.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Atespace: atespace, Name: name}); err != nil {
		s.deleteTask(client, atespace, name)
		return fmt.Errorf("ax: resume the child Task %s/%s: %w", atespace, name, err)
	}
	return nil
}

// awaitRunning waits until the Task runs. A Task that ax reports as
// suspended, because its reconciler ran before the resume took hold, is
// resumed again.
func (s *Spawner) awaitRunning(ctx context.Context, client v1alpha1.AXClient, atespace, name string) error {
	deadline := time.Now().Add(readyTimeout)
	for {
		t, err := client.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: atespace, Name: name})
		if err != nil {
			return fmt.Errorf("ax: read the child Task %s/%s: %w", atespace, name, err)
		}
		switch t.GetStatus().GetPhase() {
		case phaseRunning:
			return nil
		case phaseSuspended:
			if _, err := client.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Atespace: atespace, Name: name}); err != nil {
				s.log.Warn("resume the child Task", "atespace", atespace, "task", name, "error", err)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ax: the child Task %s/%s is %q after %s", atespace, name, t.GetStatus().GetPhase(), readyTimeout)
		}
		if err := sleep(ctx, s.readyPoll); err != nil {
			return err
		}
	}
}

// deleteTask removes a child Task that never got its run. The connection to
// the ax server can be stale after a suspend of this Task, so it tries
// three times. A Task that does not exist is gone already.
func (s *Spawner) deleteTask(client v1alpha1.AXClient, atespace, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		_, err = client.DeleteTask(ctx, &v1alpha1.DeleteTaskRequest{Atespace: atespace, Name: name})
		if err == nil || status.Code(err) == codes.NotFound {
			return
		}
		if sleep(ctx, 2*time.Second) != nil {
			break
		}
	}
	s.log.Warn("delete the child Task", "atespace", atespace, "task", name, "error", err)
}

// runBody is the body of POST /v1/runs of the child, in the shape that the
// native API takes.
type runBody struct {
	ID            string            `json:"id,omitempty"`
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
	body := runBody{ID: req.ID, Agent: req.Agent, Input: req.Input, Instructions: req.Instructions, Budget: budgetOf(req.Budget),
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
	return s.startOn(ctx, actor, "/v1/runs", body, startKey(parent.ID, actor))
}

// startKey is the idempotency key of the start of a child run. The name of
// the child Task in actor comes from the id of the child run, so the key
// is the same for every start of one child and differs for another child.
func startKey(parent, actor string) string { return "spawn:" + parent + ":" + actor }

// startOn posts a run or a branch to the child and returns the run that the
// child answers. The child boots after its Task runs, so the request is
// sent again on a gateway error, with the same idempotency key.
func (s *Spawner) startOn(ctx context.Context, actor, path string, body []byte, key string) (*fw.Run, error) {
	deadline := time.Now().Add(startTimeout)
	wait := time.Second
	for {
		status, answer, err := s.do(ctx, http.MethodPost, actor, path, body, key)
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
