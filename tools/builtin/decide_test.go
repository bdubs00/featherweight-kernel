package builtin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// recordedAnswer is what Clef answered to the noul of the model card
// example ("Is a service down?").
const recordedAnswer = `{"model": "clef-exl3-4.0bpw", "answers": {"outage": {"type": "noul", "noul": 0.8963}}, "usage": {"input_tokens": 303, "output_tokens": 0}}`

func TestDecideOffWithoutDecider(t *testing.T) {
	tool, err := fw.NewBuiltinTool("decide", fw.DefaultConfig())
	if err != nil || tool != nil {
		t.Fatalf("decide without a decider = %v, %v; want no tool", tool, err)
	}
}

func TestDecide(t *testing.T) {
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &sent)
		_, _ = io.WriteString(w, recordedAnswer)
	}))
	defer srv.Close()
	cfg := fw.DefaultConfig()
	cfg.Deciders = map[string]fw.DeciderConfig{"clef": {BaseURL: srv.URL, Model: "clef"}}
	a, err := fw.New(fw.WithoutDefaults(), fw.WithConfig(cfg), fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()), fw.WithAuthn(fw.NoAuthn{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(context.Background()) })
	tool, err := fw.NewBuiltinTool("decide", cfg)
	if err != nil || tool == nil {
		t.Fatalf("decide = %v, %v", tool, err)
	}
	if s := tool.Spec(); !s.ReadOnly || !s.Idempotent || s.Source != Source || s.Title == "" {
		t.Errorf("spec = %+v", s)
	}
	ctx := fw.WithAgent(context.Background(), a)
	// One decider needs no name.
	res := call(t, ctx, tool, `{"state": "The checkout is down.", "questions": {"outage": {"type": "noul", "instructions": "Is a service down?"}}}`)
	var d fw.DecideResult
	if err := json.Unmarshal([]byte(mustOK(t, res)), &d); err != nil {
		t.Fatal(err)
	}
	if d.Model != "clef-exl3-4.0bpw" || d.Answers["outage"].Probability != 0.8963 || d.InputTokens != 303 {
		t.Errorf("decision = %+v", d)
	}
	if sent["model"] != "clef" || sent["state"] != "The checkout is down." {
		t.Errorf("sent = %v", sent)
	}
	mustFail(t, call(t, ctx, tool, `{"questions": {}}`), "at least one question")
	mustFail(t, call(t, ctx, tool, `{"state": "x", "questions": {"team": {"type": "choice"}}}`), "the criteria of a choice")
	mustFail(t, call(t, ctx, tool, `{"decider": "jev", "state": "x", "questions": {"outage": {"type": "noul"}}}`), `no decider "jev"`)
	if !strings.Contains(tool.Spec().Description, "probabilities") {
		t.Error("description")
	}
}
