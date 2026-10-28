package native

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// TestCreateRunContext checks that POST /v1/runs takes the data blocks of
// the caller into the run, and that a request past a bound gets a 400 that
// names the bound.
func TestCreateRunContext(t *testing.T) {
	s := newScript(endTurn("done"))
	a := newAgent(t, s.model())
	srv := serve(t, a)
	resp, b := do(t, "POST", srv.URL+"/v1/runs", `{"input":"go","wait":true,"context":[{"source":"found","text":"ISSUE 42"}]}`)
	r := decodeRun(t, resp, b, http.StatusOK)
	var d fw.RunStartedData
	for _, e := range collect(t, a, r.ID) {
		if e.Type == fw.EvRunStarted {
			_ = e.Decode(&d)
		}
	}
	if d.Context != fw.ContextBlock("found", "ISSUE 42") {
		t.Fatalf("started context = %q", d.Context)
	}
	if first := fw.TextOf(s.requests()[0].Messages[0].Content[:1]); first != d.Context {
		t.Fatalf("first block = %q", first)
	}

	blocks := make([]fw.RunContext, fw.MaxRunContexts+1)
	for i := range blocks {
		blocks[i] = fw.RunContext{Source: "page", Text: "x"}
	}
	many, _ := json.Marshal(map[string]any{"input": "go", "context": blocks})
	long, _ := json.Marshal(map[string]any{"input": "go", "context": []fw.RunContext{{Source: "page", Text: strings.Repeat("x", fw.MaxRunContextBytes+1)}}})
	for _, tc := range []struct {
		body, want string
	}{
		{string(many), "at most 8"},
		{string(long), "at most 16384 bytes"},
		{`{"input":"go","context":[{"source":"Bad Name","text":"x"}]}`, "[a-z][a-z0-9-]{0,31}"},
	} {
		resp, b := do(t, "POST", srv.URL+"/v1/runs", tc.body)
		if p := decodeProblem(t, resp, b, http.StatusBadRequest); !strings.Contains(p.Detail, tc.want) {
			t.Fatalf("detail = %q, want %q", p.Detail, tc.want)
		}
	}
}
