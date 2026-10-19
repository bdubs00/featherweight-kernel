package fw

// This file is the tree of runs under one root (docs/ARCHITECTURE.md
// sections 5.2 and 11.9). The runs of a tree share the limits maxTokens,
// maxCostUSD, and maxToolCalls of the budget of each of their ancestors,
// so the usage of a tree is bounded by the budget of its root, however
// many children it has. A run in this process counts as it is now; a
// child in another process counts as it last reported its own tree. The
// file also bounds the number of runs in a tree (spawn.maxDescendants)
// and the child runs that work at the same time (spawn.maxRunning), and
// it reads the children in other processes while they work.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// Intervals of the reads of the children in other processes.
const (
	defaultFollowEvery = 5 * time.Second        // how often the kernel reads a child that works in another process
	defaultRecordEvery = time.Minute            // how often the usage of such a child enters the ledger of its parent while it grows
	followGiveUp       = 10 * time.Minute       // how long a child that does not answer still counts as working
	followReads        = 4                      // the reads or cancels of children that the kernel makes at the same time
	followTimeout      = 10 * time.Second       // how long one read or one cancel of a child gets
	cancelRetryFirst   = 100 * time.Millisecond // the wait before a cancel of a child that failed is tried again
	cancelRetryMax     = time.Minute            // the longest such wait
	maxTreeDepth       = 64                     // the deepest walk down a tree
)

// childReport is what this process last learned about the tree of a child
// in another process, from the reads of the child and from the
// fw.child.reported events of its parent.
type childReport struct {
	tree     RunUsage          // the most that any read reported, field by field
	state    RunState          // the state of the child at the latest read
	open     int               // Run.TreeOpen of the child at the latest read
	work     int               // Run.TreeWorking of the child at the latest read
	seen     time.Time         // when this process read the child last
	rec      ChildReportedData // the latest fw.child.reported of the child
	recAt    time.Time         // when the kernel appended it
	failedAt time.Time         // the first read that failed since the latest read that worked
	// lost is true once this process no longer reaches the child: its
	// process does not know it, or it did not answer a read for
	// followGiveUp. A lost child is never read or canceled again, and it
	// counts in its tree with its last usage alone.
	lost   bool
	giveUp time.Duration // followGiveUp, or what a test sets

	// The cancel of the child by a stop or a cancel of a run above it:
	// canceled is true once a cancel reached the child, and a cancel that
	// failed is tried again from nextCancel on, with a wait that doubles
	// with each failure.
	// since the latest that worked.
	canceled   bool
	cancelWait time.Duration
	nextCancel time.Time
}

// ended reports whether the child and every run under it finished, or
// whether its process lost it, so no read can tell more.
func (r *childReport) ended() bool { return !r.reachable() || (r.state.Terminal() && r.open == 0) }

// reachable reports whether this process still reaches the child: it is
// not lost, and it answered within followGiveUp of its first read that
// failed.
func (r *childReport) reachable() bool {
	return !r.lost && (r.failedAt.IsZero() || time.Since(r.failedAt) < r.grace())
}

// grace is how long the child counts as reachable after its first read or
// cancel that failed.
func (r *childReport) grace() time.Duration {
	if r.giveUp > 0 {
		return r.giveUp
	}
	return followGiveUp
}

// giveUpLocked marks a child lost when its reads have failed for
// followGiveUp, and reports whether it did so now. Only reads count: a
// child that answers reads stays reachable however its cancels fare, so
// its spend keeps counting. The caller holds k.mu.
func (r *childReport) giveUpLocked(now time.Time) bool {
	if r.lost || r.failedAt.IsZero() || now.Sub(r.failedAt) < r.grace() {
		return false
	}
	r.lost = true
	return true
}

// works reports whether a child in the state st, with work runs under it
// that work, works: queued or running, itself or a run under it. A child
// that waits for a person or sleeps does not.
func works(st RunState, work int) bool { return st == StateQueued || st == StateRunning || work > 0 }

// working reports whether the child works by its latest read. Only such a
// child is read in the background (followOnce). A child that has not
// answered for followGiveUp no longer counts, so a child that went away
// cannot keep its parent awake for ever.
func (r *childReport) working() bool { return r.reachable() && works(r.state, r.work) }

// reportLocked returns the report of a child, and makes it when there is
// none. The caller holds k.mu.
func (k *kernel) reportLocked(id string) *childReport {
	rep := k.reports[id]
	if rep == nil {
		rep = &childReport{giveUp: k.giveUp}
		k.reports[id] = rep
	}
	return rep
}

// recordLocked returns the fw.child.reported event that the latest read
// calls for: once when the tree of the child ended, once when this process
// lost the child, once each time the child stops or starts to work, and at
// most once every interval while its usage grows. A restart of the parent
// reads the latest one, so it reads again only a child that worked when
// the parent knew it last, and never a child that it lost. Before any
// event, the child counts as working, as its start said. The caller holds
// k.mu.
func (r *childReport) recordLocked(id, member string, now time.Time, every time.Duration) (ChildReportedData, bool) {
	changed := r.tree != r.rec.TreeUsage || r.open != r.rec.Open || r.work != r.rec.Working
	recEnded := r.rec.Lost || (r.rec.State.Terminal() && r.rec.Open == 0)
	recWorks := !r.rec.Lost && (r.rec.State == "" || works(r.rec.State, r.rec.Working))
	switch {
	case r.lost != r.rec.Lost:
	case r.ended() && (changed || !recEnded):
	case r.working() != recWorks:
	case changed && now.Sub(r.recAt) >= every:
	default:
		return ChildReportedData{}, false
	}
	r.rec = ChildReportedData{Child: id, Member: member, State: r.state, TreeUsage: r.tree, Open: r.open, Working: r.work, Lost: r.lost}
	r.recAt = now
	return r.rec, true
}

// record appends a fw.child.reported event to the parent of a child.
func (a *Agent) record(parent string, d ChildReportedData) {
	p, err := a.k.lookup(parent)
	if err != nil {
		return
	}
	if _, err := a.emit(nil, p, EvChildReported, d); err != nil {
		a.log.Warn("record what a child in another process reported", "run", parent, "child", d.Child, "error", err)
	}
}

// setRemoteLocked records where a remote child lives, and indexes it by
// its parent. The caller holds k.mu.
func (k *kernel) setRemoteLocked(id string, ref childRef) {
	if prev, ok := k.remote[id]; ok && prev.parent != ref.parent {
		delete(k.remoteOf[prev.parent], id)
	}
	k.remote[id] = ref
	if k.remoteOf[ref.parent] == nil {
		k.remoteOf[ref.parent] = map[string]bool{}
	}
	k.remoteOf[ref.parent][id] = true
}

// dropRemoteLocked forgets a remote child, with what it reported. The
// caller holds k.mu.
func (k *kernel) dropRemoteLocked(id string) {
	ref, ok := k.remote[id]
	if !ok {
		return
	}
	delete(k.remote, id)
	delete(k.reports, id)
	if kids := k.remoteOf[ref.parent]; kids != nil {
		delete(kids, id)
		if len(kids) == 0 {
			delete(k.remoteOf, ref.parent)
		}
	}
}

// maxDescOf reads MetaMaxDescendants from the metadata of a run, and
// returns -1 when it sets no limit.
func maxDescOf(meta map[string]string) int {
	v, ok := meta[MetaMaxDescendants]
	if !ok {
		return -1
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return -1
	}
	return n
}

// localChildren returns the child runs of a run in this process.
func (k *kernel) localChildren(id string) []*runState {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]*runState(nil), k.children[id]...)
}

// recordsOf returns the records of the children of a run that never ran
// (recordBranchEnd). They stand in for the children in a listing, and take
// no part in the tree: no usage, no size, no slot, and no share of
// spawn.maxDescendants or spawn.maxChildren.
func (k *kernel) recordsOf(id string) []*runState {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]*runState(nil), k.records[id]...)
}

// dropChildLocked removes a child from the index of its parent. The caller
// holds k.mu.
func (k *kernel) dropChildLocked(parent string, rs *runState) {
	kids := k.children[parent]
	for i, c := range kids {
		if c == rs {
			k.children[parent] = append(kids[:i:i], kids[i+1:]...)
			break
		}
	}
	if len(k.children[parent]) == 0 {
		delete(k.children, parent)
	}
}

// lineageLocked is lineage for a caller that holds k.mu.
func (k *kernel) lineageLocked(rs *runState) []*runState {
	out := []*runState{rs}
	for cur := rs; cur.parent != ""; {
		p := k.runs[cur.parent]
		if p == nil || len(out) > maxTreeDepth {
			break
		}
		out = append(out, p)
		cur = p
	}
	return out
}

// remoteBelow is a child in another process as a walk of the tree sees it.
// A child that this process does not reach (gone) counts with its last
// usage alone, neither open nor working.
type remoteBelow struct {
	tree  RunUsage
	state RunState
	open  int
	work  int
	gone  bool
}

// below returns the children of a run: those in this process, and those in
// other processes with what they last reported. A branch is a run of its
// own and no child in this sense. A child that an older kernel recorded,
// and that no read or record told about since, counts as finished: nothing
// reads it, so nothing that it spends can show here. A child that this
// process lost counts as gone.
func (k *kernel) below(id string) ([]*runState, []remoteBelow) {
	k.mu.Lock()
	defer k.mu.Unlock()
	local := append([]*runState(nil), k.children[id]...)
	var remote []remoteBelow
	for cid := range k.remoteOf[id] {
		ref := k.remote[cid]
		if ref.branch {
			continue
		}
		rb := remoteBelow{state: StateCompleted}
		if ref.known {
			rb.state = StateRunning
		}
		if rep := k.reports[cid]; rep != nil {
			rb.tree, rb.open, rb.work, rb.gone = rep.tree, rep.open, rep.work, !rep.reachable()
			if rep.state != "" {
				rb.state = rep.state
			}
		}
		if _, open := ref.starting(); open {
			rb.state, rb.gone = StateQueued, false
		}
		remote = append(remote, rb)
	}
	return local, remote
}

// hasTree reports whether a run started a child, here or in another
// process.
func (a *Agent) hasTree(rs *runState) bool {
	k := a.k
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.children[rs.id]) > 0 {
		return true
	}
	for id := range k.remoteOf[rs.id] {
		if !k.remote[id].branch {
			return true
		}
	}
	return false
}

// state returns the state of the run.
func (rs *runState) state() RunState {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.run.State
}

// treeStats is the tree of a run as this process knows it: the usage of
// the run and of every run under it, and the runs under it that did not
// finish and that work (queued or running).
type treeStats struct {
	usage         RunUsage
	open, working int
}

// treeOf returns the tree of a run. memo, when it is not nil, takes the
// usage of the tree of each run on the way.
func (a *Agent) treeOf(rs *runState, memo map[*runState]RunUsage) treeStats {
	return a.treeWalk(rs, 0, memo)
}

func (a *Agent) treeWalk(rs *runState, depth int, memo map[*runState]RunUsage) treeStats {
	t := treeStats{usage: *rs.usage()}
	if depth < maxTreeDepth {
		local, remote := a.k.below(rs.id)
		for _, c := range local {
			ct := a.treeWalk(c, depth+1, memo)
			t.usage, t.open, t.working = t.usage.plus(ct.usage), t.open+ct.open, t.working+ct.working
			t.count(c.state(), 0)
		}
		for _, r := range remote {
			t.usage = t.usage.plus(r.tree)
			// Spend is spend, but a child that this process cannot reach,
			// lost or without a spawner, neither works nor stays open here.
			if r.gone || a.spawner == nil {
				continue
			}
			t.open += r.open
			t.count(r.state, r.work)
		}
	}
	if memo != nil {
		memo[rs] = t.usage
	}
	return t
}

// count counts one child in the state st with work runs under it that
// work, as its process reported them.
func (t *treeStats) count(st RunState, work int) {
	if !st.Terminal() {
		t.open++
	}
	if st == StateQueued || st == StateRunning {
		t.working++
	}
	t.working += work
}

// report returns the status of a run with the usage of its tree.
func (a *Agent) report(rs *runState) *Run { return a.withTree(rs, rs.snapshot()) }

// withTree sets TreeUsage, TreeOpen, and TreeWorking on r, the status of
// rs, when rs started a child.
func (a *Agent) withTree(rs *runState, r *Run) *Run {
	if r == nil || !a.hasTree(rs) {
		return r
	}
	t := a.treeOf(rs, nil)
	r.TreeUsage, r.TreeOpen, r.TreeWorking = &t.usage, t.open, t.working
	return r
}

// overrun is a limit of the budget that a run, or the tree of a run,
// reached.
type overrun struct {
	limit string    // the name of the limit, such as maxTokens
	value float64   // the usage that reached it
	max   float64   // the limit
	owner *runState // the run whose budget holds the limit
	tree  bool      // the owner and the runs under it reached it together
}

// shared reports whether the limit is one that the runs of a tree share.
func (o overrun) shared() bool {
	return o.limit == "maxTokens" || o.limit == "maxCostUSD" || o.limit == "maxToolCalls"
}

// amount writes a value of the limit with its unit.
func (o overrun) amount(v float64) string {
	switch o.limit {
	case "maxTokens":
		return fmt.Sprintf("%d tokens", int(v))
	case "maxCostUSD":
		return fmt.Sprintf("$%.4f", v)
	case "maxToolCalls":
		return fmt.Sprintf("%d tool calls", int(v))
	}
	return fmt.Sprintf("%g", v)
}

// text names the limit in the error of the run self. A limit of the run
// alone is its name, as it always was. A limit of a tree says "tree" and
// who used how much.
func (o overrun) text(self string) string {
	if !o.tree {
		return o.limit
	}
	if o.owner.id == self {
		return fmt.Sprintf("%s of the run tree (this run and the runs under it used %s of %s)", o.limit, o.amount(o.value), o.amount(o.max))
	}
	return fmt.Sprintf("%s of the run tree of run %s (it and the runs under it used %s of %s)", o.limit, o.owner.id, o.amount(o.value), o.amount(o.max))
}

// why says what ran out, for a refusal and for the runs that a stop
// cancels.
func (o overrun) why() string {
	if !o.tree {
		return fmt.Sprintf("run %s used %s of its budget.%s of %s", o.owner.id, o.amount(o.value), o.limit, o.amount(o.max))
	}
	return fmt.Sprintf("run %s and the runs under it used %s of its budget.%s of %s, which the whole tree shares", o.owner.id, o.amount(o.value), o.limit, o.amount(o.max))
}

// refusal is the error of a child that cannot start because of o.
func (o overrun) refusal() error { return fmt.Errorf("%w: %s", ErrTreeBudget, o.why()) }

// data is the payload of fw.budget.exceeded for o.
func (o overrun) data() BudgetExceededData {
	d := BudgetExceededData{Limit: o.limit, Value: o.value, Max: o.max}
	if o.tree {
		d.Scope, d.Run = ScopeTree, o.owner.id
	}
	return d
}

// ownSpent reports the first limit that the tree shares and that the run
// alone used up, against b, the effective budget of the run. toolCalls is
// the number of tool calls of the run.
func ownSpent(rs *runState, b Budget, toolCalls int) (overrun, bool) {
	u := *rs.usage()
	switch {
	case b.MaxTokens > 0 && u.tokens() >= b.MaxTokens:
		return overrun{limit: "maxTokens", value: float64(u.tokens()), max: float64(b.MaxTokens), owner: rs}, true
	case b.MaxCostUSD > 0 && u.CostUSD >= b.MaxCostUSD:
		return overrun{limit: "maxCostUSD", value: u.CostUSD, max: b.MaxCostUSD, owner: rs}, true
	case b.MaxToolCalls > 0 && toolCalls >= b.MaxToolCalls:
		return overrun{limit: "maxToolCalls", value: float64(toolCalls), max: float64(b.MaxToolCalls), owner: rs}, true
	}
	return overrun{}, false
}

// allowance is what the tightest tree of a lineage has left of one limit.
type allowance struct {
	set   bool
	left  float64 // max - used; zero or less when the limit is used up
	used  float64
	max   float64
	owner *runState
}

// take keeps the tighter of a and a limit of the tree of owner.
func (a *allowance) take(limit, used float64, owner *runState) {
	if limit <= 0 {
		return
	}
	if left := limit - used; !a.set || left < a.left {
		*a = allowance{set: true, left: left, used: used, max: limit, owner: owner}
	}
}

// treeRoom is what the trees of a lineage have left of the limits that a
// tree shares.
type treeRoom struct {
	tokens, cost, calls allowance
}

// treeLimited reports whether a budget has a limit that a tree shares.
func treeLimited(b Budget) bool { return b.MaxTokens > 0 || b.MaxCostUSD > 0 || b.MaxToolCalls > 0 }

// roomOf returns what the tree of rs and the tree of each of its ancestors
// have left. Each run of the lineage bounds its own tree with its own
// budget (ownBudget), so a child never spends what an ancestor does not
// have. On a tie, the run nearer the root holds the allowance.
func (a *Agent) roomOf(rs *runState) treeRoom {
	line := a.lineage(rs)
	budgets := make([]Budget, len(line))
	limited := false
	for i, r := range line {
		budgets[i] = a.ownBudget(r)
		limited = limited || treeLimited(budgets[i])
	}
	var room treeRoom
	if !limited {
		return room
	}
	memo := map[*runState]RunUsage{}
	a.treeOf(line[len(line)-1], memo)
	for i := len(line) - 1; i >= 0; i-- {
		r, b := line[i], budgets[i]
		u, ok := memo[r]
		if !ok {
			continue
		}
		room.tokens.take(float64(b.MaxTokens), float64(u.tokens()), r)
		room.cost.take(b.MaxCostUSD, u.CostUSD, r)
		room.calls.take(float64(b.MaxToolCalls), float64(u.ToolCalls), r)
	}
	return room
}

// spent returns the first limit of the room that is used up.
func (t treeRoom) spent() (overrun, bool) {
	for _, f := range []struct {
		name string
		a    allowance
	}{{"maxTokens", t.tokens}, {"maxCostUSD", t.cost}, {"maxToolCalls", t.calls}} {
		if f.a.set && f.a.left <= 0 {
			return overrun{limit: f.name, value: f.a.used, max: f.a.max, owner: f.a.owner, tree: true}, true
		}
	}
	return overrun{}, false
}

// budget returns what is left as a budget, to cap a child with. A limit
// without an allowance stays zero, which is no limit. It reports false when
// an allowance is not above zero, because a cap of zero would be no limit:
// a limit of whole units with less than one unit left is used up.
func (t treeRoom) budget() (Budget, bool) {
	var b Budget
	if t.tokens.set {
		if b.MaxTokens = int(t.tokens.left); b.MaxTokens < 1 {
			return Budget{}, false
		}
	}
	if t.cost.set {
		if b.MaxCostUSD = t.cost.left; b.MaxCostUSD <= 0 {
			return Budget{}, false
		}
	}
	if t.calls.set {
		if b.MaxToolCalls = int(t.calls.left); b.MaxToolCalls < 1 {
			return Budget{}, false
		}
	}
	return b, true
}

// treeSpent reports a limit that the tree of rs or of one of its
// ancestors used up.
func (a *Agent) treeSpent(rs *runState) (overrun, bool) { return a.roomOf(rs).spent() }

// limitSpent reports a limit that the tree shares and that rs, its tree,
// or the tree of one of its ancestors used up. A run that reached one
// starts no child.
func (a *Agent) limitSpent(rs *runState) (overrun, bool) {
	_, o, spent := a.spendRoom(rs)
	return o, spent
}

// spendRoom returns what the trees of the lineage of rs have left, from
// one read of the usage, and the limit that rs or a tree used up, if any.
// A child in another process takes its cap from the same read, so the cap
// and the check agree.
func (a *Agent) spendRoom(rs *runState) (treeRoom, overrun, bool) {
	if o, ok := ownSpent(rs, a.budgetOf(rs), rs.usage().ToolCalls); ok {
		return treeRoom{}, o, true
	}
	room := a.roomOf(rs)
	if o, ok := room.spent(); ok {
		return room, o, true
	}
	return room, overrun{}, false
}

// treeRoomLocked checks that the tree of each run of the lineage of parent
// has room for n more runs, and returns the least room that is left, or
// -1 when no limit applies. The limit of a run is MetaMaxDescendants of
// the run, and for the run at the top of the lineage in this process also
// spawn.maxDescendants. A child here counts as one run with the runs under
// it, and a child in another process as the runs that it reserved. The
// caller holds k.mu.
func (a *Agent) treeRoomLocked(parent *runState, n int) (int, error) {
	k := a.k
	line := k.lineageLocked(parent)
	var count func(r *runState, depth int) int
	count = func(r *runState, depth int) int {
		total := 0
		for id := range k.remoteOf[r.id] {
			if ref := k.remote[id]; !ref.branch {
				total += max(1, ref.reserved)
			}
		}
		if depth >= maxTreeDepth {
			return total
		}
		for _, c := range k.children[r.id] {
			total += 1 + count(c, depth+1)
		}
		return total
	}
	left := -1
	for i, r := range line {
		limit := r.maxDesc
		if top := i == len(line)-1; top {
			if m := a.cfg.Spawn.MaxDescendants; m > 0 && (limit < 0 || m < limit) {
				limit = m
			}
		}
		if limit < 0 {
			continue
		}
		have := count(r, 0)
		if have+n > limit {
			return 0, fmt.Errorf("%w: the tree under run %s already holds %d runs, the most that spawn.maxDescendants lets it hold (%d)", ErrSpawnLimit, r.id, have, limit)
		}
		if l := limit - have; left < 0 || l < left {
			left = l
		}
	}
	return left, nil
}

// reserveLocked returns the runs that a new child in another process
// reserves of the tree of parent: the child and the runs that it can
// start under it. left is the room of the tree with the child counted as
// one run (treeRoomLocked), and depth is the depth of the child. The
// parent can start as many more children as spawn.maxChildren leaves it,
// so each one takes an equal share of the room, and a child at the last
// depth takes one. The share travels to the child, and its process holds
// its tree to it, so children that work at the same time in other
// processes never hold more runs together than the tree has. With
// spawn.maxChildren at zero, the number of children is not known, and the
// share is one. The caller holds k.mu.
func (a *Agent) reserveLocked(parent *runState, left, depth int) int {
	if left < 0 {
		return 1
	}
	if m := a.cfg.Spawn.MaxDepth; m > 0 && depth >= m {
		return 1
	}
	slots := left
	if m := a.cfg.Spawn.MaxChildren; m > 0 {
		slots = m - a.k.kids[parent.id]
	}
	if slots < 1 {
		slots = 1
	}
	return max(1, left/slots)
}

// stopTree stops the tree of the run whose tree used up a limit: it
// cancels every open run under it, at any depth, also below a run that
// finished, in this process and in other processes (cancelBelow). The run
// itself stops at its next step with fw.budget.exceeded. The stop lasts:
// the tree stays spent, so each later read or check that finds it spent
// stops it again and cancels what is still open, such as a child whose
// start ended after the stop or a child that a look found after a restart.
// A cancel of a child that fails is tried again, with a wait that doubles
// up to a minute, until it reaches the child, the child counts as lost, or
// the process shuts down.
func (a *Agent) stopTree(o overrun) {
	k := a.k
	reason := "canceled: " + o.why()
	k.mu.Lock()
	first := k.spent[o.owner.id] == ""
	if first {
		k.spent[o.owner.id] = reason
	}
	reason = k.spent[o.owner.id]
	k.mu.Unlock()
	if first {
		a.log.Warn("run tree used up a limit; canceling the open runs under it", "run", o.owner.id, "limit", o.limit, "value", o.value, "max", o.max)
	}
	a.cancelBelow(o.owner, reason)
}

// retryStop cancels the children in other processes under owner again
// until no cancel under it fails, while the process runs. One loop runs
// for each run.
func (a *Agent) retryStop(owner *runState, reason string) {
	k := a.k
	k.mu.Lock()
	if k.retrying[owner.id] {
		k.mu.Unlock()
		return
	}
	k.retrying[owner.id] = true
	k.mu.Unlock()
	a.goTracked(func() {
		defer func() {
			k.mu.Lock()
			delete(k.retrying, owner.id)
			k.mu.Unlock()
		}()
		wait := cancelRetryFirst
		for {
			select {
			case <-time.After(wait):
			case <-k.drainCh:
				return
			}
			if !a.cancelRemote(a.cancelLocalBelow(owner, reason)) {
				return
			}
			wait = min(2*wait, cancelRetryMax)
		}
	})
}

// remoteKid is a child in another process under a run that a cancel walks.
type remoteKid struct {
	id  string
	ref childRef
}

// cancelBelow cancels every open run under rs with the reason: each run of
// this process at any depth, also below a run that finished, at once, and
// each child in another process of each of them, whose process cancels the
// runs under it in turn, in the background. Neither the driver of a run nor
// the caller of Cancel waits for another process. A branch is a run of its
// own and stays. A child whose start is open has its start ended, and the
// start cancels the child if it took its run all the same (SpawnChild); a
// child that a look after a restart looks for is canceled when the look
// finds it (adoptChild). A cancel of a child in another process that fails
// is tried again (retryStop).
func (a *Agent) cancelBelow(rs *runState, reason string) {
	remote := a.cancelLocalBelow(rs, reason)
	if len(remote) == 0 {
		return
	}
	a.goTracked(func() {
		if a.cancelRemote(remote) {
			a.retryStop(rs, reason)
		}
	})
}

// cancelLocalBelow cancels the open runs under rs in this process, and
// returns the children in other processes under rs and under each run of
// this process below it. Without a spawner, it returns none: this process
// cannot reach them, and it says so once (warnNoSpawner).
func (a *Agent) cancelLocalBelow(rs *runState, reason string) []remoteKid {
	k := a.k
	var remote []remoteKid
	var walk func(r *runState, depth int)
	walk = func(r *runState, depth int) {
		for _, c := range k.localChildren(r.id) {
			if err := a.cancelOne(c, reason); err != nil {
				a.log.Warn("cancel a run under a canceled or spent run", "run", c.id, "error", err)
			}
			if depth < maxTreeDepth {
				walk(c, depth+1)
			}
		}
		for id, ref := range a.remoteChildren(r.id) {
			if !ref.branch {
				remote = append(remote, remoteKid{id, ref})
			}
		}
	}
	walk(rs, 0)
	if len(remote) > 0 && a.spawner == nil {
		a.warnNoSpawner()
		return nil
	}
	return remote
}

// cancelRemote cancels the children in other processes that a walk found,
// up to followReads at a time, each within followTimeout and at most until
// the shutdown begins. A child that is canceled, whose tree ended, or that
// counts as lost is not canceled again. After a cancel, the kernel reads
// the child once, so the parent records that the child ended. A child that refuses a
// cancel is read too: a child that answers stays reachable and counted, and
// its cancel is tried again with a wait that doubles up to a minute; only
// reads that fail count toward its grace (followGiveUp), and a child that
// its process does not know is lost. It reports whether a cancel failed
// and waits to be tried again.
func (a *Agent) cancelRemote(remote []remoteKid) bool {
	if len(remote) == 0 || a.spawner == nil {
		return false
	}
	k := a.k
	now := time.Now()
	var due []remoteKid
	pending := false
	k.mu.Lock()
	for _, c := range remote {
		if _, open := c.ref.starting(); open {
			if !c.ref.start.find {
				c.ref.start.cancel()
			}
			continue
		}
		rep := k.reportLocked(c.id)
		switch {
		case rep.canceled || rep.ended():
		case now.Before(rep.nextCancel):
			pending = true
		default:
			due = append(due, c)
		}
	}
	k.mu.Unlock()
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, followReads)
	for _, c := range due {
		wg.Add(1)
		slots <- struct{}{}
		go func(c remoteKid) {
			defer wg.Done()
			defer func() { <-slots }()
			if a.cancelOneRemote(c) {
				mu.Lock()
				pending = true
				mu.Unlock()
			}
		}(c)
	}
	wg.Wait()
	return pending
}

// cancelOneRemote cancels one child in another process, then reads it, and
// reports whether the cancel failed and waits to be tried again.
func (a *Agent) cancelOneRemote(c remoteKid) bool {
	k := a.k
	ctx, cancel := context.WithTimeout(context.Background(), 2*followTimeout)
	defer cancel()
	go func() {
		select {
		case <-k.drainCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	cctx, ccancel := context.WithTimeout(ctx, followTimeout)
	err := a.spawner.Cancel(cctx, c.ref.member, c.id)
	ccancel()
	done := err == nil || errors.Is(err, ErrRunNotFound) // a child that its process does not know cannot spend more
	rctx, rcancel := context.WithTimeout(ctx, followTimeout)
	r, rerr := a.spawner.Get(rctx, c.ref.member, c.id)
	rcancel()
	if rerr == nil {
		a.decorateChild(r, c.ref)
		a.observe(c.id, c.ref, r)
	}
	now := time.Now()
	k.mu.Lock()
	rep := k.reportLocked(c.id)
	var lost ChildReportedData
	recordLost := false
	switch {
	case rerr == nil:
	case errors.Is(rerr, ErrRunNotFound):
		if !rep.lost {
			rep.lost = true
			lost, recordLost = rep.recordLocked(c.id, c.ref.member, now, a.recordEvery)
		}
	case rep.failedAt.IsZero():
		rep.failedAt = now
	}
	if done {
		rep.canceled, rep.cancelWait = true, 0
	} else {
		rep.cancelWait = min(max(2*rep.cancelWait, cancelRetryFirst), cancelRetryMax)
		rep.nextCancel = now.Add(rep.cancelWait)
	}
	wait, retry := rep.cancelWait, !done && !rep.ended()
	k.mu.Unlock()
	if recordLost {
		a.log.Warn("a child in another process is lost: its process does not know it", "run", c.ref.parent, "child", c.id, "member", c.ref.member)
		a.record(c.ref.parent, lost)
	}
	if retry {
		a.log.Warn("cancel a child in another process under a canceled or spent run", "run", c.ref.parent, "child", c.id, "member", c.ref.member, "retryIn", wait, "error", err)
	}
	return retry
}

// warnNoSpawner says once that this process holds children in other
// processes and has no spawner to reach them.
func (a *Agent) warnNoSpawner() {
	a.k.noSpawner.Do(func() {
		a.k.mu.Lock()
		n := len(a.k.remote)
		a.k.mu.Unlock()
		a.log.Warn("this process has no spawner, so it cannot read, follow, or cancel its children in other processes; set spawn.driver to reach them", "children", n)
	})
}

// observe takes a read of a child in another process: from now on the
// usage of its tree counts toward its parent and every ancestor here. The
// child counts at least its own usage, whatever its tree says. When the
// read shows that the tree of the child ended, that the child stopped or
// started to work, or at most once every recordEvery while it grows, the
// kernel records the read on the parent (fw.child.reported), so a restart
// of the parent knows it. Then the kernel checks the trees of the lineage,
// and stops a tree that used up a limit (stopTree).
func (a *Agent) observe(id string, ref childRef, r *Run) {
	if r == nil || r.ID != id || ref.branch || a.k == nil {
		return
	}
	tree := r.Usage
	if r.TreeUsage != nil {
		tree = tree.atLeast(*r.TreeUsage)
	}
	k := a.k
	now := time.Now()
	k.mu.Lock()
	if _, ok := k.remote[id]; !ok {
		k.mu.Unlock()
		return
	}
	rep := k.reportLocked(id)
	rep.tree = rep.tree.atLeast(tree)
	rep.state, rep.open, rep.work, rep.seen = r.State, r.TreeOpen, r.TreeWorking, now
	rep.failedAt, rep.lost = time.Time{}, false
	if !r.State.Terminal() {
		// A child that works again after a cancel, for example after a
		// follow-up in its process, is canceled again by a stop. A child
		// that finished and whose tree winds down is not: its own process
		// cancels the runs under it.
		rep.canceled = false
	}
	d, record := rep.recordLocked(id, ref.member, now, a.recordEvery)
	parent := k.runs[ref.parent]
	k.mu.Unlock()
	if parent == nil {
		return
	}
	if record {
		if _, err := a.emit(nil, parent, EvChildReported, d); err != nil {
			a.log.Warn("record the usage of a child in another process", "run", parent.id, "child", id, "error", err)
		}
	}
	if o, spent := a.treeSpent(parent); spent {
		a.goTracked(func() { a.stopTree(o) })
	}
}

// goTracked runs fn in a goroutine that the shutdown waits for. It does
// nothing once the shutdown began.
func (a *Agent) goTracked(fn func()) {
	k := a.k
	k.lifeMu.Lock()
	if k.draining {
		k.lifeMu.Unlock()
		return
	}
	k.wg.Add(1)
	k.lifeMu.Unlock()
	go func() {
		defer k.wg.Done()
		fn()
	}()
}

// startFollow starts the reads of the children in other processes: every
// followEvery, the kernel reads each child that works, by what this process
// last knew of it, and that no wait read in that time, so a parent counts
// the spend of a child that nobody waits for, for example one that
// spawn_task started with wait false.
func (a *Agent) startFollow() {
	if a.spawner == nil || a.followEvery <= 0 {
		return
	}
	k := a.k
	// A read in flight ends when the shutdown begins.
	ctx, cancel := context.WithCancel(k.ctx)
	go func() {
		select {
		case <-k.drainCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	k.wg.Add(1)
	go func() {
		defer k.wg.Done()
		defer cancel()
		t := time.NewTicker(a.followEvery)
		defer t.Stop()
		for {
			select {
			case <-k.drainCh:
				return
			case <-t.C:
				a.followOnce(ctx)
			}
		}
	}()
}

// followOnce reads each child in another process that is due: a child that
// works, queued or running, itself or a run under it, by its latest read
// or record. A child that waits for a person, that sleeps, or whose tree
// ended is not read: under ax, a read goes through the router, which wakes
// the Task of the child, so reads of a child that does not work would keep
// it awake. Such a child is read again when a wait reads it (spawn_task,
// await_task) or a person reads it through this process. A child that an
// older kernel recorded has no known state and is not read either. Up to
// followReads reads run at the same time.
func (a *Agent) followOnce(ctx context.Context) {
	k := a.k
	type due struct {
		id  string
		ref childRef
	}
	var list []due
	var lost []ChildReportedData
	var parents []string
	now := time.Now()
	k.mu.Lock()
	for id, ref := range k.remote {
		if ref.branch {
			continue
		}
		if _, open := ref.starting(); open {
			continue
		}
		rep := k.reports[id]
		if rep == nil {
			continue
		}
		// A child that did not answer for followGiveUp counts as lost from
		// now on, and the parent records so.
		if rep.giveUpLocked(now) {
			if d, ok := rep.recordLocked(id, ref.member, now, a.recordEvery); ok {
				lost, parents = append(lost, d), append(parents, ref.parent)
			}
		}
		if rep.ended() || !rep.working() || now.Sub(rep.seen) < a.followEvery {
			continue
		}
		list = append(list, due{id, ref})
	}
	k.mu.Unlock()
	for i, d := range lost {
		a.log.Warn("a child in another process did not answer for a while and counts as lost", "run", parents[i], "child", d.Child, "member", d.Member)
		a.record(parents[i], d)
	}
	var wg sync.WaitGroup
	slots := make(chan struct{}, followReads)
	for _, d := range list {
		if k.isDraining() {
			break
		}
		wg.Add(1)
		slots <- struct{}{}
		go func(d due) {
			defer wg.Done()
			defer func() { <-slots }()
			rctx, cancel := context.WithTimeout(ctx, followTimeout)
			r, err := a.spawner.Get(rctx, d.ref.member, d.id)
			cancel()
			if err != nil {
				// A child that its process does not know is lost: it is not
				// read again, it counts in its tree with what it last
				// reported, and the parent records so.
				k.mu.Lock()
				rep := k.reportLocked(d.id)
				rep.seen = time.Now()
				var rec ChildReportedData
				recorded := false
				if errors.Is(err, ErrRunNotFound) {
					rep.lost = true
					rec, recorded = rep.recordLocked(d.id, d.ref.member, rep.seen, a.recordEvery)
				} else if rep.failedAt.IsZero() {
					rep.failedAt = rep.seen
				}
				k.mu.Unlock()
				if recorded {
					a.log.Warn("a child in another process is lost: its process does not know it", "run", d.ref.parent, "child", d.id, "member", d.ref.member)
					a.record(d.ref.parent, rec)
				}
				a.log.Debug("read a child in another process", "run", d.ref.parent, "child", d.id, "member", d.ref.member, "error", err)
				return
			}
			a.decorateChild(r, d.ref)
			a.observe(d.id, d.ref, r)
		}(d)
	}
	wg.Wait()
}

// Following counts the children of spawn_task in other processes that
// work, by the latest read or record of each: queued or running, the child
// itself or a run under it. The status of the process is not idle while
// one does, because the kernel reads them to count their usage toward the
// budgets of their ancestors (section 5.2), so a controller does not put
// the process to sleep while its children spend.
func (a *Agent) Following() int {
	if a.spawner == nil {
		// Nothing reads a child that this process cannot reach.
		return 0
	}
	k := a.k
	k.mu.Lock()
	defer k.mu.Unlock()
	n := 0
	for id, ref := range k.remote {
		if ref.branch {
			continue
		}
		if _, open := ref.starting(); open {
			continue
		}
		if rep := k.reports[id]; rep != nil && rep.working() {
			n++
		}
	}
	return n
}

// takeSlot takes a slot of spawn.maxRunning, and returns false when ctx
// ends or the kernel drains first.
func (a *Agent) takeSlot(ctx context.Context) bool {
	select {
	case a.k.childSem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	case <-a.k.drainCh:
		return false
	}
}

// dropSlot gives back the slot of a driver that ends.
func (a *Agent) dropSlot(rs *runState) {
	rs.slotMu.Lock()
	defer rs.slotMu.Unlock()
	if rs.held {
		rs.held = false
		<-a.k.childSem
	}
	rs.lent = false
}

// waitingRun returns the child run of this process that ctx belongs to,
// or nil: a tool of that run waits.
func (a *Agent) waitingRun(ctx context.Context) *runState {
	if a.k.childSem == nil {
		return nil
	}
	id := RunInfoFrom(ctx).RunID
	if id == "" {
		return nil
	}
	rs, err := a.k.lookup(id)
	if err != nil || rs.depth == 0 {
		return nil
	}
	return rs
}

// park gives up the slot of a child run while it waits: for another run,
// or for the answer of a person. It returns the function that takes a
// slot again when the last wait of the run ends. A run that waits holds no
// slot, so the runs that it waits for can always work, at any depth.
func (a *Agent) park(rs *runState) func() {
	k := a.k
	if k.childSem == nil || rs.depth == 0 {
		return func() {}
	}
	rs.mu.Lock()
	ctx := rs.drvCtx
	rs.mu.Unlock()
	if ctx == nil {
		ctx = k.ctx
	}
	rs.slotMu.Lock()
	rs.parked++
	if rs.held {
		rs.held, rs.lent = false, true
		<-k.childSem
	}
	rs.slotMu.Unlock()
	return func() {
		rs.slotMu.Lock()
		rs.parked--
		if rs.parked > 0 || !rs.lent {
			rs.slotMu.Unlock()
			return
		}
		rs.slotMu.Unlock()
		got := a.takeSlot(ctx)
		rs.slotMu.Lock()
		defer rs.slotMu.Unlock()
		switch {
		case !got:
			// The driver stops and holds no slot.
			rs.lent = false
		case rs.parked > 0 || !rs.lent:
			// Another wait began, or another end of a wait took a slot
			// first, or the driver ended.
			<-k.childSem
		default:
			rs.held, rs.lent = true, false
		}
	}
}
