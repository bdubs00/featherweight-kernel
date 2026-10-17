package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// sse renders server-sent events in the order given.
func sse(events ...[2]string) string {
	var sb strings.Builder
	for _, e := range events {
		fmt.Fprintf(&sb, "event: %s\ndata: %s\n\n", e[0], e[1])
	}
	return sb.String()
}

// streamBody is one streamed turn: a thinking block, a text block in two
// deltas, and a call to the native text editor with its input in two deltas.
var streamBody = sse(
	[2]string{"message_start", `{"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":9,"output_tokens":1,"cache_read_input_tokens":100,"cache_creation_input_tokens":7}}}`},
	[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`},
	[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"c2ln"}}`},
	[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
	[2]string{"ping", `{"type":"ping"}`},
	[2]string{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`},
	[2]string{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hel"}}`},
	[2]string{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"lo"}}`},
	[2]string{"content_block_stop", `{"type":"content_block_stop","index":1}`},
	[2]string{"content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_01","name":"str_replace_based_edit_tool","input":{}}}`},
	[2]string{"content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"view\","}}`},
	[2]string{"content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"path\":\"/a.go\"}"}}`},
	[2]string{"content_block_stop", `{"type":"content_block_stop","index":2}`},
	[2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null,"stop_details":null},"usage":{"output_tokens":42}}`},
	[2]string{"message_stop", `{"type":"message_stop"}`},
)

// captured is what the fake server saw.
type captured struct {
	header http.Header
	body   map[string]any
}

func fakeServer(t *testing.T, status int, body string) (*httptest.Server, *captured) {
	t.Helper()
	c := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.header = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &c.body)
		if status != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func testModel(srv *httptest.Server) *Model {
	return newModel("", "medium", 0,
		option.WithoutEnvironmentDefaults(),
		option.WithAPIKey("sk-test"),
		option.WithBaseURL(srv.URL),
		option.WithMaxRetries(0),
	)
}

var editRequest = &fw.Request{
	System:   "You edit Go code.",
	Messages: []fw.Message{fw.UserText("Show me a.go.")},
	Tools:    []fw.ToolSpec{{Name: "files", Native: fw.NativeTextEditor}},
}

func TestStream(t *testing.T) {
	t.Setenv(FallbacksEnv, "")
	srv, seen := fakeServer(t, http.StatusOK, streamBody)
	m := testModel(srv)

	var deltas []fw.Delta
	resp, err := m.Stream(context.Background(), editRequest, func(d fw.Delta) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// The request.
	if got := seen.header.Get("X-Api-Key"); got != "sk-test" {
		t.Errorf("x-api-key = %q, want the key from secrets", got)
	}
	if got := seen.header.Get("Anthropic-Beta"); !strings.Contains(got, "server-side-fallback-2026-07-01") {
		t.Errorf("anthropic-beta = %q, want server-side-fallback-2026-07-01", got)
	}
	if seen.body["stream"] != true {
		t.Errorf("stream = %v, want true", seen.body["stream"])
	}
	if seen.body["fallbacks"] != "default" {
		t.Errorf("fallbacks = %v, want default", seen.body["fallbacks"])
	}
	if got := seen.body["output_config"].(map[string]any)["effort"]; got != "medium" {
		t.Errorf("effort = %v, want the configured effort medium", got)
	}
	if _, ok := seen.body["thinking"]; ok {
		t.Errorf("thinking is set")
	}

	// The deltas.
	var text string
	var blocks []fw.Block
	for _, d := range deltas {
		switch d.Type {
		case "text":
			text += d.Text
		case "block":
			blocks = append(blocks, *d.Block)
		default:
			t.Errorf("unknown delta type %q", d.Type)
		}
	}
	if text != "Hello" {
		t.Errorf("text deltas = %q, want Hello", text)
	}
	if len(blocks) != 3 || blocks[0].Type != fw.BlockOpaque || blocks[1].Text != "Hello" || blocks[2].Name != "files" {
		t.Errorf("block deltas = %+v", blocks)
	}

	// The response.
	if resp.StopReason != fw.StopToolUse || resp.Model != "claude-opus-5-5" || resp.Provider != "anthropic" {
		t.Errorf("response = %+v", resp)
	}
	want := fw.Usage{InputTokens: 9, OutputTokens: 42, CacheReadTokens: 100, CacheWriteTokens: 7}
	if resp.Usage != want {
		t.Errorf("usage = %+v, want %+v", resp.Usage, want)
	}
	if !reflect.DeepEqual(decode(t, string(resp.Content[0].Raw)), decode(t, `{"type":"thinking","thinking":"","signature":"c2ln"}`)) {
		t.Errorf("thinking raw = %s", resp.Content[0].Raw)
	}
	use := resp.ToolUses()
	if len(use) != 1 || use[0].Name != "files" || !reflect.DeepEqual(decode(t, string(use[0].Input)), decode(t, `{"command":"view","path":"/a.go"}`)) {
		t.Errorf("tool uses = %+v", use)
	}
	if !json.Valid(resp.Raw) {
		t.Errorf("raw is not valid JSON: %s", resp.Raw)
	}

	// Generate returns the same response.
	gen, err := m.Generate(context.Background(), editRequest)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !reflect.DeepEqual(gen, resp) {
		t.Errorf("Generate and Stream differ:\n%+v\n%+v", gen, resp)
	}
}

func TestStreamAPIError(t *testing.T) {
	srv, _ := fakeServer(t, http.StatusBadRequest,
		`{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: too large"}}`)
	_, err := testModel(srv).Generate(context.Background(), editRequest)
	if err == nil {
		t.Fatal("want an error")
	}
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v does not wrap *anthropic.Error", err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", apiErr.StatusCode)
	}
	if !strings.HasPrefix(err.Error(), "anthropic: ") {
		t.Errorf("error %q does not name the package", err)
	}
	if strings.Contains(err.Error(), "sk-test") {
		t.Errorf("error leaks the API key: %v", err)
	}
}

func TestStreamMidStreamError(t *testing.T) {
	body := sse(
		[2]string{"message_start", `{"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":1}}}`},
		[2]string{"error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`},
	)
	srv, _ := fakeServer(t, http.StatusOK, body)
	if _, err := testModel(srv).Generate(context.Background(), editRequest); err == nil {
		t.Fatal("mid-stream error: want an error")
	}
}

func TestNew(t *testing.T) {
	if !slices.Contains(fw.RegisteredModels(), "anthropic") {
		t.Errorf("provider anthropic is not registered: %v", fw.RegisteredModels())
	}

	none := fw.SecretsFunc(func(context.Context, string) (string, error) { return "", fw.ErrSecretNotFound })
	_, err := New(context.Background(), "", fw.DefaultConfig(), none)
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") || !errors.Is(err, fw.ErrSecretNotFound) {
		t.Errorf("missing key: err = %v, want a clear error about ANTHROPIC_API_KEY", err)
	}
	empty := fw.SecretsFunc(func(context.Context, string) (string, error) { return "", nil })
	if _, err := New(context.Background(), "", nil, empty); err == nil {
		t.Errorf("empty key: want an error")
	}
	if _, err := New(context.Background(), "", nil, nil); err == nil {
		t.Errorf("nil secrets: want an error")
	}
	bad := fw.DefaultConfig()
	bad.Effort = "ultra"
	if _, err := New(context.Background(), "", bad, fw.SecretsFunc(func(context.Context, string) (string, error) { return "sk-test", nil })); err == nil {
		t.Errorf("unknown configured effort: want an error")
	}

	var asked string
	key := fw.SecretsFunc(func(_ context.Context, name string) (string, error) { asked = name; return "sk-test", nil })
	cfg := fw.DefaultConfig()
	cfg.Effort = "low"
	cfg.Tools.SearchThreshold = 40
	got, err := New(context.Background(), "", cfg, key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if asked != "ANTHROPIC_API_KEY" {
		t.Errorf("asked secrets for %q", asked)
	}
	m := got.(*Model)
	if m.Name() != DefaultModel || m.effort != "low" || m.threshold != 40 {
		t.Errorf("model = %q effort %q threshold %d", m.Name(), m.effort, m.threshold)
	}

	m2, err := fw.NewModel(context.Background(), "anthropic/claude-fable-5-1", cfg, key)
	if err != nil {
		t.Fatalf("fw.NewModel: %v", err)
	}
	if name := m2.(*Model).Name(); name != "claude-fable-5-1" {
		t.Errorf("selector model = %q, want claude-fable-5-1", name)
	}
}

func TestProviderConfig(t *testing.T) {
	t.Setenv(FallbacksEnv, "")
	t.Setenv(BaseURLEnv, "http://127.0.0.1:1") // loses to the configured base URL
	srv, seen := fakeServer(t, http.StatusOK, streamBody)
	cfg := fw.DefaultConfig()
	cfg.Providers = map[string]fw.ProviderConfig{Provider: {
		BaseURL:   srv.URL,
		APIKeyEnv: "MY_ANTHROPIC_KEY",
		Headers:   map[string]string{"X-Team": "platform", "X-Token": "TEAM_TOKEN", "X-Plain": "NOT_A_SECRET"},
		Timeout:   time.Minute,
	}}
	secrets := fw.SecretsFunc(func(_ context.Context, name string) (string, error) {
		switch name {
		case "MY_ANTHROPIC_KEY":
			return "sk-mine", nil
		case "TEAM_TOKEN":
			return "tok-1", nil
		}
		return "", fmt.Errorf("%w: %s", fw.ErrSecretNotFound, name)
	})
	m, err := New(context.Background(), "", cfg, secrets)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := m.Generate(context.Background(), editRequest); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for name, want := range map[string]string{
		"X-Api-Key": "sk-mine",
		"X-Team":    "platform",
		"X-Token":   "tok-1",
		"X-Plain":   "NOT_A_SECRET",
	} {
		if got := seen.header.Get(name); got != want {
			t.Errorf("header %s = %q, want %q", name, got, want)
		}
	}

	// A missing custom key names the custom secret.
	none := fw.SecretsFunc(func(context.Context, string) (string, error) { return "", fw.ErrSecretNotFound })
	_, err = New(context.Background(), "", cfg, none)
	if err == nil || !strings.Contains(err.Error(), "MY_ANTHROPIC_KEY") {
		t.Errorf("missing custom key: err = %v, want the custom secret name", err)
	}
}
