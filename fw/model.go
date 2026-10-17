package fw

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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
type Request struct {
	System    string            `json:"system,omitempty"`
	Messages  []Message         `json:"messages"`
	Tools     []ToolSpec        `json:"tools,omitempty"`
	Effort    string            `json:"effort,omitempty"` // low, medium, high, xhigh, max
	MaxTokens int               `json:"maxTokens,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"` // run id, agent, for provider-side logging
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
