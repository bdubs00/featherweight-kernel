package fw

// This file branches a run (docs/ARCHITECTURE.md section 4.7). A branch is
// a new run whose ledger begins with a copy of the events of another run,
// up to one sequence number. The branch keeps the conversation of the
// source to that point, and with it the prompt cache prefix of the
// provider, and goes on from there on its own: with a new message, with
// another model, or the same way again. The source does not change. A
// branch runs in this process, or, with Spawn and a Spawner that is a
// Forker, in another process such as a child Task, where it has a
// workspace of its own.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"time"
)

// Metadata keys of a branch.
const (
	MetaForkOf  = "fw.forkOf"  // the id of the run that the branch left
	MetaForkSeq = "fw.forkSeq" // the sequence number of the last event that the branch kept
)

// ForkRequest describes a branch of a run.
type ForkRequest struct {
	// ID is the id of the branch. Empty gives a new id. The kernel sets it
	// before it asks a Forker to start the branch in another process, so
	// the source knows the branch before that process has it, and the
	// process takes the branch under this id.
	ID string `json:"id,omitempty"`
	// Seq is the sequence number of the last event of the source that the
	// branch keeps. Zero keeps every event.
	Seq uint64 `json:"seq,omitempty"`
	// Input is a message from the person that follows the kept events, so
	// the branch goes on in another direction. Without one, the branch goes
	// on from the kept events as a resumed run does.
	Input string `json:"input,omitempty"`
	// Model is a model selector or a name from models for the branch.
	Model string `json:"model,omitempty"`
	// Metadata adds to the metadata of the branch.
	Metadata map[string]string `json:"metadata,omitempty"`
	// Spawn runs the branch in another process through the Spawner of the
	// agent, which must be a Forker. The source records the branch as a
	// child, so Get and WaitFor reach it there. Fork returns as soon as the
	// process is placed; the branch is queued until that process has it.
	Spawn bool `json:"spawn,omitempty"`
	// Origin records where the branch came from, such as OriginHTTP.
	Origin string `json:"-"`
}

// Fork branches the run src: a new run whose ledger holds a copy of the
// events of src up to req.Seq, then the message of req.Input when there is
// one. The branch has a new id, the metadata MetaForkOf and MetaForkSeq,
// and no parent; the children of the source stay with the source. The
// person in ctx is the person of the branch: the person for whom the
// caller acts (WithActingFor), with the kind KindUser, or else the
// principal. A branch whose kept events end a run stays finished unless a
// message follows them; every other branch goes on at once, as a resumed
// run does: a model request without an answer is sent again, a pending
// approval waits for its decision. The caller needs ActionRunOperate on
// the source and ActionAgentRun on its agent, also when it acts for a
// person.
//
// With req.Spawn, the branch runs in another process: the Forker places it,
// the source records it as a child with its member, and Fork returns the
// branch as queued, with Member set. The start goes on in the background:
// the Forker sends the kept events once the process runs. Until then Get
// answers from this process, WaitFor waits for the start, and Cancel ends
// it. A start that fails, or that Cancel ended, is recorded in this
// process as a failed or a canceled run under the id of the branch, with
// the reason, so nothing is lost without a trace.
func (a *Agent) Fork(ctx context.Context, src string, req ForkRequest) (*Run, error) {
	k := a.k
	// ErrClosed during a drain, ErrStorageUnavailable after a storage
	// failure.
	if err := k.writeError(); err != nil {
		return nil, err
	}
	if err := CheckMetadata(req.Metadata); err != nil {
		return nil, err
	}
	rs, err := k.lookup(src)
	if err != nil {
		return nil, err
	}
	if err := a.authorizeRun(ctx, rs, ActionRunOperate); err != nil {
		return nil, err
	}
	if err := a.Authorize(ctx, ActionAgentRun, Target{Agent: rs.agent}); err != nil {
		return nil, err
	}
	if err := rs.drivable(); err != nil {
		return nil, err
	}
	evs, err := readAll(ctx, a.ledger, src)
	if err != nil {
		return nil, err
	}
	kept, q, err := a.branchOf(ctx, src, rs.queuedData(), evs, &req)
	if err != nil {
		return nil, err
	}
	if err := branchMetadataFits(kept); err != nil {
		return nil, err
	}
	if !req.Spawn {
		return a.appendBranch(q, kept, req)
	}
	forker, ok := a.spawner.(Forker)
	if !ok {
		return nil, fmt.Errorf("%w: no spawner of this process runs a branch in another process; set spawn.driver", ErrInvalidRequest)
	}
	if a.cfg.Spawn.Disabled {
		return nil, fmt.Errorf("%w: spawning is disabled", ErrSpawnLimit)
	}
	depth := rs.depth + 1
	if max := a.cfg.Spawn.MaxDepth; max > 0 && depth > max {
		return nil, fmt.Errorf("%w: a branch at depth %d is deeper than spawn.maxDepth %d", ErrSpawnLimit, depth, max)
	}
	k.mu.Lock()
	if max := a.cfg.Spawn.MaxChildren; max > 0 && k.kids[src] >= max {
		k.mu.Unlock()
		return nil, fmt.Errorf("%w: run %s already started spawn.maxChildren (%d) child runs", ErrSpawnLimit, src, max)
	}
	k.kids[src]++
	k.mu.Unlock()
	// A drain or a storage failure refuses the branch before a process
	// exists for it in the fleet.
	if err := k.writeError(); err != nil {
		k.mu.Lock()
		k.kids[src]--
		k.mu.Unlock()
		return nil, err
	}
	member, err := forker.Place(ctx, rs.snapshot())
	if err != nil || member == "" {
		k.mu.Lock()
		k.kids[src]--
		k.mu.Unlock()
		if err == nil {
			err = errors.New("fw: the spawner placed the branch nowhere")
		}
		return nil, err
	}
	req.ID = NewID()
	placeholder, err := a.placeholder(req.ID, q, a.now())
	if err != nil {
		a.discardPlacedBranch(src, forker, req.ID, member, nil)
		return nil, err
	}
	// The start outlives the request of the caller, and Cancel ends it.
	bctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	start := &branchStart{done: make(chan struct{}), cancel: cancel, run: placeholder}
	ref := childRef{parent: src, member: member, depth: depth, start: start, branch: true}
	a.decorateChild(placeholder, ref)
	placed := RunChildData{Child: req.ID, Member: member, Principal: q.Principal, PrincipalKind: q.PrincipalKind, Metadata: q.Metadata, Kind: childBranch}
	k.lifeMu.Lock()
	if err := k.writeErrorLocked(); err != nil {
		k.lifeMu.Unlock()
		a.discardPlacedBranch(src, forker, req.ID, member, cancel)
		return nil, err
	}
	k.wg.Add(1)
	k.lifeMu.Unlock()
	// The wait ends after the start is registered, so a shutdown that passes
	// the in-flight record still reaches the open start through cancelStarts.
	defer k.wg.Done()
	_, aerr := a.emit(nil, rs, EvRunChild, placed)
	if aerr != nil {
		a.log.Error("record a remote branch", "run", src, "branch", req.ID, "member", member, "error", aerr)
		a.discardPlacedBranch(src, forker, req.ID, member, cancel)
		return nil, aerr
	}
	k.mu.Lock()
	k.setRemoteLocked(req.ID, ref)
	k.mu.Unlock()
	a.log.Info("run branched in another process", "run", src, "branch", req.ID, "member", member, "seq", req.Seq)
	go a.startBranch(bctx, forker, ref, q, kept, req)
	return cloneRun(placeholder), nil
}

// placedAbandonWait bounds the abandon of a process that Place made for a
// branch that this process then gives up (discardPlacedBranch).
const placedAbandonWait = 30 * time.Second

// discardPlacedBranch gives up a branch whose record did not reach the
// ledger before its start began: the source stops counting it, its start
// ends, and the process that Place made holds no run, so it is abandoned
// before Fork returns, for at most placedAbandonWait. The abandon does not
// run in the background, so a process that exits after Fork leaves no
// empty process behind without a record. The record can have reached
// storage anyway, so a restart reconciles what it finds there; this
// process answers for none of it.
func (a *Agent) discardPlacedBranch(src string, ab abandoner, id, member string, cancel context.CancelFunc) {
	if cancel != nil {
		cancel()
	}
	k := a.k
	k.mu.Lock()
	k.kids[src]--
	k.mu.Unlock()
	ctx, done := context.WithTimeout(context.Background(), placedAbandonWait)
	defer done()
	a.abandon(ctx, ab, id, member)
}

// Starting counts the branches and the children of this process whose
// start in another process is still open. The status of the process is not
// idle while one is, so a controller does not put the process to sleep
// before its child has its run. A look for a child after a restart does
// not count: it goes on at the next start, so a sleep loses nothing.
func (a *Agent) Starting() int {
	k := a.k
	k.mu.Lock()
	defer k.mu.Unlock()
	n := 0
	for _, ref := range k.remote {
		if _, ok := ref.starting(); ok && !ref.start.find {
			n++
		}
	}
	return n
}

// startBranch sends a branch to its process and ends its start. A branch
// that the process does not take is recorded here instead, as failed with
// the reason, or as canceled when Cancel ended the start, under the id of
// the branch; the source keeps its fw.run.child event, and the record here
// wins (resume.go).
func (a *Agent) startBranch(ctx context.Context, forker Forker, ref childRef, q RunQueuedData, kept []Event, req ForkRequest) {
	k := a.k
	defer ref.start.cancel()
	child, err := forker.Branch(ctx, ref.member, kept, req)
	switch {
	case err == nil && (child == nil || child.ID != req.ID):
		err = errors.New("fw: the spawner started another run than the branch")
	case err == nil:
		// The source records that the process has the branch, so a restart
		// does not take the branch for a lost one.
		if rs, lerr := k.lookup(ref.parent); lerr == nil {
			if _, err := a.emit(nil, rs, EvRunChild, RunChildData{Child: req.ID, Member: ref.member, Started: true}); err != nil {
				a.log.Warn("record the start of a remote branch", "run", ref.parent, "branch", req.ID, "error", err)
			}
		}
		k.mu.Lock()
		close(ref.start.done)
		k.mu.Unlock()
		a.log.Info("branch started in another process", "branch", req.ID, "member", ref.member)
		return
	}
	outcome, reason := "failed", fmt.Sprintf("the branch did not start in %s: %v", ref.member, err)
	if ctx.Err() != nil {
		outcome, reason = "canceled", "the start of the branch in "+ref.member+" was canceled"
	}
	a.log.Warn("branch did not start in another process", "branch", req.ID, "member", ref.member, "outcome", outcome, "error", err)
	a.recordBranchEnd(req.ID, ref.parent, q, outcome, reason)
	k.mu.Lock()
	close(ref.start.done)
	k.mu.Unlock()
}

// recordBranchEnd records a branch or a child of spawn_task that never
// reached its process as a run of this process with the outcome and the
// reason, under the id of the branch or the child, and stops counting it as
// a child in another process. The record stands in for the run: nothing
// drives it here. A child of spawn_task names its parent in q, so the
// parent lists it, and it does not count toward spawn.maxChildren.
func (a *Agent) recordBranchEnd(id, parent string, q RunQueuedData, outcome, reason string) {
	k := a.k
	q.Kept = 0 // the record holds no event of the source
	brs := k.newRunState(id, q.Agent, "")
	brs.queued = q
	brs.parent, brs.depth, brs.standIn = q.Parent, q.Depth, true
	k.mu.Lock()
	k.runs[id] = brs
	if q.Parent != "" {
		k.records[q.Parent] = append(k.records[q.Parent], brs)
	}
	k.dropRemoteLocked(id)
	k.kids[parent]--
	k.mu.Unlock()
	if _, err := a.emit(nil, brs, EvRunQueued, q); err != nil {
		a.log.Error("record a branch that did not start", "branch", id, "error", err)
	}
	if _, err := a.emit(nil, brs, EvRunFinished, RunFinishedData{Outcome: outcome, Error: reason}); err != nil {
		a.log.Error("record a branch that did not start", "branch", id, "error", err)
	}
}

// recordLostBranch records, after a restart, a branch that this process
// placed in another process but never sent, from the facts in the
// fw.run.child event of its source, and abandons the process. The source is
// the run state of the source run.
func (a *Agent) recordLostBranch(src *runState, d RunChildData) {
	q := src.queuedData()
	q.Parent, q.Depth = "", 0
	q.Principal, q.PrincipalKind, q.Metadata = d.Principal, d.PrincipalKind, d.Metadata
	reason := "this process restarted before " + d.Member + " took the branch"
	a.log.Warn("branch lost in a restart", "branch", d.Child, "member", d.Member, "run", src.id)
	a.recordBranchEnd(d.Child, src.id, q, "failed", reason)
	if forker, ok := a.spawner.(Forker); ok {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			a.abandon(ctx, forker, d.Child, d.Member)
		}()
	}
}

// ForkEvents makes a branch from events that another process kept: the
// fw.run.queued event first, adjusted for the branch, then the rest in
// order. The native API serves it to a trusted caller, so a Forker can
// start a branch on this process. The branch goes on as Fork describes.
func (a *Agent) ForkEvents(ctx context.Context, events []Event, req ForkRequest) (*Run, error) {
	if err := a.k.writeError(); err != nil {
		return nil, err
	}
	if len(events) == 0 || events[0].Type != EvRunQueued {
		return nil, fmt.Errorf("%w: a branch begins with fw.run.queued", ErrInvalidRequest)
	}
	var q RunQueuedData
	if err := events[0].Decode(&q); err != nil {
		return nil, fmt.Errorf("%w: the fw.run.queued event of the branch does not decode: %v", ErrInvalidRequest, err)
	}
	if req.Model != "" {
		prof := AgentProfile{}
		if q.Profile != nil {
			prof = *q.Profile
		}
		prof.Model = req.Model
		q.Profile = &prof
		q.Resolved = a.withModel(q.Resolved, req.Model)
	}
	if err := a.checkProfile(q.Profile); err != nil {
		return nil, err
	}
	if err := a.checkModel(RunRequest{Agent: q.Agent, Profile: q.Profile}); err != nil {
		return nil, err
	}
	if _, ok := a.profiles()[q.Agent]; !ok && q.Agent != "default" {
		return nil, fmt.Errorf("%w: %s", ErrAgentNotFound, q.Agent)
	}
	if err := branchMetadataFits(events); err != nil {
		return nil, err
	}
	if req.Origin != "" {
		q.Origin = req.Origin
	}
	if req.ID != "" {
		if _, ok := IDTime(req.ID); !ok {
			return nil, fmt.Errorf("%w: %q is not an id of a run", ErrInvalidRequest, req.ID)
		}
		if rs, err := a.k.lookup(req.ID); err == nil {
			// The same branch sent again, for example after a lost answer,
			// is the branch that this process has. It waits for the copy of
			// the first send, so a caller holds a branch whose events are
			// in the ledger. Another run under the id is a conflict.
			have := rs.queuedData().Metadata
			if have[MetaForkOf] == q.Metadata[MetaForkOf] && have[MetaForkSeq] == q.Metadata[MetaForkSeq] {
				return rs.accepted(ctx)
			}
			return nil, fmt.Errorf("%w: a run with the id %s exists", ErrInvalidRequest, req.ID)
		}
		if err := a.checkNamedID(ctx, RunRequest{ID: req.ID}); err != nil {
			return nil, err
		}
	}
	return a.appendBranch(q, events, req)
}

// branchOf prepares a branch of src: the kept events and the queued data of
// the branch, with the person in ctx, the metadata of the branch, no
// parent, and the model of the request. It fills req.Seq when it is zero.
// The queued data starts from fw.run.queued as the ledger of src stores
// it, with every secret replaced, and not from q, which this process
// holds as the request gave it. The branch, also in another process,
// therefore reads what the model of src read.
func (a *Agent) branchOf(ctx context.Context, src string, q RunQueuedData, evs []Event, req *ForkRequest) ([]Event, RunQueuedData, error) {
	if len(evs) == 0 || evs[0].Type != EvRunQueued {
		return nil, q, fmt.Errorf("fw: run %s: %w", src, ErrRunNotFound)
	}
	var stored RunQueuedData
	if err := evs[0].Decode(&stored); err != nil {
		return nil, q, fmt.Errorf("fw: run %s: decode fw.run.queued: %w", src, err)
	}
	q = stored
	last := evs[len(evs)-1].Seq
	if req.Seq == 0 {
		req.Seq = last
	}
	if req.Seq > last {
		return nil, q, fmt.Errorf("%w: the run %s has %d events, so a branch cannot keep %d", ErrInvalidRequest, src, last, req.Seq)
	}
	meta := maps.Clone(q.Metadata)
	if meta == nil {
		meta = map[string]string{}
	}
	delete(meta, "idempotencyKey")
	for key, v := range req.Metadata {
		meta[key] = v
	}
	meta[MetaForkOf] = src
	meta[MetaForkSeq] = strconv.FormatUint(req.Seq, 10)
	q.Metadata = meta
	q.Parent, q.Depth = "", 0
	if person := ActingFor(ctx); person != "" {
		// A trusted relay names a person, whose kind is a user.
		q.Principal, q.PrincipalKind = person, KindUser
	} else if p := PrincipalFrom(ctx); p != nil && p.Subject != "" {
		q.Principal, q.PrincipalKind = p.Subject, p.Kind
	}
	if req.Origin != "" {
		q.Origin = req.Origin
	}
	if req.Model != "" {
		prof := AgentProfile{}
		if q.Profile != nil {
			prof = *q.Profile
		}
		prof.Model = req.Model
		if err := a.checkProfile(&prof); err != nil {
			return nil, q, err
		}
		q.Profile = &prof
		q.Resolved = a.withModel(q.Resolved, req.Model)
	}
	kept := make([]Event, 0, len(evs))
	queued := -1
	for _, e := range evs {
		if e.Seq > req.Seq {
			break
		}
		if e.Type == EvRunChild || e.Type == EvChildReported {
			continue // the children belong to the source
		}
		if e.Type == EvRunQueued && queued < 0 {
			queued = len(kept)
		}
		kept = append(kept, e)
	}
	// Kept is the number of the copies, so it leaves out what the branch
	// drops, wherever that sits among the kept events (RunQueuedData.Kept).
	q.Kept = uint64(len(kept))
	if queued >= 0 {
		e := kept[queued]
		ne, err := NewEvent(e.Subject, e.Type, q)
		if err != nil {
			return nil, q, err
		}
		ne.Seq, ne.Time = e.Seq, e.Time
		kept[queued] = ne
	}
	return kept, q, nil
}

// branchMetadataFits checks that the metadata of a branch fits the bounds
// of CheckMetadata: the metadata of its source, with the annotations that
// the branch keeps, and the metadata of the request together. A branch
// past the bounds would fail every later annotation, such as a title.
func branchMetadataFits(kept []Event) error {
	if err := checkMetadataBounds(ProjectRun(kept).Metadata); err != nil {
		return fmt.Errorf("fw: the metadata of the branch: %w", err)
	}
	return nil
}

// withModel returns a copy of the resolved profile of a branch with the
// model of the request. The branch keeps the resolved profile of its source
// in the kept fw.run.queued, so a model of the request must replace the
// model there too. A branch of a run from an older ledger has no resolved
// profile and reads the model from its inline profile.
func (a *Agent) withModel(p *ResolvedProfile, model string) *ResolvedProfile {
	if p == nil {
		return nil
	}
	cp := *p
	cp.Model = a.cfg.ModelSelector(model)
	return &cp
}

// appendBranch writes a branch into this process: a new run state, the kept
// events under the new id, the message of the request, and then the resume
// or the launch of the branch. The copy is the acceptance of the branch: a
// concurrent retry of the same branch waits for it (accepted), and a caller
// that receives a run holds a branch whose events are in the ledger.
func (a *Agent) appendBranch(q RunQueuedData, kept []Event, req ForkRequest) (r *Run, err error) {
	k := a.k
	id := req.ID
	if id == "" {
		id = NewID()
	}
	// Every event of kept is a copy, so Kept is their number, whatever the
	// fw.run.queued of the copies says: the body of ForkEvents comes from
	// another process, and its value never counts.
	q.Kept = uint64(len(kept))
	brs := k.newRunState(id, q.Agent, "")
	brs.queued = q
	brs.maxDesc = maxDescOf(q.Metadata)
	brs.acceptDone = make(chan struct{})
	k.mu.Lock()
	if err := k.claimableLocked(id); err != nil {
		k.mu.Unlock()
		return nil, err
	}
	k.runs[id] = brs
	k.mu.Unlock()
	undo := func() {
		a.releaseAdmission(brs)
		k.mu.Lock()
		delete(k.runs, id)
		k.mu.Unlock()
	}
	k.lifeMu.Lock()
	if err := k.writeErrorLocked(); err != nil {
		k.lifeMu.Unlock()
		undo()
		brs.acceptErr = err
		close(brs.acceptDone)
		return nil, err
	}
	k.wg.Add(1)
	k.lifeMu.Unlock()
	defer k.wg.Done()
	defer func() {
		if err != nil {
			brs.acceptErr = err
		}
		close(brs.acceptDone)
	}()
	// A branch is new work. It takes its place in the backlog before its
	// first append, so a full backlog appends nothing of it, and the
	// copies keep that place, whatever events they hold: a copied
	// fw.run.finished does not give it back, and a copied message or
	// decision takes no other (copying).
	if err := a.reserveAdmission(brs, true); err != nil {
		undo()
		return nil, err
	}
	brs.emitMu.Lock()
	brs.copying = true
	brs.emitMu.Unlock()
	for i, e := range kept {
		data := any(e.Data)
		if i == 0 && e.Type == EvRunQueued {
			var d RunQueuedData
			if err := e.Decode(&d); err != nil {
				undo()
				return nil, fmt.Errorf("fw: the fw.run.queued of the branch: %w", err)
			}
			if d.Kept != q.Kept {
				d.Kept = q.Kept
				data = d
			}
		}
		ne, nerr := NewEvent(id, e.Type, data)
		if nerr != nil {
			undo()
			return nil, nerr
		}
		brs.emitMu.Lock()
		_, aerr := a.appendLocked(nil, brs, ne)
		brs.emitMu.Unlock()
		if aerr != nil {
			undo()
			return nil, aerr
		}
	}
	brs.emitMu.Lock()
	brs.copying = false
	brs.emitMu.Unlock()
	a.log.Info("run branched", "run", id, "from", q.Metadata[MetaForkOf], "seq", q.Metadata[MetaForkSeq], "withMessage", req.Input != "")
	if req.Input != "" {
		if _, err := a.emit(nil, brs, EvMessageReceived, MessageReceivedData{Role: RoleUser, Content: []Block{Text(req.Input)}, Origin: req.Origin}); err != nil {
			return nil, err
		}
		brs.mu.Lock()
		a.launchLocked(brs)
		brs.mu.Unlock()
		return brs.snapshot(), nil
	}
	brs.mu.Lock()
	terminal := brs.run.State.Terminal()
	brs.mu.Unlock()
	if !terminal {
		a.resume([]*runState{brs})
	}
	// A branch that its copies end, and a branch that waits for a
	// decision, start no driver, so the place of the branch goes back to
	// the backlog until a follow-up or the decision starts a driver.
	a.releaseAdmission(brs)
	return brs.snapshot(), nil
}
