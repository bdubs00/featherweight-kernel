package fw

// This file is the routine runner (docs/ROUTINES.md section 3.3). It acts
// once per run, after fw.run.started and before the first model turn: it
// asks the deciders, records fw.routine.decided for each routine, and runs
// the first live routine that applies through the tool path of the model.
// When the routine completes, the run finishes with its answer and no
// model call. Otherwise the model runs, and a live routine leaves a
// <context source="routine"> block for it. At the end of a run that the
// model completed, the runner compares the answers of the routines with
// what the agent did. Like the turn loop, it reads what is left to do from
// the log alone.

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync/atomic"
)

// routineContext is the source of the context block of a live routine.
const routineContext = "routine"

// routineCallPrefix starts the call id of every call of the runner.
const routineCallPrefix = "routine_"

// routineInterrupted is the reason of a live routine that a restart or a
// shutdown cut off. The runner never runs a routine again.
const routineInterrupted = "the process stopped during the routine"

// routineCursor is what the log of a run holds about its routine runner.
type routineCursor struct {
	seen    bool   // the runner acted: fw.routine.decided, or a call of the runner
	live    string // the first live routine that applies: the one that runs
	version int    // its version
	applied bool   // fw.routine.applied of that routine
	ok      bool   // it completed
	answer  string // its answer
	reason  string // why it stopped
	done    bool   // a fw.run.finished after fw.routine.applied
	base    string // the context of fw.run.started
	// decided holds the decisions in mode live, and calls the calls of the
	// routine that ran, for the context block. Both only grow; a result
	// replaces calls with a copy, so a snapshot of the cursor keeps its own.
	decided []RoutineDecidedData
	calls   []routineCall
}

// routineCall is one call of the routine that ran, for the context block.
type routineCall struct {
	id, tool  string
	result    string
	isError   bool
	responded bool
}

// open reports whether a live routine applied and has no
// fw.routine.applied: a restart or a shutdown cut it off.
func (r *routineCursor) open() bool { return r.live != "" && !r.applied }

// applyRoutine follows the events of the routine runner. analyze and the
// cursor builder both call it first for each event, so they agree. The
// calls of the runner come before the first model turn, so a tool event
// after it is not read again.
func (c *cursor) applyRoutine(e Event) {
	r := &c.routine
	switch e.Type {
	case EvRunStarted:
		var d RunStartedData
		if e.Decode(&d) == nil {
			r.base = d.Context
		}
		return
	case EvRoutineDecided:
		var d RoutineDecidedData
		if e.Decode(&d) != nil {
			return
		}
		r.seen = true
		if d.Mode != RoutineLive {
			return
		}
		r.decided = append(r.decided, d)
		if d.Applies && r.live == "" {
			r.live, r.version = d.Routine, d.Version
		}
	case EvToolRequested, EvToolResponded:
		if c.turns > 0 || c.answered {
			return
		}
		var d struct {
			CallID  string `json:"callId"`
			Tool    string `json:"tool"`
			Routine string `json:"routine"`
		}
		if e.Decode(&d) != nil || !strings.HasPrefix(d.CallID, routineCallPrefix) {
			return
		}
		if e.Type == EvToolRequested {
			r.seen = true
			if r.live == "" || d.Routine != r.live {
				return
			}
			r.calls = append(r.calls, routineCall{id: d.CallID, tool: d.Tool})
			break
		}
		i := slices.IndexFunc(r.calls, func(x routineCall) bool { return x.id == d.CallID })
		if i < 0 {
			return
		}
		var res ToolRespondedData
		if e.Decode(&res) != nil {
			return
		}
		r.calls = slices.Clone(r.calls)
		r.calls[i].result, r.calls[i].isError, r.calls[i].responded = TextOf(res.Content), res.IsError, true
	case EvRoutineApplied:
		var d RoutineAppliedData
		if e.Decode(&d) != nil || d.Routine != r.live {
			return
		}
		r.applied, r.ok, r.answer, r.reason = true, d.OK, d.Answer, d.Reason
	case EvRunFinished:
		if !r.applied || r.done {
			return
		}
		r.done = true
	default:
		return
	}
	c.context = joinContext(r.base, r.block())
}

// joinContext puts the context block of a routine after the context of the
// tools of the run.
func joinContext(base, block string) string {
	switch {
	case block == "":
		return base
	case base == "":
		return block
	}
	return base + "\n\n" + block
}

// block returns the context block that a live routine leaves for the
// model: the decision with its probabilities, the steps that ran with
// their results in brief, and why the routine stopped. A run without a
// decision in mode live gets none, because the model of a rehearsal or a
// canary run must not see the answers of the routine.
func (r *routineCursor) block() string {
	if len(r.decided) == 0 {
		return ""
	}
	var b strings.Builder
	switch {
	case r.ok:
		b.WriteString("A routine of this agent handled this run before you, and a message came after it. What it decided and did:")
	default:
		b.WriteString("A routine of this agent looked at this run before you and did not complete it, so the run is yours. What it decided and did:")
	}
	for _, d := range r.decided {
		state := "applies"
		if !d.Applies {
			state = "does not apply: " + d.Reason
		}
		fmt.Fprintf(&b, "\n- Routine %s, version %d: %s.", d.Routine, d.Version, state)
		if len(d.Answers) > 0 {
			b.WriteString(" Answers: " + answersText(d.Answers) + ".")
		}
	}
	if len(r.calls) > 0 {
		fmt.Fprintf(&b, "\nCalls of routine %s:", r.live)
		for i, c := range r.calls {
			status := "no result"
			switch {
			case c.responded && c.isError:
				status = "error: " + clipLine(c.result, 300)
			case c.responded:
				status = "ok: " + clipLine(c.result, 300)
			}
			fmt.Fprintf(&b, "\n%d. %s, %s", i+1, c.tool, status)
		}
	}
	switch {
	case r.applied && !r.ok:
		b.WriteString("\nIt stopped: " + r.reason + ".")
	case r.live != "" && !r.applied:
		b.WriteString("\nIt stopped: " + routineInterrupted + ".")
	case r.ok:
		b.WriteString("\nIts answer: " + clipLine(r.answer, 1000))
	}
	return ContextBlock(routineContext, b.String())
}

// answersText words the answers of a decision, by question name: the
// option of a choice or a score with its probability, and P(yes) of a
// noul.
func answersText(answers map[string]Answer) string {
	var parts []string
	for _, name := range slices.Sorted(maps.Keys(answers)) {
		a := answers[name]
		if a.Type == QuestionNoul {
			parts = append(parts, fmt.Sprintf("%s: the probability of yes is %.2f", name, a.Probability))
			continue
		}
		p := a.Probability
		if v, ok := a.Probabilities[a.Choice]; ok {
			p = v
		}
		parts = append(parts, fmt.Sprintf("%s: %s with probability %.2f", name, a.Choice, p))
	}
	return strings.Join(parts, "; ")
}

// clipLine cuts a text to n bytes at a rune boundary, on one line.
func clipLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "..."
}

// activeRoutines returns the routines that the runner acts on: those with
// standing shadow or live, in the order of the profile.
func activeRoutines(rs []Routine) []Routine {
	var out []Routine
	for _, r := range rs {
		if r.Standing == RoutineShadow || r.Standing == RoutineLive {
			out = append(out, r)
		}
	}
	return out
}

// routineDue reports whether the runner acts now: the run started, did not
// finish, has made no model turn, is no replay and no branch, and its
// profile has a routine to act on. The runner acts once: any routine
// event or call of the runner in the log means that it acted, also before
// a restart.
func (a *Agent) routineDue(rs *runState, c *cursor) bool {
	if !c.started || c.routine.seen || c.terminal || c.turns > 0 || c.answered || c.rep != nil || c.queued.Kept > 0 {
		return false
	}
	// A replay keeps the mark of its source in its metadata, also after
	// a restart, when the override of the process is gone.
	if c.queued.Metadata["replayOf"] != "" || a.k.override(rs.id) != nil {
		return false
	}
	return len(activeRoutines(a.profileOf(c.queued).Routines)) > 0
}

// routineRun is the work of the runner on one run.
type routineRun struct {
	a      *Agent
	rs     *runState
	prof   AgentProfile
	tools  map[string]Tool
	policy Policy
	calls  int // the tool calls of the run, for budget.maxToolCalls
}

// routineVerdict is the decision on one routine.
type routineVerdict struct {
	data RoutineDecidedData
	said map[string]string // the template field of each question
}

// stepRoutine is the start of the runner: the decision, then the live
// routine that applies. n is the number of events that c read.
func (a *Agent) stepRoutine(ctx context.Context, rs *runState, c *cursor, n int) error {
	prof := a.profileOf(c.queued)
	routines := activeRoutines(prof.Routines)
	_, tools := a.runTools(ctx, rs, prof)
	rr := &routineRun{a: a, rs: rs, prof: prof, tools: tools, policy: a.policyFor(rs, prof), calls: c.toolCalls - c.keptCalls}
	verdicts, err := rr.decide(ctx, routines, c.queued.Input)
	if err != nil {
		return err
	}
	chosen := -1
	for i, v := range verdicts {
		if _, err := a.emit(ctx, rs, EvRoutineDecided, v.data); err != nil {
			return err
		}
		if chosen < 0 && v.data.Mode == RoutineLive && v.data.Applies {
			chosen = i
		}
	}
	if chosen < 0 {
		return nil
	}
	applied, err := rr.apply(ctx, routines[chosen], c.queued, verdicts[chosen].said)
	if err != nil {
		return err
	}
	rs.emitMu.Lock()
	defer rs.emitMu.Unlock()
	if _, err := a.emitLocked(ctx, rs, EvRoutineApplied, applied); err != nil {
		return err
	}
	if !applied.OK || c.unseen || messageSince(rs.events, n) {
		// A message that arrived meanwhile needs the model, which starts
		// with the context block of the routine.
		return nil
	}
	_, err = a.emitLocked(ctx, rs, EvRunFinished, RunFinishedData{Outcome: OutcomeCompleted, Output: applied.Answer})
	return err
}

// closeRoutine records the end of a live routine that a restart or a
// shutdown cut off. The runner never runs it again: the model starts with
// the context block of what the routine did.
func (a *Agent) closeRoutine(ctx context.Context, rs *runState, c *cursor) error {
	r := c.routine
	// The approval that a call of the routine waited for has no step to
	// go to any more. The kernel denies it, so the answer of a person is
	// refused, not lost.
	if p := c.pending; p != nil && strings.HasPrefix(p.CallID, routineCallPrefix) {
		d := ApprovalDecidedData{ApprovalID: p.ApprovalID, Decision: "deny", Decider: OriginKernel, Note: routineInterrupted}
		if _, err := a.emit(ctx, rs, EvApprovalDecided, d); err != nil {
			return err
		}
	}
	// The failed step is the call without a result, or else the next one.
	failed := len(r.calls)
	if failed == 0 || r.calls[failed-1].responded {
		failed++
	}
	_, err := a.emit(ctx, rs, EvRoutineApplied, RoutineAppliedData{
		Routine: r.live, Version: r.version, Steps: failed, FailedStep: failed, Reason: routineInterrupted,
	})
	return err
}

// routineWaits reports whether the run waits for an approval of a call of
// its routine runner. A restart then resumes the run, so the routine ends.
// The caller holds rs.mu.
func (rs *runState) routineWaits() bool {
	p := rs.run.Pending
	return p != nil && strings.HasPrefix(p.CallID, routineCallPrefix)
}

// finishRoutine finishes a run whose routine completed when a restart came
// between fw.routine.applied and fw.run.finished.
func (a *Agent) finishRoutine(ctx context.Context, rs *runState, c *cursor, n int) error {
	rs.emitMu.Lock()
	defer rs.emitMu.Unlock()
	if messageSince(rs.events, n) {
		return nil
	}
	_, err := a.emitLocked(ctx, rs, EvRunFinished, RunFinishedData{Outcome: OutcomeCompleted, Output: c.routine.answer})
	return err
}

// routineFinishDue reports whether a run whose routine completed still
// needs its fw.run.finished, and no message waits for the model.
func routineFinishDue(c *cursor) bool {
	r := c.routine
	return r.applied && r.ok && !r.done && !c.terminal && !c.unseen && c.turns == 0
}

// messageSince reports whether a message arrived in the events from n on.
// The caller holds rs.emitMu.
func messageSince(events []Event, n int) bool {
	for _, e := range events[min(n, len(events)):] {
		if e.Type == EvMessageReceived {
			return true
		}
	}
	return false
}

// decide asks the deciders every question of every routine, one call per
// decider for the routines that share it, and decides for each routine
// whether it applies and in which mode. A decider that fails makes every
// routine that asked it not apply, with the reason.
func (rr *routineRun) decide(ctx context.Context, routines []Routine, input string) ([]routineVerdict, error) {
	out := make([]routineVerdict, len(routines))
	var order []string
	members := map[string][]int{}
	for i, r := range routines {
		mode := r.Standing
		if r.Standing == RoutineLive && CanaryPick(rr.rs.id, r.CanaryShare()) {
			mode = RoutineCanary
		}
		out[i] = routineVerdict{
			data: RoutineDecidedData{Routine: r.Name, Version: r.Version, Standing: r.Standing, Mode: mode, Applies: true, Decider: r.Decider},
			said: map[string]string{},
		}
		if len(r.Questions) == 0 {
			continue
		}
		if _, ok := members[r.Decider]; !ok {
			order = append(order, r.Decider)
		}
		members[r.Decider] = append(members[r.Decider], i)
	}
	for _, decider := range order {
		idx := members[decider]
		questions := map[string]Question{}
		var names []string
		for _, i := range idx {
			r := routines[i]
			names = append(names, r.Name)
			for name, q := range r.Questions {
				questions[r.Name+"."+name] = q.Question
			}
		}
		answers, model, latency, why, err := rr.ask(ctx, decider, strings.Join(names, ","), input, questions)
		if err != nil {
			return nil, err
		}
		for _, i := range idx {
			r, d := routines[i], &out[i].data
			d.Model, d.LatencyMS = model, latency
			if why != "" {
				d.Applies, d.Reason = false, "decider: "+why
				continue
			}
			own := map[string]Answer{}
			for name := range r.Questions {
				if a, ok := answers[r.Name+"."+name]; ok {
					own[name] = a
				}
			}
			d.Answers = own
			d.Applies, d.Reason = RoutineApplies(r, own)
			for name, q := range r.Questions {
				if a, ok := own[name]; ok {
					out[i].said[name] = SaidOf(q, a)
				}
			}
		}
	}
	return out, nil
}

// ask calls the tool decide for the questions, in calls of at most
// MaxDeciderQuestions. It returns the answers, the model, and the latency
// of the calls together, or why the decider gave no decision.
func (rr *routineRun) ask(ctx context.Context, decider, routines, input string, questions map[string]Question) (map[string]Answer, string, int64, string, error) {
	tool := rr.a.builtinTool("decide")
	if tool == nil {
		return nil, "", 0, "the built-in tool decide is off; it needs decide in tools.builtin and a decider", nil
	}
	ids := slices.Sorted(maps.Keys(questions))
	answers := map[string]Answer{}
	var model string
	var latency int64
	for start := 0; start < len(ids); start += MaxDeciderQuestions {
		part := map[string]Question{}
		for _, id := range ids[start:min(start+MaxDeciderQuestions, len(ids))] {
			part[id] = questions[id]
		}
		in, err := json.Marshal(map[string]any{"decider": decider, "state": input, "questions": part})
		if err != nil {
			return nil, "", 0, err.Error(), nil
		}
		res, why, err := rr.call(ctx, routines, tool, in)
		if err != nil || why != "" {
			return nil, "", 0, why, err
		}
		text := TextOf(res.Content)
		if res.IsError {
			return nil, "", 0, clipLine(strings.TrimPrefix(text, "decide: "), 300), nil
		}
		var d DecideResult
		if err := json.Unmarshal([]byte(text), &d); err != nil {
			return nil, "", 0, "the result of decide is not a decision", nil
		}
		for id, a := range d.Answers {
			answers[id] = a
		}
		model, latency = d.Model, latency+d.LatencyMS
	}
	return answers, model, latency, "", nil
}

// builtinTool returns the tool of the agent with that name, whatever the
// tool list of the profile. The runner asks the decider through decide
// even for an agent whose model does not get the tool.
func (a *Agent) builtinTool(name string) Tool {
	for _, t := range a.tools {
		if t.Spec().Name == name {
			return t
		}
	}
	return nil
}

// apply runs the steps of a live routine, then its checks, then renders
// its answer. A step that fails, a check that fails, or a template error
// ends the routine. An error means that the run stopped or the ledger
// failed.
func (rr *routineRun) apply(ctx context.Context, r Routine, q RunQueuedData, said map[string]string) (RoutineAppliedData, error) {
	out := RoutineAppliedData{Routine: r.Name, Version: r.Version}
	data := RoutineData(q.Input, q.Metadata)
	for k, v := range said {
		data[k] = v
	}
	stop := func(place int, reason string) (RoutineAppliedData, error) {
		out.FailedStep, out.Reason = place, reason
		return out, nil
	}
	run := func(place int, label, tool, input string) (string, string, error) {
		out.Steps = place
		t, ok := rr.tools[tool]
		if !ok {
			return "", label + ": " + tool + " is not a tool of this run", nil
		}
		in, err := RenderRoutineInput(input, data)
		if err != nil {
			return "", label + ": " + err.Error(), nil
		}
		res, why, err := rr.call(ctx, r.Name, t, in)
		switch {
		case err != nil:
			return "", "", err
		case why != "":
			return "", label + ": " + why, nil
		case res.IsError:
			return "", label + ": error: " + clipLine(TextOf(res.Content), 200), nil
		}
		return TextOf(res.Content), "", nil
	}
	for i, s := range r.Steps {
		label := fmt.Sprintf("step %d %s", i+1, s.Tool)
		text, why, err := run(i+1, label, s.Tool, s.Input)
		if err != nil || why != "" {
			if err != nil {
				return out, err
			}
			return stop(i+1, why)
		}
		if s.As != "" {
			data[s.As] = text
		}
	}
	for j, c := range r.After {
		place := len(r.Steps) + j + 1
		label := fmt.Sprintf("check %d %s", j+1, c.Tool)
		text, why, err := run(place, label, c.Tool, c.Input)
		if err != nil || why != "" {
			if err != nil {
				return out, err
			}
			return stop(place, why)
		}
		for _, want := range c.Contains {
			s, err := RenderRoutine(want, data)
			if err != nil {
				return stop(place, label+": "+err.Error())
			}
			if !strings.Contains(text, s) {
				return stop(place, fmt.Sprintf("%s: the result lacks %q", label, s))
			}
		}
		for _, bad := range c.Lacks {
			s, err := RenderRoutine(bad, data)
			if err != nil {
				return stop(place, label+": "+err.Error())
			}
			if strings.Contains(text, s) {
				return stop(place, fmt.Sprintf("%s: the result holds %q", label, s))
			}
		}
	}
	answer, err := RenderRoutine(r.Answer, data)
	if err != nil {
		out.Reason = "answer: " + err.Error()
		return out, nil
	}
	out.OK, out.Answer = true, answer
	return out, nil
}

// call makes one tool call of the runner through the path of a call of the
// model: the budget of the run and of its tree, the policy with its
// approval flow, the limits of the tool, and the events fw.tool.requested,
// with routine set, and fw.tool.responded. It returns the result, or why
// the call did not run. An error means that the run stopped or the ledger
// failed.
func (rr *routineRun) call(ctx context.Context, routine string, t Tool, input json.RawMessage) (ToolResult, string, error) {
	a, rs := rr.a, rr.rs
	spec := t.Spec()
	if o, spent := a.limitSpent(rs); spent {
		return ToolResult{}, "not run: " + o.why(), nil
	}
	if d, past := a.pastDeadline(rs); past {
		return ToolResult{}, "not run: " + d.why(rs), nil
	}
	if b := a.budgetOf(rs); b.MaxToolCalls > 0 && rr.calls >= b.MaxToolCalls {
		return ToolResult{}, fmt.Sprintf("not run: the run reached its limit of %d tool calls (budget.maxToolCalls)", b.MaxToolCalls), nil
	}
	if calls := a.roomOf(rs).calls; calls.set && calls.left <= 0 {
		return ToolResult{}, fmt.Sprintf("not run: the runs of this tree used the %d tool calls of the budget of run %s (budget.maxToolCalls)", int(calls.max), calls.owner.id), nil
	}
	callID := routineCallPrefix + NewID()
	use := Block{Type: BlockToolUse, ID: callID, Name: spec.Name, Input: input}
	pctx := a.policyCtx(ctx, rs, callID)
	d := rr.policy.Decide(pctx, ResolveCall(pctx, t, ToolCall{RunID: rs.id, Agent: rs.agent, CallID: callID, Spec: spec, Input: input}))
	switch d.Verdict {
	case VerdictAllow:
	case VerdictAsk:
		var aborted atomic.Bool
		dec, err := a.waitDecision(ctx, ctx, rs, ApprovalRequestedData{Tool: spec.Name, CallID: callID, Input: input, Reason: d.Reason}, &aborted)
		if err != nil {
			if aborted.Load() || ctx.Err() != nil {
				return ToolResult{}, "", errStopped
			}
			return ToolResult{}, "", err
		}
		if dec.Decision != "allow" {
			return ToolResult{}, deniedBy(&dec), nil
		}
		// A deny that a profile gained while the call waited wins.
		if r := a.recheck(ctx, rs, rr.prof, t, use); r.Verdict == VerdictDeny {
			return ToolResult{}, "denied by policy: " + r.Reason, nil
		}
	default:
		return ToolResult{}, "denied by policy: " + d.Reason, nil
	}
	rr.calls++
	res, err := a.runOne(ctx, rs, plannedCall{use: use, tool: t, spec: spec, run: true, routine: routine})
	return res, "", err
}

// compareLocked appends one fw.routine.compared per observed question of
// each routine that the run decided in mode shadow or canary, or in mode
// live without completing, at the end of a run that the model completed.
// It runs once per run. The caller holds rs.emitMu and appends
// fw.run.finished next.
func (a *Agent) compareLocked(ctx context.Context, rs *runState, output string) error {
	q := rs.queuedData()
	routines := a.profileOf(q).Routines
	if len(routines) == 0 {
		return nil
	}
	for _, d := range RoutineComparisons(routines, rs.events, q.Kept, output) {
		if _, err := a.emitLocked(ctx, rs, EvRoutineCompared, d); err != nil {
			return err
		}
	}
	return nil
}

// RoutineComparisons returns the comparisons of a run: for each routine
// that the events decided in mode shadow or canary, or in mode live
// without completing, and each of its questions with observe, what the
// routine said against what the agent did. It returns none when the model
// made no call or when the events hold a comparison already. A branch
// skips the events that it kept from its source (kept), so it never
// compares the decisions of its source again. output is the output text
// of the run.
func RoutineComparisons(routines []Routine, events []Event, kept uint64, output string) []RoutineComparedData {
	var decided []RoutineDecidedData
	completed := map[string]bool{}
	acted := false
	for _, e := range events {
		if e.Seq <= kept {
			continue
		}
		switch e.Type {
		case EvRoutineCompared:
			return nil
		case EvModelResponded:
			acted = true
		case EvRoutineDecided:
			var d RoutineDecidedData
			if e.Decode(&d) == nil {
				decided = append(decided, d)
			}
		case EvRoutineApplied:
			var d RoutineAppliedData
			if e.Decode(&d) == nil && d.OK {
				completed[d.Routine] = true
			}
		}
	}
	if !acted {
		return nil
	}
	var out []RoutineComparedData
	for _, d := range decided {
		if d.Mode == RoutineLive && completed[d.Routine] {
			continue
		}
		i := slices.IndexFunc(routines, func(r Routine) bool { return r.Name == d.Routine && r.Version == d.Version })
		if i < 0 {
			continue
		}
		r := routines[i]
		for _, name := range r.QuestionNames() {
			q := r.Questions[name]
			ans, ok := d.Answers[name]
			if !ok {
				continue
			}
			observed, ok := ObserveAnswer(q, output, events)
			if !ok {
				continue
			}
			said := SaidOf(q, ans)
			out = append(out, RoutineComparedData{
				Routine: r.Name, Version: r.Version, Mode: d.Mode, Question: name,
				Said: said, Observed: observed,
				Agree: strings.EqualFold(strings.TrimSpace(said), strings.TrimSpace(observed)),
			})
		}
	}
	return out
}
