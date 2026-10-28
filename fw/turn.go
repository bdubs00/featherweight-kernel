package fw

// This file is the turn loop. A driver goroutine owns a run. At each step it
// reads the log of the run, finds the next action, and does it. Because the
// next action comes from the log alone, the same loop starts a new run,
// continues after a decision or a follow-up, and resumes after a restart.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// errStopped means the run context ended or the kernel drains, so the driver
// stops without appending more events.
var errStopped = errors.New("fw: run stopped")

type exitReason int

const (
	exitIdle     exitReason = iota // the run is finished or waits for a decision
	exitCanceled                   // the run context ended
	exitDrain                      // the kernel shuts down
	exitError                      // the ledger failed
)

// answerSchema is the form of a free-text question.
const answerSchema = `{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`

// modelStreamRetryWait is the pause before the one retry of a model
// response that the network cut. The context of the call bounds both calls
// and the pause.
const modelStreamRetryWait = 500 * time.Millisecond

// maxLoopPeriod is the longest cycle of tool calls that the loop guard
// detects: a call, a pair, or a triple that comes back in the same order.
const maxLoopPeriod = 3

// questionRecord is a schema-bearing question of a tool call: the stored
// fw.approval.requested and, when the run already took it, its decision. The
// kernel keeps no channel for such a decision across a restart, so the
// ledger alone says what the person answered.
type questionRecord struct {
	req     ApprovalRequestedData
	decided *ApprovalDecidedData
}

// callRecord is what the log holds about one tool call of the latest response.
type callRecord struct {
	requestedSeq uint64               // the latest fw.tool.requested
	responded    bool                 // a fw.tool.responded exists
	decision     *ApprovalDecidedData // the latest decision of an approval for this call
	decisionSeq  uint64
	// questions holds the schema-bearing questions of the call, in order.
	// A question that a restart carried over stays here with its decision.
	questions []*questionRecord
	// connectSeq is the latest fw.approval.requested of the call with
	// Connect set. Such a call waited for a connection before it reached
	// the service, so it can run again after a restart.
	connectSeq  uint64
	connect     *ConnectRequest
	connectDeny *ApprovalDecidedData // the person skipped the service
}

// cursor is the position of a run, derived from its log.
type cursor struct {
	queued     RunQueuedData
	started    bool
	terminal   bool // the log ends in fw.run.finished with no follow-up after it
	turns      int  // number of fw.turn.started
	turnOpen   bool
	turnSeq    uint64
	resp       *ModelRespondedData // the latest response
	respSeq    uint64
	unseen     bool // a message arrived after the latest fw.model.requested
	maxTokens  int  // consecutive max_tokens responses that end at resp, at most 2
	pending    *ApprovalRequestedData
	calls      map[string]*callRecord // the tool calls of resp, when it stopped for tool use
	lastSeq    uint64
	brief      string    // the brief of fw.run.started
	context    string    // the context of fw.run.started (ContextBriefer)
	fromAgent  bool      // a message from another agent arrived (MessageNote)
	compactSeq uint64    // the latest fw.context.compacted
	toolCalls  int       // fw.tool.requested events in the run
	keptTurns  int       // the turns that a branch kept from its source (RunQueuedData.Kept)
	keptOpen   bool      // a kept turn is open, and the branch did not make its model call yet
	keptCalls  int       // the tool calls that a branch kept from its source
	prints     []uint64  // full-history reference only; the cached cursor uses loop
	priorN     int       // reference prints before the latest response
	warned     bool      // the kernel warned about a loop after the latest response
	respAt     time.Time // the time of the latest response
	noteAt     time.Time // the time of the latest time note
	zone       string    // the zone that fw.run.started recorded

	// The budget notes and the final report (budget.go).
	noted      map[noteKey]int // the highest threshold that a budget note gave, by limit
	changeSeq  uint64          // the latest fw.budget.changed
	changeNote uint64          // the latest budget note about a change
	answered   bool            // a model call of the run answered, a report call too
	rep        *reportState    // the final report that is open, or nil
	reportSeq  uint64          // the latest final report that answered or failed
	report     string          // the text of that report

	loop       loopSuffix // exact repeat counts with a bounded fingerprint suffix
	priorCount int        // repeat count before the latest response

	routine routineCursor // the routine runner of the run (routine_run.go)
}

// analyze is the cold full-history reference. The turn loop uses the
// incremental executionCursor; equivalence tests compare every cursor field.
func analyze(events []Event) *cursor {
	c := &cursor{}
	lastResp, prevResp := -1, -1
	// The final report call is not a turn: its request and its response
	// stay out of the turn logic, the loop guard, and the latest response.
	reportReq, reportResp := map[int]bool{}, map[int]bool{}
	reporting := false
	for i, e := range events {
		switch e.Type {
		case EvModelRequested:
			var d struct {
				Report bool `json:"report"`
			}
			reporting = e.Decode(&d) == nil && d.Report
			reportReq[i] = reporting
		case EvModelResponded:
			if reporting {
				reportResp[i] = true
				continue
			}
			prevResp, lastResp = lastResp, i
		}
	}
	approvals := map[string]ApprovalRequestedData{}
	var open []string
	for i, e := range events {
		c.lastSeq = e.Seq
		c.applyRoutine(e)
		switch e.Type {
		case EvRunQueued:
			_ = e.Decode(&c.queued)
		case EvRunStarted:
			c.started = true
			var d RunStartedData
			if e.Decode(&d) == nil {
				c.brief, c.context, c.zone = d.Brief, d.Context, d.TimeZone
			}
		case EvContextCompacted:
			c.compactSeq = e.Seq
		case EvMessageReceived:
			c.terminal = false
			c.unseen = true
			var d MessageReceivedData
			if e.Decode(&d) != nil {
				break
			}
			if d.fromAgent() {
				c.fromAgent = true
			}
			if d.Origin != OriginKernel {
				c.reportInput(false)
				break
			}
			if d.Role != RoleSystem {
				break
			}
			switch d.Note {
			case NoteTime:
				c.noteAt = e.Time
			case NoteBudget:
				c.budgetNote(e.Seq, d.Budget)
			case NoteReport:
				if c.rep != nil {
					c.rep.noteSeq = e.Seq
				}
			default:
				// Every other system message from the kernel is a loop
				// warning. A ledger from before the time notes has no
				// note kind on it.
				if i > lastResp {
					c.warned = true
				}
			}
		case EvBudgetChanged:
			c.changeSeq = e.Seq
			c.reportInput(true)
		case EvBudgetExceeded:
			var d BudgetExceededData
			if e.Decode(&d) == nil && d.Report {
				c.rep = &reportState{seq: e.Seq, data: d}
			}
		case EvError:
			if c.rep != nil && c.rep.reqSeq > 0 && !c.rep.done {
				// The final report call failed. It counts as the report,
				// so a later stop without a change makes no second call.
				c.reportDone(e.Seq, "")
			}
		case EvTurnStarted:
			c.turns++
			if e.Seq <= c.queued.Kept {
				c.keptTurns++
			}
			c.keptOpen = e.Seq <= c.queued.Kept
			c.turnOpen = true
			c.turnSeq = e.Seq
		case EvTurnFinished:
			c.turnOpen, c.keptOpen = false, false
		case EvModelRequested:
			c.unseen = false
			if reportReq[i] && c.rep != nil {
				c.rep.reqSeq = e.Seq
			}
		case EvModelResponded:
			c.answered = true
			if reportResp[i] {
				var d struct {
					Content []Block `json:"content"`
				}
				_ = e.Decode(&d)
				c.respAt = e.Time
				c.reportDone(e.Seq, TextOf(d.Content))
				continue
			}
			if c.keptOpen && e.Seq > c.queued.Kept {
				c.keptTurns-- // the branch made the call of the turn that it kept open
			}
			c.keptOpen = false
			var d ModelRespondedData
			if err := e.Decode(&d); err != nil {
				continue
			}
			if i == lastResp {
				c.priorN = len(c.prints)
			}
			for _, b := range toolUses(d.Content) {
				c.prints = append(c.prints, fingerprint(b))
			}
			if i != lastResp {
				continue
			}
			c.resp, c.respSeq, c.respAt = &d, e.Seq, e.Time
			c.calls = map[string]*callRecord{}
			if d.StopReason == StopToolUse {
				for _, b := range toolUses(d.Content) {
					c.calls[b.ID] = &callRecord{}
				}
			}
			if d.StopReason == StopMaxTokens {
				c.maxTokens = 1
				if prevResp >= 0 {
					var p struct {
						StopReason StopReason `json:"stopReason"`
					}
					if events[prevResp].Decode(&p) == nil && p.StopReason == StopMaxTokens {
						c.maxTokens = 2
					}
				}
			}
		case EvToolRequested, EvToolResponded:
			if e.Type == EvToolRequested {
				c.toolCalls++
				if e.Seq <= c.queued.Kept {
					c.keptCalls++
				}
			}
			if i < lastResp || c.calls == nil {
				continue
			}
			var d struct {
				CallID string `json:"callId"`
			}
			if e.Decode(&d) != nil {
				continue
			}
			if r := c.calls[d.CallID]; r != nil {
				if e.Type == EvToolRequested {
					r.requestedSeq = e.Seq
				} else {
					r.responded = true
				}
			}
		case EvApprovalRequested:
			var d ApprovalRequestedData
			if e.Decode(&d) == nil {
				approvals[d.ApprovalID] = d
				open = append(open, d.ApprovalID)
				if d.Connect != nil && c.calls != nil {
					if r := c.calls[d.CallID]; r != nil {
						r.connectSeq, r.connect, r.connectDeny = e.Seq, d.Connect, nil
					}
				}
				// A question of a call is a schema-bearing approval with
				// no connect. Its decision lives in the ledger and not in
				// a channel, so a resume can read it back.
				if d.Connect == nil && len(d.Schema) > 0 && c.calls != nil {
					if r := c.calls[d.CallID]; r != nil {
						r.questions = append(r.questions, &questionRecord{req: d})
					}
				}
			}
		case EvApprovalDecided:
			var d ApprovalDecidedData
			if e.Decode(&d) != nil {
				continue
			}
			for j, id := range open {
				if id == d.ApprovalID {
					open = append(open[:j], open[j+1:]...)
					break
				}
			}
			// A question from a tool carries a schema. Only an approval of
			// the call itself decides whether the call runs. A connect
			// that the person skipped ends the call.
			req, isReq := approvals[d.ApprovalID]
			if isReq && len(req.Schema) > 0 && req.Connect == nil && c.calls != nil {
				if r := c.calls[req.CallID]; r != nil {
					for _, q := range r.questions {
						if q.req.ApprovalID == d.ApprovalID {
							q.decided = &d
						}
					}
				}
			}
			if isReq && len(req.Schema) == 0 && c.calls != nil {
				if r := c.calls[req.CallID]; r != nil {
					switch {
					case req.Connect == nil:
						r.decision, r.decisionSeq = &d, e.Seq
					case d.Decision != "allow":
						r.connectDeny = &d
					}
				}
			}
		case EvRunFinished:
			c.terminal = true
			c.turnOpen = false
			c.rep = nil
			open = nil
		}
	}
	if len(open) > 0 {
		d := approvals[open[len(open)-1]]
		c.pending = &d
	}
	for _, print := range c.prints {
		c.loop.add(print)
	}
	_, c.priorCount = repeats(c.prints[:c.priorN])
	return c
}

// fingerprint hashes the name and the input of a tool call. The input is
// canonical JSON, so the order of the keys does not matter.
func fingerprint(b Block) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(b.Name))
	_, _ = h.Write([]byte{0})
	var v any
	if json.Unmarshal(b.Input, &v) == nil {
		if canon, err := json.Marshal(v); err == nil {
			_, _ = h.Write(canon)
			return h.Sum64()
		}
	}
	_, _ = h.Write(b.Input)
	return h.Sum64()
}

// repeats finds the loop at the end of a sequence of fingerprints. It
// returns the period of the loop and how many times in a row the loop
// appears at the end: for a, a, a the period is 1 and the count is 3; for
// a, b, a, b the period is 2 and the count is 2. It prefers the loop with
// the most repeats, and the shortest period on a tie.
func repeats(prints []uint64) (period, count int) {
	period, count = 1, 1
	n := len(prints)
	for p := 1; p <= maxLoopPeriod && p <= n; p++ {
		k := 1
		for (k+1)*p <= n {
			same := true
			for j := 0; j < p; j++ {
				if prints[n-(k+1)*p+j] != prints[n-p+j] {
					same = false
					break
				}
			}
			if !same {
				break
			}
			k++
		}
		if k > count {
			period, count = p, k
		}
	}
	return period, count
}

// drive runs the steps of a run until it is idle or stopped.
func (a *Agent) drive(ctx context.Context, rs *runState) exitReason {
	for {
		if a.k.isDraining() {
			return exitDrain
		}
		if ctx.Err() != nil {
			return exitCanceled
		}
		c, n := rs.executionCursor()
		var err error
		switch {
		case c.routine.open() && !c.terminal:
			// A restart cut a live routine off, maybe while it waited
			// for an approval: the routine ends before anything else.
			err = a.closeRoutine(ctx, rs, c)
		case c.terminal || c.pending != nil:
			if a.idleExit(rs, c.lastSeq) {
				return exitIdle
			}
			continue
		case !c.started:
			err = a.stepStart(ctx, rs, c)
		case c.queued.Check != nil:
			// A check run makes its calls and finishes with no model call.
			err = a.stepCheck(ctx, rs, c)
		case a.routineDue(rs, c):
			// The routine runner acts once, before the first model turn.
			err = a.stepRoutine(ctx, rs, c)
		case routineFinishDue(c):
			err = a.finishRoutine(ctx, rs, c, n)
		case c.rep != nil:
			// The run reached a limit and makes its final report.
			err = a.stepReport(ctx, rs, c, n)
		case c.turnOpen && c.respSeq < c.turnSeq:
			// The turn has no response yet: call the model, or call it
			// again after a restart.
			err = a.stepModel(ctx, rs, c)
		case c.turnOpen:
			err = a.stepResponse(ctx, rs, c, n)
		case c.resp != nil && !c.unseen && settles(c):
			// The turn closed, but the run did not record its end.
			err = a.stepResponse(ctx, rs, c, n)
		default:
			err = a.stepModel(ctx, rs, c)
		}
		if err != nil {
			if errors.Is(err, errStopped) || ctx.Err() != nil {
				if a.k.isDraining() {
					return exitDrain
				}
				return exitCanceled
			}
			a.log.Error("run driver stopped", "run", rs.id, "error", err)
			return exitError
		}
	}
}

// idleExit releases the run when no event arrived since the driver read the
// log. Send and Decide launch a new driver when none is active, so a message
// that arrives at the same moment is never lost.
func (a *Agent) idleExit(rs *runState, seen uint64) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.run.Seq > seen {
		return false
	}
	rs.active = false
	return true
}

// settles reports whether the latest response ends the run once its turn is
// closed.
func settles(c *cursor) bool {
	switch c.resp.StopReason {
	case StopToolUse:
		return len(c.calls) == 0
	case StopPauseTurn:
		return false
	case StopMaxTokens:
		return c.maxTokens >= 2
	}
	return true
}

func (a *Agent) stepStart(ctx context.Context, rs *runState, c *cursor) error {
	prof := a.profileOf(c.queued)
	tools, _ := a.runTools(ctx, rs, prof)
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Spec().Name)
	}
	_, zone := a.zone(a.zoneNameOf(ctx, rs))
	if o := a.k.override(rs.id); o != nil && o.zone != nil && *o.zone != "" {
		zone = *o.zone
	}
	d := RunStartedData{Tools: names, TimeZone: zone}
	// A check run calls no model, so it names none, and no tool writes a
	// brief or a context for it.
	if c.queued.Check == nil {
		d.Model, d.Brief, d.Context = a.selectorOf(c.queued, prof), a.brief(ctx, rs, tools), a.briefContext(ctx, rs, tools)
	}
	_, err := a.emit(ctx, rs, EvRunStarted, d)
	if err == nil {
	}
	return err
}

// brief asks each tool of the run that implements Briefer for its section
// of the system prompt. A replay takes the brief of the recorded run.
func (a *Agent) brief(ctx context.Context, rs *runState, tools []Tool) string {
	if o := a.k.override(rs.id); o != nil && o.brief != nil {
		return *o.brief
	}
	info := a.runInfo(rs)
	bctx := WithRunInfo(WithAgent(ctx, a), info)
	var parts []string
	for _, t := range tools {
		b, ok := brieferOf(t)
		if !ok {
			continue
		}
		text, err := b.Brief(bctx, info)
		if err != nil {
			a.log.Warn("tool brief failed", "run", rs.id, "tool", t.Spec().Name, "error", err)
			continue
		}
		if text = strings.TrimSpace(text); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// callRequest builds the request of a model call of the run without its
// messages: the system prompt, the tools, and the effort of a turn. It
// returns the effort that the call asks for. The final report call takes
// the same prefix, so it reads the prompt cache of the run.
func (a *Agent) callRequest(ctx context.Context, rs *runState, c *cursor, prof AgentProfile) (*Request, string, error) {
	tools, _ := a.runTools(ctx, rs, prof)
	specs := make([]ToolSpec, 0, len(tools))
	for _, t := range tools {
		specs = append(specs, t.Spec())
	}
	effort := prof.Effort
	if effort == "" && c.queued.Resolved == nil {
		effort = a.cfg.Effort
	}
	if c.queued.Resolved != nil {
		text, err := a.textOf(ctx, rs, c.queued)
		if err != nil {
			return nil, "", err
		}
		prof.System, prof.Instructions = text.system, text.instructions
	}
	system := a.SystemPrompt(prof, c.queued.Instructions)
	if c.brief != "" {
		system += "\n\n" + c.brief
	}
	for _, name := range systemNotes(c) {
		system += "\n\n" + systemNoteText[name]
	}
	return &Request{
		System:    system,
		Tools:     specs,
		Effort:    effort,
		EffortSet: c.queued.Resolved != nil,
		MaxTokens: defaultMaxTokens,
		Metadata:  map[string]string{"runId": rs.id, "agent": rs.agent},
	}, effort, nil
}

// systemNoteText holds the sentence of each name of SystemNotes.
var systemNoteText = map[string]string{
	SystemNoteContext: ContextNote,
	SystemNoteMessage: MessageNote,
}

// systemNotes names the sentences of the kernel that end the system prompt
// of a model call of the run, in their order. callRequest adds them, and
// fw.model.requested records their names. ContextNote comes when a tool
// added data to the first user message. MessageNote comes with the first
// message from an agent, so a run that never gets one keeps its prompt
// and its cached prefix. Neither is part of the text of the profile, so
// the digests of the run (ResolvedProfile) stay as they are.
func systemNotes(c *cursor) []string {
	var names []string
	if c.context != "" {
		names = append(names, SystemNoteContext)
	}
	if c.fromAgent {
		names = append(names, SystemNoteMessage)
	}
	return names
}

// stepModel checks the budget, compacts the history when it is too long,
// and calls the model. It calls once more when the network cut the
// response (StreamInterruptedError). A call that is in flight when the
// working time reaches the deadline ends at the deadline.
func (a *Agent) stepModel(ctx context.Context, rs *runState, c *cursor) error {
	prof := a.profileOf(c.queued)
	if o, over := a.overBudget(rs, c); over {
		return a.stopOver(ctx, rs, c, o)
	}
	selector := a.selectorOf(c.queued, prof)
	model, err := a.openModel(ctx, selector)
	if err != nil {
		return a.failAs(ctx, rs, "model: "+err.Error(), RunStopModelError)
	}
	req, effort, err := a.callRequest(ctx, rs, c, prof)
	if err != nil {
		return err
	}
	provider, name := SplitSelector(selector)
	extra := (len(req.System) + len(c.context) + jsonSize(req.Tools)) / bytesPerToken
	if !c.turnOpen {
		before := *rs.usage()
		if err := a.maybeCompact(ctx, rs, model, req, extra); err != nil {
			return err
		}
		// A compaction is a model call of its own, so the budget is checked
		// again before the call of the turn.
		if *rs.usage() != before {
			if o, over := a.overBudget(rs, c); over {
				return a.stopOver(ctx, rs, c, o)
			}
		}
	}

	note := a.timeNote(ctx, rs, c)
	notes := a.budgetNotes(ctx, rs, c)

	// The request and its event are one step, so a message that arrives now
	// is either in this request or after its fw.model.requested.
	rs.emitMu.Lock()
	if !c.turnOpen {
		if _, err := a.emitLocked(ctx, rs, EvTurnStarted, TurnData{Turn: c.turns + 1}); err != nil {
			rs.emitMu.Unlock()
			return err
		}
	}
	if note != "" {
		if _, err := a.emitLocked(ctx, rs, EvMessageReceived, MessageReceivedData{Role: RoleSystem, Content: []Block{Text(note)}, Origin: OriginKernel, Note: NoteTime}); err != nil {
			rs.emitMu.Unlock()
			return err
		}
	}
	for _, n := range notes {
		if _, err := a.emitLocked(ctx, rs, EvMessageReceived, n); err != nil {
			rs.emitMu.Unlock()
			return err
		}
	}
	h, tokens := rs.historyLocked(extra)
	if endsWithAnswer(h.Messages, c) {
		rs.emitMu.Unlock()
		return a.failAs(ctx, rs, "fw: the history of the next model call ends with an answer of the model and no message after it, and a provider refuses such a request", RunStopError)
	}
	req.Messages = withRunContext(c.context, a.readFiles(h.Messages, MediaTypesOf(model), a.dirOf(rs)))
	_, err = a.emitLocked(ctx, rs, EvModelRequested, ModelRequestedData{
		Provider:             provider,
		Model:                name,
		Effort:               requestedEffort(effort),
		EstimatedInputTokens: tokens,
		SystemNotes:          systemNotes(c),
	})
	rs.emitMu.Unlock()
	if err != nil {
		return err
	}

	mctx, cut := a.deadlineCut(ctx, rs)
	start := time.Now()
	// The text of the answer goes to the live stream of the run as the
	// adapter yields it (live.go); the ledger records the answer whole.
	turn := c.turns
	if !c.turnOpen {
		turn++
	}
	live := a.liveOn(rs.id, turn)
	resp, err := model.Stream(mctx, req, live)
	var interrupted *StreamInterruptedError
	if mctx.Err() == nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && errors.As(err, &interrupted) {
		// No delta leaves this call, and the cut response is never
		// recorded. The same request goes again in the same turn.
		if _, e := a.emit(ctx, rs, EvError, ErrorData{Message: "model: " + err.Error() + " (retrying the interrupted response)", Retryable: true}); e != nil {
			cut()
			return e
		}
		resp = nil
		timer := time.NewTimer(modelStreamRetryWait)
		select {
		case <-mctx.Done():
			timer.Stop()
			err = mctx.Err()
		case <-timer.C:
			if err = mctx.Err(); err == nil {
				resp, err = model.Stream(mctx, req, live)
			}
		}
	}
	latency := time.Since(start).Milliseconds()
	a.liveDone(rs.id, turn, resp, err)
	if cut() && err != nil && ctx.Err() == nil {
		// The working time reached the deadline while the call was in
		// flight. The call has no answer to record; the run reports.
		return a.stopAtDeadline(ctx, rs, c)
	}
	if err != nil {
		if ctx.Err() != nil {
			return errStopped
		}
		// A request that does not fit the context window gets one
		// compaction per turn, and the next step calls the model again.
		retried := c.turnOpen && c.compactSeq > c.turnSeq
		if at, _ := a.compactionOf(rs); IsContextOverflow(err) && at > 0 && !retried {
			msg := "model: " + err.Error()
			if _, err := a.emit(ctx, rs, EvError, ErrorData{Message: msg + " (compacting the context)", Retryable: true}); err != nil {
				return err
			}
			ok, err := a.compact(ctx, rs, model, req)
			if err != nil || ok {
				return err
			}
			return a.finishAs(ctx, rs, OutcomeFailed, "", msg, RunStopModelError)
		}
		return a.failAs(ctx, rs, "model: "+err.Error(), RunStopModelError)
	}
	if resp == nil {
		return a.failAs(ctx, rs, "model: no response", RunStopModelError)
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

// endsWithAnswer reports whether the messages of a model call end with an
// answer of the model. A provider takes such a request as a prefill, and
// newer models refuse it. Only the call that continues a paused turn
// (StopPauseTurn) sends the answer last on purpose.
func endsWithAnswer(msgs []Message, c *cursor) bool {
	n := len(msgs)
	return n > 0 && msgs[n-1].Role == RoleAssistant && (c.resp == nil || c.resp.StopReason != StopPauseTurn)
}

// stepResponse acts on the stop reason of the latest response.
func (a *Agent) stepResponse(ctx context.Context, rs *runState, c *cursor, n int) error {
	switch c.resp.StopReason {
	case StopToolUse:
		if len(c.calls) > 0 {
			return a.stepTools(ctx, rs, c)
		}
		return a.endTurn(ctx, rs, c, n)
	case StopPauseTurn:
		return a.closeTurn(ctx, rs, c)
	case StopMaxTokens:
		if c.maxTokens >= 2 {
			if _, err := a.emit(ctx, rs, EvError, ErrorData{Message: "the model stopped at max_tokens again after a continuation"}); err != nil {
				return err
			}
			if err := a.closeTurn(ctx, rs, c); err != nil {
				return err
			}
			return a.finishAs(ctx, rs, OutcomeFailed, TextOf(c.resp.Content), "max_tokens", RunStopMaxTokens)
		}
		if _, err := a.emit(ctx, rs, EvError, ErrorData{Message: "the model stopped at max_tokens; asking it to continue", Retryable: true}); err != nil {
			return err
		}
		if _, err := a.emit(ctx, rs, EvMessageReceived, MessageReceivedData{Role: RoleUser, Content: []Block{Text("Continue.")}, Origin: OriginKernel}); err != nil {
			return err
		}
		return a.closeTurn(ctx, rs, c)
	case StopRefusal:
		if err := a.closeTurn(ctx, rs, c); err != nil {
			return err
		}
		detail := c.resp.StopDetail
		if detail == "" {
			detail = "refusal"
		}
		return a.finishAs(ctx, rs, OutcomeFailed, "", detail, RunStopRefusal)
	}
	return a.endTurn(ctx, rs, c, n)
}

// endTurn closes the turn and finishes the run, unless a message arrived
// that the model has not seen. Then the next step starts a new turn. The
// check and the events are one step, so no message is lost.
func (a *Agent) endTurn(ctx context.Context, rs *runState, c *cursor, n int) error {
	// A run with a definition of done has its work checked first
	// (done.go), and then a run with an outcome has its answer graded
	// (outcome.go).
	if checked, err := a.checkDone(ctx, rs, c, n); checked || err != nil {
		return err
	}
	if graded, err := a.gradeOutcome(ctx, rs, c, n); graded || err != nil {
		return err
	}
	rs.emitMu.Lock()
	defer rs.emitMu.Unlock()
	fresh := c.unseen
	for _, e := range rs.events[n:] {
		if e.Type == EvMessageReceived {
			fresh = true
		}
	}
	if c.turnOpen {
		if _, err := a.emitLocked(ctx, rs, EvTurnFinished, TurnData{Turn: c.turns}); err != nil {
			return err
		}
	}
	if fresh {
		return nil
	}
	output := TextOf(c.resp.Content)
	if err := a.compareLocked(ctx, rs, c, output); err != nil {
		return err
	}
	done := doneStateOf(rs)
	stop := RunStopAnswered
	if done == DoneChecked {
		stop = RunStopChecked
	}
	_, err := a.emitLocked(ctx, rs, EvRunFinished, RunFinishedData{Outcome: OutcomeCompleted, Output: output, Done: done, Stop: stop})
	return err
}

func (a *Agent) closeTurn(ctx context.Context, rs *runState, c *cursor) error {
	if !c.turnOpen {
		return nil
	}
	_, err := a.emit(ctx, rs, EvTurnFinished, TurnData{Turn: c.turns})
	return err
}

func (a *Agent) finish(ctx context.Context, rs *runState, outcome, output, msg string) error {
	return a.finishAs(ctx, rs, outcome, output, msg, "")
}

// finishAs is finish with the stop of the run. An empty stop takes the
// stop that StopOf derives from the outcome and the error.
func (a *Agent) finishAs(ctx context.Context, rs *runState, outcome, output, msg, stop string) error {
	_, err := a.emit(ctx, rs, EvRunFinished, RunFinishedData{Outcome: outcome, Output: output, Error: msg, Stop: stop})
	return err
}

// fail records an error and finishes the run as failed.
func (a *Agent) fail(ctx context.Context, rs *runState, msg string) error {
	return a.failAs(ctx, rs, msg, "")
}

// failAs is fail with the stop of the run (see finishAs).
func (a *Agent) failAs(ctx context.Context, rs *runState, msg, stop string) error {
	if _, err := a.emit(ctx, rs, EvError, ErrorData{Message: msg}); err != nil {
		return err
	}
	return a.finishAs(ctx, rs, OutcomeFailed, "", msg, stop)
}

// profileOf returns the effective profile of a run: the profile that the
// kernel resolved when it queued the run (resolveProfile), for the life of
// the run, without its text (textOf). Only the policy of the named profile
// now applies on top, as a tightening (tighten). A run from the ledger of
// an older kernel has no resolved profile, so the named profile now with
// the inline profile of the request on top stands in for it.
func (a *Agent) profileOf(q RunQueuedData) AgentProfile {
	if q.Resolved != nil {
		prof := q.Resolved.profile(q.Agent)
		prof.Policy = tighten(prof.Policy, a.profileNamed(q.Agent).Policy)
		return prof
	}
	prof := a.profileNamed(q.Agent)
	if q.Profile != nil {
		prof = prof.Overlay(*q.Profile)
	}
	return prof
}

// lineage returns the run and its ancestors, nearest first. A parent that
// the kernel does not know ends the walk.
func (a *Agent) lineage(rs *runState) []*runState {
	out := []*runState{rs}
	for cur := rs; cur.parent != ""; {
		p, err := a.k.lookup(cur.parent)
		if err != nil || len(out) > 64 {
			break
		}
		out = append(out, p)
		cur = p
	}
	return out
}

// budgetOf returns the effective budget of a run: its own budget
// (ownBudget), capped by the budget of the parent so that a child never
// outgrows its parent. The limits that a tree shares hold for the tree of
// each ancestor too (roomOf), so the cap of those is a bound on the run
// alone.
func (a *Agent) budgetOf(rs *runState) Budget {
	b := a.ownBudget(rs)
	if rs.parent != "" {
		if p, err := a.k.lookup(rs.parent); err == nil && rs.depth < 64 {
			b = b.Cap(a.budgetOf(p))
		}
	}
	return b
}

// overBudget compares the usage of the run with its effective budget, the
// usage of its tree and of the tree of each ancestor with the budget of
// that run, and the working time of the lineage with each deadline. The
// turns of a run count against its own budget alone.
func (a *Agent) overBudget(rs *runState, c *cursor) (overrun, bool) {
	b := a.budgetOf(rs)
	// A branch is charged for its own work only: the usage leaves out the
	// events that it kept from its source, and so do the turns and the
	// tool calls here. A kept turn that is still open is the turn of the
	// branch, because the branch makes its model call.
	turns, calls := c.turns-c.keptTurns, c.toolCalls-c.keptCalls
	next := turns
	if !c.turnOpen || c.keptOpen {
		next++
	}
	if b.MaxTurns > 0 && next > b.MaxTurns {
		return overrun{limit: "maxTurns", value: float64(turns), max: float64(b.MaxTurns), owner: rs}, true
	}
	if o, over := ownSpent(rs, b, calls); over {
		return o, true
	}
	if o, over := a.treeSpent(rs); over {
		return o, true
	}
	if d, over := a.pastDeadline(rs); over {
		return d.overrun(rs), true
	}
	return overrun{}, false
}

// pastDeadline reports whether the run or one of its ancestors used up the
// working time of its deadline, and returns the deadline that is nearest.
// The deadline counts working time (see WorkingTime), so a run that waited
// for a person or for a follow-up still has its time.
func (a *Agent) pastDeadline(rs *runState) (deadlineUse, bool) {
	d, ok := a.deadlineUse(context.Background(), rs, a.now())
	return d, ok && d.left() <= 0
}

// timeNote returns the time note for the next model call, or "". A note
// goes in when time.noteAfter or more passed since the latest response of
// the run and since the latest note. It gives the time in the zone of the
// run, the gap since the latest response, and the working time left before
// the deadline. The note is an event, so the history never changes after
// the fact.
func (a *Agent) timeNote(ctx context.Context, rs *runState, c *cursor) string {
	after := a.cfg.Time.NoteAfter
	if after <= 0 || c.respAt.IsZero() {
		return ""
	}
	now := a.now()
	since := c.respAt
	if c.noteAt.After(since) {
		since = c.noteAt
	}
	if now.Sub(since) < after {
		return ""
	}
	name := c.zone
	if name == "" {
		name = a.cfg.Time.Zone
	}
	loc, _ := a.zone(name)
	text := fmt.Sprintf("Time note: it is now %s, %s after your last response.", FormatTime(now.In(loc)), FormatDuration(now.Sub(c.respAt)))
	if left, ok := a.timeLeft(ctx, rs, now); ok {
		text += fmt.Sprintf(" This run has %s of working time left before its deadline.", FormatDuration(left))
	}
	return text
}

// runTools returns the tools that the run can use: the tools that its
// profile allows and that every ancestor allows, wrapped for replay when
// the run is a replay.
func (a *Agent) runTools(ctx context.Context, rs *runState, prof AgentProfile) ([]Tool, map[string]Tool) {
	// A tool source that acts for a person reads the run from ctx.
	all, err := a.Tools(WithRunInfo(WithAgent(ctx, a), a.runInfo(rs)))
	if err != nil {
		a.log.Warn("list tools", "run", rs.id, "error", err)
	}
	var globs [][]string
	globs = append(globs, prof.Tools)
	for _, anc := range a.lineage(rs)[1:] {
		globs = append(globs, a.profileOf(anc.queuedData()).Tools)
	}
	o := a.k.override(rs.id)
	var out []Tool
	byName := map[string]Tool{}
	for _, t := range all {
		spec := t.Spec()
		name := spec.Name
		allowed := true
		for _, g := range globs {
			if !toolAllowed(g, spec) {
				allowed = false
				break
			}
		}
		if !allowed {
			continue
		}
		if o != nil && o.wrap != nil {
			t = o.wrap(t)
		}
		out = append(out, t)
		byName[name] = t
	}
	return out, byName
}

// toolAllowed matches a tool against the globs of a profile. An empty list
// or "*" allows every tool except an opt-in tool, which a glob other than
// "*" must name.
func toolAllowed(globs []string, spec ToolSpec) bool {
	if len(globs) == 0 {
		return !spec.OptIn
	}
	for _, g := range globs {
		if g == "*" {
			if !spec.OptIn {
				return true
			}
			continue
		}
		if g == spec.Name {
			return true
		}
		if ok, err := path.Match(g, spec.Name); err == nil && ok {
			return true
		}
	}
	return false
}

// policyFor returns the policy of a run: the agent policy, made stricter by
// the rules of the profile and by the rules of every ancestor. A profile or
// a parent can add an ask or a deny, but cannot loosen the agent policy.
func (a *Agent) policyFor(rs *runState, prof AgentProfile) Policy {
	if o := a.k.override(rs.id); o != nil && o.policy != nil {
		return o.policy
	}
	base := a.Policy()
	merged, ok := a.runRules(rs, prof)
	if !ok {
		return base
	}
	extra := NewRulesPolicy(merged)
	return PolicyFunc(func(ctx context.Context, call ToolCall) Decision {
		d1, d2 := base.Decide(ctx, call), extra.Decide(ctx, call)
		if verdictRank(d2.Verdict) > verdictRank(d1.Verdict) {
			return d2
		}
		return d1
	})
}

// runRules returns the rules of the profile of a run and of every ancestor
// as one conjunction, and false when there are none.
func (a *Agent) runRules(rs *runState, prof AgentProfile) (Rules, bool) {
	var rules []Rules
	if prof.Policy != nil {
		rules = append(rules, *prof.Policy)
	}
	for _, anc := range a.lineage(rs)[1:] {
		if p := a.profileOf(anc.queuedData()); p.Policy != nil {
			rules = append(rules, *p.Policy)
		}
	}
	if len(rules) == 0 {
		return Rules{}, false
	}
	merged := rules[0]
	for _, r := range rules[1:] {
		merged = MergeRules(merged, r)
	}
	return merged, true
}

// recheck decides a call that a person allowed again, by the rules of the
// profile and of every ancestor, because a person can add a rule while the
// call waits. Only a deny counts. The policy of the process asked for the
// approval and is not asked again, and a replay allows every call.
func (a *Agent) recheck(ctx context.Context, rs *runState, prof AgentProfile, t Tool, u Block) Decision {
	if o := a.k.override(rs.id); o != nil && o.policy != nil {
		return Decision{Verdict: VerdictAllow}
	}
	merged, ok := a.runRules(rs, prof)
	if !ok {
		return Decision{Verdict: VerdictAllow}
	}
	pctx := a.policyCtx(ctx, rs, u.ID)
	call := ResolveCall(pctx, t, ToolCall{RunID: rs.id, Agent: rs.agent, CallID: u.ID, Spec: t.Spec(), Input: u.Input})
	return NewRulesPolicy(merged).Decide(pctx, call)
}

func verdictRank(v Verdict) int {
	switch v {
	case VerdictAllow:
		return 0
	case VerdictAsk:
		return 1
	}
	return 2
}

// plannedCall is one tool call of a response and what to do with it.
type plannedCall struct {
	use    Block
	tool   Tool
	spec   ToolSpec
	run    bool
	result ToolResult // the result when the call does not run
	// answered holds the recorded questions and answers of the call from
	// the ledger. The kernel fills it for a resumed call that a Continuer
	// completes on its own, in place of another run of the tool.
	answered []AnsweredQuestion
	// routine names the routine of a call of the routine runner
	// (ToolRequestedData.Routine).
	routine string
}

// stepTools decides every open call of the latest response, asks for an
// approval when the policy says so, runs the allowed calls, and records one
// result per call in the order of the tool_use blocks. Before that, it
// applies the loop guard and the per-turn and per-run call limits.
func (a *Agent) stepTools(ctx context.Context, rs *runState, c *cursor) error {
	prof := a.profileOf(c.queued)
	b := a.budgetOf(rs)
	uses := toolUses(c.resp.Content)
	// Policy and tool implementations receive mutable JSON inputs. Keep
	// their writes out of the immutable decoded response in the cursor cache.
	for i := range uses {
		uses[i].Input = bytes.Clone(uses[i].Input)
	}

	period, count := c.loop.repeats()
	if b.MaxRepeats > 0 && count >= b.MaxRepeats {
		tool := uses[len(uses)-1].Name
		if _, err := a.emit(ctx, rs, EvLoopDetected, LoopDetectedData{Tool: tool, Count: count, Period: period, Action: "failed"}); err != nil {
			return err
		}
		if _, err := a.emit(ctx, rs, EvBudgetExceeded, BudgetExceededData{Limit: "maxRepeats", Value: float64(count)}); err != nil {
			return err
		}
		if err := a.closeTurn(ctx, rs, c); err != nil {
			return err
		}
		return a.finishAs(ctx, rs, OutcomeFailed, TextOf(c.resp.Content), fmt.Sprintf("budget exceeded: maxRepeats (the model made the same %s call %d times in a row)", describeLoop(tool, period), count), RunStopLoop)
	}

	// A run whose tree, or the tree of an ancestor, used up its tokens or
	// its cost runs no more tools: each open call gets an error result,
	// and the run stops at its next step. So does a run past its deadline,
	// such as a run whose model answered just after the deadline.
	why := ""
	if o, spent := a.limitSpent(rs); spent && o.limit != "maxToolCalls" {
		why = o.why()
	} else if d, past := a.pastDeadline(rs); past {
		why = d.why(rs)
	}
	if why != "" {
		var plan []plannedCall
		for _, u := range uses {
			if rec := c.calls[u.ID]; rec != nil && !rec.responded {
				plan = append(plan, plannedCall{use: u, result: ErrorResult("not run: " + why + ". This run stops.")})
			}
		}
		if err := a.execute(ctx, rs, plan); err != nil {
			return err
		}
		return a.closeTurn(ctx, rs, c)
	}

	_, byName := a.runTools(ctx, rs, prof)
	policy := a.policyFor(rs, prof)
	room := -1 // tool calls left in the run, or -1 for no limit
	if b.MaxToolCalls > 0 {
		room = max(b.MaxToolCalls-(c.toolCalls-c.keptCalls), 0)
	}
	// The tool calls of a tree count against the budget of each run above
	// them, so the tightest tree of the lineage bounds this response too.
	calls := a.roomOf(rs).calls
	shared := -1
	if calls.set {
		shared = max(int(calls.left), 0)
	}
	var plan []plannedCall
	for i, u := range uses {
		rec := c.calls[u.ID]
		if rec == nil || rec.responded {
			continue
		}
		p := plannedCall{use: u}
		t, ok := byName[u.Name]
		switch {
		case !ok:
			p.result = ErrorResult("unknown tool: " + u.Name)
			plan = append(plan, p)
			continue
		case b.MaxCallsPerTurn > 0 && i >= b.MaxCallsPerTurn:
			p.result = ErrorResult(fmt.Sprintf("not run: this response holds more than %d tool calls (budget.maxCallsPerTurn), and only the first %d ran. Make fewer calls per response.", b.MaxCallsPerTurn, b.MaxCallsPerTurn))
			plan = append(plan, p)
			continue
		case room == 0:
			p.result = ErrorResult(fmt.Sprintf("not run: the run reached its limit of %d tool calls (budget.maxToolCalls). Finish with what you have.", b.MaxToolCalls))
			plan = append(plan, p)
			continue
		case shared == 0:
			p.result = ErrorResult(fmt.Sprintf("not run: the runs of this tree used the %d tool calls of the budget of run %s (budget.maxToolCalls, which a run shares with the runs under it). Finish with what you have.", int(calls.max), calls.owner.id))
			plan = append(plan, p)
			continue
		}
		p.tool, p.spec = t, t.Spec()
		switch {
		case rec.connectDeny != nil && rec.connectSeq > rec.requestedSeq:
			p.result = ErrorResult(fmt.Sprintf("not run: %s needs the person who started this run to connect it, and they chose not to (%s).",
				connectTitle(rec.connect), deniedBy(rec.connectDeny)))
		case rec.connectSeq > rec.requestedSeq && rec.connectSeq > rec.decisionSeq:
			// The call waited for a connection when the process stopped,
			// so the service never saw it. It runs again and asks again
			// when the person is still not connected.
			p.run = true
		case rec.decision != nil && rec.decisionSeq > rec.requestedSeq:
			if rec.decision.Decision != "allow" {
				p.result = ErrorResult(deniedBy(rec.decision))
			} else if d := a.recheck(ctx, rs, prof, t, u); d.Verdict == VerdictDeny {
				// A deny that a profile gained while the call waited for
				// its approval wins over the allow.
				p.result = ErrorResult("denied by policy: " + d.Reason)
			} else {
				p.run = true
			}
		case rec.requestedSeq > 0:
			// The call started before a restart and has no result.
			if !rerunSafe(p.tool, p.spec, u.Input) {
				answered, ok := continueAsk(p.tool, rec)
				if !ok {
					return a.requestApproval(ctx, rs, u, reasonRerun, WaitRerun)
				}
				p.answered = answered
			}
			p.run = true
		default:
			pctx := a.policyCtx(ctx, rs, u.ID)
			call := ResolveCall(pctx, t, ToolCall{RunID: rs.id, Agent: rs.agent, CallID: u.ID, Spec: p.spec, Input: u.Input})
			d := policy.Decide(pctx, call)
			switch d.Verdict {
			case VerdictAllow:
				p.run = true
			case VerdictAsk:
				return a.requestApproval(ctx, rs, u, d.Reason, WaitApproval)
			default:
				p.result = ErrorResult("denied by policy: " + d.Reason)
			}
		}
		if p.run && room > 0 {
			room--
		}
		if p.run && shared > 0 {
			shared--
		}
		plan = append(plan, p)
	}
	if err := a.execute(ctx, rs, plan); err != nil {
		return err
	}
	// Warn once, on the response that brings the loop one repeat short of
	// the limit.
	if count >= 2 && count == b.MaxRepeats-1 && c.priorCount < count && !c.warned {
		if err := a.warnLoop(ctx, rs, uses[len(uses)-1].Name, period, count); err != nil {
			return err
		}
	}
	return a.closeTurn(ctx, rs, c)
}

// warnLoop tells the model that it repeats itself. The warning is a system
// message from the kernel, so it follows the tool results in the history.
func (a *Agent) warnLoop(ctx context.Context, rs *runState, tool string, period, count int) error {
	if _, err := a.emit(ctx, rs, EvLoopDetected, LoopDetectedData{Tool: tool, Count: count, Period: period, Action: "warned"}); err != nil {
		return err
	}
	text := fmt.Sprintf("You have made the same %s %d times in a row with the same input. Repeating it will not change the result. "+
		"Change your approach, or stop and report what blocks you. If you repeat it once more, this run ends.", describeLoop(tool, period), count)
	_, err := a.emit(ctx, rs, EvMessageReceived, MessageReceivedData{Role: RoleSystem, Content: []Block{Text(text)}, Origin: OriginKernel, Note: NoteLoop})
	return err
}

// describeLoop names a loop for a message: one call, or a cycle of calls.
func describeLoop(tool string, period int) string {
	if period == 1 {
		return "tool call (" + tool + ")"
	}
	return fmt.Sprintf("sequence of %d tool calls (ending with %s)", period, tool)
}

func deniedBy(d *ApprovalDecidedData) string {
	who := d.Decider
	if who == "" {
		who = "caller"
	}
	if d.Note == "" {
		return "denied by " + who
	}
	return "denied by " + who + ": " + d.Note
}

// continueAsk decides whether a resumed open call completes from the
// answers that the ledger already holds instead of running the tool again.
// The call must hold at least one recorded question, every recorded question
// must have its decision, and the tool must be a Continuer. Any other shape
// keeps the rerun gate of a non-idempotent tool. A call with a question
// that still waits never reaches here, because the run stays idle.
func continueAsk(t Tool, rec *callRecord) ([]AnsweredQuestion, bool) {
	if len(rec.questions) == 0 {
		return nil, false
	}
	out := make([]AnsweredQuestion, 0, len(rec.questions))
	for _, q := range rec.questions {
		if q.decided == nil {
			return nil, false
		}
		req, decision := q.req, *q.decided
		req.Input, req.Schema = bytes.Clone(req.Input), bytes.Clone(req.Schema)
		decision.Content = bytes.Clone(decision.Content)
		out = append(out, AnsweredQuestion{Request: req, Decision: decision})
	}
	if _, ok := continuerOf(t); !ok {
		return nil, false
	}
	return out, true
}

// requestApproval appends fw.approval.requested for a call. The run moves to
// input-required, and the driver goes idle until Decide.
func (a *Agent) requestApproval(ctx context.Context, rs *runState, u Block, reason, wait string) error {
	_, err := a.emit(ctx, rs, EvApprovalRequested, ApprovalRequestedData{
		ApprovalID: NewID(),
		Tool:       u.Name,
		CallID:     u.ID,
		Input:      u.Input,
		Reason:     reason,
		Wait:       wait,
	})
	return err
}

// execute runs the plan. Consecutive parallel-safe calls run at the same
// time. Every other call runs alone.
func (a *Agent) execute(ctx context.Context, rs *runState, plan []plannedCall) error {
	for i := 0; i < len(plan); {
		if a.k.isDraining() || ctx.Err() != nil {
			return errStopped
		}
		j := i + 1
		if plan[i].run && plan[i].spec.ParallelSafe {
			for j < len(plan) && plan[j].run && plan[j].spec.ParallelSafe {
				j++
			}
		}
		if err := a.runBatch(ctx, rs, plan[i:j]); err != nil {
			return err
		}
		i = j
	}
	return nil
}

type callOutcome struct {
	res ToolResult
	ms  int64
}

// runBatch runs a batch of calls and then records their results in order.
func (a *Agent) runBatch(ctx context.Context, rs *runState, batch []plannedCall) error {
	if len(batch) == 1 && batch[0].run {
		_, err := a.runOne(ctx, rs, batch[0])
		return err
	}
	outs := make([]callOutcome, len(batch))
	var aborted atomic.Bool
	var wg sync.WaitGroup
	for i := range batch {
		p := batch[i]
		if !p.run {
			outs[i] = callOutcome{res: p.result}
			continue
		}
		if err := a.requestCall(ctx, rs, p); err != nil {
			wg.Wait()
			return err
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outs[i] = a.invoke(ctx, rs, p, &aborted)
		}(i)
	}
	wg.Wait()
	if ctx.Err() != nil || aborted.Load() {
		// The result of a call that the shutdown interrupted is not a real
		// result. Resume runs the call again or asks first.
		return errStopped
	}
	for i, p := range batch {
		if _, err := a.respondCall(ctx, rs, p, outs[i]); err != nil {
			return err
		}
	}
	return nil
}

// runOne runs one call that runs: fw.tool.requested, the call, and
// fw.tool.responded. It returns the result as the ledger records it. The
// turn loop and the routine runner both run their calls through it.
func (a *Agent) runOne(ctx context.Context, rs *runState, p plannedCall) (ToolResult, error) {
	if err := a.requestCall(ctx, rs, p); err != nil {
		return ToolResult{}, err
	}
	var aborted atomic.Bool
	out := a.invoke(ctx, rs, p, &aborted)
	if ctx.Err() != nil || aborted.Load() {
		return ToolResult{}, errStopped
	}
	return a.respondCall(ctx, rs, p, out)
}

// requestCall records fw.tool.requested for a call that runs.
func (a *Agent) requestCall(ctx context.Context, rs *runState, p plannedCall) error {
	_, err := a.emit(ctx, rs, EvToolRequested, ToolRequestedData{Tool: p.use.Name, CallID: p.use.ID, Input: p.use.Input,
		Idempotent: p.spec.Idempotent, Source: p.spec.Source, Routine: p.routine})
	return err
}

// respondCall records fw.tool.responded for a call and returns the result
// as recorded: structured content without text becomes the text.
func (a *Agent) respondCall(ctx context.Context, rs *runState, p plannedCall, out callOutcome) (ToolResult, error) {
	res := out.res
	if len(res.Content) == 0 && len(res.Structured) > 0 {
		res.Content = []Block{Text(string(res.Structured))}
	}
	res.Content = validJSONBlocks(res.Content)
	_, err := a.emit(ctx, rs, EvToolResponded, ToolRespondedData{
		CallID:    p.use.ID,
		Tool:      p.use.Name,
		Content:   res.Content,
		IsError:   res.IsError,
		ExitCode:  res.ExitCode,
		LatencyMS: out.ms,
	})
	return res, err
}

// invoke calls one tool. A panic or a Go error becomes an error result.
func (a *Agent) invoke(ctx context.Context, rs *runState, p plannedCall, aborted *atomic.Bool) (out callOutcome) {
	start := time.Now()
	defer func() {
		if r := recover(); r != nil {
			out.res = ErrorResult(fmt.Sprintf("tool %s panicked: %v", p.use.Name, r))
		}
		out.ms = time.Since(start).Milliseconds()
	}()
	input := p.use.Input
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	res, err := a.callTool(ctx, rs, p, input, aborted)
	if err != nil {
		res = ErrorResult(fmt.Sprintf("tool %s: %v", p.use.Name, err))
	}
	out.res = res
	return out
}

// callTool runs the tool of one planned call. A call with recorded answers
// goes to the Continuer of the tool instead of Call, and its context holds
// no asker or connecter, so the continuation cannot ask again.
func (a *Agent) callTool(ctx context.Context, rs *runState, p plannedCall, input json.RawMessage, aborted *atomic.Bool) (res ToolResult, err error) {
	callID := p.use.ID
	info := a.runInfo(rs)
	info.CallID = callID
	tctx := WithRunInfo(ctx, info)
	tctx = WithAgent(tctx, a)
	if p.answered != nil {
		cont, ok := continuerOf(p.tool)
		if !ok {
			return ToolResult{}, fmt.Errorf("fw: tool %s cannot continue from the recorded answers", p.use.Name)
		}
		return cont.Continue(tctx, input, p.answered)
	}
	tctx = WithProgress(tctx, func(progress, total float64, message string) {
		_, _ = a.emit(ctx, rs, EvToolProgress, ToolProgressData{CallID: callID, Progress: progress, Total: total, Message: message})
	})
	tctx = WithAsker(tctx, a.asker(ctx, rs, p.use, aborted))
	tctx = WithConnecter(tctx, a.connecter(ctx, rs, p.use, aborted))
	tctx = WithMemoryRecorder(tctx, func(d MemoryChangedData) {
		d.CallID, d.Tool = callID, p.use.Name
		_, _ = a.emit(ctx, rs, EvMemoryChanged, d)
	})
	if a.files != nil {
		tool := p.use.Name
		ws, err := a.runWorkspace(a.dirOf(rs), func(d FileChangedData) {
			d.CallID, d.Tool = callID, tool
			_, _ = a.emit(ctx, rs, EvFileChanged, d)
		})
		if err != nil {
			return ErrorResult(fmt.Sprintf("tool %s: %v", p.use.Name, err)), nil
		}
		tctx = withFiles(tctx, ws)
	}
	res, err = p.tool.Call(tctx, input)
	return res, err
}

// policyCtx returns the context in which a tool resolves a call before the
// policy decides it: the run, and the workspace of its directory, so the
// files tool names the path that the call will open (section 7.5).
func (a *Agent) policyCtx(ctx context.Context, rs *runState, callID string) context.Context {
	info := a.runInfo(rs)
	info.CallID = callID
	ctx = WithRunInfo(ctx, info)
	if a.files != nil {
		ctx = withFiles(ctx, a.files.sub(a.dirOf(rs)))
	}
	return ctx
}

// asker returns the Asker of one tool call. A question is an approval with
// a form schema: the run moves to input-required until Decide answers it.
func (a *Agent) asker(runCtx context.Context, rs *runState, use Block, aborted *atomic.Bool) Asker {
	return func(ctx context.Context, question string, schema []byte) ([]byte, error) {
		freeText := len(schema) == 0
		if freeText {
			schema = []byte(answerSchema)
		}
		d, err := a.waitDecision(ctx, runCtx, rs, ApprovalRequestedData{
			Tool:   use.Name,
			CallID: use.ID,
			Reason: question,
			Schema: json.RawMessage(schema),
			Wait:   WaitQuestion,
		}, aborted)
		if err != nil {
			return nil, err
		}
		if d.Decision != "allow" {
			return nil, fmt.Errorf("fw: %s", deniedBy(&d))
		}
		content := []byte(d.Content)
		if len(content) == 0 && freeText {
			content, _ = json.Marshal(map[string]string{"answer": d.Note})
		}
		return content, nil
	}
}

// connecter returns the Connecter of one tool call. A connect is an
// approval with Connect set: the run moves to input-required until the
// person connects the service or skips it.
func (a *Agent) connecter(runCtx context.Context, rs *runState, use Block, aborted *atomic.Bool) Connecter {
	return func(ctx context.Context, req ConnectRequest) error {
		d, err := a.waitDecision(ctx, runCtx, rs, ApprovalRequestedData{
			Tool:    use.Name,
			CallID:  use.ID,
			Reason:  req.Message(),
			Connect: &req,
			Wait:    WaitConnect,
		}, aborted)
		if err != nil {
			return err
		}
		if d.Decision != "allow" {
			return fmt.Errorf("%w: %s", ErrConnectSkipped, deniedBy(&d))
		}
		return nil
	}
}

// waitDecision appends fw.approval.requested with a new approval id and
// waits for its decision. The shutdown of the process sets aborted, so
// resume runs the call again.
func (a *Agent) waitDecision(ctx, runCtx context.Context, rs *runState, req ApprovalRequestedData, aborted *atomic.Bool) (ApprovalDecidedData, error) {
	req.ApprovalID = NewID()
	ch := make(chan ApprovalDecidedData, 1)
	rs.mu.Lock()
	rs.waiters[req.ApprovalID] = ch
	rs.mu.Unlock()
	defer func() {
		rs.mu.Lock()
		delete(rs.waiters, req.ApprovalID)
		rs.mu.Unlock()
	}()
	if _, err := a.emit(runCtx, rs, EvApprovalRequested, req); err != nil {
		return ApprovalDecidedData{}, err
	}
	// A child run that waits for a person gives up its slot of
	// spawn.maxRunning, so the parent that relays the question can work.
	defer a.park(rs)()
	select {
	case d := <-ch:
		return d, nil
	case <-ctx.Done():
		return ApprovalDecidedData{}, ctx.Err()
	case <-a.k.drainCh:
		aborted.Store(true)
		return ApprovalDecidedData{}, ErrClosed
	}
}

// runInfo returns the run info of rs for a tool, without a call id.
func (a *Agent) runInfo(rs *runState) RunInfo {
	q := rs.queuedData()
	return RunInfo{RunID: rs.id, Agent: rs.agent, Principal: q.Principal, PrincipalKind: q.PrincipalKind, Dir: a.absDir(a.dirOf(rs))}
}

func toolUses(blocks []Block) []Block {
	var out []Block
	for _, b := range blocks {
		if b.Type == BlockToolUse {
			out = append(out, b)
		}
	}
	return out
}

func jsonSize(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b)
}

// validJSON returns raw unchanged when it is valid JSON, and as a JSON string
// otherwise. A response cut off at max_tokens can end in a tool input that is
// not complete, and the ledger stores only valid JSON.
func validJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || json.Valid(raw) {
		return raw
	}
	b, _ := json.Marshal(string(raw))
	return b
}

// validJSONBlocks returns a copy of the blocks with validJSON applied to
// every raw field.
func validJSONBlocks(blocks []Block) []Block {
	if blocks == nil {
		return nil
	}
	out := make([]Block, len(blocks))
	for i, b := range blocks {
		b.Input, b.Raw = validJSON(b.Input), validJSON(b.Raw)
		b.Content = validJSONBlocks(b.Content)
		out[i] = b
	}
	return out
}
