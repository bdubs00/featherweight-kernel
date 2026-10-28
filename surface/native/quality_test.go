package native

import (
	"net/http"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// TestQualityRoute checks POST /v1/runs/{id}/quality: an operator of the
// run records the receipt of a system of record as fw.run.annotated, on
// the latest completed end when the body names none. A retry writes
// nothing, a viewer is refused, and a second verification of the same end
// is refused.
func TestQualityRoute(t *testing.T) {
	callers := tokenMap{
		"alice-token": {Subject: "alice", Kind: fw.KindUser},
		"bob-token":   {Subject: "bob", Kind: fw.KindUser},
	}
	a := newAgent(t, newScript(endTurn("deployed")).model(), fw.WithAuthn(callers),
		fw.WithProfile(fw.AgentProfile{Name: "team", Access: &fw.Access{Owners: []string{"alice"}, Viewers: []string{"bob"}}}))
	srv := serve(t, a)
	alice, bob := []string{"Authorization", "Bearer alice-token"}, []string{"Authorization", "Bearer bob-token"}
	resp, b := do(t, "POST", srv.URL+"/v1/runs", `{"agent":"team","input":"Deploy it.","wait":true}`, alice...)
	r := decodeRun(t, resp, b, http.StatusOK)
	url := srv.URL + "/v1/runs/" + r.ID + "/quality"
	receipt := `{"id":"deploy-881","verification":"satisfied"}`

	resp, b = do(t, "POST", url, receipt, bob...)
	decodeProblem(t, resp, b, http.StatusForbidden)
	resp, b = do(t, "POST", url, receipt, alice...)
	if got := decodeRun(t, resp, b, http.StatusOK); got.State != fw.StateCompleted {
		t.Fatalf("run = %s", b)
	}
	resp, b = do(t, "POST", url, receipt, alice...) // a retry
	decodeRun(t, resp, b, http.StatusOK)
	var receipts []fw.QualityAnnotatedData
	var fin uint64
	for _, e := range collect(t, a, r.ID) {
		var d fw.QualityAnnotatedData
		switch {
		case e.Type == fw.EvRunFinished:
			fin = e.Seq
		case e.Type == fw.EvRunAnnotated && e.Decode(&d) == nil && d.Quality != nil:
			receipts = append(receipts, d)
		}
	}
	if len(receipts) != 1 || receipts[0].Quality.ID != "deploy-881" || receipts[0].Quality.Verification != "satisfied" ||
		receipts[0].Quality.Completion != fin {
		t.Fatalf("receipts = %+v, end %d", receipts, fin)
	}
	for body, want := range map[string]string{
		`{"id":"deploy-882","verification":"rejected"}`:  "already verified",
		`{"id":"x","verification":"great"}`:              "unknown quality verification",
		`{"verification":"satisfied"}`:                   "receipt id",
		`{"id":"x","verification":"satisfied","by":"y"}`: "by",
	} {
		resp, b = do(t, "POST", url, body, alice...)
		if p := decodeProblem(t, resp, b, http.StatusBadRequest); !strings.Contains(p.Detail, want) {
			t.Errorf("%s: detail %q lacks %q", body, p.Detail, want)
		}
	}
	resp, b = do(t, "POST", srv.URL+"/v1/runs/nope/quality", receipt, alice...)
	decodeProblem(t, resp, b, http.StatusNotFound)
	// A run that never completed has no end to verify. The model has no
	// answer left, so the next run fails.
	resp, b = do(t, "POST", srv.URL+"/v1/runs", `{"agent":"team","input":"Deploy again.","wait":true}`, alice...)
	failed := decodeRun(t, resp, b, http.StatusOK)
	if failed.State != fw.StateFailed || failed.Stop != fw.RunStopModelError {
		t.Fatalf("run = %+v", failed)
	}
	resp, b = do(t, "POST", srv.URL+"/v1/runs/"+failed.ID+"/quality", receipt, alice...)
	if p := decodeProblem(t, resp, b, http.StatusBadRequest); !strings.Contains(p.Detail, "never completed") {
		t.Fatalf("detail = %q", p.Detail)
	}
}
