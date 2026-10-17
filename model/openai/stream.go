package openai

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// chunk is one streamed chat completion chunk, or a whole completion from a
// server that does not stream.
type chunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Delta        delta   `json:"delta"`
		Message      *delta  `json:"message"`
		FinishReason lenient `json:"finish_reason"`
	} `json:"choices"`
	Usage *usage          `json:"usage"`
	Error json.RawMessage `json:"error"`
}

// usage is the token count of a completion. OpenAI counts the reasoning
// tokens inside completion_tokens and reports them again in the details.
type usage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// toUsage maps the count. The cached tokens leave the input count, as the
// cache read count holds them. The reasoning tokens stay inside the output
// count, where OpenAI puts them. A server that reports more reasoning
// tokens than completion tokens counts them apart, so they are added.
func (u *usage) toUsage() fw.Usage {
	cached := 0
	if u.PromptTokensDetails != nil {
		cached = u.PromptTokensDetails.CachedTokens
	}
	output := u.CompletionTokens
	if u.CompletionTokensDetails != nil && u.CompletionTokensDetails.ReasoningTokens > output {
		output += u.CompletionTokensDetails.ReasoningTokens
	}
	return fw.Usage{
		InputTokens:     max(u.PromptTokens-cached, 0),
		OutputTokens:    output,
		CacheReadTokens: cached,
	}
}

type delta struct {
	Content          lenient `json:"content"`
	Refusal          lenient `json:"refusal"`
	ReasoningContent lenient `json:"reasoning_content"`
	Reasoning        lenient `json:"reasoning"`
	ToolCalls        []struct {
		Index    *int   `json:"index"`
		ID       string `json:"id"`
		Function struct {
			Name      lenient `json:"name"`
			Arguments lenient `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

// lenient is a string field that reads null, or a value of another type, as
// empty. Servers differ in what they put in optional fields.
type lenient string

func (l *lenient) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*l = lenient(s)
	}
	return nil
}

// accumulator joins the chunks of a stream into one response.
type accumulator struct {
	names map[string]string
	on    func(fw.Delta)

	text         strings.Builder
	refusal      strings.Builder
	reasoning    strings.Builder
	reasoningKey string // the field that carried the reasoning
	calls        []*callState
	slot         map[int]int // stream index to position in calls

	model  string
	finish string
	done   bool // the stream sent [DONE] or a finish reason
	usage  fw.Usage
}

type callState struct {
	id, name string
	args     strings.Builder
}

func newAccumulator(names map[string]string, on func(fw.Delta)) *accumulator {
	return &accumulator{names: names, on: on, slot: map[int]int{}}
}

// readSSE reads server-sent events until [DONE] or the end of the body. Data
// lines of one event join with a newline.
func (a *accumulator) readSSE(r io.Reader) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var data []string
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if len(data) > 0 {
				stop, derr := a.event(strings.Join(data, "\n"))
				data = data[:0]
				if derr != nil || stop {
					return derr
				}
			}
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
		// Comments (":") and the fields event, id, and retry carry nothing
		// that the adapter uses.
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	if len(data) > 0 {
		if _, err := a.event(strings.Join(data, "\n")); err != nil {
			return err
		}
	}
	if !a.done {
		return errors.New("the stream ended before a finish reason")
	}
	return nil
}

// event takes the data of one event. It reports true at [DONE].
func (a *accumulator) event(data string) (bool, error) {
	data = strings.TrimSpace(data)
	if data == "" {
		return false, nil
	}
	if data == "[DONE]" {
		a.done = true
		return true, nil
	}
	var c chunk
	if err := json.Unmarshal([]byte(data), &c); err != nil {
		return false, fmt.Errorf("decode chunk: %w", err)
	}
	return false, a.add(&c, false)
}

// readJSON reads one completion that did not stream.
func (a *accumulator) readJSON(r io.Reader) error {
	var c chunk
	if err := json.NewDecoder(r).Decode(&c); err != nil {
		return fmt.Errorf("decode completion: %w", err)
	}
	if err := a.add(&c, true); err != nil {
		return err
	}
	a.done = true
	return nil
}

// add takes one chunk. whole marks a completion that did not stream, whose
// tool calls carry no index.
func (a *accumulator) add(c *chunk, whole bool) error {
	if len(c.Error) > 0 && string(c.Error) != "null" {
		var obj struct {
			Message string `json:"message"`
		}
		msg := string(c.Error)
		if json.Unmarshal(c.Error, &obj) == nil && obj.Message != "" {
			msg = obj.Message
		}
		return fmt.Errorf("server error in stream: %s", msg)
	}
	if c.Model != "" {
		a.model = c.Model
	}
	if c.Usage != nil {
		a.usage = c.Usage.toUsage()
	}
	if len(c.Choices) == 0 {
		return nil
	}
	ch := c.Choices[0]
	d := &ch.Delta
	if ch.Message != nil {
		d = ch.Message
	}
	if d.Content != "" {
		a.text.WriteString(string(d.Content))
		if a.on != nil {
			a.on(fw.Delta{Type: "text", Text: string(d.Content)})
		}
	}
	a.refusal.WriteString(string(d.Refusal))
	switch {
	case d.ReasoningContent != "":
		a.addReasoning("reasoning_content", string(d.ReasoningContent))
	case d.Reasoning != "":
		a.addReasoning("reasoning", string(d.Reasoning))
	}
	for i, tc := range d.ToolCalls {
		idx := i
		if tc.Index != nil && !whole {
			idx = *tc.Index
		}
		pos, ok := a.slot[idx]
		if !ok || (tc.ID != "" && a.calls[pos].id != "" && a.calls[pos].id != tc.ID) {
			// A new call, or a server that reuses an index for the next call.
			pos = len(a.calls)
			a.calls = append(a.calls, &callState{})
			a.slot[idx] = pos
		}
		cs := a.calls[pos]
		if tc.ID != "" {
			cs.id = tc.ID
		}
		if cs.name == "" {
			cs.name = string(tc.Function.Name)
		}
		cs.args.WriteString(string(tc.Function.Arguments))
	}
	if ch.FinishReason != "" {
		a.finish = string(ch.FinishReason)
		a.done = true
	}
	return nil
}

func (a *accumulator) addReasoning(key, text string) {
	if a.reasoningKey == "" {
		a.reasoningKey = key
	}
	a.reasoning.WriteString(text)
}

// response returns the joined response: the reasoning as an opaque block,
// then the text, then the tool calls in stream order. Raw holds the
// assistant message as the next call sends it.
func (a *accumulator) response(model string) (*fw.Response, error) {
	resp := &fw.Response{Provider: Provider, Model: a.model, Usage: a.usage}
	if resp.Model == "" {
		resp.Model = model
	}
	raw := message{Role: "assistant", Content: a.text.String()}
	if a.reasoning.Len() > 0 {
		v, _ := json.Marshal(a.reasoning.String())
		raw.Extra = map[string]json.RawMessage{a.reasoningKey: v}
		r, _ := json.Marshal(raw.Extra)
		resp.Content = append(resp.Content, fw.Block{Type: fw.BlockOpaque, Provider: Provider, Raw: r})
	}
	text := a.text.String()
	if a.refusal.Len() > 0 {
		text += a.refusal.String()
	}
	if text != "" {
		resp.Content = append(resp.Content, fw.Text(text))
	}
	for _, c := range a.calls {
		if c.id == "" {
			c.id = newCallID()
		}
		name := c.name
		if n, ok := a.names[name]; ok {
			name = n
		}
		args := strings.TrimSpace(c.args.String())
		if args == "" {
			args = "{}"
		}
		input := json.RawMessage(args)
		if !json.Valid(input) {
			// Arguments cut off at max_tokens. The block keeps them as a
			// JSON string, so the response stays valid JSON.
			input, _ = json.Marshal(args)
		}
		resp.Content = append(resp.Content, fw.Block{Type: fw.BlockToolUse, ID: c.id, Name: name, Input: input})
		raw.ToolCalls = append(raw.ToolCalls, toolCall{ID: c.id, Type: "function", Function: functionCall{Name: c.name, Arguments: args}})
	}
	if resp.Content == nil {
		resp.Content = []fw.Block{}
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("openai: encode assistant message: %w", err)
	}
	resp.Raw = b
	resp.StopReason, resp.StopDetail = a.stop()
	if a.on != nil {
		for i := range resp.Content {
			blk := resp.Content[i]
			a.on(fw.Delta{Type: "block", Block: &blk})
		}
	}
	return resp, nil
}

// stop maps the finish reason. Some servers end a turn with tool calls with
// "stop", so tool calls decide over "stop".
func (a *accumulator) stop() (fw.StopReason, string) {
	switch a.finish {
	case "stop", "tool_calls", "function_call", "":
		switch {
		case len(a.calls) > 0:
			return fw.StopToolUse, ""
		case a.refusal.Len() > 0:
			return fw.StopRefusal, "refusal"
		}
		return fw.StopEndTurn, ""
	case "length":
		return fw.StopMaxTokens, ""
	case "content_filter":
		return fw.StopRefusal, "content_filter"
	}
	return fw.StopOther, a.finish
}

// newCallID returns a random tool call ID for a server that sends none.
func newCallID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "call_" + hex.EncodeToString(b)
}
