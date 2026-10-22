package fw

// This file holds the runtime state of the kernel and the public run API.
// The turn loop is in turn.go, the projections are in projection.go, and
// resume, replay, compaction, and the bus are in their own files. The design
// is in docs/ARCHITECTURE.md, sections 4 and 5.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Errors of the kernel.
var (
	// ErrClosed is returned when the agent shuts down or is closed.
	ErrClosed = errors.New("fw: agent is closed")
	// ErrInvalidRequest marks a request that fails validation, so a surface answers 400.
	ErrInvalidRequest = errors.New("fw: invalid request")
	// ErrNoModel means that neither the configuration nor the profile of a
	// run names a model. The kernel has no default model.
	ErrNoModel = errors.New("fw: no model configured; set model in fw.yaml or FW_MODEL, or model on the agent profile")
	// ErrNoPendingApproval is returned by Decide when the run waits for no
	// approval with that id.
	ErrNoPendingApproval = errors.New("fw: no pending approval with that id")
	// ErrAgentNotFound is returned when a request names an unknown profile.
	ErrAgentNotFound = errors.New("fw: agent profile not found")
	// ErrSpawnLimit is returned when a child run would go deeper than
	// Config.Spawn.MaxDepth, when the parent already started
	// Config.Spawn.MaxChildren children, or when the tree of the parent
	// already holds Config.Spawn.MaxDescendants runs.
	ErrSpawnLimit = errors.New("fw: spawn limit reached")
	// ErrTreeBudget is returned when a child run would start in a tree
	// that used up a limit of the budget that holds for the whole tree:
	// maxTokens, maxCostUSD, or maxToolCalls of the parent or of one of its
	// ancestors (see Budget).
	ErrTreeBudget = errors.New("fw: the budget of the run tree is used up")
	// ErrRunExists is returned when a request names the id of a run that
	// this process knows already: a run of this process, a child of one in
	// another process, or a run of the ledger. A surface answers 409.
	ErrRunExists = errors.New("fw: a run with that id exists")
	// ErrSaturated means the finite direct-run backlog is full. Retry later.
	ErrSaturated = errors.New("fw: run admission is full; retry later")
	// ErrStorageUnavailable means storage failed and this process requires restart.
	ErrStorageUnavailable = errors.New("fw: storage unavailable; restart required")
)

// OriginKernel marks a message that the kernel adds itself, such as the
// continuation message after a max_tokens stop.
const OriginKernel = "kernel"

const (
	defaultMaxRuns       = 16 // when Config.MaxRuns is 0 outside ax; a Task runs one at a time
	defaultMaxQueuedRuns = 64
	defaultMaxTokens     = 64000
)

// kernel holds the runtime state of the agent: the run cache, the drivers of
// active runs, and the concurrency limit.
type kernel struct {
	atespace  string
	compactAt int // compaction.at: estimated tokens of a request that start a compaction

	mu        sync.Mutex
	runs      map[string]*runState
	idem      map[string]string          // idempotency key to run id
	overrides map[string]*runOverride    // per-run tools and policy, for replay
	kids      map[string]int             // child runs started by each run, in its life
	remote    map[string]childRef        // the children that a Spawner runs in other processes
	children  map[string][]*runState     // the child runs in this process, by the id of the parent
	records   map[string][]*runState     // the records of children that never ran (standIn), by the id of the parent; no part of the tree
	remoteOf  map[string]map[string]bool // the ids of remote, by the id of the parent
	reports   map[string]*childReport    // what each child in another process last reported, see tree.go
	spent     map[string]string          // the runs whose tree used up a limit, with the reason of the cancels under them
	retrying  map[string]bool            // the runs whose stop tries a failed cancel again
	noSpawner sync.Once                  // the warning that this process cannot reach its children in other processes
	giveUp    time.Duration              // how long a child that does not answer still counts as reachable (followGiveUp)

	// reactMu serializes Agent.React, so each reaction is checked against
	// the earlier ones before it appends.
	reactMu sync.Mutex

	sem      chan struct{} // one slot per active run, Config.MaxRuns slots
	childSem chan struct{} // one slot per working child run, Config.Spawn.MaxRunning slots; nil for no limit

	// slotMu guards rootDrivers and the admissions of the bus intake
	// (intake.go). rootDrivers counts the drivers of top-level runs: each
	// holds a slot of sem or waits for one. slotsChanged is closed and
	// replaced when either count changes.
	slotMu       sync.Mutex
	rootDrivers  int
	admitted     int
	slotsChanged chan struct{}

	lifeMu     sync.Mutex
	draining   bool
	drainCh    chan struct{} // closed when shutdown starts
	closedCh   chan struct{} // closed when shutdown ends
	storageErr error         // first storage failure; guarded by lifeMu
	storageAt  time.Time     // the time of storageErr; guarded by lifeMu
	failedCh   chan struct{} // closed on the first storage failure
	wg         sync.WaitGroup

	ctx    context.Context // parent of every run context
	cancel context.CancelFunc

	once       sync.Once
	intakeStop func()
}

// runOverride replaces the tools, the policy, and the brief of one run.
type runOverride struct {
	wrap    ToolMiddleware
	policy  Policy
	brief   *string // the brief of the recorded run, for a replay
	context *string // the context of the recorded run, for a replay
	zone    *string // the time zone of the recorded run, for a replay
}

// runState is one run in the cache. Every event of the run goes through
// emitLocked while emitMu is held, so the ledger order, the cached status,
// and the in-memory event list agree.
type runState struct {
	id     string
	agent  string
	source string
	trace  string
	parent string        // the parent run, or ""
	depth  int           // 0 for a top-level run
	queued RunQueuedData // the request, as fw.run.queued recorded it
	text   *runText      // the text of the run, in memory only (textOf)
	// maxDesc is the most runs that the run can have under it, from
	// MetaMaxDescendants, or -1 when the metadata sets no limit.
	maxDesc int

	// The slot of a child run in Config.Spawn.MaxRunning (tree.go). held
	// is true while the driver holds a slot, parked counts the waits for
	// other runs that are open now, and lent is true while those waits
	// gave the slot up.
	slotMu sync.Mutex
	held   bool
	lent   bool
	parked int
	// standIn marks the record of a child or a branch that never got its
	// run in another process (fork.go). Nothing drives it: it takes no
	// message, no decision, and no branch, and a restart does not resume it.
	standIn    bool
	owner      *kernel
	acceptDone chan struct{} // non-nil while a Start owns durable acceptance
	acceptErr  error         // published by closing acceptDone
	reserved   bool          // root admission held before driver launch; guarded by mu
	// copying is true while appendBranch copies the kept events of a
	// branch. The place that the branch reserved before its first append
	// then stays as it is: a copied terminal event does not give it back,
	// and a copied message or decision takes no other. Guarded by emitMu.
	copying bool

	emitMu    sync.Mutex
	events    []Event // the full log while a driver holds a reference
	eventsRef int
	hist      *historyBuilder // incremental history over events
	histN     int             // number of events that hist consumed
	exec      *cursorBuilder  // incremental execution cursor, guarded by emitMu
	execN     int             // number of events that exec consumed

	mu        sync.Mutex
	run       Run
	changed   chan struct{} // closed and replaced on every event
	active    bool          // a driver goroutine owns the run
	cancel    context.CancelFunc
	drvCtx    context.Context // the context of the driver that owns the run
	canceling bool
	waiters   map[string]chan ApprovalDecidedData // questions from tools, by approval id
	// dir is the directory of the tools of the run once dirSet (dirOf).
	dir    string
	dirSet bool
}

func (k *kernel) newRunState(id, agent, trace string) *runState {
	if agent == "" {
		agent = "default"
	}
	if trace == "" {
		trace = newTraceParent()
	}
	return &runState{
		id:      id,
		owner:   k,
		agent:   agent,
		source:  fmt.Sprintf("fw://%s/%s/%s", k.atespace, agent, id),
		trace:   trace,
		maxDesc: -1,
		changed: make(chan struct{}),
		waiters: map[string]chan ApprovalDecidedData{},
	}
}

// startKernel opens the runtime, rebuilds projections from the ledger, and
// resumes unfinished runs.
func (a *Agent) startKernel(ctx context.Context) error {
	maxRuns := a.cfg.RunSlots()
	kctx, cancel := context.WithCancel(context.Background())
	k := &kernel{
		atespace:     atespaceFromEnv(),
		compactAt:    a.cfg.Compaction.At,
		runs:         map[string]*runState{},
		idem:         map[string]string{},
		overrides:    map[string]*runOverride{},
		kids:         map[string]int{},
		remote:       map[string]childRef{},
		children:     map[string][]*runState{},
		records:      map[string][]*runState{},
		remoteOf:     map[string]map[string]bool{},
		reports:      map[string]*childReport{},
		spent:        map[string]string{},
		retrying:     map[string]bool{},
		sem:          make(chan struct{}, maxRuns),
		drainCh:      make(chan struct{}),
		slotsChanged: make(chan struct{}),
		closedCh:     make(chan struct{}),
		failedCh:     make(chan struct{}),
		ctx:          kctx,
		cancel:       cancel,
	}
	if n := a.cfg.Spawn.MaxRunning; n > 0 {
		k.childSem = make(chan struct{}, n)
	}
	k.giveUp = a.giveUp
	a.k = k
	unfinished, rec, err := a.rebuild(ctx)
	if err != nil {
		cancel()
		return err
	}
	if a.spawner == nil && len(k.remote) > 0 {
		a.warnNoSpawner()
	}
	a.startFollow()
	a.startEgress()
	a.startOutcomes()
	a.recover(rec)
	a.resendCancels()
	if !a.noResume {
		a.resume(unfinished)
	}
	a.startIntake()
	return nil
}

// cancelStarts ends the start of every branch and child in another
// process that is still open and waits for each record, until ctx ends or
// five seconds pass. A look for a child after a restart ends without a
// record, so the next start looks again.
func (a *Agent) cancelStarts(ctx context.Context) {
	k := a.k
	k.mu.Lock()
	var starts []*branchStart
	for _, ref := range k.remote {
		if _, ok := ref.starting(); ok {
			starts = append(starts, ref.start)
		}
	}
	k.mu.Unlock()
	if len(starts) == 0 {
		return
	}
	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, st := range starts {
		st.cancel()
	}
	for _, st := range starts {
		select {
		case <-st.done:
		case <-wait.Done():
			return
		}
	}
}

// shutdownKernel stops accepting work, waits until ctx ends for in-flight
// model calls, appends fw.run.suspended to every run that is still working,
// and flushes the ledger. A second call returns at once.
func (a *Agent) shutdownKernel(ctx context.Context) {
	k := a.k
	if k == nil {
		return
	}
	k.once.Do(func() {
		k.lifeMu.Lock()
		stop := k.intakeStop
		k.draining = true
		close(k.drainCh)
		k.lifeMu.Unlock()
		if stop != nil {
			stop()
		}

		done := make(chan struct{})
		go func() {
			k.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-ctx.Done():
		}
		// Stop every call that is still in flight. A driver that wakes up
		// after this point cannot append, because its context is done.
		k.cancel()
		// End the start of every branch that another process has not
		// taken yet. The driver removes the process it created, and the
		// branch is recorded here as canceled while the ledger is open.
		a.cancelStarts(ctx)

		for _, rs := range k.all() {
			rs.emitMu.Lock()
			rs.mu.Lock()
			st := rs.run.State
			rs.mu.Unlock()
			if st == StateRunning || st == StateQueued {
				if _, err := a.emitLocked(nil, rs, EvRunSuspended, RunSuspendedData{Reason: "shutdown"}); err != nil {
					a.log.Error("suspend run", "run", rs.id, "error", err)
				}
			}
			rs.emitMu.Unlock()
		}
		if f, ok := a.ledger.(interface{ Flush() error }); ok {
			if err := f.Flush(); err != nil {
				a.failStorage(storageFailed(fmt.Errorf("fw: flush ledger: %w", err)))
			}
		}
		a.closeResources()
		close(k.closedCh)
	})
}

// idemKey scopes an idempotency key by principal, so one caller cannot reach
// the run of another through a shared key.
func idemKey(principal, key string) string { return principal + "\x00" + key }

// claimableLocked reports ErrRunExists for an id that this process knows: a
// run of this process, or a child of one in another process, including a
// child whose start is open. A run under the id of a child would answer for
// the child. The caller holds k.mu.
func (k *kernel) claimableLocked(id string) error {
	if _, ok := k.runs[id]; ok {
		return fmt.Errorf("%w: run %s", ErrRunExists, id)
	}
	if ref, ok := k.remote[id]; ok {
		return fmt.Errorf("%w: %s is a child of run %s in another process", ErrRunExists, id, ref.parent)
	}
	return nil
}

// checkNamedID refuses the id that a request names when this process or
// its ledger knows the id. The same start sent again under its idempotency
// key passes, and returns the run that it started.
func (a *Agent) checkNamedID(ctx context.Context, req RunRequest) error {
	k := a.k
	k.mu.Lock()
	if req.IdempotencyKey != "" {
		if id, ok := k.idem[idemKey(req.Principal, req.IdempotencyKey)]; ok && id == req.ID {
			k.mu.Unlock()
			return nil
		}
	}
	err := k.claimableLocked(req.ID)
	k.mu.Unlock()
	if err != nil {
		return err
	}
	for _, err := range a.ledger.Read(ctx, req.ID, 0) {
		if err != nil {
			return fmt.Errorf("fw: read the ledger for the id %s: %w", req.ID, err)
		}
		return fmt.Errorf("%w: the ledger has a run %s", ErrRunExists, req.ID)
	}
	return nil
}

// drivable refuses a run that stands in for a child that never got its run
// in another process. A message or a decision would run it here, without
// the profile and the budget that the child got, as a run of its own.
func (rs *runState) drivable() error {
	if rs.standIn {
		return fmt.Errorf("%w: run %s records a child that never ran in its process, so nothing runs it here", ErrInvalidRequest, rs.id)
	}
	return nil
}

func (k *kernel) isDraining() bool {
	k.lifeMu.Lock()
	defer k.lifeMu.Unlock()
	return k.draining || k.storageErr != nil
}

func (k *kernel) lookup(id string) (*runState, error) {
	k.mu.Lock()
	rs := k.runs[id]
	k.mu.Unlock()
	if rs == nil {
		return nil, fmt.Errorf("%w: %s", ErrRunNotFound, id)
	}
	return rs, nil
}

// keyed returns the run that a start under the idempotency key of the
// principal began, or nil.
func (k *kernel) keyed(principal, key string) *runState {
	if key == "" {
		return nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if id, ok := k.idem[idemKey(principal, key)]; ok {
		return k.runs[id]
	}
	return nil
}

func (k *kernel) all() []*runState {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]*runState, 0, len(k.runs))
	for _, rs := range k.runs {
		out = append(out, rs)
	}
	return out
}

func (k *kernel) override(id string) *runOverride {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.overrides[id]
}

// Start queues a run and returns at once.
func (a *Agent) Start(ctx context.Context, input string, opts ...RunOption) (*Run, error) {
	req := RunRequest{Input: input}
	for _, o := range opts {
		o(&req)
	}
	return a.start(ctx, req)
}

// start appends fw.run.queued for a request and launches its driver.
func (a *Agent) start(ctx context.Context, req RunRequest) (*Run, error) {
	return a.startJob(ctx, req, nil)
}

// startJob is start for a job that the bus intake took with the admission
// t, or for any other start when t is nil. Every refusal comes before
// fw.run.queued is appended: a drain, a storage failure, a full backlog,
// and a request that fails validation. Once fw.run.queued is in the
// ledger, the run is accepted, and startJob returns it, also when a drain
// or a storage failure comes after the append. The ledger then holds the
// run, the bus intake acknowledges its job, and the next start of the
// process resumes the run, so no other process starts it again.
func (a *Agent) startJob(ctx context.Context, req RunRequest, t *admission) (*Run, error) {
	k := a.k
	if err := k.writeError(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Input) == "" && len(req.Files) == 0 {
		return nil, fmt.Errorf("%w: run input is empty", ErrInvalidRequest)
	}
	// Only the kernel records a change of the text of a run. The
	// idempotency key of the metadata keeps its old handling below.
	for k := range req.Metadata {
		if kernelMeta(k) && k != metaIdempotencyKey {
			return nil, fmt.Errorf("%w: the metadata key %s is reserved for the kernel", ErrInvalidRequest, k)
		}
	}
	if req.Agent == "" {
		req.Agent = "default"
	}
	// The request reads the profile once, so a SetProfiles meanwhile cannot
	// split it between two versions.
	named, ok := a.ProfileNamed(req.Agent)
	if !ok && req.Agent != "default" {
		return nil, fmt.Errorf("%w: %s", ErrAgentNotFound, req.Agent)
	}
	if !ok {
		named = AgentProfile{Name: req.Agent}
	}
	if err := a.Authorize(ctx, ActionAgentRun, Target{Agent: req.Agent}); err != nil {
		return nil, err
	}
	if req.Principal == "" {
		if p := PrincipalFrom(ctx); p != nil {
			req.Principal = p.Subject
		}
	}
	// The run keeps the kind of the person who starts it. A trusted process
	// that starts a run for a person names the kind in the request.
	kind := req.PrincipalKind
	if p := PrincipalFrom(ctx); kind == "" && p != nil && p.Subject == req.Principal {
		kind = p.Kind
	}
	namedID := req.ID != ""
	if !namedID {
		req.ID = NewID()
	} else if err := checkRunID(req.ID); err != nil {
		return nil, err
	}
	if err := a.checkProfile(req.Profile); err != nil {
		return nil, err
	}
	if err := a.checkModelOf(named, req.Profile); err != nil {
		return nil, err
	}
	if err := CheckOutcome(a.cfg, req.Outcome); err != nil {
		return nil, err
	}
	if len(req.Instructions) > maxInstructionBytes {
		return nil, fmt.Errorf("%w: the instructions are longer than %d bytes", ErrInvalidRequest, maxInstructionBytes)
	}
	var parent *runState
	var parentRun *Run
	depth := 0
	if req.Parent != "" {
		if a.cfg.Spawn.Disabled {
			return nil, fmt.Errorf("%w: spawning is disabled", ErrSpawnLimit)
		}
		p, err := k.lookup(req.Parent)
		if err != nil {
			return nil, fmt.Errorf("%w: parent run %s not found", ErrInvalidRequest, req.Parent)
		}
		parent, depth = p, p.depth+1
		parentRun = p.snapshot()
		if max := a.cfg.Spawn.MaxDepth; max > 0 && depth > max {
			return nil, fmt.Errorf("%w: a child at depth %d is deeper than spawn.maxDepth %d", ErrSpawnLimit, depth, max)
		}
		// A child belongs to whoever started the parent, so the same people
		// see and operate the whole tree.
		if req.Principal == "" {
			q := p.queuedData()
			req.Principal, kind = q.Principal, q.PrincipalKind
		}
		// A retried start takes its child even when the tree has nothing
		// left now. A new child starts only while the tree has some of
		// every limit that the tree shares.
		if prev := k.keyed(req.Principal, req.IdempotencyKey); prev != nil {
			return prev.accepted(ctx)
		}
		if o, spent := a.limitSpent(p); spent {
			return nil, o.refusal()
		}
	}
	if prev := k.keyed(req.Principal, req.IdempotencyKey); prev != nil {
		return prev.accepted(ctx)
	}
	if namedID {
		if err := a.checkNamedID(ctx, req); err != nil {
			return nil, err
		}
	}
	// A top-level run takes its place in the backlog before it stores
	// anything, so a full backlog leaves no file of the request on disk. A
	// child takes no place (driverBody). The run takes the place over at
	// its append; until then, a refusal gives it back.
	held := false
	if parent == nil {
		if err := a.claimPlace(t, true); err != nil {
			return nil, err
		}
		held = true
		defer func() {
			if held {
				k.addRootDriver(-1)
			}
		}()
	}
	files, changes, err := a.keepFiles(a.newRunDir(req.ID, parent, req.FilesRoot), req.Files)
	if err != nil {
		return nil, err
	}
	extra := a.extraTools(ctx, req, named, parent)
	resolved := a.resolveProfile(ctx, req, named, parent)
	req.Metadata = inheritBatch(req.Metadata, parentRun)
	meta := a.annotate(ctx, &req, parentRun, req.Metadata)
	if req.IdempotencyKey != "" {
		if meta == nil {
			meta = map[string]string{}
		}
		meta["idempotencyKey"] = req.IdempotencyKey
	}
	queued := RunQueuedData{
		Agent:         req.Agent,
		Input:         req.Input,
		Instructions:  req.Instructions,
		Profile:       req.Profile,
		Budget:        req.Budget,
		Metadata:      meta,
		Origin:        req.Origin,
		Principal:     req.Principal,
		PrincipalKind: kind,
		Parent:        req.Parent,
		Depth:         depth,
		Files:         files,
		ExtraTools:    extra,
		Resolved:      resolved,
		FilesRoot:     req.FilesRoot,
		Outcome:       a.resolveOutcome(req, named, parent),
	}
	text := a.pinText(named, &queued)
	ev, err := NewEvent(req.ID, EvRunQueued, queued)
	if err != nil {
		return nil, err
	}
	rs := k.newRunState(req.ID, req.Agent, "")
	rs.acceptDone = make(chan struct{})
	rs.parent, rs.depth, rs.text = req.Parent, depth, text
	_ = ev.Decode(&rs.queued)
	rs.maxDesc = maxDescOf(rs.queued.Metadata)
	if parent != nil {
		// A child shares the trace of its parent, so one trace shows the tree.
		rs.trace = parent.trace
	}
	ev.Source, ev.TraceParent = rs.source, rs.trace
	rs.run.Apply(ev, nil)

	k.mu.Lock()
	if req.IdempotencyKey != "" {
		if id, ok := k.idem[idemKey(req.Principal, req.IdempotencyKey)]; ok {
			prev := k.runs[id]
			k.mu.Unlock()
			return prev.accepted(ctx)
		}
	}
	if err := k.claimableLocked(req.ID); err != nil {
		k.mu.Unlock()
		return nil, err
	}
	// Register acceptance before publication so shutdown cannot finish its
	// wait while an initial append is still in flight.
	k.lifeMu.Lock()
	if err := k.writeErrorLocked(); err != nil {
		k.lifeMu.Unlock()
		k.mu.Unlock()
		return nil, err
	}
	k.wg.Add(1)
	k.lifeMu.Unlock()
	defer k.wg.Done()
	if parent != nil {
		if max := a.cfg.Spawn.MaxChildren; max > 0 && k.kids[req.Parent] >= max {
			k.mu.Unlock()
			return nil, fmt.Errorf("%w: run %s already started spawn.maxChildren (%d) child runs", ErrSpawnLimit, req.Parent, max)
		}
		if _, err := a.treeRoomLocked(parent, 1); err != nil {
			k.mu.Unlock()
			return nil, err
		}
		k.kids[req.Parent]++
		k.children[req.Parent] = append(k.children[req.Parent], rs)
	}
	k.runs[req.ID] = rs
	if req.IdempotencyKey != "" {
		k.idem[idemKey(req.Principal, req.IdempotencyKey)] = req.ID
	}
	k.mu.Unlock()
	defer func() { close(rs.acceptDone) }()

	rs.emitMu.Lock()
	if held {
		// A failed append gives the place back, and the launch moves it
		// to the driver.
		rs.mu.Lock()
		rs.reserved = true
		rs.mu.Unlock()
		held = false
	}
	_, err = a.appendLocked(nil, rs, ev)
	rs.emitMu.Unlock()
	if err != nil {
		rs.acceptErr = err
		k.mu.Lock()
		delete(k.runs, req.ID)
		if parent != nil {
			k.kids[req.Parent]--
			k.dropChildLocked(req.Parent, rs)
		}
		if req.IdempotencyKey != "" {
			delete(k.idem, idemKey(req.Principal, req.IdempotencyKey))
		}
		k.mu.Unlock()
		return nil, err
	}
	// The run is accepted. A record of an attachment that fails now is a
	// storage failure, which blocks the process: the run stays in the
	// ledger, and the restart reconciles it.
	for _, d := range changes {
		if _, err := a.emit(nil, rs, EvFileChanged, d); err != nil {
			a.log.Warn("the record of an attachment of an accepted run failed; a restart reconciles the run", "run", rs.id, "error", a.Redact(err.Error()))
			break
		}
	}
	// During a drain or after a storage failure, launchLocked starts no
	// driver and gives the place back. The shutdown suspends the run.
	rs.mu.Lock()
	a.launchLocked(rs)
	rs.mu.Unlock()
	return rs.snapshot(), nil
}

// Run starts a run and blocks until it reaches a terminal state or needs input.
func (a *Agent) Run(ctx context.Context, input string, opts ...RunOption) (*Run, error) {
	r, err := a.Start(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	return a.Wait(ctx, r.ID)
}

// Wait blocks until the run reaches a terminal state or needs input. When ctx
// ends first, Wait returns the current status and the context error.
func (a *Agent) Wait(ctx context.Context, id string) (*Run, error) {
	return a.WaitFor(ctx, id, func(r *Run) bool { return r.State.Terminal() || r.State == StateInputRequired })
}

// WaitFor blocks until done reports true for the status of the run. It
// checks the status now and after every event. When ctx ends first, WaitFor
// returns the current status and the context error. A tool that waits for a
// child run uses it to see the pending approval of the child. A child run
// of this process that waits gives up its place in spawn.maxRunning until
// the wait ends, so the runs that it waits for can work.
func (a *Agent) WaitFor(ctx context.Context, id string, done func(*Run) bool) (*Run, error) {
	if self := a.waitingRun(ctx); self != nil {
		defer a.park(self)()
	}
	return a.waitFor(ctx, id, done)
}

// waitFor is WaitFor without the slot of the caller.
func (a *Agent) waitFor(ctx context.Context, id string, done func(*Run) bool) (*Run, error) {
	rs, err := a.k.lookup(id)
	if err != nil {
		if ref, ok := a.remoteChild(id); ok {
			if failure := a.k.storageError(); failure != nil {
				r, err := a.getRemote(ctx, id, ref)
				if err != nil {
					return r, err
				}
				return r, failure
			}
			if r, ok := ref.starting(); ok {
				// The child is on its way to its process. Wait for the
				// start to end, then ask again: the child is then in that
				// process, or recorded here as failed.
				if done(r) {
					return r, nil
				}
				select {
				case <-ref.start.done:
					return a.waitFor(ctx, id, done)
				case <-ctx.Done():
					return r, ctx.Err()
				}
			}
			if a.spawner == nil {
				return nil, a.unreachable(id, ref)
			}
			// Each read of the child counts its tree toward the parent.
			r, err := a.spawner.WaitFor(ctx, ref.member, id, func(r *Run) bool {
				a.decorateChild(r, ref)
				a.observe(id, ref, r)
				return done(r)
			})
			a.decorateChild(r, ref)
			a.observe(id, ref, r)
			return r, err
		}
		return nil, err
	}
	for {
		rs.mu.Lock()
		cur, ch := cloneRun(&rs.run), rs.changed
		rs.mu.Unlock()
		if err := a.k.storageError(); err != nil && !cur.State.Terminal() {
			return a.report(rs), err
		}
		if done(cur) {
			return a.withTree(rs, cur), nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return a.report(rs), ctx.Err()
		case <-a.k.closedCh:
			return a.report(rs), ErrClosed
		case <-a.k.failedCh:
			return a.report(rs), a.k.storageError()
		}
	}
}

// Children returns the child runs of a run that the principal in ctx can
// view, newest first.
func (a *Agent) Children(ctx context.Context, id string) ([]*Run, error) {
	rs, err := a.k.lookup(id)
	if err != nil {
		return nil, err
	}
	if err := a.authorizeRun(ctx, rs, ActionRunView); err != nil {
		return nil, err
	}
	var kids []*Run
	for _, child := range a.k.localChildren(id) {
		kids = append(kids, a.report(child))
	}
	for _, rec := range a.k.recordsOf(id) {
		kids = append(kids, rec.snapshot())
	}
	for cid, ref := range a.remoteChildren(id) {
		r, ok := ref.starting()
		if !ok {
			if a.spawner == nil {
				a.warnNoSpawner()
				continue
			}
			var err error
			if r, err = a.spawner.Get(ctx, ref.member, cid); err != nil {
				a.log.Warn("read a remote child", "run", cid, "member", ref.member, "error", err)
				continue
			}
			a.observe(cid, ref, r)
		}
		a.decorateChild(r, ref)
		kids = append(kids, r)
	}
	out, err := a.viewable(ctx, kids)
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// viewable returns the runs that the principal in ctx can view, in one
// batch of checks. A decision point that does not answer is an error, so a
// listing never hides an outage as an empty list.
func (a *Agent) viewable(ctx context.Context, runs []*Run) ([]*Run, error) {
	checks := make([]AuthzCheck, len(runs))
	for i, r := range runs {
		checks[i] = AuthzCheck{Action: ActionRunView, Target: Target{Agent: r.Agent, Run: r}}
	}
	errs := a.AuthorizeAll(ctx, checks)
	out := make([]*Run, 0, len(runs))
	for i, r := range runs {
		switch {
		case errs[i] == nil:
			out = append(out, r)
		case errors.Is(errs[i], ErrAuthzUnavailable):
			return nil, errs[i]
		}
	}
	return out, nil
}

// authorizeRun checks an action on a run for the principal in ctx.
func (a *Agent) authorizeRun(ctx context.Context, rs *runState, action string) error {
	if PrincipalFrom(ctx) == nil {
		return nil
	}
	r := rs.snapshot()
	return a.Authorize(ctx, action, Target{Agent: r.Agent, Run: r})
}

// Limits of an inline profile and of run instructions.
const (
	maxInstructionBytes = 256 << 10
	maxProfileGlobs     = 64
)

// checkProfile validates an inline profile of a request.
func (a *Agent) checkProfile(p *AgentProfile) error {
	if p == nil {
		return nil
	}
	if len(p.System) > maxInstructionBytes {
		return fmt.Errorf("%w: the profile system prompt is longer than %d bytes", ErrInvalidRequest, maxInstructionBytes)
	}
	total := 0
	for _, s := range p.Instructions {
		total += len(s)
	}
	if total > maxInstructionBytes {
		return fmt.Errorf("%w: the profile instructions are longer than %d bytes", ErrInvalidRequest, maxInstructionBytes)
	}
	if len(p.Tools) > maxProfileGlobs || len(p.Skills) > maxProfileGlobs {
		return fmt.Errorf("%w: a profile lists at most %d tool or skill globs", ErrInvalidRequest, maxProfileGlobs)
	}
	if !ValidEffort(p.Effort) {
		return fmt.Errorf("%w: the effort %q is not one of %s", ErrInvalidRequest, p.Effort, strings.Join(Efforts, ", "))
	}
	if p.Model != "" {
		sel := a.cfg.ModelSelector(p.Model)
		if provider, model := SplitSelector(sel); provider == "" || model == "" {
			return fmt.Errorf("%w: the model %q is neither a provider/model selector nor a name from models", ErrInvalidRequest, p.Model)
		}
	}
	if p.TimeZone != "" {
		if _, _, err := LoadZone(p.TimeZone); err != nil {
			return fmt.Errorf("%w: the profile time zone %q is not an IANA name such as America/Chicago, or UTC", ErrInvalidRequest, p.TimeZone)
		}
	}
	if p.Budget != nil && p.Budget.negative() {
		return fmt.Errorf("%w: a budget limit is negative", ErrInvalidRequest)
	}
	return CheckOutcome(a.cfg, p.Outcome)
}

// checkModel makes sure that a run has a model: the one of the request
// profile, else the one of the agent profile, else the default model of
// the configuration. The kernel has no built-in default, so a process
// without a model refuses the run here, before anything is recorded.
func (a *Agent) checkModel(req RunRequest) error {
	named, _ := a.ProfileNamed(req.Agent)
	return a.checkModelOf(named, req.Profile)
}

// checkModelOf is checkModel for a named profile that the caller read.
func (a *Agent) checkModelOf(named AgentProfile, inline *AgentProfile) error {
	if a.model != nil {
		return nil
	}
	model := named.Model
	if inline != nil && inline.Model != "" {
		model = inline.Model
	}
	if a.cfg.ModelSelector(model) == "" {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, ErrNoModel)
	}
	return nil
}

// Get returns the status of a run. With a principal in ctx, the principal
// needs ActionRunView on the run.
func (a *Agent) Get(ctx context.Context, id string) (*Run, error) {
	rs, err := a.k.lookup(id)
	if err != nil {
		if ref, ok := a.remoteChild(id); ok {
			return a.getRemote(ctx, id, ref)
		}
		return nil, err
	}
	r := a.report(rs)
	if err := a.Authorize(ctx, ActionRunView, Target{Agent: r.Agent, Run: r}); err != nil {
		return nil, err
	}
	return r, nil
}

// RunBudget returns the effective budget of a run: its own budget, merged
// with the budget of its profile and of the configuration as the kernel
// resolved them when it queued the run, with every change of the budget
// (ChangeBudget) on top, and capped by the budget of its parent. It
// authorizes the caller as Get does. A zero field has no limit.
// MaxTokens, MaxCostUSD, and MaxToolCalls hold for the run and every run
// under it together, and the limits of each ancestor hold for its tree
// too; Run.TreeUsage of a run tells what its tree used.
func (a *Agent) RunBudget(ctx context.Context, id string) (Budget, error) {
	rs, err := a.k.lookup(id)
	if err != nil {
		return Budget{}, err
	}
	r := rs.snapshot()
	if err := a.Authorize(ctx, ActionRunView, Target{Agent: r.Agent, Run: r}); err != nil {
		return Budget{}, err
	}
	return a.budgetOf(rs), nil
}

// List returns every known run, newest first. With a principal in ctx, it
// returns the runs that the principal can view.
func (a *Agent) List(ctx context.Context) ([]*Run, error) {
	all := a.k.all()
	runs := make([]*Run, 0, len(all))
	for _, rs := range all {
		runs = append(runs, a.report(rs))
	}
	out, err := a.viewable(ctx, runs)
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// Events replays the ledger of a run from a sequence number and then follows
// it. The channel closes after the terminal event that ends the log, or when
// ctx ends.
func (a *Agent) Events(ctx context.Context, id string, from uint64) (<-chan Event, error) {
	rs, err := a.k.lookup(id)
	if err != nil {
		return nil, err
	}
	if err := a.authorizeRun(ctx, rs, ActionRunView); err != nil {
		return nil, err
	}
	wctx, cancel := context.WithCancel(ctx)
	in, err := a.ledger.Watch(wctx, id, from)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("fw: watch %s: %w", id, err)
	}
	out := make(chan Event, 16)
	go func() {
		defer close(out)
		defer cancel()
		ended := false
		for e := range in {
			select {
			case out <- e:
			case <-wctx.Done():
				return
			}
			// A follow-up reopens a finished run, so close only when no
			// event follows the terminal event. A note after the end, such
			// as an annotation or the report of a child, leaves the run
			// finished (LeavesFinished).
			switch {
			case IsTerminal(e.Type):
				ended = true
			case !LeavesFinished(e.Type):
				ended = false
			}
			// The watch reads an event as soon as the ledger stores it,
			// before the run applies it, so the stream waits for the run:
			// a Get after the end then reads the state that ended it.
			if !ended {
				continue
			}
			applied, ok := rs.appliedThrough(wctx, e.Seq)
			if !ok || applied <= e.Seq {
				return
			}
		}
	}()
	return out, nil
}

// SendOption changes a follow-up message (see Send).
type SendOption func(*sendOptions)

type sendOptions struct {
	budget *Budget
	from   *MessageFrom
}

// WithSendBudget changes the budget of the run together with the message:
// each field that b sets replaces the same field of the budget of the run
// for the work that follows, as ChangeBudget does. A person continues a run
// that stopped at a limit this way. Every limit still counts the whole
// run: the turns, the tool calls, the tokens, the cost, and the working
// time before the message count against the new limits.
func WithSendBudget(b Budget) SendOption { return func(o *sendOptions) { o.budget = &b } }

// WithMessageFrom names the party that wrote the message when the caller
// relays it for that party (MessageReceivedData.From), such as the front
// of a fleet that passes the message of one agent to another. Only the
// process itself, without a principal in the context, the static token,
// or a workload relays; Send refuses the option with ErrForbidden for any
// other principal, so a person cannot pose as another party. A message
// from an agent has the role user, and the model reads it inside a block
// (MessageBlock) that has the authority of a tool result.
func WithMessageFrom(f MessageFrom) SendOption { return func(o *sendOptions) { o.from = &f } }

// Send appends a follow-up message to a run. A finished or idle run starts a
// new turn. A run in the middle of a turn sends the message with its next
// model call, after the tool results. A run that waits for an approval holds
// the message until the decision. WithSendBudget changes the budget of the
// run in the same step, before the message, so the turn that the message
// starts has the new limits. The message records the subject of the
// principal in ctx as its sender (MessageReceivedData.By), and
// WithMessageFrom the party that the sender relays it for.
func (a *Agent) Send(ctx context.Context, id string, msg Message, origin string, opts ...SendOption) error {
	if err := a.k.writeError(); err != nil {
		return err
	}
	var o sendOptions
	for _, opt := range opts {
		opt(&o)
	}
	rs, err := a.k.lookup(id)
	if err != nil {
		return err
	}
	if err := a.authorizeRun(ctx, rs, ActionRunOperate); err != nil {
		return err
	}
	if err := rs.drivable(); err != nil {
		return err
	}
	if msg.Role == "" {
		msg.Role = RoleUser
	}
	if msg.Role != RoleUser && msg.Role != RoleSystem {
		return fmt.Errorf("%w: a follow-up message has role user or system, not %q", ErrInvalidRequest, msg.Role)
	}
	if len(msg.Content) == 0 {
		return fmt.Errorf("%w: a follow-up message has no content", ErrInvalidRequest)
	}
	if o.from != nil {
		if err := checkMessageFrom(*o.from); err != nil {
			return err
		}
		if !speaksForOthers(ctx) {
			return fmt.Errorf("%w: only the static token or a workload can name the sender of a message", ErrForbidden)
		}
		if o.from.Kind == FromAgent && msg.Role != RoleUser {
			return fmt.Errorf("%w: a message from an agent has the role user", ErrInvalidRequest)
		}
	}
	if o.budget != nil {
		if err := checkBudgetChange(*o.budget); err != nil {
			return err
		}
	}
	// The message can start a driver. Reserve its place before any write,
	// even while a driver is active, because the driver can idle before the
	// message lands. A follow-up continues a run that this process
	// accepted, so no bound of the backlog refuses the place; the driver
	// waits for a working slot. The launch below moves the place to the
	// driver, and the release gives it back when the active driver takes
	// the message instead.
	if err := a.reserveAdmissionFor(rs); err != nil {
		return err
	}
	content, changes, err := a.keepFiles(a.dirOf(rs), msg.Content)
	if err != nil {
		a.releaseAdmission(rs)
		return err
	}
	for _, d := range changes {
		if _, err := a.emit(nil, rs, EvFileChanged, d); err != nil {
			// appendLocked released the reservation on its failure.
			return err
		}
	}
	// The change and the message are one step, so the driver never reads
	// the message without the budget that came with it.
	rs.emitMu.Lock()
	if o.budget != nil {
		if _, err := a.emitLocked(nil, rs, EvBudgetChanged, BudgetChangedData{Budget: *o.budget, By: subjectOf(ctx), Origin: origin}); err != nil {
			rs.emitMu.Unlock()
			return err
		}
	}
	_, err = a.emitLocked(nil, rs, EvMessageReceived, MessageReceivedData{Role: msg.Role, Content: content, Origin: origin, By: subjectOf(ctx), From: o.from})
	rs.emitMu.Unlock()
	if err != nil {
		return err
	}
	if o.budget != nil {
		a.budgetChanged(rs, *o.budget, origin)
	}
	rs.mu.Lock()
	if !rs.active {
		a.launchLocked(rs)
	}
	rs.mu.Unlock()
	// The launch consumed the place, or the active driver holds the run and
	// the place goes back to the backlog.
	a.releaseAdmission(rs)
	return nil
}

// ChangeBudget changes the budget of a run that this process holds: each
// field that change sets replaces the same field of the budget of the run
// for the work that follows, and a zero field keeps the field as it was. A
// running run reads the new budget at its next check, before its next model
// call or tool calls. A finished run stays finished, and a follow-up starts
// it with the new budget. Every limit counts the whole run, so the working
// time, the turns, the tool calls, the tokens, and the cost before the
// change count against the new limits. The budget of the parent of a child
// run still caps the child.
//
// The kernel records the change as fw.budget.changed, with the subject of
// the principal in ctx. With a principal, the caller needs the operator
// role on the run, as for a decision. ChangeBudget returns the effective
// budget of the run after the change (see RunBudget). A child in another
// process changes its budget in its own process.
func (a *Agent) ChangeBudget(ctx context.Context, id string, change Budget, origin string) (Budget, error) {
	if err := a.k.writeError(); err != nil {
		return Budget{}, err
	}
	rs, err := a.k.lookup(id)
	if err != nil {
		if ref, ok := a.remoteChild(id); ok {
			return Budget{}, fmt.Errorf("%w: run %s runs in %s; change its budget there", ErrInvalidRequest, id, ref.member)
		}
		return Budget{}, err
	}
	if err := a.authorizeRun(ctx, rs, ActionRunOperate); err != nil {
		return Budget{}, err
	}
	if err := rs.drivable(); err != nil {
		return Budget{}, err
	}
	if err := checkBudgetChange(change); err != nil {
		return Budget{}, err
	}
	if _, err := a.emit(nil, rs, EvBudgetChanged, BudgetChangedData{Budget: change, By: subjectOf(ctx), Origin: origin}); err != nil {
		return Budget{}, err
	}
	a.budgetChanged(rs, change, origin)
	return a.budgetOf(rs), nil
}

// checkBudgetChange validates a change of a budget with ErrInvalidRequest.
func checkBudgetChange(b Budget) error {
	if b.negative() {
		return fmt.Errorf("%w: a budget limit is negative", ErrInvalidRequest)
	}
	if b.empty() {
		return fmt.Errorf("%w: a budget change sets at least one field", ErrInvalidRequest)
	}
	return nil
}

// budgetChanged follows a change of the budget of rs: a stop of the tree of
// rs that the old budget caused is over, so a later stop logs again.
func (a *Agent) budgetChanged(rs *runState, change Budget, origin string) {
	a.k.mu.Lock()
	delete(a.k.spent, rs.id)
	a.k.mu.Unlock()
	a.log.Info("run budget changed", "run", rs.id, "origin", origin, "change", BudgetText(change))
}

// subjectOf returns the subject of the principal in ctx, or "".
func subjectOf(ctx context.Context) string {
	if p := PrincipalFrom(ctx); p != nil {
		return p.Subject
	}
	return ""
}

// Decide answers a pending approval. The decider is d.Decider, or the
// principal in ctx.
func (a *Agent) Decide(ctx context.Context, id string, d ApprovalDecision) error {
	if err := a.k.writeError(); err != nil {
		return err
	}
	rs, err := a.k.lookup(id)
	if err != nil {
		if ref, ok := a.remoteChild(id); ok {
			r, err := a.getRemote(ctx, id, ref)
			if err != nil {
				return err
			}
			if err := a.Authorize(ctx, ActionRunOperate, Target{Agent: r.Agent, Run: r}); err != nil {
				return err
			}
			if _, ok := ref.starting(); ok {
				if ref.start.find {
					// A child that a restart interrupted can exist and wait
					// for a decision. Wait until the look for it ends.
					select {
					case <-ref.start.done:
						return a.Decide(ctx, id, d)
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return fmt.Errorf("%w: the run %s has not started in %s yet, so it has no approval to decide", ErrInvalidRequest, id, ref.member)
			}
			return a.spawner.Decide(ctx, ref.member, id, d)
		}
		return err
	}
	if err := a.authorizeRun(ctx, rs, ActionRunOperate); err != nil {
		return err
	}
	if err := rs.drivable(); err != nil {
		return err
	}
	verdict, err := normalizeDecision(d)
	if err != nil {
		return err
	}
	decider := d.Decider
	if decider == "" {
		if p := PrincipalFrom(ctx); p != nil {
			decider = p.Subject
		}
	}
	data := ApprovalDecidedData{ApprovalID: d.ApprovalID, Decision: verdict, Decider: decider, Note: d.Note, Content: d.Content}

	rs.emitMu.Lock()
	rs.mu.Lock()
	p := rs.run.Pending
	rs.mu.Unlock()
	if p == nil || p.ID != d.ApprovalID {
		rs.emitMu.Unlock()
		return fmt.Errorf("%w: run %s, approval %s", ErrNoPendingApproval, id, d.ApprovalID)
	}
	if p.Connect != nil && verdict == "allow" && PrincipalFrom(ctx) != nil {
		// A click is not a grant: only the end of a connection, which
		// decides as the process, lets the call go on.
		rs.emitMu.Unlock()
		return fmt.Errorf("%w: connect %s to continue; a decision can only skip it", ErrInvalidRequest, connectTitle(p.Connect))
	}
	_, err = a.emitLocked(nil, rs, EvApprovalDecided, data)
	rs.emitMu.Unlock()
	if err != nil {
		return err
	}

	rs.mu.Lock()
	defer rs.mu.Unlock()
	if ch, ok := rs.waiters[d.ApprovalID]; ok {
		delete(rs.waiters, d.ApprovalID)
		ch <- data
		return nil
	}
	if !rs.active {
		a.launchLocked(rs)
	}
	return nil
}

func normalizeDecision(d ApprovalDecision) (string, error) {
	switch strings.ToLower(strings.TrimSpace(d.Decision)) {
	case "allow", "approve", "approved", "accept", "yes":
		return "allow", nil
	case "deny", "denied", "reject", "rejected", "decline", "cancel", "no":
		return "deny", nil
	case "":
		if len(d.Content) > 0 {
			return "allow", nil
		}
	}
	return "", fmt.Errorf("%w: a decision is allow or deny, not %q", ErrInvalidRequest, d.Decision)
}

// Cancel cancels a run. It stops the model call or the tools in flight and
// appends fw.run.finished with outcome canceled. Then it cancels every run
// under it that is still open, at any depth, in this process and in other
// processes, also below a child that finished. Cancel on a finished run
// appends nothing to it, and cancels the runs under it that are still
// open. A branch is a run of its own, and Cancel leaves it alone.
func (a *Agent) Cancel(ctx context.Context, id string) error {
	return a.cancelRun(ctx, id, "canceled")
}

// cancelRun is Cancel with the error that fw.run.finished records for the
// run and for each run under it in this process.
func (a *Agent) cancelRun(ctx context.Context, id, reason string) error {
	if err := a.k.storageError(); err != nil {
		return err
	}
	rs, err := a.k.lookup(id)
	if err != nil {
		if ref, ok := a.remoteChild(id); ok {
			r, err := a.getRemote(ctx, id, ref)
			if err != nil {
				return err
			}
			if err := a.Authorize(ctx, ActionRunOperate, Target{Agent: r.Agent, Run: r}); err != nil {
				return err
			}
			if _, ok := ref.starting(); ok {
				if ref.start.find {
					// A child that a restart interrupted can exist. Wait
					// until the look for it ends, then cancel what there is.
					select {
					case <-ref.start.done:
						return a.Cancel(ctx, id)
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				// The start ends, and the child is recorded here as
				// canceled. Wait for that record, so the next Get sees it.
				ref.start.cancel()
				select {
				case <-ref.start.done:
				case <-ctx.Done():
					return ctx.Err()
				}
				return nil
			}
			return a.spawner.Cancel(ctx, ref.member, id)
		}
		return err
	}
	if err := a.authorizeRun(ctx, rs, ActionRunOperate); err != nil {
		return err
	}
	if err := a.cancelOne(rs, reason); err != nil {
		return err
	}
	// A canceled run takes the runs under it along, so no run works for a
	// parent that is gone.
	a.cancelBelow(rs, reason)
	return nil
}

// cancelOne cancels the run rs alone: it stops what the run does and
// appends fw.run.finished with outcome canceled and the reason. A run that
// finished stays as it is.
func (a *Agent) cancelOne(rs *runState, reason string) error {
	rs.emitMu.Lock()
	defer rs.emitMu.Unlock()
	rs.mu.Lock()
	if rs.run.State.Terminal() {
		rs.mu.Unlock()
		return nil
	}
	rs.canceling = true
	cancel := rs.cancel
	rs.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	_, err := a.emitLocked(nil, rs, EvRunFinished, RunFinishedData{Outcome: OutcomeCanceled, Error: reason})
	rs.mu.Lock()
	rs.canceling = false
	rs.mu.Unlock()
	return err
}

// emit appends one event of a run. See emitLocked.
func (a *Agent) emit(guard context.Context, rs *runState, typ string, data any) (Event, error) {
	rs.emitMu.Lock()
	defer rs.emitMu.Unlock()
	return a.emitLocked(guard, rs, typ, data)
}

// emitLocked builds an event, appends it, and publishes it. The caller holds
// rs.emitMu. A guard context that is done refuses the event: a driver passes
// its run context, so a canceled or suspended driver never appends after the
// event that stopped it.
func (a *Agent) emitLocked(guard context.Context, rs *runState, typ string, data any) (Event, error) {
	e, err := NewEvent(rs.id, typ, withTotals(rs, data))
	if err != nil {
		return Event{}, err
	}
	return a.appendLocked(guard, rs, e)
}

// withTotals sets the totals of the run on the data of fw.run.finished and
// fw.run.suspended: the usage of the run as the status holds it before the
// event, which is the projection of every earlier event. The caller holds
// rs.emitMu, so no other event of the run changes the usage meanwhile.
// Every other data passes unchanged. The events that a branch copies from
// its source keep the totals that the source recorded (see appendBranch).
func withTotals(rs *runState, data any) any {
	switch d := data.(type) {
	case RunFinishedData:
		d.Totals = rs.usage()
		return d
	case RunSuspendedData:
		d.Totals = rs.usage()
		return d
	}
	return data
}

// appendLocked is the one path of every event: it sets the time from the
// clock of the agent, Source, and TraceParent, removes secrets from the
// data (see redact.go), appends to the ledger,
// updates the cached status and the in-memory log, and then notifies the
// observers.
func (a *Agent) appendLocked(guard context.Context, rs *runState, e Event) (Event, error) {
	if err := a.k.storageError(); err != nil {
		return Event{}, err
	}
	if guard != nil && guard.Err() != nil {
		return Event{}, errStopped
	}
	// Reserve before writing an event that can start direct work. The
	// reservation transfers to its driver, so concurrent starts cannot
	// overbook the backlog while their ledger writes are in flight. Only
	// new work, fw.run.queued, meets the bound; a follow-up and a decision
	// continue an accepted run and are never refused. The copies of a
	// branch use the place that the branch reserved before its copy.
	if !rs.copying && (e.Type == EvRunQueued || e.Type == EvMessageReceived || e.Type == EvApprovalDecided) {
		if err := a.reserveAdmission(rs, e.Type == EvRunQueued); err != nil {
			return Event{}, err
		}
	}
	e.Time = a.now().UTC()
	e.Source = rs.source
	e.Node = a.node
	e.TraceParent = rs.trace
	e.Data = a.redact.data(e.Data)
	began := time.Now()
	seq, err := a.ledger.Append(context.Background(), e)
	if err != nil {
		err = a.failStorage(storageFailed(fmt.Errorf("fw: append %s to run %s: %w", e.Type, rs.id, err)))
		a.releaseAdmission(rs)
		return Event{}, err
	}
	a.observeAppend(time.Since(began))
	e.Seq = seq
	rs.mu.Lock()
	rs.run.Apply(e, a.prices)
	if IsTerminal(e.Type) && rs.reserved && !rs.copying {
		rs.reserved = false
		a.k.addRootDriver(-1)
	}
	close(rs.changed)
	rs.changed = make(chan struct{})
	rs.mu.Unlock()
	if rs.eventsRef > 0 {
		rs.events = append(rs.events, e)
	}
	a.notify(e)
	if IsTerminal(e.Type) {
		a.releaseRun(rs.id)
	}
	return e, nil
}

// releaseRun tells every tool that keeps state per run (RunCloser) that
// the run ended, so a shell or a session of the run goes. The tools of the
// sources are not asked: a remote server keeps no state of a run here.
func (a *Agent) releaseRun(id string) {
	for _, t := range a.tools {
		if c, ok := runCloserOf(t); ok {
			c.CloseRun(id)
		}
	}
}

// launchLocked starts the driver goroutine of a run. The caller holds rs.mu.
func (a *Agent) launchLocked(rs *runState) {
	k := a.k
	k.lifeMu.Lock()
	if k.draining || k.storageErr != nil {
		k.lifeMu.Unlock()
		if rs.reserved {
			rs.reserved = false
			k.addRootDriver(-1)
		}
		return
	}
	k.wg.Add(1)
	k.lifeMu.Unlock()
	ctx, cancel := context.WithCancel(k.ctx)
	rs.active = true
	rs.cancel, rs.drvCtx = cancel, ctx
	if rs.depth == 0 {
		if !rs.reserved {
			// Recovery resumes work already accepted by the ledger.
			k.addRootDriver(1)
		}
		rs.reserved = false
	}
	go a.runDriver(ctx, cancel, rs)
}

// addRootDriver counts a driver of a top-level run that starts (1) or
// ends (-1), and wakes the bus intake, which takes a job only for a free
// slot of maxRuns (intake.go).
func (k *kernel) addRootDriver(n int) {
	k.slotMu.Lock()
	k.rootDrivers += n
	k.slotsChangedLocked()
	k.slotMu.Unlock()
}

// slotsChangedLocked wakes every wait for a slot. The caller holds slotMu.
func (k *kernel) slotsChangedLocked() {
	close(k.slotsChanged)
	k.slotsChanged = make(chan struct{})
}

// runDriver owns the run until it is idle, canceled, or the kernel drains.
// A driver of a top-level run counts in rootDrivers until it ends, after
// it gave its slot of maxRuns back.
func (a *Agent) runDriver(ctx context.Context, cancel context.CancelFunc, rs *runState) {
	defer a.k.wg.Done()
	if rs.depth == 0 {
		defer a.k.addRootDriver(-1)
	}
	defer cancel()
	reason := a.driverBody(ctx, rs)
	if reason == exitError && a.k.storageError() == nil {
		// The driver stopped on an error that is not a storage fault,
		// such as a broken pipe of a hosted harness or an error of a
		// runner. The ledger works, so only this run fails, and the
		// process goes on. A follow-up can continue the run.
		a.failRun(rs)
	}
	if reason == exitIdle {
		return
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.active = false
	// A follow-up that arrived during a cancel reopens the run, and the old
	// driver hands it to a new one.
	st := rs.run.State
	if reason == exitCanceled && !rs.canceling && !st.Terminal() && st != StateInputRequired && st != StateSuspended {
		a.launchLocked(rs)
	}
}

// failRun records the end of a run whose driver stopped on an error that
// is not a storage fault. The log of the process has the cause. The event
// does not hold it, because the cause can name a path or a peer.
func (a *Agent) failRun(rs *runState) {
	rs.emitMu.Lock()
	defer rs.emitMu.Unlock()
	rs.mu.Lock()
	terminal := rs.run.State.Terminal()
	rs.mu.Unlock()
	if terminal {
		return
	}
	d := RunFinishedData{Outcome: OutcomeFailed, Error: "the driver of the run stopped on an error; the log of the process has the cause"}
	if _, err := a.emitLocked(nil, rs, EvRunFinished, d); err != nil {
		a.log.Error("record the failure of a run driver", "run", rs.id, "error", a.Redact(err.Error()))
	}
}

func (a *Agent) driverBody(ctx context.Context, rs *runState) exitReason {
	// A child run takes no slot of maxRuns: its parent holds one while it
	// waits, and the spawn limits bound the children. Otherwise parents
	// that wait for children could fill every slot and no child could
	// start. A child takes a slot of spawn.maxRunning instead, which a run
	// gives up while it waits for another run (tree.go).
	if rs.depth == 0 {
		select {
		case a.k.sem <- struct{}{}:
		case <-ctx.Done():
			return exitCanceled
		case <-a.k.drainCh:
			return exitDrain
		}
		defer func() { <-a.k.sem }()
	} else if a.k.childSem != nil {
		if !a.takeSlot(ctx) {
			if a.k.isDraining() {
				return exitDrain
			}
			return exitCanceled
		}
		rs.slotMu.Lock()
		rs.held = true
		rs.slotMu.Unlock()
		defer a.dropSlot(rs)
	}
	if err := a.holdEvents(rs); err != nil {
		a.failStorage(storageFailed(fmt.Errorf("fw: load run %s: %w", rs.id, err)))
		return exitError
	}
	defer rs.releaseEvents()
	if a.runner != nil {
		if reason, ok := a.host(ctx, rs); ok {
			return reason
		}
	}
	return a.drive(ctx, rs)
}

// holdEvents loads the log of a run into memory for a driver.
func (a *Agent) holdEvents(rs *runState) error {
	rs.emitMu.Lock()
	defer rs.emitMu.Unlock()
	if rs.eventsRef == 0 {
		evs, err := readAll(context.Background(), a.ledger, rs.id)
		if err != nil {
			return err
		}
		rs.events = evs
		rs.hist, rs.histN = nil, 0
		rs.exec, rs.execN = nil, 0
	}
	rs.eventsRef++
	return nil
}

func (rs *runState) releaseEvents() {
	rs.emitMu.Lock()
	defer rs.emitMu.Unlock()
	rs.eventsRef--
	if rs.eventsRef == 0 {
		rs.events, rs.hist, rs.histN = nil, nil, 0
		rs.exec, rs.execN = nil, 0
	}
}

// eventLog returns the events in memory. The slice never changes, because
// the log only grows.
func (rs *runState) eventLog() []Event {
	rs.emitMu.Lock()
	defer rs.emitMu.Unlock()
	return rs.events[:len(rs.events):len(rs.events)]
}

// historyLocked feeds new events to the incremental history and returns it
// with the estimated tokens of a request that sends it. extra is the
// estimate of the system prompt and the tools of that request. The caller
// holds rs.emitMu.
func (rs *runState) historyLocked(extra int) (History, int) {
	if rs.hist == nil {
		rs.hist, rs.histN = &historyBuilder{}, 0
	}
	for _, e := range rs.events[rs.histN:] {
		rs.hist.apply(e)
	}
	rs.histN = len(rs.events)
	return rs.hist.history(), rs.hist.contextTokens(extra)
}

func (rs *runState) snapshot() *Run {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	r := cloneRun(&rs.run)
	if rs.owner != nil && !r.State.Terminal() {
		if err := rs.owner.storageError(); err != nil {
			r.Blocked = err.Error()
		}
	}
	return r
}

// usage returns a copy of the usage of the run.
func (rs *runState) usage() *RunUsage {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	u := rs.run.Usage
	return &u
}

func (rs *runState) lastSeq() uint64 {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.run.Seq
}

// appliedThrough waits until the run applied the event seq, and returns the
// sequence number of the last event that the run applied. It returns false
// when ctx ends first.
func (rs *runState) appliedThrough(ctx context.Context, seq uint64) (uint64, bool) {
	for {
		rs.mu.Lock()
		cur, changed := rs.run.Seq, rs.changed
		rs.mu.Unlock()
		if cur >= seq {
			return cur, true
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return cur, false
		}
	}
}

// budgetChange returns the fields that the changes of the budget of the
// run set (fw.budget.changed), a later change on top of an earlier one.
func (rs *runState) budgetChange() Budget {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.run.budget
}

// queuedData returns the request of the run as fw.run.queued recorded it.
func (rs *runState) queuedData() RunQueuedData {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.queued
}

// createdAt returns the time of fw.run.queued.
func (rs *runState) createdAt() time.Time {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.run.CreatedAt
}

func cloneRun(r *Run) *Run {
	cp := *r
	cp.Metadata = maps.Clone(r.Metadata)
	if r.Pending != nil {
		p := *r.Pending
		cp.Pending = &p
	}
	if r.TreeUsage != nil {
		u := *r.TreeUsage
		cp.TreeUsage = &u
	}
	if r.Outcome != nil {
		o := *r.Outcome
		cp.Outcome = &o
	}
	cp.Reactions = maps.Clone(r.Reactions)
	return &cp
}

// WithRequest copies every set field of req into the run request. A surface
// uses it to start a run from a decoded request body.
func WithRequest(req RunRequest) RunOption {
	return func(r *RunRequest) {
		if req.ID != "" {
			r.ID = req.ID
		}
		if req.Agent != "" {
			r.Agent = req.Agent
		}
		if req.Input != "" {
			r.Input = req.Input
		}
		if req.Instructions != "" {
			r.Instructions = req.Instructions
		}
		if req.Profile != nil {
			p := *req.Profile
			r.Profile = &p
		}
		if req.Budget != nil {
			b := *req.Budget
			r.Budget = &b
		}
		if len(req.Metadata) > 0 {
			WithMetadata(req.Metadata)(r)
		}
		if req.Origin != "" {
			r.Origin = req.Origin
		}
		if req.PrincipalKind != "" {
			r.PrincipalKind = req.PrincipalKind
		}
		if req.Principal != "" {
			r.Principal = req.Principal
		}
		if req.Parent != "" {
			r.Parent = req.Parent
		}
		if req.IdempotencyKey != "" {
			r.IdempotencyKey = req.IdempotencyKey
		}
		if len(req.Files) > 0 {
			r.Files = append(r.Files, req.Files...)
		}
		if req.Outcome != nil {
			o := *req.Outcome
			r.Outcome = &o
		}
	}
}

func readAll(ctx context.Context, l Ledger, id string) ([]Event, error) {
	var out []Event
	for e, err := range l.Read(ctx, id, 0) {
		if err != nil {
			return nil, fmt.Errorf("fw: read run %s: %w", id, err)
		}
		out = append(out, e)
	}
	return out, nil
}

// checkRunID accepts ids that are safe as a path element and a bus subject
// token: letters, digits, '-', '_', and '.', at most 128 bytes, and no
// leading '.'.
func checkRunID(id string) error {
	if id == "" || len(id) > 128 || id[0] == '.' {
		return fmt.Errorf("%w: invalid run id %q", ErrInvalidRequest, id)
	}
	for _, c := range id {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.'
		if !ok {
			return fmt.Errorf("%w: invalid run id %q", ErrInvalidRequest, id)
		}
	}
	return nil
}

func atespaceFromEnv() string {
	if v := os.Getenv("FW_ATESPACE"); v != "" {
		return v
	}
	return "local"
}

// newTraceParent returns a W3C traceparent with a random trace id and span id.
func newTraceParent() string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return "00-" + hex.EncodeToString(b[:16]) + "-" + hex.EncodeToString(b[16:]) + "-01"
}
