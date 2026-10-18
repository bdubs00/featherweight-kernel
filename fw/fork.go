package fw

// This file branches a run (docs/ARCHITECTURE.md section 4.7). A branch is
// a new run whose ledger begins with a copy of the events of another run,
// up to one sequence number. The branch keeps the conversation of the
// source to that point, and with it the prompt cache prefix of the
// provider, and goes on from there on its own: with a new message, with
// another model, or the same way again. The source does not change.

import (
	"context"
	"fmt"
	"maps"
	"strconv"
)

// Metadata keys of a branch.
const (
	MetaForkOf  = "fw.forkOf"  // the id of the run that the branch left
	MetaForkSeq = "fw.forkSeq" // the sequence number of the last event that the branch kept
)

// ForkRequest describes a branch of a run.
type ForkRequest struct {
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
	// Origin records where the branch came from, such as OriginHTTP.
	Origin string `json:"-"`
}

// Fork branches the run src: a new run whose ledger holds a copy of the
// events of src up to req.Seq, then the message of req.Input when there is
// one. The branch has a new id, the metadata MetaForkOf and MetaForkSeq,
// and no parent; the children of the source stay with the source. The
// person in ctx is the person of the branch. A branch whose kept events end
// a run stays finished unless a message follows them; every other branch
// goes on at once, as a resumed run does: a model request without an
// answer is sent again, a pending approval waits for its decision. The
// caller needs ActionRunOperate on the source and ActionAgentRun on its
// agent.
func (a *Agent) Fork(ctx context.Context, src string, req ForkRequest) (*Run, error) {
	k := a.k
	if k.isDraining() {
		return nil, ErrClosed
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
	evs, err := readAll(ctx, a.ledger, src)
	if err != nil {
		return nil, err
	}
	if len(evs) == 0 || evs[0].Type != EvRunQueued {
		return nil, fmt.Errorf("fw: run %s: %w", src, ErrRunNotFound)
	}
	last := evs[len(evs)-1].Seq
	if req.Seq == 0 {
		req.Seq = last
	}
	if req.Seq > last {
		return nil, fmt.Errorf("%w: the run %s has %d events, so a branch cannot keep %d", ErrInvalidRequest, src, last, req.Seq)
	}

	q := rs.queuedData()
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
	if p := PrincipalFrom(ctx); p != nil && p.Subject != "" {
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
			return nil, err
		}
		q.Profile = &prof
	}

	id := NewID()
	brs := k.newRunState(id, q.Agent, "")
	brs.queued = q
	k.mu.Lock()
	k.runs[id] = brs
	k.mu.Unlock()
	undo := func() {
		k.mu.Lock()
		delete(k.runs, id)
		k.mu.Unlock()
	}
	for _, e := range evs {
		if e.Seq > req.Seq {
			break
		}
		if e.Type == EvRunChild {
			continue
		}
		var ne Event
		if e.Type == EvRunQueued {
			ne, err = NewEvent(id, e.Type, q)
		} else {
			ne, err = NewEvent(id, e.Type, e.Data)
		}
		if err != nil {
			undo()
			return nil, err
		}
		brs.emitMu.Lock()
		_, err = a.appendLocked(nil, brs, ne)
		brs.emitMu.Unlock()
		if err != nil {
			undo()
			return nil, err
		}
	}
	a.log.Info("run branched", "run", id, "from", src, "seq", req.Seq, "withMessage", req.Input != "")
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
	return brs.snapshot(), nil
}
