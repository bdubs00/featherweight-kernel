package fw

// This file is the Spawner: the plug behind spawn_task that starts a child
// run in another process (docs/ARCHITECTURE.md section 11.9). Without one,
// a child runs in this process. With one, the kernel keeps the place of
// each remote child in the ledger of its parent, so Get, WaitFor, Decide,
// Cancel, and Children reach the child after a restart too, and the
// surfaces show it with Member set.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// A Spawner runs the child runs of spawn_task in other processes. The ax
// driver creates a child Task for each one.
//
// Spawn starts a child of the parent run with req, and returns the child as
// the other process reports it and the name of that process, which
// Run.Member carries from then on. The kernel fills req before the call:
// Principal and PrincipalKind name the person of the parent, so the child
// acts as that person; Budget is the budget of the parent, capped, with
// Deadline the working time that the parent has left; the metadata key
// MetaParent holds the id of the parent; and Origin is OriginTool. The
// spawner adds MetaParentMember, the name of this process, when it knows
// it. Get, WaitFor, Decide, and Cancel reach a child at member, with the
// meaning of the Agent methods of the same names. They return
// ErrRunNotFound for a child that the process does not know.
type Spawner interface {
	Spawn(ctx context.Context, parent *Run, req RunRequest) (*Run, string, error)
	Get(ctx context.Context, member, id string) (*Run, error)
	WaitFor(ctx context.Context, member, id string, done func(*Run) bool) (*Run, error)
	Decide(ctx context.Context, member, id string, d ApprovalDecision) error
	Cancel(ctx context.Context, member, id string) error
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
// parent run when a spawner started a child of it in another process.
type RunChildData struct {
	Child  string `json:"child"`
	Member string `json:"member"`
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

// childRef is where a remote child lives.
type childRef struct {
	parent string
	member string
	depth  int
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
// of the parent as its cap, and is recorded in the ledger of the parent.
func (a *Agent) SpawnChild(ctx context.Context, parent string, req RunRequest) (*Run, error) {
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
	depth := p.depth + 1
	if max := a.cfg.Spawn.MaxDepth; max > 0 && depth > max {
		return nil, fmt.Errorf("%w: a child at depth %d is deeper than spawn.maxDepth %d", ErrSpawnLimit, depth, max)
	}
	q := p.queuedData()
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
	// The child gets at most the tools of the parent. A child without a
	// tool list gets the list of the parent; a child with one keeps it,
	// because the other process cannot compare the two.
	if parentTools := a.profileOf(q).Tools; parentTools != nil && (req.Profile == nil || req.Profile.Tools == nil) {
		prof := AgentProfile{}
		if req.Profile != nil {
			prof = *req.Profile
		}
		prof.Tools = append([]string(nil), parentTools...)
		req.Profile = &prof
	}
	k.mu.Lock()
	if max := a.cfg.Spawn.MaxChildren; max > 0 && k.kids[parent] >= max {
		k.mu.Unlock()
		return nil, fmt.Errorf("%w: run %s already started spawn.maxChildren (%d) child runs", ErrSpawnLimit, parent, max)
	}
	k.kids[parent]++
	k.mu.Unlock()
	child, member, err := a.spawner.Spawn(ctx, p.snapshot(), req)
	if err != nil || child == nil || child.ID == "" || member == "" {
		k.mu.Lock()
		k.kids[parent]--
		k.mu.Unlock()
		if err == nil {
			err = errors.New("fw: the spawner returned no child")
		}
		return nil, err
	}
	ref := childRef{parent: parent, member: member, depth: depth}
	if _, err := a.emit(nil, p, EvRunChild, RunChildData{Child: child.ID, Member: member}); err != nil {
		a.log.Warn("record a remote child", "run", parent, "child", child.ID, "member", member, "error", err)
	}
	k.mu.Lock()
	k.remote[child.ID] = ref
	k.mu.Unlock()
	a.decorateChild(child, ref)
	return child, nil
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
// authorizes a run of this process.
func (a *Agent) getRemote(ctx context.Context, id string, ref childRef) (*Run, error) {
	r, err := a.spawner.Get(ctx, ref.member, id)
	if err != nil {
		return nil, err
	}
	a.decorateChild(r, ref)
	if err := a.Authorize(ctx, ActionRunView, Target{Agent: r.Agent, Run: r}); err != nil {
		return nil, err
	}
	return r, nil
}
