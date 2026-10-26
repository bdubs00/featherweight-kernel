package fw

// This file holds the pure projections of a run: the message history that
// the model sees, and the status object that every surface returns. Both are
// functions of the ledger alone.

import (
	"maps"
	"strings"
)

// ProjectHistory builds the message history that the next model call sends.
// It starts at the latest fw.context.compacted event, with a user message
// that holds the summary block, followed by the messages that the compactor
// kept.
//
// The input of fw.run.queued is the first user message. Each
// fw.message.received with role user or system adds a message. Each
// fw.model.responded adds the assistant message with its full content,
// opaque blocks included and in order. The fw.tool.responded events of one
// response become one user message of tool_result blocks, in the order of
// the tool_use blocks. A message that arrives while a model call is open
// follows the answer of that call, and a message that arrives while tool
// calls are open follows the tool results, so the next request ends with
// the message. A call that never answers leaves its messages for the next
// request, at the end. A tool_use block that never runs gets an error
// result, so every request has a valid history: this covers a run that
// finished with open calls and a response that stopped for a reason other
// than tool_use.
func ProjectHistory(events []Event) History {
	start := 0
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == EvContextCompacted {
			start = i
			break
		}
	}
	var b historyBuilder
	for _, e := range events[start:] {
		b.apply(e)
	}
	return b.history()
}

// historyBuilder folds events into a history one at a time. The kernel keeps
// one per active run, so each event is decoded once.
type historyBuilder struct {
	epoch    int
	msgs     []Message
	tokens   int              // estimated tokens of msgs
	known    int              // tokens of the latest response of the epoch and its request, from the reported usage
	after    int              // estimated tokens of the messages after that response
	asked    bool             // a model call has no answer yet
	open     []string         // tool_use ids that wait for a result, in order
	results  map[string]Block // results of open ids
	deferred []Message        // messages that arrived while calls were open
}

func (b *historyBuilder) add(m Message) {
	b.msgs = append(b.msgs, m)
	n := estimateTokens(m.Content)
	b.tokens += n
	b.after += n
}

// contextTokens estimates the tokens of a request that sends this history.
// After a response with a reported usage, the estimate is that usage plus
// the messages after the response. Otherwise it is the estimate of the
// history plus extra, the estimate of the system prompt and the tools.
func (b *historyBuilder) contextTokens(extra int) int {
	waiting := 0
	if len(b.open) == 0 {
		for _, m := range b.deferred {
			waiting += estimateTokens(m.Content)
		}
	}
	if b.known > 0 {
		return b.known + b.after + waiting
	}
	return b.tokens + extra + waiting
}

func (b *historyBuilder) apply(e Event) {
	switch e.Type {
	case EvRunQueued:
		var d RunQueuedData
		if e.Decode(&d) != nil {
			return
		}
		var content []Block
		if d.Input != "" {
			content = append(content, Text(d.Input))
		}
		content = append(content, d.Files...)
		if len(content) > 0 {
			b.add(Message{Role: RoleUser, Content: content})
		}
	case EvMessageReceived:
		m, ok := receivedMessage(e)
		if !ok {
			return
		}
		if b.asked || len(b.open) > 0 {
			b.deferred = append(b.deferred, m)
			return
		}
		b.add(m)
	case EvModelRequested:
		// A call that never answered left its messages waiting, and this
		// request sent them last.
		b.asked = false
		b.release()
		b.asked = true
	case EvModelResponded:
		var d ModelRespondedData
		if e.Decode(&d) != nil {
			return
		}
		b.abandonCalls("no result was recorded for this tool call")
		b.asked = false
		if len(d.Content) > 0 {
			b.add(Message{Role: RoleAssistant, Content: d.Content})
		}
		if u := d.Usage; u.InputTokens+u.CacheReadTokens+u.CacheWriteTokens > 0 {
			b.known = u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens + u.OutputTokens
			b.after = 0
		}
		uses := toolUses(d.Content)
		if len(uses) == 0 {
			b.release()
			return
		}
		for _, u := range uses {
			b.open = append(b.open, u.ID)
		}
		b.results = map[string]Block{}
		if d.StopReason != StopToolUse {
			// A response cut off at max_tokens can end in a tool_use block.
			// The kernel runs no call of such a response.
			b.abandonCalls("this tool call did not run, because the response stopped with " + string(d.StopReason))
		}
	case EvToolResponded:
		var d ToolRespondedData
		if e.Decode(&d) != nil || !b.isOpen(d.CallID) {
			return
		}
		b.results[d.CallID] = Block{Type: BlockToolResult, ToolUseID: d.CallID, Content: d.Content, IsError: d.IsError}
		if len(b.results) == len(b.open) {
			b.closeCalls()
		}
	case EvRunFinished:
		b.abandonCalls("the run ended before this tool call finished")
		b.asked = false
		b.release()
	case EvContextCompacted:
		var d ContextCompactedData
		if e.Decode(&d) != nil {
			return
		}
		*b = historyBuilder{epoch: d.Epoch}
		b.add(Message{Role: RoleUser, Content: []Block{d.Summary}})
		for _, m := range d.Kept {
			b.add(m)
		}
	}
}

func (b *historyBuilder) isOpen(id string) bool {
	for _, x := range b.open {
		if x == id {
			return true
		}
	}
	return false
}

// closeCalls adds the tool results as one user message, then the messages
// that waited for them.
func (b *historyBuilder) closeCalls() {
	blocks := make([]Block, 0, len(b.open))
	for _, id := range b.open {
		blocks = append(blocks, b.results[id])
	}
	b.open, b.results = nil, nil
	b.add(Message{Role: RoleUser, Content: blocks})
	b.release()
}

// release adds the messages that waited, once no model call and no tool
// call is open.
func (b *historyBuilder) release() {
	if b.asked || len(b.open) > 0 {
		return
	}
	for _, m := range b.deferred {
		b.add(m)
	}
	b.deferred = nil
}

// abandonCalls gives every open call without a result an error result.
func (b *historyBuilder) abandonCalls(reason string) {
	if len(b.open) == 0 {
		return
	}
	for _, id := range b.open {
		if _, ok := b.results[id]; !ok {
			b.results[id] = Block{Type: BlockToolResult, ToolUseID: id, Content: []Block{Text(reason)}, IsError: true}
		}
	}
	b.closeCalls()
}

// history returns the messages of the next request. After a model call
// that has no answer, they end with the messages that wait for it.
func (b *historyBuilder) history() History {
	msgs := append([]Message(nil), b.msgs...)
	if len(b.open) == 0 {
		msgs = append(msgs, b.deferred...)
	}
	return History{Epoch: b.epoch, Messages: msgs}
}

// receivedMessage returns the message of a fw.message.received event. Only
// the roles user and system enter the history. A message from an agent
// (MessageReceivedData.From) is a user message whose content sits inside
// a block that names the agent (MessageBlock), so the model reads it as
// data with the authority of a tool result. A message of a person reaches
// the model as it came.
func receivedMessage(e Event) (Message, bool) {
	var d MessageReceivedData
	if e.Decode(&d) != nil || len(d.Content) == 0 {
		return Message{}, false
	}
	if d.Role == "" {
		d.Role = RoleUser
	}
	if d.Role != RoleUser && d.Role != RoleSystem {
		return Message{}, false
	}
	if d.fromAgent() {
		return Message{Role: RoleUser, Content: agentContent(*d.From, d.Content)}, true
	}
	return Message{Role: d.Role, Content: d.Content}, true
}

// ProjectRun builds the status object of a run from its events. The prices
// argument is optional. When it knows the model of a response, the cost of
// that response enters the usage. Otherwise the tokens of the response
// enter UnpricedTokens.
func ProjectRun(events []Event, prices ...Prices) *Run {
	var p Prices
	if len(prices) > 0 {
		p = prices[0]
	}
	r := &Run{}
	for _, e := range events {
		r.Apply(e, p)
	}
	return r
}

// Apply folds one event into the run status. A nil prices adds no cost,
// so every token is unpriced.
//
// The usage grows with each fw.model.responded, fw.run.annotated, and
// fw.context.compacted, and with each fw.turn.started and
// fw.tool.requested. A fw.run.finished or fw.run.suspended with totals
// sets the usage to the totals instead, because the kernel that appended
// the event knew every earlier event of the run: a consumer that saw the
// increments counts nothing twice, and a consumer that missed some, such
// as the front of a fleet that receives only the lifecycle events,
// catches up. The cost then is the cost at the prices of that kernel; the
// prices argument applies only to the increments after the totals. An
// event of an older kernel has no totals and changes the usage in no way.
//
// A branch (RunQueuedData.Kept) counts its own work only. The events that
// it kept from its source add no usage and set no totals, because the
// source holds that usage. A turn that is open at the end of the kept
// events becomes a turn of the branch when the branch makes its model
// call. A kept event after which the branch is finished, such as the
// copied end of the run that a branch of a finished run without a message
// keeps, gives the branch a known usage of zero (UsageSeq), as totals
// would, because the branch did no work up to it. A consumer that receives
// only some of the events, such as the lifecycle events, learns it there.
//
// A fw.run.queued with a lower sequence number than an event that Apply
// already folded, which a bus can deliver late, gives the run its
// identity and changes nothing else: the state, the usage, and the time
// of the latest change stay, and the metadata and the title of later
// annotations win. A run that no event gave a state yet becomes queued.
func (r *Run) Apply(e Event, prices Prices) {
	if r.ID == "" {
		r.ID = e.Subject
	}
	late := e.Seq != 0 && e.Seq < r.Seq
	if r.keptEvent(e) {
		defer r.endKept(e.Seq)
	}
	if e.Seq > r.Seq {
		r.Seq = e.Seq
	}
	if !e.Time.IsZero() && !late {
		r.UpdatedAt = e.Time
	}
	if !late {
		r.applyActivity(e)
	}
	switch e.Type {
	case EvRunQueued:
		var d RunQueuedData
		if e.Decode(&d) != nil {
			return
		}
		r.Agent, r.Input, r.Origin = d.Agent, d.Input, d.Origin
		r.Principal, r.Parent = d.Principal, d.Parent
		r.Depth, r.ExtraTools = d.Depth, d.ExtraTools
		r.kept = d.Kept
		r.CreatedAt = e.Time
		title := strings.TrimSpace(d.Metadata[MetaTitle])
		if title == "" {
			title = TitleFrom(d.Input)
		}
		if late {
			if len(d.Metadata) > 0 {
				md := maps.Clone(d.Metadata)
				maps.Copy(md, r.Metadata)
				r.Metadata = md
			}
			if r.Title == "" {
				r.Title = title
			}
			if r.State == "" {
				r.State = StateQueued
			}
			return
		}
		r.Metadata, r.Title = d.Metadata, title
		r.State = StateQueued
	case EvRunStarted, EvRunResumed, EvTurnStarted:
		if e.Type == EvTurnStarted {
			r.keptTurn = r.keptEvent(e)
			if !r.keptTurn {
				r.Usage.Turns++
			}
		}
		if r.State.Terminal() {
			return
		}
		if r.Pending != nil {
			r.State = StateInputRequired
		} else {
			r.State = StateRunning
		}
	case EvMessageReceived:
		var pin struct {
			Pinned bool `json:"pinned"`
		}
		if e.Decode(&pin) == nil && pin.Pinned {
			var d MessageReceivedData
			if e.Decode(&d) == nil {
				r.pinnedBytes += contentBytes(d.Content)
			}
		}
		if r.Pending != nil {
			return
		}
		if r.State.Terminal() {
			r.Output, r.Error, r.Outcome = "", "", nil
		}
		r.State = StateRunning
	case EvModelResponded:
		var d struct {
			Usage    Usage  `json:"usage"`
			Provider string `json:"provider"`
			Model    string `json:"model"`
		}
		// The model call of a turn that the branch kept open makes the turn
		// its own; a kept call leaves the turn to the source.
		open := r.keptTurn
		r.keptTurn = false
		if r.keptEvent(e) || e.Decode(&d) != nil {
			return
		}
		if open {
			r.Usage.Turns++
		}
		r.addUsage(d.Usage, d.Provider, d.Model, prices)
	case EvTurnFinished:
		r.keptTurn = false
	case EvOutcomeEvaluated:
		r.applyOutcome(e)
	case EvRunAnnotated:
		var d RunAnnotatedData
		if e.Decode(&d) != nil {
			return
		}
		if len(d.Metadata) > 0 {
			r.Metadata = maps.Clone(r.Metadata)
			if r.Metadata == nil {
				r.Metadata = map[string]string{}
			}
			maps.Copy(r.Metadata, d.Metadata)
			if t := strings.TrimSpace(r.Metadata[MetaTitle]); t != "" {
				r.Title = t
			}
		}
		if d.Usage != nil && !r.keptEvent(e) {
			r.addUsage(*d.Usage, d.Provider, d.Model, prices)
		}
	case EvContextCompacted:
		var d ContextCompactedData
		if r.keptEvent(e) || e.Decode(&d) != nil || d.Usage == nil {
			return
		}
		r.addUsage(*d.Usage, d.Provider, d.Model, prices)
	case EvRunReacted:
		// A reaction changes no state. The count is a new map, so a copy
		// of the run that shares the old map keeps its count.
		var d ReactionData
		if r.keptEvent(e) || e.Decode(&d) != nil || d.Reaction == "" {
			return
		}
		counts := make(map[string]int, len(r.Reactions)+1)
		maps.Copy(counts, r.Reactions)
		switch {
		case !d.Removed:
			counts[d.Reaction]++
		case counts[d.Reaction] > 1:
			counts[d.Reaction]--
		default:
			delete(counts, d.Reaction)
		}
		if len(counts) == 0 {
			counts = nil
		}
		r.Reactions = counts
	case EvBudgetChanged:
		// A branch keeps the changes that it copied, because it goes on
		// with the budget that its source had at that point.
		var d BudgetChangedData
		if e.Decode(&d) == nil {
			r.budget = d.Budget.Merge(r.budget)
		}
	case EvToolRequested:
		if !r.keptEvent(e) {
			r.Usage.ToolCalls++
		}
	case EvApprovalRequested:
		var d ApprovalRequestedData
		if e.Decode(&d) != nil {
			return
		}
		r.Pending = &Approval{
			ID:           d.ApprovalID,
			Tool:         d.Tool,
			CallID:       d.CallID,
			Input:        d.Input,
			Reason:       d.Reason,
			Schema:       d.Schema,
			RequestState: d.RequestState,
			RequestedAt:  e.Time,
			Connect:      d.Connect,
		}
		r.State = StateInputRequired
	case EvApprovalDecided:
		var d struct {
			ApprovalID string `json:"approvalId"`
		}
		if e.Decode(&d) != nil || r.Pending == nil || r.Pending.ID != d.ApprovalID {
			return
		}
		r.Pending = nil
		if !r.State.Terminal() {
			r.State = StateRunning
		}
	case EvRunSuspended:
		var d RunSuspendedData
		if e.Decode(&d) == nil {
			r.setTotals(d.Totals, e)
		}
		r.State = StateSuspended
	case EvRunFinished:
		var d RunFinishedData
		if e.Decode(&d) != nil {
			return
		}
		r.setTotals(d.Totals, e)
		r.Pending = nil
		switch d.Outcome {
		case OutcomeCompleted:
			r.State = StateCompleted
		case OutcomeCanceled:
			r.State = StateCanceled
		default:
			r.State = StateFailed
		}
		r.Output, r.Error = d.Output, d.Error
	}
}

// applyActivity follows what the run does now (Run.Activity), before the
// event changes the state. fw.turn.started sets the turn and clears the
// tool. fw.tool.requested sets the tool and the call id. fw.tool.responded
// of that call clears the tool. fw.turn.finished and fw.run.finished clear
// the activity and close the turn. fw.approval.requested and
// fw.run.suspended clear the activity and keep it aside; a decision, or a
// resume of a run that does not wait for one, brings it back while the
// turn is open, so a call that asked a question goes on with its tool.
// Each change sets a new value, so a copy of the run keeps its own.
func (r *Run) applyActivity(e Event) {
	switch e.Type {
	case EvTurnStarted:
		var d TurnData
		if e.Decode(&d) != nil {
			return
		}
		r.turn, r.paused = d.Turn, nil
		r.Activity = &RunActivity{Turn: d.Turn, Since: e.Time}
	case EvToolRequested:
		var d struct {
			Tool   string `json:"tool"`
			CallID string `json:"callId"`
		}
		if e.Decode(&d) != nil || r.State.Terminal() {
			return
		}
		r.Activity = &RunActivity{Turn: r.turn, Tool: d.Tool, CallID: d.CallID, Since: e.Time}
	case EvToolResponded:
		var d struct {
			CallID string `json:"callId"`
		}
		if r.Activity == nil || r.Activity.CallID == "" || e.Decode(&d) != nil || d.CallID != r.Activity.CallID {
			return
		}
		r.Activity = &RunActivity{Turn: r.turn, Since: e.Time}
	case EvApprovalRequested:
		r.paused, r.Activity = r.Activity, nil
	case EvRunSuspended:
		if r.Activity != nil {
			r.paused = r.Activity
		}
		r.Activity = nil
	case EvApprovalDecided, EvRunResumed:
		if r.turn == 0 || r.State.Terminal() {
			return
		}
		if e.Type == EvApprovalDecided {
			var d struct {
				ApprovalID string `json:"approvalId"`
			}
			if e.Decode(&d) != nil || r.Pending == nil || r.Pending.ID != d.ApprovalID {
				return
			}
		} else if r.Pending != nil {
			return
		}
		act := RunActivity{Turn: r.turn}
		if r.paused != nil {
			act = *r.paused
		}
		act.Since = e.Time
		r.Activity, r.paused = &act, nil
	case EvTurnFinished, EvRunFinished:
		r.Activity, r.paused, r.turn = nil, nil, 0
	}
}

// addUsage adds the usage of one model call. With a price for its model,
// the cost of the call enters CostUSD. Without one, its tokens enter
// UnpricedTokens, because their cost is unknown, not zero.
func (r *Run) addUsage(u Usage, provider, model string, prices Prices) {
	r.Usage.Usage = r.Usage.Usage.Add(u)
	if p, ok := priceOf(prices, provider, model); ok {
		r.Usage.CostUSD += Cost(u, p)
		return
	}
	r.Usage.UnpricedTokens += u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// setTotals sets the usage to the totals of a fw.run.finished or
// fw.run.suspended. Nil totals, from an older kernel, and the totals of an
// event that a branch kept from its source change nothing.
func (r *Run) setTotals(t *RunUsage, e Event) {
	if t == nil || r.keptEvent(e) {
		return
	}
	r.Usage = *t
	r.UsageSeq = e.Seq
}

// endKept follows an event that a branch kept from its source. A branch
// that is finished then did no work of its own, so its usage, zero, is
// known as of that event.
func (r *Run) endKept(seq uint64) {
	if r.State.Terminal() && r.UsageSeq == 0 {
		r.UsageSeq = seq
	}
}

// keptEvent reports whether e is one of the events that a branch kept from
// its source (RunQueuedData.Kept).
func (r *Run) keptEvent(e Event) bool { return e.Seq != 0 && e.Seq <= r.kept }

// priceOf looks up a price by "provider/model", then by the model alone.
func priceOf(prices Prices, provider, model string) (Price, bool) {
	if prices == nil || model == "" {
		return Price{}, false
	}
	if provider != "" {
		if p, ok := prices.Price(provider + "/" + model); ok {
			return p, true
		}
	}
	return prices.Price(model)
}
