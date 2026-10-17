package native

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
)

// tokenMap is a test Authn that maps a bearer token to a principal.
type tokenMap map[string]*fw.Principal

func (m tokenMap) Authenticate(r *http.Request) (*fw.Principal, error) {
	tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if p, ok := m[tok]; ok && tok != "" {
		return p, nil
	}
	return nil, fw.ErrUnauthorized
}

func (m tokenMap) Challenge(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="fw"`)
	w.WriteHeader(http.StatusUnauthorized)
}

var people = tokenMap{
	"alice-token": {Subject: "alice", Name: "alice@example.com", Kind: fw.KindUser, Groups: []string{"platform"}},
	"bob-token":   {Subject: "bob", Kind: fw.KindUser},
}

const (
	asAlice = "Bearer alice-token"
	asBob   = "Bearer bob-token"
)

// manageAgent is an agent with three profiles: reviewer, which alice owns
// and bob can view, shared, which every platform member operates, and
// private, which neither can see. The configuration is closed, and alice is
// a creator.
func manageAgent(t *testing.T, m fw.Model, opts ...fw.Option) *fw.Agent {
	t.Helper()
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Access.Default = "closed"
	cfg.Access.Creators = []string{"alice"}
	base := []fw.Option{
		fw.WithConfig(cfg),
		fw.WithAuthn(people),
		fw.WithProfile(fw.AgentProfile{Name: "reviewer", Description: "Reviews code", Access: &fw.Access{Owners: []string{"alice"}, Viewers: []string{"bob"}}}),
		fw.WithProfile(fw.AgentProfile{Name: "shared", Access: &fw.Access{Operators: []string{"group:platform"}}}),
		fw.WithProfile(fw.AgentProfile{Name: "private", Access: &fw.Access{Owners: []string{"carol"}}}),
	}
	return newAgent(t, m, append(base, opts...)...)
}

func decodeJSON(t *testing.T, resp *http.Response, b []byte, status int, v any) {
	t.Helper()
	if resp.StatusCode != status {
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, status, b)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type = %q", ct)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("decode: %v: %s", err, b)
	}
}

func TestWhoamiAndAgents(t *testing.T) {
	a := manageAgent(t, newScript(endTurn("ok")).model())
	srv := serve(t, a)
	if _, err := a.Run(fw.WithPrincipal(testCtx(t), people["alice-token"]), "review", fw.AsAgent("reviewer")); err != nil {
		t.Fatal(err)
	}

	// The roles of the caller come from view.WhoamiOf, which the workbench
	// serves at GET /v1/whoami with the teams of the caller. The kernel
	// serves no whoami route.
	who, err := view.WhoamiOf(fw.WithPrincipal(testCtx(t), people["alice-token"]), a)
	if err != nil {
		t.Fatal(err)
	}
	if who.Principal.Subject != "alice" || who.Principal.Groups[0] != "platform" || who.Roles["reviewer"] != "owner" || who.Roles["shared"] != "operator" ||
		who.Roles["private"] != "" || !who.CanCreate || who.Admin {
		t.Fatalf("alice = %+v", who)
	}
	bob, err := view.WhoamiOf(fw.WithPrincipal(testCtx(t), people["bob-token"]), a)
	if err != nil {
		t.Fatal(err)
	}
	if bob.Principal.Subject != "bob" || bob.Roles["reviewer"] != "viewer" || len(bob.Roles) != 1 || bob.CanCreate {
		t.Fatalf("bob = %+v", bob)
	}

	if resp, b := do(t, "GET", srv.URL+"/v1/whoami", "", "Authorization", asAlice); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /v1/whoami = %d: %s", resp.StatusCode, b)
	}

	var agents struct{ Agents []view.AgentView }
	resp, b := do(t, "GET", srv.URL+"/v1/agents", "", "Authorization", asBob)
	decodeJSON(t, resp, b, http.StatusOK, &agents)
	if len(agents.Agents) != 1 || agents.Agents[0].Name != "reviewer" || agents.Agents[0].Role != "viewer" || agents.Agents[0].Usage.Runs != 1 || agents.Agents[0].Usage.Tokens != 15 {
		t.Fatalf("bob agents = %+v", agents)
	}
	resp, b = do(t, "GET", srv.URL+"/v1/agents", "", "Authorization", asAlice)
	decodeJSON(t, resp, b, http.StatusOK, &agents)
	if len(agents.Agents) != 2 || agents.Agents[0].Role != "owner" || agents.Agents[1].Name != "shared" || agents.Agents[1].Role != "operator" {
		t.Fatalf("alice agents = %+v", agents)
	}

	var one view.AgentView
	resp, b = do(t, "GET", srv.URL+"/v1/agents/reviewer", "", "Authorization", asBob)
	decodeJSON(t, resp, b, http.StatusOK, &one)
	if one.Name != "reviewer" || one.Role != "viewer" || one.Description != "Reviews code" || one.Access.Owners[0] != "alice" {
		t.Fatalf("agent = %+v", one)
	}
	for _, name := range []string{"private", "nope"} {
		resp, b = do(t, "GET", srv.URL+"/v1/agents/"+name, "", "Authorization", asBob)
		if p := decodeProblem(t, resp, b, http.StatusNotFound); !strings.Contains(p.Detail, name) {
			t.Errorf("problem for %s = %+v", name, p)
		}
	}
}

func TestPutAndDeleteAgent(t *testing.T) {
	a := manageAgent(t, newScript().model())
	srv := serve(t, a)
	url := srv.URL + "/v1/agents/reviewer"

	// A stranger gets a 403 that names the role.
	resp, b := do(t, "PUT", url, `{"description":"mine"}`, "Authorization", asBob)
	if p := decodeProblem(t, resp, b, http.StatusForbidden); !strings.Contains(p.Detail, "bob agent.edit needs an owner entry on the agent reviewer") {
		t.Fatalf("problem = %+v", p)
	}
	resp, b = do(t, "PUT", srv.URL+"/v1/agents/bobs", `{}`, "Authorization", asBob)
	if p := decodeProblem(t, resp, b, http.StatusForbidden); !strings.Contains(p.Detail, "access.creators") {
		t.Fatalf("problem = %+v", p)
	}
	resp, b = do(t, "DELETE", url, "", "Authorization", asBob)
	if p := decodeProblem(t, resp, b, http.StatusForbidden); !strings.Contains(p.Detail, "owner entry") {
		t.Fatalf("problem = %+v", p)
	}

	// The owner replaces the profile; the access block survives.
	var v view.AgentView
	resp, b = do(t, "PUT", url, `{"description":"Reviews pull requests","system":"You review.","tools":["files"],"skills":[],"effort":"low","timeZone":"Europe/Berlin","policy":{"default":"ask"},"budget":{"maxTurns":5,"deadline":600000000000}}`, "Authorization", asAlice)
	decodeJSON(t, resp, b, http.StatusOK, &v)
	if v.Name != "reviewer" || v.Description != "Reviews pull requests" || v.Role != "owner" || v.Access == nil || v.Access.Owners[0] != "alice" || v.Access.Viewers[0] != "bob" ||
		v.Skills == nil || len(v.Skills) != 0 || v.Budget == nil || v.Budget.Deadline.Minutes() != 10 || v.Policy.Default != "ask" || v.TimeZone != "Europe/Berlin" {
		t.Fatalf("put = %+v", v)
	}
	// GET returns the time zone.
	resp, b = do(t, "GET", url, "", "Authorization", asAlice)
	if !strings.Contains(string(b), `"timeZone":"Europe/Berlin"`) {
		t.Fatalf("get = %d %s", resp.StatusCode, b)
	}
	// A new profile answers 201 and names the creator as its owner.
	resp, b = do(t, "PUT", srv.URL+"/v1/agents/fresh", `{"name":"fresh","description":"new"}`, "Authorization", asAlice)
	decodeJSON(t, resp, b, http.StatusCreated, &v)
	if resp.Header.Get("Location") != "/v1/agents/fresh" || v.Access == nil || v.Access.Owners[0] != "alice" || v.Role != "owner" {
		t.Fatalf("create = %+v, location %q", v, resp.Header.Get("Location"))
	}
	// Bad bodies are 400 problems.
	for name, body := range map[string]string{
		"other name": `{"name":"other"}`,
		"effort":     `{"effort":"turbo"}`,
		"policy":     `{"policy":{"default":"maybe"}}`,
		"unknown":    `{"colour":"blue"}`,
		"model":      `{"model":"nope"}`,
		"time zone":  `{"timeZone":"Mars/Olympus"}`,
	} {
		resp, b = do(t, "PUT", srv.URL+"/v1/agents/fresh", body, "Authorization", asAlice)
		p := decodeProblem(t, resp, b, http.StatusBadRequest)
		if name == "time zone" && !strings.Contains(p.Detail, "timeZone") {
			t.Errorf("time zone problem = %+v", p)
		}
	}
	resp, b = do(t, "PUT", srv.URL+"/v1/agents/..bad", `{}`, "Authorization", asAlice)
	decodeProblem(t, resp, b, http.StatusBadRequest)

	// Delete: 404 for an unknown name, 204 for the owner, then 404 again.
	resp, b = do(t, "DELETE", srv.URL+"/v1/agents/nope", "", "Authorization", asAlice)
	if p := decodeProblem(t, resp, b, http.StatusNotFound); !strings.Contains(p.Detail, `"nope"`) {
		t.Fatalf("problem = %+v", p)
	}
	resp, b = do(t, "DELETE", srv.URL+"/v1/agents/fresh", "", "Authorization", asAlice)
	if resp.StatusCode != http.StatusNoContent || len(b) != 0 {
		t.Fatalf("delete = %d %s", resp.StatusCode, b)
	}
	resp, b = do(t, "GET", srv.URL+"/v1/agents/fresh", "", "Authorization", asAlice)
	decodeProblem(t, resp, b, http.StatusNotFound)
}

func TestRunsFiltersAndEventsPage(t *testing.T) {
	ctx := testCtx(t)
	a := manageAgent(t, newScript(endTurn("one"), endTurn("two"), endTurn("three")).model())
	srv := serve(t, a)
	actx := fw.WithPrincipal(ctx, people["alice-token"])
	r1, err := a.Run(actx, "review this", fw.AsAgent("reviewer"))
	if err != nil {
		t.Fatal(err)
	}
	r2, err := a.Run(actx, "shared work", fw.AsAgent("shared"))
	if err != nil {
		t.Fatal(err)
	}
	child, err := a.Run(ctx, "part", fw.AsAgent("shared"), fw.WithParent(r2.ID))
	if err != nil {
		t.Fatal(err)
	}
	list := func(query, auth string) []fw.Run {
		t.Helper()
		var out struct{ Runs []fw.Run }
		resp, b := do(t, "GET", srv.URL+"/v1/runs"+query, "", "Authorization", auth)
		decodeJSON(t, resp, b, http.StatusOK, &out)
		return out.Runs
	}
	if runs := list("", asBob); len(runs) != 1 || runs[0].ID != r1.ID {
		t.Fatalf("bob runs = %+v", runs)
	}
	if runs := list("", asAlice); len(runs) != 3 || runs[0].ID != child.ID {
		t.Fatalf("alice runs = %+v", runs)
	}
	if runs := list("?agent=shared&state=completed&principal=alice&limit=1", asAlice); len(runs) != 1 || runs[0].ID != child.ID {
		t.Fatalf("filtered = %+v", runs)
	}
	if runs := list("?state=active", asAlice); len(runs) != 0 {
		t.Fatalf("active = %+v", runs)
	}
	if runs := list("?agent=reviewer", asAlice); len(runs) != 1 || runs[0].ID != r1.ID {
		t.Fatalf("by agent = %+v", runs)
	}
	resp, b := do(t, "GET", srv.URL+"/v1/runs?limit=0", "", "Authorization", asAlice)
	decodeProblem(t, resp, b, http.StatusBadRequest)

	// A JSON client gets a page of the ledger instead of a stream.
	var page struct {
		Run    fw.Run
		Events []fw.Event
		Lines  []view.EventLine
	}
	resp, b = do(t, "GET", srv.URL+"/v1/runs/"+r1.ID+"/events?limit=2", "", "Authorization", asAlice, "Accept", "application/json")
	decodeJSON(t, resp, b, http.StatusOK, &page)
	if page.Run.ID != r1.ID || len(page.Events) != 2 || page.Events[1].Type != fw.EvRunFinished || page.Events[0].Seq != page.Run.Seq-1 {
		t.Fatalf("tail = %+v", page)
	}
	if len(page.Lines) != 2 || page.Lines[1].Seq != page.Events[1].Seq || !strings.HasPrefix(page.Lines[1].Line, "finished") {
		t.Fatalf("lines = %+v", page.Lines)
	}
	resp, b = do(t, "GET", srv.URL+"/v1/runs/"+r1.ID+"/events?from=1&limit=2", "", "Authorization", asAlice, "Accept", "application/json")
	decodeJSON(t, resp, b, http.StatusOK, &page)
	if len(page.Events) != 2 || page.Events[0].Seq != 1 || page.Events[1].Seq != 2 {
		t.Fatalf("page = %+v", page.Events)
	}
	resp, b = do(t, "GET", srv.URL+"/v1/runs/"+r2.ID+"/events", "", "Authorization", asBob, "Accept", "application/json")
	if p := decodeProblem(t, resp, b, http.StatusForbidden); !strings.Contains(p.Detail, "bob run.view") {
		t.Fatalf("problem = %+v", p)
	}
	resp, b = do(t, "GET", srv.URL+"/v1/runs/"+r2.ID, "", "Authorization", asBob)
	decodeProblem(t, resp, b, http.StatusForbidden)

}

// downAuthz is an Authz whose control plane does not answer.
var downAuthz = fw.AuthzFunc(func(context.Context, *fw.Principal, string, fw.Target) error {
	return fmt.Errorf("%w: test", fw.ErrAuthzUnavailable)
})

func TestAuthzUnavailableIs503(t *testing.T) {
	a := manageAgent(t, newScript(endTurn("never")).model(), fw.WithAuthz(downAuthz))
	srv := serve(t, a)
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/v1/runs", `{"input":"hi"}`},
		{"GET", "/v1/agents", ""},
		{"GET", "/v1/agents/reviewer", ""},
	} {
		resp, b := do(t, c.method, srv.URL+c.path, c.body, "Authorization", asAlice)
		p := decodeProblem(t, resp, b, http.StatusServiceUnavailable)
		if resp.Header.Get("Retry-After") != "5" || !strings.Contains(p.Detail, "the authorization service did not answer") || !strings.Contains(p.Detail, "test") {
			t.Fatalf("%s %s: Retry-After %q, detail %q", c.method, c.path, resp.Header.Get("Retry-After"), p.Detail)
		}
	}
}

func TestPutAgentDryRun(t *testing.T) {
	a := manageAgent(t, newScript().model())
	srv := serve(t, a)

	// A dry run of a new profile says what it can do and saves nothing.
	var d view.Draft
	resp, b := do(t, "PUT", srv.URL+"/v1/agents/fresh?dryRun=true", `{"description":"new","tools":["files"]}`, "Authorization", asAlice)
	decodeJSON(t, resp, b, http.StatusOK, &d)
	if d.Exists || d.Name != "fresh" || d.Role != "owner" {
		t.Fatalf("draft = %+v", d)
	}
	if _, ok := a.ProfileNamed("fresh"); ok {
		t.Fatal("a dry run saved the profile")
	}
	// An existing profile reports exists and the role of the caller.
	resp, b = do(t, "PUT", srv.URL+"/v1/agents/reviewer?dryRun=true", `{"description":"changed"}`, "Authorization", asAlice)
	decodeJSON(t, resp, b, http.StatusOK, &d)
	if !d.Exists || d.Role != "owner" {
		t.Fatalf("draft of reviewer = %+v", d)
	}
	if p, _ := a.ProfileNamed("reviewer"); p.Description != "Reviews code" {
		t.Fatalf("a dry run changed reviewer: %+v", p)
	}
	// A dry run checks the rights as a save does.
	resp, b = do(t, "PUT", srv.URL+"/v1/agents/reviewer?dryRun=true", `{}`, "Authorization", asBob)
	decodeProblem(t, resp, b, http.StatusForbidden)
}
