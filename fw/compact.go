package fw

// This file is context management (docs/ARCHITECTURE.md section 5.4). Before
// a model call, the kernel estimates the size of the request. When the size
// reaches compaction.at, or when a call fails because the request does not
// fit the context window, a Compactor summarizes the history, and the kernel
// appends fw.context.compacted with a new epoch. The ledger before the epoch
// stays unchanged.
//
// The default compactor uses simple compaction: the summary replaces the
// whole history, and no earlier turn or thinking block is replayed. A
// provider binds a thinking block to the history before it, so a kept tail of
// old turns is not valid after a summary. The summary call sends the history
// with the prefix of a turn, so it reads the prompt cache.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
)

// Estimates of the token count of content. The usage that a provider reports
// replaces them after each response (see historyBuilder.contextTokens).
const (
	bytesPerToken    = 4    // text, JSON, and opaque data
	imageTokens      = 1600 // one image, about the size that providers bill
	pdfBytesPerToken = 40   // a document that is not text, such as a PDF
)

const compactBlockMax = 4000 // characters of one block in a text transcript

// compactPrompt asks for the summary. It follows the summary prompt that
// Anthropic recommends for client-side compaction. The last sentence keeps
// the model from calling a tool, because the call still carries the tools of
// the run.
const compactPrompt = "Summarize this conversation inside <summary></summary> tags. " +
	"Include the information that lets the work continue in a new context window without redoing work or asking again for constraints or context. " +
	"Preserve: (1) every difficulty or problem that came up, and how it was handled or resolved; " +
	"(2) every option or approach that was raised, tried, or set aside, and why; " +
	"(3) everything that was asked for, decided, agreed, ruled out, or set as a preference, constraint, or boundary, stated exactly; " +
	"(4) exactly where the work stands now: what is covered, settled, or complete; " +
	"(5) everything still open, unresolved, promised, or expected next; " +
	"(6) specific details that are hard to reconstruct, such as names, numbers, dates, paths, commands, exact wording, and links, kept exactly. " +
	"Be complete on these, even at the cost of length, and keep everything else short. " +
	"Keep what the user said, asked for, or established close to their own words. Condense your own explanations and reasoning to what they concluded or produced. " +
	"Do not call any tools while you write this summary. Respond with text only."

// summaryIntro starts the user message that opens a new epoch.
const summaryIntro = "This run continues from an earlier part of the conversation that grew too long for the context window. " +
	"The summary below covers that part. Continue the work from where it stopped, and do not ask the caller to repeat what the summary holds.\n\n"

var errNothingToCompact = errors.New("fw: nothing to compact")

// DefaultCompactor returns the default compactor. It sends the history to
// the model with an instruction to summarize it, and the summary replaces
// the history. The trailing messages from the caller, such as a new
// question, stay after the summary word for word, because they carry no
// provider data. When the history with the instruction does not fit the
// context window, it summarizes the history up to the last response, which
// fit an earlier call, and adds the rest after the summary as plain text.
func DefaultCompactor() Compactor { return CompactorFunc(summarizeHistory) }

func summarizeHistory(ctx context.Context, m Model, h History) (Block, []Message, error) {
	msgs := h.Messages
	keep := callerTail(msgs)
	if len(keep) == len(msgs) {
		return Block{}, nil, errNothingToCompact
	}
	text, err := askSummary(ctx, m, msgs)
	var rest []Message
	if IsContextOverflow(err) {
		cut := lastResponse(msgs)
		if cut <= 0 {
			return Block{}, nil, fmt.Errorf("fw: compaction summary: %w", err)
		}
		rest = msgs[cut : len(msgs)-len(keep)]
		text, err = askSummary(ctx, m, msgs[:cut])
	}
	if err != nil {
		return Block{}, nil, fmt.Errorf("fw: compaction summary: %w", err)
	}
	var sb strings.Builder
	sb.WriteString(summaryIntro)
	sb.WriteString("<summary>\n")
	sb.WriteString(text)
	sb.WriteString("\n</summary>")
	if len(rest) > 0 {
		sb.WriteString("\n\nThese messages came after the part that the summary covers. Long blocks are cut.\n\n")
		sb.WriteString(transcript(rest))
	}
	return Text(sb.String()), append([]Message(nil), keep...), nil
}

// askSummary sends the messages and the summary instruction, and returns the
// text inside the summary tags, or the whole text when the tags are missing.
func askSummary(ctx context.Context, m Model, msgs []Message) (string, error) {
	req := &Request{Messages: append(append(make([]Message, 0, len(msgs)+1), msgs...), UserText(compactPrompt))}
	resp, err := m.Generate(ctx, req)
	if err != nil {
		return "", err
	}
	text := TextOf(resp.Content)
	if _, after, ok := strings.Cut(text, "<summary>"); ok {
		text, _, _ = strings.Cut(after, "</summary>")
	}
	if text = strings.TrimSpace(text); text == "" {
		return "", errors.New("the model returned no summary text")
	}
	return text, nil
}

// callerTail returns the messages at the end of the history that come from
// the caller or the kernel and hold no tool result and no provider data.
func callerTail(msgs []Message) []Message {
	i := len(msgs)
	for i > 0 && msgs[i-1].Role != RoleAssistant && plainContent(msgs[i-1].Content) {
		i--
	}
	return msgs[i:]
}

func plainContent(blocks []Block) bool {
	for _, b := range blocks {
		if b.Type == BlockToolResult || b.Type == BlockOpaque || b.Type == BlockToolUse {
			return false
		}
	}
	return true
}

// lastResponse returns the index of the last assistant message, or -1.
func lastResponse(msgs []Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == RoleAssistant {
			return i
		}
	}
	return -1
}

// transcript renders messages as plain text, so the text carries no tool
// blocks and no provider data.
func transcript(msgs []Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		for _, b := range m.Content {
			switch b.Type {
			case BlockText:
				fmt.Fprintf(&sb, "%s: %s\n", m.Role, clip(b.Text))
			case BlockToolUse:
				fmt.Fprintf(&sb, "%s called tool %s with %s\n", m.Role, b.Name, clip(string(b.Input)))
			case BlockToolResult:
				label := "tool result"
				if b.IsError {
					label = "tool error"
				}
				fmt.Fprintf(&sb, "%s: %s\n", label, clip(TextOf(b.Content)))
			case BlockImage, BlockDocument:
				fmt.Fprintf(&sb, "%s attached a %s\n", m.Role, b.Type)
			case BlockFile:
				if b.File != nil {
					fmt.Fprintf(&sb, "%s attached the file %q at %s\n", m.Role, b.File.Name, b.File.Path)
				}
			}
		}
	}
	return sb.String()
}

func clip(s string) string {
	if len(s) <= compactBlockMax {
		return s
	}
	cut := compactBlockMax
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + " [cut]"
}

// utf8RuneStart reports whether b starts a UTF-8 sequence.
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// estimateTokens estimates the tokens of content blocks.
func estimateTokens(blocks []Block) int {
	bytes, tokens := 0, 0
	var walk func([]Block)
	walk = func(bs []Block) {
		for _, b := range bs {
			switch b.Type {
			case BlockText:
				bytes += len(b.Text)
			case BlockToolUse:
				bytes += len(b.Name) + len(b.Input)
			case BlockToolResult:
				walk(b.Content)
			case BlockOpaque:
				bytes += len(b.Raw)
			case BlockImage:
				tokens += imageTokens
			case BlockDocument:
				if strings.HasPrefix(b.MediaType, "text/") {
					bytes += len(b.Data)
				} else {
					tokens += max(imageTokens, len(b.Data)/pdfBytesPerToken)
				}
			case BlockFile:
				tokens += fileTokens(b.File)
			}
		}
	}
	walk(blocks)
	return tokens + bytes/bytesPerToken
}

// boundModel is the model of a run as a Compactor sees it: a request that
// leaves a field empty gets the value of the next turn, so a summary call
// has the same prefix as a turn. It adds up the usage of its calls.
type boundModel struct {
	m    Model
	next *Request

	mu       sync.Mutex
	usage    Usage
	provider string
	model    string
}

func (b *boundModel) fill(req *Request) *Request {
	r := *req
	if r.System == "" {
		r.System = b.next.System
	}
	if r.Tools == nil {
		r.Tools = b.next.Tools
	}
	if r.Effort == "" && !r.EffortSet {
		r.Effort, r.EffortSet = b.next.Effort, b.next.EffortSet
	}
	if r.MaxTokens == 0 {
		r.MaxTokens = b.next.MaxTokens
	}
	if r.Metadata == nil {
		r.Metadata = b.next.Metadata
	}
	return &r
}

func (b *boundModel) record(resp *Response) {
	if resp == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.usage = b.usage.Add(resp.Usage)
	if resp.Provider != "" {
		b.provider = resp.Provider
	}
	if resp.Model != "" {
		b.model = resp.Model
	}
}

// Generate implements Model.
func (b *boundModel) Generate(ctx context.Context, req *Request) (*Response, error) {
	resp, err := b.m.Generate(ctx, b.fill(req))
	b.record(resp)
	return resp, err
}

// Stream implements Model.
func (b *boundModel) Stream(ctx context.Context, req *Request, on func(Delta)) (*Response, error) {
	resp, err := b.m.Stream(ctx, b.fill(req), on)
	b.record(resp)
	return resp, err
}

// CheckCompaction reports with ErrInvalidRequest whether the kernel
// accepts the compaction block of a profile. At must not be negative, Off
// and At do not go together, and Model must be a provider/model selector
// or, with cfg, a name from models. The text of the error names the key,
// such as compaction.at.
func CheckCompaction(cfg *Config, c *CompactionConfig) error {
	if c == nil {
		return nil
	}
	if c.At < 0 {
		return fmt.Errorf("%w: compaction.at %d is negative; 0 takes compaction.at of the configuration, and off: true turns compaction off", ErrInvalidRequest, c.At)
	}
	if c.Off && c.At != 0 {
		return fmt.Errorf("%w: compaction.off turns compaction off, so compaction.at must be 0", ErrInvalidRequest)
	}
	if c.Model != "" {
		sel := c.Model
		if cfg != nil {
			sel = cfg.ModelSelector(sel)
		}
		if provider, model := SplitSelector(sel); provider == "" || model == "" || strings.ContainsAny(sel, " \t\r\n") {
			return fmt.Errorf("%w: compaction.model %q is neither a provider/model selector nor a name from models", ErrInvalidRequest, c.Model)
		}
	}
	return nil
}

// compactionOf returns the compaction setting of a run: the size that
// starts a compaction, 0 when the run never compacts, and the selector of
// the summary model, "" for the model of the run. A run that the kernel
// resolved keeps the setting of its queue time (ResolvedProfile); a run
// from the ledger of an older kernel reads the block of the configuration.
func (a *Agent) compactionOf(rs *runState) (int, string) {
	if r := rs.queuedData().Resolved; r != nil && r.Compaction != nil {
		at, _ := r.Compaction.on()
		return at, r.Compaction.Model
	}
	c := a.processCompaction()
	at, _ := c.on()
	if c.Model != "" {
		return at, a.cfg.ModelSelector(c.Model)
	}
	return at, ""
}

// processCompaction returns the compaction block of the configuration,
// with the size that the kernel took from it at start.
func (a *Agent) processCompaction() CompactionConfig {
	c := a.cfg.Compaction
	c.At = a.k.compactAt
	return c
}

// maybeCompact compacts the history before a model call when the estimated
// size of the request reaches the compaction size of the run. next is the
// request of the call without its messages, and extra is the estimate of
// its system prompt and tools. A compactor error is recorded, and the turn
// goes on with the full history.
func (a *Agent) maybeCompact(ctx context.Context, rs *runState, m Model, next *Request, extra int) error {
	at, _ := a.compactionOf(rs)
	if at <= 0 {
		return nil
	}
	rs.emitMu.Lock()
	_, tokens := rs.historyLocked(extra)
	rs.emitMu.Unlock()
	if tokens < at {
		return nil
	}
	_, err := a.compact(ctx, rs, m, next)
	return err
}

// compact summarizes the history of a run and appends fw.context.compacted.
// It reports false when the compactor found nothing to compact or failed,
// and it records a failure as fw.error. The error is a ledger error or
// errStopped.
func (a *Agent) compact(ctx context.Context, rs *runState, m Model, next *Request) (bool, error) {
	rs.emitMu.Lock()
	h, _ := rs.historyLocked(0)
	n := len(rs.events)
	rs.emitMu.Unlock()
	c := a.Compactor()
	if c == nil {
		c = DefaultCompactor()
	}
	bm := &boundModel{m: m, next: next}
	_, model := a.compactionOf(rs)
	summary, keep, err := a.runCompactor(ctx, c, bm, h, model)
	if err != nil {
		if ctx.Err() != nil {
			return false, errStopped
		}
		if errors.Is(err, errNothingToCompact) {
			return false, nil
		}
		_, err := a.emit(ctx, rs, EvError, ErrorData{Message: "compaction: " + err.Error(), Retryable: true})
		return false, err
	}
	d := ContextCompactedData{Epoch: h.Epoch + 1, Summary: summary, Kept: keep, Provider: bm.provider, Model: bm.model}
	if bm.usage != (Usage{}) {
		d.Usage = &bm.usage
	}
	rs.emitMu.Lock()
	defer rs.emitMu.Unlock()
	// The pinned messages of the run come right after the summary, in
	// ledger order and verbatim, once each: a pinned message that the
	// compactor kept is not kept twice.
	if pinned := pinnedMessages(rs.events[:n]); len(pinned) > 0 {
		d.Kept = append(pinned, withoutMessages(d.Kept, pinned)...)
	}
	// A message that arrived during the summary call belongs to the new
	// epoch, after them.
	for _, e := range rs.events[n:] {
		if e.Type == EvMessageReceived {
			if msg, ok := receivedMessage(e); ok {
				d.Kept = append(d.Kept, msg)
			}
		}
	}
	_, err = a.emitLocked(ctx, rs, EvContextCompacted, d)
	return err == nil, err
}

// withoutMessages returns msgs without one copy of each message of drop,
// the last one, so a pinned message that the compactor also kept stays
// once.
func withoutMessages(msgs, drop []Message) []Message {
	out := append([]Message(nil), msgs...)
	for _, m := range drop {
		for i := len(out) - 1; i >= 0; i-- {
			if reflect.DeepEqual(out[i], m) {
				out = append(out[:i], out[i+1:]...)
				break
			}
		}
	}
	return out
}

// runCompactor calls the compactor. The summary call goes to the model
// selector of the compaction setting of the run when it is set, else to
// the model of the run.
func (a *Agent) runCompactor(ctx context.Context, c Compactor, bm *boundModel, h History, model string) (Block, []Message, error) {
	if model != "" {
		sm, err := a.openModel(ctx, model)
		if err != nil {
			return Block{}, nil, fmt.Errorf("compaction.model: %w", err)
		}
		bm.m = sm
	}
	return c.Compact(ctx, bm, h)
}

// fileTokens estimates an attachment as readFiles sends it: an image, a
// document, the text of a small text file, or a note of about 40 tokens.
func fileTokens(f *FileRef) int {
	const note = 40
	switch {
	case f == nil:
		return 0
	case strings.HasPrefix(f.MediaType, "image/"):
		return note + imageTokens
	case f.MediaType == "application/pdf":
		return note + max(imageTokens, int(f.Size)/pdfBytesPerToken)
	case strings.HasPrefix(f.MediaType, "text/"):
		return note + int(min(f.Size, maxInlineText))/bytesPerToken
	}
	return note
}
