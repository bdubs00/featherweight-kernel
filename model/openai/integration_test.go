package openai

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// TestIntegration calls a real server. It runs only when OPENAI_API_KEY is
// set, and it never logs a request or response body. FW_OPENAI_TEST_MODEL
// names the model, default gpt-5-mini; OPENAI_BASE_URL points it at another
// server.
func TestIntegration(t *testing.T) {
	if os.Getenv(APIKeySecret) == "" {
		t.Skip("OPENAI_API_KEY is not set")
	}
	if testing.Short() {
		t.Skip("skipped in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	model := os.Getenv("FW_OPENAI_TEST_MODEL")
	if model == "" {
		model = "gpt-5-mini"
	}
	m, err := New(ctx, model, fw.DefaultConfig(), fw.EnvSecrets{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := &fw.Request{
		System:    "You answer questions about the weather. Use the get_weather tool to look up the weather.",
		Messages:  []fw.Message{fw.UserText("What is the weather in Paris right now? Use the get_weather tool.")},
		Effort:    "low",
		MaxTokens: 4000,
		Tools: []fw.ToolSpec{{
			Name:        "weather.get",
			Description: "Get the current weather for a city. Call it when the user asks about the weather.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string","description":"The city name"}},"required":["city"]}`),
		}},
	}
	var texts int
	resp, err := m.Stream(ctx, req, func(d fw.Delta) {
		if d.Type == "text" {
			texts++
		}
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	t.Logf("model %s, stop %s, usage %+v, %d blocks, %d text deltas",
		resp.Model, resp.StopReason, resp.Usage, len(resp.Content), texts)
	if resp.Usage.InputTokens+resp.Usage.CacheReadTokens == 0 || resp.Usage.OutputTokens == 0 {
		t.Errorf("usage = %+v, want input and output tokens", resp.Usage)
	}
	if resp.StopReason != fw.StopToolUse {
		t.Logf("the model answered without the tool: stop %s", resp.StopReason)
		return
	}
	uses := resp.ToolUses()
	if len(uses) == 0 || uses[0].Name != "weather.get" {
		t.Fatalf("tool uses = %+v, want weather.get", uses)
	}
	req.Messages = append(req.Messages,
		fw.Message{Role: fw.RoleAssistant, Content: resp.Content},
		fw.Message{Role: fw.RoleUser, Content: []fw.Block{{
			Type: fw.BlockToolResult, ToolUseID: uses[0].ID,
			Content: []fw.Block{fw.Text("18 C and sunny")},
		}}},
	)
	final, err := m.Generate(ctx, req)
	if err != nil {
		t.Fatalf("second turn: %v", err)
	}
	t.Logf("second turn: stop %s, usage %+v", final.StopReason, final.Usage)
	if final.StopReason != fw.StopEndTurn || fw.TextOf(final.Content) == "" {
		t.Errorf("second turn: stop %s with %d text characters, want an answer", final.StopReason, len(fw.TextOf(final.Content)))
	}
}
