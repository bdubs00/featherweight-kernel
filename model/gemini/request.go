package gemini

import (
	"encoding/json"
	"fmt"
	"math"
	"mime"
	"net/url"
	"path"
	"strconv"
	"strings"

	"google.golang.org/genai"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// maxNameLen is the longest function name that the API accepts.
const maxNameLen = 64

// BuildRequest maps a request to the contents and the configuration of a
// GenerateContent call.
//
// It returns a map from the wire name of each tool to the name in its
// fw.ToolSpec, for the tools whose names differ. MapResponse uses the map to
// rename function calls.
//
// An empty model selects DefaultModel. The model name decides how the effort
// maps to the thinking configuration.
func BuildRequest(req *fw.Request, model string) ([]*genai.Content, *genai.GenerateContentConfig, map[string]string, error) {
	if req == nil {
		return nil, nil, nil, fmt.Errorf("gemini: nil request")
	}
	if len(req.Messages) == 0 {
		return nil, nil, nil, fmt.Errorf("gemini: request has no messages")
	}
	if model == "" {
		model = DefaultModel
	}
	thinking, err := thinkingConfig(model, req.Effort)
	if err != nil {
		return nil, nil, nil, err
	}
	tools, names, err := buildTools(req.Tools)
	if err != nil {
		return nil, nil, nil, err
	}
	toWire := make(map[string]string, len(names))
	for wire, spec := range names {
		toWire[spec] = wire
	}
	contents, err := buildContents(req.Messages, toWire)
	if err != nil {
		return nil, nil, nil, err
	}
	cfg := &genai.GenerateContentConfig{Tools: tools, ThinkingConfig: thinking}
	if req.MaxTokens > 0 {
		cfg.MaxOutputTokens = int32(min(req.MaxTokens, math.MaxInt32))
	}
	if req.System != "" {
		cfg.SystemInstruction = &genai.Content{Parts: []*genai.Part{{Text: req.System}}}
	}
	return contents, cfg, names, nil
}

// checkEffort validates an effort level. Empty means the model default.
func checkEffort(effort string) error {
	switch effort {
	case "", "low", "medium", "high", "xhigh", "max":
		return nil
	}
	return fmt.Errorf("gemini: unknown effort %q (want low, medium, high, xhigh, or max)", effort)
}

// thinkingConfig maps an effort level to the thinking configuration of the
// model family. Gemini 3 and later take a thinking level, and the SDK names
// no level above HIGH, so xhigh and max map to HIGH. Gemini 2.5 takes a token
// budget. Older models and models outside the Gemini family get no thinking
// configuration. An empty effort leaves thinking at the model default.
func thinkingConfig(model, effort string) (*genai.ThinkingConfig, error) {
	if err := checkEffort(effort); err != nil {
		return nil, err
	}
	if effort == "" {
		return nil, nil
	}
	base := strings.TrimPrefix(model, "models/")
	switch {
	case !strings.HasPrefix(base, "gemini-"),
		strings.HasPrefix(base, "gemini-1"),
		strings.HasPrefix(base, "gemini-2.0"):
		return nil, nil
	case strings.HasPrefix(base, "gemini-2"):
		budget := map[string]int32{"low": 1024, "medium": 8192, "high": 24576, "xhigh": 24576, "max": 24576}[effort]
		return &genai.ThinkingConfig{ThinkingBudget: genai.Ptr(budget)}, nil
	}
	level := map[string]genai.ThinkingLevel{
		"low":    genai.ThinkingLevelLow,
		"medium": genai.ThinkingLevelMedium,
		"high":   genai.ThinkingLevelHigh,
		"xhigh":  genai.ThinkingLevelHigh,
		"max":    genai.ThinkingLevelHigh,
	}[effort]
	return &genai.ThinkingConfig{ThinkingLevel: level}, nil
}

// buildTools maps tool specs to function declarations in the order given. It
// returns the tools and the map from wire name to spec name. Native shapes
// are an Anthropic feature, so a native tool goes out with its JSON schema.
func buildTools(specs []fw.ToolSpec) ([]*genai.Tool, map[string]string, error) {
	names := map[string]string{}
	if len(specs) == 0 {
		return nil, names, nil
	}
	wire := make(map[string]string, len(specs)) // wire name to spec name, to find collisions
	decls := make([]*genai.FunctionDeclaration, 0, len(specs))
	for _, s := range specs {
		if s.Name == "" {
			return nil, nil, fmt.Errorf("gemini: tool spec has no name")
		}
		w := wireNameFor(s.Name, wire)
		if prev, ok := wire[w]; ok {
			return nil, nil, fmt.Errorf("gemini: tools %q and %q both reach the model as %q", prev, s.Name, w)
		}
		wire[w] = s.Name
		if w != s.Name {
			names[w] = s.Name
		}
		params, err := parameters(s)
		if err != nil {
			return nil, nil, err
		}
		decls = append(decls, &genai.FunctionDeclaration{Name: w, Description: s.Description, Parameters: params})
	}
	return []*genai.Tool{{FunctionDeclarations: decls}}, names, nil
}

// parameters converts the input schema of a tool. An object with no
// properties becomes no parameters, because the API rejects an OBJECT schema
// with an empty property list.
func parameters(s fw.ToolSpec) (*genai.Schema, error) {
	schema, err := ConvertSchema(s.InputSchema)
	if err != nil {
		return nil, fmt.Errorf("gemini: tool %q: %w", s.Name, err)
	}
	if schema == nil {
		return nil, nil
	}
	if schema.Type != genai.TypeObject {
		return nil, fmt.Errorf("gemini: tool %q: input schema has type %s, want object", s.Name, schema.Type)
	}
	if len(schema.Properties) == 0 {
		return nil, nil
	}
	return schema, nil
}

// sanitize returns a function name the API accepts: it starts with a letter
// or an underscore, holds only [A-Za-z0-9_.:-], and has at most 64
// characters. Any other character becomes "_".
func sanitize(name string) string {
	var sb strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == ':', r == '-':
			sb.WriteRune(r)
		default:
			sb.WriteByte('_')
		}
	}
	w := sb.String()
	if w == "" {
		w = "tool"
	}
	if c := w[0]; !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_') {
		w = "_" + w
	}
	if len(w) > maxNameLen {
		w = w[:maxNameLen]
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
		if len(base)+len(suffix) > maxNameLen {
			base = base[:maxNameLen-len(suffix)]
		}
		w = base + suffix
	}
}

// buildContents maps the conversation. toWire maps a spec name to its wire
// name. Gemini keys a function response by the function name, so the names
// of the tool_use blocks in the history give each tool_result its name.
//
// The API takes only the roles user and model. A system message becomes a
// user part in a <system-reminder> block. Adjacent contents of one role
// merge, and in a user content the function responses come first.
func buildContents(in []fw.Message, toWire map[string]string) ([]*genai.Content, error) {
	wireName := func(name string) string {
		if w, ok := toWire[name]; ok {
			return w
		}
		return sanitize(name)
	}
	calls := map[string]string{} // tool_use ID to wire name
	for _, m := range in {
		if m.Role != fw.RoleAssistant {
			continue
		}
		for _, b := range m.Content {
			if b.Type == fw.BlockToolUse {
				calls[b.ID] = wireName(b.Name)
			}
		}
	}

	var out []*genai.Content
	add := func(role string, parts []*genai.Part) {
		if len(parts) == 0 {
			return
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Parts = append(out[n-1].Parts, parts...)
			return
		}
		out = append(out, &genai.Content{Role: role, Parts: parts})
	}
	for i, m := range in {
		switch m.Role {
		case fw.RoleSystem:
			if text := fw.TextOf(m.Content); text != "" {
				add(genai.RoleUser, []*genai.Part{{Text: "<system-reminder>\n" + text + "\n</system-reminder>"}})
			}
		case fw.RoleUser:
			parts, err := userParts(m.Content, i, calls)
			if err != nil {
				return nil, err
			}
			add(genai.RoleUser, parts)
		case fw.RoleAssistant:
			parts, err := modelParts(m.Content, i, wireName)
			if err != nil {
				return nil, err
			}
			add(genai.RoleModel, parts)
		default:
			return nil, fmt.Errorf("gemini: message %d has unknown role %q", i, m.Role)
		}
	}
	for _, c := range out {
		if c.Role == genai.RoleUser {
			responsesFirst(c.Parts)
		}
	}
	return out, nil
}

// responsesFirst moves the function responses to the front, in order.
func responsesFirst(parts []*genai.Part) {
	responses := make([]*genai.Part, 0, len(parts))
	others := make([]*genai.Part, 0, len(parts))
	for _, p := range parts {
		if p.FunctionResponse != nil {
			responses = append(responses, p)
		} else {
			others = append(others, p)
		}
	}
	copy(parts, append(responses, others...))
}

// userParts maps the blocks of a user message.
func userParts(in []fw.Block, msg int, calls map[string]string) ([]*genai.Part, error) {
	out := make([]*genai.Part, 0, len(in))
	for j, b := range in {
		var (
			p   *genai.Part
			err error
		)
		switch b.Type {
		case fw.BlockText:
			if b.Text == "" {
				continue
			}
			p = &genai.Part{Text: b.Text}
		case fw.BlockImage, fw.BlockDocument:
			p, err = mediaPart(b)
		case fw.BlockToolResult:
			p, err = responsePart(b, calls)
		case fw.BlockOpaque:
			if b.Provider != Provider {
				continue
			}
			p, err = opaquePart(b.Raw)
		default:
			err = fmt.Errorf("block type %q is not allowed in a user message", b.Type)
		}
		if err != nil {
			return nil, fmt.Errorf("gemini: message %d block %d: %w", msg, j, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// modelParts maps the blocks of an assistant message. An opaque block that
// holds a function call or a text part carries its thought signature: it
// goes back as it came, and the tool_use or text block after it, which holds
// the same call or text, is skipped. Such an opaque block without that block
// after it is dropped, so a call never goes out twice.
func modelParts(in []fw.Block, msg int, wireName func(string) string) ([]*genai.Part, error) {
	out := make([]*genai.Part, 0, len(in))
	for j := 0; j < len(in); j++ {
		b := in[j]
		switch b.Type {
		case fw.BlockText:
			if b.Text != "" {
				out = append(out, &genai.Part{Text: b.Text})
			}
		case fw.BlockToolUse:
			out = append(out, &genai.Part{FunctionCall: &genai.FunctionCall{
				ID:   wireID(b.ID),
				Name: wireName(b.Name),
				Args: args(b.Input),
			}})
		case fw.BlockOpaque:
			if b.Provider != Provider {
				continue
			}
			p, err := opaquePart(b.Raw)
			if err != nil {
				return nil, fmt.Errorf("gemini: message %d block %d: %w", msg, j, err)
			}
			if carrier(p) {
				if j+1 >= len(in) || !twin(p, in[j+1], wireName) {
					continue
				}
				j++
			}
			out = append(out, p)
		default:
			return nil, fmt.Errorf("gemini: message %d block %d: block type %q is not allowed in an assistant message", msg, j, b.Type)
		}
	}
	return out, nil
}

// carrier reports whether a part from an opaque block also appears as a
// tool_use or text block.
func carrier(p *genai.Part) bool {
	return p.FunctionCall != nil || (p.Text != "" && !p.Thought)
}

// twin reports whether b holds the same call or text as the part p.
func twin(p *genai.Part, b fw.Block, wireName func(string) string) bool {
	if fc := p.FunctionCall; fc != nil {
		return b.Type == fw.BlockToolUse && wireName(b.Name) == fc.Name && (fc.ID == "" || fc.ID == b.ID)
	}
	return b.Type == fw.BlockText && b.Text == p.Text
}

// args parses the input of a tool_use block. Input that is not a JSON object,
// such as a call cut off at max_tokens, goes out with no arguments.
func args(input json.RawMessage) map[string]any {
	var m map[string]any
	if len(input) == 0 || json.Unmarshal(input, &m) != nil {
		return nil
	}
	return m
}

// wireID returns the call ID to send. An ID that the adapter generated stays
// local, because the API did not issue it.
func wireID(id string) string {
	if strings.HasPrefix(id, callIDPrefix) {
		return ""
	}
	return id
}

// opaquePart turns the JSON of a part from an earlier response back into a
// part.
func opaquePart(raw json.RawMessage) (*genai.Part, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("opaque block has no raw JSON")
	}
	var p genai.Part
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("opaque block: %w", err)
	}
	if isEmpty(&p) {
		return nil, fmt.Errorf("opaque block holds an empty part")
	}
	return &p, nil
}

// mediaPart maps an image or a document from bytes or a URL. A document
// without a media type is a PDF.
func mediaPart(b fw.Block) (*genai.Part, error) {
	mt := b.MediaType
	switch {
	case len(b.Data) > 0:
		if mt == "" && b.Type == fw.BlockDocument {
			mt = "application/pdf"
		}
		if mt == "" {
			return nil, fmt.Errorf("%s has data but no media type", b.Type)
		}
		return &genai.Part{InlineData: &genai.Blob{MIMEType: mt, Data: b.Data}}, nil
	case b.URL != "":
		if mt == "" {
			mt = mimeOfURL(b.URL)
		}
		if mt == "" && b.Type == fw.BlockDocument {
			mt = "application/pdf"
		}
		if mt == "" {
			return nil, fmt.Errorf("%s URL has no media type, and its extension names none", b.Type)
		}
		return &genai.Part{FileData: &genai.FileData{FileURI: b.URL, MIMEType: mt}}, nil
	}
	return nil, fmt.Errorf("%s has neither data nor a URL", b.Type)
}

// mimeOfURL guesses a media type from the extension of a URL path.
func mimeOfURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	mt := mime.TypeByExtension(path.Ext(u.Path))
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = mt[:i]
	}
	return mt
}

// responsePart maps a tool result to a function response. The text goes
// under "output", or under "error" when the result is an error. Images and
// documents with data go in the parts of the response. A media URL goes into
// the text, because the Gemini API takes no file URI in a function response.
func responsePart(b fw.Block, calls map[string]string) (*genai.Part, error) {
	if b.ToolUseID == "" {
		return nil, fmt.Errorf("tool_result has no tool use ID")
	}
	name, ok := calls[b.ToolUseID]
	if !ok {
		return nil, fmt.Errorf("tool_result %s has no tool_use block in the history, so its function name is unknown", b.ToolUseID)
	}
	var (
		texts []string
		media []*genai.FunctionResponsePart
	)
	for _, c := range b.Content {
		switch c.Type {
		case fw.BlockText:
			if c.Text != "" {
				texts = append(texts, c.Text)
			}
		case fw.BlockImage, fw.BlockDocument:
			switch {
			case len(c.Data) > 0:
				mt := c.MediaType
				if mt == "" && c.Type == fw.BlockDocument {
					mt = "application/pdf"
				}
				if mt == "" {
					return nil, fmt.Errorf("tool_result %s: %s has data but no media type", b.ToolUseID, c.Type)
				}
				media = append(media, genai.NewFunctionResponsePartFromBytes(c.Data, mt))
			case c.URL != "":
				texts = append(texts, c.Type+": "+c.URL)
			default:
				return nil, fmt.Errorf("tool_result %s: %s has neither data nor a URL", b.ToolUseID, c.Type)
			}
		default:
			return nil, fmt.Errorf("tool_result %s: block type %q is not allowed in a tool result", b.ToolUseID, c.Type)
		}
	}
	key := "output"
	if b.IsError {
		key = "error"
	}
	return &genai.Part{FunctionResponse: &genai.FunctionResponse{
		ID:       wireID(b.ToolUseID),
		Name:     name,
		Response: map[string]any{key: strings.Join(texts, "\n")},
		Parts:    media,
	}}, nil
}
