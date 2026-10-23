package native

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// stored returns the events that the ledger holds for a run now, without
// a wait for the end of the run, as collect waits.
func stored(t *testing.T, a *fw.Agent, id string) []fw.Event {
	t.Helper()
	var out []fw.Event
	for e, err := range a.Ledger().Read(t.Context(), id, 0) {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

// lastOf decodes the data of the last event of a type in a run, and
// reports whether the run has one.
func lastOf(t *testing.T, a *fw.Agent, id, typ string, v any) bool {
	t.Helper()
	found := false
	for _, e := range stored(t, a, id) {
		if e.Type == typ {
			if err := e.Decode(v); err != nil {
				t.Fatal(err)
			}
			found = true
		}
	}
	return found
}

// TestOnBehalfOfNamesThePerson relays the actions of a person as the front
// of a fleet does: the static token with the header Fw-On-Behalf-Of. The
// ledger names the person as the decider of the approval, as the sender of
// a follow-up, as the person of a change of the budget, of a reaction, and
// of a cancel, and as the person of a branch. Without the header, the
// records name the token, as before.
func TestOnBehalfOfNamesThePerson(t *testing.T) {
	ctx := testCtx(t)
	callers := tokenMap{"member-token": {Subject: fw.KindToken, Kind: fw.KindToken}}
	var calls atomic.Int32
	a := newAgent(t, newScript(callTool("c1", "rm", `{"path":"x"}`), endTurn("done"), endTurn("again"), endTurn("as the token"),
		callTool("c2", "rm", `{"path":"y"}`), callTool("c3", "rm", `{"path":"z"}`)).model(), fw.WithAuthn(callers), fw.WithTools(rmTool(&calls)))
	srv := serve(t, a)
	member := []string{"Authorization", "Bearer member-token"}
	forAlice := []string{"Authorization", "Bearer member-token", HeaderOnBehalfOf, "alice"}
	start := `{"input":"clean up","principal":"alice","principalKind":"user","wait":true}`

	resp, b := do(t, "POST", srv.URL+"/v1/runs", start, member...)
	r := decodeRun(t, resp, b, http.StatusOK)
	if r.State != fw.StateInputRequired || r.Principal != "alice" {
		t.Fatalf("run = %s", b)
	}
	resp, b = do(t, "POST", srv.URL+"/v1/runs/"+r.ID+"/approvals/"+r.Pending.ID, `{"decision":"allow"}`, forAlice...)
	decodeRun(t, resp, b, http.StatusOK)
	if r, err := a.Wait(ctx, r.ID); err != nil || r.State != fw.StateCompleted || calls.Load() != 1 {
		t.Fatalf("run = %+v, %v, %d calls", r, err, calls.Load())
	}
	resp, b = do(t, "POST", srv.URL+"/v1/runs/"+r.ID+"/messages", `{"content":"again","budget":{"maxTurns":9}}`, forAlice...)
	decodeRun(t, resp, b, http.StatusAccepted)
	if _, err := a.Wait(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	resp, b = do(t, "PATCH", srv.URL+"/v1/runs/"+r.ID+"/budget", `{"maxTurns":10}`, forAlice...)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, b)
	}
	resp, b = do(t, "POST", srv.URL+"/v1/runs/"+r.ID+"/reactions", `{"reaction":"heart"}`, forAlice...)
	decodeRun(t, resp, b, http.StatusOK)
	resp, b = do(t, "POST", srv.URL+"/v1/runs/"+r.ID+"/fork", `{}`, forAlice...)
	branch := decodeRun(t, resp, b, http.StatusOK)
	resp, b = do(t, "POST", srv.URL+"/v1/runs/"+r.ID+"/messages", `{"content":"as the token"}`, member...)
	decodeRun(t, resp, b, http.StatusAccepted)
	if _, err := a.Wait(ctx, r.ID); err != nil {
		t.Fatal(err)
	}

	var decided fw.ApprovalDecidedData
	if !lastOf(t, a, r.ID, fw.EvApprovalDecided, &decided) || decided.Decider != "alice" || decided.Decision != "allow" {
		t.Fatalf("decided = %+v", decided)
	}
	if msgs := messagesOf(t, a, r.ID); len(msgs) != 2 || msgs[0].By != "alice" || msgs[1].By != fw.KindToken {
		t.Fatalf("messages = %+v", msgs)
	}
	var budgets []fw.BudgetChangedData
	var reaction fw.ReactionData
	for _, e := range stored(t, a, r.ID) {
		switch e.Type {
		case fw.EvBudgetChanged:
			var d fw.BudgetChangedData
			_ = e.Decode(&d)
			budgets = append(budgets, d)
		case fw.EvRunReacted:
			_ = e.Decode(&reaction)
		}
	}
	if len(budgets) != 2 || budgets[0].By != "alice" || budgets[1].By != "alice" || budgets[1].Budget.MaxTurns != 10 {
		t.Fatalf("budget changes = %+v", budgets)
	}
	if reaction.By != "alice" || reaction.Reaction != fw.ReactionHeart {
		t.Fatalf("reaction = %+v", reaction)
	}
	var q fw.RunQueuedData
	if !lastOf(t, a, branch.ID, fw.EvRunQueued, &q) || q.Principal != "alice" || q.PrincipalKind != fw.KindUser || branch.Principal != "alice" {
		t.Fatalf("branch = %+v, queued principal %q of kind %q", branch, q.Principal, q.PrincipalKind)
	}

	// The front cancels a run for the person, and the token cancels
	// another one for itself.
	for _, tc := range []struct {
		header []string
		want   string
	}{{forAlice, "alice"}, {member, fw.KindToken}} {
		resp, b = do(t, "POST", srv.URL+"/v1/runs", strings.Replace(start, "clean up", "clean up again", 1), member...)
		r2 := decodeRun(t, resp, b, http.StatusOK)
		if r2.State != fw.StateInputRequired {
			t.Fatalf("run = %s", b)
		}
		resp, b = do(t, "POST", srv.URL+"/v1/runs/"+r2.ID+"/cancel", "", tc.header...)
		if got := decodeRun(t, resp, b, http.StatusOK); got.State != fw.StateCanceled {
			t.Fatalf("run = %s", b)
		}
		var fin fw.RunFinishedData
		if !lastOf(t, a, r2.ID, fw.EvRunFinished, &fin) || fin.Outcome != fw.OutcomeCanceled || fin.By != tc.want {
			t.Fatalf("finished = %+v, want by %q", fin, tc.want)
		}
	}
}

// TestOnBehalfOfRefusals checks who can send the header: a person who
// names another person gets a 403 on every route, and nothing reaches the
// ledger. A person who names itself changes nothing. A value that is too
// long, that holds a control character, or that is not UTF-8 is a 400, and
// so is a second header.
func TestOnBehalfOfRefusals(t *testing.T) {
	callers := tokenMap{
		"alice-token":  {Subject: "alice", Kind: fw.KindUser},
		"member-token": {Subject: fw.KindToken, Kind: fw.KindToken},
	}
	var calls atomic.Int32
	a := newAgent(t, newScript(callTool("c1", "rm", `{"path":"x"}`), endTurn("done")).model(), fw.WithAuthn(callers), fw.WithTools(rmTool(&calls)))
	srv := serve(t, a)
	resp, b := do(t, "POST", srv.URL+"/v1/runs", `{"input":"clean up","wait":true}`, "Authorization", "Bearer alice-token")
	r := decodeRun(t, resp, b, http.StatusOK)
	if r.State != fw.StateInputRequired {
		t.Fatalf("run = %s", b)
	}
	before := len(stored(t, a, r.ID))
	run := srv.URL + "/v1/runs/" + r.ID
	for _, rt := range []struct{ method, url, body string }{
		{"POST", run + "/approvals/" + r.Pending.ID, `{"decision":"allow"}`},
		{"POST", run + "/messages", `{"content":"hi"}`},
		{"PATCH", run + "/budget", `{"maxTurns":3}`},
		{"POST", run + "/reactions", `{"reaction":"up"}`},
		{"POST", run + "/fork", `{}`},
		{"POST", run + "/cancel", ""},
		{"GET", run, ""},
		{"POST", srv.URL + "/v1/runs", `{"input":"as bob"}`},
	} {
		resp, b = do(t, rt.method, rt.url, rt.body, "Authorization", "Bearer alice-token", HeaderOnBehalfOf, "bob")
		if p := decodeProblem(t, resp, b, http.StatusForbidden); p.Detail != "only the static token or a workload can act for another person" {
			t.Errorf("%s %s: detail = %q", rt.method, rt.url, p.Detail)
		}
	}
	if n := len(stored(t, a, r.ID)); n != before {
		t.Fatalf("the refused requests appended %d events", n-before)
	}
	if list, err := a.List(t.Context()); err != nil || len(list) != 1 {
		t.Fatalf("the agent holds %d runs, %v", len(list), err)
	}

	for _, v := range []string{strings.Repeat("x", fw.MaxActingForBytes+1), "ada\tlovelace", "ada\xff"} {
		resp, b = do(t, "POST", run+"/messages", `{"content":"hi"}`, "Authorization", "Bearer member-token", HeaderOnBehalfOf, v)
		if p := decodeProblem(t, resp, b, http.StatusBadRequest); !strings.Contains(p.Detail, HeaderOnBehalfOf) {
			t.Errorf("%q: detail = %q", v, p.Detail)
		}
	}
	req, err := http.NewRequest("POST", run+"/messages", strings.NewReader(`{"content":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer member-token")
	req.Header.Add(HeaderOnBehalfOf, "ada")
	req.Header.Add(HeaderOnBehalfOf, "bob")
	twice, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, err = io.ReadAll(twice.Body)
	twice.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if p := decodeProblem(t, twice, b, http.StatusBadRequest); !strings.Contains(p.Detail, "more than once") {
		t.Fatalf("two headers: detail = %q", p.Detail)
	}
	if n := len(stored(t, a, r.ID)); n != before {
		t.Fatalf("the bad headers appended %d events", n-before)
	}

	// Alice names herself: the decision is hers, as without the header.
	resp, b = do(t, "POST", run+"/approvals/"+r.Pending.ID, `{"decision":"allow"}`, "Authorization", "Bearer alice-token", HeaderOnBehalfOf, "alice")
	decodeRun(t, resp, b, http.StatusOK)
	var decided fw.ApprovalDecidedData
	if !lastOf(t, a, r.ID, fw.EvApprovalDecided, &decided) || decided.Decider != "alice" {
		t.Fatalf("decided = %+v", decided)
	}
}

// TestOnBehalfOfLeavesTheAuthorization checks that the header changes no
// authorization result. The kernel decides for the caller, so the static
// token decides for a person who has no role on the agent, and a person
// who lacks the right is refused with the header as without it. The person
// that the header names never widens the rights of the caller.
func TestOnBehalfOfLeavesTheAuthorization(t *testing.T) {
	ctx := testCtx(t)
	callers := tokenMap{
		"alice-token":  {Subject: "alice", Kind: fw.KindUser},
		"bob-token":    {Subject: "bob", Kind: fw.KindUser},
		"member-token": {Subject: fw.KindToken, Kind: fw.KindToken},
	}
	var calls atomic.Int32
	a := newAgent(t, newScript(callTool("c1", "rm", `{"path":"x"}`), endTurn("done"), callTool("c2", "rm", `{"path":"y"}`), endTurn("done")).model(),
		fw.WithAuthn(callers), fw.WithTools(rmTool(&calls)),
		fw.WithProfile(fw.AgentProfile{Name: "team", Access: &fw.Access{Owners: []string{"alice"}, Operators: []string{"kind:token"}}}),
		fw.WithProfile(fw.AgentProfile{Name: "private", Access: &fw.Access{Owners: []string{"alice"}}}))
	srv := serve(t, a)
	alice := []string{"Authorization", "Bearer alice-token"}

	// The token is an operator of team, and bob has no role there.
	resp, b := do(t, "POST", srv.URL+"/v1/runs", `{"agent":"team","input":"clean up","wait":true}`, alice...)
	team := decodeRun(t, resp, b, http.StatusOK)
	resp, b = do(t, "POST", srv.URL+"/v1/runs/"+team.ID+"/approvals/"+team.Pending.ID, `{"decision":"allow"}`,
		"Authorization", "Bearer member-token", HeaderOnBehalfOf, "bob")
	decodeRun(t, resp, b, http.StatusOK)
	var decided fw.ApprovalDecidedData
	if !lastOf(t, a, team.ID, fw.EvApprovalDecided, &decided) || decided.Decider != "bob" {
		t.Fatalf("decided = %+v", decided)
	}
	if r, err := a.Wait(ctx, team.ID); err != nil || r.State != fw.StateCompleted {
		t.Fatalf("run = %+v, %v", r, err)
	}

	// The token has no role on private. Naming its owner does not help, and
	// bob is refused with his own subject in the header as without it.
	resp, b = do(t, "POST", srv.URL+"/v1/runs", `{"agent":"private","input":"clean up","wait":true}`, alice...)
	private := decodeRun(t, resp, b, http.StatusOK)
	decision := srv.URL + "/v1/runs/" + private.ID + "/approvals/" + private.Pending.ID
	for _, header := range [][]string{
		{"Authorization", "Bearer member-token", HeaderOnBehalfOf, "alice"},
		{"Authorization", "Bearer member-token"},
		{"Authorization", "Bearer bob-token", HeaderOnBehalfOf, "bob"},
		{"Authorization", "Bearer bob-token"},
	} {
		resp, b = do(t, "POST", decision, `{"decision":"allow"}`, header...)
		decodeProblem(t, resp, b, http.StatusForbidden)
		resp, b = do(t, "POST", srv.URL+"/v1/runs/"+private.ID+"/cancel", "", header...)
		decodeProblem(t, resp, b, http.StatusForbidden)
	}
	resp, b = do(t, "POST", decision, `{"decision":"allow"}`, alice...)
	decodeRun(t, resp, b, http.StatusOK)
	if r, err := a.Wait(ctx, private.ID); err != nil || r.State != fw.StateCompleted || calls.Load() != 2 {
		t.Fatalf("run = %+v, %v, %d calls", r, err, calls.Load())
	}
	if !lastOf(t, a, private.ID, fw.EvApprovalDecided, &decided) || decided.Decider != "alice" {
		t.Fatalf("decided = %+v", decided)
	}
}

// TestOnBehalfOfNamesNoPersonOfAStart checks POST /v1/runs: a person
// cannot start a run as another person with the header, and with its own
// subject starts the run as itself. A trusted caller names the person of a
// run in the body, not with the header.
func TestOnBehalfOfNamesNoPersonOfAStart(t *testing.T) {
	callers := tokenMap{
		"alice-token":  {Subject: "alice", Kind: fw.KindUser},
		"member-token": {Subject: fw.KindToken, Kind: fw.KindToken},
	}
	a := newAgent(t, newScript(endTurn("done"), endTurn("done"), endTurn("done")).model(), fw.WithAuthn(callers))
	srv := serve(t, a)

	resp, b := do(t, "POST", srv.URL+"/v1/runs", `{"input":"as bob","wait":true}`, "Authorization", "Bearer alice-token", HeaderOnBehalfOf, "bob")
	decodeProblem(t, resp, b, http.StatusForbidden)
	resp, b = do(t, "POST", srv.URL+"/v1/runs", `{"input":"as bob","principal":"bob","wait":true}`, "Authorization", "Bearer alice-token", HeaderOnBehalfOf, "bob")
	decodeProblem(t, resp, b, http.StatusForbidden)
	if list, err := a.List(t.Context()); err != nil || len(list) != 0 {
		t.Fatalf("the agent holds %d runs, %v", len(list), err)
	}
	resp, b = do(t, "POST", srv.URL+"/v1/runs", `{"input":"mine","wait":true}`, "Authorization", "Bearer alice-token", HeaderOnBehalfOf, "alice")
	if r := decodeRun(t, resp, b, http.StatusOK); r.Principal != "alice" {
		t.Fatalf("alice started the run as %q", r.Principal)
	}
	resp, b = do(t, "POST", srv.URL+"/v1/runs", `{"input":"for ada","wait":true}`, "Authorization", "Bearer member-token", HeaderOnBehalfOf, "ada")
	if r := decodeRun(t, resp, b, http.StatusOK); r.Principal != fw.KindToken {
		t.Fatalf("the header started the run as %q", r.Principal)
	}
	resp, b = do(t, "POST", srv.URL+"/v1/runs", `{"input":"for ada","principal":"ada","principalKind":"user","wait":true}`, "Authorization", "Bearer member-token", HeaderOnBehalfOf, "ada")
	if r := decodeRun(t, resp, b, http.StatusOK); r.Principal != "ada" {
		t.Fatalf("the body started the run as %q", r.Principal)
	}
}
