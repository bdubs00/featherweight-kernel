package fw

// This file is the outcome of a run (docs/ARCHITECTURE.md section 20). A
// profile or a request can name a rubric. When a run with a rubric ends a
// turn with its answer, the kernel starts a grader run as a child of the
// run, in this process. The grader reads the rubric, the task, the answer,
// and the files that the run changed, and ends its answer with a verdict.
// The kernel records each pass as fw.outcome.evaluated. A verdict of
// needs_revision gives the run one more turn with the notes of the grader,
// up to outcome.iterations passes. Then the run finishes with its last
// answer. Every step reads the ledger, so a restart in the middle of a pass
// takes the same grader run again (its idempotency key) and records each
// verdict once.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// EvOutcomeEvaluated is the event of one pass of the grader of a run. Its
// data is OutcomeEvaluatedData.
const EvOutcomeEvaluated = "fw.outcome.evaluated"

// NoteOutcome is the note kind of the message from the kernel that gives a
// run the notes of its grader for a revision.
const NoteOutcome = "outcome"

// The verdicts of fw.outcome.evaluated. The grader answers satisfied or
// needs_revision. The kernel records max_iterations for a needs_revision
// at the last pass, unparsed when no verdict can be read from the grader,
// and skipped when no grader run can start, such as in a tree that used up
// its budget.
const (
	GradeSatisfied     = "satisfied"
	GradeNeedsRevision = "needs_revision"
	GradeMaxIterations = "max_iterations"
	GradeUnparsed      = "unparsed"
	GradeSkipped       = "skipped"
)

// MaxOutcomeIterations is the most grader passes that outcome.iterations
// can ask for.
const MaxOutcomeIterations = 10

// The bounds of a grader run and of its input.
const (
	graderMaxTurns     = 6
	graderMaxToolCalls = 24
	graderDeadline     = 10 * time.Minute
	graderTaskBytes    = 16 << 10 // the input and the follow-ups of the run
	graderAnswerBytes  = 32 << 10 // the answer of the run
	graderFileBytes    = 64 << 10 // the content of the files of the run, all together
	graderNotesBytes   = 8 << 10  // the notes of one pass
	graderMaxFiles     = 200      // the files that the input lists
)

// Outcome asks for a grade of the answer of a run. Rubric is plain text
// that the grader applies, and an outcome without a rubric is off. Grader
// is the model of the grader, a provider/model selector or a name from
// models; empty means the model of the run, and there is no other default.
// Iterations is the most grader passes: 1 grades once and never asks for a
// revision, and 2 or more give the run one more turn with the notes of the
// grader after each needs_revision, up to this number of passes. Zero
// means 1.
//
// The block of a request replaces the block of the profile as a whole, so
// a request with an empty block turns the outcome of the profile off. The
// kernel resolves the outcome once, when it queues the run, and records it
// in fw.run.queued. A child run never takes the outcome of its profile:
// only a block in its own request turns grading on for it.
type Outcome struct {
	Rubric     string `yaml:"rubric,omitempty" json:"rubric,omitempty"`
	Grader     string `yaml:"grader,omitempty" json:"grader,omitempty"`
	Iterations int    `yaml:"iterations,omitempty" json:"iterations,omitempty"`
}

// on reports whether the outcome asks for a grade.
func (o *Outcome) on() bool { return o != nil && strings.TrimSpace(o.Rubric) != "" }

// passes returns the most grader passes of the outcome.
func (o *Outcome) passes() int { return min(max(o.Iterations, 1), MaxOutcomeIterations) }

// WithOutcome sets the outcome of the run. It replaces the outcome of the
// profile.
func WithOutcome(o Outcome) RunOption { return func(r *RunRequest) { r.Outcome = &o } }

// CheckOutcome reports with ErrInvalidRequest whether the kernel accepts an
// outcome block. cfg resolves a name of the grader through models; a nil
// cfg checks the shape of the selector only.
func CheckOutcome(cfg *Config, o *Outcome) error {
	if o == nil {
		return nil
	}
	if len(o.Rubric) > maxInstructionBytes {
		return fmt.Errorf("%w: outcome.rubric is longer than %d bytes", ErrInvalidRequest, maxInstructionBytes)
	}
	if o.Iterations < 0 || o.Iterations > MaxOutcomeIterations {
		return fmt.Errorf("%w: outcome.iterations is %d; it must be from 0 to %d", ErrInvalidRequest, o.Iterations, MaxOutcomeIterations)
	}
	if !o.on() && (o.Grader != "" || o.Iterations != 0) {
		return fmt.Errorf("%w: outcome.grader and outcome.iterations need outcome.rubric; an outcome without a rubric is off", ErrInvalidRequest)
	}
	if o.Grader != "" {
		sel := o.Grader
		if cfg != nil {
			sel = cfg.ModelSelector(sel)
		}
		if provider, model := SplitSelector(sel); provider == "" || model == "" || strings.ContainsAny(sel, " \t\r\n") {
			return fmt.Errorf("%w: outcome.grader %q is neither a provider/model selector nor a name from models", ErrInvalidRequest, o.Grader)
		}
	}
	return nil
}

// resolveOutcome returns the outcome of a new run for fw.run.queued, or nil
// when it has none: the block of the request, else the block of the inline
// profile, else, for a run that is not a child, the block of the named
// profile. The grader is resolved to a selector, so a later change of
// models does not reach the run.
func (a *Agent) resolveOutcome(req RunRequest, named AgentProfile, parent *runState) *Outcome {
	o := req.Outcome
	if o == nil && req.Profile != nil {
		o = req.Profile.Outcome
	}
	if o == nil && parent == nil && req.Metadata[MetaParent] == "" {
		o = named.Outcome
	}
	if !o.on() {
		return nil
	}
	out := *o
	out.Iterations = out.passes()
	if out.Grader != "" {
		out.Grader = a.cfg.ModelSelector(out.Grader)
	}
	return &out
}

// OutcomeEvaluatedData is the payload of fw.outcome.evaluated, one pass of
// the grader of a run. Verdict is one of the Grade constants, and Notes
// are the notes of the grader. Iteration is the number of the pass, from 1,
// and Iterations the most passes of the outcome. Grader is the id of the
// grader run, empty when none started. Continue is true when the run gets
// one more turn with the notes; otherwise the run finishes with its
// answer. Reason says why the kernel recorded max_iterations, unparsed, or
// skipped, or why a needs_revision does not continue. Answer is the
// sequence number of the fw.model.responded whose answer the pass graded.
type OutcomeEvaluatedData struct {
	Verdict    string `json:"verdict"`
	Notes      string `json:"notes,omitempty"`
	Iteration  int    `json:"iteration"`
	Iterations int    `json:"iterations"`
	Grader     string `json:"grader,omitempty"`
	Continue   bool   `json:"continue,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Answer     uint64 `json:"answer"`
}

// RunOutcome is the latest grade of a run, as the run object shows it next
// to the answer: the verdict, the notes, the passes of the grader so far
// (Iterations), the id of the latest grader run, and the reason of a
// verdict that the kernel recorded. A follow-up to a finished run clears
// it with the output.
type RunOutcome struct {
	Verdict    string `json:"verdict"`
	Notes      string `json:"notes,omitempty"`
	Iterations int    `json:"iterations"`
	Grader     string `json:"grader,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// applyOutcome folds a fw.outcome.evaluated into the run object.
func (r *Run) applyOutcome(e Event) {
	var d OutcomeEvaluatedData
	if e.Decode(&d) != nil {
		return
	}
	r.Outcome = &RunOutcome{Verdict: d.Verdict, Notes: d.Notes, Iterations: d.Iteration, Grader: d.Grader, Reason: d.Reason}
}

// gradeOutcome grades the answer of a run with an outcome before the run
// finishes. endTurn calls it with the cursor of the step. It returns true
// when it appended events and the driver must read the log again, and
// false when endTurn goes on as for a run without an outcome: the run has
// no rubric, a message arrived, or the last pass of this answer is
// recorded and the run finishes.
//
// One step does one thing. A step with the turn open closes the turn. The
// next step grades and records the verdict, with the notes for a revision
// in the same step. A restart between the verdict and the notes gives the
// notes at the next step.
func (a *Agent) gradeOutcome(ctx context.Context, rs *runState, c *cursor, n int) (bool, error) {
	o := rs.queuedData().Outcome
	if !o.on() || c.resp == nil {
		return false, nil
	}
	evs := rs.eventLog()
	passes := outcomePasses(evs, c.respSeq)
	if k := len(passes); k > 0 && passes[k-1].Answer == c.respSeq {
		if !passes[k-1].Continue {
			return false, nil
		}
		_, err := a.emit(ctx, rs, EvMessageReceived, revisionMessage(passes[k-1]))
		return true, err
	}
	if len(passes) >= o.passes() {
		return false, nil
	}
	rs.emitMu.Lock()
	fresh := c.unseen
	for _, e := range rs.events[n:] {
		if e.Type == EvMessageReceived {
			fresh = true
		}
	}
	if fresh {
		rs.emitMu.Unlock()
		return false, nil
	}
	if c.turnOpen {
		_, err := a.emitLocked(ctx, rs, EvTurnFinished, TurnData{Turn: c.turns})
		rs.emitMu.Unlock()
		return true, err
	}
	rs.emitMu.Unlock()

	d := OutcomeEvaluatedData{Iteration: len(passes) + 1, Iterations: o.passes(), Answer: c.respSeq}
	if err := a.runGrader(ctx, rs, c, o, evs, passes, &d); err != nil {
		return true, err
	}
	if d.Verdict == GradeNeedsRevision {
		next := *c
		next.turnOpen, next.keptOpen = false, false
		if d.Iteration >= d.Iterations {
			d.Verdict = GradeMaxIterations
			d.Reason = fmt.Sprintf("the grader asked for a revision at the last of %d passes", d.Iterations)
		} else if ov, over := a.overBudget(rs, &next); over {
			d.Reason = "the budget leaves no room for another turn: " + ov.why()
		} else {
			d.Continue = true
		}
	}
	a.log.Info("outcome graded", "run", rs.id, "grader", d.Grader, "verdict", d.Verdict,
		"iteration", d.Iteration, "iterations", d.Iterations, "continue", d.Continue, "reason", d.Reason)
	rs.emitMu.Lock()
	defer rs.emitMu.Unlock()
	if _, err := a.emitLocked(ctx, rs, EvOutcomeEvaluated, d); err != nil {
		return true, err
	}
	if d.Continue {
		if _, err := a.emitLocked(ctx, rs, EvMessageReceived, revisionMessage(d)); err != nil {
			return true, err
		}
	}
	return true, nil
}

// outcomePasses returns the passes of the grader that count for the answer
// at the sequence number answer: those for the answers since the latest
// message that did not come from the kernel, such as a follow-up of a
// person, which starts the passes again.
func outcomePasses(evs []Event, answer uint64) []OutcomeEvaluatedData {
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
	var out []OutcomeEvaluatedData
	for _, e := range evs {
		if e.Type != EvOutcomeEvaluated {
			continue
		}
		var d OutcomeEvaluatedData
		if e.Decode(&d) == nil && d.Answer > start {
			out = append(out, d)
		}
	}
	return out
}

// runGrader runs one pass of the grader and sets the verdict, the notes,
// the grader, and the reason in d. It returns an error only when the run
// stops: its context ended or the agent shuts down.
func (a *Agent) runGrader(ctx context.Context, rs *runState, c *cursor, o *Outcome, evs []Event, passes []OutcomeEvaluatedData, d *OutcomeEvaluatedData) error {
	if ov, spent := a.limitSpent(rs); spent {
		d.Verdict, d.Reason = GradeSkipped, "the budget is used up: "+ov.why()
		return nil
	}
	if dl, past := a.pastDeadline(rs); past {
		d.Verdict, d.Reason = GradeSkipped, "the working time is used up: "+dl.why(rs)
		return nil
	}
	child, err := a.start(ctx, a.graderRequest(rs, c, o, a.graderInput(rs, evs, o, TextOf(c.resp.Content), d.Iteration, passes), d.Iteration))
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, ErrClosed) {
			return errStopped
		}
		d.Verdict, d.Reason = GradeSkipped, "the grader run did not start: "+strings.TrimPrefix(err.Error(), "fw: ")
		return nil
	}
	d.Grader = child.ID
	done, err := a.waitGrader(ctx, rs, child.ID)
	switch {
	case ctx.Err() != nil || errors.Is(err, ErrClosed):
		return errStopped
	case err != nil:
		d.Verdict, d.Reason = GradeUnparsed, "the wait for the grader run failed: "+strings.TrimPrefix(err.Error(), "fw: ")
		return nil
	case done.State != StateCompleted:
		d.Verdict, d.Reason = GradeUnparsed, fmt.Sprintf("the grader run ended as %s without a verdict: %s", done.State, done.Error)
		return nil
	}
	verdict, notes, ok := parseGrade(done.Output)
	if !ok {
		d.Verdict = GradeUnparsed
		d.Reason = `the last line of the answer of the grader is not one JSON object {"verdict": "satisfied" or "needs_revision", "notes": "..."}`
		return nil
	}
	d.Verdict, d.Notes = verdict, ClipText(notes, graderNotesBytes, "the notes")
	return nil
}

// graderRequest returns the request of the grader run of a pass: a child
// of the run with the same agent profile, the system prompt of a grader,
// the files tool for reading only, no skill, no outcome of its own, and a
// small budget, which the budget of the run caps. Its idempotency key
// names the answer, so a restart takes the same grader run.
func (a *Agent) graderRequest(rs *runState, c *cursor, o *Outcome, input string, iteration int) RunRequest {
	q := rs.queuedData()
	model := o.Grader
	if model == "" {
		model = a.selectorOf(q, a.profileOf(q))
	}
	title := rs.snapshot().Title
	if title == "" {
		title = "run " + rs.id
	}
	off := false
	return RunRequest{
		Agent:          rs.agent,
		Input:          input,
		Parent:         rs.id,
		Origin:         OriginKernel,
		IdempotencyKey: fmt.Sprintf("outcome:%s:%d", rs.id, c.respSeq),
		Metadata:       map[string]string{MetaTitle: TitleFrom(fmt.Sprintf("Grade %d of %s", iteration, title))},
		Profile: &AgentProfile{
			System: graderSystem,
			Model:  model,
			Tools:  []string{"files"},
			Skills: []string{},
			Policy: &Rules{Deny: []string{"files: command=create", "files: command=str_replace", "files: command=insert"}},
			// A grader is never graded.
			Outcome: &Outcome{},
		},
		Budget: &Budget{MaxTurns: graderMaxTurns, MaxToolCalls: graderMaxToolCalls, Deadline: graderDeadline, FinalReport: &off},
	}
}

// waitGrader waits until the grader run ends. The grader works alone, so
// the kernel denies each approval or question of the grader run.
func (a *Agent) waitGrader(ctx context.Context, rs *runState, id string) (*Run, error) {
	wctx := WithRunInfo(ctx, a.runInfo(rs))
	denied := ""
	for {
		r, err := a.WaitFor(wctx, id, func(r *Run) bool {
			return r.State.Terminal() || (r.Pending != nil && r.Pending.ID != denied)
		})
		if err != nil || r.State.Terminal() {
			return r, err
		}
		denied = r.Pending.ID
		err = a.Decide(ctx, id, ApprovalDecision{ApprovalID: denied, Decision: "deny", Decider: rs.id,
			Note: "the grader of an outcome works without approvals and answers"})
		if err != nil && !errors.Is(err, ErrNoPendingApproval) {
			return r, err
		}
	}
}

// graderSystem is the system prompt of a grader run.
const graderSystem = `You are the grader of a run of an agent. You did not do the work of the run, and you do not do it now. You check it.

The message holds the rubric of the run, the task that the run got, the final answer of the run, and the files that the run changed. Apply each criterion of the rubric to the work. Check each claim of the answer against the files and the facts in the message. A claim that nothing in front of you supports counts as not done. If you have the files tool, you can read a file of the run with its view command. Do not change a file.

Write your notes for the agent that did the work. Name each criterion that the work does not meet, and say what must change. Keep the notes short and specific.

The last line of your answer must hold one JSON object and nothing else, without a code fence:
{"verdict": "satisfied", "notes": "..."} when the work meets every criterion of the rubric, or
{"verdict": "needs_revision", "notes": "..."} when it does not.`

// graderInput returns the input of a grader run: the rubric, the task and
// the follow-ups of the run, the answer, the notes of the earlier passes,
// and the files that the run changed. Each part has a bound, and the text
// says what it cut.
func (a *Agent) graderInput(rs *runState, evs []Event, o *Outcome, answer string, iteration int, passes []OutcomeEvaluatedData) string {
	var b strings.Builder
	b.WriteString("# Rubric\n\n")
	b.WriteString(strings.TrimSpace(o.Rubric))
	task := rs.queuedData().Input
	var follow []string
	for _, e := range evs {
		if e.Type != EvMessageReceived {
			continue
		}
		var d MessageReceivedData
		if e.Decode(&d) == nil && d.Origin != OriginKernel && d.Role != RoleSystem {
			if t := strings.TrimSpace(TextOf(d.Content)); t != "" {
				follow = append(follow, t)
			}
		}
	}
	if len(follow) > 0 {
		task += "\n\n## Follow-up messages, in order\n\n" + strings.Join(follow, "\n\n---\n\n")
	}
	b.WriteString("\n\n# Task of the run\n\n")
	b.WriteString(ClipText(strings.TrimSpace(task), graderTaskBytes, "the task"))
	fmt.Fprintf(&b, "\n\n# Final answer of the run\n\nThis is pass %d of at most %d.\n\n", iteration, o.passes())
	b.WriteString(ClipText(strings.TrimSpace(answer), graderAnswerBytes, "the answer"))
	if len(passes) > 0 {
		b.WriteString("\n\n# Your notes of the earlier passes\n")
		for _, p := range passes {
			fmt.Fprintf(&b, "\nPass %d, %s: %s\n", p.Iteration, p.Verdict, strings.TrimSpace(p.Notes))
		}
	}
	b.WriteString("\n\n# Files that the run changed\n\n")
	b.WriteString(a.graderFiles(rs, evs))
	b.WriteString("\n\nGrade the work against the rubric, and end your answer with the JSON line.")
	return b.String()
}

// graderFiles lists the files that the run changed through the files tool,
// from its fw.file.changed events, with the content of each text file that
// fits in graderFileBytes together. A path is relative to the directory of
// the run, where the files tool of the grader works.
func (a *Agent) graderFiles(rs *runState, evs []Event) string {
	files := RunFiles(evs)
	// The full text of a cut tool result is a file of the run too
	// (KeptResultsDir), but it is not work that the run did.
	files = slices.DeleteFunc(files, func(f RunFile) bool {
		return f.Path == KeptResultsDir || strings.HasPrefix(f.Path, KeptResultsDir+"/")
	})
	if len(files) == 0 {
		return "The run changed no file through the files tool. A change that a shell command made does not show here."
	}
	ws := a.Files()
	dir := a.dirOf(rs)
	left, unseen := graderFileBytes, 0
	var b strings.Builder
	for i, f := range files {
		if i == graderMaxFiles {
			fmt.Fprintf(&b, "\n%d more files are not listed.\n", len(files)-i)
			unseen++
			break
		}
		name := f.Path
		if dir != "" {
			name = strings.TrimPrefix(name, dir+"/")
		}
		if f.Op == FileRemoved {
			fmt.Fprintf(&b, "\n## %s\n\nThe run removed this file.\n", name)
			continue
		}
		fmt.Fprintf(&b, "\n## %s (%s, %d bytes)\n\n", name, f.Op, f.Size)
		if ws == nil {
			b.WriteString("Not shown: this process has no workspace to read it from.\n")
			unseen++
			continue
		}
		if left < 1024 {
			fmt.Fprintf(&b, "Not shown: the %d KiB for the content of files is used up.\n", graderFileBytes>>10)
			unseen++
			continue
		}
		data, size, err := readHead(ws, f.Path, left)
		switch {
		case err != nil:
			fmt.Fprintf(&b, "Not shown: the file cannot be read now: %v.\n", err)
			unseen++
			continue
		case !textual(data):
			b.WriteString("Not shown: the file is not text.\n")
			continue
		}
		left -= len(data)
		fence := strings.Repeat("`", max(3, longestRun(string(data), '`')+1))
		fmt.Fprintf(&b, "%s\n%s\n%s\n", fence, strings.TrimRight(string(data), "\n"), fence)
		if int64(len(data)) < size {
			fmt.Fprintf(&b, "Cut: this shows the first %d of %d bytes.\n", len(data), size)
			unseen++
		}
	}
	if unseen > 0 {
		b.WriteString("\nSome content is cut or not shown. If you have the files tool, read such a file with its view command.\n")
	}
	return b.String()
}

// readHead reads at most n bytes of a file, cut at the end of a whole
// character, and returns them with the size of the file.
func readHead(fsys fs.FS, name string, n int) ([]byte, int64, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(n)))
	if err != nil {
		return nil, 0, err
	}
	if int64(len(data)) < st.Size() {
		for i := 0; i < utf8.UTFMax && len(data) > 0 && !utf8.Valid(data); i++ {
			data = data[:len(data)-1]
		}
	}
	return data, st.Size(), nil
}

// textual reports whether data is text: valid UTF-8 without a NUL byte.
func textual(data []byte) bool {
	return utf8.Valid(data) && !strings.ContainsRune(string(data), 0)
}

// longestRun returns the length of the longest run of r in s.
func longestRun(s string, r rune) int {
	best, cur := 0, 0
	for _, c := range s {
		if c == r {
			cur++
			best = max(best, cur)
		} else {
			cur = 0
		}
	}
	return best
}

// ClipText cuts s to at most n bytes at the end of a whole character, and
// says so after the text, naming it what. Package routine bounds the
// inputs of a divergence with it.
func ClipText(s string, n int, what string) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	cut := s[:n]
	return fmt.Sprintf("%s\n\n[Cut: %s has %d bytes, and this shows the first %d.]", cut, what, len(s), len(cut))
}

// parseGrade reads the verdict of a grader from the last line of its
// answer. The line must hold one JSON object with exactly the keys verdict
// and notes, both strings, and the verdict must be satisfied or
// needs_revision. Anything else reports false.
func parseGrade(out string) (verdict, notes string, ok bool) {
	out = strings.TrimSpace(out)
	if i := strings.LastIndexByte(out, '\n'); i >= 0 {
		out = strings.TrimSpace(out[i+1:])
	}
	if out == "" {
		return "", "", false
	}
	dec := json.NewDecoder(strings.NewReader(out))
	dec.DisallowUnknownFields()
	var g struct {
		Verdict *string `json:"verdict"`
		Notes   *string `json:"notes"`
	}
	if err := dec.Decode(&g); err != nil || g.Verdict == nil || g.Notes == nil {
		return "", "", false
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", "", false
	}
	switch *g.Verdict {
	case GradeSatisfied, GradeNeedsRevision:
		return *g.Verdict, strings.TrimSpace(*g.Notes), true
	}
	return "", "", false
}

// revisionMessage is the message from the kernel that gives the run the
// notes of a pass that asks for a revision.
func revisionMessage(d OutcomeEvaluatedData) MessageReceivedData {
	notes := strings.TrimSpace(d.Notes)
	if notes == "" {
		notes = "The grader gave no notes."
	}
	text := fmt.Sprintf("The grader of this run applied the rubric of the run to your answer and asks for a revision. "+
		"This was pass %d of at most %d.\n\nNotes of the grader:\n\n%s\n\n"+
		"Fix what the notes name and check each claim of your answer against the work. "+
		"Then end your turn with your complete final answer. It replaces your earlier answer.", d.Iteration, d.Iterations, notes)
	return MessageReceivedData{Role: RoleUser, Content: []Block{Text(text)}, Origin: OriginKernel, Note: NoteOutcome}
}
