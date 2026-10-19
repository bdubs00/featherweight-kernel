package openai

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// sse renders data events and the final [DONE].
func sse(chunks ...string) string {
	var sb strings.Builder
	for _, c := range chunks {
		fmt.Fprintf(&sb, "data: %s\n\n", c)
	}
	sb.WriteString(": keep-alive comment\n\ndata: [DONE]\n\n")
	return sb.String()
}

// streamBody is one streamed turn: text in two deltas, a tool call whose
// arguments arrive in three fragments, then usage in a chunk with no choices.
var streamBody = sse(
	`{"id":"c1","model":"served-model","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
	`{"id":"c1","choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}`,
	`{"id":"c1","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}`,
	`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"git__commit","arguments":""}}]},"finish_reason":null}]}`,
	`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"msg\":"}}]},"finish_reason":null}]}`,
	`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"fix\"}"}}]},"finish_reason":null}]}`,
	`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	`{"id":"c1","choices":[],"usage":{"prompt_tokens":120,"completion_tokens":30,"total_tokens":150,"prompt_tokens_details":{"cached_tokens":100}}}`,
)

// reply is one canned answer of the fake server.
type reply struct {
	status int
	header map[string]string
	body   string
}

// fake is a server that answers with the replies in order and records each
// request.
type fake struct {
	mu      sync.Mutex
	replies []reply
	headers []http.Header
	bodies  []map[string]any
	paths   []string
}

func newFake(t *testing.T, replies ...reply) (*fake, *httptest.Server) {
	t.Helper()
	f := &fake{replies: replies}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		n := len(f.headers)
		f.headers = append(f.headers, r.Header.Clone())
		f.paths = append(f.paths, r.URL.RequestURI())
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		f.bodies = append(f.bodies, body)
		rep := f.replies[min(n, len(f.replies)-1)]
		f.mu.Unlock()
		for k, v := range rep.header {
			w.Header().Set(k, v)
		}
		if rep.status != 0 && rep.status != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(rep.status)
		} else if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "text/event-stream")
		}
		_, _ = io.WriteString(w, rep.body)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func testModel(t *testing.T, base, key, model string, reasoning bool) *Model {
	t.Helper()
	return testModelWith(t, settings{base: base, key: key, model: model, reasoning: reasoning})
}

// testModelWith builds a model from settings with a short backoff, so a
// retry test runs fast.
func testModelWith(t *testing.T, s settings) *Model {
	t.Helper()
	m, err := newModel(s)
	if err != nil {
		t.Fatal(err)
	}
	m.backoff = time.Millisecond
	m.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	return m
}

// history is a conversation with every block type that the adapter maps.
var history = []fw.Message{
	{Role: fw.RoleSystem, Content: []fw.Block{fw.Text("Operator note.")}},
	{Role: fw.RoleUser, Content: []fw.Block{
		fw.Text("Commit this."),
		{Type: fw.BlockImage, MediaType: "image/png", Data: []byte{1, 2, 3}},
		{Type: fw.BlockImage, URL: "https://example.com/a.png"},
	}},
	{Role: fw.RoleAssistant, Content: []fw.Block{
		{Type: fw.BlockOpaque, Provider: Provider, Raw: json.RawMessage(`{"reasoning_content":"think"}`)},
		{Type: fw.BlockOpaque, Provider: "anthropic", Raw: json.RawMessage(`{"type":"thinking"}`)},
		fw.Text("Sure."),
		{Type: fw.BlockToolUse, ID: "call_1", Name: "git.commit", Input: json.RawMessage(`{"msg":"x"}`)},
	}},
	{Role: fw.RoleUser, Content: []fw.Block{
		{Type: fw.BlockToolResult, ToolUseID: "call_1", IsError: true, Content: []fw.Block{
			fw.Text("conflict"),
			{Type: fw.BlockImage, MediaType: "image/png", Data: []byte{9}},
		}},
		fw.Text("Try again."),
	}},
	{Role: fw.RoleSystem, Content: []fw.Block{fw.Text("Be quick.")}},
}

var tools = []fw.ToolSpec{{
	Name:        "git.commit",
	Description: "Commit the work tree",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"msg":{"type":"string"}},"required":["msg"]}`),
}}

func TestStream(t *testing.T) {
	f, srv := newFake(t, reply{body: streamBody})
	m := testModel(t, srv.URL+"/v1/", "", "qwen3-32b", false)
	req := &fw.Request{System: "Be brief.", Messages: history, Tools: tools, Effort: "high", MaxTokens: 512}
	var (
		texts  []string
		blocks []string
	)
	resp, err := m.Stream(context.Background(), req, func(d fw.Delta) {
		switch d.Type {
		case "text":
			texts = append(texts, d.Text)
		case "block":
			blocks = append(blocks, d.Block.Type)
		}
	})
	if err != nil {
		t.Fatal(err)
	}

	if f.paths[0] != "/v1/chat/completions" {
		t.Errorf("path = %s", f.paths[0])
	}
	if h := f.headers[0].Get("Authorization"); h != "" {
		t.Errorf("Authorization = %q, want none without a key", h)
	}
	got, _ := json.Marshal(f.bodies[0])
	want := `{
	  "model": "qwen3-32b",
	  "stream": true,
	  "stream_options": {"include_usage": true},
	  "max_tokens": 512,
	  "tools": [{"type": "function", "function": {
	    "name": "git__commit", "description": "Commit the work tree", "strict": false,
	    "parameters": {"type":"object","properties":{"msg":{"type":"string"}},"required":["msg"]}}}],
	  "messages": [
	    {"role": "system", "content": "Be brief."},
	    {"role": "system", "content": "Operator note."},
	    {"role": "user", "content": [
	      {"type": "text", "text": "Commit this."},
	      {"type": "image_url", "image_url": {"url": "data:image/png;base64,AQID"}},
	      {"type": "image_url", "image_url": {"url": "https://example.com/a.png"}}
	    ]},
	    {"role": "assistant", "content": "Sure.", "reasoning_content": "think",
	     "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "git__commit", "arguments": "{\"msg\":\"x\"}"}}]},
	    {"role": "tool", "tool_call_id": "call_1", "content": "Error: conflict"},
	    {"role": "user", "content": [
	      {"type": "text", "text": "Attachments of the result of tool call call_1:"},
	      {"type": "image_url", "image_url": {"url": "data:image/png;base64,CQ=="}},
	      {"type": "text", "text": "Try again."}
	    ]},
	    {"role": "user", "content": "<system-reminder>\nBe quick.\n</system-reminder>"}
	  ]
	}`
	jsonEqual(t, got, want)

	if strings.Join(texts, "|") != "Hel|lo" {
		t.Errorf("text deltas = %q", texts)
	}
	if strings.Join(blocks, " ") != "text tool_use" {
		t.Errorf("block deltas = %v", blocks)
	}
	if fw.TextOf(resp.Content) != "Hello" {
		t.Errorf("text = %q", fw.TextOf(resp.Content))
	}
	uses := resp.ToolUses()
	if len(uses) != 1 || uses[0].ID != "call_abc" || uses[0].Name != "git.commit" || string(uses[0].Input) != `{"msg":"fix"}` {
		t.Errorf("tool uses = %+v", uses)
	}
	if resp.StopReason != fw.StopToolUse {
		t.Errorf("stop = %s", resp.StopReason)
	}
	if resp.Usage != (fw.Usage{InputTokens: 20, OutputTokens: 30, CacheReadTokens: 100}) {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if resp.Model != "served-model" || resp.Provider != Provider {
		t.Errorf("model %q provider %q", resp.Model, resp.Provider)
	}
	jsonEqual(t, resp.Raw, `{"role":"assistant","content":"Hello",
	  "tool_calls":[{"id":"call_abc","type":"function","function":{"name":"git__commit","arguments":"{\"msg\":\"fix\"}"}}]}`)
}

func TestBearerHeader(t *testing.T) {
	f, srv := newFake(t, reply{body: streamBody})
	m := testModel(t, srv.URL, "sk-test", "local", false)
	if _, err := m.Generate(context.Background(), &fw.Request{Messages: []fw.Message{fw.UserText("hi")}}); err != nil {
		t.Fatal(err)
	}
	if h := f.headers[0].Get("Authorization"); h != "Bearer sk-test" {
		t.Errorf("Authorization = %q", h)
	}
	if _, ok := f.bodies[0]["reasoning_effort"]; ok {
		t.Error("reasoning_effort sent to a plain model")
	}
	if _, ok := f.bodies[0]["max_tokens"]; ok {
		t.Error("max_tokens sent without a limit in the request")
	}
}

func TestReasoning(t *testing.T) {
	body := sse(
		`{"model":"o4-mini","choices":[{"index":0,"delta":{"reasoning_content":"Let me "}}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning_content":"think."}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"42"},"finish_reason":"stop"}]}`,
	)
	f, srv := newFake(t, reply{body: body})
	m := testModel(t, srv.URL, "", "o4-mini", true)
	resp, err := m.Generate(context.Background(), &fw.Request{Messages: []fw.Message{fw.UserText("q")}, Effort: "max", MaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	if f.bodies[0]["reasoning_effort"] != "high" || f.bodies[0]["max_completion_tokens"] != float64(100) {
		t.Errorf("body = %v, want reasoning_effort high and max_completion_tokens", f.bodies[0])
	}
	if _, ok := f.bodies[0]["max_tokens"]; ok {
		t.Error("max_tokens sent to a reasoning model")
	}
	if len(resp.Content) != 2 || resp.Content[0].Type != fw.BlockOpaque || resp.StopReason != fw.StopEndTurn {
		t.Fatalf("response = %+v", resp)
	}
	jsonEqual(t, resp.Content[0].Raw, `{"reasoning_content":"Let me think."}`)

	// The reasoning goes back on the assistant message.
	b, _, err := BuildBody(&fw.Request{Messages: []fw.Message{
		fw.UserText("q"), {Role: fw.RoleAssistant, Content: resp.Content}, fw.UserText("why?"),
	}}, "o4-mini", BodyOptions{Reasoning: true})
	if err != nil {
		t.Fatal(err)
	}
	var sent struct{ Messages []map[string]any }
	_ = json.Unmarshal(b, &sent)
	if sent.Messages[1]["reasoning_content"] != "Let me think." || sent.Messages[1]["content"] != "42" {
		t.Errorf("assistant message = %v", sent.Messages[1])
	}
}

func TestReasoningModel(t *testing.T) {
	for name, want := range map[string]bool{
		"o1": true, "o3-mini": true, "o4-mini": true, "gpt-5": true, "gpt-5-mini": true,
		"openai/o3": true, "openai/gpt-5.1": true,
		"gpt-4.1": false, "qwen3-32b": false, "olmo-2": false, "openchat": false, "llama3.1": false,
	} {
		if got := reasoningModel(name); got != want {
			t.Errorf("reasoningModel(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestNew(t *testing.T) {
	ctx := context.Background()
	clearEnv(t)
	if _, err := New(ctx, "", nil, fw.EnvSecrets{}); err == nil {
		t.Error("an empty model name should fail")
	}
	m, err := New(ctx, "llama3", nil, fw.EnvSecrets{})
	if err != nil {
		t.Fatal(err)
	}
	om := m.(*Model)
	if om.endpoint.String() != DefaultBaseURL+"/chat/completions" || om.key != "" || om.reasoning {
		t.Errorf("model = %+v", om)
	}

	t.Setenv(BaseURLSecret, "http://localhost:11434/v1")
	t.Setenv(ReasoningEnv, "1")
	secrets := fw.SecretsFunc(func(ctx context.Context, name string) (string, error) {
		if name == APIKeySecret {
			return "k", nil
		}
		return "", fmt.Errorf("%w: %s", fw.ErrSecretNotFound, name)
	})
	m, err = New(ctx, "llama3", &fw.Config{Effort: "low"}, secrets)
	if err != nil {
		t.Fatal(err)
	}
	om = m.(*Model)
	if om.endpoint.String() != "http://localhost:11434/v1/chat/completions" || om.key != "k" || !om.reasoning || om.effort != "low" {
		t.Errorf("model = %+v", om)
	}

	broken := fw.SecretsFunc(func(context.Context, string) (string, error) { return "", errors.New("vault down") })
	if _, err := New(ctx, "llama3", nil, broken); err == nil {
		t.Error("a failing secrets store should fail")
	}
	if _, err := New(ctx, "llama3", &fw.Config{Effort: "turbo"}, fw.EnvSecrets{}); err == nil {
		t.Error("an unknown effort should fail")
	}
}

func TestRetry(t *testing.T) {
	f, srv := newFake(t,
		reply{status: http.StatusTooManyRequests, header: map[string]string{"Retry-After": "0"}, body: `{"error":{"message":"slow down"}}`},
		reply{status: http.StatusServiceUnavailable, body: `{"error":{"message":"busy"}}`},
		reply{body: streamBody},
	)
	m := testModel(t, srv.URL, "", "local", false)
	resp, err := m.Generate(context.Background(), &fw.Request{Messages: []fw.Message{fw.UserText("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.bodies) != 3 || fw.TextOf(resp.Content) != "Hello" {
		t.Errorf("%d requests, text %q; want 3 and Hello", len(f.bodies), fw.TextOf(resp.Content))
	}
}

func TestRetryGivesUp(t *testing.T) {
	f, srv := newFake(t, reply{status: http.StatusInternalServerError, body: `oops`})
	m := testModel(t, srv.URL, "", "local", false)
	_, err := m.Generate(context.Background(), &fw.Request{Messages: []fw.Message{fw.UserText("hi")}})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 500 || apiErr.Message != "oops" {
		t.Fatalf("error = %v", err)
	}
	if len(f.bodies) != 1+maxRetries {
		t.Errorf("%d requests, want %d", len(f.bodies), 1+maxRetries)
	}
}

func TestClientError(t *testing.T) {
	f, srv := newFake(t, reply{
		status: http.StatusBadRequest,
		body:   `{"error":{"message":"Invalid 'messages[1].content': string too long.","type":"invalid_request_error","code":null}}`,
	})
	m := testModel(t, srv.URL+"/v1?api-key=sk-query-secret", "sk-test", "local", false)
	_, err := m.Generate(context.Background(), &fw.Request{Messages: []fw.Message{fw.UserText("hi")}})
	if err == nil {
		t.Fatal("no error")
	}
	if len(f.bodies) != 1 {
		t.Errorf("%d requests, want 1: a 4xx is not retried", len(f.bodies))
	}
	if f.paths[0] != "/v1/chat/completions?api-key=sk-query-secret" {
		t.Errorf("path = %s, want the query kept on the request", f.paths[0])
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 400 || apiErr.Type != "invalid_request_error" {
		t.Errorf("error %v is not a 400 APIError", err)
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "openai: ") || !strings.Contains(msg, "string too long") || !strings.Contains(msg, "400") {
		t.Errorf("error = %q", msg)
	}
	if strings.Contains(msg, "sk-query-secret") || strings.Contains(msg, "sk-test") {
		t.Errorf("error leaks a secret: %q", msg)
	}
}

func TestErrorBodies(t *testing.T) {
	cases := map[string]string{
		`{"error":{"message":"m1"}}`:                         "m1",
		`{"error":"m2"}`:                                     "m2",
		`{"object":"error","message":"m3","code":400}`:       "m3",
		`{"detail":"m4"}`:                                    "m4",
		`plain text`:                                         "plain text",
		`{"error":{"message":"bad key sk-live-1 provided"}}`: "bad key [redacted] provided",
	}
	for body, want := range cases {
		resp := &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(body))}
		if got := readAPIError(resp, "sk-live-1").Message; got != want {
			t.Errorf("body %s: message %q, want %q", body, got, want)
		}
	}
}

func TestStopReasons(t *testing.T) {
	cases := []struct {
		chunks []string
		want   fw.StopReason
		detail string
	}{
		{[]string{`{"choices":[{"delta":{"content":"a"},"finish_reason":"length"}]}`}, fw.StopMaxTokens, ""},
		{[]string{`{"choices":[{"delta":{"content":"a"},"finish_reason":"content_filter"}]}`}, fw.StopRefusal, "content_filter"},
		{[]string{`{"choices":[{"delta":{"refusal":"I can't."},"finish_reason":"stop"}]}`}, fw.StopRefusal, "refusal"},
		{[]string{`{"choices":[{"delta":{"content":"a"},"finish_reason":"weird"}]}`}, fw.StopOther, "weird"},
		// A server that ends a tool call turn with "stop" and sends no index or ID.
		{[]string{`{"choices":[{"delta":{"tool_calls":[{"function":{"name":"f","arguments":"{}"}}]},"finish_reason":"stop"}]}`}, fw.StopToolUse, ""},
	}
	for i, c := range cases {
		acc := newAccumulator(nil, nil)
		if err := acc.readSSE(strings.NewReader(sse(c.chunks...))); err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		resp, err := acc.response("m")
		if err != nil {
			t.Fatal(err)
		}
		if resp.StopReason != c.want || resp.StopDetail != c.detail {
			t.Errorf("case %d: stop %s %q, want %s %q", i, resp.StopReason, resp.StopDetail, c.want, c.detail)
		}
		for _, u := range resp.ToolUses() {
			if !strings.HasPrefix(u.ID, "call_") {
				t.Errorf("case %d: generated ID %q", i, u.ID)
			}
		}
	}
}

func TestStreamFailures(t *testing.T) {
	cases := map[string]string{
		"cut off":      "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n",
		"server error": "data: {\"error\":{\"message\":\"overloaded\"}}\n\n",
		"bad json":     "data: {nope\n\n",
	}
	for name, body := range cases {
		acc := newAccumulator(nil, nil)
		if err := acc.readSSE(strings.NewReader(body)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestNonStreamingServer(t *testing.T) {
	_, srv := newFake(t, reply{
		header: map[string]string{"Content-Type": "application/json"},
		body: `{"model":"m","choices":[{"message":{"role":"assistant","content":"hi","tool_calls":[
		  {"id":"a","type":"function","function":{"name":"f","arguments":"{\"x\":1}"}},
		  {"id":"b","type":"function","function":{"name":"g","arguments":""}}]},"finish_reason":"tool_calls"}],
		  "usage":{"prompt_tokens":5,"completion_tokens":2}}`,
	})
	m := testModel(t, srv.URL, "", "local", false)
	resp, err := m.Generate(context.Background(), &fw.Request{Messages: []fw.Message{fw.UserText("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	uses := resp.ToolUses()
	if len(uses) != 2 || uses[1].Name != "g" || string(uses[1].Input) != "{}" || resp.Usage.InputTokens != 5 {
		t.Errorf("response = %+v", resp)
	}
}

func TestCutOffArguments(t *testing.T) {
	acc := newAccumulator(nil, nil)
	body := sse(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":"{\"x\":"}}]},"finish_reason":"length"}]}`)
	if err := acc.readSSE(strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	resp, err := acc.response("m")
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(resp.Content[0].Input) {
		t.Fatalf("input %s is not valid JSON", resp.Content[0].Input)
	}
	b, _, err := BuildBody(&fw.Request{Messages: []fw.Message{fw.UserText("q"), {Role: fw.RoleAssistant, Content: resp.Content}}}, "m", BodyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"arguments":"{\"x\":"`) {
		t.Errorf("arguments did not go back as they came: %s", b)
	}
}

func TestBuildBodyErrors(t *testing.T) {
	cases := map[string]*fw.Request{
		"no messages": {},
		"bad effort":  {Effort: "huge", Messages: []fw.Message{fw.UserText("hi")}},
		"bad schema": {Messages: []fw.Message{fw.UserText("hi")}, Tools: []fw.ToolSpec{
			{Name: "t", InputSchema: json.RawMessage(`[1]`)},
		}},
		"collision": {Messages: []fw.Message{fw.UserText("hi")}, Tools: []fw.ToolSpec{{Name: "a"}, {Name: "a"}}},
		"document URL": {Messages: []fw.Message{{Role: fw.RoleUser, Content: []fw.Block{
			{Type: fw.BlockDocument, URL: "https://example.com/a.pdf"},
		}}}},
	}
	for name, req := range cases {
		if _, _, err := BuildBody(req, "m", BodyOptions{}); err == nil {
			t.Errorf("%s: no error", name)
		} else if !strings.HasPrefix(err.Error(), "openai: ") {
			t.Errorf("%s: error %q does not name the package", name, err)
		}
	}
}

func TestRegistered(t *testing.T) {
	for _, p := range fw.RegisteredModels() {
		if p == Provider {
			return
		}
	}
	t.Errorf("provider %q is not registered", Provider)
}

func jsonEqual(t *testing.T, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got is not JSON: %v\n%s", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want is not JSON: %v\n%s", err, want)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("JSON mismatch\n got: %s\nwant: %s", got, want)
	}
}

// secretsOf returns a secrets store over a map. Any other name is missing.
func secretsOf(vals map[string]string) fw.Secrets {
	return fw.SecretsFunc(func(_ context.Context, name string) (string, error) {
		if v, ok := vals[name]; ok {
			return v, nil
		}
		return "", fmt.Errorf("%w: %s", fw.ErrSecretNotFound, name)
	})
}

// providerConfig returns a configuration with one openai provider entry.
func providerConfig(pc fw.ProviderConfig) *fw.Config {
	return &fw.Config{Providers: map[string]fw.ProviderConfig{Provider: pc}}
}

// clearEnv blanks the variables that New reads, so the machine that runs
// the tests cannot leak its settings into them.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{BaseURLSecret, APIKeySecret, OrganizationSecret, ProjectSecret, ReasoningEnv} {
		t.Setenv(name, "")
	}
}

var hello = &fw.Request{Messages: []fw.Message{fw.UserText("hi")}}

func TestParseOptions(t *testing.T) {
	o, err := parseOptions(nil)
	if err != nil {
		t.Fatal(err)
	}
	want := options{reasoning: "auto", tokenField: "auto", authHeader: "Authorization", cacheKey: "auto", mediaTypes: "auto", idle: DefaultIdleTimeout}
	if o != want {
		t.Errorf("defaults = %+v, want %+v", o, want)
	}

	o, err = parseOptions(map[string]string{
		"reasoning": " ON ", "maxTokensField": "max_completion_tokens", "authHeader": "api-key",
		"organization": "org-1", "project": "proj-1", "promptCacheKey": "off",
		"parallelToolCalls": "false", "idleTimeout": "5m", "mediaTypes": "image/png, image/jpeg",
	})
	if err != nil {
		t.Fatal(err)
	}
	want = options{reasoning: "on", tokenField: "max_completion_tokens", authHeader: "api-key",
		organization: "org-1", project: "proj-1", cacheKey: "off", parallel: "false", mediaTypes: "image/png, image/jpeg", idle: 5 * time.Minute}
	if o != want {
		t.Errorf("options = %+v, want %+v", o, want)
	}
	if o, err := parseOptions(map[string]string{"idleTimeout": "0"}); err != nil || o.idle != 0 {
		t.Errorf("idleTimeout 0: %+v, %v", o, err)
	}

	_, err = parseOptions(map[string]string{"bogus": "1"})
	if err == nil || !strings.Contains(err.Error(), `"bogus"`) || !strings.Contains(err.Error(), "authHeader, idleTimeout") {
		t.Errorf("unknown key: error = %v, want the key and the known keys", err)
	}
	for key, value := range map[string]string{
		"reasoning": "maybe", "maxTokensField": "tokens", "authHeader": "", "promptCacheKey": "yes",
		"parallelToolCalls": "no", "idleTimeout": "soon", "mediaTypes": "images",
	} {
		_, err := parseOptions(map[string]string{key: value})
		if err == nil || !strings.Contains(err.Error(), key) || !strings.HasPrefix(err.Error(), "openai: ") {
			t.Errorf("%s=%q: error = %v, want an error that names the option", key, value, err)
		}
	}
}

func TestNewProviderConfig(t *testing.T) {
	clearEnv(t)
	ctx := context.Background()
	f, srv := newFake(t, reply{body: streamBody})
	t.Setenv(BaseURLSecret, "http://127.0.0.1:1/v1") // loses to the configured base URL
	cfg := providerConfig(fw.ProviderConfig{
		BaseURL:   srv.URL + "/v1",
		APIKeyEnv: "MY_KEY",
		Headers: map[string]string{
			"X-Team":        "platform",
			"X-Proxy-Token": "PROXY_TOKEN",
			"X-Missing":     "NOT_A_SECRET",
		},
		Options: map[string]string{
			"authHeader":   "api-key",
			"organization": "org-1",
			"project":      "proj-1",
		},
	})
	secrets := secretsOf(map[string]string{"MY_KEY": "sk-azure", "PROXY_TOKEN": "tok-1"})
	m, err := New(ctx, "deployment-1", cfg, secrets)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Generate(ctx, hello); err != nil {
		t.Fatal(err)
	}
	h := f.headers[0]
	for name, want := range map[string]string{
		"api-key":             "sk-azure",
		"Authorization":       "",
		"X-Team":              "platform",
		"X-Proxy-Token":       "tok-1",
		"X-Missing":           "NOT_A_SECRET",
		"OpenAI-Organization": "org-1",
		"OpenAI-Project":      "proj-1",
		"User-Agent":          userAgent,
		"Content-Type":        "application/json",
	} {
		if got := h.Get(name); got != want {
			t.Errorf("header %s = %q, want %q", name, got, want)
		}
	}
	if f.paths[0] != "/v1/chat/completions" {
		t.Errorf("path = %s, want the configured base URL", f.paths[0])
	}

	// An unknown option fails at New.
	bad := providerConfig(fw.ProviderConfig{Options: map[string]string{"bogus": "1"}})
	if _, err := New(ctx, "m", bad, secrets); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Errorf("unknown option: error = %v", err)
	}
	// A vault that fails is an error, not a silent literal.
	broken := fw.SecretsFunc(func(context.Context, string) (string, error) { return "", errors.New("vault down") })
	if _, err := New(ctx, "m", cfg, broken); err == nil {
		t.Error("a failing secrets store is an error")
	}
}

func TestOrganizationFromEnvironment(t *testing.T) {
	clearEnv(t)
	t.Setenv(OrganizationSecret, "org-env")
	t.Setenv(ProjectSecret, "proj-env")
	f, srv := newFake(t, reply{body: streamBody})
	m, err := New(context.Background(), "local", providerConfig(fw.ProviderConfig{BaseURL: srv.URL}), secretsOf(nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Generate(context.Background(), hello); err != nil {
		t.Fatal(err)
	}
	if f.headers[0].Get("OpenAI-Organization") != "org-env" || f.headers[0].Get("OpenAI-Project") != "proj-env" {
		t.Errorf("headers = %v, want the organization and the project from the environment", f.headers[0])
	}
}

func TestHeaderReplacesAuthorization(t *testing.T) {
	f, srv := newFake(t, reply{body: streamBody})
	m := testModelWith(t, settings{base: srv.URL, key: "sk-test", model: "local",
		headers: map[string]string{"Authorization": "Basic abc"}})
	if _, err := m.Generate(context.Background(), hello); err != nil {
		t.Fatal(err)
	}
	if got := f.headers[0].Values("Authorization"); len(got) != 1 || got[0] != "Basic abc" {
		t.Errorf("Authorization = %q, want the configured header alone", got)
	}
}

func TestPromptCacheKey(t *testing.T) {
	req := &fw.Request{Messages: []fw.Message{fw.UserText("hi")}, Metadata: map[string]string{"runId": "run-7"}}
	for mode, want := range map[string]any{"on": "run-7", "off": nil, "auto": nil} {
		f, srv := newFake(t, reply{body: streamBody})
		m := testModelWith(t, settings{base: srv.URL, model: "local", cacheKey: mode})
		if _, err := m.Generate(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if got := f.bodies[0]["prompt_cache_key"]; got != want {
			t.Errorf("promptCacheKey %s: prompt_cache_key = %v, want %v", mode, got, want)
		}
	}
	// Auto sends the key to api.openai.com only.
	m, err := newModel(settings{base: "https://api.openai.com/v1", model: "gpt-5"})
	if err != nil {
		t.Fatal(err)
	}
	if !m.cacheKey {
		t.Error("auto does not send prompt_cache_key to api.openai.com")
	}
	if o := m.bodyOptions(hello); o.PromptCacheKey != "" {
		t.Errorf("a request without a run id sends prompt_cache_key %q", o.PromptCacheKey)
	}
}

func TestParallelToolCalls(t *testing.T) {
	for mode, want := range map[string]any{"false": false, "true": true, "": nil} {
		f, srv := newFake(t, reply{body: streamBody})
		m := testModelWith(t, settings{base: srv.URL, model: "local", parallel: mode})
		if _, err := m.Generate(context.Background(), hello); err != nil {
			t.Fatal(err)
		}
		if got := f.bodies[0]["parallel_tool_calls"]; got != want {
			t.Errorf("parallelToolCalls %q: parallel_tool_calls = %v, want %v", mode, got, want)
		}
	}
}

func TestMaxTokensField(t *testing.T) {
	f, srv := newFake(t, reply{body: streamBody})
	m := testModelWith(t, settings{base: srv.URL, model: "local", tokenField: "max_completion_tokens"})
	req := &fw.Request{Messages: []fw.Message{fw.UserText("hi")}, MaxTokens: 77}
	if _, err := m.Generate(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	body := f.bodies[0]
	if body["max_completion_tokens"] != float64(77) {
		t.Errorf("max_completion_tokens = %v, want 77", body["max_completion_tokens"])
	}
	if _, ok := body["max_tokens"]; ok {
		t.Error("max_tokens sent with maxTokensField max_completion_tokens")
	}
	if _, ok := body["reasoning_effort"]; ok {
		t.Error("reasoning_effort sent without reasoning")
	}
}

func TestReasoningOption(t *testing.T) {
	clearEnv(t)
	ctx := context.Background()
	cases := []struct {
		model, option, env string
		want               bool
	}{
		{"gpt-5", "", "", true},
		{"llama3", "", "", false},
		{"gpt-5", "off", "", false},
		{"llama3", "on", "", true},
		{"llama3", "auto", "1", true},
		{"gpt-5", "auto", "0", false},
		{"gpt-5", "off", "1", false},
	}
	for _, c := range cases {
		t.Setenv(ReasoningEnv, c.env)
		var opts map[string]string
		if c.option != "" {
			opts = map[string]string{"reasoning": c.option}
		}
		m, err := New(ctx, c.model, providerConfig(fw.ProviderConfig{Options: opts}), secretsOf(nil))
		if err != nil {
			t.Fatal(err)
		}
		if got := m.(*Model).reasoning; got != c.want {
			t.Errorf("model %s option %q env %q: reasoning %v, want %v", c.model, c.option, c.env, got, c.want)
		}
	}
	t.Setenv(ReasoningEnv, "maybe")
	if _, err := New(ctx, "gpt-5", nil, secretsOf(nil)); err == nil {
		t.Error("a bad FW_OPENAI_REASONING is an error")
	}
}

// slowServer answers with the first chunk of a stream and then sends nothing
// until the request ends or the hold time passes.
func slowServer(t *testing.T, hold time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hel\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(hold):
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestIdleTimeout(t *testing.T) {
	srv := slowServer(t, 5*time.Second)
	m := testModelWith(t, settings{base: srv.URL, model: "local", idle: 50 * time.Millisecond})
	start := time.Now()
	_, err := m.Generate(context.Background(), hello)
	if err == nil {
		t.Fatal("no error from a stalled stream")
	}
	if !errors.Is(err, errStalled) || !strings.Contains(err.Error(), "idleTimeout") || !strings.HasPrefix(err.Error(), "openai: stream local: ") {
		t.Errorf("error = %v, want a stall error that names the option", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("the stall took %v to detect", time.Since(start))
	}
}

func TestTimeout(t *testing.T) {
	srv := slowServer(t, 5*time.Second)
	m := testModelWith(t, settings{base: srv.URL, model: "local", timeout: 50 * time.Millisecond})
	_, err := m.Generate(context.Background(), hello)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "providers.openai.timeout") {
		t.Errorf("error = %v, want the configured timeout", err)
	}

	// A server that never answers hits the same limit on Models.
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	t.Cleanup(silent.Close)
	m = testModelWith(t, settings{base: silent.URL, model: "local", timeout: 50 * time.Millisecond})
	if _, err := m.Models(context.Background()); !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "providers.openai.timeout") {
		t.Errorf("Models: error = %v, want the configured timeout", err)
	}
}

func TestCancel(t *testing.T) {
	srv := slowServer(t, 5*time.Second)
	m := testModelWith(t, settings{base: srv.URL, model: "local"})
	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	_, err := m.Stream(ctx, hello, func(fw.Delta) { once.Do(cancel) })
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func TestNetworkErrorRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			// Close the connection before any byte of an answer.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, streamBody)
	}))
	t.Cleanup(srv.Close)
	m := testModelWith(t, settings{base: srv.URL, model: "local"})
	resp, err := m.Generate(context.Background(), hello)
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&calls) != 2 || fw.TextOf(resp.Content) != "Hello" {
		t.Errorf("%d calls, text %q; want 2 and Hello", calls, fw.TextOf(resp.Content))
	}
}

// A connection that is reset before any byte of the answer is retried more
// often than a server error, because under ax the egress policy of a Task
// lands a few seconds after the Task boots.
func TestNetworkErrorRetriesLonger(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) <= int32(maxRetries)+1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, streamBody)
	}))
	t.Cleanup(srv.Close)
	m := testModelWith(t, settings{base: srv.URL, model: "local"})
	resp, err := m.Generate(context.Background(), hello)
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != int32(maxRetries)+2 || fw.TextOf(resp.Content) != "Hello" {
		t.Errorf("%d calls, text %q; want %d and Hello", got, fw.TextOf(resp.Content), maxRetries+2)
	}
}

func TestConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	m := testModelWith(t, settings{base: srv.URL, model: "local"})
	start := time.Now()
	_, err := m.Generate(context.Background(), hello)
	if err == nil || !strings.Contains(err.Error(), "connection refused") || !strings.HasPrefix(err.Error(), "openai: POST ") {
		t.Errorf("error = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("the refused connection took %v with %d retries", time.Since(start), maxRetries)
	}
}

func TestNoRetryAfterStreamStart(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hel\"}}]}\n\n")
		w.(http.Flusher).Flush()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	}))
	t.Cleanup(srv.Close)
	m := testModelWith(t, settings{base: srv.URL, model: "local"})
	if _, err := m.Generate(context.Background(), hello); err == nil {
		t.Fatal("no error from a cut stream")
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("%d calls, want 1: a started stream is never retried", calls)
	}
}

func TestRetryableNetErr(t *testing.T) {
	refused := &net.OpError{Op: "dial", Err: errors.New("connect: connection refused")}
	cases := map[string]struct {
		err  error
		want bool
	}{
		"refused":        {&url.Error{Op: "Post", Err: refused}, true},
		"reset":          {&net.OpError{Op: "read", Err: errors.New("connection reset by peer")}, true},
		"eof":            {&url.Error{Op: "Post", Err: io.EOF}, true},
		"unexpected eof": {io.ErrUnexpectedEOF, true},
		"closed idle":    {errors.New("http: server closed idle connection"), true},
		"dns lookup":     {&net.DNSError{Err: "server misbehaving", IsTemporary: true}, true},
		"no such host":   {&net.DNSError{Err: "no such host", IsNotFound: true}, false},
		"canceled":       {&url.Error{Op: "Post", Err: context.Canceled}, false},
		"deadline":       {context.DeadlineExceeded, false},
		"unknown ca":     {&url.Error{Op: "Post", Err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}}, false},
		"header timeout": {headerTimeout{}, false},
		"something else": {errors.New("boom"), false},
	}
	for name, c := range cases {
		if got := retryableNetErr(c.err); got != c.want {
			t.Errorf("%s: retryable = %v, want %v", name, got, c.want)
		}
	}
}

// headerTimeout mimics the net/http error of a wait for the response headers.
type headerTimeout struct{}

func (headerTimeout) Error() string   { return "net/http: timeout awaiting response headers" }
func (headerTimeout) Timeout() bool   { return true }
func (headerTimeout) Temporary() bool { return true }

// modelServer answers the chat endpoint with one reply and the models
// endpoint with a list.
func modelServer(t *testing.T, chat reply, list string) (*httptest.Server, *fake) {
	t.Helper()
	f := &fake{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.headers = append(f.headers, r.Header.Clone())
		f.paths = append(f.paths, r.URL.RequestURI())
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, list)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.RequestURI())
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(chat.status)
		_, _ = io.WriteString(w, chat.body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, f
}

func TestUnknownModelListsModels(t *testing.T) {
	srv, f := modelServer(t,
		reply{status: http.StatusNotFound, body: "{\"error\":{\"message\":\"The model `gpt-9` does not exist or you do not have access to it.\",\"type\":\"invalid_request_error\",\"code\":\"model_not_found\"}}"},
		`{"object":"list","data":[{"id":"qwen3-32b","object":"model"},{"id":"llama3","object":"model"}]}`,
	)
	m := testModelWith(t, settings{base: srv.URL + "/v1", model: "gpt-9"})
	_, err := m.Generate(context.Background(), hello)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 404 || apiErr.Code != "model_not_found" {
		t.Fatalf("error = %v, want a 404 APIError with the code", err)
	}
	if !strings.Contains(err.Error(), "llama3, qwen3-32b") || !strings.HasPrefix(err.Error(), "openai: POST ") {
		t.Errorf("error = %v, want the sorted model list", err)
	}
	if strings.Join(f.paths, " ") != "/v1/chat/completions /v1/models" {
		t.Errorf("paths = %v", f.paths)
	}
}

func TestNotFoundPathHint(t *testing.T) {
	_, srv := newFake(t, reply{status: http.StatusNotFound, body: "404 page not found"})
	m := testModel(t, srv.URL, "", "local", false)
	_, err := m.Generate(context.Background(), hello)
	if err == nil || !strings.Contains(err.Error(), "providers.openai.baseUrl") {
		t.Errorf("error = %v, want a hint about the base URL", err)
	}
}

func TestAuthErrorNamesSecret(t *testing.T) {
	_, srv := newFake(t, reply{status: http.StatusUnauthorized, body: `{"error":{"message":"Incorrect API key provided: sk-live-9.","type":"invalid_request_error","code":"invalid_api_key"}}`})
	m := testModelWith(t, settings{base: srv.URL, key: "sk-live-9", keyName: "MY_KEY", model: "local"})
	_, err := m.Generate(context.Background(), hello)
	if err == nil || !strings.Contains(err.Error(), "secret MY_KEY") || strings.Contains(err.Error(), "sk-live-9") {
		t.Errorf("error = %v, want the secret name and never the key", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 401 {
		t.Errorf("error %v is not a 401 APIError", err)
	}

	_, srv = newFake(t, reply{status: http.StatusForbidden, body: `{"error":"forbidden"}`})
	m = testModel(t, srv.URL, "", "local", false)
	_, err = m.Generate(context.Background(), hello)
	if err == nil || !strings.Contains(err.Error(), "no API key is set; put it in the secret "+APIKeySecret) {
		t.Errorf("error = %v, want a hint to set the key", err)
	}
	if _, err := m.Models(context.Background()); err == nil || !strings.Contains(err.Error(), APIKeySecret) || !strings.HasPrefix(err.Error(), "openai: GET ") {
		t.Errorf("Models: error = %v, want the same hint", err)
	}
}

func TestTLS(t *testing.T) {
	clearEnv(t)
	ctx := context.Background()
	var calls int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, streamBody)
	}))
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}

	// The system roots do not sign the test certificate, and a certificate
	// error is not retried.
	m, err := New(ctx, "local", providerConfig(fw.ProviderConfig{BaseURL: srv.URL}), secretsOf(nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Generate(ctx, hello); err == nil || !strings.Contains(strings.ToLower(err.Error()), "certificate") {
		t.Errorf("without a CA: error = %v, want a certificate error", err)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Errorf("%d calls reached the server without a trusted certificate", calls)
	}

	// The CA bundle makes the connection work.
	m, err = New(ctx, "local", providerConfig(fw.ProviderConfig{BaseURL: srv.URL, TLS: fw.TLSConfig{CA: ca}}), secretsOf(nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Generate(ctx, hello); err != nil {
		t.Errorf("with the CA: %v", err)
	}

	// So does insecure, for development.
	m, err = New(ctx, "local", providerConfig(fw.ProviderConfig{BaseURL: srv.URL, TLS: fw.TLSConfig{Insecure: true}}), secretsOf(nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Generate(ctx, hello); err != nil {
		t.Errorf("insecure: %v", err)
	}

	// Bad TLS settings fail at New.
	for name, tc := range map[string]fw.TLSConfig{
		"missing ca":   {CA: filepath.Join(t.TempDir(), "none.pem")},
		"not a pem":    {CA: ca + ".txt"},
		"cert alone":   {Cert: ca},
		"bad key pair": {Cert: ca, Key: ca},
	} {
		if name == "not a pem" {
			if err := os.WriteFile(tc.CA, []byte("hello"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		_, err := New(ctx, "local", providerConfig(fw.ProviderConfig{BaseURL: srv.URL, TLS: tc}), secretsOf(nil))
		if err == nil || !strings.HasPrefix(err.Error(), "openai: ") {
			t.Errorf("%s: error = %v", name, err)
		}
	}
}

func TestModels(t *testing.T) {
	srv, f := modelServer(t, reply{status: http.StatusOK, body: ""},
		`{"object":"list","data":[{"id":"b"},{"id":"a"},{"id":"a"},{"id":""}]}`)
	m := testModelWith(t, settings{base: srv.URL + "/v1/", key: "sk-test", model: "local"})
	ids, err := m.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids, []string{"a", "b"}) {
		t.Errorf("ids = %v, want a and b", ids)
	}
	if f.paths[0] != "/v1/models" || f.headers[0].Get("Authorization") != "Bearer sk-test" || f.headers[0].Get("Accept") != "application/json" {
		t.Errorf("request = %s %v", f.paths[0], f.headers[0])
	}
	var lister Lister = m
	if _, err := lister.Models(context.Background()); err != nil {
		t.Error(err)
	}

	_, srv2 := newFake(t, reply{header: map[string]string{"Content-Type": "application/json"}, body: `not json`})
	m = testModel(t, srv2.URL, "", "local", false)
	if _, err := m.Models(context.Background()); err == nil || !strings.HasPrefix(err.Error(), "openai: GET ") {
		t.Errorf("bad list: error = %v", err)
	}
}

func TestResponseTooLarge(t *testing.T) {
	_, srv := newFake(t, reply{body: streamBody})
	m := testModel(t, srv.URL, "", "local", false)
	m.maxBytes = 64
	_, err := m.Generate(context.Background(), hello)
	if !errors.Is(err, errTooLarge) || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("error = %v, want the byte cap", err)
	}
}

func TestUsageReasoningTokens(t *testing.T) {
	cases := []struct {
		usage string
		want  fw.Usage
	}{
		// OpenAI counts the reasoning tokens inside completion_tokens.
		{`{"prompt_tokens":100,"completion_tokens":30,"prompt_tokens_details":{"cached_tokens":40},"completion_tokens_details":{"reasoning_tokens":10}}`,
			fw.Usage{InputTokens: 60, OutputTokens: 30, CacheReadTokens: 40}},
		// A server that counts them apart reports more reasoning than completion tokens.
		{`{"prompt_tokens":100,"completion_tokens":5,"completion_tokens_details":{"reasoning_tokens":10}}`,
			fw.Usage{InputTokens: 100, OutputTokens: 15}},
		{`{"prompt_tokens":100,"completion_tokens":30}`, fw.Usage{InputTokens: 100, OutputTokens: 30}},
	}
	for i, c := range cases {
		acc := newAccumulator(nil, nil)
		body := sse(`{"choices":[{"delta":{"content":"a"},"finish_reason":"stop"}]}`, `{"choices":[],"usage":`+c.usage+`}`)
		if err := acc.readSSE(strings.NewReader(body)); err != nil {
			t.Fatal(err)
		}
		if acc.usage != c.want {
			t.Errorf("case %d: usage = %+v, want %+v", i, acc.usage, c.want)
		}
	}
}

func TestToolErrorPrefix(t *testing.T) {
	for text, want := range map[string]string{
		"conflict":                     "Error: conflict",
		"Error: the path is denied.":   "Error: the path is denied.",
		"error: lower case stays once": "error: lower case stays once",
	} {
		m, _, err := toolMessage(fw.Block{Type: fw.BlockToolResult, ToolUseID: "c1", IsError: true, Content: []fw.Block{fw.Text(text)}})
		if err != nil || m.Content != want {
			t.Errorf("toolMessage(%q) = %v, %v; want %q", text, m.Content, err, want)
		}
	}
}

func TestMediaTypes(t *testing.T) {
	for _, tc := range []struct {
		option, base string
		want         []string
	}{
		{"auto", "http://127.0.0.1:8080/v1", []string{"image/png", "image/jpeg", "image/gif", "image/webp"}},
		{"", "https://api.openai.com/v1", []string{"image/png", "image/jpeg", "image/gif", "image/webp", "application/pdf"}},
		{"none", "https://api.openai.com/v1", nil},
		{"image/png, application/pdf", "http://127.0.0.1:8080/v1", []string{"image/png", "application/pdf"}},
	} {
		m, err := newModel(settings{model: "m", base: tc.base, mediaTypes: tc.option})
		if err != nil {
			t.Fatal(err)
		}
		if got := fw.MediaTypesOf(m); !slices.Equal(got, tc.want) {
			t.Errorf("mediaTypes %q at %s = %v, want %v", tc.option, tc.base, got, tc.want)
		}
	}
}

// A request that sets an empty effort sends no reasoning_effort, and not
// the effort that the adapter was opened with. A request that leaves the
// effort to the adapter sends that effort.
func TestReasoningKeepsAnEmptyEffortOfTheRequest(t *testing.T) {
	f, srv := newFake(t, reply{body: streamBody})
	m := testModelWith(t, settings{base: srv.URL, model: "o4-mini", reasoning: true, effort: "low"})
	for _, set := range []bool{false, true} {
		req := &fw.Request{Messages: []fw.Message{fw.UserText("q")}, EffortSet: set}
		if _, err := m.Generate(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	if f.bodies[0]["reasoning_effort"] != "low" {
		t.Errorf("body = %v, want the effort of the adapter, low", f.bodies[0])
	}
	if _, ok := f.bodies[1]["reasoning_effort"]; ok {
		t.Errorf("body = %v, want no reasoning_effort", f.bodies[1])
	}
}
