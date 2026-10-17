package anthropic

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// wire marshals params and decodes the JSON into generic values, so tests
// assert on what the API receives.
func wire(t *testing.T, p anthropic.BetaMessageNewParams) map[string]any {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	return out
}

// decode decodes a JSON text into generic values.
func decode(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("decode %s: %v", s, err)
	}
	return v
}

func mustBuild(t *testing.T, req *fw.Request, threshold int) (anthropic.BetaMessageNewParams, map[string]string) {
	t.Helper()
	p, names, err := BuildParams(req, "", threshold)
	if err != nil {
		t.Fatalf("BuildParams: %v", err)
	}
	return p, names
}

func msgsOf(t *testing.T, w map[string]any) []map[string]any {
	t.Helper()
	raw, _ := w["messages"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, m := range raw {
		out = append(out, m.(map[string]any))
	}
	return out
}

func TestBuildParamsDefaults(t *testing.T) {
	p, _ := mustBuild(t, &fw.Request{System: "You are terse.", Messages: []fw.Message{fw.UserText("hi")}}, 0)
	w := wire(t, p)

	if w["model"] != DefaultModel {
		t.Errorf("model = %v, want %s", w["model"], DefaultModel)
	}
	if w["max_tokens"] != float64(DefaultMaxTokens) {
		t.Errorf("max_tokens = %v, want %d", w["max_tokens"], DefaultMaxTokens)
	}
	if _, ok := w["thinking"]; ok {
		t.Errorf("thinking is set; the adapter sends no thinking parameter")
	}
	oc, _ := w["output_config"].(map[string]any)
	if oc["effort"] != "high" {
		t.Errorf("output_config.effort = %v, want high", oc["effort"])
	}
	sys, _ := w["system"].([]any)
	if len(sys) != 1 {
		t.Fatalf("system = %v, want one block", w["system"])
	}
	last := sys[len(sys)-1].(map[string]any)
	if last["text"] != "You are terse." || last["type"] != "text" {
		t.Errorf("system block = %v", last)
	}
	if cc, _ := last["cache_control"].(map[string]any); cc["type"] != "ephemeral" {
		t.Errorf("last system block cache_control = %v, want ephemeral", last["cache_control"])
	}
	if cc, _ := w["cache_control"].(map[string]any); cc["type"] != "ephemeral" {
		t.Errorf("top-level cache_control = %v, want ephemeral", w["cache_control"])
	}
	msgs := msgsOf(t, w)
	if len(msgs) != 1 || msgs[0]["role"] != "user" {
		t.Errorf("messages = %v", msgs)
	}
}

func TestBuildParamsEffortAndMaxTokens(t *testing.T) {
	for _, e := range []string{"low", "medium", "high", "xhigh", "max"} {
		p, _ := mustBuild(t, &fw.Request{Effort: e, MaxTokens: 1234, Messages: []fw.Message{fw.UserText("hi")}}, 0)
		w := wire(t, p)
		if got := w["output_config"].(map[string]any)["effort"]; got != e {
			t.Errorf("effort %s: got %v", e, got)
		}
		if w["max_tokens"] != float64(1234) {
			t.Errorf("max_tokens = %v, want 1234", w["max_tokens"])
		}
	}
	if _, _, err := BuildParams(&fw.Request{Effort: "huge", Messages: []fw.Message{fw.UserText("hi")}}, "", 0); err == nil {
		t.Errorf("unknown effort: want an error")
	}
}

func TestBuildParamsNativeTools(t *testing.T) {
	req := &fw.Request{
		Messages: []fw.Message{fw.UserText("hi")},
		Tools: []fw.ToolSpec{
			{Name: "bash", Native: fw.NativeBash, Description: "ignored for native tools"},
			{Name: "files", Native: fw.NativeTextEditor},
			{Name: "http", Description: "Fetch a URL.", InputSchema: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"}},"required":["url"]}`)},
			{Name: "memory", Native: fw.NativeMemory},
		},
	}
	p, names := mustBuild(t, req, 0)

	wantNames := map[string]string{"bash": "bash", "str_replace_based_edit_tool": "files", "memory": "memory"}
	if !reflect.DeepEqual(names, wantNames) {
		t.Errorf("names = %v, want %v", names, wantNames)
	}
	got := wire(t, p)["tools"]
	want := decode(t, `[
		{"type":"bash_20250124","name":"bash"},
		{"type":"text_editor_20250728","name":"str_replace_based_edit_tool"},
		{"name":"http","description":"Fetch a URL.","input_schema":{"type":"object","properties":{"url":{"type":"string"}},"required":["url"]}},
		{"type":"memory_20250818","name":"memory"}
	]`)
	if !reflect.DeepEqual(got, want) {
		gb, _ := json.Marshal(got)
		t.Errorf("tools =\n%s\nwant the native shapes in the given order", gb)
	}
}

func TestBuildParamsCustomSchemaPassthrough(t *testing.T) {
	// Keys out of order and keywords that strict mode drops must reach the API
	// unchanged.
	schema := `{"type":"object","properties":{"q":{"type":"string","minLength":1},"n":{"$ref":"#/$defs/count"}},"required":["q"],"additionalProperties":false,"$defs":{"count":{"type":"integer","minimum":0}}}`
	req := &fw.Request{
		Messages: []fw.Message{fw.UserText("hi")},
		Tools: []fw.ToolSpec{
			{Name: "search", Description: "Search the index.", InputSchema: json.RawMessage(schema)},
			{Name: "noop"},
		},
	}
	p, _ := mustBuild(t, req, 0)
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"input_schema":`+schema) {
		t.Errorf("schema did not pass through byte for byte:\n%s", b)
	}
	tools := wire(t, p)["tools"].([]any)
	noop := tools[1].(map[string]any)
	if !reflect.DeepEqual(noop["input_schema"], decode(t, `{"type":"object"}`)) {
		t.Errorf("empty schema = %v, want {\"type\":\"object\"}", noop["input_schema"])
	}
	if _, ok := noop["description"]; ok {
		t.Errorf("empty description is sent: %v", noop)
	}

	req.Tools = []fw.ToolSpec{{Name: "bad", InputSchema: json.RawMessage(`[1,2]`)}}
	if _, _, err := BuildParams(req, "", 0); err == nil {
		t.Errorf("schema that is not an object: want an error")
	}
}

func TestBuildParamsToolErrors(t *testing.T) {
	msgs := []fw.Message{fw.UserText("hi")}
	cases := map[string][]fw.ToolSpec{
		"unknown native": {{Name: "x", Native: "computer_20991231"}},
		"wire collision": {{Name: "shell", Native: fw.NativeBash}, {Name: "bash"}},
		"two editors":    {{Name: "a", Native: fw.NativeTextEditor}, {Name: "b", Native: fw.NativeTextEditor}},
		"no name":        {{Native: fw.NativeBash}},
	}
	for name, tools := range cases {
		if _, _, err := BuildParams(&fw.Request{Messages: msgs, Tools: tools}, "", 0); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestBuildParamsToolSearch(t *testing.T) {
	var specs []fw.ToolSpec
	specs = append(specs, fw.ToolSpec{Name: "bash", Native: fw.NativeBash})
	for i := 0; i < 29; i++ {
		specs = append(specs, fw.ToolSpec{Name: fmt.Sprintf("t%02d", i)})
	}
	req := &fw.Request{Messages: []fw.Message{fw.UserText("hi")}, Tools: specs}

	// 30 tools is above the default threshold of 24.
	p, _ := mustBuild(t, req, 0)
	tools := wire(t, p)["tools"].([]any)
	if len(tools) != len(specs)+1 {
		t.Fatalf("got %d tools, want %d (the specs plus tool search)", len(tools), len(specs)+1)
	}
	search := tools[len(tools)-1].(map[string]any)
	if search["type"] != "tool_search_tool_bm25_20251119" || search["name"] != "tool_search_tool_bm25" {
		t.Errorf("last tool = %v, want the BM25 tool search tool", search)
	}
	if _, ok := tools[0].(map[string]any)["defer_loading"]; ok {
		t.Errorf("native bash tool is deferred")
	}
	// The first eight custom tools stay loaded; the rest are deferred.
	for i := 1; i < len(specs); i++ {
		tool := tools[i].(map[string]any)
		deferred := tool["defer_loading"] == true
		if wantDeferred := i-1 >= 8; deferred != wantDeferred {
			t.Errorf("tool %s: deferred = %v, want %v", tool["name"], deferred, wantDeferred)
		}
	}

	// At the threshold, nothing is deferred and no search tool is added.
	req.Tools = specs[:24]
	p, _ = mustBuild(t, req, 0)
	tools = wire(t, p)["tools"].([]any)
	if len(tools) != 24 {
		t.Errorf("at the threshold: got %d tools, want 24", len(tools))
	}
	for _, tool := range tools {
		if _, ok := tool.(map[string]any)["defer_loading"]; ok {
			t.Errorf("at the threshold: %v is deferred", tool)
		}
	}

	// A custom threshold applies, and a negative threshold turns search off.
	req.Tools = specs[:12]
	p, _ = mustBuild(t, req, 10)
	if n := len(wire(t, p)["tools"].([]any)); n != 13 {
		t.Errorf("threshold 10 with 12 tools: got %d tools, want 13", n)
	}
	req.Tools = specs
	p, _ = mustBuild(t, req, -1)
	if n := len(wire(t, p)["tools"].([]any)); n != len(specs) {
		t.Errorf("threshold -1: got %d tools, want %d", n, len(specs))
	}
}

func TestBuildParamsFallbacks(t *testing.T) {
	req := &fw.Request{Messages: []fw.Message{fw.UserText("hi")}}

	t.Setenv(FallbacksEnv, "")
	p, _ := mustBuild(t, req, 0)
	if got := wire(t, p)["fallbacks"]; got != "default" {
		t.Errorf("fallbacks = %v, want \"default\"", got)
	}
	if !slices.Contains(p.Betas, anthropic.AnthropicBetaServerSideFallback2026_07_01) {
		t.Errorf("betas = %v, want %s", p.Betas, anthropic.AnthropicBetaServerSideFallback2026_07_01)
	}

	t.Setenv(FallbacksEnv, "off")
	p, _ = mustBuild(t, req, 0)
	if got, ok := wire(t, p)["fallbacks"]; ok {
		t.Errorf("fallbacks = %v with %s=off, want none", got, FallbacksEnv)
	}
	if len(p.Betas) != 0 {
		t.Errorf("betas = %v with %s=off, want none", p.Betas, FallbacksEnv)
	}
}

func TestBuildParamsOpaqueRoundTrip(t *testing.T) {
	// A response from an earlier call, built with the SDK's own types.
	content := `[
		{"type":"thinking","thinking":"","signature":"EqQBCkYIBxgCKkBsig=="},
		{"type":"redacted_thinking","data":"EmwKAhgBEgy3va3pzix/LafPsn4aDFIT"},
		{"type":"text","text":"I will edit the file."},
		{"type":"tool_use","id":"toolu_01","name":"str_replace_based_edit_tool","input":{"command":"view","path":"/a.go"}}
	]`
	var msg anthropic.BetaMessage
	if err := json.Unmarshal([]byte(`{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","stop_reason":"tool_use","content":`+content+`,"usage":{"input_tokens":10,"output_tokens":20}}`), &msg); err != nil {
		t.Fatal(err)
	}
	names := map[string]string{"str_replace_based_edit_tool": "files"}
	resp, err := MapResponse(&msg, names)
	if err != nil {
		t.Fatal(err)
	}

	// The kernel appends the response as an assistant turn, plus an opaque
	// block from another provider that the adapter skips.
	assistant := fw.Message{Role: fw.RoleAssistant, Content: append(resp.Content,
		fw.Block{Type: fw.BlockOpaque, Provider: "gemini", Raw: json.RawMessage(`{"thought":"x"}`)})}
	req := &fw.Request{
		Tools: []fw.ToolSpec{{Name: "files", Native: fw.NativeTextEditor}},
		Messages: []fw.Message{
			fw.UserText("Look at a.go."),
			assistant,
			{Role: fw.RoleUser, Content: []fw.Block{{Type: fw.BlockToolResult, ToolUseID: "toolu_01", Content: []fw.Block{fw.Text("package a")}}}},
		},
	}
	p, gotNames := mustBuild(t, req, 0)
	if !reflect.DeepEqual(gotNames, names) {
		t.Errorf("names = %v, want %v", gotNames, names)
	}
	msgs := msgsOf(t, wire(t, p))
	if len(msgs) != 3 || msgs[1]["role"] != "assistant" {
		t.Fatalf("messages = %v", msgs)
	}
	// The assistant turn reaches the API as the model produced it: the same
	// thinking blocks with their signatures, and the native tool name.
	if !reflect.DeepEqual(msgs[1]["content"], decode(t, content)) {
		got, _ := json.Marshal(msgs[1]["content"])
		t.Errorf("assistant content =\n%s\nwant\n%s", got, content)
	}
	wantResult := decode(t, `[{"type":"tool_result","tool_use_id":"toolu_01","content":[{"type":"text","text":"package a"}]}]`)
	if !reflect.DeepEqual(msgs[2]["content"], wantResult) {
		t.Errorf("tool result = %v", msgs[2]["content"])
	}

	// A broken opaque block is an error, not a silent drop.
	req.Messages[1] = fw.Message{Role: fw.RoleAssistant, Content: []fw.Block{{Type: fw.BlockOpaque, Provider: Provider, Raw: json.RawMessage(`{"no_type":1}`)}}}
	if _, _, err := BuildParams(req, "", 0); err == nil {
		t.Errorf("opaque block without a type: want an error")
	}
}

func TestBuildParamsSystemMessages(t *testing.T) {
	sys := func(s string) fw.Message { return fw.Message{Role: fw.RoleSystem, Content: []fw.Block{fw.Text(s)}} }
	asst := func(s string) fw.Message { return fw.Message{Role: fw.RoleAssistant, Content: []fw.Block{fw.Text(s)}} }
	rem := func(s string) string { return "<system-reminder>\\n" + s + "\\n</system-reminder>" }

	cases := []struct {
		name string
		in   []fw.Message
		want string
	}{
		{
			name: "after a user message and last",
			in:   []fw.Message{fw.UserText("a"), sys("terse mode")},
			want: `[{"role":"user","content":[{"type":"text","text":"a"}]},
				{"role":"system","content":[{"type":"text","text":"terse mode"}]}]`,
		},
		{
			name: "after a user message and before an assistant message",
			in:   []fw.Message{fw.UserText("a"), sys("s1"), sys("s2"), asst("b")},
			want: `[{"role":"user","content":[{"type":"text","text":"a"}]},
				{"role":"system","content":[{"type":"text","text":"s1"},{"type":"text","text":"s2"}]},
				{"role":"assistant","content":[{"type":"text","text":"b"}]}]`,
		},
		{
			name: "before a user message",
			in:   []fw.Message{fw.UserText("a"), asst("b"), sys("note"), fw.UserText("c")},
			want: `[{"role":"user","content":[{"type":"text","text":"a"}]},
				{"role":"assistant","content":[{"type":"text","text":"b"}]},
				{"role":"user","content":[{"type":"text","text":"` + rem("note") + `"},{"type":"text","text":"c"}]}]`,
		},
		{
			name: "first message",
			in:   []fw.Message{sys("note"), fw.UserText("a")},
			want: `[{"role":"user","content":[{"type":"text","text":"` + rem("note") + `"},{"type":"text","text":"a"}]}]`,
		},
		{
			name: "after an assistant message and last",
			in:   []fw.Message{fw.UserText("a"), asst("b"), sys("note")},
			want: `[{"role":"user","content":[{"type":"text","text":"a"}]},
				{"role":"assistant","content":[{"type":"text","text":"b"}]},
				{"role":"user","content":[{"type":"text","text":"` + rem("note") + `"}]}]`,
		},
	}
	for _, c := range cases {
		p, _ := mustBuild(t, &fw.Request{Messages: c.in}, 0)
		got := wire(t, p)["messages"]
		if !reflect.DeepEqual(got, decode(t, c.want)) {
			gb, _ := json.Marshal(got)
			t.Errorf("%s:\ngot  %s\nwant %s", c.name, gb, c.want)
		}
	}
}

func TestBuildParamsUserBlocks(t *testing.T) {
	req := &fw.Request{Messages: []fw.Message{{Role: fw.RoleUser, Content: []fw.Block{
		{Type: fw.BlockText, Text: "Read these."},
		{Type: fw.BlockText, Text: ""},
		{Type: fw.BlockImage, MediaType: "image/png", Data: []byte("png")},
		{Type: fw.BlockImage, URL: "https://example.com/a.jpg"},
		{Type: fw.BlockDocument, MediaType: "application/pdf", Data: []byte("%PDF")},
		{Type: fw.BlockDocument, MediaType: "text/plain", Data: []byte("plain text")},
		{Type: fw.BlockToolResult, ToolUseID: "toolu_9", IsError: true, Content: []fw.Block{fw.Text("boom")}},
	}}}}
	p, _ := mustBuild(t, req, 0)
	got := msgsOf(t, wire(t, p))[0]["content"]
	want := decode(t, `[
		{"type":"text","text":"Read these."},
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"cG5n"}},
		{"type":"image","source":{"type":"url","url":"https://example.com/a.jpg"}},
		{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERg=="}},
		{"type":"document","source":{"type":"text","media_type":"text/plain","data":"plain text"}},
		{"type":"tool_result","tool_use_id":"toolu_9","is_error":true,"content":[{"type":"text","text":"boom"}]}
	]`)
	if !reflect.DeepEqual(got, want) {
		gb, _ := json.Marshal(got)
		t.Errorf("user content =\n%s", gb)
	}
}

func TestBuildParamsMessageErrors(t *testing.T) {
	cases := map[string][]fw.Message{
		"no messages":            nil,
		"unknown role":           {{Role: "tool", Content: []fw.Block{fw.Text("x")}}},
		"tool_use in user turn":  {{Role: fw.RoleUser, Content: []fw.Block{{Type: fw.BlockToolUse, ID: "a", Name: "b"}}}},
		"tool_result in reply":   {fw.UserText("a"), {Role: fw.RoleAssistant, Content: []fw.Block{{Type: fw.BlockToolResult, ToolUseID: "a"}}}},
		"invalid tool input":     {fw.UserText("a"), {Role: fw.RoleAssistant, Content: []fw.Block{{Type: fw.BlockToolUse, ID: "a", Name: "b", Input: json.RawMessage(`{`)}}}},
		"image without a source": {{Role: fw.RoleUser, Content: []fw.Block{{Type: fw.BlockImage}}}},
		"word document":          {{Role: fw.RoleUser, Content: []fw.Block{{Type: fw.BlockDocument, MediaType: "application/msword", Data: []byte("x")}}}},
	}
	for name, msgs := range cases {
		if _, _, err := BuildParams(&fw.Request{Messages: msgs}, "", 0); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
