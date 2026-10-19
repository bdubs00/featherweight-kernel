package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// endBody is one streamed turn that ends with a short text.
var endBody = sse(
	[2]string{"message_start", `{"type":"message_start","message":{"id":"msg_02","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":1}}}`},
	[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
	[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Done."}}`},
	[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
	[2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null,"stop_details":null},"usage":{"output_tokens":2}}`},
	[2]string{"message_stop", `{"type":"message_stop"}`},
)

// A request that sets its effort, also to empty, keeps it: an empty one
// takes the default of the adapter, not the effort that the model was
// opened with. A request that leaves the effort to the model takes that.
func TestStreamKeepsTheEffortOfTheRequest(t *testing.T) {
	t.Setenv(FallbacksEnv, "")
	srv, seen := fakeServer(t, http.StatusOK, endBody)
	m := testModel(srv) // opened with the effort medium
	for _, c := range []struct {
		req  fw.Request
		want string
	}{
		{fw.Request{Messages: []fw.Message{fw.UserText("hi")}}, "medium"},
		{fw.Request{Messages: []fw.Message{fw.UserText("hi")}, EffortSet: true}, DefaultEffort},
		{fw.Request{Messages: []fw.Message{fw.UserText("hi")}, Effort: "low", EffortSet: true}, "low"},
	} {
		if _, err := m.Stream(context.Background(), &c.req, func(fw.Delta) {}); err != nil {
			t.Fatal(err)
		}
		if got := seen.body["output_config"].(map[string]any)["effort"]; got != c.want {
			t.Errorf("effort of %+v = %v, want %s", c.req, got, c.want)
		}
	}
}

// A run that the kernel queued while the configuration named no effort
// keeps the default of the adapter after a restart with the effort max in
// the configuration. A new run takes max. The adapter builds each request
// and sends it to a local server.
func TestRunKeepsItsEffortAcrossARestart(t *testing.T) {
	t.Setenv(FallbacksEnv, "")
	t.Setenv("FW_ATESPACE", "")
	var mu sync.Mutex
	var efforts []any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		oc, _ := body["output_config"].(map[string]any)
		mu.Lock()
		efforts = append(efforts, oc["effort"])
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, endBody)
	}))
	t.Cleanup(srv.Close)
	t.Setenv(BaseURLEnv, srv.URL)
	dir := filepath.Join(t.TempDir(), "runs")
	start := func(effort string) *fw.Agent {
		t.Helper()
		cfg := fw.DefaultConfig()
		cfg.Home = t.TempDir()
		cfg.Model = "anthropic/claude-opus-5-5"
		cfg.Effort = effort
		l, err := fw.OpenFileLedger(dir)
		if err != nil {
			t.Fatal(err)
		}
		a, err := fw.New(fw.WithoutDefaults(), fw.WithConfig(cfg), fw.WithAuthn(fw.NoAuthn{}), fw.WithLedger(l),
			fw.WithBus(fw.NewMemBus()), fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
			fw.WithSecrets(fw.SecretsFunc(func(context.Context, string) (string, error) { return "sk-test-key-for-a-local-server", nil })))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { closeAgent(a) })
		return a
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	a := start("")
	r, err := a.Run(ctx, "work")
	if err != nil || r.State != fw.StateCompleted {
		t.Fatalf("run = %+v, %v", r, err)
	}
	closeAgent(a)

	b := start("max")
	if err := b.Send(ctx, r.ID, fw.UserText("again"), fw.OriginCLI); err != nil {
		t.Fatal(err)
	}
	if r, err = b.Wait(ctx, r.ID); err != nil || r.State != fw.StateCompleted {
		t.Fatalf("follow-up = %+v, %v", r, err)
	}
	if fresh, err := b.Run(ctx, "fresh"); err != nil || fresh.State != fw.StateCompleted {
		t.Fatalf("new run = %+v, %v", fresh, err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []any{DefaultEffort, DefaultEffort, "max"}
	if len(efforts) != len(want) {
		t.Fatalf("efforts = %v, want %v", efforts, want)
	}
	for i := range want {
		if efforts[i] != want[i] {
			t.Fatalf("efforts = %v, want %v", efforts, want)
		}
	}
}

func closeAgent(a *fw.Agent) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a.Close(ctx)
}
