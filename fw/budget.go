package fw

// This file is the behavior of a run near and at a limit of its budget
// (docs/ARCHITECTURE.md section 5.2): the budget notes that tell the model
// how much of a limit it used, the final report call at a limit of turns,
// tool calls, or working time, the end of a model call at the deadline, and
// the notes about a change of the budget.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// Thresholds of the budget notes, in percent of a limit.
const (
	notePlan   = 75 // the model plans the rest of the work
	noteWrapUp = 90 // the model wraps up and reports
)

var (
	// errPastDeadline is the cause of the end of a model call at the
	// deadline of the working time.
	errPastDeadline = errors.New("fw: the working time reached the deadline")
	// errReportTimeout is the cause of the end of a final report call that
	// took longer than budget.reportTimeout.
	errReportTimeout = errors.New("fw: the final report call took too long")
)

// reportState is the final report of a run that is open: the
// fw.budget.exceeded that started it, its instruction, its call, and its
// answer.
type reportState struct {
	data    BudgetExceededData
	seq     uint64 // the fw.budget.exceeded
	noteSeq uint64 // the instruction (NoteReport)
	reqSeq  uint64 // the fw.model.requested of the call
	done    bool   // the call answered or failed
	fresh   bool   // a follow-up or a change of the budget arrived during the call
	text    string // the text of the answer
}

// noteKey names the subject of a budget note: the limit, its value, and
// the run whose budget holds it when that is not the run of the note.
type noteKey struct {
	limit string
	run   string
	max   float64
}

// budgetNote records a budget note of the ledger in the cursor.
func (c *cursor) budgetNote(seq uint64, d *BudgetNoteData) {
	if d == nil {
		return
	}
	if d.Changed {
		c.changeNote = seq
		return
	}
	if c.noted == nil {
		c.noted = map[noteKey]int{}
	}
	k := noteKey{limit: d.Limit, run: d.Run, max: d.Max}
	if d.Percent > c.noted[k] {
		c.noted[k] = d.Percent
	}
}

// reportInput handles a follow-up message or a change of the budget while
// a final report is open. Before the call, a message goes into the call,
// and a change ends the report, because the next step checks the budget
// again and the change can lift the limit. During the call, the input
// keeps the run open after the answer. After the answer, the report ends,
// and the run goes on with the input as a new step.
func (c *cursor) reportInput(change bool) {
	r := c.rep
	switch {
	case r == nil:
	case r.done:
		c.rep, c.unseen = nil, true
	case r.reqSeq > 0:
		r.fresh = true
	case change:
		c.rep = nil
	}
}

// reportDone records the answer or the failure of a final report call.
func (c *cursor) reportDone(seq uint64, text string) {
	c.reportSeq, c.report = seq, text
	if c.rep == nil {
		return
	}
	c.rep.done, c.rep.text = true, text
	if c.rep.fresh {
		c.rep, c.unseen = nil, true
	}
}

// lastReport returns the text of the final report that the run made since
// the latest change of its budget, or "". A run that stops again at a limit
// without a change keeps that report as its output.
func (c *cursor) lastReport() string {
	if c.reportSeq > c.changeSeq {
		return c.report
	}
	return ""
}

// stopOver ends a run at the limit o and appends fw.budget.exceeded. A run
// that makes its final report (wantsReport) goes on to stepReport, which
// finishes it; every other stop finishes the run as failed at once, after
// the checks of its definition of done (doneAtLimit). No run under the
// owner of a shared limit can spend more, so the open ones stop now
// instead of at their next step.
func (a *Agent) stopOver(ctx context.Context, rs *runState, c *cursor, o overrun) error {
	report := a.wantsReport(ctx, rs, c, o)
	d := o.data()
	d.Report = report
	if _, err := a.emit(ctx, rs, EvBudgetExceeded, d); err != nil {
		return err
	}
	if report {
		// The report call spends nothing of a shared limit, but the runs
		// under this run must not work while it reports.
		if o.shared() {
			a.stopTree(o)
		}
		return nil
	}
	if doneAtLimit(rs, o) {
		// The runs under this run must not work while the checks run.
		if o.shared() {
			a.stopTree(o)
		}
		pass, err := a.limitDone(ctx, rs, c, d, c.lastReport())
		if err != nil {
			return err
		}
		rs.emitMu.Lock()
		defer rs.emitMu.Unlock()
		return a.finishAtLimitLocked(ctx, rs, c.lastReport(), d, pass)
	}
	if err := a.finishAs(ctx, rs, OutcomeFailed, c.lastReport(), "budget exceeded: "+o.text(rs.id), RunStopBudget(o.limit)); err != nil {
		return err
	}
	if o.shared() {
		a.stopTree(o)
	}
	return nil
}

// wantsReport reports whether the stop at o makes the final report call:
// the limit is maxTurns, maxToolCalls, or the deadline, the run holds the
// limit itself, its budget turns the report on, the run made no report
// since the latest change of its budget, and the call fits the tokens and
// the cost that are left (reportFits). A limit of tokens or of cost never
// gets the call, because that call spends past a limit of money. A run
// that stops at a limit that an ancestor holds for the tree stops at once:
// the ancestor reports for the tree at its own next step.
func (a *Agent) wantsReport(ctx context.Context, rs *runState, c *cursor, o overrun) bool {
	switch o.limit {
	case "maxTurns", "maxToolCalls", "deadline":
	default:
		return false
	}
	if o.tree && o.owner != rs {
		return false
	}
	if c.reportSeq > c.changeSeq {
		return false
	}
	b := a.budgetOf(rs)
	if !b.reportOn() {
		return false
	}
	if why, ok := a.reportFits(ctx, rs, c, b); !ok {
		a.log.Info("no final report, because the call can pass a limit of money", "run", rs.id, "limit", o.limit, "reason", why)
		return false
	}
	return true
}

// reportFits reports whether the final report call fits what the run and
// the trees of its lineage have left of maxTokens and maxCostUSD, so the
// call never spends past a limit of money. The bound is the most that the
// call can count: the whole context as input, which the budget counts when
// no cache serves it, and the output cap. When it does not fit, it says
// why.
func (a *Agent) reportFits(ctx context.Context, rs *runState, c *cursor, b Budget) (string, bool) {
	prof := a.profileOf(c.queued)
	req, _, err := a.callRequest(ctx, rs, c, prof)
	if err != nil {
		return "the kernel failed to build the call: " + err.Error(), false
	}
	rs.emitMu.Lock()
	_, in := rs.historyLocked((len(req.System) + len(c.context) + jsonSize(req.Tools)) / bytesPerToken)
	rs.emitMu.Unlock()
	out := b.reportTokens()
	u := *rs.usage()
	room := a.roomOf(rs)
	tokensLeft, costLeft := math.Inf(1), math.Inf(1)
	if b.MaxTokens > 0 {
		tokensLeft = float64(b.MaxTokens - u.tokens())
	}
	if room.tokens.set {
		tokensLeft = min(tokensLeft, room.tokens.left)
	}
	if need := in + out; float64(need) > tokensLeft {
		return fmt.Sprintf("the call can count about %d tokens, and %d are left (budget.maxTokens)", need, int(tokensLeft)), false
	}
	if b.MaxCostUSD > 0 {
		costLeft = b.MaxCostUSD - u.CostUSD
	}
	if room.cost.set {
		costLeft = min(costLeft, room.cost.left)
	}
	provider, name := SplitSelector(a.selectorOf(c.queued, prof))
	if p, ok := priceOf(a.prices, provider, name); ok && !math.IsInf(costLeft, 1) {
		if cost := Cost(Usage{InputTokens: in, OutputTokens: out}, p); cost > costLeft {
			return fmt.Sprintf("the call can cost about $%.4f, and $%.4f is left (budget.maxCostUSD)", cost, costLeft), false
		}
	}
	return "", true
}

// stepReport makes the final report call of a run that reached a limit,
// or finishes the run once that call answered or failed. The call has the
// prefix of a turn, so it reads the prompt cache of the run, and the
// history with the instruction of the report. Its output is capped at
// budget.reportMaxTokens and its time at budget.reportTimeout. The kernel
// runs no tool call of the answer. The text of the answer becomes the
// output of the run, which finishes as failed with the budget error.
func (a *Agent) stepReport(ctx context.Context, rs *runState, c *cursor, n int) error {
	r := c.rep
	if r.done {
		return a.finishReport(ctx, rs, c, n)
	}
	prof := a.profileOf(c.queued)
	selector := a.selectorOf(c.queued, prof)
	model, err := a.openModel(ctx, selector)
	if err != nil {
		if _, err := a.emit(ctx, rs, EvError, ErrorData{Message: "model: " + err.Error()}); err != nil {
			return err
		}
		return a.finishAs(ctx, rs, OutcomeFailed, "", "budget exceeded: "+r.data.text(rs.id), RunStopBudget(r.data.Limit))
	}
	req, effort, err := a.callRequest(ctx, rs, c, prof)
	if err != nil {
		return err
	}
	b := a.budgetOf(rs)
	req.MaxTokens = b.reportTokens()
	provider, name := SplitSelector(selector)
	extra := (len(req.System) + len(c.context) + jsonSize(req.Tools)) / bytesPerToken

	rs.emitMu.Lock()
	if r.noteSeq == 0 {
		note := MessageReceivedData{Role: RoleSystem, Content: []Block{Text(reportPrompt(r.data, rs.id))}, Origin: OriginKernel, Note: NoteReport}
		if _, err := a.emitLocked(ctx, rs, EvMessageReceived, note); err != nil {
			rs.emitMu.Unlock()
			return err
		}
	}
	h, tokens := rs.historyLocked(extra)
	req.Messages = withRunContext(c.context, a.readFiles(h.Messages, MediaTypesOf(model), a.dirOf(rs)))
	_, err = a.emitLocked(ctx, rs, EvModelRequested, ModelRequestedData{
		Provider:             provider,
		Model:                name,
		Effort:               requestedEffort(effort),
		EstimatedInputTokens: tokens,
		Report:               true,
		SystemNotes:          systemNotes(c),
	})
	rs.emitMu.Unlock()
	if err != nil {
		return err
	}

	rctx, cancel := context.WithTimeoutCause(ctx, b.reportTimeout(), errReportTimeout)
	start := time.Now()
	resp, err := model.Stream(rctx, req, func(Delta) {})
	latency := time.Since(start).Milliseconds()
	timedOut := errors.Is(context.Cause(rctx), errReportTimeout)
	cancel()
	if err != nil || resp == nil {
		if ctx.Err() != nil {
			return errStopped
		}
		why := "the model gave no response"
		switch {
		case timedOut:
			why = fmt.Sprintf("the call did not finish within %s (budget.reportTimeout)", FormatDuration(b.reportTimeout()))
		case err != nil:
			why = err.Error()
		}
		_, err := a.emit(ctx, rs, EvError, ErrorData{Message: "model: the final report call failed: " + why})
		return err
	}
	if resp.Provider != "" {
		provider = resp.Provider
	}
	if resp.Model != "" {
		name = resp.Model
	}
	_, err = a.emit(ctx, rs, EvModelResponded, ModelRespondedData{
		Content:     validJSONBlocks(resp.Content),
		Raw:         validJSON(resp.Raw),
		StopReason:  resp.StopReason,
		StopDetail:  resp.StopDetail,
		Usage:       resp.Usage,
		LatencyMS:   latency,
		FirstByteMS: resp.FirstByteMS,
		QueuedMS:    resp.QueuedMS,
		Provider:    provider,
		Model:       name,
	})
	return err
}

// finishReport finishes a run after its final report call: the text of
// the report is the output, and the error names the limit. The checks of
// the definition of done of the run run first, after the report
// (limitDone), except at the deadline. A follow-up or a change of the
// budget that arrived since the driver read the log keeps the run open,
// as it does at the end of a turn, and a pass that ran meanwhile is not
// recorded: the next step checks the budget again, so a larger budget lets
// the run go on, and a run that stays over its limit stops with the report
// as its output.
func (a *Agent) finishReport(ctx context.Context, rs *runState, c *cursor, n int) error {
	fresh := func() bool {
		for _, e := range rs.events[n:] {
			if freshInput(e) {
				return true
			}
		}
		return false
	}
	var pass *DoneCheckedData
	if doneAtLimit(rs, overrun{limit: c.rep.data.Limit, owner: rs}) {
		rs.emitMu.Lock()
		cut := fresh()
		rs.emitMu.Unlock()
		if cut {
			return nil
		}
		var err error
		if pass, err = a.limitDone(ctx, rs, c, c.rep.data, c.rep.text); err != nil {
			return err
		}
	}
	rs.emitMu.Lock()
	defer rs.emitMu.Unlock()
	if fresh() {
		return nil
	}
	return a.finishAtLimitLocked(ctx, rs, c.rep.text, c.rep.data, pass)
}

// freshInput reports whether an event gives a run new input: a message
// that is not from the kernel, or a change of the budget.
func freshInput(e Event) bool {
	switch e.Type {
	case EvBudgetChanged:
		return true
	case EvMessageReceived:
		var d struct {
			Origin string `json:"origin"`
		}
		return e.Decode(&d) == nil && d.Origin != OriginKernel
	}
	return false
}

// reportPrompt is the instruction of the final report call.
func reportPrompt(d BudgetExceededData, self string) string {
	return "Final report: this run reached " + limitPhrase(d, self) + ", so it ends now. " +
		"Do not call any tool; this call runs none. Reply with text only, in at most about 300 words, for the person who started the run: " +
		"(1) what you did, (2) what is left to do, and (3) where your work is: the files that you changed, the branches and commits, and any other place to look. " +
		"A person can continue this run later with a follow-up message and a larger budget."
}

// limitPhrase names the limit of a stop in a sentence for the model.
func limitPhrase(d BudgetExceededData, self string) string {
	switch d.Limit {
	case "maxTurns":
		return fmt.Sprintf("its limit of %d model turns (budget.maxTurns)", int(d.Max))
	case "maxToolCalls", "maxTokens", "maxCostUSD":
		if d.Scope == ScopeTree {
			if d.Run != self {
				return fmt.Sprintf("the limit of %s of run %s, which this run shares (budget.%s)", amountOf(d.Limit, d.Max), d.Run, d.Limit)
			}
			return fmt.Sprintf("the limit of %s that it shares with the runs under it (budget.%s)", amountOf(d.Limit, d.Max), d.Limit)
		}
		return fmt.Sprintf("its limit of %s (budget.%s)", amountOf(d.Limit, d.Max), d.Limit)
	case "deadline":
		if d.Max > 0 {
			return "the deadline of its working time, " + FormatDuration(seconds(d.Max)) + " (budget.deadline)"
		}
		return "the deadline of its working time (budget.deadline)"
	}
	return "its limit " + d.Limit
}

// seconds converts a number of seconds to a duration.
func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// deadlineUse is the deadline of the lineage of a run that is nearest: the
// working time that the run, or one of its ancestors, used of its deadline.
type deadlineUse struct {
	used, max time.Duration
	owner     *runState // the run whose budget holds the deadline
}

// left is the working time that remains; zero or less when it is used up.
func (d deadlineUse) left() time.Duration { return d.max - d.used }

// overrun is the stop of rs at this deadline.
func (d deadlineUse) overrun(rs *runState) overrun {
	return overrun{limit: "deadline", value: d.used.Seconds(), max: d.max.Seconds(), owner: rs}
}

// why says which deadline the run reached, for the error result of a tool
// call that does not run.
func (d deadlineUse) why(rs *runState) string {
	if d.owner == nil || d.owner == rs {
		return fmt.Sprintf("the run used %s of working time, and its deadline is %s (budget.deadline)", FormatDuration(d.used), FormatDuration(d.max))
	}
	return fmt.Sprintf("run %s, which this run belongs to, used %s of working time, and its deadline is %s (budget.deadline)", d.owner.id, FormatDuration(d.used), FormatDuration(d.max))
}

// deadlineUse returns the deadline of the lineage of rs that has the least
// working time left, and false when no deadline applies. Each run of the
// lineage counts its own working time against its own deadline.
func (a *Agent) deadlineUse(ctx context.Context, rs *runState, now time.Time) (deadlineUse, bool) {
	var out deadlineUse
	found := false
	for _, r := range a.lineage(rs) {
		b := a.ownBudget(r)
		if b.Deadline <= 0 {
			continue
		}
		evs, err := a.eventsOf(ctx, r)
		if err != nil {
			a.log.Warn("read run for its deadline", "run", r.id, "error", err)
			continue
		}
		d := deadlineUse{used: WorkingTime(evs, now), max: b.Deadline, owner: r}
		if !found || d.left() < out.left() {
			out, found = d, true
		}
	}
	return out, found
}

// deadlineCut returns the context of a model call that ends when the
// working time of the run, or of an ancestor, reaches its deadline, and a
// function that releases the context and reports whether the deadline
// ended it. Without a deadline, the context is ctx.
func (a *Agent) deadlineCut(ctx context.Context, rs *runState) (context.Context, func() bool) {
	d, ok := a.deadlineUse(ctx, rs, a.now())
	if !ok {
		return ctx, func() bool { return false }
	}
	cctx, cancel := context.WithTimeoutCause(ctx, max(d.left(), 0), errPastDeadline)
	return cctx, func() bool {
		cut := errors.Is(context.Cause(cctx), errPastDeadline)
		cancel()
		return cut
	}
}

// stopAtDeadline follows a model call that the deadline ended. The call has
// no answer, so the kernel records the error, closes the turn, and stops
// the run at its deadline, with the final report when the budget turns it
// on. An answer that came before the end is recorded as any other, so an
// answered call is recorded once. After a restart between these events,
// the turn without an answer calls the model again, and the check before
// that call stops the run, because the deadline is still past.
func (a *Agent) stopAtDeadline(ctx context.Context, rs *runState, c *cursor) error {
	d, _ := a.deadlineUse(ctx, rs, a.now())
	msg := "model: the working time reached the deadline during the model call, so the kernel stopped the call (budget.deadline)"
	if _, err := a.emit(ctx, rs, EvError, ErrorData{Message: msg}); err != nil {
		return err
	}
	turn := c.turns
	if !c.turnOpen {
		turn++ // the step started the turn
	}
	if _, err := a.emit(ctx, rs, EvTurnFinished, TurnData{Turn: turn}); err != nil {
		return err
	}
	return a.stopOver(ctx, rs, c, d.overrun(rs))
}

// budgetUse is how much of one limit a run, or the tree that holds the
// limit, used.
type budgetUse struct {
	limit     string
	used, max float64
	owner     *runState // the run whose budget holds the limit
	tree      bool      // the runs under the owner share the limit
}

// threshold returns the highest threshold of a budget note that the usage
// passed, or 0.
func (u budgetUse) threshold() int {
	if u.max <= 0 {
		return 0
	}
	switch f := u.used / u.max; {
	case f >= noteWrapUp/100.0:
		return noteWrapUp
	case f >= notePlan/100.0:
		return notePlan
	}
	return 0
}

// key is the subject of a note about the use, for rs.
func (u budgetUse) key(rs *runState) noteKey {
	k := noteKey{limit: u.limit, max: u.max}
	if u.owner != nil && u.owner != rs {
		k.run = u.owner.id
	}
	return k
}

// budgetUses returns the use of each limit of the effective budget b of rs
// that a budget note covers: the turns of the run, the tool calls, the
// tokens, and the cost of the tightest tree of its lineage, and the nearest
// deadline of its lineage.
func (a *Agent) budgetUses(ctx context.Context, rs *runState, c *cursor, b Budget) []budgetUse {
	var out []budgetUse
	if b.MaxTurns > 0 {
		// The turn of the next model call counts as used, as in overBudget.
		next := c.turns - c.keptTurns
		if !c.turnOpen || c.keptOpen {
			next++
		}
		out = append(out, budgetUse{limit: "maxTurns", used: float64(next), max: float64(b.MaxTurns), owner: rs})
	}
	room := a.roomOf(rs)
	for _, f := range []struct {
		name string
		a    allowance
	}{{"maxToolCalls", room.calls}, {"maxTokens", room.tokens}, {"maxCostUSD", room.cost}} {
		if f.a.set {
			out = append(out, budgetUse{limit: f.name, used: f.a.used, max: f.a.max, owner: f.a.owner, tree: true})
		}
	}
	if d, ok := a.deadlineUse(ctx, rs, a.now()); ok {
		out = append(out, budgetUse{limit: "deadline", used: d.used.Seconds(), max: d.max.Seconds(), owner: d.owner})
	}
	return out
}

// budgetNotes returns the budget notes of the next model call: one note
// for each limit whose use passed 75% or 90% of the limit with no note of
// that threshold or a higher one yet, and one note after a change of the
// budget. Each note is a system message from the kernel that the ledger
// keeps, so a resume gives no note twice. The first model call of a run
// gets no note, as for the time notes.
func (a *Agent) budgetNotes(ctx context.Context, rs *runState, c *cursor) []MessageReceivedData {
	if !c.answered {
		return nil
	}
	b := a.budgetOf(rs)
	uses := a.budgetUses(ctx, rs, c, b)
	var out []MessageReceivedData
	if c.changeSeq > c.changeNote {
		out = append(out, MessageReceivedData{Role: RoleSystem, Content: []Block{Text(a.changeNoteText(rs, uses))},
			Origin: OriginKernel, Note: NoteBudget, Budget: &BudgetNoteData{Changed: true}})
	}
	for _, u := range uses {
		pct := u.threshold()
		k := u.key(rs)
		if pct == 0 || c.noted[k] >= pct {
			continue
		}
		data := &BudgetNoteData{Limit: u.limit, Percent: pct, Value: u.used, Max: u.max, Run: k.run}
		out = append(out, MessageReceivedData{Role: RoleSystem, Content: []Block{Text(a.budgetNoteText(rs, u, pct, b.reportOn()))},
			Origin: OriginKernel, Note: NoteBudget, Budget: data})
	}
	return out
}

// budgetNoteText is the text of a budget note: which limit, how much is
// used, how much is left, and what to do now.
func (a *Agent) budgetNoteText(rs *runState, u budgetUse, pct int, report bool) string {
	who := a.useSubject(rs, u)
	left := max(u.max-u.used, 0)
	used := int(u.used / u.max * 100)
	var text string
	switch u.limit {
	case "maxTurns":
		text = fmt.Sprintf("Budget note: this is model turn %d of the %d that the budget of this run allows (budget.maxTurns, %d%% used). ", int(u.used), int(u.max), used)
		if left <= 0 {
			text += "This is the last turn."
		} else {
			text += plural(int(left), "turn is", "turns are") + " left after this one."
		}
	case "maxToolCalls":
		text = fmt.Sprintf("Budget note: %s made %d of the %d tool calls that the budget allows (budget.maxToolCalls, %d%% used). %s left.",
			who, int(u.used), int(u.max), used, plural(int(left), "tool call is", "tool calls are"))
	case "maxTokens":
		text = fmt.Sprintf("Budget note: %s used %d of the %d tokens that the budget allows (budget.maxTokens, %d%% used). %d tokens are left.",
			who, int(u.used), int(u.max), used, int(left))
	case "maxCostUSD":
		text = fmt.Sprintf("Budget note: %s spent $%.2f of the $%.2f that the budget allows (budget.maxCostUSD, %d%% used). $%.2f is left.", who, u.used, u.max, used, left)
	case "deadline":
		text = fmt.Sprintf("Budget note: %s used %s of the %s of working time that the budget allows (budget.deadline, %d%% used). The run has %s of working time left. The time that a run waits for a person does not count.",
			who, FormatDuration(seconds(u.used)), FormatDuration(seconds(u.max)), used, FormatDuration(seconds(left)))
	}
	if pct < noteWrapUp {
		return text + " Plan the rest of the work to fit in what is left."
	}
	text += " Wrap up now: finish the step that you are on, then end your turn with a report of what you did, what is left, and where your work is (files, branches)."
	switch {
	case u.limit == "maxTokens" || u.limit == "maxCostUSD" || !report:
		return text + " At the limit, the run ends at once, with no further model call."
	case u.tree && u.owner != rs:
		return text + " At the limit, this run ends at once."
	}
	return text + " If the run reaches the limit first, the kernel asks you for that report in one last call without tools."
}

// useSubject names who used a limit, for a budget note.
func (a *Agent) useSubject(rs *runState, u budgetUse) string {
	switch {
	case u.owner != nil && u.owner != rs && u.tree:
		return fmt.Sprintf("run %s, which this run belongs to, and the runs under it", u.owner.id)
	case u.owner != nil && u.owner != rs:
		return fmt.Sprintf("run %s, which this run belongs to,", u.owner.id)
	case u.tree && a.hasTree(rs):
		return "this run and the runs under it"
	}
	return "this run"
}

// changeNoteText is the text of the note after a change of the budget: the
// limits that hold now and how much of each is used.
func (a *Agent) changeNoteText(rs *runState, uses []budgetUse) string {
	text := "Budget note: the budget of this run changed."
	if len(uses) == 0 {
		return text + " It now sets no limit that a note covers. Go on with the work."
	}
	text += " The limits now:"
	for i, u := range uses {
		if i > 0 {
			text += ";"
		}
		switch u.limit {
		case "maxTurns":
			text += fmt.Sprintf(" %d model turns (this call is turn %d)", int(u.max), int(u.used))
		case "maxToolCalls":
			text += fmt.Sprintf(" %d tool calls (%d made)", int(u.max), int(u.used))
		case "maxTokens":
			text += fmt.Sprintf(" %d tokens (%d used)", int(u.max), int(u.used))
		case "maxCostUSD":
			text += fmt.Sprintf(" $%.2f of cost ($%.2f spent)", u.max, u.used)
		case "deadline":
			text += fmt.Sprintf(" %s of working time (%s used)", FormatDuration(seconds(u.max)), FormatDuration(seconds(u.used)))
		}
		if u.owner != nil && u.owner != rs {
			text += " for run " + u.owner.id
		}
	}
	return text + ". Plan the rest of the work with these limits; a note about a limit before the change no longer holds."
}

// plural writes a count with the singular or the plural phrase.
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// BudgetText lists the fields that a budget sets as key=value pairs in
// the words of fw.yaml, such as "maxTurns=300 deadline=3h0m0s", for a log
// line or a line about an event. It returns "" for a budget that sets none.
func BudgetText(b Budget) string {
	var parts []string
	add := func(name string, v any) { parts = append(parts, fmt.Sprintf("%s=%v", name, v)) }
	if b.MaxTurns != 0 {
		add("maxTurns", b.MaxTurns)
	}
	if b.MaxTokens != 0 {
		add("maxTokens", b.MaxTokens)
	}
	if b.MaxCostUSD != 0 {
		add("maxCostUSD", b.MaxCostUSD)
	}
	if b.Deadline != 0 {
		add("deadline", b.Deadline)
	}
	if b.MaxToolCalls != 0 {
		add("maxToolCalls", b.MaxToolCalls)
	}
	if b.MaxCallsPerTurn != 0 {
		add("maxCallsPerTurn", b.MaxCallsPerTurn)
	}
	if b.MaxRepeats != 0 {
		add("maxRepeats", b.MaxRepeats)
	}
	if b.FinalReport != nil {
		add("finalReport", *b.FinalReport)
	}
	if b.ReportMaxTokens != 0 {
		add("reportMaxTokens", b.ReportMaxTokens)
	}
	if b.ReportTimeout != 0 {
		add("reportTimeout", b.ReportTimeout)
	}
	return strings.Join(parts, " ")
}
