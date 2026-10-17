package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// MapResponse maps a complete beta message to a fw.Response.
//
// Text blocks become fw.BlockText. Tool use blocks become fw.BlockToolUse,
// and names maps the wire name of a native tool back to its spec name. Every
// other block, such as thinking, redacted thinking, tool search, compaction,
// and fallback blocks, becomes an fw.BlockOpaque that holds the block JSON,
// so the next call sends it back unchanged. Response.Raw holds the whole
// content array as the API returned it.
//
// After a refusal fallback in the middle of the output, the API wants the
// thinking, redacted thinking, and tool use blocks of the declined attempt
// left out of the next call. MapResponse leaves them out of Content, so the
// kernel neither runs those tools nor sends those blocks back. Raw keeps them.
func MapResponse(msg *anthropic.BetaMessage, names map[string]string) (*fw.Response, error) {
	if msg == nil {
		return nil, fmt.Errorf("anthropic: nil message")
	}
	resp := &fw.Response{
		StopReason: stopReason(msg.StopReason),
		Usage: fw.Usage{
			InputTokens:      int(msg.Usage.InputTokens),
			OutputTokens:     int(msg.Usage.OutputTokens),
			CacheReadTokens:  int(msg.Usage.CacheReadInputTokens),
			CacheWriteTokens: int(msg.Usage.CacheCreationInputTokens),
		},
		Provider: Provider,
		Model:    msg.Model,
	}
	if resp.StopReason == fw.StopRefusal {
		resp.StopDetail = string(msg.StopDetails.Category)
	}

	declined := declinedBlocks(msg.Content)
	var raw bytes.Buffer
	raw.WriteByte('[')
	resp.Content = make([]fw.Block, 0, len(msg.Content))
	for i, cb := range msg.Content {
		r := cb.RawJSON()
		if r == "" {
			return nil, fmt.Errorf("anthropic: content block %d (%s) has no raw JSON", i, cb.Type)
		}
		if i > 0 {
			raw.WriteByte(',')
		}
		raw.WriteString(r)
		if declined[i] {
			continue
		}
		b, err := mapBlock(cb, names)
		if err != nil {
			return nil, err
		}
		resp.Content = append(resp.Content, b)
	}
	raw.WriteByte(']')
	resp.Raw = json.RawMessage(raw.Bytes())
	return resp, nil
}

// mapBlock maps one response block.
func mapBlock(cb anthropic.BetaContentBlockUnion, names map[string]string) (fw.Block, error) {
	switch cb.Type {
	case "text":
		return fw.Block{Type: fw.BlockText, Text: cb.Text}, nil
	case "tool_use":
		name := cb.Name
		if n, ok := names[name]; ok {
			name = n
		}
		input := json.RawMessage(`{}`)
		if len(cb.Input) > 0 {
			input = append(json.RawMessage(nil), cb.Input...)
		}
		return fw.Block{Type: fw.BlockToolUse, ID: cb.ID, Name: name, Input: input}, nil
	}
	r := cb.RawJSON()
	if r == "" {
		return fw.Block{}, fmt.Errorf("anthropic: %s block has no raw JSON", cb.Type)
	}
	return fw.Block{Type: fw.BlockOpaque, Provider: Provider, Raw: json.RawMessage(r)}, nil
}

// declinedBlocks marks the blocks that the next call leaves out after a
// mid-output fallback: thinking, redacted thinking, and tool use blocks
// before the last fallback block, and server tool calls there that have no
// result before it.
func declinedBlocks(content []anthropic.BetaContentBlockUnion) map[int]bool {
	last := -1
	for i, cb := range content {
		if cb.Type == "fallback" {
			last = i
		}
	}
	if last < 0 {
		return nil
	}
	answered := map[string]bool{}
	for _, cb := range content[:last] {
		if cb.ToolUseID != "" {
			answered[cb.ToolUseID] = true
		}
	}
	out := map[int]bool{}
	for i, cb := range content[:last] {
		switch cb.Type {
		case "thinking", "redacted_thinking", "tool_use":
			out[i] = true
		case "server_tool_use", "mcp_tool_use":
			if !answered[cb.ID] {
				out[i] = true
			}
		}
	}
	return out
}

// stopReason maps a stop reason. Reasons that fw does not name map to
// fw.StopOther.
func stopReason(s anthropic.BetaStopReason) fw.StopReason {
	switch s {
	case anthropic.BetaStopReasonEndTurn:
		return fw.StopEndTurn
	case anthropic.BetaStopReasonToolUse:
		return fw.StopToolUse
	case anthropic.BetaStopReasonMaxTokens:
		return fw.StopMaxTokens
	case anthropic.BetaStopReasonRefusal:
		return fw.StopRefusal
	case anthropic.BetaStopReasonPauseTurn:
		return fw.StopPauseTurn
	}
	return fw.StopOther
}
