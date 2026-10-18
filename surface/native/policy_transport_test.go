package native

// Tests for the transport of the recursive All clauses of profile policy
// through POST /v1/runs: invalid clauses fail as invalid requests, valid
// clauses land in the run queue unchanged, and the merged policy of a run
// denies in fact, not only in shape.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// TestAllClauseTransportRejectsInvalidDefaults posts inline profiles whose
// nested clauses hold an invalid default, at three nesting depths, and
// expects an invalid-request problem that names the field.
func TestAllClauseTransportRejectsInvalidDefaults(t *testing.T) {
	a := newAgent(t, newScript(endTurn("hello")).model())
	srv := serve(t, a)
	for _, def := range []string{`{"default":"sometimes"}`, `{"all":[{"default":"maybe"}]}`, `{"all":[{"all":[{"default":"perhaps"}]}]}`} {
		body := `{"input":"go","wait":true,"profile":{"policy":` + def + `}}`
		resp, b := do(t, "POST", srv.URL+"/v1/runs", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("policy %s: status = %d, body %s", def, resp.StatusCode, b)
		}
		if !strings.Contains(string(b), "profile.policy.") {
			t.Fatalf("policy %s: problem does not name the field: %s", def, b)
		}
	}
}

// TestAllClauseTransportRoundTrip posts a profile whose policy carries All
// clauses, finishes the run, and checks three things: the run queue keeps
// every clause unchanged, the tool call that only a nested clause denies is
// denied in fact, and the call that both the own rules and every clause
// allow runs.
func TestAllClauseTransportRoundTrip(t *testing.T) {
	var calls atomic.Int32
	tool := fw.NewTool(fw.ToolSpec{Name: "clock", ReadOnly: true}, func(context.Context, json.RawMessage) (fw.ToolResult, error) {
		calls.Add(1)
		return fw.TextResult("clock"), nil
	})
	bash := fw.NewTool(fw.ToolSpec{Name: "bash", Destructive: true}, func(context.Context, json.RawMessage) (fw.ToolResult, error) {
		calls.Add(1)
		return fw.TextResult("ran"), nil
	})
	// The own rules allow clock by default and ask for bash. A clause
	// denies bash. The merged verdict for bash is deny in fact.
	s := newScript(
		callTool("c1", "clock", `{}`),
		callTool("c2", "bash", `{}`),
		endTurn("done"),
	)
	a := newAgent(t, s.model(), fw.WithTools(tool, bash))
	srv := serve(t, a)
	body := `{"input":"go","wait":true,"profile":{"policy":{` +
		`"allow":["clock"],"ask":["bash"],` +
		`"all":[{"default":"deny","allow":["clock"]},{"ask":["http"]}]}}}`
	resp, b := do(t, "POST", srv.URL+"/v1/runs", body)
	r := decodeRun(t, resp, b, http.StatusOK)
	if r.State != fw.StateCompleted {
		t.Fatalf("run = %+v", r)
	}
	// The queued profile holds the clauses unchanged.
	var q fw.RunQueuedData
	if err := collect(t, a, r.ID)[0].Decode(&q); err != nil {
		t.Fatal(err)
	}
	if q.Profile == nil || q.Profile.Policy == nil || len(q.Profile.Policy.All) != 2 {
		t.Fatalf("queued policy = %+v", q.Profile)
	}
	got := *q.Profile.Policy
	if len(got.Allow) != 1 || got.Allow[0] != "clock" || len(got.Ask) != 1 || got.Ask[0] != "bash" {
		t.Fatalf("queued own rules = %+v", got)
	}
	if got.All[0].Default != "deny" || len(got.All[0].Allow) != 1 || got.All[0].Allow[0] != "clock" ||
		len(got.All[1].Ask) != 1 || got.All[1].Ask[0] != "http" {
		t.Fatalf("queued clauses = %+v and %+v", got.All[0], got.All[1])
	}
	// The effective decisions, as the model saw them. The script model
	// answers one tool call per response, so the second request carries the
	// result of clock and the third the result of bash.
	reqs := s.requests()
	clockRes, bashRes := lastResults(t, reqs[1])[0], lastResults(t, reqs[2])[0]
	if fw.TextOf(clockRes.Content) != "clock" || clockRes.IsError {
		t.Fatalf("clock result = %+v", clockRes)
	}
	if !bashRes.IsError || !strings.Contains(fw.TextOf(bashRes.Content), "denied by policy") {
		t.Fatalf("bash result = %+v", bashRes)
	}
	// The own rules ask for bash, so only the nested deny clause can make
	// the verdict deny; the default of that clause decided, hence the
	// reason of the denial is the reason of a default.
	if !strings.Contains(fw.TextOf(bashRes.Content), "default") {
		t.Fatalf("bash result does not name the clause default: %+v", bashRes)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("tool bodies ran %d times, want clock once and bash never", n)
	}
}

func lastResults(t *testing.T, req *fw.Request) []fw.Block {
	t.Helper()
	m := req.Messages[len(req.Messages)-1]
	var out []fw.Block
	for _, b := range m.Content {
		if b.Type == fw.BlockToolResult {
			out = append(out, b)
		}
	}
	return out
}
