package native

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// POST /v1/runs/{id}/fork answers with a branch of the run, which goes on
// with the message of the body; a sequence number beyond the ledger is a
// bad request.
func TestForkRoute(t *testing.T) {
	a := newAgent(t, answers("first", "second"))
	srv := serve(t, a)
	resp, b := do(t, "POST", srv.URL+"/v1/runs", `{"input":"hello","wait":true}`)
	run := decodeRun(t, resp, b, http.StatusOK)
	resp, b = do(t, "POST", srv.URL+"/v1/runs/"+run.ID+"/fork", `{"input":"and then?","metadata":{"why":"try"}}`)
	branch := decodeRun(t, resp, b, http.StatusOK)
	if branch.ID == run.ID || branch.Metadata[fw.MetaForkOf] != run.ID || branch.Metadata["why"] != "try" {
		t.Fatalf("branch = %+v", branch)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !branch.State.Terminal() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		resp, b = do(t, "GET", srv.URL+"/v1/runs/"+branch.ID, "")
		branch = decodeRun(t, resp, b, http.StatusOK)
	}
	if branch.State != fw.StateCompleted || branch.Output != "second" {
		t.Fatalf("branch after the message = %+v", branch)
	}
	resp, b = do(t, "GET", srv.URL+"/v1/runs/"+run.ID, "")
	if again := decodeRun(t, resp, b, http.StatusOK); again.Output != "first" {
		t.Fatalf("the source changed: %+v", again)
	}
	resp, b = do(t, "POST", srv.URL+"/v1/runs/"+run.ID+"/fork", `{"seq":999}`)
	decodeProblem(t, resp, b, http.StatusBadRequest)
}

// POST /v1/runs/fork takes the kept events of a branch from a trusted
// caller, and refuses a person.
func TestForkEventsRoute(t *testing.T) {
	a := newAgent(t, answers("first", "second"), fw.WithAuthn(fw.TokenAuthn{Token: "s3cret"}))
	srv := serve(t, a)
	auth := []string{"Authorization", "Bearer s3cret"}
	resp, b := do(t, "POST", srv.URL+"/v1/runs", `{"input":"hello","wait":true}`, auth...)
	run := decodeRun(t, resp, b, http.StatusOK)
	resp, b = do(t, "GET", srv.URL+"/v1/runs/"+run.ID+"/events", "", "Authorization", "Bearer s3cret", "Accept", "application/json")
	var page struct {
		Events []fw.Event `json:"events"`
	}
	decodeJSON(t, resp, b, http.StatusOK, &page)
	if len(page.Events) == 0 {
		t.Fatal("no events")
	}
	body, _ := json.Marshal(map[string]any{"events": page.Events, "input": "and then?"})
	resp, b = do(t, "POST", srv.URL+"/v1/runs/fork", string(body), auth...)
	branch := decodeRun(t, resp, b, http.StatusCreated)
	if branch.ID == run.ID || branch.Metadata[fw.MetaForkOf] != "" {
		// The events of this process carry no fork metadata; a Forker sets it.
	}
	deadline := time.Now().Add(5 * time.Second)
	for !branch.State.Terminal() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		resp, b = do(t, "GET", srv.URL+"/v1/runs/"+branch.ID, "", auth...)
		branch = decodeRun(t, resp, b, http.StatusOK)
	}
	if branch.State != fw.StateCompleted || branch.Output != "second" {
		t.Fatalf("branch from events = %+v", branch)
	}
}
