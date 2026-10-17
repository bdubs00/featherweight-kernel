package fw

// This file rebuilds the run cache from the ledger at start and resumes
// every run that did not finish (docs/ARCHITECTURE.md section 4.5). The turn
// loop finds the point to continue from in the log itself:
//
//   - a fw.model.requested without a response sends the request again;
//   - a fw.tool.requested without a result runs an idempotent tool again, and
//     asks with the reason "rerun after resume" before any other tool;
//   - a pending fw.approval.requested waits for Decide.

import (
	"context"
	"fmt"
	"sort"
)

// rebuild loads every run of the ledger into the cache. It returns the runs
// whose last event is not terminal, oldest first.
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
		for _, e := range evs {
			if e.Type == EvRunQueued {
				_ = e.Decode(&q)
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
		if key := q.Metadata["idempotencyKey"]; key != "" {
			k.idem[idemKey(q.Principal, key)] = id
		}
		k.mu.Unlock()
		if !IsTerminal(evs[len(evs)-1].Type) {
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
