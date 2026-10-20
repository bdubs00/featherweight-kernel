package fw

// This file is the seam of a runner: a linked package that drives the runs
// of an agent in place of the turn loop, such as the host of a third-party
// harness (docs/ARCHITECTURE.md section 19). The kernel still owns the run:
// the runner records every step through the same append path as the turn
// loop, asks the policy of the run, and waits for a decision as an idle
// run, so the budget, the projections, the surfaces, and a suspend work as
// they do for a native run.

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// A Runner drives the runs of an agent in place of the turn loop.
type Runner interface {
	// Drive does the work of the run of h that is left, from the events of
	// the run: it starts the run, continues it after a decision or a
	// follow-up, or picks it up after a restart. It returns when the run
	// is finished, waits for a decision, or ctx ends. The kernel calls
	// Drive again when an event arrived while Drive worked, so Drive must
	// find in the events what is left to do, and record nothing when
	// nothing is left. Drive reports false, before it records any event,
	// for a run that it does not drive; the turn loop then drives that run.
	// An error means that the ledger failed or that ctx ended.
	Drive(ctx context.Context, h *Hosted) (bool, error)
}

// RunnerCtor builds the Runner of an agent, before the kernel resumes the
// runs of the ledger. It returns nil when the configuration asks for no
// runner. An error stops New.
type RunnerCtor func(ctx context.Context, a *Agent) (Runner, error)

var runners struct {
	mu   sync.Mutex
	list []RunnerCtor
}

// RegisterRunner registers a RunnerCtor. A package that a program links in
// calls it in init. New asks each registered constructor in the order of
// registration, unless WithoutDefaults leaves them out, and the first
// Runner that one returns drives the runs of the agent.
func RegisterRunner(c RunnerCtor) {
	runners.mu.Lock()
	defer runners.mu.Unlock()
	runners.list = append(runners.list, c)
}

// newRunner returns the Runner of the first registered constructor that
// gives one, or nil.
func newRunner(ctx context.Context, a *Agent) (Runner, error) {
	runners.mu.Lock()
	list := append([]RunnerCtor(nil), runners.list...)
	runners.mu.Unlock()
	for _, c := range list {
		r, err := c(ctx, a)
		if err != nil {
			return nil, err
		}
		if r != nil {
			return r, nil
		}
	}
	return nil, nil
}

// Runner returns the Runner that drives the runs of the agent, or nil when
// the turn loop drives them.
func (a *Agent) Runner() Runner { return a.runner }

// host lets the Runner drive the run rs and reports false when the Runner
// does not drive it. The caller holds the events of the run (holdEvents).
// Like the turn loop, it releases the run only when no event arrived since
// the latest call of Drive began, so a message or a decision that arrives
// meanwhile is never lost.
func (a *Agent) host(ctx context.Context, rs *runState) (exitReason, bool) {
	// A shutdown ends the work of the runner at once, as it stops a model
	// call in flight; the next start resumes the run from its events.
	dctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-a.k.drainCh:
			cancel()
		case <-dctx.Done():
		}
	}()
	h := &Hosted{a: a, rs: rs}
	for first := true; ; first = false {
		if a.k.isDraining() {
			return exitDrain, true
		}
		if ctx.Err() != nil {
			return exitCanceled, true
		}
		seen := rs.lastSeq()
		handled, err := a.runner.Drive(dctx, h)
		if first && !handled && err == nil {
			return 0, false
		}
		if err != nil {
			if a.k.isDraining() {
				return exitDrain, true
			}
			if errors.Is(err, errStopped) || ctx.Err() != nil {
				return exitCanceled, true
			}
			a.log.Error("runner stopped", "run", rs.id, "error", err)
			return exitError, true
		}
		if a.idleExit(rs, seen) {
			return exitIdle, true
		}
	}
}

// Hosted is a run that a Runner drives. Its methods give the Runner what
// the turn loop uses: the events of the run, the append path, the policy,
// the budget, and the directory of its tools.
type Hosted struct {
	a  *Agent
	rs *runState
}

// Agent returns the agent of the run.
func (h *Hosted) Agent() *Agent { return h.a }

// ID returns the id of the run.
func (h *Hosted) ID() string { return h.rs.id }

// Queued returns the request of the run as fw.run.queued recorded it.
func (h *Hosted) Queued() RunQueuedData { return h.rs.queuedData() }

// Model returns the model selector of the run, such as openai/gpt-5, or ""
// when neither the run nor the configuration names one.
func (h *Hosted) Model() string {
	q := h.rs.queuedData()
	return h.a.selectorOf(q, h.a.profileOf(q))
}

// Events returns the events of the run, oldest first. The slice never
// changes; call Events again to see the events that came after.
func (h *Hosted) Events() []Event { return h.rs.eventLog() }

// Emit appends one event of the run through the append path of the kernel:
// the secrets of the data are replaced, the status and the observers see
// it, and an approval or a terminal event is synced. A ctx that ended
// refuses the event, so a canceled run gets nothing after its end. The
// kernel alone records fw.run.queued.
func (h *Hosted) Emit(ctx context.Context, typ string, data any) (Event, error) {
	if typ == EvRunQueued {
		return Event{}, fmt.Errorf("%w: only the kernel records %s", ErrInvalidRequest, typ)
	}
	return h.a.emit(ctx, h.rs, typ, data)
}

// Finish appends fw.run.finished with d, unless a fw.message.received came
// after the event seq. Then it appends nothing and reports false, because
// the run has a message that the runner did not handle yet; the kernel
// calls Drive again for it. The check and the event are one step, as at
// the end of a turn of the turn loop.
func (h *Hosted) Finish(ctx context.Context, d RunFinishedData, seq uint64) (bool, error) {
	rs := h.rs
	rs.emitMu.Lock()
	defer rs.emitMu.Unlock()
	for _, e := range rs.events {
		if e.Seq > seq && e.Type == EvMessageReceived {
			return false, nil
		}
	}
	if _, err := h.a.emitLocked(ctx, rs, EvRunFinished, d); err != nil {
		return false, err
	}
	return true, nil
}

// Decide asks the policy of the run about a tool call: the policy of the
// agent, made stricter by the rules of the profile of the run and of every
// ancestor, as for a call of the turn loop. RunID and Agent of call are
// set from the run when they are empty.
func (h *Hosted) Decide(ctx context.Context, call ToolCall) Decision {
	rs := h.rs
	if call.RunID == "" {
		call.RunID = rs.id
	}
	if call.Agent == "" {
		call.Agent = rs.agent
	}
	info := h.a.runInfo(rs)
	info.CallID = call.CallID
	pol := h.a.policyFor(rs, h.a.profileOf(rs.queuedData()))
	return pol.Decide(WithRunInfo(WithAgent(ctx, h.a), info), call)
}

// Over reports whether the run reached a limit of its budget before its
// next turn: its turns, its tool calls, the tokens, the cost, and the
// tool calls of its tree and of the tree of each ancestor, and the
// working time of its lineage. It returns the data of fw.budget.exceeded
// and the error of fw.run.finished, such as "budget exceeded: maxTurns".
// The runner records both; a hosted run makes no final report.
func (h *Hosted) Over() (BudgetExceededData, string, bool) {
	o, over := h.a.overBudget(h.rs, analyze(h.rs.eventLog()))
	if !over {
		return BudgetExceededData{}, "", false
	}
	return o.data(), "budget exceeded: " + o.text(h.rs.id), true
}

// Budget returns the effective budget of the run, capped by the budget of
// its parent.
func (h *Hosted) Budget() Budget { return h.a.budgetOf(h.rs) }

// Usage returns the usage of the run so far.
func (h *Hosted) Usage() RunUsage { return *h.rs.usage() }

// Dir returns the directory where the tools of the run work, as an
// absolute path and as a path relative to the root of the workspace, ""
// for the root. With tools.files.perRun it creates the directory of the
// run first, as the first tool call of a run does (section 7.9).
func (h *Hosted) Dir() (abs, rel string, err error) {
	rel = h.a.dirOf(h.rs)
	if rel != "" {
		if _, err := h.a.runWorkspace(rel, nil); err != nil {
			return "", "", err
		}
	}
	return h.a.absDir(rel), rel, nil
}
