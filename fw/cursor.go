package fw

import (
	"maps"
	"time"
)

// loopSuffix counts the matching suffix for each possible period. A suffix
// of k matches at distance p contains 1+k/p copies of that period. Keeping
// these counts, rather than the calls, preserves even an unlimited repeat
// count when a budget changes. Ties favor the shortest period, as repeats does.
type loopSuffix struct {
	tail    [maxLoopPeriod]uint64
	matched [maxLoopPeriod]int
	n       int
}

func (s *loopSuffix) add(print uint64) {
	for p := 1; p <= maxLoopPeriod; p++ {
		if s.n >= p && s.tail[(s.n-p)%maxLoopPeriod] == print {
			s.matched[p-1]++
		} else {
			s.matched[p-1] = 0
		}
	}
	s.tail[s.n%maxLoopPeriod] = print
	s.n++
}

func (s loopSuffix) repeats() (period, count int) {
	period, count = 1, 1
	for p := 1; p <= maxLoopPeriod && p <= s.n; p++ {
		if k := 1 + s.matched[p-1]/p; k > count {
			period, count = p, k
		}
	}
	return period, count
}

// cursorBuilder consumes append-only ledger events once. Decoded payloads
// are immutable. Only the builder mutates call records and the report state.
// Approvals retain small lookup entries for late decisions, including an
// older approval that names a call ID used again by the latest response.
// A decided entry releases its payload after its last open occurrence.
type cursorBuilder struct {
	c         cursor
	approvals map[string]cursorApproval
	open      []string
	reporting bool
	prevMax   bool
	reportAt  time.Time
}

type cursorApproval struct {
	callID   string
	question bool
	connect  bool
	req      *ApprovalRequestedData // only while this ID is open
}

// executionCursor returns a cursor and its event boundary atomically. A
// concurrent append only extends events; no event or Data bytes in the held
// log are rewritten. The returned cursor owns its mutable records, so another
// reader can advance the cache while the driver acts on this snapshot.
func (rs *runState) executionCursor() (*cursor, int) {
	rs.emitMu.Lock()
	defer rs.emitMu.Unlock()
	if rs.exec == nil {
		rs.exec, rs.execN = &cursorBuilder{}, 0
	}
	for _, e := range rs.events[rs.execN:] {
		rs.exec.apply(e)
	}
	rs.execN = len(rs.events)
	return rs.exec.snapshot(), rs.execN
}

func (b *cursorBuilder) snapshot() *cursor {
	c := b.c
	c.priorCount = max(1, c.priorCount)
	if c.calls != nil {
		c.calls = make(map[string]*callRecord, len(b.c.calls))
		for id, r := range b.c.calls {
			cp := *r
			if r.questions != nil {
				cp.questions = make([]*questionRecord, len(r.questions))
				for i, q := range r.questions {
					v := *q
					cp.questions[i] = &v
				}
			}
			c.calls[id] = &cp
		}
	}
	if c.rep != nil {
		r := *c.rep
		c.rep = &r
	}
	// noted is immutable between budget notes; apply uses copy-on-write.
	return &c
}

func (b *cursorBuilder) apply(e Event) {
	c := &b.c
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
		c.terminal, c.unseen = false, true
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
			if d.Budget != nil && !d.Budget.Changed {
				c.noted = maps.Clone(c.noted)
			}
			c.budgetNote(e.Seq, d.Budget)
		case NoteReport:
			if c.rep != nil {
				c.rep.noteSeq = e.Seq
			}
		default:
			c.warned = true
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
			c.reportDone(e.Seq, "")
		}
	case EvTurnStarted:
		c.turns++
		if e.Seq <= c.queued.Kept {
			c.keptTurns++
		}
		c.keptOpen = e.Seq <= c.queued.Kept
		c.turnOpen, c.turnSeq = true, e.Seq
	case EvTurnFinished:
		c.turnOpen, c.keptOpen = false, false
	case EvModelRequested:
		var d struct {
			Report bool `json:"report"`
		}
		b.reporting = e.Decode(&d) == nil && d.Report
		c.unseen = false
		if b.reporting && c.rep != nil {
			c.rep.reqSeq = e.Seq
		}
	case EvModelResponded:
		c.answered = true
		if b.reporting {
			var d struct {
				Content []Block `json:"content"`
			}
			_ = e.Decode(&d)
			c.respAt, b.reportAt = e.Time, e.Time
			c.reportDone(e.Seq, TextOf(d.Content))
			break
		}
		if c.keptOpen && e.Seq > c.queued.Kept {
			c.keptTurns--
		}
		c.keptOpen, c.warned = false, false
		c.resp, c.respSeq, c.calls = nil, 0, nil
		c.respAt, c.maxTokens, c.priorCount = b.reportAt, 0, 1
		var d ModelRespondedData
		previousMax := b.prevMax
		if e.Decode(&d) != nil {
			var stop struct {
				StopReason StopReason `json:"stopReason"`
			}
			b.prevMax = e.Decode(&stop) == nil && stop.StopReason == StopMaxTokens
			break
		}
		b.prevMax = d.StopReason == StopMaxTokens
		_, c.priorCount = c.loop.repeats()
		for _, block := range d.Content {
			if block.Type == BlockToolUse {
				c.loop.add(fingerprint(block))
			}
		}
		c.resp, c.respSeq, c.respAt = &d, e.Seq, e.Time
		c.calls = map[string]*callRecord{}
		if d.StopReason == StopToolUse {
			for _, block := range d.Content {
				if block.Type == BlockToolUse {
					c.calls[block.ID] = &callRecord{}
				}
			}
		}
		if d.StopReason == StopMaxTokens {
			c.maxTokens = 1
			if previousMax {
				c.maxTokens = 2
			}
		}
	case EvToolRequested, EvToolResponded:
		if e.Type == EvToolRequested && !doneCall(e) {
			c.toolCalls++
			if e.Seq <= c.queued.Kept {
				c.keptCalls++
			}
		}
		if c.calls == nil {
			break
		}
		var d struct {
			CallID string `json:"callId"`
		}
		if e.Decode(&d) == nil {
			if r := c.calls[d.CallID]; r != nil {
				if e.Type == EvToolRequested {
					r.requestedSeq = e.Seq
				} else {
					r.responded = true
				}
			}
		}
	case EvApprovalRequested:
		var d ApprovalRequestedData
		if e.Decode(&d) != nil {
			break
		}
		if b.approvals == nil {
			b.approvals = map[string]cursorApproval{}
		}
		b.approvals[d.ApprovalID] = cursorApproval{callID: d.CallID, question: len(d.Schema) > 0, connect: d.Connect != nil, req: &d}
		b.open = append(b.open, d.ApprovalID)
		if r := c.calls[d.CallID]; r != nil {
			if d.Connect != nil {
				r.connectSeq, r.connect, r.connectDeny = e.Seq, d.Connect, nil
			} else if len(d.Schema) > 0 {
				r.questions = append(r.questions, &questionRecord{req: d})
			}
		}
		b.pending()
	case EvApprovalDecided:
		var d ApprovalDecidedData
		if e.Decode(&d) != nil {
			break
		}
		for i, id := range b.open {
			if id == d.ApprovalID {
				b.open = append(b.open[:i], b.open[i+1:]...)
				break
			}
		}
		req, ok := b.approvals[d.ApprovalID]
		if r := c.calls[req.callID]; ok && r != nil {
			switch {
			case req.question && !req.connect:
				for _, q := range r.questions {
					if q.req.ApprovalID == d.ApprovalID {
						q.decided = &d
					}
				}
			case !req.question && !req.connect:
				r.decision, r.decisionSeq = &d, e.Seq
			case !req.question && d.Decision != "allow":
				r.connectDeny = &d
			}
		}
		if ok {
			stillOpen := false
			for _, id := range b.open {
				if id == d.ApprovalID {
					stillOpen = true
					break
				}
			}
			if !stillOpen {
				req.req = nil
				b.approvals[d.ApprovalID] = req
			}
		}
		b.pending()
	case EvRunFinished:
		c.terminal, c.turnOpen, c.rep = true, false, nil
		for _, id := range b.open {
			req := b.approvals[id]
			req.req = nil
			b.approvals[id] = req
		}
		b.open, c.pending = nil, nil
	}
}

func (b *cursorBuilder) pending() {
	b.c.pending = nil
	if n := len(b.open); n > 0 {
		b.c.pending = b.approvals[b.open[n-1]].req
	}
}
