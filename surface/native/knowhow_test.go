package native

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/memory"
	"github.com/bdubs00/featherweight-kernel/memory/fronttest"
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

// TestMemoryRoutesWithFront checks that a note written through the native
// API goes to the front as the change of a person, that a 412 of the front
// answers 412 with the sentence of the contract, and that a front that
// does not answer gets 503 and leaves the copy as it was.
func TestMemoryRoutesWithFront(t *testing.T) {
	f := fronttest.New("front-test-token")
	t.Cleanup(f.Close)
	t.Setenv("FW_TEST_FRONT_TOKEN", "front-test-token")
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Access.Default = "closed"
	cfg.Access.Creators = []string{"alice"}
	cfg.Tools.Memory.Front = &fw.MemoryFrontConfig{URL: f.URL, TokenEnv: "FW_TEST_FRONT_TOKEN"}
	a := newAgent(t, newScript(endTurn("ok")).model(), fw.WithConfig(cfg), fw.WithAuthn(people),
		fw.WithProfile(fw.AgentProfile{Name: "reviewer", Access: &fw.Access{Owners: []string{"alice"}}}))
	srv := serve(t, a)
	put := func(body string, header ...string) (*http.Response, []byte) {
		return do(t, "PUT", srv.URL+"/v1/memory/notes/build.md?agent=reviewer", body, append([]string{"Authorization", asAlice}, header...)...)
	}
	resp, b := put("How to build\n")
	var rec fw.MemoryRecord
	decodeJSON(t, resp, b, http.StatusOK, &rec)
	ch := f.Changes()
	if len(ch) != 1 || ch[0].By != "person:alice" || ch[0].Run != "" || ch[0].IfHash != "absent" {
		t.Fatalf("changes = %+v", ch)
	}

	f.Put(fronttest.Note{Agent: "reviewer", Scope: "agent", Name: "build.md", Text: "Changed elsewhere\n", State: "unchecked", By: "agent"})
	resp, b = put("Mine\n")
	if p := decodeProblem(t, resp, b, http.StatusPreconditionFailed); p.Detail != memory.ConflictText {
		t.Fatalf("detail = %q", p.Detail)
	}
	resp, b = do(t, "GET", srv.URL+"/v1/memory/notes/build.md?agent=reviewer", "", "Authorization", asAlice)
	decodeJSON(t, resp, b, http.StatusOK, &rec)
	if rec.Body != "Changed elsewhere\n" {
		t.Fatalf("the copy after a conflict = %q", rec.Body)
	}

	f.SetDown(true)
	resp, b = put("Lost\n")
	if p := decodeProblem(t, resp, b, http.StatusServiceUnavailable); !strings.Contains(p.Detail, "not kept") || strings.Contains(p.Detail, "front-test-token") {
		t.Fatalf("detail = %q", p.Detail)
	}
	f.SetDown(false)
	resp, b = do(t, "GET", srv.URL+"/v1/memory/notes/build.md?agent=reviewer", "", "Authorization", asAlice)
	decodeJSON(t, resp, b, http.StatusOK, &rec)
	if rec.Body != "Changed elsewhere\n" {
		t.Fatalf("the copy after no answer = %q", rec.Body)
	}
}
