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
	// compared is true when the run holds a comparison of its own.
	compared bool
	// decided holds every decision of the run, for the context block and
	// the comparison, and kept counts those at its start that a branch
	// kept from its source. calls holds the calls of the routine that ran.
	// Both lists only grow; a result replaces calls with a copy, so a
	// snapshot of the cursor keeps its own.
	decided []RoutineDecidedData
	kept    int
	calls   []routineCall
}

// failedExit reports whether a tool reported the exit code of a command
// that failed: set and not zero.
func failedExit(code *int) bool { return code != nil && *code != 0 }

// routineCall is one call of the routine that ran, for the context block.
// isError is true for an error result and for a command that failed.
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
		r.decided = append(r.decided, d)
		if e.Seq <= c.queued.Kept {
			r.kept++
		}
		if d.Mode != RoutineLive {
			return
		}
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
		r.calls[i].result, r.calls[i].isError, r.calls[i].responded = TextOf(res.Content), res.IsError || failedExit(res.ExitCode), true
	case EvRoutineApplied:
		var d RoutineAppliedData
		if e.Decode(&d) != nil || d.Routine != r.live {
			return
		}
		r.applied, r.ok, r.answer, r.reason = true, d.OK, d.Answer, d.Reason
	case EvRoutineCompared:
		r.compared = r.compared || e.Seq > c.queued.Kept
		return
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
	var live []RoutineDecidedData
	for _, d := range r.decided {
		if d.Mode == RoutineLive {
			live = append(live, d)
		}
	}
	if len(live) == 0 {
		return ""
	}
	var b strings.Builder
	switch {
	case r.ok:
		b.WriteString("A routine of this agent handled this run before you, and a message came after it. What it decided and did:")
	default:
		b.WriteString("A routine of this agent looked at this run before you and did not complete it, so the run is yours. What it decided and did:")
	}
	for _, d := range live {
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
				status = "error: " + ClipText(c.result, 300, "the result")
			case c.responded:
				status = "ok: " + ClipText(c.result, 300, "the result")
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
		b.WriteString("\nIts answer: " + ClipText(r.answer, 1000, "the answer"))
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
		parts = append(parts, fmt.Sprintf("%s: %s with probability %.2f", name, a.Choice, a.Probability))
	}
	return strings.Join(parts, "; ")
}

// routineDue reports whether the runner acts now: the run started, did not
// finish, has made no model turn, is no replay, no branch, no check run,
// and not held back from Know-how (HeldBack), and its profile has a
// routine to act on. The runner acts once: any routine event or call of
// the runner in the log means that it acted, also before a restart.
func (a *Agent) routineDue(rs *runState, c *cursor) bool {
	if !c.started || c.routine.seen || c.terminal || c.turns > 0 || c.answered || c.rep != nil || c.queued.Kept > 0 || c.queued.Check != nil {
		return false
	}
	if HeldBack(c.queued.Metadata) {
		return false
	}
	// A replay keeps the mark of its source in its metadata, also after
	// a restart, when the override of the process is gone.
	if c.queued.Metadata["replayOf"] != "" || a.k.override(rs.id) != nil {
		return false
	}
	return len(routinesFor(a.profileOf(c.queued).Routines, c.queued.Metadata)) > 0
}

// routinesFor returns the routines that the runner considers for a run, in
// profile order: a routine bound to a Duty (Routine.Duty) only when the
// metadata MetaDuty of the run names that Duty, and every routine without
// a Duty.
func routinesFor(routines []Routine, meta map[string]string) []Routine {
	duty := strings.TrimSpace(meta[MetaDuty])
	var out []Routine
	for _, r := range routines {
		if r.Duty == "" || r.Duty == duty {
			out = append(out, r)
		}
	}
	return out
}

// routineRisk returns RiskRead when every tool of the steps and of the
// checks of a routine is a tool of the run whose annotations say that it
// is read-only (ToolSpec.ReadOnly), and RiskWrite otherwise. A tool that
// the run lacks counts as a write, because nothing says that it only
// reads.
func routineRisk(r Routine, tools map[string]Tool) string {
	readOnly := func(name string) bool {
		t, ok := tools[name]
		return ok && t.Spec().ReadOnly
	}
	for _, s := range r.Steps {
		if !readOnly(s.Tool) {
			return RiskWrite
		}
	}
	for _, c := range r.After {
		if !readOnly(c.Tool) {
			return RiskWrite
		}
	}
	return RiskRead
}

// routineRun is the work of the runner on one run. A check run makes its
// calls through it too (check.go).
type routineRun struct {
	a      *Agent
	rs     *runState
	prof   AgentProfile
	tools  map[string]Tool
	policy Policy
	calls  int    // the tool calls of the run, for budget.maxToolCalls
	prefix string // starts the call id of each call: routineCallPrefix or checkCallPrefix
	// check is true for the calls of a check run: a call that the policy
	// asks for does not run, because nobody answers a check.
	check bool
}

// routineVerdict is the decision on one routine.
type routineVerdict struct {
	data RoutineDecidedData
	said map[string]string // the template field of each question
}

// stepRoutine is the start of the runner: the decision, then the live
// routine that applies, which ends with fw.routine.applied. The next step
// finishes a run whose routine completed (finishRoutine), the same step
// that a restart between the two events takes.
func (a *Agent) stepRoutine(ctx context.Context, rs *runState, c *cursor) error {
	prof := a.profileOf(c.queued)
	routines := routinesFor(prof.Routines, c.queued.Metadata)
	_, tools := a.runTools(ctx, rs, prof)
	rr := &routineRun{a: a, rs: rs, prof: prof, tools: tools, policy: a.policyFor(rs, prof), calls: c.toolCalls - c.keptCalls, prefix: routineCallPrefix}
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
	if applied.OK {
		if err := a.checkRoutineDone(ctx, rs, routines[chosen], &applied); err != nil {
			return err
		}
	}
	_, err = a.emit(ctx, rs, EvRoutineApplied, applied)
	return err
}

// checkRoutineDone runs the checks of the definition of done of the run
// on the work of a live routine that completed, before fw.routine.applied,
// as on an answer of the model. A run that a routine finishes therefore
// counts as checked only as any run does (CheckedDone). The pass has the
// answer 0, because no model answered: the passes of the model leave it
// out, so the model keeps every attempt. When a check fails, the routine
// did not complete. Its fw.routine.applied names the failed checks, one
// place after its steps and its checks, and the model takes the run with
// the context block of the routine.
func (a *Agent) checkRoutineDone(ctx context.Context, rs *runState, r Routine, applied *RoutineAppliedData) error {
	d := rs.queuedData().Done
	if !d.on() {
		return nil
	}
	// The cursor of the step that ran the routine does not count its
	// calls yet.
	c, _ := rs.executionCursor()
	pass := DoneCheckedData{Attempt: 1, Attempts: d.attempts()}
	if err := a.runDone(ctx, rs, c, d, &pass); err != nil {
		return err
	}
	pass.Continue = !pass.Passed
	a.log.Info("done checked after a routine", "run", rs.id, "routine", r.Name, "passed", pass.Passed)
	if _, err := a.emit(ctx, rs, EvDoneChecked, pass); err != nil {
		return err
	}
	if pass.Passed {
		applied.Done = DoneChecked
		return nil
	}
	var failed []string
	for i, res := range pass.Checks {
		if !res.Passed {
			why, _, _ := strings.Cut(res.Why, "\n")
			failed = append(failed, DoneCheck{Name: res.Name, Tool: res.Tool}.label(i)+" failed: "+strings.TrimSuffix(why, ":"))
		}
	}
	place := len(r.Steps) + len(r.After) + 1
	applied.OK, applied.Answer, applied.Steps, applied.FailedStep, applied.Done = false, "", place, place, DoneFailed
	applied.Reason = ClipText("done: "+strings.Join(failed, "; "), 500, "the reason")
	return nil
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
		d := ApprovalDecidedData{ApprovalID: p.ApprovalID, Decision: "deny", Decider: OriginKernel, DeciderKind: ActorKernel, Note: routineInterrupted}
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

// finishRoutine finishes a run whose routine completed, with its answer as
// the output. A routine completes only when the checks of done of the run
// passed (checkRoutineDone), so the end carries them as the end of a run
// of the model does (endTurn). A message that arrived meanwhile goes to
// the model instead, which starts with the context block of the routine.
func (a *Agent) finishRoutine(ctx context.Context, rs *runState, c *cursor, n int) error {
	rs.emitMu.Lock()
	defer rs.emitMu.Unlock()
	if messageSince(rs.events, n) {
		return nil
	}
	done := doneStateOf(rs)
	stop := RunStopAnswered
	if done == DoneChecked {
		stop = RunStopChecked
	}
	_, err := a.emitLocked(ctx, rs, EvRunFinished, RunFinishedData{Outcome: OutcomeCompleted, Output: c.routine.answer, Done: done, Stop: stop})
	return err
}

// routineFinishDue reports whether a run whose routine completed needs its
// fw.run.finished, and no message waits for the model.
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
		if r.Standing == RoutineLive && canaryPick(rr.rs.id, r.CanaryShare()) {
			mode = RoutineCanary
		}
		out[i] = routineVerdict{
			data: RoutineDecidedData{Routine: r.Name, Version: r.Version, Standing: r.Standing, Mode: mode, Applies: true, Decider: r.Decider,
				Risk: routineRisk(r, rr.tools)},
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
			d.Applies, d.Reason = routineApplies(r, own)
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
	// The runner asks through the tool decide of the kernel, with the
	// middleware of every tool, whatever tools.builtin holds.
	tool := rr.a.wrapTool(DecideTool())
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
			return nil, "", 0, ClipText(strings.TrimPrefix(text, "decide: "), 300, "the error"), nil
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
			return "", label + ": error: " + ClipText(TextOf(res.Content), 200, "the result"), nil
		case failedExit(res.ExitCode):
			// A command that failed is a failed step or check, although
			// the tool reports it as a normal result for the model.
			return "", fmt.Sprintf("%s: exit code %d: %s", label, *res.ExitCode, ClipText(TextOf(res.Content), 200, "the result")), nil
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
// with routine set, and fw.tool.responded. A call of a check run has no
// approval flow: a call that the policy asks for does not run. It returns
// the result, or why the call did not run. An error means that the run
// stopped or the ledger failed.
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
	callID := rr.prefix + NewID()
	use := Block{Type: BlockToolUse, ID: callID, Name: spec.Name, Input: input}
	pctx := a.policyCtx(ctx, rs, callID)
	d := rr.policy.Decide(pctx, ResolveCall(pctx, t, ToolCall{RunID: rs.id, Agent: rs.agent, CallID: callID, Spec: spec, Input: input}))
	switch d.Verdict {
	case VerdictAllow:
	case VerdictAsk:
		if rr.check {
			why := "the policy asks before this tool"
			if d.Reason != "" {
				why += " (" + d.Reason + ")"
			}
			return ToolResult{}, why + ", and a check runs only tools that the policy allows without a question", nil
		}
		var aborted atomic.Bool
		dec, err := a.waitDecision(ctx, ctx, rs, ApprovalRequestedData{Tool: spec.Name, CallID: callID, Input: input, Reason: d.Reason, Wait: WaitApproval}, &aborted)
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
// live without completing, and one comparison of the calls for each
// rehearsal that applies, at the end of a run that the model completed.
// It runs once per run. The caller holds rs.emitMu and appends
// fw.run.finished next.
func (a *Agent) compareLocked(ctx context.Context, rs *runState, c *cursor, output string) error {
	routines := a.profileOf(c.queued).Routines
	if len(routines) == 0 || !c.answered {
		return nil
	}
	for _, d := range routineComparisons(routines, c.routine, c.queued, rs.events, output) {
		if _, err := a.emitLocked(ctx, rs, EvRoutineCompared, d); err != nil {
			return err
		}
	}
	return nil
}

// routineComparisons returns the comparisons of a run from its cursor: for
// each routine that the run decided in mode shadow or canary, or in mode
// live without completing, and each of its questions with observe, what
// the routine said against what the agent did. For each routine that the
// run decided in mode shadow or canary and that applies, it adds the
// comparison of the calls: its steps and its checks, rendered on this run
// with its own answers, against the calls of the agent and their results.
// So a routine without questions gains agreement too. A run that the
// routine would hand to the agent, because a find finds no value, gets no
// comparison of the calls. A run compares once, and a branch never
// compares the decisions that it kept from its source. q is the start of
// the run, and output is its output text.
func routineComparisons(routines []Routine, r routineCursor, q RunQueuedData, events []Event, output string) []RoutineComparedData {
	if r.compared {
		return nil
	}
	var out []RoutineComparedData
	var calls []RoutineCall
	read := false
	for _, d := range r.decided[r.kept:] {
		if d.Mode == RoutineLive && r.ok && d.Routine == r.live {
			continue
		}
		i := slices.IndexFunc(routines, func(x Routine) bool { return x.Name == d.Routine && x.Version == d.Version })
		if i < 0 {
			continue
		}
		rt := routines[i]
		said := map[string]string{}
		for _, name := range rt.QuestionNames() {
			q := rt.Questions[name]
			ans, ok := d.Answers[name]
			if !ok {
				continue
			}
			said[name] = SaidOf(q, ans)
			observed, ok := ObserveAnswer(q, output, events)
			if !ok {
				continue
			}
			out = append(out, RoutineComparedData{
				Routine: rt.Name, Version: rt.Version, Mode: d.Mode, Question: name,
				Said: said[name], Observed: observed,
				Agree: strings.EqualFold(strings.TrimSpace(said[name]), strings.TrimSpace(observed)),
			})
		}
		if !d.Applies || (d.Mode != RoutineShadow && d.Mode != RoutineCanary) || len(rt.Steps)+len(rt.After) == 0 {
			continue
		}
		if !read {
			calls, read = ModelCalls(events), true
		}
		data := RoutineData(q.Input, q.Metadata)
		for k, v := range said {
			data[k] = v
		}
		cmp := CompareRoutineCalls(rt, data, calls)
		if cmp.Handed != "" {
			continue
		}
		c := RoutineComparedData{Routine: rt.Name, Version: rt.Version, Mode: d.Mode, Calls: true, Agree: cmp.Agree()}
		if !c.Agree {
			place := fmt.Sprintf("step %d", cmp.Step)
			if cmp.Step > len(rt.Steps) {
				place = fmt.Sprintf("check %d", cmp.Step-len(rt.Steps))
			}
			c.Step, c.Said, c.Observed = cmp.Step, cmp.Rendered, cmp.Recorded
			c.Reason = place + " " + cmp.Tool + ": " + cmp.Reason
		}
		out = append(out, c)
	}
	return out
}
