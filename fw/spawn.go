package fw

// This file is the Spawner: the plug behind spawn_task that starts a child
// run in another process (docs/ARCHITECTURE.md section 11.9). Without one,
// a child runs in this process. With one, the kernel keeps the place of
// each remote child in the ledger of its parent, so Get, WaitFor, Decide,
// Cancel, and Children reach the child after a restart too, and the
// surfaces show it with Member set.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// A Spawner runs the child runs of spawn_task in other processes. The ax
// driver creates a child Task for each one.
//
// Spawn starts a child of the parent run with req, and returns the child as
// the other process reports it and the name of that process, which
// Run.Member carries from then on. The kernel fills req before the call:
// ID is the id of the child, which the kernel chose and recorded in the
// ledger of the parent before the call, so the other process must start the
// child under it; IdempotencyKey is empty, because the kernel keeps the key
// of the start and ID names the child; Principal and PrincipalKind name the
// person of the parent, so the child acts as that person; Budget is the
// budget of the parent, capped, with Deadline the working time that the
// parent has left; the metadata key MetaParent holds the id of the parent;
// and Origin is OriginTool. The spawner adds MetaParentMember, the name of
// this process, when it knows it. On an error, Spawn leaves no process
// behind; the kernel abandons the process that a Planner named as well,
// because a call that ends early can leave one. Get, WaitFor, Decide, and
// Cancel reach a child at member, with the meaning of the Agent methods of
// the same names. They return ErrRunNotFound for a child that the process
// does not know.
type Spawner interface {
	Spawn(ctx context.Context, parent *Run, req RunRequest) (*Run, string, error)
	Get(ctx context.Context, member, id string) (*Run, error)
	WaitFor(ctx context.Context, member, id string, done func(*Run) bool) (*Run, error)
	Decide(ctx context.Context, member, id string, d ApprovalDecision) error
	Cancel(ctx context.Context, member, id string) error
}

// Forker is a Spawner that can run a branch of a run in another process
// (fork.go), in two steps, because the other process takes a while to run
// and the caller of Fork does not wait for it.
//
// Place chooses or creates the process for a branch of the parent run and
// returns its member name, as Spawn does. It returns as soon as the process
// exists, for example once the child Task is created. The kernel then
// records the branch as a child of the source under the id of the request
// and answers the caller.
//
// Branch sends the kept events to that member, with the fw.run.queued event
// already adjusted for the branch, and returns the branch as the other
// process reports it, under req.ID. It returns when the process has the
// branch or when it cannot have it. On an error it leaves no process
// behind. A canceled ctx ends the start.
//
// Abandon removes the process that Place created, for a branch that never
// reached it: the kernel calls it after a restart for a branch whose start
// was open when the process went away.
type Forker interface {
	Place(ctx context.Context, parent *Run) (string, error)
	Branch(ctx context.Context, member string, events []Event, req ForkRequest) (*Run, error)
	Abandon(ctx context.Context, member string) error
}

// Planner is a Spawner that names the process of a child before Spawn
// creates it, and that finds the child again after a restart. The kernel
// records each child in the ledger of the parent with its id and that
// name before Spawn runs, so after a restart at any point of the start the
// parent knows every process that Spawn created or might have created. A
// Spawner that is not a Planner gets the same record without the name,
// and after such a restart the kernel cannot find the child.
//
// Plan returns the member name that Spawn will return for req, and
// creates nothing. The name depends on req.ID, so Plan and Spawn agree.
//
// Find returns the child run id at member, after a restart of this
// process. It returns ErrRunNotFound when no process at member has the
// run, and another error when it cannot tell, for example when the
// platform does not answer. The kernel then asks again later, and removes
// nothing.
//
// Abandon removes the process at member, for a child that never got its
// run. It returns nil when the process does not exist. A Forker has the
// same method, so one implementation serves both.
type Planner interface {
	Plan(parent *Run, req RunRequest) (string, error)
	Find(ctx context.Context, member, id string) (*Run, error)
	Abandon(ctx context.Context, member string) error
}

// SpawnerCtor builds a Spawner from the configuration.
type SpawnerCtor func(ctx context.Context, cfg *Config) (Spawner, error)

var spawners = struct {
	mu sync.RWMutex
	m  map[string]SpawnerCtor
}{m: map[string]SpawnerCtor{}}

// RegisterSpawner registers a spawner driver by name, for spawn.driver. A
// driver package calls it in init.
func RegisterSpawner(name string, c SpawnerCtor) {
	spawners.mu.Lock()
	defer spawners.mu.Unlock()
	spawners.m[name] = c
}

// newSpawner builds the spawner that spawn.driver names, or nil for the
// children in this process.
func newSpawner(ctx context.Context, cfg *Config) (Spawner, error) {
	name := cfg.Spawn.Driver
	if name == "" {
		return nil, nil
	}
	spawners.mu.RLock()
	c, ok := spawners.m[name]
	spawners.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("fw: unknown spawn.driver %q", name)
	}
	return c(ctx, cfg)
}

// Metadata keys of a child run in another process.
const (
	// MetaParent is the id of the parent run, which lives in the process
	// that MetaParentMember names.
	MetaParent = "fw.parent"
	// MetaParentMember is the name of the process that holds the parent,
	// such as "ax:<atespace>/<task>".
	MetaParentMember = "fw.parentMember"
)

// RunChildData is the data of EvRunChild, which the kernel appends to a
// parent run when a spawner runs a child of it in another process. Each
// child is recorded twice. A child of spawn_task is recorded before the
// spawner creates its process, without Started and with the facts of the
// child, and again once its run started, with Started. A branch is
// recorded when its process is placed, without Started and with the facts
// of the branch, and again once the process took it, with Started. After a
// restart, a branch that was placed but never started is recorded as
// failed from those facts, and its process is abandoned. A child of
// spawn_task without a start is looked for at its member: a child that
// exists is recorded as started, and one that does not is recorded as
// failed, and its process is abandoned.
type RunChildData struct {
	Child   string `json:"child"`
	Member  string `json:"member"` // empty before the start when the spawner is not a Planner
	Started bool   `json:"started,omitempty"`

	// The facts of a child whose start is open: the person of the child and
	// its metadata. The metadata of a branch carries MetaForkOf, and the
	// metadata of a child of spawn_task carries MetaParent.
	Principal     string            `json:"principal,omitempty"`
	PrincipalKind string            `json:"principalKind,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`

	// More facts of a child of spawn_task: its profile name, its task, and
	// the idempotency key of its start, with which a retry of the same start
	// finds the child.
	Agent string `json:"agent,omitempty"`
	Input string `json:"input,omitempty"`
	Key   string `json:"key,omitempty"`
}

// placed reports whether the event records a branch that its process has
// not taken yet. An event of an older kernel has no facts, so it counts as
// started.
func (d RunChildData) placed() bool { return !d.Started && d.Metadata[MetaForkOf] != "" }

// spawning reports whether the event records a child of spawn_task whose
// start is open: the record that the kernel appends before the spawner
// creates the process of the child.
func (d RunChildData) spawning() bool {
	return !d.Started && d.Metadata[MetaForkOf] == "" && d.Metadata[MetaParent] != ""
}

// queued returns the queued data of a child of spawn_task as the parent
// knows it before the other process has it, from the facts of the event,
// with the parent and the depth of the child.
func (d RunChildData) queued(parent string, depth int) RunQueuedData {
	agent := d.Agent
	if agent == "" {
		agent = "default"
	}
	return RunQueuedData{Agent: agent, Input: d.Input, Origin: OriginTool, Principal: d.Principal, PrincipalKind: d.PrincipalKind,
		Metadata: d.Metadata, Parent: parent, Depth: depth}
}

// spawnKey returns the idempotency key of the start of a child: the key of
// the request and a digest of what the child is asked to do. A start that
// reuses a key for another task, as a provider that repeats the id of a
// tool call gives, therefore begins a new child. The digest leaves out
// what the kernel fills in, which differs between two starts of one child.
func spawnKey(req RunRequest) string {
	if req.IdempotencyKey == "" {
		return ""
	}
	meta := map[string]string{}
	for k, v := range req.Metadata {
		if k != MetaParent && k != MetaParentMember {
			meta[k] = v
		}
	}
	b, err := json.Marshal(struct {
		Agent, Input, Instructions string
		Profile                    *AgentProfile
		Budget                     *Budget
		Metadata                   map[string]string
		Files                      []Block
	}{req.Agent, req.Input, req.Instructions, req.Profile, req.Budget, meta, req.Files})
	if err != nil {
		return req.IdempotencyKey
	}
	sum := sha256.Sum256(b)
	return req.IdempotencyKey + "#" + hex.EncodeToString(sum[:8])
}

// inheritBatch gives a child the batch of its parent when the request
// names none.
func inheritBatch(meta map[string]string, parent *Run) map[string]string {
	if parent == nil {
		return meta
	}
	batch := parent.Metadata[MetaBatch]
	if batch == "" || meta[MetaBatch] != "" {
		return meta
	}
	out := make(map[string]string, len(meta)+1)
	for k, v := range meta {
		out[k] = v
	}
	out[MetaBatch] = batch
	return out
}

// childRef is where a remote child lives. A child that its process does
// not have yet carries its start. key is the idempotency key of the start
// of a child of spawn_task.
type childRef struct {
	parent string
	member string
	depth  int
	start  *branchStart
	key    string
}

// branchStart is the start of a branch or a child in another process
// (fork.go, spawn.go). The kernel knows the child before that process has
// it, and answers for it from run until then. A start with find set is
// the look for a child of spawn_task after a restart: Cancel and Decide
// wait for its end instead of ending it, because the child can exist.
type branchStart struct {
	done   chan struct{}      // closed when the start ends, well or not
	cancel context.CancelFunc // ends the start
	run    *Run               // the child as this process knows it until the start ends
	find   bool               // the start looks for a child after a restart
}

// starting returns the branch as this process knows it while its start is
// still open, and false after the start ended.
func (ref childRef) starting() (*Run, bool) {
	if ref.start == nil {
		return nil, false
	}
	select {
	case <-ref.start.done:
		return nil, false
	default:
		r := cloneRun(ref.start.run)
		return r, true
	}
}

// NewRunRequest builds a request from an input and options.
func NewRunRequest(input string, opts ...RunOption) RunRequest {
	req := RunRequest{Input: input}
	for _, o := range opts {
		o(&req)
	}
	return req
}

// Spawner returns the spawner of the agent, or nil when children run in
// this process.
func (a *Agent) Spawner() Spawner { return a.spawner }

// SpawnChild starts a child of the run parent with req, in this process or
// through the Spawner. It enforces spawn.maxDepth and spawn.maxChildren for
// both. A remote child acts as the person of the parent, gets the budget
// of the parent as its cap, and is recorded in the ledger of the parent
// before the spawner creates its process, so a restart at any point of the
// start finds it (see Planner). A request with an IdempotencyKey that an
// earlier start of a child of the same parent used returns the child of
// that start, in this process or in another one. spawn_task keys each
// start by its tool call, so a rerun of the call after a restart reaches
// the child of the first attempt instead of starting another.
func (a *Agent) SpawnChild(ctx context.Context, parent string, req RunRequest) (*Run, error) {
	if a.cfg.Spawn.Disabled {
		return nil, fmt.Errorf("%w: spawning is disabled", ErrSpawnLimit)
	}
	req.IdempotencyKey = spawnKey(req)
	if a.spawner == nil {
		req.Parent = parent
		return a.start(ctx, req)
	}
	k := a.k
	if k.isDraining() {
		return nil, ErrClosed
	}
	p, err := k.lookup(parent)
	if err != nil {
		return nil, fmt.Errorf("%w: parent run %s not found", ErrInvalidRequest, parent)
	}
	key := req.IdempotencyKey
	if key != "" {
		if r, ok, err := a.spawnedBefore(ctx, parent, key); ok {
			return r, err
		}
	}
	q := p.queuedData()
	// Rules travel as data. A custom or external policy needs an explicit
	// remote enforcement protocol; sharing an image does not prove that
	// the receiving process has the same policy or control-plane binding.
	if !a.rulesPolicy {
		return nil, fmt.Errorf("%w: the policy of this agent does not travel to a child in another process", ErrInvalidRequest)
	}
	depth := p.depth + 1
	if max := a.cfg.Spawn.MaxDepth; max > 0 && depth > max {
		return nil, fmt.Errorf("%w: a child at depth %d is deeper than spawn.maxDepth %d", ErrSpawnLimit, depth, max)
	}
	req.Parent = ""
	req.Principal, req.PrincipalKind = q.Principal, q.PrincipalKind
	req.Origin = OriginTool
	if req.Metadata == nil {
		req.Metadata = map[string]string{}
	}
	req.Metadata[MetaParent] = parent
	req.Metadata = inheritBatch(req.Metadata, p.snapshot())
	var b Budget
	if req.Budget != nil {
		b = *req.Budget
	}
	b = b.Cap(a.budgetOf(p))
	if left, ok := a.timeLeft(ctx, p, a.now()); ok && (b.Deadline <= 0 || left < b.Deadline) {
		if left < time.Second {
			left = time.Second
		}
		b.Deadline = left
	}
	req.Budget = &b
	// The child gets at most the tools and the rules of the parent: the
	// other process cannot compare the two, so this process composes them
	// into the inline profile of the child before the spawner travels it.
	prof, err := a.childProfile(ctx, p, req, depth)
	if err != nil {
		return nil, err
	}
	req.Profile = prof
	// The kernel names the child, and a planner names its process, before
	// anything exists, so the record of the child comes first. The key of
	// the start stays here: a new start after a lost child must not reach
	// the run of the lost child in a process that holds both.
	req.ID, req.IdempotencyKey = NewID(), ""
	member := ""
	planner, _ := a.spawner.(Planner)
	if planner != nil {
		if member, err = planner.Plan(p.snapshot(), req); err != nil {
			return nil, err
		}
		if member == "" {
			return nil, errors.New("fw: the spawner planned the child nowhere")
		}
	}
	intent := RunChildData{Child: req.ID, Member: member, Principal: req.Principal, PrincipalKind: req.PrincipalKind,
		Metadata: req.Metadata, Agent: req.Agent, Input: req.Input, Key: key}
	cq := intent.queued(parent, depth)
	placeholder, err := a.placeholder(req.ID, cq, a.now())
	if err != nil {
		return nil, err
	}
	// Until the spawner returns, the parent answers for the child, a start
	// under the same key waits for this one, and Cancel ends the start.
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	start := &branchStart{done: make(chan struct{}), cancel: cancel, run: placeholder}
	ref := childRef{parent: parent, member: member, depth: depth, start: start, key: key}
	a.decorateChild(placeholder, ref)
	k.mu.Lock()
	for key != "" && a.keyedLocked(parent, key) {
		k.mu.Unlock()
		if r, ok, err := a.spawnedBefore(ctx, parent, key); ok {
			return r, err
		}
		k.mu.Lock()
	}
	if max := a.cfg.Spawn.MaxChildren; max > 0 && k.kids[parent] >= max {
		k.mu.Unlock()
		return nil, fmt.Errorf("%w: run %s already started spawn.maxChildren (%d) child runs", ErrSpawnLimit, parent, max)
	}
	k.kids[parent]++
	k.remote[req.ID] = ref
	k.mu.Unlock()
	// The record must be on disk before the process exists: a restart then
	// knows the child, wherever the start stopped. The file ledger syncs
	// every fw.run.child event.
	if _, err := a.emit(nil, p, EvRunChild, intent); err != nil {
		k.mu.Lock()
		delete(k.remote, req.ID)
		k.kids[parent]--
		close(start.done)
		k.mu.Unlock()
		return nil, fmt.Errorf("fw: record the child %s before its start: %w", req.ID, err)
	}
	child, got, err := a.spawner.Spawn(sctx, p.snapshot(), req)
	if err == nil && (child == nil || child.ID != req.ID || got == "" || (member != "" && got != member)) {
		if child != nil && got != "" && got != member {
			_ = a.dropChild(planner, got, child.ID)
		}
		err = fmt.Errorf("fw: the spawner started another child than %s", req.ID)
	}
	if err != nil {
		where := member
		if where == "" {
			where = "another process"
		}
		outcome, reason := "failed", fmt.Sprintf("the child did not start in %s: %v", where, err)
		if sctx.Err() != nil {
			outcome, reason = "canceled", "the start of the child in "+where+" was canceled"
		}
		a.log.Warn("child did not start in another process", "run", parent, "child", req.ID, "member", member, "outcome", outcome, "error", err)
		// A cut start can leave the process that the planner named, for
		// example a Task that the server created while the call ended.
		var aerr error
		if planner != nil {
			aerr = a.abandonNow(planner, req.ID, member)
		}
		a.endStart(p, intent, start, outcome, reason, aerr)
		return nil, err
	}
	ref.member = got
	if err := a.recordStart(p, RunChildData{Child: req.ID, Member: got, Started: true}); err != nil {
		// A parent that cannot record the start of its child cannot answer
		// for the child later. The child ends here, so it does not run for
		// nobody; the record of the child stays on disk, so a restart finds
		// whatever the end leaves.
		aerr := a.dropChild(planner, got, req.ID)
		intent.Member = got
		a.endStart(p, intent, start, "failed", "the parent did not record the start of the child in "+got+": "+err.Error(), aerr)
		return nil, fmt.Errorf("fw: record the start of the child %s in %s: %w", req.ID, got, err)
	}
	if sctx.Err() != nil {
		// Cancel ended the start after the child took its run. The child is
		// recorded, and the cancel goes on to it.
		cctx, cancel := context.WithTimeout(k.ctx, time.Minute)
		if err := a.spawner.Cancel(cctx, got, req.ID); err != nil {
			a.log.Warn("cancel a child whose start was canceled", "run", parent, "child", req.ID, "member", got, "error", err)
		}
		cancel()
	}
	k.mu.Lock()
	k.remote[req.ID] = ref
	close(start.done)
	k.mu.Unlock()
	a.log.Info("child started in another process", "run", parent, "child", req.ID, "member", got)
	a.decorateChild(child, ref)
	return child, nil
}

// endStart ends a start that did not give a child: it records the child
// here as a run with the outcome and the reason, once the planner abandoned
// the process of the child. When the abandon failed (aerr), the process can
// still exist, so the child stays a child of the parent and the look of a
// restart takes over at once: it removes the process before it records the
// end, however many tries that takes.
func (a *Agent) endStart(p *runState, d RunChildData, start *branchStart, outcome, reason string, aerr error) {
	k := a.k
	if aerr != nil && d.Member != "" {
		a.log.Warn("the process of a child that did not start stays for a look", "run", p.id, "child", d.Child, "member", d.Member, "error", aerr)
		a.findChild(p, d, outcome, reason)
	} else {
		a.recordBranchEnd(d.Child, p.id, d.queued(p.id, p.depth+1), outcome, reason)
	}
	k.mu.Lock()
	close(start.done)
	k.mu.Unlock()
}

// startRecordAttempts is how many times the kernel appends the record of a
// start that the ledger refused. A failed fsync of the file ledger keeps
// its debt, and a later append can pay it.
const startRecordAttempts = 3

// recordStart appends the fw.run.child event that marks the start of a
// child of rs, and tries again when the ledger refuses it.
func (a *Agent) recordStart(rs *runState, d RunChildData) error {
	var err error
	for attempt := 1; attempt <= startRecordAttempts; attempt++ {
		if _, err = a.emit(nil, rs, EvRunChild, d); err == nil {
			return nil
		}
		a.log.Warn("record the start of a child in another process", "run", rs.id, "child", d.Child, "member", d.Member, "attempt", attempt, "error", err)
		if attempt < startRecordAttempts {
			time.Sleep(time.Duration(attempt) * 100 * time.Millisecond)
		}
	}
	return err
}

// dropChild ends a child that this process cannot keep: it cancels the run
// at member and abandons the process, when the spawner is a Planner. It
// returns the error of the abandon.
func (a *Agent) dropChild(planner Planner, member, id string) error {
	ctx, cancel := context.WithTimeout(a.k.ctx, time.Minute)
	defer cancel()
	if err := a.spawner.Cancel(ctx, member, id); err != nil {
		a.log.Warn("cancel a child that this process cannot keep", "child", id, "member", member, "error", err)
	}
	if planner == nil {
		return nil
	}
	return a.abandonNow(planner, id, member)
}

// abandonNow abandons the process at member of the child id, for at most a
// minute and not past the life of the kernel, so no call to the spawner
// outlives Close. It returns the error of the abandon.
func (a *Agent) abandonNow(planner Planner, id, member string) error {
	if member == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(a.k.ctx, time.Minute)
	defer cancel()
	err := planner.Abandon(ctx, member)
	if err != nil {
		a.log.Warn("abandon the process of a child", "child", id, "member", member, "error", err)
	}
	return err
}

// abandoner removes the process of a child that never got its run. Planner
// and Forker have the method.
type abandoner interface {
	Abandon(ctx context.Context, member string) error
}

// abandon removes the process at member of a child or a branch that never
// got its run, and logs a failure.
func (a *Agent) abandon(ctx context.Context, ab abandoner, id, member string) {
	if err := ab.Abandon(ctx, member); err != nil {
		a.log.Warn("abandon the process of a lost child", "child", id, "member", member, "error", err)
	}
}

// placeholder returns a child as this process knows it before the other
// process has it: a queued run with the queued data q, created at at.
func (a *Agent) placeholder(id string, q RunQueuedData, at time.Time) (*Run, error) {
	qe, err := NewEvent(id, EvRunQueued, q)
	if err != nil {
		return nil, err
	}
	qe.Time = at.UTC()
	return ProjectRun([]Event{qe}, a.prices), nil
}

// keyedLocked reports whether a start under key began a remote child of
// parent that this process still knows. The caller holds k.mu.
func (a *Agent) keyedLocked(parent, key string) bool {
	_, _, ok := a.keyedChildLocked(parent, key)
	return ok
}

// keyedChildLocked returns the remote child of parent that a start under
// key began. The caller holds k.mu.
func (a *Agent) keyedChildLocked(parent, key string) (string, childRef, bool) {
	for id, ref := range a.k.remote {
		if ref.parent == parent && ref.key == key {
			return id, ref, true
		}
	}
	return "", childRef{}, false
}

// spawnedBefore returns the remote child of parent that an earlier start
// under key began, and true when there is one. It waits first for a start
// of that child that is still open, here or in the look after a restart.
// A child whose start failed is no longer a child of the parent, so a new
// start under the key begins a new child.
func (a *Agent) spawnedBefore(ctx context.Context, parent, key string) (*Run, bool, error) {
	for {
		a.k.mu.Lock()
		id, ref, ok := a.keyedChildLocked(parent, key)
		a.k.mu.Unlock()
		if !ok {
			return nil, false, nil
		}
		if _, open := ref.starting(); open {
			select {
			case <-ref.start.done:
				continue
			case <-ctx.Done():
				return nil, true, ctx.Err()
			}
		}
		r, err := a.spawner.Get(ctx, ref.member, id)
		if err != nil {
			return nil, true, fmt.Errorf("fw: the child %s of an earlier start under the same key runs in %s and does not answer: %w", id, ref.member, err)
		}
		a.decorateChild(r, ref)
		a.log.Info("child of an earlier start taken again", "run", parent, "child", id, "member", ref.member)
		return r, true, nil
	}
}

// findChild looks for a child of spawn_task whose start was open when this
// process went away, or whose start failed and left a process that did
// not go. The Planner looks for the child at its member in the background
// (see find). A child that exists is recorded as started. A child that no
// process has is recorded here with the outcome and the reason why, with
// the answer of the planner, once its process is abandoned. While the
// planner cannot tell, the child stays a child of the parent and the look
// goes on, up to lookLimit. Until the look ends, the parent answers Get for
// the child, and WaitFor, Cancel, Decide, and a start under the same key
// wait for the end.
func (a *Agent) findChild(parent *runState, d RunChildData, outcome, why string) {
	k := a.k
	q := d.queued(parent.id, parent.depth+1)
	planner, ok := a.spawner.(Planner)
	switch {
	case a.spawner != nil && d.Member == "":
		a.log.Warn("child lost in a restart", "run", parent.id, "child", d.Child)
		a.recordBranchEnd(d.Child, parent.id, q, "failed", "this process restarted while the child started, and the spawner named no process for it, so the child cannot be looked for")
		return
	case !ok:
		// The child can be at its member, so it stays a child of the
		// parent, which reaches it through the spawner.
		a.log.Warn("a child in another process cannot be looked for", "run", parent.id, "child", d.Child, "member", d.Member)
		return
	}
	at, ok := IDTime(d.Child)
	if !ok {
		at = a.now()
	}
	placeholder, err := a.placeholder(d.Child, q, at)
	if err != nil {
		a.log.Error("look for a child after a restart", "run", parent.id, "child", d.Child, "error", err)
		return
	}
	ctx, cancel := context.WithCancel(k.ctx)
	start := &branchStart{done: make(chan struct{}), cancel: cancel, run: placeholder, find: true}
	ref := childRef{parent: parent.id, member: d.Member, depth: parent.depth + 1, start: start, key: d.Key}
	a.decorateChild(placeholder, ref)
	k.mu.Lock()
	k.remote[d.Child] = ref
	k.mu.Unlock()
	go a.find(ctx, planner, ref, d.Child, q, outcome, why)
}

// lookLimit bounds the look for a child, counted from the record of the
// child, which the id of the child dates. Past it, a look that cannot tell
// gives up, so a lasting error, such as a token that the router refuses,
// does not keep the look going forever, and the bound holds over restarts.
const lookLimit = 15 * time.Minute

// find asks the planner for a child until it tells, or until this process
// shuts down. Each look that cannot tell waits longer, up to a minute. A
// child that no process has is abandoned first and recorded as failed
// after, so a restart between the two looks again and finds nothing to
// remove; a failed abandon is tried again. Past lookLimit, the look gives
// up: it abandons the process, because a child that the parent cannot
// reach can never report to it, and records the child as failed with the
// reason, so a person sees what happened. Every call to the spawner ends
// before the look ends, so none outlives Close.
func (a *Agent) find(ctx context.Context, planner Planner, ref childRef, id string, q RunQueuedData, outcome, why string) {
	k := a.k
	defer ref.start.cancel()
	done := func() {
		k.mu.Lock()
		close(ref.start.done)
		k.mu.Unlock()
	}
	at, ok := IDTime(id)
	if !ok {
		at = a.now()
	}
	deadline := at.Add(lookLimit)
	wait := time.Second
	for {
		_, err := planner.Find(ctx, ref.member, id)
		if ctx.Err() != nil {
			// This process shuts down. The record of the child stays as it
			// is, and the next start looks again.
			done()
			return
		}
		if err == nil {
			a.adoptChild(ctx, ref, id)
			done()
			return
		}
		missing := errors.Is(err, ErrRunNotFound)
		end, reason := outcome, why+": "+err.Error()
		if !missing {
			end, reason = "failed", "this process could not reach "+ref.member+" to look for the child: "+err.Error()
		}
		late := !a.now().Before(deadline)
		if missing || late {
			aerr := a.abandonNow(planner, id, ref.member)
			if ctx.Err() != nil {
				done()
				return
			}
			if aerr == nil || late {
				if aerr != nil {
					reason += "; its process could not be removed: " + aerr.Error()
				}
				a.log.Warn("child lost in a restart", "run", ref.parent, "child", id, "member", ref.member, "reason", reason)
				a.recordBranchEnd(id, ref.parent, q, end, reason)
				done()
				return
			}
			err = aerr
		}
		a.log.Warn("look for a child after a restart", "run", ref.parent, "child", id, "member", ref.member, "retryIn", wait, "error", err)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
		}
		wait = min(2*wait, time.Minute)
	}
}

// adoptChild records a child that the look after a restart found as
// started, so the next restart does not look again. A parent that ended
// cancels the child: the call that started the child never returned, so
// nothing reads the result of the child. The process of the child stays,
// as for any child that ended, with the ledger of its run.
func (a *Agent) adoptChild(ctx context.Context, ref childRef, id string) {
	rs, err := a.k.lookup(ref.parent)
	if err != nil {
		return
	}
	if err := a.recordStart(rs, RunChildData{Child: id, Member: ref.member, Started: true}); err != nil {
		a.log.Error("record a child found after a restart", "run", ref.parent, "child", id, "member", ref.member, "error", err)
	}
	a.log.Info("child found after a restart", "run", ref.parent, "child", id, "member", ref.member)
	if rs.snapshot().State.Terminal() {
		cctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		if err := a.spawner.Cancel(cctx, ref.member, id); err != nil {
			a.log.Warn("cancel a child of a run that ended", "run", ref.parent, "child", id, "member", ref.member, "error", err)
		}
	}
}

// childProfile returns the inline profile that travels with a child run in
// another process, where the child cannot walk the tree of the caller.
// Rules retain their independent defaults. Tool names are a snapshot of
// the effective intersection, including opt-in restrictions, even for an
// unrestricted parent. New or disconnected tools are not granted later.
func (a *Agent) childProfile(ctx context.Context, parent *runState, req RunRequest, depth int) (*AgentProfile, error) {
	name := req.Agent
	if name == "" {
		name = "default"
	}
	if _, ok := a.profiles()[name]; !ok && name != "default" {
		return nil, fmt.Errorf("%w: %s", ErrAgentNotFound, name)
	}
	effective := a.profileNamed(name)
	out := AgentProfile{}
	if req.Profile != nil {
		effective = effective.Overlay(*req.Profile)
		out = *req.Profile
	}
	ancestors := a.lineage(parent)
	merged := a.cfg.Policy.Rules
	var chain [][]string
	for _, anc := range ancestors {
		prof := a.profileOf(anc.queuedData())
		chain = append(chain, prof.Tools)
		if prof.Policy != nil {
			merged = MergeRules(merged, *prof.Policy)
		}
	}
	if effective.Policy != nil {
		merged = MergeRules(merged, *effective.Policy)
	}
	if max := a.cfg.Spawn.MaxDepth; max > 0 && depth >= max {
		merged = MergeRules(merged, Rules{Deny: []string{"spawn_task"}})
	}
	chain = append(chain, effective.Tools)
	specs, err := a.ToolSpecs(WithRunInfo(WithAgent(ctx, a), a.runInfo(parent)))
	if err != nil {
		return nil, fmt.Errorf("fw: list the tools of the agent: %w", err)
	}
	out.Tools = concreteToolNames(specs, chain)
	if len(out.Tools) == 0 && anyGlobs(chain) {
		return nil, fmt.Errorf("%w: the tools of the child and the tools of the runs above it share no tool", ErrInvalidRequest)
	}
	if len(out.Tools) == 0 {
		// Tool-free actors can still delegate reasoning. Empty Tools means
		// unrestricted, so a deny-all policy makes this empty cap explicit.
		merged = MergeRules(merged, Rules{Deny: []string{"*"}})
	}
	out.Policy = &merged
	out.TimeZone = effective.TimeZone
	if out.TimeZone == "" {
		out.TimeZone = a.zoneNameOf(ctx, parent)
	}
	return &out, nil
}

// anyGlobs reports whether one list in a chain restricts the tools.
func anyGlobs(chain [][]string) bool {
	for _, g := range chain {
		if g != nil {
			return true
		}
	}
	return false
}

func concreteToolNames(specs []ToolSpec, chain [][]string) []string {
	var out []string
	for _, spec := range specs {
		ok := true
		for _, globs := range chain {
			if !toolAllowed(globs, spec) {
				ok = false
				break
			}
		}
		if ok {
			// Names can contain glob metacharacters (for example from a tool
			// source). Escape them before using them as profile patterns.
			out = append(out, strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`, `]`, `\]`).Replace(spec.Name))
		}
	}
	return out
}

// remoteChild returns where a remote child lives.
func (a *Agent) remoteChild(id string) (childRef, bool) {
	a.k.mu.Lock()
	defer a.k.mu.Unlock()
	ref, ok := a.k.remote[id]
	return ref, ok
}

// remoteChildren returns the remote children of a parent.
func (a *Agent) remoteChildren(parent string) map[string]childRef {
	a.k.mu.Lock()
	defer a.k.mu.Unlock()
	out := map[string]childRef{}
	for id, ref := range a.k.remote {
		if ref.parent == parent {
			out[id] = ref
		}
	}
	return out
}

// decorateChild sets the fields of a remote child that only this process
// knows: its parent, its depth, and the process that holds it.
func (a *Agent) decorateChild(r *Run, ref childRef) {
	if r == nil {
		return
	}
	r.Parent, r.Depth, r.Member = ref.parent, ref.depth, ref.member
}

// getRemote returns a remote child through the spawner, authorized as Get
// authorizes a run of this process. A branch whose start is still open is
// answered from this process.
func (a *Agent) getRemote(ctx context.Context, id string, ref childRef) (*Run, error) {
	r, ok := ref.starting()
	if !ok {
		var err error
		if r, err = a.spawner.Get(ctx, ref.member, id); err != nil {
			return nil, err
		}
	}
	a.decorateChild(r, ref)
	if err := a.Authorize(ctx, ActionRunView, Target{Agent: r.Agent, Run: r}); err != nil {
		return nil, err
	}
	return r, nil
}
