package native

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// TestBudgetRoutes continues a run that failed at its limit: the change of
// PATCH /v1/runs/{id}/budget and the budget of a follow-up. Only an
// operator of the run changes its budget, and the route checks the body.
func TestBudgetRoutes(t *testing.T) {
	ctx := testCtx(t)
	callers := tokenMap{
		"alice-token": {Subject: "alice", Kind: fw.KindUser},
		"bob-token":   {Subject: "bob", Kind: fw.KindUser},
	}
	echo := fw.NewTool(fw.ToolSpec{Name: "echo", ReadOnly: true}, func(context.Context, json.RawMessage) (fw.ToolResult, error) {
		return fw.TextResult("ok"), nil
	})
	a := newAgent(t, newScript(callTool("c1", "echo", `{}`), endTurn("the report"), endTurn("done")).model(),
		fw.WithAuthn(callers), fw.WithTools(echo),
		fw.WithProfile(fw.AgentProfile{Name: "team", Access: &fw.Access{Owners: []string{"alice"}, Viewers: []string{"bob"}}}))
	srv := serve(t, a)
	alice, bob := []string{"Authorization", "Bearer alice-token"}, []string{"Authorization", "Bearer bob-token"}

	resp, b := do(t, "POST", srv.URL+"/v1/runs", `{"agent":"team","input":"work","budget":{"maxTurns":1},"wait":true}`, alice...)
	r := decodeRun(t, resp, b, http.StatusOK)
	if r.State != fw.StateFailed || r.Output != "the report" || r.Error != "budget exceeded: maxTurns" {
		t.Fatalf("run = %+v", r)
	}
	url := srv.URL + "/v1/runs/" + r.ID + "/budget"

	resp, b = do(t, "PATCH", url, `{"maxTurns":3}`, bob...)
	decodeProblem(t, resp, b, http.StatusForbidden)
	for body, want := range map[string]string{
		`{}`:                         "at least one field",
		`{"maxTurns":-1}`:            "negative",
		`{"deadline":"later"}`:       "deadline",
		`{"reportTimeout":"soon"}`:   "reportTimeout",
		`{"maxTurns":3,"color":"x"}`: "color",
	} {
		resp, b = do(t, "PATCH", url, body, alice...)
		if p := decodeProblem(t, resp, b, http.StatusBadRequest); !strings.Contains(p.Detail, want) {
			t.Errorf("%s: detail %q lacks %q", body, p.Detail, want)
		}
	}
	resp, b = do(t, "PATCH", srv.URL+"/v1/runs/nope/budget", `{"maxTurns":3}`, alice...)
	decodeProblem(t, resp, b, http.StatusNotFound)

	resp, b = do(t, "PATCH", url, `{"maxTurns":2,"deadline":"1h","reportMaxTokens":500}`, alice...)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, b)
	}
	var got struct {
		Budget fw.Budget `json:"budget"`
		Run    fw.Run    `json:"run"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Budget.MaxTurns != 2 || got.Budget.Deadline != time.Hour || got.Budget.ReportMaxTokens != 500 || got.Run.State != fw.StateFailed {
		t.Fatalf("answer = %s", b)
	}

	resp, b = do(t, "POST", srv.URL+"/v1/runs/"+r.ID+"/messages", `{"content":"go on","budget":{"maxTurns":3,"finalReport":false}}`, alice...)
	decodeRun(t, resp, b, http.StatusAccepted)
	r, err := a.Wait(ctx, r.ID)
	if err != nil || r.State != fw.StateCompleted || r.Output != "done" {
		t.Fatalf("run = %+v, %v", r, err)
	}
	budget, err := a.RunBudget(ctx, r.ID)
	if err != nil || budget.MaxTurns != 3 || budget.Deadline != time.Hour || budget.FinalReport == nil || *budget.FinalReport {
		t.Fatalf("budget = %+v, %v", budget, err)
	}
	var changes []fw.BudgetChangedData
	for _, e := range collect(t, a, r.ID) {
		if e.Type == fw.EvBudgetChanged {
			var d fw.BudgetChangedData
			_ = e.Decode(&d)
			changes = append(changes, d)
		}
	}
	if len(changes) != 2 || changes[0].By != "alice" || changes[0].Origin != fw.OriginHTTP || changes[1].Budget.MaxTurns != 3 {
		t.Fatalf("changes = %+v", changes)
	}
	resp, b = do(t, "POST", srv.URL+"/v1/runs/"+r.ID+"/messages", `{"content":"x","budget":{"maxTurns":-2}}`, alice...)
	decodeProblem(t, resp, b, http.StatusBadRequest)
}
