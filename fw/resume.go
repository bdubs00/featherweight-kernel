package fw

// This file rebuilds the run cache from the ledger at start and resumes
// every run that did not finish (docs/ARCHITECTURE.md section 4.5). The turn
// loop finds the point to continue from in the log itself:
//
//   - a fw.model.requested without a response sends the request again;
//   - a fw.tool.requested without a result runs an idempotent tool again, and
//     asks with the reason "rerun after resume" before another tool, unless
//     its Continuer can finish from recorded questions and their answers;
//   - a pending fw.approval.requested waits for Decide.

import (
	"context"
	"fmt"
	"sort"
)

// rebuild loads every run of the ledger into the cache. It returns the runs
// whose state is not terminal, oldest first. The state, not the last event,
// decides: metadata that arrives after fw.run.finished leaves a run
// finished, and a follow-up message opens it again.
func (a *Agent) rebuild(ctx context.Context) ([]*runState, error) {
	k := a.k
	var unfinished []*runState
	for id, err := range a.ledger.Runs(ctx) {
		if err != nil {
			return nil, fmt.Errorf("fw: list runs: %w", err)
		}
		evs, err := readAll(ctx, a.ledger, id)
		if err != nil {
			return nil, err
		}
		if len(evs) == 0 {
			continue
		}
		var q RunQueuedData
		trace := ""
		var remote []RunChildData
		for _, e := range evs {
			switch e.Type {
			case EvRunQueued:
				_ = e.Decode(&q)
			case EvRunChild:
				var d RunChildData
				if e.Decode(&d) == nil && d.Child != "" && d.Member != "" {
					remote = append(remote, d)
				}
			}
			if trace == "" {
				trace = e.TraceParent
			}
		}
		rs := k.newRunState(id, q.Agent, trace)
		rs.parent, rs.depth, rs.queued = q.Parent, q.Depth, q
		rs.run = *ProjectRun(evs, a.prices)
		k.mu.Lock()
		k.runs[id] = rs
		if q.Parent != "" {
			k.kids[q.Parent]++
		}
		for _, d := range remote {
			k.remote[d.Child] = childRef{parent: id, member: d.Member, depth: q.Depth + 1}
			k.kids[id]++
		}
		if key := q.Metadata["idempotencyKey"]; key != "" {
			k.idem[idemKey(q.Principal, key)] = id
		}
		k.mu.Unlock()
		if !rs.run.State.Terminal() {
			unfinished = append(unfinished, rs)
		}
	}
	sort.SliceStable(unfinished, func(i, j int) bool {
		return unfinished[i].run.CreatedAt.Before(unfinished[j].run.CreatedAt)
	})
	return unfinished, nil
}

// resume appends fw.run.resumed to each unfinished run and starts its driver.
// A run that waits for a decision stays idle until Decide.
func (a *Agent) resume(runs []*runState) {
	for _, rs := range runs {
		from := rs.lastSeq()
		if _, err := a.emit(nil, rs, EvRunResumed, RunResumedData{FromSeq: from}); err != nil {
			a.log.Error("resume run", "run", rs.id, "error", err)
			continue
		}
		a.log.Info("run resumed", "run", rs.id, "fromSeq", from)
		rs.mu.Lock()
		if rs.run.State != StateInputRequired {
			a.launchLocked(rs)
		}
		rs.mu.Unlock()
	}
}
