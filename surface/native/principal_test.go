package native

import (
	"net/http"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// TestTrustedCallerStartsARunForAPerson checks who a run belongs to when
// the body names a principal: a trusted process keeps it, a person does
// not.
func TestTrustedCallerStartsARunForAPerson(t *testing.T) {
	callers := tokenMap{
		"alice-token":  {Subject: "alice", Kind: fw.KindUser},
		"member-token": {Subject: fw.KindToken, Kind: fw.KindToken},
	}
	a := newAgent(t, newScript(endTurn("done"), endTurn("done"), endTurn("done")).model(), fw.WithAuthn(callers))
	srv := serve(t, a)

	resp, b := do(t, "POST", srv.URL+"/v1/runs", `{"input":"for ada","principal":"ada","principalKind":"user","wait":true}`, "Authorization", "Bearer member-token")
	r := decodeRun(t, resp, b, http.StatusOK)
	if r.Principal != "ada" {
		t.Fatalf("a trusted caller started the run as %q, want ada", r.Principal)
	}
	var q fw.RunQueuedData
	for e, err := range a.Ledger().Read(t.Context(), r.ID, 0) {
		if err == nil && e.Type == fw.EvRunQueued {
			_ = e.Decode(&q)
		}
	}
	if q.Principal != "ada" || q.PrincipalKind != fw.KindUser {
		t.Fatalf("queued = %+v", q)
	}

	resp, b = do(t, "POST", srv.URL+"/v1/runs", `{"input":"for ada","principal":"ada","principalKind":"user","wait":true}`, "Authorization", "Bearer alice-token")
	r = decodeRun(t, resp, b, http.StatusOK)
	if r.Principal != "alice" {
		t.Fatalf("a person started the run as %q, want alice", r.Principal)
	}

	resp, b = do(t, "POST", srv.URL+"/v1/runs", `{"input":"own","wait":true}`, "Authorization", "Bearer member-token")
	r = decodeRun(t, resp, b, http.StatusOK)
	if r.Principal != fw.KindToken {
		t.Fatalf("a trusted caller without a principal started the run as %q", r.Principal)
	}
}

// TestTrustedCallerNamesTheIDOfARun checks the id in the body: a trusted
// process names the id of the run, as a parent in another process does
// for its child, and the same start sent again under its idempotency key
// is the same run. A person cannot name the id.
func TestTrustedCallerNamesTheIDOfARun(t *testing.T) {
	callers := tokenMap{
		"alice-token":  {Subject: "alice", Kind: fw.KindUser},
		"member-token": {Subject: fw.KindToken, Kind: fw.KindToken},
	}
	a := newAgent(t, newScript(endTurn("done")).model(), fw.WithAuthn(callers))
	srv := serve(t, a)
	id := fw.NewID()
	body := `{"input":"for ada","principal":"ada","id":"` + id + `"}`

	resp, b := do(t, "POST", srv.URL+"/v1/runs", body, "Authorization", "Bearer member-token", HeaderIdempotencyKey, "spawn:p:c")
	if r := decodeRun(t, resp, b, http.StatusCreated); r.ID != id {
		t.Fatalf("a trusted caller started the run %q, want %q", r.ID, id)
	}
	resp, b = do(t, "POST", srv.URL+"/v1/runs", body, "Authorization", "Bearer member-token", HeaderIdempotencyKey, "spawn:p:c")
	if r := decodeRun(t, resp, b, http.StatusCreated); r.ID != id {
		t.Fatalf("the start sent again gave the run %q, want %q", r.ID, id)
	}
	if list, err := a.List(t.Context()); err != nil || len(list) != 1 {
		t.Fatalf("the agent holds %d runs, %v", len(list), err)
	}

	resp, _ = do(t, "POST", srv.URL+"/v1/runs", `{"input":"mine","id":"`+fw.NewID()+`"}`, "Authorization", "Bearer alice-token")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a person named the id of a run: status %d", resp.StatusCode)
	}
}
