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
	defaultFollowEvery = 5 * time.Second  // how often the kernel reads a child that works in another process
	defaultRecordEvery = time.Minute      // how often the usage of such a child enters the ledger of its parent while it grows
	followGiveUp       = 10 * time.Minute // how long a child that does not answer still counts as working
	maxTreeDepth       = 64               // the deepest walk down a tree
)

// childReport is what this process last learned about the tree of a child
// in another process, from the reads of the child and from the
// fw.child.reported events of its parent.
type childReport struct {
	tree     RunUsage          // the most that any read reported, field by field
	state    RunState          // the state of the child at the latest read
	open     int               // Run.TreeOpen of the child at the latest read
	seen     time.Time         // when this process read the child last
	rec      ChildReportedData // the latest fw.child.reported of the child
	recAt    time.Time         // when the kernel appended it
	canceled bool              // a stop of the tree canceled the child
	failedAt time.Time         // the first read that failed since the latest read that worked
	lost     bool              // the process at the member does not know the child
}

// ended reports whether the child and every run under it finished, or
// whether its process lost it, so no read can tell more.
func (r *childReport) ended() bool { return r.lost || (r.state.Terminal() && r.open == 0) }

// working reports whether the child itself works, by its latest read. A
// child that has not answered for followGiveUp no longer counts, so a
// child that went away cannot keep its parent awake for ever.
func (r *childReport) working() bool {
	if r.lost || (!r.failedAt.IsZero() && time.Since(r.failedAt) >= followGiveUp) {
		return false
	}
	return r.state == "" || r.state == StateQueued || r.state == StateRunning
}

// reportLocked returns the report of a child, and makes it when there is
// none. The caller holds k.mu.
func (k *kernel) reportLocked(id string) *childReport {
	rep := k.reports[id]
	if rep == nil {
		rep = &childReport{}
		k.reports[id] = rep
	}
	return rep
}

// recordLocked returns the fw.child.reported event that the latest read
// calls for: once when the tree of the child ended, and at most once every
// interval while it grows. The caller holds k.mu.
func (r *childReport) recordLocked(id, member string, now time.Time, every time.Duration) (ChildReportedData, bool) {
	changed := r.tree != r.rec.TreeUsage || r.open != r.rec.Open
	recEnded := r.rec.State.Terminal() && r.rec.Open == 0
	switch {
	case r.ended() && (changed || !recEnded):
	case changed && now.Sub(r.recAt) >= every:
	default:
		return ChildReportedData{}, false
	}
	r.rec = ChildReportedData{Child: id, Member: member, State: r.state, TreeUsage: r.tree, Open: r.open}
	r.recAt = now
	return r.rec, true
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
type remoteBelow struct {
	tree  RunUsage
	state RunState
	open  int
}

// below returns the children of a run: those in this process, and those in
// other processes with what they last reported. A branch is a run of its
// own and no child in this sense.
func (k *kernel) below(id string) ([]*runState, []remoteBelow) {
	k.mu.Lock()
	defer k.mu.Unlock()
	local := append([]*runState(nil), k.children[id]...)
	var remote []remoteBelow
	for cid, ref := range k.remote {
		if ref.parent != id || ref.branch {
			continue
		}
		rb := remoteBelow{state: StateQueued}
		if rep := k.reports[cid]; rep != nil {
			rb.tree, rb.open = rep.tree, rep.open
			if rep.state != "" {
				rb.state = rep.state
			}
		}
		if _, open := ref.starting(); open {
			rb.state = StateQueued
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
	for _, ref := range k.remote {
		if ref.parent == rs.id && !ref.branch {
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

// treeOf returns the usage of a run and of every run under it that this
// process knows, and the number of the runs under it that did not finish.
// memo, when it is not nil, takes the usage of the tree of each run on the
// way.
func (a *Agent) treeOf(rs *runState, memo map[*runState]RunUsage) (RunUsage, int) {
	return a.treeWalk(rs, 0, memo)
}

func (a *Agent) treeWalk(rs *runState, depth int, memo map[*runState]RunUsage) (RunUsage, int) {
	u := *rs.usage()
	open := 0
	if depth < maxTreeDepth {
		local, remote := a.k.below(rs.id)
		for _, c := range local {
			cu, co := a.treeWalk(c, depth+1, memo)
			u, open = u.plus(cu), open+co
			if !c.state().Terminal() {
				open++
			}
		}
		for _, r := range remote {
			u, open = u.plus(r.tree), open+r.open
			if !r.state.Terminal() {
				open++
			}
		}
	}
	if memo != nil {
		memo[rs] = u
	}
	return u, open
}

// report returns the status of a run with the usage of its tree.
func (a *Agent) report(rs *runState) *Run { return a.withTree(rs, rs.snapshot()) }

// withTree sets TreeUsage and TreeOpen on r, the status of rs, when rs
// started a child.
func (a *Agent) withTree(rs *runState, r *Run) *Run {
	if r == nil || !a.hasTree(rs) {
		return r
	}
	u, open := a.treeOf(rs, nil)
	r.TreeUsage, r.TreeOpen = &u, open
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
// without an allowance stays zero, which is no limit. The caller checks
// spent first, so every allowance here is above zero.
func (t treeRoom) budget() Budget {
	var b Budget
	if t.tokens.set {
		b.MaxTokens = max(int(t.tokens.left), 1)
	}
	if t.cost.set {
		b.MaxCostUSD = t.cost.left
	}
	if t.calls.set {
		b.MaxToolCalls = max(int(t.calls.left), 1)
	}
	return b
}

// treeSpent reports a limit that the tree of rs or of one of its
// ancestors used up.
func (a *Agent) treeSpent(rs *runState) (overrun, bool) { return a.roomOf(rs).spent() }

// limitSpent reports a limit that the tree shares and that rs, its tree,
// or the tree of one of its ancestors used up. A run that reached one
// starts no child.
func (a *Agent) limitSpent(rs *runState) (overrun, bool) {
	if o, ok := ownSpent(rs, a.budgetOf(rs), rs.usage().ToolCalls); ok {
		return o, true
	}
	return a.treeSpent(rs)
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
	byParent := map[string][]childRef{}
	for _, ref := range k.remote {
		if !ref.branch {
			byParent[ref.parent] = append(byParent[ref.parent], ref)
		}
	}
	var count func(r *runState, depth int) int
	count = func(r *runState, depth int) int {
		total := 0
		for _, ref := range byParent[r.id] {
			total += max(1, ref.reserved)
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

// stopTree cancels the open runs under the run whose tree used up a limit:
// its children in this process with the runs under them, and its children
// in other processes. The run itself stops at its next step with
// fw.budget.exceeded. Each run stops its tree once.
func (a *Agent) stopTree(o overrun) {
	k := a.k
	k.mu.Lock()
	if k.spent[o.owner.id] {
		k.mu.Unlock()
		return
	}
	k.spent[o.owner.id] = true
	k.mu.Unlock()
	a.log.Warn("run tree used up a limit; canceling the open runs under it", "run", o.owner.id, "limit", o.limit, "value", o.value, "max", o.max)
	a.cancelUnder(o.owner, "canceled: "+o.why())
}

// cancelUnder cancels the open runs under rs. A child that is open takes
// the runs under it along (Cancel); the walk goes on below a child that
// finished.
func (a *Agent) cancelUnder(rs *runState, reason string) {
	for _, c := range a.k.localChildren(rs.id) {
		if c.state().Terminal() {
			a.cancelUnder(c, reason)
			continue
		}
		if err := a.cancelRun(context.Background(), c.id, reason); err != nil {
			a.log.Warn("cancel a run of a tree that used up its budget", "run", c.id, "error", err)
		}
	}
	var wg sync.WaitGroup
	for id, ref := range a.remoteChildren(rs.id) {
		if ref.branch {
			continue
		}
		if _, open := ref.starting(); open {
			if !ref.start.find {
				ref.start.cancel()
			}
			continue
		}
		a.k.mu.Lock()
		rep := a.k.reports[id]
		skip := rep != nil && (rep.ended() || rep.canceled)
		if rep != nil {
			rep.canceled = true
		}
		a.k.mu.Unlock()
		if skip {
			continue
		}
		wg.Add(1)
		go func(id string, ref childRef) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := a.spawner.Cancel(ctx, ref.member, id); err != nil {
				a.log.Warn("cancel a child in another process of a tree that used up its budget", "run", ref.parent, "child", id, "member", ref.member, "error", err)
			}
		}(id, ref)
	}
	wg.Wait()
}

// observe takes a read of a child in another process: from now on the
// usage of its tree counts toward its parent and every ancestor here. When
// the read shows that the tree of the child ended, or at most once every
// recordEvery while it grows, the kernel records the usage on the parent
// (fw.child.reported), so a restart of the parent knows it. Then the kernel
// checks the trees of the lineage, and stops a tree that used up a limit.
func (a *Agent) observe(id string, ref childRef, r *Run) {
	if r == nil || r.ID != id || ref.branch || a.k == nil {
		return
	}
	tree := r.Usage
	if r.TreeUsage != nil {
		tree = *r.TreeUsage
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
	rep.state, rep.open, rep.seen = r.State, r.TreeOpen, now
	rep.failedAt, rep.lost = time.Time{}, false
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
// followEvery, the kernel reads each child whose tree has not ended and
// that no wait read in that time, so a parent counts the spend of a child
// that nobody waits for, for example one that spawn_task started with
// wait false.
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

// followOnce reads each child in another process that is due.
func (a *Agent) followOnce(ctx context.Context) {
	k := a.k
	type due struct {
		id  string
		ref childRef
	}
	var list []due
	now := time.Now()
	k.mu.Lock()
	for id, ref := range k.remote {
		if ref.branch {
			continue
		}
		if _, open := ref.starting(); open {
			continue
		}
		if rep := k.reports[id]; rep != nil && (rep.ended() || now.Sub(rep.seen) < a.followEvery) {
			continue
		}
		list = append(list, due{id, ref})
	}
	k.mu.Unlock()
	for _, d := range list {
		if k.isDraining() {
			return
		}
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		r, err := a.spawner.Get(rctx, d.ref.member, d.id)
		cancel()
		if err != nil {
			// A child that its process does not know is not read again. Its
			// tree keeps what it last reported.
			k.mu.Lock()
			rep := k.reportLocked(d.id)
			rep.seen = time.Now()
			if errors.Is(err, ErrRunNotFound) {
				rep.lost = true
			} else if rep.failedAt.IsZero() {
				rep.failedAt = rep.seen
			}
			k.mu.Unlock()
			a.log.Debug("read a child in another process", "run", d.ref.parent, "child", d.id, "member", d.ref.member, "error", err)
			continue
		}
		a.decorateChild(r, d.ref)
		a.observe(d.id, d.ref, r)
	}
}

// Following counts the children of spawn_task in other processes that
// work, by the latest read of each: queued or running. The status of the
// process is not idle while one does, because the kernel reads them to
// count their usage toward the budgets of their ancestors (section 5.2),
// so a controller does not put the process to sleep while its children
// spend.
func (a *Agent) Following() int {
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
		if rep := k.reports[id]; rep == nil || rep.working() {
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
