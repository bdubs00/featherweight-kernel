package native

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// fakeFleet is a fleet with one other member, "owner", that a test server
// stands for. Forward sends each request to that server, as the fleet
// driver does, and records it.
type fakeFleet struct {
	name    string
	owner   string // the URL of the owner
	members []fw.FleetMember

	mu      sync.Mutex
	runs    []*fw.Run
	err     error  // Forward returns it when set
	cred    string // a credential that replaces the one of the caller
	forward []http.Header
}

func (f *fakeFleet) Name() string { return f.name }

func (f *fakeFleet) Members(context.Context) ([]fw.FleetMember, error) { return f.members, nil }

func (f *fakeFleet) Runs(context.Context) ([]*fw.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*fw.Run, len(f.runs))
	for i, r := range f.runs {
		c := *r
		out[i] = &c
	}
	return out, nil
}

func (f *fakeFleet) Run(_ context.Context, id string) (*fw.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.runs {
		if r.ID == id {
			c := *r
			return &c, nil
		}
	}
	return nil, fw.ErrRunNotFound
}

func (f *fakeFleet) Forward(ctx context.Context, member string, req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	ferr, cred := f.err, f.cred
	f.mu.Unlock()
	if ferr != nil {
		return nil, ferr
	}
	if member != "owner" {
		return nil, errors.Join(fw.ErrFleetNoRoute, errors.New("unknown member "+member))
	}
	if cred == "" {
		cred = fw.CredentialFrom(ctx)
	}
	if cred == "" {
		return nil, fw.ErrFleetNoCredential
	}
	u, err := url.Parse(f.owner + req.URL.String())
	if err != nil {
		return nil, err
	}
	out := req.Clone(ctx)
	out.URL, out.Host, out.RequestURI = u, u.Host, ""
	out.Header.Set("Authorization", cred)
	out.Header.Set(fw.HeaderFleetVia, f.name)
	f.mu.Lock()
	f.forward = append(f.forward, out.Header.Clone())
	f.mu.Unlock()
	return http.DefaultClient.Do(out)
}

func (f *fakeFleet) set(runs ...*fw.Run) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = nil
	for _, r := range runs {
		c := *r
		c.Member = "owner"
		f.runs = append(f.runs, &c)
	}
}

func (f *fakeFleet) fail(err error, cred string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err, f.cred = err, cred
}

func (f *fakeFleet) forwarded() []http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]http.Header(nil), f.forward...)
}

// fleetPair is a view with a fake fleet and an owner agent that serves the
// runs of the member "owner". Alice owns the agent reviewer on both.
type fleetPair struct {
	view, owner       *fw.Agent
	viewURL, ownerURL string
	fleet             *fakeFleet
	calls             *atomic.Int32
	remote            *fw.Run // a run on the owner that waits for an approval
	local             *fw.Run // a run on the view that completed
}

// The options apply to both agents.
func newFleetPair(t *testing.T, opts ...fw.Option) *fleetPair {
	t.Helper()
	p := &fleetPair{calls: &atomic.Int32{}}
	p.owner = manageAgent(t, newScript(callTool("c1", "rm", `{"path":"x"}`), endTurn("done"), endTurn("again")).model(), append([]fw.Option{fw.WithTools(rmTool(p.calls))}, opts...)...)
	p.ownerURL = serve(t, p.owner).URL
	p.fleet = &fakeFleet{name: "view", owner: p.ownerURL, members: []fw.FleetMember{
		{Name: "view", Self: true, State: fw.MemberLive, AccessMode: fw.AccessModeRules},
		{Name: "owner", State: fw.MemberLive, CanForward: true, AccessMode: fw.AccessModeRules, Agents: []fw.FleetAgent{{Name: "reviewer", Role: "owner"}}},
	}}
	p.view = manageAgent(t, newScript(endTurn("local")).model(), append([]fw.Option{fw.WithFleet(p.fleet)}, opts...)...)
	p.viewURL = serve(t, p.view).URL

	resp, b := do(t, "POST", p.ownerURL+"/v1/runs", `{"agent":"reviewer","input":"clean up","wait":true}`, "Authorization", asAlice)
	p.remote = decodeRun(t, resp, b, http.StatusOK)
	if p.remote.State != fw.StateInputRequired || p.remote.Pending == nil {
		t.Fatalf("remote run = %+v", p.remote)
	}
	p.fleet.set(p.remote)
	resp, b = do(t, "POST", p.viewURL+"/v1/runs", `{"agent":"reviewer","input":"here","wait":true}`, "Authorization", asAlice)
	p.local = decodeRun(t, resp, b, http.StatusOK)
	return p
}

func runIDs(runs []*fw.Run) string {
	var ids []string
	for _, r := range runs {
		id := r.ID
		if r.Member != "" {
			id += "@" + r.Member
		}
		ids = append(ids, id)
	}
	return strings.Join(ids, ",")
}

func TestFleetActs(t *testing.T) {
	ctx := testCtx(t)
	p := newFleetPair(t)
	base := p.viewURL + "/v1/runs/" + p.remote.ID

	// An approval through the view reaches the owner, which records alice.
	resp, b := do(t, "POST", base+"/approvals/"+p.remote.Pending.ID, `{"decision":"allow","note":"fine"}`, "Authorization", asAlice)
	if r := decodeRun(t, resp, b, http.StatusOK); r.Member != "owner" {
		t.Fatalf("decided run = %+v", r)
	}
	r, err := p.owner.Wait(ctx, p.remote.ID)
	if err != nil || r.State != fw.StateCompleted || p.calls.Load() != 1 {
		t.Fatalf("owner run = %+v, %v, calls %d", r, err, p.calls.Load())
	}
	var d fw.ApprovalDecidedData
	for e, err := range p.owner.Ledger().Read(ctx, r.ID, 0) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Type == fw.EvApprovalDecided {
			_ = e.Decode(&d)
		}
	}
	if d.Decider != "alice" || d.Note != "fine" {
		t.Fatalf("decided = %+v", d)
	}

	// A follow-up, then a cancel, both on the owner.
	resp, b = do(t, "POST", base+"/messages", `{"content":"once more"}`, "Authorization", asAlice)
	if r := decodeRun(t, resp, b, http.StatusAccepted); r.Member != "owner" {
		t.Fatalf("sent run = %+v", r)
	}
	if _, err := p.owner.WaitFor(ctx, p.remote.ID, func(r *fw.Run) bool { return r.State.Terminal() }); err != nil {
		t.Fatal(err)
	}
	resp, b = do(t, "POST", base+"/cancel", `{}`, "Authorization", asAlice)
	decodeRun(t, resp, b, http.StatusOK)

	// Bob only views the reviewer agent, so the owner refuses him.
	resp, b = do(t, "POST", base+"/cancel", `{}`, "Authorization", asBob)
	decodeProblem(t, resp, b, http.StatusForbidden)

	// A start on another member goes there; a start on this member stays.
	resp, b = do(t, "POST", p.viewURL+"/v1/runs?member=owner", `{"agent":"shared","input":"remote"}`, "Authorization", asAlice, HeaderIdempotencyKey, "k1")
	started := decodeRun(t, resp, b, http.StatusCreated)
	if started.Member != "owner" || resp.Header.Get("Location") != "/v1/runs/"+started.ID {
		t.Fatalf("started = %+v, location %q", started, resp.Header.Get("Location"))
	}
	if _, err := p.owner.Get(context.Background(), started.ID); err != nil {
		t.Fatalf("the owner lacks the run: %v", err)
	}
	if h := p.fleet.forwarded(); h[len(h)-1].Get(HeaderIdempotencyKey) != "k1" || h[len(h)-1].Get("Content-Type") != "application/json" {
		t.Errorf("forwarded headers = %v", h[len(h)-1])
	}
	resp, b = do(t, "POST", p.viewURL+"/v1/runs?member=view", `{"agent":"shared","input":"here"}`, "Authorization", asAlice)
	if r := decodeRun(t, resp, b, http.StatusCreated); r.Member != "" {
		t.Fatalf("local start = %+v", r)
	}
	resp, b = do(t, "POST", p.viewURL+"/v1/runs?member=owner", `{"input":"x"}`, "Authorization", asAlice, fw.HeaderFleetVia, "elsewhere")
	decodeProblem(t, resp, b, http.StatusBadGateway)
}

func TestFleetErrors(t *testing.T) {
	p := newFleetPair(t)
	get := func() (*http.Response, []byte) {
		return do(t, "GET", p.viewURL+"/v1/runs/"+p.remote.ID, "", "Authorization", asAlice)
	}
	p.fleet.fail(errors.Join(fw.ErrFleetNoRoute, errors.New("the member owner has no URL")), "")
	resp, b := get()
	if pr := decodeProblem(t, resp, b, http.StatusBadGateway); !strings.Contains(pr.Detail, "no URL") {
		t.Errorf("detail = %q", pr.Detail)
	}
	p.fleet.fail(fw.ErrFleetNoCredential, "")
	resp, b = get()
	decodeProblem(t, resp, b, http.StatusForbidden)
	p.fleet.fail(errors.New("dial tcp: connection refused"), "")
	resp, b = get()
	if pr := decodeProblem(t, resp, b, http.StatusBadGateway); !strings.Contains(pr.Detail, "did not answer") || strings.Contains(pr.Detail, "dial") {
		t.Errorf("detail = %q", pr.Detail)
	}
	// The owner does not accept the credential: the caller gets a 403, not a
	// 401 that would sign it out here.
	p.fleet.fail(nil, "Bearer stranger")
	resp, b = get()
	if pr := decodeProblem(t, resp, b, http.StatusForbidden); !strings.Contains(pr.Detail, "did not accept your credential") {
		t.Errorf("detail = %q", pr.Detail)
	}
	if resp.Header.Get("WWW-Authenticate") != "" {
		t.Errorf("the 403 carries a challenge")
	}
}

func TestWithMember(t *testing.T) {
	if got := string(withMember([]byte(`{"id":"r1","state":"running"}`), "m")); !strings.Contains(got, `"member":"m"`) {
		t.Errorf("run = %s", got)
	}
	var page struct {
		Run    map[string]any `json:"run"`
		Events []any          `json:"events"`
	}
	if err := json.Unmarshal(withMember([]byte(`{"run":{"id":"r1","state":"running"},"events":[]}`), "m"), &page); err != nil || page.Run["member"] != "m" || page.Events == nil {
		t.Errorf("page = %v, %v", page, err)
	}
	for _, in := range []string{`{"runs":[]}`, `[1,2]`, `not json`} {
		if got := string(withMember([]byte(in), "m")); got != in {
			t.Errorf("%s became %s", in, got)
		}
	}
}
