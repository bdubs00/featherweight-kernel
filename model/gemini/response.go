package gemini

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"google.golang.org/genai"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// callIDPrefix starts the ID that the adapter gives a function call when the
// API sends none.
const callIDPrefix = "fwcall_"

// MapResponse maps a complete GenerateContent response to a fw.Response. It
// takes the first candidate.
//
// A text part becomes fw.BlockText and a function call becomes
// fw.BlockToolUse; names maps the wire name of a tool back to its spec name.
// A function call without an ID gets a generated one. A part that carries a
// thought signature, or a field that fw has no block for, also becomes an
// fw.BlockOpaque that holds the part JSON, placed before the text or
// tool_use block, so the next call sends the part back unchanged. A thought
// part and any other part become an fw.BlockOpaque only. Response.Raw holds
// the candidate content.
func MapResponse(resp *genai.GenerateContentResponse, names map[string]string) (*fw.Response, error) {
	if resp == nil {
		return nil, fmt.Errorf("gemini: nil response")
	}
	acc := newAccumulator(names, nil)
	if err := acc.add(resp); err != nil {
		return nil, err
	}
	return acc.response("")
}

// accumulator joins the chunks of a stream into one response. Adjacent text
// fragments of the same kind join into one part, so a streamed text becomes
// one block. A fragment that carries a thought signature closes its part.
type accumulator struct {
	names  map[string]string
	on     func(fw.Delta)
	parts  []*genai.Part
	blocks []fw.Block
	mapped int // parts[:mapped] are in blocks

	chunks    int
	model     string
	finish    genai.FinishReason
	usage     *genai.GenerateContentResponseUsageMetadata
	blockedBy string
}

func newAccumulator(names map[string]string, on func(fw.Delta)) *accumulator {
	return &accumulator{names: names, on: on}
}

// add takes one chunk.
func (a *accumulator) add(r *genai.GenerateContentResponse) error {
	if r == nil {
		return nil
	}
	a.chunks++
	if r.ModelVersion != "" {
		a.model = r.ModelVersion
	}
	if r.UsageMetadata != nil {
		a.usage = r.UsageMetadata
	}
	if f := r.PromptFeedback; f != nil && f.BlockReason != "" {
		a.blockedBy = string(f.BlockReason)
	}
	if len(r.Candidates) == 0 || r.Candidates[0] == nil {
		return nil
	}
	c := r.Candidates[0]
	if c.FinishReason != "" && c.FinishReason != genai.FinishReasonUnspecified {
		a.finish = c.FinishReason
	}
	if c.Content == nil {
		return nil
	}
	for _, p := range c.Content.Parts {
		if p == nil || isEmpty(p) {
			continue
		}
		if p.Text != "" && !p.Thought && a.on != nil {
			a.on(fw.Delta{Type: "text", Text: p.Text})
		}
		if n := len(a.parts); n > 0 && canMerge(a.parts[n-1], p) {
			last := a.parts[n-1]
			last.Text += p.Text
			last.ThoughtSignature = p.ThoughtSignature
			continue
		}
		if err := a.flush(); err != nil {
			return err
		}
		q := *p
		a.parts = append(a.parts, &q)
	}
	return nil
}

// flush maps the parts that are not mapped yet and reports their blocks.
func (a *accumulator) flush() error {
	for ; a.mapped < len(a.parts); a.mapped++ {
		bs, err := mapPart(a.parts[a.mapped], a.names)
		if err != nil {
			return err
		}
		for i := range bs {
			a.blocks = append(a.blocks, bs[i])
			if a.on != nil {
				b := bs[i]
				a.on(fw.Delta{Type: "block", Block: &b})
			}
		}
	}
	return nil
}

// response returns the joined response. model names the model when no chunk
// did.
func (a *accumulator) response(model string) (*fw.Response, error) {
	if a.chunks == 0 {
		return nil, fmt.Errorf("gemini: %s returned no response", model)
	}
	if err := a.flush(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(&genai.Content{Role: genai.RoleModel, Parts: a.parts})
	if err != nil {
		return nil, fmt.Errorf("gemini: encode candidate content: %w", err)
	}
	resp := &fw.Response{
		Content:  append(make([]fw.Block, 0, len(a.blocks)), a.blocks...),
		Raw:      raw,
		Usage:    usage(a.usage),
		Provider: Provider,
		Model:    a.model,
	}
	if resp.Model == "" {
		resp.Model = model
	}
	resp.StopReason, resp.StopDetail = a.stop()
	return resp, nil
}

// stop maps the finish reason. A prompt that the API blocked, and a response
// that a safety filter stopped, are refusals with the reason as the detail.
func (a *accumulator) stop() (fw.StopReason, string) {
	if a.blockedBy != "" && len(a.parts) == 0 {
		return fw.StopRefusal, a.blockedBy
	}
	calls := false
	for _, p := range a.parts {
		if p.FunctionCall != nil {
			calls = true
		}
	}
	switch a.finish {
	case genai.FinishReasonStop, "":
		if calls {
			return fw.StopToolUse, ""
		}
		return fw.StopEndTurn, ""
	case genai.FinishReasonMaxTokens:
		return fw.StopMaxTokens, ""
	case genai.FinishReasonSafety,
		genai.FinishReasonProhibitedContent,
		genai.FinishReasonBlocklist,
		genai.FinishReasonSPII,
		genai.FinishReasonImageSafety,
		genai.FinishReasonImageProhibitedContent:
		return fw.StopRefusal, string(a.finish)
	}
	return fw.StopOther, string(a.finish)
}

// usage maps the usage metadata. The prompt count includes cached tokens, and
// fw counts them apart, so input is the prompt minus the cache plus the tool
// results of server-side tools. Thought tokens count as output.
func usage(u *genai.GenerateContentResponseUsageMetadata) fw.Usage {
	if u == nil {
		return fw.Usage{}
	}
	in := int(u.PromptTokenCount) - int(u.CachedContentTokenCount) + int(u.ToolUsePromptTokenCount)
	return fw.Usage{
		InputTokens:     max(in, 0),
		OutputTokens:    int(u.CandidatesTokenCount) + int(u.ThoughtsTokenCount),
		CacheReadTokens: int(u.CachedContentTokenCount),
	}
}

// mapPart maps one joined part to blocks.
func mapPart(p *genai.Part, names map[string]string) ([]fw.Block, error) {
	switch {
	case p.FunctionCall != nil:
		fc := p.FunctionCall
		id := fc.ID
		if id == "" {
			id = newCallID()
		}
		name := fc.Name
		if n, ok := names[name]; ok {
			name = n
		}
		input := json.RawMessage(`{}`)
		if len(fc.Args) > 0 {
			b, err := json.Marshal(fc.Args)
			if err != nil {
				return nil, fmt.Errorf("gemini: function call %s arguments: %w", fc.Name, err)
			}
			input = b
		}
		use := fw.Block{Type: fw.BlockToolUse, ID: id, Name: name, Input: input}
		q := *p
		q.FunctionCall = nil
		if isEmpty(&q) && fc.PartialArgs == nil && fc.WillContinue == nil {
			return []fw.Block{use}, nil
		}
		op, err := opaqueBlock(p)
		return []fw.Block{op, use}, err
	case p.Text != "" && !p.Thought:
		text := fw.Text(p.Text)
		q := *p
		q.Text = ""
		if isEmpty(&q) {
			return []fw.Block{text}, nil
		}
		op, err := opaqueBlock(p)
		return []fw.Block{op, text}, err
	}
	op, err := opaqueBlock(p)
	return []fw.Block{op}, err
}

func opaqueBlock(p *genai.Part) (fw.Block, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return fw.Block{}, fmt.Errorf("gemini: encode part: %w", err)
	}
	return fw.Block{Type: fw.BlockOpaque, Provider: Provider, Raw: raw}, nil
}

// canMerge reports whether the fragment p continues the part last: both hold
// text and nothing else, both are thoughts or neither is, and last has no
// thought signature yet.
func canMerge(last, p *genai.Part) bool {
	return textOnly(last) && textOnly(p) && last.Thought == p.Thought && len(last.ThoughtSignature) == 0
}

// textOnly reports whether a part holds nothing but text, the thought flag,
// and a thought signature.
func textOnly(p *genai.Part) bool {
	q := *p
	q.Text, q.Thought, q.ThoughtSignature = "", false, nil
	return isEmpty(&q)
}

// isEmpty reports whether a part sets no field. Encoding the part checks
// every field, including fields that a later SDK adds.
func isEmpty(p *genai.Part) bool {
	b, err := json.Marshal(p)
	return err == nil && string(b) == "{}"
}

// newCallID returns a random function call ID.
func newCallID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return callIDPrefix + hex.EncodeToString(b)
}
