package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func TestStopReasons(t *testing.T) {
	call := []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: "f"}}}
	text := []*genai.Part{{Text: "hi"}}
	cases := []struct {
		parts   []*genai.Part
		finish  genai.FinishReason
		blocked genai.BlockedReason
		want    fw.StopReason
		detail  string
	}{
		{text, genai.FinishReasonStop, "", fw.StopEndTurn, ""},
		{call, genai.FinishReasonStop, "", fw.StopToolUse, ""},
		{call, genai.FinishReasonMaxTokens, "", fw.StopMaxTokens, ""},
		{nil, genai.FinishReasonSafety, "", fw.StopRefusal, "SAFETY"},
		{text, genai.FinishReasonProhibitedContent, "", fw.StopRefusal, "PROHIBITED_CONTENT"},
		{nil, "", genai.BlockedReasonSafety, fw.StopRefusal, "SAFETY"},
		{call, genai.FinishReasonMalformedFunctionCall, "", fw.StopOther, "MALFORMED_FUNCTION_CALL"},
		{text, genai.FinishReasonRecitation, "", fw.StopOther, "RECITATION"},
	}
	for i, c := range cases {
		r := &genai.GenerateContentResponse{}
		if c.blocked != "" {
			r.PromptFeedback = &genai.GenerateContentResponsePromptFeedback{BlockReason: c.blocked}
		} else {
			r.Candidates = []*genai.Candidate{{Content: &genai.Content{Parts: c.parts}, FinishReason: c.finish}}
		}
		resp, err := MapResponse(r, nil)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if resp.StopReason != c.want || resp.StopDetail != c.detail {
			t.Errorf("case %d: stop = %s %q, want %s %q", i, resp.StopReason, resp.StopDetail, c.want, c.detail)
		}
	}
}

func TestUsage(t *testing.T) {
	r := &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{Content: &genai.Content{Parts: []*genai.Part{{Text: "x"}}}, FinishReason: genai.FinishReasonStop}},
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:        100,
			CachedContentTokenCount: 60,
			ToolUsePromptTokenCount: 5,
			CandidatesTokenCount:    7,
			ThoughtsTokenCount:      30,
		},
		ModelVersion: "gemini-3.8-flash-001",
	}
	resp, err := MapResponse(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := fw.Usage{InputTokens: 45, OutputTokens: 37, CacheReadTokens: 60}
	if resp.Usage != want {
		t.Errorf("usage = %+v, want %+v", resp.Usage, want)
	}
	if resp.Model != "gemini-3.8-flash-001" || resp.Provider != Provider {
		t.Errorf("model %q provider %q", resp.Model, resp.Provider)
	}
	jsonEqual(t, resp.Raw, `{"role":"model","parts":[{"text":"x"}]}`)
}

// streamChunks is one streamed turn: a thought, a text in two fragments, a
// signature in a chunk of its own, then a signed function call, then usage.
var streamChunks = []string{
	`{"candidates":[{"content":{"role":"model","parts":[{"text":"plan","thought":true}]}}]}`,
	`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hel"}]}}]}`,
	`{"candidates":[{"content":{"role":"model","parts":[{"text":"lo"}]}}]}`,
	`{"candidates":[{"content":{"role":"model","parts":[{"text":"","thoughtSignature":"c2lnMQ=="}]}}]}`,
	`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"git.commit","args":{"msg":"fix"}},"thoughtSignature":"c2lnMg=="}]},"finishReason":"STOP"}],` +
		`"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":8,"thoughtsTokenCount":4,"totalTokenCount":24},"modelVersion":"gemini-test-001"}`,
}

func fakeServer(t *testing.T, status int, chunks []string) (*httptest.Server, *http.Request, *[]byte) {
	t.Helper()
	var (
		got  http.Request
		body []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = *r.Clone(context.Background())
		body, _ = io.ReadAll(r.Body)
		if status != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			fmt.Fprint(w, `{"error":{"code":400,"message":"Invalid argument: bad schema","status":"INVALID_ARGUMENT"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\r\n\r\n", c)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &got, &body
}

func testModel(t *testing.T, srv *httptest.Server) *Model {
	t.Helper()
	m, err := newModel(context.Background(), "gemini-3.8-flash", "low", &genai.ClientConfig{
		APIKey:      "test-key",
		Backend:     genai.BackendGeminiAPI,
		HTTPOptions: genai.HTTPOptions{BaseURL: srv.URL + "/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestStream(t *testing.T) {
	srv, got, body := fakeServer(t, http.StatusOK, streamChunks)
	m := testModel(t, srv)
	req := &fw.Request{
		System:   "sys",
		Messages: []fw.Message{fw.UserText("hi")},
		Tools:    []fw.ToolSpec{{Name: "git.commit", InputSchema: json.RawMessage(`{"type":"object","properties":{"msg":{"type":"string"}}}`)}},
	}
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
	if got.Header.Get("x-goog-api-key") != "test-key" {
		t.Errorf("api key header missing")
	}
	if !strings.Contains(got.URL.Path, "gemini-3.8-flash:streamGenerateContent") {
		t.Errorf("path = %s", got.URL.Path)
	}
	var sent map[string]any
	if err := json.Unmarshal(*body, &sent); err != nil {
		t.Fatal(err)
	}
	gc, _ := sent["generationConfig"].(map[string]any)
	if tc, _ := gc["thinkingConfig"].(map[string]any); tc["thinkingLevel"] != "LOW" {
		t.Errorf("generationConfig = %v, want thinking level LOW from the model effort", gc)
	}

	if strings.Join(texts, "|") != "Hel|lo" {
		t.Errorf("text deltas = %q", texts)
	}
	if strings.Join(blocks, " ") != "opaque opaque text opaque tool_use" {
		t.Errorf("block deltas = %v", blocks)
	}
	if len(resp.Content) != 5 || fw.TextOf(resp.Content) != "Hello" {
		t.Fatalf("content = %+v", resp.Content)
	}
	var carrier genai.Part
	if err := json.Unmarshal(resp.Content[1].Raw, &carrier); err != nil || carrier.Text != "Hello" || string(carrier.ThoughtSignature) != "sig1" {
		t.Errorf("text carrier = %s (%v)", resp.Content[1].Raw, err)
	}
	use := resp.ToolUses()
	if len(use) != 1 || use[0].Name != "git.commit" || string(use[0].Input) != `{"msg":"fix"}` {
		t.Errorf("tool uses = %+v", use)
	}
	if resp.StopReason != fw.StopToolUse || resp.Model != "gemini-test-001" {
		t.Errorf("stop %s model %s", resp.StopReason, resp.Model)
	}
	if resp.Usage != (fw.Usage{InputTokens: 12, OutputTokens: 12}) {
		t.Errorf("usage = %+v", resp.Usage)
	}
	jsonEqual(t, resp.Raw, `{"role":"model","parts":[
	  {"text":"plan","thought":true},
	  {"text":"Hello","thoughtSignature":"c2lnMQ=="},
	  {"functionCall":{"name":"git.commit","args":{"msg":"fix"}},"thoughtSignature":"c2lnMg=="}
	]}`)
}

func TestStreamError(t *testing.T) {
	srv, _, _ := fakeServer(t, http.StatusBadRequest, nil)
	m := testModel(t, srv)
	_, err := m.Generate(context.Background(), &fw.Request{Messages: []fw.Message{fw.UserText("hi")}})
	if err == nil {
		t.Fatal("no error")
	}
	var apiErr genai.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != 400 {
		t.Errorf("error %v does not wrap a 400 APIError", err)
	}
	if !strings.HasPrefix(err.Error(), "gemini: ") || !strings.Contains(err.Error(), "bad schema") || strings.Contains(err.Error(), "test-key") {
		t.Errorf("error = %v", err)
	}
}

func TestNewKeys(t *testing.T) {
	ctx := context.Background()
	secrets := func(vals map[string]string) fw.Secrets {
		return fw.SecretsFunc(func(_ context.Context, name string) (string, error) {
			if v, ok := vals[name]; ok {
				return v, nil
			}
			return "", fmt.Errorf("%w: %s", fw.ErrSecretNotFound, name)
		})
	}
	if _, err := New(ctx, "", nil, secrets(nil)); !errors.Is(err, fw.ErrSecretNotFound) {
		t.Errorf("no key: error = %v, want ErrSecretNotFound", err)
	}
	m, err := New(ctx, "", nil, secrets(map[string]string{AltAPIKeySecret: "alt"}))
	if err != nil {
		t.Fatalf("fallback key: %v", err)
	}
	if m.(*Model).Name() != DefaultModel {
		t.Errorf("model = %s, want %s", m.(*Model).Name(), DefaultModel)
	}
	if _, err := New(ctx, "", &fw.Config{Effort: "turbo"}, secrets(map[string]string{APIKeySecret: "k"})); err == nil {
		t.Error("an unknown effort should fail")
	}
	if !contains(fw.RegisteredModels(), Provider) {
		t.Errorf("provider %q is not registered", Provider)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestProviderConfig(t *testing.T) {
	t.Setenv(BaseURLEnv, "http://127.0.0.1:1") // loses to the configured base URL
	srv, got, _ := fakeServer(t, http.StatusOK, streamChunks)
	cfg := fw.DefaultConfig()
	cfg.Providers = map[string]fw.ProviderConfig{Provider: {
		BaseURL:   srv.URL,
		APIKeyEnv: "MY_GEMINI_KEY",
		Headers:   map[string]string{"X-Team": "platform", "X-Token": "TEAM_TOKEN", "X-Plain": "NOT_A_SECRET"},
		Timeout:   time.Minute,
	}}
	secrets := fw.SecretsFunc(func(_ context.Context, name string) (string, error) {
		switch name {
		case "MY_GEMINI_KEY":
			return "key-mine", nil
		case "TEAM_TOKEN":
			return "tok-1", nil
		}
		return "", fmt.Errorf("%w: %s", fw.ErrSecretNotFound, name)
	})
	m, err := New(context.Background(), "", cfg, secrets)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := m.Generate(context.Background(), &fw.Request{Messages: []fw.Message{fw.UserText("hi")}})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if fw.TextOf(resp.Content) != "Hello" {
		t.Errorf("text = %q", fw.TextOf(resp.Content))
	}
	for name, want := range map[string]string{
		"x-goog-api-key": "key-mine",
		"X-Team":         "platform",
		"X-Token":        "tok-1",
		"X-Plain":        "NOT_A_SECRET",
	} {
		if v := got.Header.Get(name); v != want {
			t.Errorf("header %s = %q, want %q", name, v, want)
		}
	}

	// A missing custom key names the custom secret.
	none := fw.SecretsFunc(func(context.Context, string) (string, error) { return "", fw.ErrSecretNotFound })
	_, err = New(context.Background(), "", cfg, none)
	if err == nil || !strings.Contains(err.Error(), "MY_GEMINI_KEY") || !errors.Is(err, fw.ErrSecretNotFound) {
		t.Errorf("missing custom key: err = %v, want the custom secret name", err)
	}
}
