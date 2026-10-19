package native

import (
	"context"
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

// childSpawner runs each child in another agent, as a child Task would.
type childSpawner struct{ other *fw.Agent }

func (s childSpawner) Spawn(ctx context.Context, _ *fw.Run, req fw.RunRequest) (*fw.Run, string, error) {
	r, err := s.other.Start(ctx, req.Input, fw.WithRequest(req))
	return r, "ax:team/child", err
}
func (s childSpawner) Plan(*fw.Run, fw.RunRequest) (string, error) { return "ax:team/child", nil }
func (s childSpawner) Find(ctx context.Context, _, id string) (*fw.Run, error) {
	return s.other.Get(ctx, id)
}
func (s childSpawner) Abandon(context.Context, string) error { return nil }
func (s childSpawner) Get(ctx context.Context, _, id string) (*fw.Run, error) {
	return s.other.Get(ctx, id)
}
func (s childSpawner) WaitFor(ctx context.Context, _, id string, done func(*fw.Run) bool) (*fw.Run, error) {
	return s.other.WaitFor(ctx, id, done)
}
func (s childSpawner) Decide(ctx context.Context, _, id string, d fw.ApprovalDecision) error {
	return s.other.Decide(ctx, id, d)
}
func (s childSpawner) Cancel(ctx context.Context, _, id string) error { return s.other.Cancel(ctx, id) }

// TestTrustedCallerCannotTakeTheIDOfAChild checks that a trusted caller
// cannot start a run under the id of a child of another person's run, so
// the parent and its person keep reading the child. The id of a run that
// exists is a conflict, and an id that is not an id of a run is refused.
func TestTrustedCallerCannotTakeTheIDOfAChild(t *testing.T) {
	callers := tokenMap{
		"alice-token": {Subject: "alice", Kind: fw.KindUser},
		"svc-token":   {Subject: "svc", Kind: fw.KindWorkload},
	}
	other := newAgent(t, newScript(endTurn("child done")).model())
	a := newAgent(t, newScript(endTurn("parent done"), endTurn("impostor")).model(),
		fw.WithAuthn(callers), fw.WithSpawner(childSpawner{other}))
	srv := serve(t, a)
	resp, b := do(t, "POST", srv.URL+"/v1/runs", `{"input":"parent work","wait":true}`, "Authorization", "Bearer alice-token")
	parent := decodeRun(t, resp, b, http.StatusOK)
	child, err := a.SpawnChild(t.Context(), parent.ID, fw.NewRunRequest("do x"))
	if err != nil {
		t.Fatal(err)
	}

	resp, b = do(t, "POST", srv.URL+"/v1/runs", `{"input":"impostor","principal":"alice","id":"`+child.ID+`"}`, "Authorization", "Bearer svc-token")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("a workload named the id of a child: status %d: %s", resp.StatusCode, b)
	}
	got, err := a.WaitFor(t.Context(), child.ID, func(r *fw.Run) bool { return r.State.Terminal() })
	if err != nil || got.Member != "ax:team/child" || got.Output != "child done" {
		t.Fatalf("the child after the attempt = %+v, %v", got, err)
	}
	resp, b = do(t, "POST", srv.URL+"/v1/runs", `{"input":"again","id":"`+parent.ID+`"}`, "Authorization", "Bearer svc-token")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("POST with the id of a run that exists: status %d: %s", resp.StatusCode, b)
	}
	resp, b = do(t, "POST", srv.URL+"/v1/runs", `{"input":"odd","id":"index.jsonl"}`, "Authorization", "Bearer svc-token")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST with an id that is not an id of a run: status %d: %s", resp.StatusCode, b)
	}
}
