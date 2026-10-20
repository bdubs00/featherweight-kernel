package fw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Role of a message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
)

// Block types.
const (
	BlockText       = "text"
	BlockImage      = "image"
	BlockDocument   = "document"
	BlockToolUse    = "tool_use"
	BlockToolResult = "tool_result"
	BlockOpaque     = "opaque" // provider data such as thinking, passed back unchanged
	BlockFile       = "file"   // an attachment of a person (section 7.10)
)

// Block is one provider-neutral content block.
type Block struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// image and document
	MediaType string `json:"mediaType,omitempty"`
	Data      []byte `json:"data,omitempty"` // raw bytes; JSON encodes base64
	URL       string `json:"url,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string  `json:"toolUseId,omitempty"`
	Content   []Block `json:"content,omitempty"`
	IsError   bool    `json:"isError,omitempty"`

	// opaque
	Provider string          `json:"provider,omitempty"`
	Raw      json.RawMessage `json:"raw,omitempty"`

	// file: a reference in the ledger; see Attachment for a new file
	File *FileRef `json:"file,omitempty"`
}

// Text returns a text block.
func Text(s string) Block { return Block{Type: BlockText, Text: s} }

// Message is one turn of the conversation.
type Message struct {
	Role    Role    `json:"role"`
	Content []Block `json:"content"`
}

// UserText returns a user message with one text block.
func UserText(s string) Message { return Message{Role: RoleUser, Content: []Block{Text(s)}} }

// TextOf joins the text blocks of a message or a response.
func TextOf(blocks []Block) string {
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == BlockText {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// Request is one model call.
//
// A model fills an empty Effort with the effort that it was opened with,
// the effort of the configuration of its process, unless EffortSet is
// true: then Effort is the effort of the call as it is, and an empty one
// means the default of the provider (see EffortOr). The kernel sets
// EffortSet for a run with a resolved profile, so a run keeps the effort
// that it was queued with when the configuration changes.
type Request struct {
	System    string            `json:"system,omitempty"`
	Messages  []Message         `json:"messages"`
	Tools     []ToolSpec        `json:"tools,omitempty"`
	Effort    string            `json:"effort,omitempty"` // low, medium, high, xhigh, max
	EffortSet bool              `json:"effortSet,omitempty"`
	MaxTokens int               `json:"maxTokens,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"` // run id, agent, for provider-side logging
}

// EffortDefault is the effort that fw.model.requested records for a call
// that leaves the effort to the provider: the model sends the default of
// its provider, such as high for Anthropic, or no effort at all.
const EffortDefault = "default"

// requestedEffort returns the effort that fw.model.requested records for
// the effort of a call. An empty effort reaches the model as the default
// of its provider: the effort of the configuration is empty too, or the
// call sets it (EffortSet).
func requestedEffort(effort string) string {
	if effort == "" {
		return EffortDefault
	}
	return effort
}

// EffortOr returns the effort that a model sends for the request: Effort,
// else def, the effort that the model was opened with. With EffortSet, an
// empty Effort stays empty, and the model uses the default of its provider.
func (r *Request) EffortOr(def string) string {
	if r.Effort != "" || r.EffortSet {
		return r.Effort
	}
	return def
}

// Usage counts tokens for one call.
type Usage struct {
	InputTokens      int `json:"inputTokens"`
	OutputTokens     int `json:"outputTokens"`
	CacheReadTokens  int `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens int `json:"cacheWriteTokens,omitempty"`
}

// Add returns the sum of two usages.
func (u Usage) Add(o Usage) Usage {
	return Usage{
		InputTokens:      u.InputTokens + o.InputTokens,
		OutputTokens:     u.OutputTokens + o.OutputTokens,
		CacheReadTokens:  u.CacheReadTokens + o.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens + o.CacheWriteTokens,
	}
}

// StopReason of a model response.
type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"
	StopToolUse   StopReason = "tool_use"
	StopMaxTokens StopReason = "max_tokens"
	StopRefusal   StopReason = "refusal"
	StopPauseTurn StopReason = "pause_turn"
	StopOther     StopReason = "other"
)

// Response is one model answer.
type Response struct {
	Content    []Block         `json:"content"`
	Raw        json.RawMessage `json:"raw,omitempty"` // provider blocks, unchanged, for the next call
	StopReason StopReason      `json:"stopReason"`
	StopDetail string          `json:"stopDetail,omitempty"` // refusal category
	Usage      Usage           `json:"usage"`
	Provider   string          `json:"provider"`
	Model      string          `json:"model"`
	// FirstByteMS is the time in milliseconds from the start of the call to
	// the first streamed event of the answer. A model that does not
	// measure it leaves it at zero.
	FirstByteMS int64 `json:"ttfbMs,omitempty"`
	// QueuedMS is the time in milliseconds that the call waited for a free
	// slot of its provider (ProviderConfig.MaxConcurrent). The kernel sets
	// it.
	QueuedMS int64 `json:"queuedMs,omitempty"`
}

// ToolUses returns the tool_use blocks of a response.
func (r *Response) ToolUses() []Block {
	var out []Block
	for _, b := range r.Content {
		if b.Type == BlockToolUse {
			out = append(out, b)
		}
	}
	return out
}

// Delta is one streaming increment.
type Delta struct {
	Type  string `json:"type"`            // "text" for a text increment, "block" for a finished block
	Text  string `json:"text,omitempty"`  // for "text"
	Block *Block `json:"block,omitempty"` // for "block"
}

// Model generates responses. Generate and Stream return the same Response.
type Model interface {
	Generate(ctx context.Context, req *Request) (*Response, error)
	Stream(ctx context.Context, req *Request, on func(Delta)) (*Response, error)
}

// ModelMiddleware wraps a model.
type ModelMiddleware func(Model) Model

// ModelFunc adapts two functions to Model.
type ModelFunc struct {
	GenerateFn func(ctx context.Context, req *Request) (*Response, error)
	StreamFn   func(ctx context.Context, req *Request, on func(Delta)) (*Response, error)
}

// Generate implements Model.
func (m ModelFunc) Generate(ctx context.Context, req *Request) (*Response, error) {
	if m.GenerateFn != nil {
		return m.GenerateFn(ctx, req)
	}
	return m.StreamFn(ctx, req, func(Delta) {})
}

// Stream implements Model.
func (m ModelFunc) Stream(ctx context.Context, req *Request, on func(Delta)) (*Response, error) {
	if m.StreamFn != nil {
		return m.StreamFn(ctx, req, on)
	}
	resp, err := m.GenerateFn(ctx, req)
	if err != nil {
		return nil, err
	}
	for i := range resp.Content {
		b := resp.Content[i]
		if b.Type == BlockText {
			on(Delta{Type: "text", Text: b.Text})
		}
		on(Delta{Type: "block", Block: &b})
	}
	return resp, nil
}

// slotModel holds each call of a model until its provider has a free slot
// (ProviderConfig.MaxConcurrent), and records the wait in the response.
// The slots of a provider are shared by every model of that provider in
// the process.
type slotModel struct {
	Model
	slots chan struct{}
}

// modelSlots returns the slots of each provider with a limit of concurrent
// calls.
func modelSlots(cfg *Config) map[string]chan struct{} {
	out := map[string]chan struct{}{}
	for name, pc := range cfg.Providers {
		if pc.MaxConcurrent > 0 {
			out[name] = make(chan struct{}, pc.MaxConcurrent)
		}
	}
	return out
}

// limitModel returns m behind the slots of the provider of selector, or m
// when the provider has no limit.
func limitModel(slots map[string]chan struct{}, selector string, m Model) Model {
	provider, _ := SplitSelector(selector)
	if s := slots[provider]; s != nil && m != nil {
		return &slotModel{Model: m, slots: s}
	}
	return m
}

// Unwrap returns the model behind the slots, so MediaTypesOf finds it.
func (m *slotModel) Unwrap() Model { return m.Model }

// Generate implements Model.
func (m *slotModel) Generate(ctx context.Context, req *Request) (*Response, error) {
	return m.call(ctx, func() (*Response, error) { return m.Model.Generate(ctx, req) })
}

// Stream implements Model.
func (m *slotModel) Stream(ctx context.Context, req *Request, on func(Delta)) (*Response, error) {
	return m.call(ctx, func() (*Response, error) { return m.Model.Stream(ctx, req, on) })
}

func (m *slotModel) call(ctx context.Context, fn func() (*Response, error)) (*Response, error) {
	start := time.Now()
	select {
	case m.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("fw: wait for a model slot: %w", context.Cause(ctx))
	}
	defer func() { <-m.slots }()
	queued := time.Since(start)
	resp, err := fn()
	if resp != nil {
		resp.QueuedMS = queued.Milliseconds()
	}
	return resp, err
}

// ErrContextOverflow marks a model error that says the request is larger
// than the context window of the model. An adapter or a custom model wraps
// it with %w. The kernel compacts the history and calls the model once more.
var ErrContextOverflow = errors.New("fw: the request is larger than the context window")

// overflowPhrases are the parts of the error messages that providers and
// OpenAI-compatible servers send when a request does not fit the context
// window. IsContextOverflow matches them, so an adapter, a proxy, or a local
// server works without a change.
var overflowPhrases = []string{
	"prompt is too long",                   // Anthropic
	"exceed context limit",                 // Anthropic, input and max_tokens together
	"context_length_exceeded",              // OpenAI error code
	"maximum context length",               // OpenAI, vLLM, OpenRouter
	"exceeds the maximum number of tokens", // Gemini
	"context window",                       // LiteLLM and others
	"exceeds the available context size",   // llama.cpp
	"max_seq_len",                          // tabbyAPI and ExLlama
}

// IsContextOverflow reports whether err says that the request is larger
// than the context window of the model: either err wraps ErrContextOverflow,
// or its message holds a phrase that a provider uses for it.
func IsContextOverflow(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrContextOverflow) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, p := range overflowPhrases {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

// SplitSelector splits "provider/model" into its parts.
func SplitSelector(selector string) (provider, model string) {
	i := strings.IndexByte(selector, '/')
	if i < 0 {
		return selector, ""
	}
	return selector[:i], selector[i+1:]
}
