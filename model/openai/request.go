package openai

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// defaultSchema is the parameters of a tool that has no input schema.
var defaultSchema = json.RawMessage(`{"type":"object","properties":{}}`)

// chatRequest is the body of a chat completions call.
type chatRequest struct {
	Model               string         `json:"model"`
	Messages            []message      `json:"messages"`
	Tools               []tool         `json:"tools,omitempty"`
	Stream              bool           `json:"stream"`
	StreamOptions       *streamOptions `json:"stream_options,omitempty"`
	MaxTokens           int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens int            `json:"max_completion_tokens,omitempty"`
	ReasoningEffort     string         `json:"reasoning_effort,omitempty"`
	PromptCacheKey      string         `json:"prompt_cache_key,omitempty"`
	ParallelToolCalls   *bool          `json:"parallel_tool_calls,omitempty"`
}

// BodyOptions shape the request body beyond what the request itself says.
// The zero value sends the effort nowhere, puts the token limit in
// max_tokens, and adds no other field, which every server takes.
type BodyOptions struct {
	// Reasoning sends the effort as reasoning_effort, which OpenAI
	// reasoning models take and other servers reject.
	Reasoning bool

	// MaxTokensField names the field that carries the token limit:
	// "max_tokens" or "max_completion_tokens". Empty or "auto" follows
	// Reasoning: max_completion_tokens with it, max_tokens without it.
	MaxTokensField string

	// PromptCacheKey goes out as prompt_cache_key when it is not empty.
	PromptCacheKey string

	// ParallelToolCalls goes out as parallel_tool_calls when it is set.
	ParallelToolCalls *bool
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// message is one chat message. Content is a string, a list of parts, or nil.
// Extra holds the fields of an opaque block, such as reasoning_content, that
// go back on an assistant message as they came.
type message struct {
	Role       string                     `json:"role"`
	Content    any                        `json:"content,omitempty"`
	ToolCalls  []toolCall                 `json:"tool_calls,omitempty"`
	ToolCallID string                     `json:"tool_call_id,omitempty"`
	Extra      map[string]json.RawMessage `json:"-"`
}

// MarshalJSON adds Extra to the fields of the message. A field of the
// message wins over an extra field of the same name.
func (m message) MarshalJSON() ([]byte, error) {
	type plain message
	b, err := json.Marshal(plain(m))
	if err != nil || len(m.Extra) == 0 {
		return b, err
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil {
		return nil, err
	}
	for k, v := range m.Extra {
		if _, ok := obj[k]; !ok {
			obj[k] = v
		}
	}
	return json.Marshal(obj)
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
	File     *file     `json:"file,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type file struct {
	Filename string `json:"filename,omitempty"`
	FileData string `json:"file_data"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function functionCall `json:"function"`
}

type functionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type tool struct {
	Type     string   `json:"type"`
	Function function `json:"function"`
}

type function struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict"`
}

// BuildBody maps a request to the JSON body of a streamed chat completions
// call.
//
// It returns the body and a map from the wire name of each tool to the name
// in its fw.ToolSpec, for the tools whose names differ. The stream uses the
// map to rename tool calls.
func BuildBody(req *fw.Request, model string, o BodyOptions) ([]byte, map[string]string, error) {
	if req == nil {
		return nil, nil, fmt.Errorf("openai: nil request")
	}
	if len(req.Messages) == 0 {
		return nil, nil, fmt.Errorf("openai: request has no messages")
	}
	if model == "" {
		return nil, nil, fmt.Errorf("openai: no model name")
	}
	effort, err := effortOf(req.Effort)
	if err != nil {
		return nil, nil, err
	}
	tools, names, err := buildTools(req.Tools)
	if err != nil {
		return nil, nil, err
	}
	toWire := make(map[string]string, len(names))
	for wire, spec := range names {
		toWire[spec] = wire
	}
	msgs, err := buildMessages(req.System, req.Messages, toWire)
	if err != nil {
		return nil, nil, err
	}
	body := chatRequest{
		Model:             model,
		Messages:          msgs,
		Tools:             tools,
		Stream:            true,
		StreamOptions:     &streamOptions{IncludeUsage: true},
		PromptCacheKey:    o.PromptCacheKey,
		ParallelToolCalls: o.ParallelToolCalls,
	}
	if o.Reasoning {
		body.ReasoningEffort = effort
	}
	field := o.MaxTokensField
	if field == "" || field == "auto" {
		field = "max_tokens"
		if o.Reasoning {
			field = "max_completion_tokens"
		}
	}
	switch field {
	case "max_tokens":
		body.MaxTokens = req.MaxTokens
	case "max_completion_tokens":
		body.MaxCompletionTokens = req.MaxTokens
	default:
		return nil, nil, fmt.Errorf("openai: unknown max tokens field %q (want max_tokens or max_completion_tokens)", field)
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, nil, fmt.Errorf("openai: encode request: %w", err)
	}
	return b, names, nil
}

// effortOf maps an effort level to a reasoning_effort value. The API names
// no level above high, so xhigh and max map to high. Empty stays empty.
func effortOf(s string) (string, error) {
	switch s {
	case "", "low", "medium", "high":
		return s, nil
	case "xhigh", "max":
		return "high", nil
	}
	return "", fmt.Errorf("openai: unknown effort %q (want low, medium, high, xhigh, or max)", s)
}

// buildTools maps tool specs to function tools in the order given. Native
// shapes are an Anthropic feature, so a native tool goes out with its JSON
// schema.
func buildTools(specs []fw.ToolSpec) ([]tool, map[string]string, error) {
	names := map[string]string{}
	if len(specs) == 0 {
		return nil, names, nil
	}
	wire := make(map[string]string, len(specs)) // wire name to spec name, to find collisions
	out := make([]tool, 0, len(specs))
	for _, s := range specs {
		if s.Name == "" {
			return nil, nil, fmt.Errorf("openai: tool spec has no name")
		}
		w := wireNameFor(s.Name, wire)
		if prev, ok := wire[w]; ok {
			return nil, nil, fmt.Errorf("openai: tools %q and %q both reach the model as %q", prev, s.Name, w)
		}
		wire[w] = s.Name
		if w != s.Name {
			names[w] = s.Name
		}
		schema := s.InputSchema
		if len(schema) == 0 {
			schema = defaultSchema
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(schema, &obj); err != nil {
			return nil, nil, fmt.Errorf("openai: tool %q input schema is not a JSON object: %w", s.Name, err)
		}
		out = append(out, tool{Type: "function", Function: function{
			Name:        w,
			Description: s.Description,
			Parameters:  schema,
		}})
	}
	return out, names, nil
}

// sanitize returns a function name that OpenAI accepts: the characters
// [A-Za-z0-9_-], at most 64 of them. A dot, as in "git.commit", becomes
// "__", and any other character becomes "_".
func sanitize(name string) string {
	var sb strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			sb.WriteRune(r)
		case r == '.':
			sb.WriteString("__")
		default:
			sb.WriteByte('_')
		}
	}
	w := sb.String()
	if w == "" {
		w = "tool"
	}
	if len(w) > 64 {
		w = w[:64]
	}
	return w
}

// wireNameFor returns the sanitized name of a tool. A sanitized name that
// collides with one already taken gets a numeric suffix. A name that the API
// accepts as it is stays as it is, and a collision between two such names is
// a configuration error that the caller reports.
func wireNameFor(name string, taken map[string]string) string {
	w := sanitize(name)
	if w == name {
		return w
	}
	base := w
	for i := 2; ; i++ {
		prev, ok := taken[w]
		if !ok || prev == name {
			return w
		}
		suffix := "_" + strconv.Itoa(i)
		if len(base)+len(suffix) > 64 {
			base = base[:64-len(suffix)]
		}
		w = base + suffix
	}
}

// buildMessages maps the system prompt and the conversation.
//
// A system message before the first other message joins the system prompt
// as a system message of its own. A later system message becomes a user
// message in a <system-reminder> block, because many local chat templates
// reject a system message after the start.
func buildMessages(system string, in []fw.Message, toWire map[string]string) ([]message, error) {
	wireName := func(name string) string {
		if w, ok := toWire[name]; ok {
			return w
		}
		return sanitize(name)
	}
	out := make([]message, 0, len(in)+1)
	if system != "" {
		out = append(out, message{Role: "system", Content: system})
	}
	started := false
	for i, m := range in {
		switch m.Role {
		case fw.RoleSystem:
			text := fw.TextOf(m.Content)
			if text == "" {
				continue
			}
			if !started {
				out = append(out, message{Role: "system", Content: text})
				continue
			}
			out = append(out, message{Role: "user", Content: "<system-reminder>\n" + text + "\n</system-reminder>"})
		case fw.RoleUser:
			msgs, err := userMessages(m.Content, i)
			if err != nil {
				return nil, err
			}
			out = append(out, msgs...)
		case fw.RoleAssistant:
			msg, ok, err := assistantMessage(m.Content, i, wireName)
			if err != nil {
				return nil, err
			}
			if ok {
				out = append(out, msg)
			}
		default:
			return nil, fmt.Errorf("openai: message %d has unknown role %q", i, m.Role)
		}
		if m.Role != fw.RoleSystem {
			started = true
		}
	}
	return out, nil
}

// userMessages maps a user message. Each tool result becomes a tool message,
// first, as the API requires after an assistant message with tool calls.
// The other blocks form one user message after them. A tool message holds
// only text, so the images and documents of a tool result go into that user
// message.
func userMessages(in []fw.Block, msg int) ([]message, error) {
	var (
		tools []message
		media []contentPart
		parts []contentPart
	)
	for j, b := range in {
		switch b.Type {
		case fw.BlockText:
			if b.Text != "" {
				parts = append(parts, contentPart{Type: "text", Text: b.Text})
			}
		case fw.BlockImage, fw.BlockDocument:
			p, err := mediaPart(b)
			if err != nil {
				return nil, fmt.Errorf("openai: message %d block %d: %w", msg, j, err)
			}
			parts = append(parts, p)
		case fw.BlockToolResult:
			tm, tmedia, err := toolMessage(b)
			if err != nil {
				return nil, fmt.Errorf("openai: message %d block %d: %w", msg, j, err)
			}
			tools = append(tools, tm)
			media = append(media, tmedia...)
		case fw.BlockOpaque:
			// No opaque data goes on a user message.
		default:
			return nil, fmt.Errorf("openai: message %d block %d: block type %q is not allowed in a user message", msg, j, b.Type)
		}
	}
	out := tools
	if all := append(media, parts...); len(all) > 0 {
		var content any = all
		if len(all) == 1 && all[0].Type == "text" {
			content = all[0].Text
		}
		out = append(out, message{Role: "user", Content: content})
	}
	return out, nil
}

// toolMessage maps a tool result. An error result starts with "Error: ",
// because the API has no error flag, unless its text already starts with
// "Error", as the text editor tools write it. It returns the images and documents of
// the result apart, after a text part that names the call.
func toolMessage(b fw.Block) (message, []contentPart, error) {
	if b.ToolUseID == "" {
		return message{}, nil, fmt.Errorf("tool_result has no tool use ID")
	}
	var (
		texts []string
		media []contentPart
	)
	for _, c := range b.Content {
		switch c.Type {
		case fw.BlockText:
			if c.Text != "" {
				texts = append(texts, c.Text)
			}
		case fw.BlockImage, fw.BlockDocument:
			p, err := mediaPart(c)
			if err != nil {
				return message{}, nil, fmt.Errorf("tool_result %s: %w", b.ToolUseID, err)
			}
			media = append(media, p)
		default:
			return message{}, nil, fmt.Errorf("tool_result %s: block type %q is not allowed in a tool result", b.ToolUseID, c.Type)
		}
	}
	text := strings.Join(texts, "\n")
	if b.IsError && !strings.HasPrefix(strings.ToLower(text), "error") {
		text = "Error: " + text
	}
	if len(media) > 0 {
		media = append([]contentPart{{Type: "text", Text: "Attachments of the result of tool call " + b.ToolUseID + ":"}}, media...)
	}
	return message{Role: "tool", ToolCallID: b.ToolUseID, Content: text}, media, nil
}

// mediaPart maps an image to an image_url part and a document to a text part
// (text/*) or a file part (PDF).
func mediaPart(b fw.Block) (contentPart, error) {
	if b.Type == fw.BlockImage {
		switch {
		case len(b.Data) > 0:
			if b.MediaType == "" {
				return contentPart{}, fmt.Errorf("image has data but no media type")
			}
			return contentPart{Type: "image_url", ImageURL: &imageURL{URL: dataURL(b.MediaType, b.Data)}}, nil
		case b.URL != "":
			return contentPart{Type: "image_url", ImageURL: &imageURL{URL: b.URL}}, nil
		}
		return contentPart{}, fmt.Errorf("image has neither data nor a URL")
	}
	mt := strings.ToLower(b.MediaType)
	switch {
	case len(b.Data) > 0 && strings.HasPrefix(mt, "text/"):
		return contentPart{Type: "text", Text: string(b.Data)}, nil
	case len(b.Data) > 0 && (mt == "" || mt == "application/pdf"):
		return contentPart{Type: "file", File: &file{Filename: "document.pdf", FileData: dataURL("application/pdf", b.Data)}}, nil
	case len(b.Data) > 0:
		return contentPart{}, fmt.Errorf("document media type %q is not supported (want application/pdf or text/*)", b.MediaType)
	case b.URL != "":
		return contentPart{}, fmt.Errorf("a document URL is not supported; send the document bytes")
	}
	return contentPart{}, fmt.Errorf("document has neither data nor a URL")
}

func dataURL(mediaType string, data []byte) string {
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// assistantMessage maps an assistant message. The text blocks join into the
// content, and an opaque block of this provider adds its fields, such as
// reasoning_content. It reports false for a message with nothing to send.
func assistantMessage(in []fw.Block, msg int, wireName func(string) string) (message, bool, error) {
	m := message{Role: "assistant"}
	var text strings.Builder
	for j, b := range in {
		switch b.Type {
		case fw.BlockText:
			text.WriteString(b.Text)
		case fw.BlockToolUse:
			args := string(b.Input)
			var cut string
			switch {
			case args == "":
				args = "{}"
			case json.Unmarshal(b.Input, &cut) == nil:
				// Arguments that were not valid JSON, kept as a JSON string.
				args = cut
			}
			m.ToolCalls = append(m.ToolCalls, toolCall{
				ID:       b.ID,
				Type:     "function",
				Function: functionCall{Name: wireName(b.Name), Arguments: args},
			})
		case fw.BlockOpaque:
			if b.Provider != Provider {
				continue
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(b.Raw, &fields); err != nil {
				return message{}, false, fmt.Errorf("openai: message %d block %d: opaque block is not a JSON object: %w", msg, j, err)
			}
			if m.Extra == nil {
				m.Extra = map[string]json.RawMessage{}
			}
			for k, v := range fields {
				m.Extra[k] = v
			}
		default:
			return message{}, false, fmt.Errorf("openai: message %d block %d: block type %q is not allowed in an assistant message", msg, j, b.Type)
		}
	}
	if text.Len() == 0 && len(m.ToolCalls) == 0 && len(m.Extra) == 0 {
		return message{}, false, nil
	}
	// An empty string, not null: some chat templates print a null content.
	m.Content = text.String()
	return m, true, nil
}
