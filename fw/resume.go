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
// finished, and a follow-up message opens it again. The tree of each run
// comes back with it: its children in this process from their
// fw.run.queued, its children in other processes with their shares from
// fw.run.child, and what those last reported from fw.child.reported, so
// the usage and the size of the tree count again before the first read.
func (a *Agent) rebuild(ctx context.Context) ([]*runState, error) {
	k := a.k
	var unfinished []*runState
	var lost []lostBranch
	reports := map[string]ChildReportedData{}
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
		index := map[string]int{} // the child id to its place in remote
		for _, e := range evs {
			switch e.Type {
			case EvRunQueued:
				_ = e.Decode(&q)
			case EvRunChild:
				var d RunChildData
				if e.Decode(&d) == nil && d.Child != "" && (d.Member != "" || d.spawning()) {
					// A child is recorded before its start and again when
					// it started; the later event only marks the start,
					// and names the member when the first did not.
					if i, ok := index[d.Child]; ok {
						remote[i].Started = remote[i].Started || d.Started
						if remote[i].Member == "" {
							remote[i].Member = d.Member
						}
					} else {
						index[d.Child] = len(remote)
						remote = append(remote, d)
					}
				}
			case EvChildReported:
				var d ChildReportedData
				if e.Decode(&d) == nil && d.Child != "" {
					prev := reports[d.Child]
					d.TreeUsage = d.TreeUsage.atLeast(prev.TreeUsage)
					reports[d.Child] = d
				}
			}
			if trace == "" {
				trace = e.TraceParent
			}
		}
		rs := k.newRunState(id, q.Agent, trace)
		rs.parent, rs.depth, rs.queued = q.Parent, q.Depth, q
		rs.maxDesc = maxDescOf(q.Metadata)
		rs.run = *ProjectRun(evs, a.prices)
		k.mu.Lock()
		k.runs[id] = rs
		if q.Parent != "" {
			k.kids[q.Parent]++
			k.children[q.Parent] = append(k.children[q.Parent], rs)
		}
		for _, d := range remote {
			k.remote[d.Child] = childRef{parent: id, member: d.Member, depth: q.Depth + 1, key: d.Key,
				reserved: d.Reserved, branch: d.Metadata[MetaForkOf] != ""}
			k.kids[id]++
			if d.placed() || d.spawning() {
				lost = append(lost, lostBranch{src: rs, data: d})
			}
		}
		if key := q.Metadata["idempotencyKey"]; key != "" {
			k.idem[idemKey(q.Principal, key)] = id
		}
		k.mu.Unlock()
		if !rs.run.State.Terminal() {
			unfinished = append(unfinished, rs)
		}
	}
	// A branch that never reached its process is recorded here as failed
	// (fork.go), after the source recorded it as a child. The record here
	// wins, so the child is not asked about a run that it does not have.
	k.mu.Lock()
	for id, ref := range k.remote {
		if _, ok := k.runs[id]; ok {
			delete(k.remote, id)
			k.kids[ref.parent]--
		}
	}
	for id, d := range reports {
		if _, ok := k.remote[id]; ok {
			k.reports[id] = &childReport{tree: d.TreeUsage, state: d.State, open: d.Open, rec: d}
		}
	}
	k.mu.Unlock()
	// A branch that was placed in another process but never started when
	// this process went away is lost: no process has it, and nothing will
	// send it. It is recorded here as failed, and its process is abandoned.
	// A child of spawn_task whose start was open can exist, because its
	// spawner was on its way to start it, so this process looks for it
	// (spawn.go).
	for _, l := range lost {
		k.mu.Lock()
		_, pending := k.remote[l.data.Child]
		k.mu.Unlock()
		switch {
		case !pending:
		case l.data.placed():
			a.recordLostBranch(l.src, l.data)
		default:
			a.findChild(l.src, l.data)
		}
	}
	sort.SliceStable(unfinished, func(i, j int) bool {
		return unfinished[i].run.CreatedAt.Before(unfinished[j].run.CreatedAt)
	})
	return unfinished, nil
}

// lostBranch is a branch that its source placed in another process and
// never started, or a child of spawn_task whose start was open, found at a
// rebuild.
type lostBranch struct {
	src  *runState
	data RunChildData
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
