package anthropic

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// message builds a response with the SDK's own decoder.
func message(t *testing.T, stopReason, stopDetails, content string) *anthropic.BetaMessage {
	t.Helper()
	if stopDetails == "" {
		stopDetails = "null"
	}
	body := `{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5",
		"stop_reason":"` + stopReason + `","stop_details":` + stopDetails + `,"stop_sequence":null,
		"content":` + content + `,
		"usage":{"input_tokens":12,"output_tokens":34,"cache_read_input_tokens":560,"cache_creation_input_tokens":78}}`
	var msg anthropic.BetaMessage
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatalf("decode message: %v", err)
	}
	return &msg
}

func TestMapResponse(t *testing.T) {
	thinking := `{"type":"thinking","thinking":"Check the file first.","signature":"EqQBCkYIBxgCKkBsig=="}`
	redacted := `{"type":"redacted_thinking","data":"EmwKAhgBEgy3va3pzix/LafPsn4aDFIT"}`
	content := `[` + thinking + `,` + redacted + `,
		{"type":"text","text":"Reading a.go now."},
		{"type":"tool_use","id":"toolu_01","name":"str_replace_based_edit_tool","input":{"command":"view","path":"/a.go"}},
		{"type":"tool_use","id":"toolu_02","name":"http","input":{"url":"https://example.com"}}]`
	msg := message(t, "tool_use", "", content)

	resp, err := MapResponse(msg, map[string]string{"str_replace_based_edit_tool": "files", "bash": "bash"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Provider != "anthropic" || resp.Model != "claude-opus-5-5" {
		t.Errorf("provider, model = %q, %q", resp.Provider, resp.Model)
	}
	if resp.StopReason != fw.StopToolUse || resp.StopDetail != "" {
		t.Errorf("stop = %q %q, want tool_use", resp.StopReason, resp.StopDetail)
	}
	wantUsage := fw.Usage{InputTokens: 12, OutputTokens: 34, CacheReadTokens: 560, CacheWriteTokens: 78}
	if resp.Usage != wantUsage {
		t.Errorf("usage = %+v, want %+v", resp.Usage, wantUsage)
	}
	if len(resp.Content) != 5 {
		t.Fatalf("got %d blocks, want 5: %+v", len(resp.Content), resp.Content)
	}

	for i, want := range []string{thinking, redacted} {
		b := resp.Content[i]
		if b.Type != fw.BlockOpaque || b.Provider != "anthropic" {
			t.Errorf("block %d = %s/%s, want opaque/anthropic", i, b.Type, b.Provider)
		}
		if !reflect.DeepEqual(decode(t, string(b.Raw)), decode(t, want)) {
			t.Errorf("block %d raw = %s, want %s", i, b.Raw, want)
		}
	}
	if b := resp.Content[2]; b.Type != fw.BlockText || b.Text != "Reading a.go now." {
		t.Errorf("text block = %+v", b)
	}
	files := resp.Content[3]
	if files.Type != fw.BlockToolUse || files.ID != "toolu_01" || files.Name != "files" {
		t.Errorf("native tool_use = %+v, want the name rewritten to files", files)
	}
	if !reflect.DeepEqual(decode(t, string(files.Input)), decode(t, `{"command":"view","path":"/a.go"}`)) {
		t.Errorf("native tool_use input = %s", files.Input)
	}
	if http := resp.Content[4]; http.Name != "http" || http.ID != "toolu_02" {
		t.Errorf("custom tool_use = %+v, want the name unchanged", http)
	}
	if uses := resp.ToolUses(); len(uses) != 2 {
		t.Errorf("ToolUses = %d, want 2", len(uses))
	}
	if !reflect.DeepEqual(decode(t, string(resp.Raw)), decode(t, content)) {
		t.Errorf("raw = %s, want the content array unchanged", resp.Raw)
	}
}

func TestMapResponseStopReasons(t *testing.T) {
	cases := []struct {
		reason, details string
		want            fw.StopReason
		detail          string
	}{
		{"end_turn", "", fw.StopEndTurn, ""},
		{"tool_use", "", fw.StopToolUse, ""},
		{"max_tokens", "", fw.StopMaxTokens, ""},
		{"pause_turn", "", fw.StopPauseTurn, ""},
		{"refusal", `{"type":"refusal","category":"cyber","explanation":"declined"}`, fw.StopRefusal, "cyber"},
		{"refusal", `{"type":"refusal","category":null,"explanation":null}`, fw.StopRefusal, ""},
		{"stop_sequence", "", fw.StopOther, ""},
		{"compaction", "", fw.StopOther, ""},
		{"model_context_window_exceeded", "", fw.StopOther, ""},
	}
	for _, c := range cases {
		resp, err := MapResponse(message(t, c.reason, c.details, `[]`), nil)
		if err != nil {
			t.Fatalf("%s: %v", c.reason, err)
		}
		if resp.StopReason != c.want || resp.StopDetail != c.detail {
			t.Errorf("%s: got %q %q, want %q %q", c.reason, resp.StopReason, resp.StopDetail, c.want, c.detail)
		}
		if string(resp.Raw) != "[]" || len(resp.Content) != 0 {
			t.Errorf("%s: empty content gave raw %s and %d blocks", c.reason, resp.Raw, len(resp.Content))
		}
	}
}

func TestMapResponseOtherBlocksStayOpaque(t *testing.T) {
	content := `[
		{"type":"server_tool_use","id":"srvtoolu_01","name":"tool_search_tool_bm25","input":{"query":"weather"}},
		{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_01","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"get_weather"}]}},
		{"type":"some_future_block","payload":{"k":1}},
		{"type":"tool_use","id":"toolu_01","name":"get_weather","input":{"city":"Paris"}}
	]`
	resp, err := MapResponse(message(t, "tool_use", "", content), nil)
	if err != nil {
		t.Fatal(err)
	}
	wantTypes := []string{fw.BlockOpaque, fw.BlockOpaque, fw.BlockOpaque, fw.BlockToolUse}
	for i, b := range resp.Content {
		if b.Type != wantTypes[i] {
			t.Errorf("block %d type = %s, want %s", i, b.Type, wantTypes[i])
		}
	}

	// The opaque blocks go back to the API as they came.
	req := &fw.Request{Messages: []fw.Message{fw.UserText("weather?"), {Role: fw.RoleAssistant, Content: resp.Content}}}
	p, _, err := BuildParams(req, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	got := msgsOf(t, wire(t, p))[1]["content"]
	if !reflect.DeepEqual(got, decode(t, content)) {
		gb, _ := json.Marshal(got)
		t.Errorf("echoed content =\n%s\nwant\n%s", gb, content)
	}
}

func TestMapResponseMidOutputFallback(t *testing.T) {
	// The first model thought, started a tool call, and declined. The
	// fallback model continued after the fallback block.
	content := `[
		{"type":"thinking","thinking":"","signature":"c2lnMQ=="},
		{"type":"text","text":"Partial answer. "},
		{"type":"tool_use","id":"toolu_01","name":"bash","input":{}},
		{"type":"server_tool_use","id":"srvtoolu_01","name":"tool_search_tool_bm25","input":{"query":"x"}},
		{"type":"fallback","from":{"model":"claude-opus-5-5"},"to":{"model":"claude-opus-4-8"}},
		{"type":"thinking","thinking":"","signature":"c2lnMg=="},
		{"type":"tool_use","id":"toolu_02","name":"bash","input":{"command":"ls"}}
	]`
	resp, err := MapResponse(message(t, "tool_use", "", content), nil)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, b := range resp.Content {
		k := b.Type
		if b.Type == fw.BlockOpaque {
			var head struct{ Type string }
			_ = json.Unmarshal(b.Raw, &head)
			k = "opaque:" + head.Type
		}
		kinds = append(kinds, k)
	}
	want := []string{"text", "opaque:fallback", "opaque:thinking", "tool_use"}
	if !reflect.DeepEqual(kinds, want) {
		t.Errorf("content = %v, want %v", kinds, want)
	}
	if uses := resp.ToolUses(); len(uses) != 1 || uses[0].ID != "toolu_02" {
		t.Errorf("tool uses = %+v, want only toolu_02", uses)
	}
	var raw []any
	if err := json.Unmarshal(resp.Raw, &raw); err != nil || len(raw) != 7 {
		t.Errorf("raw keeps %d blocks (err %v), want all 7", len(raw), err)
	}
}

func TestMapResponseNil(t *testing.T) {
	if _, err := MapResponse(nil, nil); err == nil {
		t.Errorf("nil message: want an error")
	}
}
