package fw

// This file is the definition of done of a run (docs/ARCHITECTURE.md
// section 24). A profile can name checks: tool calls that the kernel makes,
// with no model, when the model ends a turn with its answer. The run
// finishes as completed only when every check passes. A failed pass gives
// the model the failures and one more turn, up to done.attempts failed
// passes. Then the run fails with the reason "not done". So a completed run
// of a profile with a definition of done is a run whose work the kernel
// checked, not only a run whose model stopped.
//
// A check passes when its call runs, its result is no error, its exit code
// is zero when the tool reports one, and its text holds every string of
// contains and no string of lacks. The checks run under the policy of the
// run, and a check never asks a person: a call that the policy asks for
// fails the check. The checks run before the grader of an outcome, because
// a check is cheap and certain, and a grader is neither.
//
// Each call of a check gets fresh tool state (RunInfo.Fresh): bash runs it
// in a new shell in the directory of the run, never in the shell of the
// run, so the model cannot change a check with a cd or a variable.
//
// Every step reads the ledger. A restart that cuts a pass off runs the
// pass again, so a check must be safe to run twice, as a test or a read
// is. The author of the profile writes the checks, not the model.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// EvDoneChecked is the event of one pass of the checks of the definition
// of done of a run. Its data is DoneCheckedData.
const EvDoneChecked = "fw.done.checked"

// NoteDone is the note kind of the message from the kernel that gives a
// run the failures of its checks.
const NoteDone = "done"

// The values of RunFinishedData.Done and of RunDone.State. A run without a
// definition of done has none.
const (
	// DoneChecked: every check of the latest pass passed.
	DoneChecked = "checked"
	// DoneFixing: a check failed, and the model has one more turn to fix
	// the work. Only RunDone.State takes it.
	DoneFixing = "fixing"
	// DoneFailed: a check failed, and the run ends as failed.
	DoneFailed = "failed"
)

// Bounds and defaults of a definition of done.
const (
	// MaxDoneChecks is the most checks of a definition of done.
	MaxDoneChecks = 8
	// DefaultDoneAttempts is the number of failed passes that end a run
	// whose block sets none.
	DefaultDoneAttempts = 3
	// MaxDoneAttempts is the largest done.attempts.
	MaxDoneAttempts = 10
	// DefaultDoneTimeout is the time limit of one check whose block sets
	// none.
	DefaultDoneTimeout = 5 * time.Minute
	// MaxDoneTimeout is the longest time limit of one check, the time
	// limit of a tool call.
	MaxDoneTimeout = 10 * time.Minute
	// maxDoneNameBytes bounds the name of a check.
	maxDoneNameBytes = 200
	// maxDoneMatchBytes bounds each string of contains and lacks.
	maxDoneMatchBytes = 4 << 10
	// maxDoneMatches bounds the strings of contains and lacks of a check.
	maxDoneMatches = 16
	// doneResultBytes is the text of a result that a check reads.
	doneResultBytes = 256 << 10
	// doneWhyBytes bounds the failure of one check in the event and in the
	// note to the model.
	doneWhyBytes = 2 << 10
	// doneCallPrefix starts the call id of every call of a check.
	doneCallPrefix = "done_"
)

// Done is the definition of done of a run: the checks that the work of the
// run must pass before the run finishes as completed. Attempts is the
// number of failed passes that end the run as failed: each failed pass
// before it gives the model one more turn with the failures. Zero means
// DefaultDoneAttempts, and 1 gives no other turn. Timeout bounds each
// check: zero means DefaultDoneTimeout, and the limit is MaxDoneTimeout.
//
// A child run never takes the block of its named profile, as for an
// outcome: only the block of its inline profile applies to it. An inline
// profile adds its checks to the checks of the named profile and never
// removes one, so a request cannot weaken the definition of done that the
// owner of an agent wrote.
type Done struct {
	Checks   []DoneCheck   `yaml:"checks,omitempty" json:"checks,omitempty"`
	Attempts int           `yaml:"attempts,omitempty" json:"attempts,omitempty"`
	Timeout  time.Duration `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// DoneCheck is one check of a definition of done: a tool call and what its
// result text must hold (Contains) and must not hold (Lacks), as
// substrings. Name says in a few words what the check shows, such as "the
// tests pass", for a person and for the model. Input is a JSON object as
// text; empty means {}.
type DoneCheck struct {
	Name     string   `yaml:"name,omitempty" json:"name,omitempty"`
	Tool     string   `yaml:"tool" json:"tool"`
	Input    string   `yaml:"input,omitempty" json:"input,omitempty"`
	Contains []string `yaml:"contains,omitempty" json:"contains,omitempty"`
	Lacks    []string `yaml:"lacks,omitempty" json:"lacks,omitempty"`
}

// label names the check for a person: its name, else its tool.
func (c DoneCheck) label(i int) string {
	if c.Name != "" {
		return fmt.Sprintf("check %d %q (%s)", i+1, c.Name, c.Tool)
	}
	return fmt.Sprintf("check %d (%s)", i+1, c.Tool)
}

// on reports whether the block has a check.
func (d *Done) on() bool { return d != nil && len(d.Checks) > 0 }

// attempts returns the number of failed passes that end the run.
func (d *Done) attempts() int {
	if d.Attempts > 0 {
		return d.Attempts
	}
	return DefaultDoneAttempts
}

// timeout returns the time limit of one check.
func (d *Done) timeout() time.Duration {
	if d.Timeout > 0 {
		return d.Timeout
	}
	return DefaultDoneTimeout
}

// CheckDone reports with ErrInvalidRequest whether the kernel accepts a
// definition of done: at most MaxDoneChecks checks, a tool name in each, an
// input that is a JSON object, attempts from 0 to MaxDoneAttempts, and a
// timeout from 0 to MaxDoneTimeout. A nil block is valid.
func CheckDone(d *Done) error {
	if d == nil {
		return nil
	}
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrInvalidRequest}, args...)...)
	}
	if len(d.Checks) > MaxDoneChecks {
		return bad("done.checks has %d checks; a definition of done has at most %d", len(d.Checks), MaxDoneChecks)
	}
	if len(d.Checks) == 0 && (d.Attempts != 0 || d.Timeout != 0) {
		return bad("done.attempts and done.timeout need done.checks; a definition of done without a check is off")
	}
	switch {
	case d.Attempts < 0 || d.Attempts > MaxDoneAttempts:
		return bad("done.attempts is %d; it must be from 0 to %d", d.Attempts, MaxDoneAttempts)
	case d.Timeout < 0:
		return bad("done.timeout %s is negative", d.Timeout)
	case d.Timeout > MaxDoneTimeout:
		return bad("done.timeout %s is longer than the limit of %s", d.Timeout, MaxDoneTimeout)
	}
	for i, c := range d.Checks {
		if t := strings.TrimSpace(c.Tool); t == "" || strings.ContainsAny(t, " \t\r\n") {
			return bad("done.checks[%d].tool %q is not a tool name", i, c.Tool)
		}
		if len(c.Name) > maxDoneNameBytes || strings.ContainsAny(c.Name, "\r\n") {
			return bad("done.checks[%d].name is longer than %d bytes or holds a line break", i, maxDoneNameBytes)
		}
		if _, err := doneInput(c.Input); err != nil {
			return bad("done.checks[%d].input is not a JSON object", i)
		}
		if n := len(c.Contains) + len(c.Lacks); n > maxDoneMatches {
			return bad("done.checks[%d] has %d strings in contains and lacks; the most is %d", i, n, maxDoneMatches)
		}
		for _, s := range append(append([]string(nil), c.Contains...), c.Lacks...) {
			if s == "" || len(s) > maxDoneMatchBytes {
				return bad("done.checks[%d] has an empty string, or one longer than %d bytes, in contains or lacks", i, maxDoneMatchBytes)
			}
		}
	}
	return nil
}

// doneInput returns the input of a check as compact JSON: {} for an empty
// one, and an error for text that is not a JSON object.
func doneInput(s string) (json.RawMessage, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "null" {
		return json.RawMessage("{}"), nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &obj); err != nil || obj == nil {
		return nil, fmt.Errorf("fw: the input is not a JSON object")
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(obj); err != nil {
		return nil, err
	}
	return json.RawMessage(strings.TrimSpace(b.String())), nil
}

// mergeDone returns the definition of done of a named profile with the
// checks of an overlay added. The attempts and the timeout of the base win
// when it sets them. Neither argument changes.
func mergeDone(base, over *Done) *Done {
	switch {
	case !over.on():
		if base == nil {
			return nil
		}
		out := *base
		out.Checks = append([]DoneCheck(nil), base.Checks...)
		return &out
	case !base.on():
		out := *over
		out.Checks = append([]DoneCheck(nil), over.Checks...)
		return &out
	}
	out := *base
	out.Checks = append(append([]DoneCheck(nil), base.Checks...), over.Checks...)
	if out.Attempts == 0 {
		out.Attempts = over.Attempts
	}
	if out.Timeout == 0 {
		out.Timeout = over.Timeout
	}
	return &out
}

// resolveDone returns the definition of done of a new run for
// fw.run.queued, with its defaults, or nil when it has none: the block of
// the named profile for a run that is not a child, with the checks of the
// inline profile added.
func (a *Agent) resolveDone(req RunRequest, named AgentProfile, parent *runState) *Done {
	var d *Done
	if parent == nil && req.Metadata[MetaParent] == "" {
		d = named.Done
	}
	if req.Profile != nil {
		d = mergeDone(d, req.Profile.Done)
	}
	if !d.on() {
		return nil
	}
	out := *d
	out.Attempts, out.Timeout = d.attempts(), d.timeout()
	out.Checks = make([]DoneCheck, len(d.Checks))
	for i, c := range d.Checks {
		in, _ := doneInput(c.Input)
		c.Input = string(in)
		c.Contains, c.Lacks = append([]string(nil), c.Contains...), append([]string(nil), c.Lacks...)
		out.Checks[i] = c
	}
	return &out
}

// DoneCheckedData is the payload of fw.done.checked, one pass of the
// checks of a run. Passed is true when every check passed. Attempt is the
// number of the failed pass, from 1, for a pass that failed, and one more
// than the failed passes before it for a pass that passed. Attempts is the
// number of failed passes that end the run. Answer is the sequence number
// of the fw.model.responded whose answer the pass checked. Continue is
// true when the model gets one more turn with the failures. Reason says
// why a failed pass does not continue.
type DoneCheckedData struct {
	Passed   bool         `json:"passed"`
	Checks   []DoneResult `json:"checks"`
	Attempt  int          `json:"attempt"`
	Attempts int          `json:"attempts"`
	Answer   uint64       `json:"answer"`
	Continue bool         `json:"continue,omitempty"`
	Reason   string       `json:"reason,omitempty"`
}

// DoneResult is the result of one check of a pass. Why says why a check
// failed, with the head and the tail of the result.
type DoneResult struct {
	Name   string `json:"name,omitempty"`
	Tool   string `json:"tool"`
	Passed bool   `json:"passed"`
	Why    string `json:"why,omitempty"`
}

// RunDone is the state of the definition of done of a run, as the run
// object shows it: State is DoneChecked, DoneFixing, or DoneFailed, Attempt
// and Attempts are those of the latest pass, and Checks are its results. A
// consumer that sees only the lifecycle events of the run, such as the
// front of a fleet, gets the state from fw.run.finished and no checks. A
// follow-up to a finished run clears it with the output.
type RunDone struct {
	State    string       `json:"state"`
	Attempt  int          `json:"attempt,omitempty"`
	Attempts int          `json:"attempts,omitempty"`
	Checks   []DoneResult `json:"checks,omitempty"`
}

// applyDone folds a fw.done.checked into the run object.
func (r *Run) applyDone(e Event) {
	var d DoneCheckedData
	if e.Decode(&d) != nil {
		return
	}
	state := DoneChecked
	switch {
	case d.Passed:
	case d.Continue:
		state = DoneFixing
	default:
		state = DoneFailed
	}
	r.Done = &RunDone{State: state, Attempt: d.Attempt, Attempts: d.Attempts, Checks: d.Checks}
}

// donePasses returns the passes that count for the answer at the sequence
// number answer: those for the answers since the latest message that did
// not come from the kernel, such as a follow-up of a person, which starts
// the passes again.
func donePasses(evs []Event, answer uint64) []DoneCheckedData {
	var start uint64
	for _, e := range evs {
		if e.Seq >= answer {
			break
		}
		if e.Type != EvMessageReceived {
			continue
		}
		var d struct {
			Origin string `json:"origin"`
		}
		if e.Decode(&d) == nil && d.Origin != OriginKernel {
			start = e.Seq
		}
	}
	var out []DoneCheckedData
	for _, e := range evs {
		if e.Type != EvDoneChecked {
			continue
		}
		var d DoneCheckedData
		if e.Decode(&d) == nil && d.Answer > start {
			out = append(out, d)
		}
	}
	return out
}

// checkDone runs the checks of the definition of done of a run on the
// answer of the model before the run finishes. endTurn calls it with the
// cursor of the step, before the grader of an outcome. It returns true
// when it appended events and the driver must read the log again, and
// false when endTurn goes on: the run has no definition of done, a message
// arrived, or the latest pass of this answer passed.
//
// One step does one thing. A step with the turn open closes the turn. The
// next step runs the checks and records the pass, with the note for the
// model or the end of the run in the same step. A restart between the pass
// and its note or its end gives them at the next step.
func (a *Agent) checkDone(ctx context.Context, rs *runState, c *cursor, n int) (bool, error) {
	d := rs.queuedData().Done
	if !d.on() || c.resp == nil {
		return false, nil
	}
	passes := donePasses(rs.eventLog(), c.respSeq)
	if k := len(passes); k > 0 && passes[k-1].Answer == c.respSeq {
		switch last := passes[k-1]; {
		case last.Passed:
			return false, nil
		case last.Continue:
			_, err := a.emit(ctx, rs, EvMessageReceived, doneMessage(last))
			return true, err
		default:
			rs.emitMu.Lock()
			defer rs.emitMu.Unlock()
			return true, a.notDoneLocked(ctx, rs, c, n, last)
		}
	}
	rs.emitMu.Lock()
	if freshSince(rs, c, n) {
		rs.emitMu.Unlock()
		return false, nil
	}
	if c.turnOpen {
		_, err := a.emitLocked(ctx, rs, EvTurnFinished, TurnData{Turn: c.turns})
		rs.emitMu.Unlock()
		return true, err
	}
	rs.emitMu.Unlock()

	failed := 0
	for _, p := range passes {
		if !p.Passed {
			failed++
		}
	}
	pass := DoneCheckedData{Attempt: failed + 1, Attempts: d.attempts(), Answer: c.respSeq}
	if err := a.runDone(ctx, rs, c, d, &pass); err != nil {
		return true, err
	}
	if !pass.Passed {
		next := *c
		next.turnOpen, next.keptOpen = false, false
		if pass.Attempt >= pass.Attempts {
			pass.Reason = fmt.Sprintf("the checks failed at the last of %d attempts", pass.Attempts)
		} else if ov, over := a.overBudget(rs, &next); over {
			pass.Reason = "the budget leaves no room for another turn: " + ov.why()
		} else {
			pass.Continue = true
		}
	}
	a.log.Info("done checked", "run", rs.id, "passed", pass.Passed, "attempt", pass.Attempt,
		"attempts", pass.Attempts, "continue", pass.Continue, "reason", pass.Reason)
	rs.emitMu.Lock()
	defer rs.emitMu.Unlock()
	if _, err := a.emitLocked(ctx, rs, EvDoneChecked, pass); err != nil {
		return true, err
	}
	switch {
	case pass.Passed:
		return true, nil
	case pass.Continue:
		_, err := a.emitLocked(ctx, rs, EvMessageReceived, doneMessage(pass))
		return true, err
	}
	return true, a.notDoneLocked(ctx, rs, c, n, pass)
}

// freshSince reports whether a message arrived after the latest model
// request or after the n events that the step read. The caller holds
// rs.emitMu.
func freshSince(rs *runState, c *cursor, n int) bool {
	if c.unseen {
		return true
	}
	for _, e := range rs.events[n:] {
		if e.Type == EvMessageReceived {
			return true
		}
	}
	return false
}

// notDoneLocked finishes a run whose last pass failed with no other turn:
// failed, with the answer of the model as the output, so a person reads
// what the model claimed beside the checks that refute it. A message that
// arrived during the pass keeps the run open instead. The caller holds
// rs.emitMu.
func (a *Agent) notDoneLocked(ctx context.Context, rs *runState, c *cursor, n int, pass DoneCheckedData) error {
	if freshSince(rs, c, n) {
		return nil
	}
	_, err := a.emitLocked(ctx, rs, EvRunFinished, RunFinishedData{
		Outcome: OutcomeFailed,
		Output:  TextOf(c.resp.Content),
		Error:   notDoneText(pass),
		Done:    DoneFailed,
	})
	return err
}

// notDoneText is the error of a run that the checks ended.
func notDoneText(pass DoneCheckedData) string {
	var names []string
	for i, r := range pass.Checks {
		if !r.Passed {
			names = append(names, DoneCheck{Name: r.Name, Tool: r.Tool}.label(i))
		}
	}
	s := "not done: " + strings.Join(names, ", ") + " failed"
	if pass.Reason != "" {
		s += "; " + pass.Reason
	}
	return s
}

// runDone runs every check of d once and sets the results and Passed in
// pass. It returns an error only when the run stops: its context ended or
// the agent shuts down. A restart then runs the pass again.
func (a *Agent) runDone(ctx context.Context, rs *runState, c *cursor, d *Done, pass *DoneCheckedData) error {
	prof := a.profileOf(c.queued)
	_, tools := a.runTools(ctx, rs, prof)
	rr := &routineRun{a: a, rs: rs, prof: prof, tools: tools, policy: a.policyFor(rs, prof), prefix: doneCallPrefix, check: true}
	// The limit of tool calls of the run counts the calls of the model too.
	rr.calls = c.toolCalls - c.keptCalls
	pass.Passed = true
	pass.Checks = make([]DoneResult, 0, len(d.Checks))
	for i, chk := range d.Checks {
		why, err := a.oneDone(ctx, rr, tools, chk, d.timeout())
		if err != nil {
			return err
		}
		r := DoneResult{Name: chk.Name, Tool: chk.Tool, Passed: why == ""}
		if !r.Passed {
			pass.Passed = false
			r.Why = CutMiddle(why, doneWhyBytes, doneWhyBytes)
			a.log.Info("done check failed", "run", rs.id, "check", chk.label(i), "why", oneLineOf(why, 200))
		}
		pass.Checks = append(pass.Checks, r)
	}
	return nil
}

// oneDone runs one check and returns why it failed, or "" when it passed.
func (a *Agent) oneDone(ctx context.Context, rr *routineRun, tools map[string]Tool, chk DoneCheck, timeout time.Duration) (string, error) {
	t, ok := tools[chk.Tool]
	if !ok {
		return "the agent has no tool " + chk.Tool, nil
	}
	in, err := doneInput(chk.Input)
	if err != nil {
		return "the input of the check is not a JSON object", nil
	}
	ct := &checkTool{Tool: t, deadline: time.Now().Add(timeout), max: doneResultBytes, fresh: true}
	res, why, err := rr.call(ctx, "", ct, in)
	switch {
	case err != nil:
		return "", err
	case why != "":
		return why, nil
	case ct.late:
		return fmt.Sprintf("the check did not finish within %s", timeout), nil
	}
	text := TextOf(res.Content)
	switch {
	case res.IsError:
		return "the result is an error:\n" + text, nil
	case failedExit(res.ExitCode):
		return fmt.Sprintf("the exit code is %d:\n%s", *res.ExitCode, text), nil
	}
	for _, s := range chk.Contains {
		if !strings.Contains(text, s) {
			return fmt.Sprintf("the result lacks %q:\n%s", s, text), nil
		}
	}
	for _, s := range chk.Lacks {
		if strings.Contains(text, s) {
			return fmt.Sprintf("the result holds %q:\n%s", s, text), nil
		}
	}
	return "", nil
}

// doneMessage is the message from the kernel that gives the model the
// failures of a pass and one more turn.
func doneMessage(pass DoneCheckedData) MessageReceivedData {
	var b strings.Builder
	failed := 0
	for _, r := range pass.Checks {
		if !r.Passed {
			failed++
		}
	}
	fmt.Fprintf(&b, "The definition of done of this run checks the work when you end your turn. "+
		"%d of %d checks failed. This was failed attempt %d of at most %d.\n", failed, len(pass.Checks), pass.Attempt, pass.Attempts)
	for i, r := range pass.Checks {
		if r.Passed {
			continue
		}
		fmt.Fprintf(&b, "\n%s failed: %s\n", DoneCheck{Name: r.Name, Tool: r.Tool}.label(i), r.Why)
	}
	b.WriteString("\nFix the work so that every check passes. Do not change a check to make it pass. " +
		"The kernel runs the checks again when you end your turn. " +
		"Then end your turn with your complete final answer. It replaces your earlier answer.")
	return MessageReceivedData{Role: RoleUser, Content: []Block{Text(b.String())}, Origin: OriginKernel, Note: NoteDone}
}

// doneStateOf returns RunFinishedData.Done for a run that completes at the
// answer of the cursor: DoneChecked when the run has a definition of done,
// and "" when it has none.
func doneStateOf(rs *runState) string {
	if rs.queuedData().Done.on() {
		return DoneChecked
	}
	return ""
}

// oneLineOf returns s on one line, cut to n bytes, for a log line.
func oneLineOf(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return ClipText(s, n, "the text")
	}
	return s
}
