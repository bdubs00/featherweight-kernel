package fw

// This file is the turn loop. A driver goroutine owns a run. At each step it
// reads the log of the run, finds the next action, and does it. Because the
// next action comes from the log alone, the same loop starts a new run,
// continues after a decision or a follow-up, and resumes after a restart.

import (
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
	compactSeq uint64    // the latest fw.context.compacted
	toolCalls  int       // fw.tool.requested events in the run
	prints     []uint64  // fingerprint of each tool_use block of every response, in order
	priorN     int       // prints before the latest response
	warned     bool      // the kernel warned about a loop after the latest response
	respAt     time.Time // the time of the latest response
	noteAt     time.Time // the time of the latest time note
	zone       string    // the zone that fw.run.started recorded
}

// analyze builds the cursor. It decodes only the events that it needs.
func analyze(events []Event) *cursor {
	c := &cursor{}
	lastResp, prevResp := -1, -1
	for i, e := range events {
		if e.Type == EvModelResponded {
			prevResp, lastResp = lastResp, i
		}
	}
	approvals := map[string]ApprovalRequestedData{}
	var open []string
	for i, e := range events {
		c.lastSeq = e.Seq
		switch e.Type {
		case EvRunQueued:
			_ = e.Decode(&c.queued)
		case EvRunStarted:
			c.started = true
			var d RunStartedData
			if e.Decode(&d) == nil {
				c.brief, c.zone = d.Brief, d.TimeZone
			}
		case EvContextCompacted:
			c.compactSeq = e.Seq
		case EvMessageReceived:
			c.terminal = false
			c.unseen = true
			var d MessageReceivedData
			if e.Decode(&d) != nil || d.Origin != OriginKernel || d.Role != RoleSystem {
				break
			}
			switch {
			case d.Note == NoteTime:
				c.noteAt = e.Time
			case i > lastResp:
				// Every other system message from the kernel is a loop
				// warning. A ledger from before the time notes has no
				// note kind on it.
				c.warned = true
			}
		case EvTurnStarted:
			c.turns++
			c.turnOpen = true
			c.turnSeq = e.Seq
		case EvTurnFinished:
			c.turnOpen = false
		case EvModelRequested:
			c.unseen = false
		case EvModelResponded:
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
			open = nil
		}
	}
	if len(open) > 0 {
		d := approvals[open[len(open)-1]]
		c.pending = &d
	}
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
		evs := rs.eventLog()
		c := analyze(evs)
		var err error
		switch {
		case c.terminal || c.pending != nil:
			if a.idleExit(rs, c.lastSeq) {
				return exitIdle
			}
			continue
		case !c.started:
			err = a.stepStart(ctx, rs, c)
		case c.turnOpen && c.respSeq < c.turnSeq:
			// The turn has no response yet: call the model, or call it
			// again after a restart.
			err = a.stepModel(ctx, rs, c)
		case c.turnOpen:
			err = a.stepResponse(ctx, rs, c, len(evs))
		case c.resp != nil && !c.unseen && settles(c):
			// The turn closed, but the run did not record its end.
			err = a.stepResponse(ctx, rs, c, len(evs))
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
	_, err := a.emit(ctx, rs, EvRunStarted, RunStartedData{Model: a.selectorOf(c.queued, prof), Tools: names, Brief: a.brief(ctx, rs, tools), TimeZone: zone})
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

// stepModel checks the budget, compacts the history when it is too long,
// and calls the model once.
func (a *Agent) stepModel(ctx context.Context, rs *runState, c *cursor) error {
	prof := a.profileOf(c.queued)
	if limit, value, over := a.overBudget(rs, c); over {
		if _, err := a.emit(ctx, rs, EvBudgetExceeded, BudgetExceededData{Limit: limit, Value: value}); err != nil {
			return err
		}
		return a.finish(ctx, rs, OutcomeFailed, "", "budget exceeded: "+limit)
	}
	selector := a.selectorOf(c.queued, prof)
	model, err := a.openModel(ctx, selector)
	if err != nil {
		return a.fail(ctx, rs, "model: "+err.Error())
	}
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
			return err
		}
		prof.System, prof.Instructions = text.system, text.instructions
	}
	provider, name := SplitSelector(selector)
	system := a.SystemPrompt(prof, c.queued.Instructions)
	if c.brief != "" {
		system += "\n\n" + c.brief
	}
	req := &Request{
		System:    system,
		Tools:     specs,
		Effort:    effort,
		EffortSet: c.queued.Resolved != nil,
		MaxTokens: defaultMaxTokens,
		Metadata:  map[string]string{"runId": rs.id, "agent": rs.agent},
	}
	extra := (len(req.System) + jsonSize(specs)) / bytesPerToken
	if !c.turnOpen {
		if err := a.maybeCompact(ctx, rs, model, req, extra); err != nil {
			return err
		}
	}

	note := a.timeNote(ctx, rs, c)

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
	h, tokens := rs.historyLocked(extra)
	req.Messages = a.readFiles(h.Messages, MediaTypesOf(model))
	_, err = a.emitLocked(ctx, rs, EvModelRequested, ModelRequestedData{
		Provider:             provider,
		Model:                name,
		Effort:               effort,
		EstimatedInputTokens: tokens,
	})
	rs.emitMu.Unlock()
	if err != nil {
		return err
	}

	start := time.Now()
	resp, err := model.Stream(ctx, req, func(Delta) {})
	latency := time.Since(start).Milliseconds()
	if err != nil {
		if ctx.Err() != nil {
			return errStopped
		}
		// A request that does not fit the context window gets one
		// compaction per turn, and the next step calls the model again.
		retried := c.turnOpen && c.compactSeq > c.turnSeq
		if IsContextOverflow(err) && a.k.compactAt > 0 && !retried {
			msg := "model: " + err.Error()
			if _, err := a.emit(ctx, rs, EvError, ErrorData{Message: msg + " (compacting the context)", Retryable: true}); err != nil {
				return err
			}
			ok, err := a.compact(ctx, rs, model, req)
			if err != nil || ok {
				return err
			}
			return a.finish(ctx, rs, OutcomeFailed, "", msg)
		}
		return a.fail(ctx, rs, "model: "+err.Error())
	}
	if resp == nil {
		return a.fail(ctx, rs, "model: no response")
	}
	if resp.Provider != "" {
		provider = resp.Provider
	}
	if resp.Model != "" {
		name = resp.Model
	}
	_, err = a.emit(ctx, rs, EvModelResponded, ModelRespondedData{
		Content:    validJSONBlocks(resp.Content),
		Raw:        validJSON(resp.Raw),
		StopReason: resp.StopReason,
		StopDetail: resp.StopDetail,
		Usage:      resp.Usage,
		LatencyMS:  latency,
		Provider:   provider,
		Model:      name,
	})
	return err
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
			return a.finish(ctx, rs, OutcomeFailed, TextOf(c.resp.Content), "max_tokens")
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
		return a.finish(ctx, rs, OutcomeFailed, "", detail)
	}
	return a.endTurn(ctx, rs, c, n)
}

// endTurn closes the turn and finishes the run, unless a message arrived
// that the model has not seen. Then the next step starts a new turn. The
// check and the events are one step, so no message is lost.
func (a *Agent) endTurn(ctx context.Context, rs *runState, c *cursor, n int) error {
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
	_, err := a.emitLocked(ctx, rs, EvRunFinished, RunFinishedData{Outcome: OutcomeCompleted, Output: TextOf(c.resp.Content)})
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
	_, err := a.emit(ctx, rs, EvRunFinished, RunFinishedData{Outcome: outcome, Output: output, Error: msg})
	return err
}

// fail records an error and finishes the run as failed.
func (a *Agent) fail(ctx context.Context, rs *runState, msg string) error {
	if _, err := a.emit(ctx, rs, EvError, ErrorData{Message: msg}); err != nil {
		return err
	}
	return a.finish(ctx, rs, OutcomeFailed, "", msg)
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
// outgrows its parent.
func (a *Agent) budgetOf(rs *runState) Budget {
	b := a.ownBudget(rs)
	if rs.parent != "" {
		if p, err := a.k.lookup(rs.parent); err == nil && rs.depth < 64 {
			b = b.Cap(a.budgetOf(p))
		}
	}
	return b
}

// overBudget compares the usage of the run with its effective budget.
func (a *Agent) overBudget(rs *runState, c *cursor) (string, float64, bool) {
	b := a.budgetOf(rs)
	rs.mu.Lock()
	u := rs.run.Usage
	rs.mu.Unlock()
	next := c.turns
	if !c.turnOpen {
		next++
	}
	tokens := u.InputTokens + u.OutputTokens
	switch {
	case b.MaxTurns > 0 && next > b.MaxTurns:
		return "maxTurns", float64(c.turns), true
	case b.MaxTokens > 0 && tokens >= b.MaxTokens:
		return "maxTokens", float64(tokens), true
	case b.MaxCostUSD > 0 && u.CostUSD >= b.MaxCostUSD:
		return "maxCostUSD", u.CostUSD, true
	case b.MaxToolCalls > 0 && c.toolCalls >= b.MaxToolCalls:
		return "maxToolCalls", float64(c.toolCalls), true
	}
	if worked, over := a.pastDeadline(rs); over {
		return "deadline", worked.Seconds(), true
	}
	return "", 0, false
}

// pastDeadline reports whether the run or one of its ancestors used up the
// working time of its deadline, and returns the working time of that run.
// The deadline counts working time (see WorkingTime), so a run that waited
// for a person or for a follow-up still has its time.
func (a *Agent) pastDeadline(rs *runState) (time.Duration, bool) {
	now := a.now()
	for _, r := range a.lineage(rs) {
		b := a.ownBudget(r)
		if b.Deadline <= 0 {
			continue
		}
		evs, err := a.eventsOf(context.Background(), r)
		if err != nil {
			a.log.Warn("read run for its deadline", "run", r.id, "error", err)
			continue
		}
		if worked := WorkingTime(evs, now); worked >= b.Deadline {
			return worked, true
		}
	}
	return 0, false
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
	pctx := WithRunInfo(ctx, RunInfo{RunID: rs.id, Agent: rs.agent, CallID: u.ID})
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
}

// stepTools decides every open call of the latest response, asks for an
// approval when the policy says so, runs the allowed calls, and records one
// result per call in the order of the tool_use blocks. Before that, it
// applies the loop guard and the per-turn and per-run call limits.
func (a *Agent) stepTools(ctx context.Context, rs *runState, c *cursor) error {
	prof := a.profileOf(c.queued)
	b := a.budgetOf(rs)
	uses := toolUses(c.resp.Content)

	period, count := repeats(c.prints)
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
		return a.finish(ctx, rs, OutcomeFailed, TextOf(c.resp.Content), fmt.Sprintf("budget exceeded: maxRepeats (the model made the same %s call %d times in a row)", describeLoop(tool, period), count))
	}

	_, byName := a.runTools(ctx, rs, prof)
	policy := a.policyFor(rs, prof)
	room := -1 // tool calls left in the run, or -1 for no limit
	if b.MaxToolCalls > 0 {
		room = max(b.MaxToolCalls-c.toolCalls, 0)
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
			if !p.spec.Idempotent {
				answered, ok := continueAsk(p.tool, rec)
				if !ok {
					return a.requestApproval(ctx, rs, u, "rerun after resume")
				}
				p.answered = answered
			}
			p.run = true
		default:
			pctx := WithRunInfo(ctx, RunInfo{RunID: rs.id, Agent: rs.agent, CallID: u.ID})
			call := ResolveCall(pctx, t, ToolCall{RunID: rs.id, Agent: rs.agent, CallID: u.ID, Spec: p.spec, Input: u.Input})
			d := policy.Decide(pctx, call)
			switch d.Verdict {
			case VerdictAllow:
				p.run = true
			case VerdictAsk:
				return a.requestApproval(ctx, rs, u, d.Reason)
			default:
				p.result = ErrorResult("denied by policy: " + d.Reason)
			}
		}
		if p.run && room > 0 {
			room--
		}
		plan = append(plan, p)
	}
	if err := a.execute(ctx, rs, plan); err != nil {
		return err
	}
	// Warn once, on the response that brings the loop one repeat short of
	// the limit.
	if _, before := repeats(c.prints[:c.priorN]); count >= 2 && count == b.MaxRepeats-1 && before < count && !c.warned {
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
		out = append(out, AnsweredQuestion{Request: q.req, Decision: *q.decided})
	}
	if _, ok := continuerOf(t); !ok {
		return nil, false
	}
	return out, true
}

// requestApproval appends fw.approval.requested for a call. The run moves to
// input-required, and the driver goes idle until Decide.
func (a *Agent) requestApproval(ctx context.Context, rs *runState, u Block, reason string) error {
	_, err := a.emit(ctx, rs, EvApprovalRequested, ApprovalRequestedData{
		ApprovalID: NewID(),
		Tool:       u.Name,
		CallID:     u.ID,
		Input:      u.Input,
		Reason:     reason,
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
	outs := make([]callOutcome, len(batch))
	var aborted atomic.Bool
	var wg sync.WaitGroup
	for i := range batch {
		p := batch[i]
		if !p.run {
			outs[i] = callOutcome{res: p.result}
			continue
		}
		if _, err := a.emit(ctx, rs, EvToolRequested, ToolRequestedData{Tool: p.use.Name, CallID: p.use.ID, Input: p.use.Input, Idempotent: p.spec.Idempotent}); err != nil {
			wg.Wait()
			return err
		}
		if len(batch) == 1 {
			outs[i] = a.invoke(ctx, rs, p, &aborted)
			continue
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
		res := outs[i].res
		content := res.Content
		if len(content) == 0 && len(res.Structured) > 0 {
			content = []Block{Text(string(res.Structured))}
		}
		if _, err := a.emit(ctx, rs, EvToolResponded, ToolRespondedData{
			CallID:    p.use.ID,
			Tool:      p.use.Name,
			Content:   validJSONBlocks(content),
			IsError:   res.IsError,
			LatencyMS: outs[i].ms,
		}); err != nil {
			return err
		}
	}
	return nil
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
	if a.files != nil {
		tool := p.use.Name
		tctx = withFiles(tctx, a.files.bind(func(d FileChangedData) {
			d.CallID, d.Tool = callID, tool
			_, _ = a.emit(ctx, rs, EvFileChanged, d)
		}))
	}
	res, err = p.tool.Call(tctx, input)
	return res, err
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
	return RunInfo{RunID: rs.id, Agent: rs.agent, Principal: q.Principal, PrincipalKind: q.PrincipalKind}
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
