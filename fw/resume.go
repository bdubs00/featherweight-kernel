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
// whose state is not terminal, oldest first, and what recover does once the
// egress runs. The state, not the last event, decides: metadata that
// arrives after fw.run.finished leaves a run finished, and a follow-up
// message opens it again. The tree of each run comes back with it: its
// children in this process from their fw.run.queued, its children in
// other processes with their shares from fw.run.child, and what those last
// reported from fw.child.reported, so the usage and the size of the tree
// count again before the first read.
func (a *Agent) rebuild(ctx context.Context) ([]*runState, recovery, error) {
	k := a.k
	var unfinished []*runState
	var rec recovery
	started := map[string]bool{} // the children in other processes that started there
	reports := map[string]ChildReportedData{}
	for id, err := range a.ledger.Runs(ctx) {
		if err != nil {
			return nil, rec, fmt.Errorf("fw: list runs: %w", err)
		}
		evs, err := readAll(ctx, a.ledger, id)
		if err != nil {
			return nil, rec, err
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
						if d.Ended != "" {
							remote[i].Ended, remote[i].Reason = d.Ended, d.Reason
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
			k.setRemoteLocked(d.Child, childRef{parent: id, member: d.Member, depth: q.Depth + 1, key: d.Key,
				reserved: d.Reserved, branch: d.branch(), known: d.Kind != ""})
			k.kids[id]++
			started[d.Child] = d.Started
			if d.placed() || d.spawning() {
				rec.lost = append(rec.lost, lostBranch{src: rs, data: d})
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
	// A branch or a child that never reached its process is recorded here
	// as failed (fork.go, spawn.go), after the parent recorded it as a
	// child. The record here stands in for it and wins, so the child is not
	// asked about a run that it does not have. A child that started in its
	// process has no such record: a run of this process under its id is not
	// the child, so the child wins, and the run is set aside.
	k.mu.Lock()
	for id, ref := range k.remote {
		rs, ok := k.runs[id]
		if !ok {
			continue
		}
		if rs.parent != "" {
			// Neither a record nor a run that is set aside is a child in
			// the tree of its parent.
			k.kids[rs.parent]--
			k.dropChildLocked(rs.parent, rs)
		}
		if started[id] {
			delete(k.runs, id)
			for key, v := range k.idem {
				if v == id {
					delete(k.idem, key)
				}
			}
			a.log.Error("a run of this process has the id of a child in another process; the child wins", "run", id, "parent", ref.parent, "member", ref.member)
			continue
		}
		rs.standIn = true
		if rs.parent != "" {
			k.records[rs.parent] = append(k.records[rs.parent], rs)
		}
		k.dropRemoteLocked(id)
		k.kids[ref.parent]--
	}
	kept := unfinished[:0]
	for _, rs := range unfinished {
		switch {
		case k.runs[rs.id] != rs:
		case rs.standIn:
			rec.cut = append(rec.cut, rs)
		default:
			kept = append(kept, rs)
		}
	}
	// A child that a kernel of this version recorded starts from what its
	// parent recorded last, or from its start when nothing came after it.
	// A child of an older kernel has no known state, so nothing reads it
	// in the background (followOnce).
	for id, ref := range k.remote {
		if d, ok := reports[id]; ok {
			k.reports[id] = &childReport{tree: d.TreeUsage, state: d.State, open: d.Open, work: d.Working, lost: d.Lost, rec: d, giveUp: k.giveUp}
		} else if ref.known && !ref.branch {
			k.reports[id] = &childReport{state: StateRunning, giveUp: k.giveUp}
		}
	}
	unfinished = kept
	k.mu.Unlock()
	sort.SliceStable(unfinished, func(i, j int) bool {
		return unfinished[i].run.CreatedAt.Before(unfinished[j].run.CreatedAt)
	})
	return unfinished, rec, nil
}

// recovery is what a rebuild leaves for the start of the kernel, which does
// it once the egress runs, so the front of a fleet sees every record that
// it appends.
type recovery struct {
	lost []lostBranch // branches and children whose start was open
	cut  []*runState  // records of a child that a restart cut before their end
}

// recover ends the records that a restart cut, and handles every branch
// and child whose start was open when this process went away. A branch
// that was placed in another process but never started is lost: no process
// has it, and nothing will send it. It is recorded here as failed, and its
// process is abandoned. A child of spawn_task whose start was open can
// exist, because its spawner was on its way to start it, so this process
// looks for it (spawn.go).
func (a *Agent) recover(rec recovery) {
	k := a.k
	for _, rs := range rec.cut {
		if _, err := a.emit(nil, rs, EvRunFinished, RunFinishedData{Outcome: "failed", Error: "the record of a child that never ran lost its end in a restart"}); err != nil {
			a.log.Error("end the record of a child", "run", rs.id, "error", err)
		}
	}
	for _, l := range rec.lost {
		k.mu.Lock()
		_, pending := k.remote[l.data.Child]
		k.mu.Unlock()
		switch {
		case !pending:
		case l.data.placed():
			a.recordLostBranch(l.src, l.data)
		default:
			a.findChild(l.src, l.data, "failed", "this process restarted before the child run started in "+l.data.Member)
		}
	}
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
