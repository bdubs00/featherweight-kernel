package anthropic

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Wire names of the Anthropic-defined tools.
const (
	nativeBashName       = "bash"
	nativeTextEditorName = "str_replace_based_edit_tool"
	nativeMemoryName     = "memory"
)

// eagerTools is the number of custom tools that stay loaded when tool search
// defers the rest. The first tools of the sorted list stay, so the cached
// prefix does not move when the list grows at the end.
const eagerTools = 8

// defaultSchema is the input schema of a custom tool that has none.
var defaultSchema = json.RawMessage(`{"type":"object"}`)

// BuildParams maps a request to the beta message parameters.
//
// It returns the parameters and a map from the wire name of each native tool
// to the name in its fw.ToolSpec, for example "str_replace_based_edit_tool"
// to "files". MapResponse uses the map to rename tool_use blocks.
//
// An empty model selects DefaultModel. A threshold of zero selects
// DefaultSearchThreshold, and a negative threshold turns tool search off.
// The environment variable FW_ANTHROPIC_FALLBACKS=off turns the server-side
// refusal fallback off.
func BuildParams(req *fw.Request, model string, threshold int) (anthropic.BetaMessageNewParams, map[string]string, error) {
	var p anthropic.BetaMessageNewParams
	if req == nil {
		return p, nil, fmt.Errorf("anthropic: nil request")
	}
	if len(req.Messages) == 0 {
		return p, nil, fmt.Errorf("anthropic: request has no messages")
	}
	if model == "" {
		model = DefaultModel
	}
	if threshold == 0 {
		threshold = DefaultSearchThreshold
	}

	effort, err := effortOf(req.Effort)
	if err != nil {
		return p, nil, err
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}

	tools, names, err := buildTools(req.Tools, threshold)
	if err != nil {
		return p, nil, err
	}
	toWire := make(map[string]string, len(names))
	for wire, spec := range names {
		toWire[spec] = wire
	}
	msgs, err := buildMessages(req.Messages, toWire)
	if err != nil {
		return p, nil, err
	}

	p.Model = model
	p.MaxTokens = int64(maxTokens)
	p.Messages = msgs
	p.Tools = tools
	// Thinking stays on: the adapter sends no thinking parameter. Effort
	// controls how much the model thinks.
	p.OutputConfig = anthropic.BetaOutputConfigParam{Effort: effort}
	if req.System != "" {
		// One breakpoint on the last system block caches the tools and the
		// system prompt together.
		p.System = []anthropic.BetaTextBlockParam{{
			Text:         req.System,
			CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
		}}
	}
	// Automatic caching moves a second breakpoint to the end of the
	// conversation on every call, so each turn reads the history that the
	// turn before it wrote.
	p.CacheControl = anthropic.NewBetaCacheControlEphemeralParam()
	if fallbacksOn() {
		p.Fallbacks = anthropic.BetaFallbacksParamOfDefault()
		p.Betas = append(p.Betas, anthropic.AnthropicBetaServerSideFallback2026_07_01)
	}
	return p, names, nil
}

// fallbacksOn reports whether the server-side refusal fallback is on.
func fallbacksOn() bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv(FallbacksEnv)), "off")
}

// effortOf validates an effort level. Empty means DefaultEffort.
func effortOf(s string) (anthropic.BetaOutputConfigEffort, error) {
	if s == "" {
		s = DefaultEffort
	}
	switch e := anthropic.BetaOutputConfigEffort(s); e {
	case anthropic.BetaOutputConfigEffortLow,
		anthropic.BetaOutputConfigEffortMedium,
		anthropic.BetaOutputConfigEffortHigh,
		anthropic.BetaOutputConfigEffortXhigh,
		anthropic.BetaOutputConfigEffortMax:
		return e, nil
	}
	return "", fmt.Errorf("anthropic: unknown effort %q (want low, medium, high, xhigh, or max)", s)
}

// buildTools maps tool specs in the order given. It returns the tools and the
// map from native wire name to spec name.
func buildTools(specs []fw.ToolSpec, threshold int) ([]anthropic.BetaToolUnionParam, map[string]string, error) {
	names := map[string]string{}
	if len(specs) == 0 {
		return nil, names, nil
	}
	search := threshold > 0 && len(specs) > threshold
	wire := make(map[string]string, len(specs)) // wire name to spec name, to find collisions
	out := make([]anthropic.BetaToolUnionParam, 0, len(specs)+1)
	custom, deferred := 0, 0
	for _, s := range specs {
		if s.Name == "" {
			return nil, nil, fmt.Errorf("anthropic: tool spec has no name")
		}
		var (
			t        anthropic.BetaToolUnionParam
			wireName string
		)
		switch s.Native {
		case fw.NativeBash:
			wireName = nativeBashName
			t.OfBashTool20250124 = &anthropic.BetaToolBash20250124Param{}
		case fw.NativeTextEditor:
			wireName = nativeTextEditorName
			t.OfTextEditor20250728 = &anthropic.BetaToolTextEditor20250728Param{}
		case fw.NativeMemory:
			wireName = nativeMemoryName
			t.OfMemoryTool20250818 = &anthropic.BetaMemoryTool20250818Param{}
		case "":
			wireName = wireNameFor(s.Name, wire)
			ct, err := customTool(s)
			if err != nil {
				return nil, nil, err
			}
			ct.Name = wireName
			if search && custom >= eagerTools {
				ct.DeferLoading = param.NewOpt(true)
				deferred++
			}
			custom++
			t.OfTool = ct
		default:
			return nil, nil, fmt.Errorf("anthropic: tool %q has unknown native shape %q", s.Name, s.Native)
		}
		if prev, ok := wire[wireName]; ok {
			return nil, nil, fmt.Errorf("anthropic: tools %q and %q both reach the model as %q", prev, s.Name, wireName)
		}
		wire[wireName] = s.Name
		if s.Native != "" || wireName != s.Name {
			names[wireName] = s.Name
		}
		out = append(out, t)
	}
	// The search tool only helps when at least one tool is deferred.
	if deferred > 0 {
		out = append(out, anthropic.BetaToolUnionParam{
			OfToolSearchToolBm25_20251119: &anthropic.BetaToolSearchToolBm25_20251119Param{
				Type: anthropic.BetaToolSearchToolBm25_20251119TypeToolSearchToolBm25_20251119,
			},
		})
	}
	return out, names, nil
}

// wireNameFor returns a tool name the API accepts: the characters
// [A-Za-z0-9_-], at most 64 of them. A dot, as in "git.commit", becomes "__",
// and any other character becomes "_". A name that collides with one already
// taken gets a numeric suffix.
func wireNameFor(name string, taken map[string]string) string {
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
	if w == name {
		// A name the API accepts as it is. A collision between two such names
		// is a configuration error that the caller reports.
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

// customTool maps a spec with no native shape. The input schema goes to the
// API byte for byte, so the cached prefix matches the spec exactly.
func customTool(s fw.ToolSpec) (*anthropic.BetaToolParam, error) {
	schema := s.InputSchema
	if len(schema) == 0 {
		schema = defaultSchema
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(schema, &obj); err != nil {
		return nil, fmt.Errorf("anthropic: tool %q input schema is not a JSON object: %w", s.Name, err)
	}
	t := &anthropic.BetaToolParam{
		Name:        s.Name,
		InputSchema: param.Override[anthropic.BetaToolInputSchemaParam](schema),
	}
	if s.Description != "" {
		t.Description = param.NewOpt(s.Description)
	}
	return t, nil
}

// buildMessages maps the conversation. toWire maps a spec name to the wire
// name of its native tool, so a tool_use block in history carries the name
// that the model produced.
//
// A system message becomes a mid-conversation system message where the API
// accepts one: after a user message, and last or before an assistant
// message. Elsewhere its text goes into the next user message as a
// <system-reminder> block. The placement depends only on the neighbors of
// the message, so a later call renders earlier turns the same way.
func buildMessages(in []fw.Message, toWire map[string]string) ([]anthropic.BetaMessageParam, error) {
	out := make([]anthropic.BetaMessageParam, 0, len(in))
	var pending []anthropic.BetaContentBlockParamUnion
	for i, m := range in {
		switch m.Role {
		case fw.RoleSystem:
			text := fw.TextOf(m.Content)
			if text == "" {
				continue
			}
			if n := len(out); n > 0 && out[n-1].Role == anthropic.BetaMessageParamRoleSystem {
				out[n-1].Content = append(out[n-1].Content, anthropic.NewBetaTextBlock(text))
				continue
			}
			if systemFits(out, in, i) {
				out = append(out, anthropic.BetaMessageParam{
					Role:    anthropic.BetaMessageParamRoleSystem,
					Content: []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock(text)},
				})
				continue
			}
			pending = append(pending, anthropic.NewBetaTextBlock(reminder(text)))
		case fw.RoleUser:
			blocks, err := userBlocks(m.Content, i)
			if err != nil {
				return nil, err
			}
			if len(pending) > 0 {
				blocks = append(pending, blocks...)
				pending = nil
			}
			if len(blocks) == 0 {
				continue
			}
			out = append(out, anthropic.BetaMessageParam{Role: anthropic.BetaMessageParamRoleUser, Content: blocks})
		case fw.RoleAssistant:
			blocks, err := assistantBlocks(m.Content, i, toWire)
			if err != nil {
				return nil, err
			}
			if len(blocks) == 0 {
				continue
			}
			out = append(out, anthropic.BetaMessageParam{Role: anthropic.BetaMessageParamRoleAssistant, Content: blocks})
		default:
			return nil, fmt.Errorf("anthropic: message %d has unknown role %q", i, m.Role)
		}
	}
	if len(pending) > 0 {
		out = append(out, anthropic.BetaMessageParam{Role: anthropic.BetaMessageParamRoleUser, Content: pending})
	}
	return out, nil
}

// systemFits reports whether the system message at in[i] can be a
// mid-conversation system message. It must follow a user message, and it
// must be last or come before an assistant message.
func systemFits(out []anthropic.BetaMessageParam, in []fw.Message, i int) bool {
	if n := len(out); n == 0 || out[n-1].Role != anthropic.BetaMessageParamRoleUser {
		return false
	}
	for _, next := range in[i+1:] {
		if next.Role == fw.RoleSystem {
			continue
		}
		return next.Role == fw.RoleAssistant
	}
	return true
}

func reminder(text string) string {
	return "<system-reminder>\n" + text + "\n</system-reminder>"
}

// userBlocks maps the blocks of a user message.
func userBlocks(in []fw.Block, msg int) ([]anthropic.BetaContentBlockParamUnion, error) {
	out := make([]anthropic.BetaContentBlockParamUnion, 0, len(in))
	for j, b := range in {
		switch b.Type {
		case fw.BlockText:
			if b.Text != "" {
				out = append(out, anthropic.NewBetaTextBlock(b.Text))
			}
		case fw.BlockImage:
			img, err := imageBlock(b)
			if err != nil {
				return nil, fmt.Errorf("anthropic: message %d block %d: %w", msg, j, err)
			}
			out = append(out, anthropic.BetaContentBlockParamUnion{OfImage: img})
		case fw.BlockDocument:
			doc, err := documentBlock(b)
			if err != nil {
				return nil, fmt.Errorf("anthropic: message %d block %d: %w", msg, j, err)
			}
			out = append(out, anthropic.BetaContentBlockParamUnion{OfDocument: doc})
		case fw.BlockToolResult:
			tr, err := toolResultBlock(b)
			if err != nil {
				return nil, fmt.Errorf("anthropic: message %d block %d: %w", msg, j, err)
			}
			out = append(out, anthropic.BetaContentBlockParamUnion{OfToolResult: tr})
		case fw.BlockOpaque:
			if b.Provider != Provider {
				continue
			}
			u, err := opaqueBlock(b.Raw)
			if err != nil {
				return nil, fmt.Errorf("anthropic: message %d block %d: %w", msg, j, err)
			}
			out = append(out, u)
		default:
			return nil, fmt.Errorf("anthropic: message %d block %d: block type %q is not allowed in a user message", msg, j, b.Type)
		}
	}
	return out, nil
}

// assistantBlocks maps the blocks of an assistant message.
func assistantBlocks(in []fw.Block, msg int, toWire map[string]string) ([]anthropic.BetaContentBlockParamUnion, error) {
	out := make([]anthropic.BetaContentBlockParamUnion, 0, len(in))
	for j, b := range in {
		switch b.Type {
		case fw.BlockText:
			if b.Text != "" {
				out = append(out, anthropic.NewBetaTextBlock(b.Text))
			}
		case fw.BlockToolUse:
			input := b.Input
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			if !json.Valid(input) {
				return nil, fmt.Errorf("anthropic: message %d block %d: tool_use %q input is not valid JSON", msg, j, b.ID)
			}
			name := b.Name
			if w, ok := toWire[name]; ok {
				name = w
			}
			out = append(out, anthropic.NewBetaToolUseBlock(b.ID, input, name))
		case fw.BlockOpaque:
			if b.Provider != Provider {
				continue
			}
			u, err := opaqueBlock(b.Raw)
			if err != nil {
				return nil, fmt.Errorf("anthropic: message %d block %d: %w", msg, j, err)
			}
			out = append(out, u)
		default:
			return nil, fmt.Errorf("anthropic: message %d block %d: block type %q is not allowed in an assistant message", msg, j, b.Type)
		}
	}
	return out, nil
}

// opaqueBlock turns the JSON of a block from an earlier response back into a
// content param. It decodes the block as a response block and converts it
// with the SDK, so a thinking block keeps its text and signature unchanged
// and a block type that this SDK does not know goes back as it came.
func opaqueBlock(raw json.RawMessage) (anthropic.BetaContentBlockParamUnion, error) {
	var cb anthropic.BetaContentBlockUnion
	if len(raw) == 0 {
		return anthropic.BetaContentBlockParamUnion{}, fmt.Errorf("opaque block has no raw JSON")
	}
	if err := json.Unmarshal(raw, &cb); err != nil {
		return anthropic.BetaContentBlockParamUnion{}, fmt.Errorf("opaque block: %w", err)
	}
	if cb.Type == "" {
		return anthropic.BetaContentBlockParamUnion{}, fmt.Errorf("opaque block has no type")
	}
	return cb.ToParam(), nil
}

// imageBlock maps an image from bytes or a URL.
func imageBlock(b fw.Block) (*anthropic.BetaImageBlockParam, error) {
	var src anthropic.BetaImageBlockParamSourceUnion
	switch {
	case len(b.Data) > 0:
		if b.MediaType == "" {
			return nil, fmt.Errorf("image has data but no media type")
		}
		src.OfBase64 = &anthropic.BetaBase64ImageSourceParam{
			Data:      base64.StdEncoding.EncodeToString(b.Data),
			MediaType: anthropic.BetaBase64ImageSourceMediaType(b.MediaType),
		}
	case b.URL != "":
		src.OfURL = &anthropic.BetaURLImageSourceParam{URL: b.URL}
	default:
		return nil, fmt.Errorf("image has neither data nor a URL")
	}
	return &anthropic.BetaImageBlockParam{Source: src}, nil
}

// documentBlock maps a PDF or a plain text document from bytes or a URL.
func documentBlock(b fw.Block) (*anthropic.BetaRequestDocumentBlockParam, error) {
	var src anthropic.BetaRequestDocumentBlockSourceUnionParam
	mt := strings.ToLower(b.MediaType)
	switch {
	case len(b.Data) > 0 && (mt == "application/pdf" || mt == ""):
		src.OfBase64 = &anthropic.BetaBase64PDFSourceParam{Data: base64.StdEncoding.EncodeToString(b.Data)}
	case len(b.Data) > 0 && strings.HasPrefix(mt, "text/"):
		src.OfText = &anthropic.BetaPlainTextSourceParam{Data: string(b.Data)}
	case len(b.Data) > 0:
		return nil, fmt.Errorf("document media type %q is not supported (want application/pdf or text/*)", b.MediaType)
	case b.URL != "":
		src.OfURL = &anthropic.BetaURLPDFSourceParam{URL: b.URL}
	default:
		return nil, fmt.Errorf("document has neither data nor a URL")
	}
	return &anthropic.BetaRequestDocumentBlockParam{Source: src}, nil
}

// toolResultBlock maps a tool result with text, image, and document content.
func toolResultBlock(b fw.Block) (*anthropic.BetaToolResultBlockParam, error) {
	if b.ToolUseID == "" {
		return nil, fmt.Errorf("tool_result has no tool use ID")
	}
	tr := &anthropic.BetaToolResultBlockParam{ToolUseID: b.ToolUseID}
	if b.IsError {
		tr.IsError = param.NewOpt(true)
	}
	for _, c := range b.Content {
		switch c.Type {
		case fw.BlockText:
			if c.Text != "" {
				tr.Content = append(tr.Content, anthropic.BetaToolResultBlockParamContentUnion{
					OfText: &anthropic.BetaTextBlockParam{Text: c.Text},
				})
			}
		case fw.BlockImage:
			img, err := imageBlock(c)
			if err != nil {
				return nil, fmt.Errorf("tool_result %s: %w", b.ToolUseID, err)
			}
			tr.Content = append(tr.Content, anthropic.BetaToolResultBlockParamContentUnion{OfImage: img})
		case fw.BlockDocument:
			doc, err := documentBlock(c)
			if err != nil {
				return nil, fmt.Errorf("tool_result %s: %w", b.ToolUseID, err)
			}
			tr.Content = append(tr.Content, anthropic.BetaToolResultBlockParamContentUnion{OfDocument: doc})
		default:
			return nil, fmt.Errorf("tool_result %s: block type %q is not allowed in a tool result", b.ToolUseID, c.Type)
		}
	}
	return tr, nil
}
