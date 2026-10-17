package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// busControlPlane answers authorization requests on a bus subject and
// records the subject of every principal that it sees.
type busControlPlane struct {
	mu       sync.Mutex
	subjects []string
	allow    func(p fw.Principal, q fw.AuthzQuery) bool
}

func (c *busControlPlane) serve(t *testing.T, bus fw.Bus, subject string) {
	t.Helper()
	stop, err := bus.Subscribe(context.Background(), subject, func(e fw.Event) error {
		raw, err := fw.DecisionRequestOf(e)
		if err != nil {
			return nil
		}
		var req fw.AuthzRequest
		if json.Unmarshal(raw, &req) != nil || req.Kind != fw.DecisionAuthz {
			return nil
		}
		c.mu.Lock()
		c.subjects = append(c.subjects, req.Principal.Subject)
		c.mu.Unlock()
		var ans fw.AuthzAnswer
		for _, q := range req.Checks {
			ok := c.allow(req.Principal, q)
			d := fw.AuthzDecision{Allow: ok}
			if !ok {
				d.Reason = "the control plane says no"
			}
			ans.Decisions = append(ans.Decisions, d)
		}
		to, reply, err := fw.AnswerDecision(e, ans)
		if err != nil {
			return nil
		}
		return bus.Publish(context.Background(), to, reply)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
}

func (c *busControlPlane) seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.subjects...)
}

// externalEnv prepares an environment whose access goes to a control
// plane on a shared in-memory bus.
func externalEnv(t *testing.T, allow func(fw.Principal, fw.AuthzQuery) bool, opts ...fw.Option) *busControlPlane {
	t.Helper()
	bus := fw.NewMemBus()
	env(t, append([]fw.Option{fw.WithBus(bus)}, opts...)...)
	t.Setenv("FW_ACCESS_EXTERNAL_BUS", "fw.authz")
	cp := &busControlPlane{allow: allow}
	if allow != nil {
		cp.serve(t, bus, "fw.authz")
	}
	return cp
}

func TestRunAsksTheControlPlaneForTheLocalUser(t *testing.T) {
	cp := externalEnv(t, func(fw.Principal, fw.AuthzQuery) bool { return true },
		fw.WithModel(&scriptModel{resps: []*fw.Response{endTurn("hello back")}}))
	code, out, errOut := run(t, "", "run", "hello")
	if code != 0 || !strings.Contains(out, "hello back") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	// access.default is open, but with access.external the local user
	// still reaches the control plane.
	want := localPrincipal().Subject
	seen := cp.seen()
	if len(seen) == 0 || seen[0] != want {
		t.Fatalf("the control plane saw %v, want %s", seen, want)
	}
}

func TestRunNamesARefusalOfTheControlPlane(t *testing.T) {
	externalEnv(t, func(fw.Principal, fw.AuthzQuery) bool { return false },
		fw.WithModel(&scriptModel{resps: []*fw.Response{endTurn("never")}}))
	code, _, errOut := run(t, "", "run", "hello")
	if code == 0 || !strings.Contains(errOut, "the control plane says no") ||
		!strings.Contains(errOut, "the bus subject fw.authz refused it; access.admins and the access blocks do not apply while access.external is set") {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
}

func TestRunNamesAControlPlaneThatDoesNotAnswer(t *testing.T) {
	externalEnv(t, nil, fw.WithModel(&scriptModel{resps: []*fw.Response{endTurn("never")}}))
	t.Setenv("FW_ACCESS_EXTERNAL_TIMEOUT", "50ms")
	code, _, errOut := run(t, "", "run", "hello")
	if code == 0 || !strings.Contains(errOut, "the control plane did not answer: make sure that the bus subject fw.authz is reachable; fw doctor checks it") {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
}

func TestWhoamiAsksTheControlPlane(t *testing.T) {
	externalEnv(t, func(p fw.Principal, q fw.AuthzQuery) bool {
		return q.Action == fw.ActionAdmin && p.Kind == fw.KindLocal
	})
	code, out, errOut := run(t, "", "whoami")
	if code != 0 || !strings.Contains(out, "admin: true\n") ||
		!strings.Contains(out, "acts: always; access.external hands every decision to the bus subject fw.authz") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
}

func TestWhoamiWithoutAnAnswer(t *testing.T) {
	externalEnv(t, nil)
	t.Setenv("FW_ACCESS_EXTERNAL_TIMEOUT", "50ms")
	code, out, errOut := run(t, "", "whoami")
	if code != 1 || !strings.Contains(out, "subject: "+localPrincipal().Subject) || !strings.Contains(out, "admin: unknown\n") ||
		!strings.Contains(errOut, "the control plane did not answer") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
}

func TestAgentsAskTheControlPlane(t *testing.T) {
	cp := externalEnv(t, func(_ fw.Principal, q fw.AuthzQuery) bool {
		return q.Action == fw.ActionAgentView && q.Agent == "reviewer"
	})
	t.Setenv("FW_AGENTS_REVIEWER_DESCRIPTION", "Reviews code")
	t.Setenv("FW_AGENTS_HIDDEN_DESCRIPTION", "Not for you")
	code, out, errOut := run(t, "", "agents")
	if code != 0 || !strings.Contains(out, "reviewer") || strings.Contains(out, "hidden") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	// The listing sends its checks in one request.
	if seen := cp.seen(); len(seen) != 1 || seen[0] != localPrincipal().Subject {
		t.Fatalf("the control plane saw %v", seen)
	}
}

// probePoint is an MCP decision point for the doctor tests. The cli tests
// do not link the MCP driver, so the test registers this stand-in.
type probePoint struct{ err error }

func (probePoint) Decide(context.Context, json.RawMessage) (json.RawMessage, error) {
	return nil, errors.New("not used")
}
func (p probePoint) Probe(context.Context) error { return p.err }

var registerProbe sync.Once

func TestDoctorExternal(t *testing.T) {
	registerProbe.Do(func() {
		fw.RegisterDecisionPoint("mcp", func(_ context.Context, _ *fw.Agent, ext fw.ExternalPolicyConfig) (fw.DecisionPoint, error) {
			if ext.Tool == "missing" {
				return probePoint{err: errors.New("the server lists no tool named missing")}, nil
			}
			return probePoint{}, nil
		})
	})
	externalEnv(t, func(fw.Principal, fw.AuthzQuery) bool { return true })
	doctorSeams(t, &scriptModel{}, nil, nil)
	t.Setenv("FW_TOOLS_MCP_CONTROL_ENDPOINT", "https://cp.example.com/mcp")
	t.Setenv("FW_POLICY_EXTERNAL_MCP", "control")
	_, out, _ := run(t, "", "doctor")
	for _, want := range []string{
		"ok    access: access.external hands every decision to the bus subject fw.authz; access.default, access.admins, and access.creators do not apply, and the access block of each profile goes to it as data\n",
		"ok    access.external: decided by the bus subject fw.authz; this transport has no probe, so fw doctor cannot make sure that it answers\n",
		"ok    policy.external: decided by the tool decide of the MCP server control, which is ready\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	t.Setenv("FW_POLICY_EXTERNAL_TOOL", "missing")
	code, out, _ := run(t, "", "doctor")
	if code != 1 || !strings.Contains(out, "FAIL  policy.external: the tool missing of the MCP server control is not ready: the server lists no tool named missing\n") {
		t.Errorf("exit %d\n%s", code, out)
	}

	// A name that tools.mcp does not hold fails the configuration line.
	t.Setenv("FW_POLICY_EXTERNAL_MCP", "nowhere")
	code, out, _ = run(t, "", "doctor")
	if code != 1 || !strings.Contains(out, `policy.external.mcp names "nowhere", but tools.mcp has no entry with that name`) {
		t.Errorf("exit %d\n%s", code, out)
	}
}
