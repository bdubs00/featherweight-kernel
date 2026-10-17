package gemini

import (
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func TestBuildRequest(t *testing.T) {
	req := &fw.Request{
		System:    "Be brief.",
		Effort:    "medium",
		MaxTokens: 1000,
		Tools: []fw.ToolSpec{
			{
				Name:        "git.commit",
				Description: "Commit",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"msg":{"type":"string"}},"required":["msg"]}`),
			},
			{Name: "now", Description: "Time", InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`)},
			{Name: "web search", Description: "Search", Native: fw.NativeBash, InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)},
		},
		Messages: []fw.Message{
			{Role: fw.RoleUser, Content: []fw.Block{
				fw.Text("Look at this."),
				{Type: fw.BlockImage, MediaType: "image/png", Data: []byte{1, 2, 3}},
				{Type: fw.BlockImage, URL: "https://example.com/a.jpg"},
				{Type: fw.BlockDocument, Data: []byte("%PDF")},
			}},
			{Role: fw.RoleAssistant, Content: []fw.Block{
				fw.Text("Committing."),
				{Type: fw.BlockToolUse, ID: "fwcall_1", Name: "git.commit", Input: json.RawMessage(`{"msg":"fix"}`)},
				{Type: fw.BlockToolUse, ID: "srv-2", Name: "web search", Input: json.RawMessage(`{"q":"go"}`)},
				{Type: fw.BlockOpaque, Provider: "anthropic", Raw: json.RawMessage(`{"type":"thinking"}`)},
			}},
			{Role: fw.RoleUser, Content: []fw.Block{
				{Type: fw.BlockToolResult, ToolUseID: "fwcall_1", Content: []fw.Block{fw.Text("done"), fw.Text("ok")}},
				{Type: fw.BlockToolResult, ToolUseID: "srv-2", IsError: true, Content: []fw.Block{
					fw.Text("no network"),
					{Type: fw.BlockImage, MediaType: "image/png", Data: []byte{9}},
				}},
			}},
			{Role: fw.RoleSystem, Content: []fw.Block{fw.Text("Wrap up.")}},
		},
	}
	contents, cfg, names, err := BuildRequest(req, "gemini-3-pro")
	if err != nil {
		t.Fatal(err)
	}
	if names["web_search"] != "web search" || len(names) != 1 {
		t.Errorf("names = %v, want only web_search", names)
	}
	got, _ := json.Marshal(contents)
	jsonEqual(t, got, `[
	  {"role": "user", "parts": [
	    {"text": "Look at this."},
	    {"inlineData": {"mimeType": "image/png", "data": "AQID"}},
	    {"fileData": {"fileUri": "https://example.com/a.jpg", "mimeType": "image/jpeg"}},
	    {"inlineData": {"mimeType": "application/pdf", "data": "JVBERg=="}}
	  ]},
	  {"role": "model", "parts": [
	    {"text": "Committing."},
	    {"functionCall": {"name": "git.commit", "args": {"msg": "fix"}}},
	    {"functionCall": {"id": "srv-2", "name": "web_search", "args": {"q": "go"}}}
	  ]},
	  {"role": "user", "parts": [
	    {"functionResponse": {"name": "git.commit", "response": {"output": "done\nok"}}},
	    {"functionResponse": {"id": "srv-2", "name": "web_search", "response": {"error": "no network"},
	      "parts": [{"inlineData": {"mimeType": "image/png", "data": "CQ=="}}]}},
	    {"text": "<system-reminder>\nWrap up.\n</system-reminder>"}
	  ]}
	]`)

	gotCfg, _ := json.Marshal(cfg)
	jsonEqual(t, gotCfg, `{
	  "systemInstruction": {"parts": [{"text": "Be brief."}]},
	  "maxOutputTokens": 1000,
	  "thinkingConfig": {"thinkingLevel": "MEDIUM"},
	  "tools": [{"functionDeclarations": [
	    {"name": "git.commit", "description": "Commit",
	     "parameters": {"type": "OBJECT", "properties": {"msg": {"type": "STRING"}}, "required": ["msg"]}},
	    {"name": "now", "description": "Time"},
	    {"name": "web_search", "description": "Search",
	     "parameters": {"type": "OBJECT", "properties": {"q": {"type": "STRING"}}}}
	  ]}]
	}`)
}

func TestBuildRequestErrors(t *testing.T) {
	cases := map[string]*fw.Request{
		"no messages": {},
		"bad effort":  {Effort: "huge", Messages: []fw.Message{fw.UserText("hi")}},
		"orphan tool result": {Messages: []fw.Message{{Role: fw.RoleUser, Content: []fw.Block{
			{Type: fw.BlockToolResult, ToolUseID: "x", Content: []fw.Block{fw.Text("r")}},
		}}}},
		"image without type": {Messages: []fw.Message{{Role: fw.RoleUser, Content: []fw.Block{
			{Type: fw.BlockImage, Data: []byte{1}},
		}}}},
		"non-object schema": {Messages: []fw.Message{fw.UserText("hi")}, Tools: []fw.ToolSpec{
			{Name: "t", InputSchema: json.RawMessage(`{"type":"string"}`)},
		}},
		"tool use in user message": {Messages: []fw.Message{{Role: fw.RoleUser, Content: []fw.Block{
			{Type: fw.BlockToolUse, ID: "a", Name: "t"},
		}}}},
	}
	for name, req := range cases {
		if _, _, _, err := BuildRequest(req, ""); err == nil {
			t.Errorf("%s: no error", name)
		} else if !strings.HasPrefix(err.Error(), "gemini: ") {
			t.Errorf("%s: error %q does not name the package", name, err)
		}
	}
}

func TestThinkingConfig(t *testing.T) {
	cases := []struct {
		model, effort, want string
	}{
		{"gemini-3.8-flash", "", "null"},
		{"gemini-3.8-flash", "low", `{"thinkingLevel":"LOW"}`},
		{"gemini-3.8-flash", "max", `{"thinkingLevel":"HIGH"}`},
		{"models/gemini-3-pro", "xhigh", `{"thinkingLevel":"HIGH"}`},
		{"gemini-2.5-flash", "medium", `{"thinkingBudget":8192}`},
		{"gemini-2.5-pro", "max", `{"thinkingBudget":24576}`},
		{"gemini-2.0-flash", "high", "null"},
		{"gemma-3-27b-it", "high", "null"},
	}
	for _, c := range cases {
		tc, err := thinkingConfig(c.model, c.effort)
		if err != nil {
			t.Fatalf("%s %s: %v", c.model, c.effort, err)
		}
		got, _ := json.Marshal(tc)
		if string(got) != c.want {
			t.Errorf("%s %s: got %s, want %s", c.model, c.effort, got, c.want)
		}
	}
}

func TestWireNameFor(t *testing.T) {
	taken := map[string]string{}
	cases := map[string]string{
		"git.commit": "git.commit",
		"ns:tool-x":  "ns:tool-x",
		"a b/c":      "a_b_c",
		"9lives":     "_9lives",
		"":           "tool",
	}
	for in, want := range cases {
		if got := wireNameFor(in, taken); got != want {
			t.Errorf("wireNameFor(%q) = %q, want %q", in, got, want)
		}
	}
	taken["a_b"] = "other"
	if got := wireNameFor("a b", taken); got != "a_b_2" {
		t.Errorf("collision should get a suffix, got %q", got)
	}
	long := strings.Repeat("x", 70)
	if got := wireNameFor(long, taken); len(got) != maxNameLen {
		t.Errorf("long name kept %d characters, want %d", len(got), maxNameLen)
	}
}

func TestToolNameCollision(t *testing.T) {
	req := &fw.Request{
		Messages: []fw.Message{fw.UserText("hi")},
		Tools:    []fw.ToolSpec{{Name: "a_b"}, {Name: "a_b"}},
	}
	if _, _, _, err := BuildRequest(req, ""); err == nil {
		t.Fatal("two tools with one name should fail")
	}
}

// TestOpaqueRoundTrip maps a response with thought signatures to blocks, sends
// the blocks back, and checks that the parts are unchanged.
func TestOpaqueRoundTrip(t *testing.T) {
	parts := []*genai.Part{
		{Text: "thinking summary", Thought: true},
		{Text: "Let me check.", ThoughtSignature: []byte("sig-text")},
		{FunctionCall: &genai.FunctionCall{Name: "web_search", Args: map[string]any{"q": "go"}}, ThoughtSignature: []byte("sig-call")},
		{FunctionCall: &genai.FunctionCall{Name: "now"}},
		{ExecutableCode: &genai.ExecutableCode{Code: "print(1)", Language: genai.LanguagePython}},
	}
	names := map[string]string{"web_search": "web search"}
	resp, err := MapResponse(&genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{Content: &genai.Content{Role: "model", Parts: parts}, FinishReason: genai.FinishReasonStop}},
	}, names)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, b := range resp.Content {
		types = append(types, b.Type)
	}
	want := "opaque opaque text opaque tool_use tool_use opaque"
	if got := strings.Join(types, " "); got != want {
		t.Fatalf("block types = %s, want %s", got, want)
	}
	uses := resp.ToolUses()
	if uses[0].Name != "web search" || string(uses[0].Input) != `{"q":"go"}` || !strings.HasPrefix(uses[0].ID, callIDPrefix) {
		t.Errorf("first tool use = %+v", uses[0])
	}
	if string(uses[1].Input) != `{}` || uses[1].ID == uses[0].ID {
		t.Errorf("second tool use = %+v", uses[1])
	}
	if resp.StopReason != fw.StopToolUse {
		t.Errorf("stop = %s, want tool_use", resp.StopReason)
	}
	if fw.TextOf(resp.Content) != "Let me check." {
		t.Errorf("text = %q", fw.TextOf(resp.Content))
	}

	// Send the turn back with results.
	history := []fw.Message{
		fw.UserText("go?"),
		{Role: fw.RoleAssistant, Content: resp.Content},
		{Role: fw.RoleUser, Content: []fw.Block{
			{Type: fw.BlockToolResult, ToolUseID: uses[0].ID, Content: []fw.Block{fw.Text("r1")}},
			{Type: fw.BlockToolResult, ToolUseID: uses[1].ID, Content: []fw.Block{fw.Text("r2")}},
		}},
	}
	tools := []fw.ToolSpec{{Name: "web search"}, {Name: "now"}}
	contents, _, _, err := BuildRequest(&fw.Request{Messages: history, Tools: tools}, "")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(contents[1].Parts)
	wantParts, _ := json.Marshal(parts)
	jsonEqual(t, got, string(wantParts))
	gotResults, _ := json.Marshal(contents[2].Parts)
	jsonEqual(t, gotResults, `[
	  {"functionResponse": {"name": "web_search", "response": {"output": "r1"}}},
	  {"functionResponse": {"name": "now", "response": {"output": "r2"}}}
	]`)
}

// TestCarrierWithoutTwin drops a signature carrier whose tool_use block is
// gone, so the call does not go out twice or alone.
func TestCarrierWithoutTwin(t *testing.T) {
	raw, _ := json.Marshal(&genai.Part{FunctionCall: &genai.FunctionCall{Name: "a"}, ThoughtSignature: []byte("s")})
	msgs := []fw.Message{
		fw.UserText("hi"),
		{Role: fw.RoleAssistant, Content: []fw.Block{
			{Type: fw.BlockOpaque, Provider: Provider, Raw: raw},
			fw.Text("no call after all"),
		}},
	}
	contents, _, _, err := BuildRequest(&fw.Request{Messages: msgs}, "")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(contents[1].Parts)
	jsonEqual(t, got, `[{"text":"no call after all"}]`)
}
