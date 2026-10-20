package native

import (
	"net/http"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func TestCreateWithOutcome(t *testing.T) {
	// The run answers first, and its grader second.
	s := newScript(endTurn("done"), endTurn(`{"verdict": "satisfied", "notes": "It is done."}`))
	a := newAgent(t, s.model())
	srv := serve(t, a)
	resp, b := do(t, "POST", srv.URL+"/v1/runs", `{"input":"do it","wait":true,"outcome":{"rubric":"It is done.","iterations":2}}`)
	r := decodeRun(t, resp, b, http.StatusOK)
	if r.State != fw.StateCompleted || r.Output != "done" {
		t.Fatalf("run = %+v", r)
	}
	if r.Outcome == nil || r.Outcome.Verdict != fw.GradeSatisfied || r.Outcome.Notes != "It is done." || r.Outcome.Iterations != 1 || r.Outcome.Grader == "" {
		t.Fatalf("outcome = %+v", r.Outcome)
	}
	if reqs := s.requests(); len(reqs) != 2 || !strings.Contains(fw.TextOf(reqs[1].Messages[0].Content), "It is done.") {
		t.Fatalf("model requests = %d", len(reqs))
	}

	resp, b = do(t, "POST", srv.URL+"/v1/runs", `{"input":"do it","outcome":{"rubric":"x","iterations":99}}`)
	if p := decodeProblem(t, resp, b, http.StatusBadRequest); !strings.Contains(p.Detail, "outcome.iterations") {
		t.Fatalf("problem = %+v", p)
	}
}
