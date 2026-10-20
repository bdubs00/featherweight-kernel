package harness

// This file is the core that both drivers share: what the ledger says
// about a hosted run, the record of the turns and the tool calls, the
// files that a turn changed, the policy and the budget, the model, and
// the processes of the harness.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// EventData is the payload of fw.harness.event. Harness names the kind
// of harness. Kind is the type of an event of the harness, such as
// message.part.updated or result, with a compact payload in Data, or a
// step of the kernel in the session, which starts with "fw.":
//
//   - fw.start: the run is a session of a harness.
//   - fw.session: the harness created the session Session.
//   - fw.prompt: the harness got the messages of the run up to Seq.
//   - fw.answer: the harness got the decision of the approval Approval.
//   - fw.idle: the harness finished the exchange that the latest
//     fw.prompt or fw.answer started.
//
// A cold boot finds an exchange that has no fw.idle and continues it. No
// event holds a key or a secret: the kernel replaces each one before it
// appends the event.
type EventData struct {
	Harness  string          `json:"harness"`
	Kind     string          `json:"kind"`
	Session  string          `json:"session,omitempty"`
	Seq      uint64          `json:"seq,omitempty"`
	Approval string          `json:"approval,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
}

// The steps of the kernel in a session (EventData).
const (
	KindStart   = "fw.start"
	KindSession = "fw.session"
	KindPrompt  = "fw.prompt"
	KindAnswer  = "fw.answer"
	KindIdle    = "fw.idle"
)

// state is what the events of a hosted run say.
type state struct {
	started  bool // fw.run.started exists
	hosted   bool // fw.harness.event exists: the run is a session
	session  string
	sessSeq  uint64 // the fw.harness.event that named the session
	prompted uint64 // the latest message that the harness got
	exchange uint64 // the latest fw.prompt or fw.answer
	idle     uint64 // the latest fw.idle
	lastSeq  uint64

	run      *fw.Run
	pending  *fw.Approval // the approval that waits
	terminal bool

	unseen []message // the messages after prompted
	// decided holds the decisions that the harness did not get yet, in
	// order, and decisions every decision by the call id.
	decided   []decision
	decisions map[string]decision

	turns     int
	turnOpen  bool
	responded bool              // the open turn has its fw.model.responded
	open      map[string]string // the tool calls without a result, by call id, to the tool
	output    string            // the text of the latest response with text
	answer    uint64            // the latest fw.model.responded
	graded    uint64            // the latest answer that a fw.outcome.evaluated names
}

// message is a message of the run for the harness: the input of the run
// or a follow-up. A message with an empty text gives the harness nothing,
// such as a blank follow-up or a note of the kernel, but the harness still
// got it, so the run can finish after it.
type message struct {
	seq  uint64
	text string
}

// filesNote is the text of a message that holds attachments and no text,
// when the attachments are not files of the workspace.
const filesNote = "The person sent attachments without text. They do not reach this session. Ask the person for their content as text if you need it."

// messageText returns the text of a message for the harness. A message
// with attachments alone gets a text that names them: the kernel keeps
// each file in the directory attachments of the directory of the run,
// where the harness works, so the harness reads it with its tools.
func messageText(blocks []fw.Block) string {
	if t := strings.TrimSpace(fw.TextOf(blocks)); t != "" {
		return t
	}
	var files []string
	other := false
	for _, b := range blocks {
		switch {
		case b.Type == fw.BlockFile && b.File != nil && b.File.Path != "":
			files = append(files, path.Join(fw.AttachmentsDir, path.Base(b.File.Path)))
		case b.Type != fw.BlockText:
			other = true
		}
	}
	switch {
	case len(files) > 0:
		return "The person sent files without text: " + strings.Join(files, ", ") + "."
	case other:
		return filesNote
	}
	return ""
}

// decision is an approval of the run and its decision.
type decision struct {
	req fw.ApprovalRequestedData
	dec fw.ApprovalDecidedData
	seq uint64 // of the decision
}

// openExchange reports whether the latest exchange has no fw.idle: the
// harness worked when the process stopped.
func (s *state) openExchange() bool { return s.exchange > s.idle }

// scan reads the events of a run.
func scan(evs []fw.Event) *state {
	s := &state{open: map[string]string{}, decisions: map[string]decision{}}
	s.run = fw.ProjectRun(evs)
	s.pending, s.terminal = s.run.Pending, s.run.State.Terminal()
	reqs := map[string]fw.ApprovalRequestedData{}
	answered := map[string]bool{}
	var msgs []message
	var all []decision
	for _, e := range evs {
		s.lastSeq = e.Seq
		switch e.Type {
		case fw.EvRunQueued:
			var d fw.RunQueuedData
			if e.Decode(&d) != nil {
				continue
			}
			if strings.TrimSpace(d.Input) != "" {
				msgs = append(msgs, message{seq: e.Seq, text: d.Input})
			} else if t := messageText(d.Files); t != "" {
				msgs = append(msgs, message{seq: e.Seq, text: t})
			}
		case fw.EvRunStarted:
			s.started = true
		case fw.EvMessageReceived:
			// Every message counts, so prompted covers it once the harness
			// got the messages before and after it (Hosted.Finish). Only a
			// message of a person or a caller has a text for the harness.
			m := message{seq: e.Seq}
			var d fw.MessageReceivedData
			if e.Decode(&d) == nil && d.Origin != fw.OriginKernel && (d.Role == fw.RoleUser || d.Role == fw.RoleSystem) {
				m.text = messageText(d.Content)
			}
			msgs = append(msgs, m)
		case EvEvent:
			s.hosted = true
			var d EventData
			if e.Decode(&d) != nil {
				continue
			}
			switch d.Kind {
			case KindSession:
				s.session, s.sessSeq = d.Session, e.Seq
			case KindPrompt:
				s.prompted = max(s.prompted, d.Seq)
				s.exchange = e.Seq
			case KindAnswer:
				answered[d.Approval] = true
				s.exchange = e.Seq
			case KindIdle:
				s.idle = e.Seq
			}
		case fw.EvTurnStarted:
			s.turns++
			s.turnOpen, s.responded = true, false
		case fw.EvTurnFinished:
			s.turnOpen = false
		case fw.EvOutcomeEvaluated:
			var d fw.OutcomeEvaluatedData
			if e.Decode(&d) == nil {
				s.graded = max(s.graded, d.Answer)
			}
		case fw.EvModelResponded:
			s.responded, s.answer = true, e.Seq
			var d fw.ModelRespondedData
			if e.Decode(&d) == nil {
				if t := strings.TrimSpace(fw.TextOf(d.Content)); t != "" {
					s.output = t
				}
			}
		case fw.EvToolRequested:
			var d fw.ToolRequestedData
			if e.Decode(&d) == nil {
				s.open[d.CallID] = d.Tool
			}
		case fw.EvToolResponded:
			var d fw.ToolRespondedData
			if e.Decode(&d) == nil {
				delete(s.open, d.CallID)
			}
		case fw.EvApprovalRequested:
			var d fw.ApprovalRequestedData
			if e.Decode(&d) == nil {
				reqs[d.ApprovalID] = d
			}
		case fw.EvApprovalDecided:
			var d fw.ApprovalDecidedData
			if e.Decode(&d) != nil {
				continue
			}
			if req, ok := reqs[d.ApprovalID]; ok {
				all = append(all, decision{req: req, dec: d, seq: e.Seq})
			}
		}
	}
	for _, m := range msgs {
		if m.seq > s.prompted {
			s.unseen = append(s.unseen, m)
		}
	}
	for _, d := range all {
		if d.req.CallID != "" {
			s.decisions[d.req.CallID] = d
		}
		if !answered[d.req.ApprovalID] {
			s.decided = append(s.decided, d)
		}
	}
	return s
}

// run is one call of Drive.
type run struct {
	h   *host
	hr  *fw.Hosted
	st  *state
	dir string // the absolute directory of the run, where the harness works
}

// rescan reads the events again.
func (r *run) rescan() *state {
	r.st = scan(r.hr.Events())
	return r.st
}

// mark records a step of the kernel in the session, or an event of the
// harness.
func (r *run) mark(ctx context.Context, d EventData) error {
	d.Harness = r.h.cfg.Kind
	_, err := r.hr.Emit(ctx, EvEvent, d)
	return err
}

// raw records an event of the harness with a compact payload.
func (r *run) raw(ctx context.Context, kind string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("harness: encode the event %s: %w", kind, err)
	}
	return r.mark(ctx, EventData{Kind: kind, Data: b})
}

// fail records the error and finishes the run as failed.
func (r *run) fail(ctx context.Context, msg string) error {
	r.h.log.Warn("harness run failed", "run", r.hr.ID(), "error", r.h.a.Redact(msg))
	if _, err := r.hr.Emit(ctx, fw.EvError, fw.ErrorData{Message: msg}); err != nil {
		return err
	}
	_, err := r.hr.Emit(ctx, fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeFailed, Output: r.st.output, Error: msg})
	return err
}

// noGrader is the reason of the outcome of a hosted run with a rubric.
const noGrader = "a hosted harness has no grader"

// finish records the end of the run after an exchange, unless a message
// came that the harness did not get (Hosted.Finish). A run with a rubric
// records first that no grader graded its answer, once for each answer,
// so the person sees why the run has no grade.
func (r *run) finish(ctx context.Context, st *state) (bool, error) {
	if o := r.hr.Queued().Outcome; o != nil && strings.TrimSpace(o.Rubric) != "" && st.answer > st.graded {
		d := fw.OutcomeEvaluatedData{Verdict: fw.GradeSkipped, Iteration: 1, Iterations: max(o.Iterations, 1), Reason: noGrader, Answer: st.answer}
		if _, err := r.hr.Emit(ctx, fw.EvOutcomeEvaluated, d); err != nil {
			return false, err
		}
		st.graded = st.answer
	}
	return r.hr.Finish(ctx, fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Output: st.output}, st.prompted)
}

// prompt returns the text of the messages that the harness did not get,
// and the sequence number of the latest one. The text is empty when none
// of them has a text.
func (s *state) prompt() (string, uint64) {
	var parts []string
	var last uint64
	for _, m := range s.unseen {
		if m.text != "" {
			parts = append(parts, m.text)
		}
		last = m.seq
	}
	return strings.Join(parts, "\n\n"), last
}

// skip records that the harness got the messages up to through, which
// hold no text, so nothing goes to the harness. The exchange ends at once
// with its fw.idle, so no driver waits for the harness.
func (r *run) skip(ctx context.Context, through uint64) error {
	if err := r.mark(ctx, EventData{Kind: KindPrompt, Seq: through}); err != nil {
		return err
	}
	return r.mark(ctx, EventData{Kind: KindIdle})
}

// restartNote is the message that continues a session that a restart
// interrupted.
const restartNote = "The process of this session stopped and started again. Continue the task from where you stopped; check the state of the files first."

// errOver means that the run reached a limit of its budget, and the run
// finished for it.
var errOver = errors.New("harness: budget exceeded")

// recorder records the turns of a session: one turn for each model call
// of the harness, with its tool calls, as the turn loop records them.
type recorder struct {
	r        *run
	provider string
	model    string
	files    *tracker

	turn    int
	open    bool
	flushed bool // the model call of the open turn has its fw.model.responded
	began   time.Time
	calls   map[string]string // the tool calls without a result, by call id, to the tool
	asked   map[string]time.Time
	wrote   bool // a tool that can write ran in the open turn
}

// newRecorder continues the turns that the events of the run hold.
func newRecorder(r *run, provider, model string, files *tracker) *recorder {
	rec := &recorder{r: r, provider: provider, model: model, files: files, turn: r.st.turns, open: r.st.turnOpen, flushed: !r.st.turnOpen || r.st.responded,
		calls: map[string]string{}, asked: map[string]time.Time{}}
	for id, tool := range r.st.open {
		rec.calls[id] = tool
	}
	return rec
}

// begin closes the open turn and opens the next one, unless the run
// reached a limit of its budget.
func (rec *recorder) begin(ctx context.Context) error {
	if err := rec.end(ctx); err != nil {
		return err
	}
	if err := rec.check(ctx); err != nil {
		return err
	}
	rec.turn++
	if _, err := rec.r.hr.Emit(ctx, fw.EvTurnStarted, fw.TurnData{Turn: rec.turn}); err != nil {
		return err
	}
	if _, err := rec.r.hr.Emit(ctx, fw.EvModelRequested, fw.ModelRequestedData{Provider: rec.provider, Model: rec.model, Effort: fw.EffortDefault}); err != nil {
		return err
	}
	rec.open, rec.flushed, rec.began, rec.wrote = true, false, time.Now(), false
	return nil
}

// respond records the response of the model call of the open turn.
func (rec *recorder) respond(ctx context.Context, content []fw.Block, u fw.Usage, stop fw.StopReason) error {
	if !rec.open {
		if err := rec.begin(ctx); err != nil {
			return err
		}
	}
	if content == nil {
		content = []fw.Block{}
	}
	d := fw.ModelRespondedData{Content: content, StopReason: stop, Usage: u, LatencyMS: time.Since(rec.began).Milliseconds(), Provider: rec.provider, Model: rec.model}
	if _, err := rec.r.hr.Emit(ctx, fw.EvModelResponded, d); err != nil {
		return err
	}
	rec.flushed = true
	if t := strings.TrimSpace(fw.TextOf(content)); t != "" {
		rec.r.st.output = t
	}
	return rec.check(ctx)
}

// usage records the usage of a model call whose response is recorded
// already, or of a model call of a subagent, so the budget counts it.
func (rec *recorder) usage(ctx context.Context, u fw.Usage) error {
	if u == (fw.Usage{}) {
		return nil
	}
	if _, err := rec.r.hr.Emit(ctx, fw.EvRunAnnotated, fw.RunAnnotatedData{Usage: &u, Provider: rec.provider, Model: rec.model}); err != nil {
		return err
	}
	return rec.check(ctx)
}

// request records a tool call before it runs. A call that the run
// recorded already stays as it is.
func (rec *recorder) request(ctx context.Context, callID, tool string, input json.RawMessage) error {
	if _, ok := rec.calls[callID]; ok {
		return nil
	}
	if len(input) == 0 || !json.Valid(input) {
		input = json.RawMessage(`{}`)
	}
	if _, err := rec.r.hr.Emit(ctx, fw.EvToolRequested, fw.ToolRequestedData{Tool: tool, CallID: callID, Input: input}); err != nil {
		return err
	}
	rec.calls[callID] = tool
	rec.asked[callID] = time.Now()
	return nil
}

// result records the result of a tool call.
func (rec *recorder) result(ctx context.Context, callID, tool, text string, isErr bool) error {
	if t, ok := rec.calls[callID]; ok && tool == "" {
		tool = t
	}
	var latency int64
	if at, ok := rec.asked[callID]; ok {
		latency = time.Since(at).Milliseconds()
	}
	d := fw.ToolRespondedData{CallID: callID, Tool: tool, Content: []fw.Block{fw.Text(text)}, IsError: isErr, LatencyMS: latency}
	if _, err := rec.r.hr.Emit(ctx, fw.EvToolResponded, d); err != nil {
		return err
	}
	delete(rec.calls, callID)
	delete(rec.asked, callID)
	if !readOnly(tool) {
		rec.wrote = true
	}
	return nil
}

// end closes the open turn and records the files that it changed.
func (rec *recorder) end(ctx context.Context) error {
	if !rec.open {
		return nil
	}
	if _, err := rec.r.hr.Emit(ctx, fw.EvTurnFinished, fw.TurnData{Turn: rec.turn}); err != nil {
		return err
	}
	rec.open = false
	if rec.wrote {
		rec.wrote = false
		return rec.files.record(ctx, rec.r)
	}
	return nil
}

// interrupt gives each open tool call an error result that says why it has
// none, and closes the open turn.
func (rec *recorder) interrupt(ctx context.Context, why string) error {
	ids := make([]string, 0, len(rec.calls))
	for id := range rec.calls {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := rec.result(ctx, id, "", why, true); err != nil {
			return err
		}
	}
	if rec.open && !rec.flushed {
		// A turn without its response gets an empty one, so the next
		// turn starts after a closed model call.
		if err := rec.respond(ctx, nil, fw.Usage{}, fw.StopEndTurn); err != nil && !errors.Is(err, errOver) {
			return err
		}
	}
	return rec.end(ctx)
}

// check stops the run when it reached a limit of its budget: it records
// fw.budget.exceeded and finishes the run as failed. A hosted run makes no
// final report.
func (rec *recorder) check(ctx context.Context) error {
	d, msg, over := rec.r.hr.Over()
	if !over {
		return nil
	}
	if _, err := rec.r.hr.Emit(ctx, fw.EvBudgetExceeded, d); err != nil {
		return err
	}
	for id := range rec.calls {
		if err := rec.result(ctx, id, "", "not finished: "+msg, true); err != nil {
			return err
		}
	}
	if rec.open {
		if _, err := rec.r.hr.Emit(ctx, fw.EvTurnFinished, fw.TurnData{Turn: rec.turn}); err != nil {
			return err
		}
		rec.open = false
	}
	if err := rec.files.record(ctx, rec.r); err != nil {
		return err
	}
	if _, err := rec.r.hr.Emit(ctx, fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeFailed, Output: rec.r.st.output, Error: msg}); err != nil {
		return err
	}
	rec.r.h.log.Info("harness run stopped at its budget", "run", rec.r.hr.ID(), "limit", d.Limit)
	return errOver
}

// toolSpec returns the spec of a tool of the harness for the policy. The
// kernel names the tools of a harness in lower case, so one rule, such as
// "bash: git push*", fits both kinds. The default policy asks before a
// destructive tool, as for the bash tool of the kernel.
func toolSpec(name string) fw.ToolSpec {
	s := fw.ToolSpec{Name: name}
	switch name {
	case "bash", "external_directory", "doom_loop":
		s.Destructive = true
	default:
		s.ReadOnly = readOnly(name)
	}
	return s
}

// readOnly reports whether a tool of a harness changes nothing in the
// workspace.
func readOnly(name string) bool {
	switch name {
	case "read", "glob", "grep", "list", "ls", "lsp", "skill", "webfetch", "websearch", "codesearch",
		"todoread", "todowrite", "bashoutput", "question", "askuserquestion", "exitplanmode":
		return true
	}
	return false
}

// decide asks the policy of the run about a tool call of the harness. A
// call with a path inside a private path of the host gets a deny first,
// whatever the policy says, as the file tracker skips those paths: the
// home of the harness holds the transcripts of every run.
func (r *run) decide(ctx context.Context, callID, tool string, input json.RawMessage) fw.Decision {
	if len(input) == 0 || !json.Valid(input) {
		input = json.RawMessage(`{}`)
	}
	if p := r.privatePath(tool, input); p != "" {
		return fw.Decision{Verdict: fw.VerdictDeny, Reason: "the path " + p + " is inside the home of the harness, the home of fw, or a path that fw keeps for itself"}
	}
	return r.hr.Decide(ctx, fw.ToolCall{CallID: callID, Spec: toolSpec(tool), Input: input})
}

// pathFields are the fields of the input of a tool of opencode or Claude
// Code that name a file or a directory.
var pathFields = []string{"file_path", "filePath", "path", "notebook_path", "notebookPath"}

// privatePath returns the first path of a tool input that resolves inside
// a private path of the host, or "". A relative path starts in the
// directory of the run, ~ is the home of the harness, and the links of
// the part of the path that exists are resolved. The pattern of glob
// counts when it is absolute; the pattern of grep is a regular
// expression and does not.
func (r *run) privatePath(tool string, input json.RawMessage) string {
	var in map[string]any
	if json.Unmarshal(input, &in) != nil {
		return ""
	}
	var paths []string
	for _, k := range pathFields {
		if s, ok := in[k].(string); ok && strings.TrimSpace(s) != "" {
			paths = append(paths, s)
		}
	}
	if s, ok := in["pattern"].(string); ok && tool == "glob" && (filepath.IsAbs(s) || strings.HasPrefix(s, "~")) {
		if i := strings.IndexAny(s, "*?[{"); i >= 0 {
			s = filepath.Dir(s[:i] + "x")
		}
		paths = append(paths, s)
	}
	for _, p := range paths {
		abs := strings.TrimSpace(p)
		if abs == "~" || strings.HasPrefix(abs, "~/") {
			abs = filepath.Join(r.h.home, abs[1:])
		}
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(r.dir, abs)
		}
		abs = realPath(filepath.Clean(abs))
		if r.h.cutOutput(tool, abs) {
			continue
		}
		for _, priv := range r.h.private {
			if within(abs, priv) {
				return p
			}
		}
	}
	return ""
}

// cutOutput reports whether a call reads one file in which the harness
// keeps the full output of a tool call that it cut, and tells the model to
// read: projects/<project>/<session>/tool-results/<file> of Claude Code,
// and .local/share/opencode/tool-output/<file> of opencode, under the home
// of the harness. The name of such a file holds the id of the call, so a
// session cannot guess the files of another session. A call that lists or
// searches the directory itself gets no such pass.
func (h *host) cutOutput(tool, p string) bool {
	if tool == "glob" || tool == "list" || tool == "ls" {
		return false
	}
	for _, home := range []string{h.home, realPath(h.home)} {
		var rel string
		var err error
		var want []string
		switch h.cfg.Kind {
		case KindClaudeCode:
			rel, err = filepath.Rel(filepath.Join(home, "projects"), p)
			want = []string{"", "", "tool-results", ""}
		case KindOpencode:
			rel, err = filepath.Rel(filepath.Join(home, ".local", "share", "opencode", "tool-output"), p)
			want = []string{""}
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if err != nil || len(parts) != len(want) || parts[0] == ".." {
			continue
		}
		ok := true
		for i, w := range want {
			if parts[i] == "" || parts[i] == "." || parts[i] == ".." || (w != "" && parts[i] != w) {
				ok = false
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// realPath returns the absolute path p with the links of its longest part
// that exists resolved. A link whose target does not exist yet is followed
// too.
func realPath(p string) string {
	for hops := 0; hops < 40; hops++ {
		rest, next := "", ""
		for q := p; ; q = filepath.Dir(q) {
			if real, err := filepath.EvalSymlinks(q); err == nil {
				return filepath.Join(real, rest)
			}
			if fi, err := os.Lstat(q); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
				if target, err := os.Readlink(q); err == nil {
					if !filepath.IsAbs(target) {
						target = filepath.Join(filepath.Dir(q), target)
					}
					next = filepath.Join(target, rest)
				}
				break
			}
			if q == filepath.Dir(q) {
				return p
			}
			rest = filepath.Join(filepath.Base(q), rest)
		}
		if next == "" {
			return p
		}
		p = next
	}
	return p
}

// recheck decides a call that a person allowed: a deny that the policy of
// the run gained meanwhile wins over the allow, as in the turn loop.
func (r *run) recheck(ctx context.Context, d decision) (bool, string) {
	if d.dec.Decision != "allow" {
		return false, deniedBy(d.dec)
	}
	if v := r.decide(ctx, d.req.CallID, d.req.Tool, d.req.Input); v.Verdict == fw.VerdictDeny {
		return false, "denied by policy: " + v.Reason
	}
	return true, ""
}

// deniedBy says who denied a call and why.
func deniedBy(d fw.ApprovalDecidedData) string {
	who := d.Decider
	if who == "" {
		who = "caller"
	}
	if d.Note == "" {
		return "denied by " + who
	}
	return "denied by " + who + ": " + d.Note
}

// approval records that a tool call waits for a person. The run moves to
// input-required, and Drive returns.
func (r *run) approval(ctx context.Context, callID, tool string, input json.RawMessage, reason string) (string, error) {
	if len(input) == 0 || !json.Valid(input) {
		input = json.RawMessage(`{}`)
	}
	id := fw.NewID()
	_, err := r.hr.Emit(ctx, fw.EvApprovalRequested, fw.ApprovalRequestedData{ApprovalID: id, Tool: tool, CallID: callID, Input: input, Reason: reason})
	return id, err
}

// tracker records the files that a session changed in the directory of
// its run: it compares the size, the time, and the mode of each file with
// the state of the latest record, and records each change as
// fw.file.changed, as the files tool does.
type tracker struct {
	dir  string // the absolute directory of the run
	rel  string // the directory of the run in the workspace, "" for the root
	tool string // the tool of the events: the kind of harness
	skip []string
	base map[string]fileState
}

type fileState struct {
	size int64
	mod  time.Time
	mode fs.FileMode
}

// maxTracked bounds the files that one record compares.
const maxTracked = 50000

func newTracker(h *host, dir, rel string) *tracker {
	t := &tracker{dir: dir, rel: rel, tool: h.cfg.Kind, skip: []string{h.home}}
	for _, p := range h.a.Config().Reserved() {
		if abs, err := filepath.Abs(p); err == nil {
			t.skip = append(t.skip, abs)
		}
	}
	t.base, _ = t.walk(h)
	return t
}

// walk reads the state of the files under the directory. It skips the
// paths of fw and of the harness, .git, and node_modules.
func (t *tracker) walk(h *host) (map[string]fileState, bool) {
	out := map[string]fileState{}
	full := true
	err := filepath.WalkDir(t.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == t.dir {
				return err
			}
			return nil
		}
		for _, s := range t.skip {
			if within(p, s) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		if d.IsDir() {
			if p != t.dir && (d.Name() == ".git" || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if len(out) >= maxTracked {
			full = false
			return filepath.SkipAll
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(t.dir, p)
		if err != nil {
			return nil
		}
		out[filepath.ToSlash(rel)] = fileState{size: info.Size(), mod: info.ModTime(), mode: info.Mode()}
		return nil
	})
	if err != nil {
		h.log.Warn("harness: read the files of the run", "dir", t.dir, "error", err)
	}
	if !full {
		h.log.Warn("harness: the directory of the run holds too many files to record the changes", "dir", t.dir, "limit", maxTracked)
	}
	return out, full
}

// record compares the files with the latest record and records each
// change.
func (t *tracker) record(ctx context.Context, r *run) error {
	if t == nil {
		return nil
	}
	now, full := t.walk(r.h)
	if !full {
		return nil
	}
	var names []string
	for name := range now {
		names = append(names, name)
	}
	for name := range t.base {
		if _, ok := now[name]; !ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		cur, ok := now[name]
		old, had := t.base[name]
		var d fw.FileChangedData
		switch {
		case !ok:
			d = fw.FileChangedData{Op: fw.FileRemoved}
		case !had:
			d = fw.FileChangedData{Op: fw.FileCreated, Size: cur.size}
		case cur != old:
			d = fw.FileChangedData{Op: fw.FileChanged, Size: cur.size}
		default:
			continue
		}
		d.Tool, d.Path = t.tool, path.Join(t.rel, name)
		if ok {
			d.SHA256 = hashFile(filepath.Join(t.dir, filepath.FromSlash(name)))
		}
		if _, err := r.hr.Emit(ctx, fw.EvFileChanged, d); err != nil {
			return err
		}
	}
	t.base = now
	return nil
}

// hashFile returns the hex SHA-256 of a file, or "" when it cannot be read.
func hashFile(p string) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return ""
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// modelConf is the model of a run as the harness needs it: the endpoint
// and the key that the kernel uses for the same model.
type modelConf struct {
	provider string // the provider of the kernel, such as openai
	name     string // the model name, without the provider
	baseURL  string // providers.<provider>.baseUrl, or ""
	keyEnv   string // the variable that holds the key
	key      string // the key, or "" for a server that needs none
}

// defaultKeyEnv names the variable of the key of a provider that names no
// apiKeyEnv, as the model adapters of the kernel do.
var defaultKeyEnv = map[string]string{"openai": "OPENAI_API_KEY", "anthropic": "ANTHROPIC_API_KEY", "gemini": "GEMINI_API_KEY"}

// modelOf returns the model of the run. The kernel has no default model,
// so a run without one fails.
func (r *run) modelOf(ctx context.Context) (modelConf, error) {
	sel := r.hr.Model()
	if sel == "" {
		return modelConf{}, fw.ErrNoModel
	}
	provider, name := fw.SplitSelector(sel)
	if name == "" {
		return modelConf{}, fmt.Errorf("harness: the model %q has no provider; use a selector such as openai/<model>", sel)
	}
	pc := r.h.a.Config().ProviderNamed(provider)
	m := modelConf{provider: provider, name: name, baseURL: strings.TrimSpace(pc.BaseURL), keyEnv: strings.TrimSpace(pc.APIKeyEnv)}
	if m.keyEnv == "" {
		m.keyEnv = defaultKeyEnv[provider]
	}
	if m.keyEnv != "" {
		if key, err := r.h.a.Secrets().Get(ctx, m.keyEnv); err == nil {
			m.key = key
		}
	}
	return m, nil
}

// info returns the run info of a process that the harness starts for the
// run, as a tool call of the run gets it. The hooks of the linked packages
// read it (fw.ExtendToolEnv): the environment of section 18 waits for its
// install, or fails, before such a process starts.
func (r *run) info(dir string) fw.RunInfo {
	q := r.hr.Queued()
	return fw.RunInfo{RunID: r.hr.ID(), Agent: q.Agent, Principal: q.Principal, PrincipalKind: q.PrincipalKind, Dir: dir}
}

// environ returns the environment of a process of the harness that runs
// in dir for the run of info: the environment of a tool process
// (fw.ToolEnv) and the variables of tools.bash.env, HOME, the home of the
// harness, and FW_RUN_DIR, then the changes of the linked packages
// (fw.ExtendToolEnv), such as the PATH of the environment of section 18,
// whose error stops the process. prog names the program in the error.
func (h *host) environ(ctx context.Context, info fw.RunInfo, dir, prog string) ([]string, error) {
	cfg := h.a.Config()
	var configured []string
	for name, v := range cfg.Tools.Bash.Env {
		if name != "" && !strings.Contains(name, "=") {
			configured = append(configured, name+"="+v)
		}
	}
	sort.Strings(configured)
	actx := fw.WithRunInfo(fw.WithAgent(ctx, h.a), info)
	env := fw.ToolEnv(actx, cfg, os.Environ(), configured)
	env, err := fw.ExtendToolEnv(actx, cfg, append(env, "HOME="+h.home, "FW_RUN_DIR="+dir))
	if err != nil {
		return nil, fmt.Errorf("harness: the environment of %s: %w", prog, err)
	}
	return env, nil
}

// command builds the command of a process of the harness, which runs as
// the tool user in dir with the environment env (environ). extra comes
// last: the variables that the harness needs on purpose, such as the key
// of its model, which the environment of a tool process leaves out.
func (h *host) command(argv []string, dir string, env []string, extra ...string) *exec.Cmd {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(slices.Clip(env), extra...)
	h.a.ToolUser().Apply(cmd)
	procAttr(cmd)
	return cmd
}

// envDigest returns the SHA-256 of an environment, in hex.
func envDigest(env []string) string {
	sorted := slices.Sorted(slices.Values(env))
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\x00")))
	return hex.EncodeToString(sum[:])
}

// logLines logs each line of the output of a process of the harness at
// the debug level, with every secret replaced. The cut to 2000 bytes comes
// after the replacement, so it never leaves a part of a secret that the
// redactor no longer matches.
func (h *host) logLines(rd io.Reader, stream string) {
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := h.a.Redact(sc.Text())
		if len(line) > 2000 {
			line = line[:2000]
		}
		h.log.Debug("harness output", "stream", stream, "line", line)
	}
}

// textOf returns a short text of a JSON value: a string as it is, and any
// other value as JSON.
func textOf(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}
