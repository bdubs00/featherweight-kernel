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
// the tool_use blocks. A message that arrives while tool calls are open
// follows the tool results. A tool_use block that never runs gets an error
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
	if b.known > 0 {
		return b.known + b.after
	}
	return b.tokens + extra
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
		if len(b.open) > 0 {
			b.deferred = append(b.deferred, m)
			return
		}
		b.add(m)
	case EvModelResponded:
		var d ModelRespondedData
		if e.Decode(&d) != nil {
			return
		}
		b.abandonCalls("no result was recorded for this tool call")
		if len(d.Content) > 0 {
			b.add(Message{Role: RoleAssistant, Content: d.Content})
		}
		if u := d.Usage; u.InputTokens+u.CacheReadTokens+u.CacheWriteTokens > 0 {
			b.known = u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens + u.OutputTokens
			b.after = 0
		}
		uses := toolUses(d.Content)
		if len(uses) == 0 {
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

func (b *historyBuilder) history() History {
	return History{Epoch: b.epoch, Messages: append([]Message(nil), b.msgs...)}
}

// receivedMessage returns the message of a fw.message.received event. Only
// the roles user and system enter the history.
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
	return Message{Role: d.Role, Content: d.Content}, true
}

// ProjectRun builds the status object of a run from its events. The prices
// argument is optional. When it knows the model of a response, the cost of
// that response enters the usage.
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

// Apply folds one event into the run status. A nil prices adds no cost.
func (r *Run) Apply(e Event, prices Prices) {
	if r.ID == "" {
		r.ID = e.Subject
	}
	if e.Seq > r.Seq {
		r.Seq = e.Seq
	}
	if !e.Time.IsZero() {
		r.UpdatedAt = e.Time
	}
	switch e.Type {
	case EvRunQueued:
		var d RunQueuedData
		if e.Decode(&d) != nil {
			return
		}
		r.Agent, r.Input, r.Origin = d.Agent, d.Input, d.Origin
		r.Principal, r.Parent, r.Metadata = d.Principal, d.Parent, d.Metadata
		r.Depth, r.ExtraTools = d.Depth, d.ExtraTools
		r.Title = strings.TrimSpace(d.Metadata[MetaTitle])
		if r.Title == "" {
			r.Title = TitleFrom(d.Input)
		}
		r.State = StateQueued
		r.CreatedAt = e.Time
	case EvRunStarted, EvRunResumed, EvTurnStarted:
		if e.Type == EvTurnStarted {
			r.Usage.Turns++
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
		if r.Pending != nil {
			return
		}
		if r.State.Terminal() {
			r.Output, r.Error = "", ""
		}
		r.State = StateRunning
	case EvModelResponded:
		var d struct {
			Usage    Usage  `json:"usage"`
			Provider string `json:"provider"`
			Model    string `json:"model"`
		}
		if e.Decode(&d) != nil {
			return
		}
		r.Usage.Usage = r.Usage.Usage.Add(d.Usage)
		if p, ok := priceOf(prices, d.Provider, d.Model); ok {
			r.Usage.CostUSD += Cost(d.Usage, p)
		}
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
		if d.Usage != nil {
			r.Usage.Usage = r.Usage.Usage.Add(*d.Usage)
			if p, ok := priceOf(prices, d.Provider, d.Model); ok {
				r.Usage.CostUSD += Cost(*d.Usage, p)
			}
		}
	case EvContextCompacted:
		var d ContextCompactedData
		if e.Decode(&d) != nil || d.Usage == nil {
			return
		}
		r.Usage.Usage = r.Usage.Usage.Add(*d.Usage)
		if p, ok := priceOf(prices, d.Provider, d.Model); ok {
			r.Usage.CostUSD += Cost(*d.Usage, p)
		}
	case EvToolRequested:
		r.Usage.ToolCalls++
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
		r.State = StateSuspended
	case EvRunFinished:
		var d RunFinishedData
		if e.Decode(&d) != nil {
			return
		}
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
