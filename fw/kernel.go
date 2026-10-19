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
	// Config.Spawn.MaxDepth, or when the parent already started
	// Config.Spawn.MaxChildren children.
	ErrSpawnLimit = errors.New("fw: spawn limit reached")
)

// OriginKernel marks a message that the kernel adds itself, such as the
// continuation message after a max_tokens stop.
const OriginKernel = "kernel"

const (
	defaultMaxRuns   = 16 // when Config.MaxRuns is 0 outside ax; a Task runs one at a time
	defaultMaxTokens = 64000
)

// kernel holds the runtime state of the agent: the run cache, the drivers of
// active runs, and the concurrency limit.
type kernel struct {
	atespace  string
	compactAt int // compaction.at: estimated tokens of a request that start a compaction

	mu        sync.Mutex
	runs      map[string]*runState
	idem      map[string]string       // idempotency key to run id
	overrides map[string]*runOverride // per-run tools and policy, for replay
	kids      map[string]int          // child runs started by each run, in its life
	remote    map[string]childRef     // the children that a Spawner runs in other processes

	sem chan struct{} // one slot per active run, Config.MaxRuns slots

	lifeMu   sync.Mutex
	draining bool
	drainCh  chan struct{} // closed when shutdown starts
	closedCh chan struct{} // closed when shutdown ends
	wg       sync.WaitGroup

	ctx    context.Context // parent of every run context
	cancel context.CancelFunc

	once       sync.Once
	intakeStop func()
}

// runOverride replaces the tools, the policy, and the brief of one run.
type runOverride struct {
	wrap   ToolMiddleware
	policy Policy
	brief  *string // the brief of the recorded run, for a replay
	zone   *string // the time zone of the recorded run, for a replay
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

	emitMu    sync.Mutex
	events    []Event // the full log while a driver holds a reference
	eventsRef int
	hist      *historyBuilder // incremental history over events
	histN     int             // number of events that hist consumed

	mu        sync.Mutex
	run       Run
	changed   chan struct{} // closed and replaced on every event
	active    bool          // a driver goroutine owns the run
	cancel    context.CancelFunc
	canceling bool
	waiters   map[string]chan ApprovalDecidedData // questions from tools, by approval id
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
		agent:   agent,
		source:  fmt.Sprintf("fw://%s/%s/%s", k.atespace, agent, id),
		trace:   trace,
		changed: make(chan struct{}),
		waiters: map[string]chan ApprovalDecidedData{},
	}
}

// startKernel opens the runtime, rebuilds projections from the ledger, and
// resumes unfinished runs.
func (a *Agent) startKernel(ctx context.Context) error {
	maxRuns := a.cfg.MaxRuns
	if maxRuns <= 0 {
		maxRuns = defaultMaxRuns
	}
	kctx, cancel := context.WithCancel(context.Background())
	k := &kernel{
		atespace:  atespaceFromEnv(),
		compactAt: a.cfg.Compaction.At,
		runs:      map[string]*runState{},
		idem:      map[string]string{},
		overrides: map[string]*runOverride{},
		kids:      map[string]int{},
		remote:    map[string]childRef{},
		sem:       make(chan struct{}, maxRuns),
		drainCh:   make(chan struct{}),
		closedCh:  make(chan struct{}),
		ctx:       kctx,
		cancel:    cancel,
	}
	a.k = k
	unfinished, err := a.rebuild(ctx)
	if err != nil {
		cancel()
		return err
	}
	a.startEgress()
	if !a.noResume {
		a.resume(unfinished)
	}
	a.startIntake()
	return nil
}

// cancelStarts ends the start of every branch in another process that is
// still open and waits for each record, until ctx ends or five seconds
// pass.
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
				a.log.Error("flush ledger", "error", err)
			}
		}
		a.closeResources()
		close(k.closedCh)
	})
}

// idemKey scopes an idempotency key by principal, so one caller cannot reach
// the run of another through a shared key.
func idemKey(principal, key string) string { return principal + "\x00" + key }

func (k *kernel) isDraining() bool {
	k.lifeMu.Lock()
	defer k.lifeMu.Unlock()
	return k.draining
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
	k := a.k
	if k.isDraining() {
		return nil, ErrClosed
	}
	if strings.TrimSpace(req.Input) == "" && len(req.Files) == 0 {
		return nil, fmt.Errorf("%w: run input is empty", ErrInvalidRequest)
	}
	if req.Agent == "" {
		req.Agent = "default"
	}
	if _, ok := a.profiles()[req.Agent]; !ok && req.Agent != "default" {
		return nil, fmt.Errorf("%w: %s", ErrAgentNotFound, req.Agent)
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
	if req.ID == "" {
		req.ID = NewID()
	} else if err := checkRunID(req.ID); err != nil {
		return nil, err
	}
	if err := a.checkProfile(req.Profile); err != nil {
		return nil, err
	}
	if err := a.checkModel(req); err != nil {
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
	}
	files, changes, err := a.keepFiles(req.Files)
	if err != nil {
		return nil, err
	}
	extra := a.extraTools(ctx, req, parent)
	req.Metadata = inheritBatch(req.Metadata, parentRun)
	meta := a.annotate(ctx, &req, parentRun, req.Metadata)
	if req.IdempotencyKey != "" {
		if meta == nil {
			meta = map[string]string{}
		}
		meta["idempotencyKey"] = req.IdempotencyKey
	}
	ev, err := NewEvent(req.ID, EvRunQueued, RunQueuedData{
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
	})
	if err != nil {
		return nil, err
	}
	rs := k.newRunState(req.ID, req.Agent, "")
	rs.parent, rs.depth = req.Parent, depth
	_ = ev.Decode(&rs.queued)
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
			return prev.snapshot(), nil
		}
	}
	if _, ok := k.runs[req.ID]; ok {
		k.mu.Unlock()
		return nil, fmt.Errorf("fw: run %s already exists", req.ID)
	}
	if parent != nil {
		if max := a.cfg.Spawn.MaxChildren; max > 0 && k.kids[req.Parent] >= max {
			k.mu.Unlock()
			return nil, fmt.Errorf("%w: run %s already started spawn.maxChildren (%d) child runs", ErrSpawnLimit, req.Parent, max)
		}
		k.kids[req.Parent]++
	}
	k.runs[req.ID] = rs
	if req.IdempotencyKey != "" {
		k.idem[idemKey(req.Principal, req.IdempotencyKey)] = req.ID
	}
	k.mu.Unlock()

	rs.emitMu.Lock()
	_, err = a.appendLocked(nil, rs, ev)
	rs.emitMu.Unlock()
	if err != nil {
		k.mu.Lock()
		delete(k.runs, req.ID)
		if parent != nil {
			k.kids[req.Parent]--
		}
		if req.IdempotencyKey != "" {
			delete(k.idem, idemKey(req.Principal, req.IdempotencyKey))
		}
		k.mu.Unlock()
		return nil, err
	}
	for _, d := range changes {
		if _, err := a.emit(nil, rs, EvFileChanged, d); err != nil {
			return nil, err
		}
	}
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
// child run uses it to see the pending approval of the child.
func (a *Agent) WaitFor(ctx context.Context, id string, done func(*Run) bool) (*Run, error) {
	rs, err := a.k.lookup(id)
	if err != nil {
		if ref, ok := a.remoteChild(id); ok {
			if r, ok := ref.starting(); ok {
				// The branch is on its way to its process. Wait for the
				// start to end, then ask again: the branch is then in that
				// process, or recorded here as failed.
				if done(r) {
					return r, nil
				}
				select {
				case <-ref.start.done:
					return a.WaitFor(ctx, id, done)
				case <-ctx.Done():
					return r, ctx.Err()
				}
			}
			r, err := a.spawner.WaitFor(ctx, ref.member, id, func(r *Run) bool {
				a.decorateChild(r, ref)
				return done(r)
			})
			a.decorateChild(r, ref)
			return r, err
		}
		return nil, err
	}
	for {
		rs.mu.Lock()
		cur, ch := cloneRun(&rs.run), rs.changed
		rs.mu.Unlock()
		if done(cur) {
			return cur, nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return rs.snapshot(), ctx.Err()
		case <-a.k.closedCh:
			return rs.snapshot(), ErrClosed
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
	for _, child := range a.k.all() {
		if child.parent == id {
			kids = append(kids, child.snapshot())
		}
	}
	for cid, ref := range a.remoteChildren(id) {
		r, ok := ref.starting()
		if !ok {
			var err error
			if r, err = a.spawner.Get(ctx, ref.member, cid); err != nil {
				a.log.Warn("read a remote child", "run", cid, "member", ref.member, "error", err)
				continue
			}
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
	if p.Budget != nil {
		b := *p.Budget
		if b.MaxTurns < 0 || b.MaxTokens < 0 || b.MaxCostUSD < 0 || b.Deadline < 0 || b.MaxToolCalls < 0 || b.MaxCallsPerTurn < 0 || b.MaxRepeats < 0 {
			return fmt.Errorf("%w: a budget limit is negative", ErrInvalidRequest)
		}
	}
	return nil
}

// checkModel makes sure that a run has a model: the one of the request
// profile, else the one of the agent profile, else the default model of
// the configuration. The kernel has no built-in default, so a process
// without a model refuses the run here, before anything is recorded.
func (a *Agent) checkModel(req RunRequest) error {
	if a.model != nil {
		return nil
	}
	model := ""
	if p, ok := a.ProfileNamed(req.Agent); ok {
		model = p.Model
	}
	if req.Profile != nil && req.Profile.Model != "" {
		model = req.Profile.Model
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
	r := rs.snapshot()
	if err := a.Authorize(ctx, ActionRunView, Target{Agent: r.Agent, Run: r}); err != nil {
		return nil, err
	}
	return r, nil
}

// RunBudget returns the effective budget of a run: its own budget, merged
// with the budget of its profile and of the configuration, and capped by
// the budget of its parent. It authorizes the caller as Get does. A zero
// field has no limit.
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
		runs = append(runs, rs.snapshot())
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
		for e := range in {
			select {
			case out <- e:
			case <-wctx.Done():
				return
			}
			// A follow-up reopens a finished run, so close only when no
			// event follows the terminal event.
			if IsTerminal(e.Type) && rs.lastSeq() <= e.Seq {
				return
			}
		}
	}()
	return out, nil
}

// Send appends a follow-up message to a run. A finished or idle run starts a
// new turn. A run in the middle of a turn sends the message with its next
// model call, after the tool results. A run that waits for an approval holds
// the message until the decision.
func (a *Agent) Send(ctx context.Context, id string, msg Message, origin string) error {
	if a.k.isDraining() {
		return ErrClosed
	}
	rs, err := a.k.lookup(id)
	if err != nil {
		return err
	}
	if err := a.authorizeRun(ctx, rs, ActionRunOperate); err != nil {
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
	content, changes, err := a.keepFiles(msg.Content)
	if err != nil {
		return err
	}
	for _, d := range changes {
		if _, err := a.emit(nil, rs, EvFileChanged, d); err != nil {
			return err
		}
	}
	if _, err := a.emit(nil, rs, EvMessageReceived, MessageReceivedData{Role: msg.Role, Content: content, Origin: origin}); err != nil {
		return err
	}
	rs.mu.Lock()
	if !rs.active {
		a.launchLocked(rs)
	}
	rs.mu.Unlock()
	return nil
}

// Decide answers a pending approval. The decider is d.Decider, or the
// principal in ctx.
func (a *Agent) Decide(ctx context.Context, id string, d ApprovalDecision) error {
	if a.k.isDraining() {
		return ErrClosed
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
				return fmt.Errorf("%w: the branch %s has not started in %s yet, so it has no approval to decide", ErrInvalidRequest, id, ref.member)
			}
			return a.spawner.Decide(ctx, ref.member, id, d)
		}
		return err
	}
	if err := a.authorizeRun(ctx, rs, ActionRunOperate); err != nil {
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
// appends fw.run.finished with outcome canceled. Cancel on a finished run
// does nothing.
func (a *Agent) Cancel(ctx context.Context, id string) error {
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
				// The start ends, and the branch is recorded here as
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
	_, err = a.emitLocked(nil, rs, EvRunFinished, RunFinishedData{Outcome: OutcomeCanceled, Error: "canceled"})
	rs.mu.Lock()
	rs.canceling = false
	rs.mu.Unlock()
	if err != nil {
		return err
	}
	// A canceled run takes its children with it, so no child works for a
	// parent that is gone.
	for _, child := range a.k.all() {
		if child.parent != rs.id {
			continue
		}
		if cerr := a.Cancel(context.Background(), child.id); cerr != nil {
			a.log.Warn("cancel child run", "run", child.id, "parent", rs.id, "error", cerr)
		}
	}
	for cid, ref := range a.remoteChildren(rs.id) {
		if cerr := a.spawner.Cancel(context.Background(), ref.member, cid); cerr != nil {
			a.log.Warn("cancel remote child run", "run", cid, "member", ref.member, "parent", rs.id, "error", cerr)
		}
	}
	return nil
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
	if guard != nil && guard.Err() != nil {
		return Event{}, errStopped
	}
	e.Time = a.now().UTC()
	e.Source = rs.source
	e.Node = a.node
	e.TraceParent = rs.trace
	e.Data = a.redact.data(e.Data)
	began := time.Now()
	seq, err := a.ledger.Append(context.Background(), e)
	if err != nil {
		return Event{}, fmt.Errorf("fw: append %s to run %s: %w", e.Type, rs.id, err)
	}
	a.observeAppend(time.Since(began))
	e.Seq = seq
	rs.mu.Lock()
	rs.run.Apply(e, a.prices)
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
	if k.draining {
		k.lifeMu.Unlock()
		return
	}
	k.wg.Add(1)
	k.lifeMu.Unlock()
	ctx, cancel := context.WithCancel(k.ctx)
	rs.active = true
	rs.cancel = cancel
	go a.runDriver(ctx, cancel, rs)
}

// runDriver owns the run until it is idle, canceled, or the kernel drains.
func (a *Agent) runDriver(ctx context.Context, cancel context.CancelFunc, rs *runState) {
	defer a.k.wg.Done()
	defer cancel()
	reason := a.driverBody(ctx, rs)
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

func (a *Agent) driverBody(ctx context.Context, rs *runState) exitReason {
	// A child run takes no slot: its parent holds one while it waits, and
	// the spawn limits bound the children. Otherwise parents that wait for
	// children could fill every slot and no child could start.
	if rs.depth == 0 {
		select {
		case a.k.sem <- struct{}{}:
		case <-ctx.Done():
			return exitCanceled
		case <-a.k.drainCh:
			return exitDrain
		}
		defer func() { <-a.k.sem }()
	}
	if err := a.holdEvents(rs); err != nil {
		a.log.Error("load run", "run", rs.id, "error", err)
		return exitError
	}
	defer rs.releaseEvents()
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
	return cloneRun(&rs.run)
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
