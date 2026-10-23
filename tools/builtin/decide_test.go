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

	_ "github.com/bdubs00/featherweight-kernel/decider/systemone"
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

// The routine runner asks the decider through the same tool of the kernel,
// so a shadow routine records the answers of the server in
// fw.routine.decided, also without decide in the tools of the agent.
func TestDecideServesTheRoutineRunner(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Questions map[string]json.RawMessage `json:"questions"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &in)
		answers := map[string]any{}
		for id := range in.Questions {
			answers[id] = map[string]any{"type": "noul", "noul": 0.8963}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "clef-exl3-4.0bpw", "answers": answers, "usage": map[string]int{"input_tokens": 303}})
	}))
	defer srv.Close()
	cfg := fw.DefaultConfig()
	cfg.Model = "anthropic/claude-opus-5-5"
	cfg.Deciders = map[string]fw.DeciderConfig{"clef": {BaseURL: srv.URL, Model: "clef"}}
	routine := fw.Routine{Name: "outage", Version: 1, Standing: fw.RoutineShadow, Decider: "clef",
		Questions: map[string]fw.RoutineQuestion{"down": {Question: fw.Question{Type: fw.QuestionNoul, Instructions: "Is a service down?"}, Act: 0.8, Expect: fw.Expect{"yes"},
			Observe: &fw.Observe{Answer: `(?i)\bdown\b`}}},
		Answer: "down"}
	model := fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
		return &fw.Response{Content: []fw.Block{fw.Text("The checkout is down.")}, StopReason: fw.StopEndTurn}, nil
	}}
	a, err := fw.New(fw.WithoutDefaults(), fw.WithConfig(cfg), fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()), fw.WithAuthn(fw.NoAuthn{}),
		fw.WithModel(model), fw.WithProfile(fw.AgentProfile{Name: "ops", Routines: []fw.Routine{routine}}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(context.Background()) })
	r, err := a.Run(context.Background(), "Our checkout started returning errors.", fw.AsAgent("ops"))
	if err != nil || r.State != fw.StateCompleted {
		t.Fatalf("run = %+v, %v", r, err)
	}
	var decided fw.RoutineDecidedData
	var compared fw.RoutineComparedData
	for e, err := range a.Ledger().Read(context.Background(), r.ID, 0) {
		if err != nil {
			t.Fatal(err)
		}
		switch e.Type {
		case fw.EvRoutineDecided:
			_ = e.Decode(&decided)
		case fw.EvRoutineCompared:
			_ = e.Decode(&compared)
		}
	}
	if !decided.Applies || decided.Model != "clef-exl3-4.0bpw" || decided.Answers["down"].Probability != 0.8963 {
		t.Fatalf("decided = %+v", decided)
	}
	if compared.Said != "yes" || compared.Observed != "yes" || !compared.Agree || compared.Mode != fw.RoutineShadow {
		t.Fatalf("compared = %+v", compared)
	}
}
