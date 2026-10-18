package native

import (
	"net/http"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/surface/view"
)

// GET /v1/status counts the runs of the agent. It answers with zero counts
// and a null approval before the first run.
func TestStatusRoute(t *testing.T) {
	a := newAgent(t, answers("done"))
	srv := serve(t, a)
	resp, b := do(t, "GET", srv.URL+"/v1/status", "")
	var st view.Status
	decodeJSON(t, resp, b, http.StatusOK, &st)
	// Before any run the counts are zero, and the process is idle since
	// its start.
	if st.Runs != (view.RunCounts{}) || st.Usage != (view.StatusUsage{}) || st.PendingApproval != nil || !st.Idle || st.IdleSince == nil || st.IdleSince.After(time.Now()) {
		t.Fatalf("status before any run = %s", b)
	}
	if m := keys(t, b); string(m["pendingApproval"]) != "null" {
		t.Errorf("pendingApproval = %s", m["pendingApproval"])
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}

	resp, b = do(t, "POST", srv.URL+"/v1/runs", `{"input":"do the thing","wait":true}`)
	decodeRun(t, resp, b, http.StatusOK)
	resp, b = do(t, "GET", srv.URL+"/v1/status", "")
	decodeJSON(t, resp, b, http.StatusOK, &st)
	if st.Runs.Total != 1 || st.Runs.Active != 0 || st.PendingApproval != nil || !st.Idle || st.IdleSince == nil {
		t.Errorf("status after one run = %s", b)
	}
}
