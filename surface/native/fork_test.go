package native

import (
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
