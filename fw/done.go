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
// a check is cheap and certain, and a grader is neither. A check of the
// answer (DoneAnswer) makes no call: its contains and lacks apply to the
// final answer of the run.
//
// Each call of a check gets fresh tool state (RunInfo.Fresh): bash runs it
// in a new shell in the directory of the run, never in the shell of the
// run, so the model cannot change a check with a cd or a variable.
//
// The checks have a tool budget of their own: at most MaxDoneChecks calls
// a pass, each within the time limit of the block. The limits of tool
// calls, of tokens, and of cost of the run and of its tree neither refuse
// them nor count them, because a check calls no model. So a run that used
// its tool calls still has its work checked, also a run that stops at such
// a limit: its checks run after its final report (limitDone). Only the
// deadline of the run, the limit of a person's time, stops a check.
//
// A pass that passed can be weak: it shows nothing about the work of this
// run (weakPass), such as a pass of a run that changed nothing, unless a
// check of the answer with contains or lacks passed. Then the run completes as answered, not as checked
// (CheckedDone). In a run that is held back from Know-how, a check of the
// memory or the recall tool is skipped, not failed, because the kernel
// took that tool out of the run.
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

// CheckedDone reports whether a run counts as checked: its definition of
// done passed (DoneChecked), and the pass was not weak. A weak pass shows
// nothing about the work of this run, such as a check that only reads a
// file that was there before the run. A consumer of the lifecycle events
// passes RunFinishedData.Done and DoneWeak; a reader of the run object
// passes RunDone.State and Weak.
func CheckedDone(state string, weak bool) bool { return state == DoneChecked && !weak }

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

// DoneAnswer is the reserved tool name of a check of the answer of a run
// (DoneCheck.OfAnswer). Such a check makes no tool call, also when an
// agent has a tool of that name.
const DoneAnswer = "answer"

// DoneCheck is one check of a definition of done: a tool call and what its
// result text must hold (Contains) and must not hold (Lacks), as
// substrings. Name says in a few words what the check shows, such as "the
// tests pass", for a person and for the model. Input is a JSON object as
// text; empty means {}.
//
// A check whose Tool is DoneAnswer checks the final answer of the run
// instead of the result of a call: Contains and Lacks apply to the text of
// the answer, and it takes no input. It is the check of a job that only
// reads, whose product is its answer.
type DoneCheck struct {
	Name     string   `yaml:"name,omitempty" json:"name,omitempty"`
	Tool     string   `yaml:"tool" json:"tool"`
	Input    string   `yaml:"input,omitempty" json:"input,omitempty"`
	Contains []string `yaml:"contains,omitempty" json:"contains,omitempty"`
	Lacks    []string `yaml:"lacks,omitempty" json:"lacks,omitempty"`
}

// Weak reports whether the check alone can show nothing about the work of
// a run: it names no Contains and no Lacks, and either its call only reads
// a file or a page by its path (ReadsPath), or it checks the answer
// (OfAnswer). Such a check passes for every path that exists, also one
// that was there before the run, and for every answer that is not empty.
// A pass whose every check is weak is a weak pass. A check of the answer
// that names a Contains or a Lacks is strong, also in a run that changed
// nothing (weakPass). The profile editor of the workbench warns about a
// weak check with the same rule.
func (c DoneCheck) Weak() bool {
	if len(c.Contains) > 0 || len(c.Lacks) > 0 {
		return false
	}
	return c.OfAnswer() || ReadsPath(c.Tool, json.RawMessage(c.Input))
}

// OfAnswer reports whether the check tests the final answer of the run
// (DoneAnswer) and makes no tool call.
func (c DoneCheck) OfAnswer() bool { return strings.TrimSpace(c.Tool) == DoneAnswer }

// pathKeys are the keys of an input that name the path of a file or a
// page.
var pathKeys = []string{"path", "file", "file_path", "filePath", "filename"}

// ReadsPath reports whether a call of the tool name with the input only
// reads a file or a page by its path. The input names a path (the key
// path, file, file_path, filePath, or filename), and either its command
// is view or read, as for the files and the memory tools, or the first or
// the last word of the name of the tool is read or view, as for read_file
// or front.pages_read. The words of a name are split at '_' and '-', after
// the prefix of an MCP server, which ends at the last '.'.
func ReadsPath(tool string, input json.RawMessage) bool {
	var in map[string]json.RawMessage
	if json.Unmarshal(input, &in) != nil {
		return false
	}
	named := false
	for _, k := range pathKeys {
		var p string
		if json.Unmarshal(in[k], &p) == nil && strings.TrimSpace(p) != "" {
			named = true
			break
		}
	}
	if !named {
		return false
	}
	reads := func(w string) bool { w = strings.ToLower(w); return w == "view" || w == "read" }
	if raw, ok := in["command"]; ok {
		var cmd string
		return json.Unmarshal(raw, &cmd) == nil && reads(cmd)
	}
	name := tool[strings.LastIndex(tool, ".")+1:]
	words := strings.FieldsFunc(name, func(r rune) bool { return r == '_' || r == '-' })
	return len(words) > 0 && (reads(words[0]) || reads(words[len(words)-1]))
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
		in, err := doneInput(c.Input)
		if err != nil {
			return bad("done.checks[%d].input is not a JSON object", i)
		}
		if c.OfAnswer() && string(in) != "{}" {
			return bad("done.checks[%d] checks the answer of the run (tool %s), which takes no input", i, DoneAnswer)
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
// of the fw.model.responded whose answer the pass checked, and 0 for a
// pass after a routine or at a limit, which no answer of a turn started.
// Continue is true when the model gets one more turn with the failures.
// Reason says why a failed pass does not continue. Weak is true for a pass
// that passed and shows nothing about the work of the run (weakPass).
// Limit names the limit of the budget at which the run stopped, such as
// maxToolCalls, for a pass that ran after that stop (limitDone).
type DoneCheckedData struct {
	Passed   bool         `json:"passed"`
	Weak     bool         `json:"weak,omitempty"`
	Checks   []DoneResult `json:"checks"`
	Attempt  int          `json:"attempt"`
	Attempts int          `json:"attempts"`
	Answer   uint64       `json:"answer"`
	Continue bool         `json:"continue,omitempty"`
	Reason   string       `json:"reason,omitempty"`
	Limit    string       `json:"limit,omitempty"`
}

// DoneResult is the result of one check of a pass. Why says why a check
// failed, with the head and the tail of the result. Skipped says why the
// kernel did not run a check, such as a check of the memory tool in a run
// that is held back from Know-how. A skipped check counts as passed, and
// it shows nothing about the work of the run.
type DoneResult struct {
	Name    string `json:"name,omitempty"`
	Tool    string `json:"tool"`
	Passed  bool   `json:"passed"`
	Why     string `json:"why,omitempty"`
	Skipped string `json:"skipped,omitempty"`
}

// RunDone is the state of the definition of done of a run, as the run
// object shows it: State is DoneChecked, DoneFixing, or DoneFailed, Weak
// marks a pass that passed and shows nothing about the work of the run,
// Attempt and Attempts are those of the latest pass, and Checks are its
// results. A consumer that sees only the lifecycle events of the run, such
// as the front of a fleet, gets the state and Weak from fw.run.finished and
// no checks. A follow-up to a finished run clears it with the output.
type RunDone struct {
	State    string       `json:"state"`
	Weak     bool         `json:"weak,omitempty"`
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
	r.Done = &RunDone{State: state, Weak: d.Passed && d.Weak, Attempt: d.Attempt, Attempts: d.Attempts, Checks: d.Checks}
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
	if err := a.runDone(ctx, rs, c, d, &pass, TextOf(c.resp.Content)); err != nil {
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

// doneAtLimit reports whether a run that stops at the limit o of its
// budget runs its checks of done before it finishes (limitDone): it has a
// definition of done, the limit is a limit of turns, of tool calls, of
// tokens, or of cost, and the run holds the limit itself. The deadline is
// the limit of the working time that a person gave the run, so no check
// runs past it. A run that stops at a limit that an ancestor holds for the
// tree runs none, because the tree must stop now.
func doneAtLimit(rs *runState, o overrun) bool {
	return rs.queuedData().Done.on() && o.limit != "deadline" && (o.owner == nil || o.owner == rs)
}

// limitDone runs the checks of the definition of done of a run that
// stopped at a limit of its budget (doneAtLimit), after its final report
// when it makes one, and returns the pass. A run that stopped at a limit
// may still have done its work, such as a run whose last tool call did
// it, so the checks give it its state of done. The checks cost no tokens
// and have a tool budget of their own (runDone). A check of the answer
// reads output, the final report or the output that the run finishes
// with. The pass has the answer 0 and names the limit. A pass that failed
// gives the model no other turn, because the run stopped. The caller
// records the pass with the end of the run (finishAtLimitLocked), which
// still names the limit.
func (a *Agent) limitDone(ctx context.Context, rs *runState, c *cursor, d BudgetExceededData, output string) (*DoneCheckedData, error) {
	done := rs.queuedData().Done
	failed := 0
	for _, p := range donePasses(rs.eventLog(), c.respSeq) {
		if !p.Passed {
			failed++
		}
	}
	pass := DoneCheckedData{Attempt: failed + 1, Attempts: done.attempts(), Limit: d.Limit}
	if err := a.runDone(ctx, rs, c, done, &pass, output); err != nil {
		return nil, err
	}
	if !pass.Passed {
		pass.Reason = "the run stopped at " + limitPhrase(d, rs.id) + ", so the model gets no other turn"
	}
	a.log.Info("done checked at a limit", "run", rs.id, "limit", d.Limit, "passed", pass.Passed, "weak", pass.Weak)
	return &pass, nil
}

// finishAtLimitLocked records the pass of the checks at a limit, when one
// ran, and finishes the run as failed with the limit in its error and its
// stop. The pass gives the end its state of done: DoneChecked with the
// mark of a weak pass, or DoneFailed. The caller holds rs.emitMu.
func (a *Agent) finishAtLimitLocked(ctx context.Context, rs *runState, output string, d BudgetExceededData, pass *DoneCheckedData) error {
	end := RunFinishedData{Outcome: OutcomeFailed, Output: output, Error: "budget exceeded: " + d.text(rs.id), Stop: RunStopBudget(d.Limit)}
	if pass != nil {
		if _, err := a.emitLocked(ctx, rs, EvDoneChecked, *pass); err != nil {
			return err
		}
		end.Done = DoneFailed
		if pass.Passed {
			end.Done, end.DoneWeak = DoneChecked, pass.Weak
		}
	}
	_, err := a.emitLocked(ctx, rs, EvRunFinished, end)
	return err
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
		Stop:    RunStopNotDone,
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

// runDone runs every check of d once and sets the results, Passed, and
// Weak in pass. answer is the final answer of the run, which a check of
// the answer (DoneCheck.OfAnswer) reads with no tool call. The checks have
// a tool budget of their own (ownBudget), so a run that used its tool
// calls still runs them. In a run that is held back from Know-how, a check
// of a tool that the kernel took out of the run (KnowhowTool) is skipped.
// It returns an error only when the run stops: its context ended or the
// agent shuts down. A restart then runs the pass again.
func (a *Agent) runDone(ctx context.Context, rs *runState, c *cursor, d *Done, pass *DoneCheckedData, answer string) error {
	prof := a.profileOf(c.queued)
	_, tools := a.runTools(ctx, rs, prof)
	rr := &routineRun{a: a, rs: rs, prof: prof, tools: tools, policy: a.policyFor(rs, prof), prefix: doneCallPrefix, check: true, ownBudget: true}
	held := HeldBack(c.queued.Metadata)
	pass.Passed = true
	pass.Checks = make([]DoneResult, 0, len(d.Checks))
	for i, chk := range d.Checks {
		if _, ok := tools[chk.Tool]; !ok && held && KnowhowTool(chk.Tool) {
			pass.Checks = append(pass.Checks, DoneResult{Name: chk.Name, Tool: chk.Tool, Passed: true,
				Skipped: "this is a comparison run, which goes without what the agent learned, so it has no tool " + chk.Tool})
			continue
		}
		var why string
		var err error
		if chk.OfAnswer() {
			why = answerDone(chk, answer)
		} else {
			why, err = a.oneDone(ctx, rr, tools, chk, d.timeout())
		}
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
	pass.Weak = pass.Passed && weakPass(d, pass.Checks, rs.eventLog(), tools)
	return nil
}

// doneCall reports whether the event fw.tool.requested is a call of a
// check of a definition of done. Such a call has a tool budget of its own,
// so the count of the tool calls of a run and its usage leave it out.
func doneCall(e Event) bool {
	var d struct {
		CallID string `json:"callId"`
	}
	return e.Decode(&d) == nil && strings.HasPrefix(d.CallID, doneCallPrefix)
}

// weakPass reports whether a pass that passed shows nothing about the work
// of the run. That is so when no check of the pass that ran is strong: each
// one is weak (DoneCheck.Weak) or was skipped. It is so too when the run
// changed nothing (changedWork), because then every check of a call saw
// only what was there before the run. A strong check of the answer
// (DoneCheck.OfAnswer) makes the pass strong also then, because the answer
// is the product of a run that only reads, and it comes from this run.
// results are the results of the pass, in the order of the checks of d.
func weakPass(d *Done, results []DoneResult, events []Event, tools map[string]Tool) bool {
	strong := false
	for i, r := range results {
		if i >= len(d.Checks) || r.Skipped != "" || d.Checks[i].Weak() {
			continue
		}
		if d.Checks[i].OfAnswer() {
			return false
		}
		strong = true
	}
	return !strong || !changedWork(events, tools)
}

// answerDone runs a check of the answer (DoneCheck.OfAnswer) on the final
// answer of the run and returns why it failed, or "" when it passed: the
// answer is not empty, and it holds every string of Contains and no string
// of Lacks.
func answerDone(chk DoneCheck, answer string) string {
	if strings.TrimSpace(answer) == "" {
		return "the run has no answer"
	}
	return matchDone(chk, answer, "the answer")
}

// changedWork reports whether the run made a call of its own that can have
// changed something: a call of the model or of a routine, not of a check,
// that ran without an error result, of a tool that is not read-only, that
// is not a view or a read of a path (ReadsPath), and that is not a tool of
// Know-how (KnowhowTool), because a note of the agent is not the work of
// the run. A call that a restart cut off before its result counts too, and
// so does a change of a file of the workspace by such a call. A tool that
// the run lacks counts as one that changes, because nothing says that it
// only reads.
func changedWork(events []Event, tools map[string]Tool) bool {
	open := map[string]bool{}
	for _, e := range events {
		switch e.Type {
		case EvToolRequested:
			var d ToolRequestedData
			if e.Decode(&d) != nil || strings.HasPrefix(d.CallID, doneCallPrefix) || KnowhowTool(d.Tool) || ReadsPath(d.Tool, d.Input) {
				continue
			}
			if t, ok := tools[d.Tool]; ok && t.Spec().ReadOnly {
				continue
			}
			open[d.CallID] = true
		case EvToolResponded:
			var d ToolRespondedData
			if e.Decode(&d) != nil || !open[d.CallID] {
				continue
			}
			if !d.IsError {
				return true
			}
			delete(open, d.CallID)
		case EvFileChanged:
			var d FileChangedData
			if e.Decode(&d) == nil && d.CallID != "" && !strings.HasPrefix(d.CallID, doneCallPrefix) {
				return true
			}
		}
	}
	return len(open) > 0
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
	return matchDone(chk, text, "the result"), nil
}

// matchDone returns why text, which what names, fails the strings of a
// check: the first string of Contains that it lacks, or the first string
// of Lacks that it holds, with the text; "" when it passes.
func matchDone(chk DoneCheck, text, what string) string {
	for _, s := range chk.Contains {
		if !strings.Contains(text, s) {
			return fmt.Sprintf("%s lacks %q:\n%s", what, s, text)
		}
	}
	for _, s := range chk.Lacks {
		if strings.Contains(text, s) {
			return fmt.Sprintf("%s holds %q:\n%s", what, s, text)
		}
	}
	return ""
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

// doneOfLocked returns RunFinishedData.Done and DoneWeak for a run that
// completes at the answer of the cursor: DoneChecked when the run has a
// definition of done, with the mark of the pass of that answer that
// passed, and "" when it has none. The caller holds rs.emitMu.
func doneOfLocked(rs *runState, c *cursor) (string, bool) {
	if !rs.queuedData().Done.on() {
		return "", false
	}
	passes := donePasses(rs.events, c.respSeq)
	if k := len(passes); k > 0 && passes[k-1].Passed {
		return DoneChecked, passes[k-1].Weak
	}
	return DoneChecked, false
}

// oneLineOf returns s on one line, cut to n bytes, for a log line.
func oneLineOf(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return ClipText(s, n, "the text")
	}
	return s
}
