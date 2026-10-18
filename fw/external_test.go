package fw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// controlPlane answers decision requests on a bus subject, like a control
// plane outside the process. It records every request.
type controlPlane struct {
	mu      sync.Mutex
	authz   []AuthzRequest
	policy  []PolicyRequest
	decide  func(p Principal, q AuthzQuery) AuthzDecision
	verdict func(r PolicyRequest) PolicyAnswer
}

func (c *controlPlane) serve(t *testing.T, bus Bus, subject string) {
	t.Helper()
	stop, err := bus.Subscribe(context.Background(), subject, func(e Event) error {
		raw, err := DecisionRequestOf(e)
		if err != nil {
			t.Errorf("request: %v", err)
			return nil
		}
		var kind struct {
			Kind string `json:"kind"`
		}
		_ = json.Unmarshal(raw, &kind)
		var answer any
		c.mu.Lock()
		switch kind.Kind {
		case DecisionAuthz:
			var req AuthzRequest
			_ = json.Unmarshal(raw, &req)
			c.authz = append(c.authz, req)
			var ans AuthzAnswer
			for _, q := range req.Checks {
				ans.Decisions = append(ans.Decisions, c.decide(req.Principal, q))
			}
			answer = ans
		case DecisionPolicy:
			var req PolicyRequest
			_ = json.Unmarshal(raw, &req)
			c.policy = append(c.policy, req)
			answer = c.verdict(req)
		}
		c.mu.Unlock()
		subject, reply, err := AnswerDecision(e, answer)
		if err != nil {
			t.Errorf("answer: %v", err)
			return nil
		}
		return bus.Publish(context.Background(), subject, reply)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
}

func (c *controlPlane) authzRequests() []AuthzRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]AuthzRequest(nil), c.authz...)
}

func (c *controlPlane) policyRequests() []PolicyRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]PolicyRequest(nil), c.policy...)
}

// externalAgent builds an agent whose policy goes to the bus.
func externalAgent(t *testing.T, m Model, bus Bus, mutate func(*Config), opts ...Option) *Agent {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Policy.External = &ExternalPolicyConfig{Bus: "fw.policy", Timeout: 2 * time.Second}
	if mutate != nil {
		mutate(cfg)
	}
	return newTestAgent(t, m, append([]Option{WithConfig(cfg), WithBus(bus)}, opts...)...)
}

var (
	alice = &Principal{Subject: "alice", Kind: KindUser, Groups: []string{"platform"}}
	bob   = &Principal{Subject: "bob", Kind: KindUser}
)

// busAuthz is the smallest authorization plug over a decision point, as the
// workbench builds one: it sends one AuthzRequest per check and refuses
// when the control plane does not answer. It decides about the lineage
// itself, so the kernel does not ask again for the ancestors.
type busAuthz struct {
	a  *Agent
	dp DecisionPoint
}

func (z *busAuthz) DecidesLineage() {}

func (z *busAuthz) Allow(ctx context.Context, p *Principal, action string, t Target) error {
	agent := t.Agent
	if agent == "" && t.Run != nil {
		agent = t.Run.Agent
	}
	req := AuthzRequest{Kind: DecisionAuthz, Version: DecisionVersion, Principal: *p,
		Checks: []AuthzQuery{{Action: action, Agent: agent, Run: z.a.RunFacts(t.Run)}}}
	body, _ := json.Marshal(req)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	raw, err := z.dp.Decide(ctx, body)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrAuthzUnavailable, err)
	}
	var ans AuthzAnswer
	if json.Unmarshal(raw, &ans) != nil || len(ans.Decisions) != 1 {
		return fmt.Errorf("%w: a bad answer", ErrAuthzUnavailable)
	}
	if !ans.Decisions[0].Allow {
		return fmt.Errorf("%w: %s", ErrForbidden, ans.Decisions[0].Reason)
	}
	return nil
}

// WithAuthzFrom builds the plug from the agent, so a plug that needs the
// bus and the configuration of the agent, such as the control-plane
// authorization of the workbench, plugs in through one option. The agent
// reports the mode that the builder names, closes the decision point, and
// probes it for fw doctor. A LineageAuthz is not asked again for the
// ancestors of a run after a refusal.
func TestWithAuthzFrom(t *testing.T) {
	ctx := testCtx(t)
	bus := NewMemBus()
	var asked atomic.Int32
	cp := &controlPlane{decide: func(p Principal, q AuthzQuery) AuthzDecision {
		asked.Add(1)
		if q.Run != nil && q.Run.ID != "" && p.Subject == "bob" {
			return AuthzDecision{Reason: "bob reads no run"}
		}
		return AuthzDecision{Allow: p.Subject == "alice" || p.Subject == "bob"}
	}}
	cp.serve(t, bus, "fw.authz")
	build := func(a *Agent) (Authz, string, error) {
		dp, err := a.NewDecisionPoint(context.Background(), "workbench.access", ExternalPolicyConfig{Bus: "fw.authz"})
		if err != nil {
			return nil, "", err
		}
		return &busAuthz{a: a, dp: dp}, AccessModeExternal, nil
	}
	cfg := DefaultConfig()
	cfg.Access.Default = "closed" // the rules would refuse alice; the plug decides instead
	a := newTestAgent(t, newScript(endTurn("p"), endTurn("c")).Model(), WithConfig(cfg), WithBus(bus), WithAuthzFrom(build))
	if a.AccessMode() != AccessModeExternal {
		t.Fatalf("mode = %q", a.AccessMode())
	}
	st := a.ProbeExternal(ctx)
	if len(st) != 1 || st[0].Use != "workbench.access" || st[0].Probed || st[0].Target != "the bus subject fw.authz" {
		t.Fatalf("status = %+v", st)
	}
	parent, err := a.Run(WithPrincipal(ctx, alice), "parent")
	mustState(t, parent, err, StateCompleted)
	child, err := a.Run(ctx, "child", WithParent(parent.ID))
	mustState(t, child, err, StateCompleted)
	// A refusal about the child is final: one request, no walk up the tree.
	asked.Store(0)
	if _, err := a.Get(WithPrincipal(ctx, bob), child.ID); !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "bob reads no run") {
		t.Fatalf("bob reads the child: %v", err)
	}
	if n := asked.Load(); n != 1 {
		t.Fatalf("one refusal asked %d times", n)
	}
	// A nil plug from the builder keeps the rules.
	b := newTestAgent(t, newScript().Model(), WithAuthzFrom(func(*Agent) (Authz, string, error) { return nil, "", nil }))
	if b.AccessMode() != AccessModeRules {
		t.Fatalf("mode without a plug = %q", b.AccessMode())
	}
	// An error of the builder is an error of New.
	base := []Option{WithoutDefaults(), WithAuthn(NoAuthn{}), WithLedger(NewMemLedger()), WithBus(NewMemBus()), WithLogger(quietLogger())}
	if _, err := New(append(base, WithAuthzFrom(func(*Agent) (Authz, string, error) { return nil, "", errors.New("no control plane") }))...); err == nil || !strings.Contains(err.Error(), "no control plane") {
		t.Fatalf("New with a builder that fails = %v", err)
	}
}

func TestExternalPolicy(t *testing.T) {
	ctx := testCtx(t)
	bus := NewMemBus()
	cp := &controlPlane{verdict: func(r PolicyRequest) PolicyAnswer {
		switch r.Tool.Name {
		case "rm":
			return PolicyAnswer{Verdict: "deny", Reason: "no deletes in prod"}
		case "send":
			return PolicyAnswer{Verdict: "ask", Reason: "a person signs off on sends"}
		}
		return PolicyAnswer{Verdict: "allow"}
	}}
	cp.serve(t, bus, "fw.policy")
	var rmCalls, sendCalls, readCalls atomic.Int32
	count := func(name string, n *atomic.Int32, spec ToolSpec) Tool {
		spec.Name = name
		return NewTool(spec, func(context.Context, json.RawMessage) (ToolResult, error) {
			n.Add(1)
			return TextResult(name + " done"), nil
		})
	}
	tools := WithTools(
		count("rm", &rmCalls, ToolSpec{}),
		count("send", &sendCalls, ToolSpec{}),
		// A destructive read: the local default rules ask, but the control
		// plane replaces them and allows it.
		count("read", &readCalls, ToolSpec{Destructive: true}),
	)
	s := newScript(callTools(use("c1", "rm", `{"path":"/"}`), use("c2", "read", `{}`), use("c3", "send", `{"to":"x"}`)), endTurn("done"))
	cfg := DefaultConfig()
	cfg.Policy.External = &ExternalPolicyConfig{Bus: "fw.policy", Timeout: 2 * time.Second}
	a := newTestAgent(t, s.Model(), WithConfig(cfg), WithBus(bus), tools)
	r, err := a.Run(ctx, "work", WithPrincipalName("alice"))
	mustState(t, r, err, StateInputRequired)
	if r.Pending == nil || r.Pending.Tool != "send" || !strings.Contains(r.Pending.Reason, "a person signs off on sends") {
		t.Fatalf("pending = %+v", r.Pending)
	}
	if err := a.Decide(ctx, r.ID, ApprovalDecision{ApprovalID: r.Pending.ID, Decision: "allow"}); err != nil {
		t.Fatal(err)
	}
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)
	if rmCalls.Load() != 0 || readCalls.Load() != 1 || sendCalls.Load() != 1 {
		t.Fatalf("calls rm=%d read=%d send=%d", rmCalls.Load(), readCalls.Load(), sendCalls.Load())
	}
	// The kernel asks again for the calls that were still open after the
	// approval, so a verdict holds at the moment that a call runs.
	reqs := cp.policyRequests()
	if len(reqs) < 3 || reqs[0].Kind != DecisionPolicy || reqs[0].Tool.Name != "rm" || string(reqs[0].Input) != `{"path":"/"}` ||
		reqs[0].Run == nil || reqs[0].Run.ID != r.ID || reqs[0].Run.Principal != "alice" || !reqs[1].Tool.Destructive {
		t.Fatalf("policy requests = %+v", reqs)
	}
}

func TestExternalPolicyIsAFloor(t *testing.T) {
	ctx := testCtx(t)
	bus := NewMemBus()
	cp := &controlPlane{verdict: func(PolicyRequest) PolicyAnswer { return PolicyAnswer{Verdict: "allow"} }}
	cp.serve(t, bus, "fw.policy")
	var calls atomic.Int32
	rm := destructiveTool(&calls)
	s := newScript(callTools(use("c1", "rm", `{}`)), endTurn("done"))
	cfg := DefaultConfig()
	cfg.Policy.External = &ExternalPolicyConfig{Bus: "fw.policy"}
	a := newTestAgent(t, s.Model(), WithConfig(cfg), WithBus(bus), WithTools(rm))
	// A profile can still make the decision stricter, never looser.
	r, err := a.Run(ctx, "clean", WithRunProfile(AgentProfile{Policy: &Rules{Deny: []string{"rm"}}}))
	mustState(t, r, err, StateCompleted)
	if calls.Load() != 0 {
		t.Fatal("the profile rule did not tighten the external verdict")
	}
}

func TestExternalPolicyUnavailableDenies(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	read := NewTool(ToolSpec{Name: "read", ReadOnly: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		calls.Add(1)
		return TextResult("read"), nil
	})
	s := newScript(callTools(use("c1", "read", `{}`)), endTurn("done"))
	cfg := DefaultConfig()
	cfg.Policy.External = &ExternalPolicyConfig{Bus: "fw.policy", Timeout: 30 * time.Millisecond}
	a := newTestAgent(t, s.Model(), WithConfig(cfg), WithTools(read))
	r, err := a.Run(ctx, "read")
	mustState(t, r, err, StateCompleted)
	res := lastToolResults(t, s.requests()[1])
	if calls.Load() != 0 || !res[0].IsError || !strings.Contains(TextOf(res[0].Content), "did not answer") {
		t.Fatalf("calls = %d, result = %+v", calls.Load(), res[0])
	}
}

func TestExternalConfigErrors(t *testing.T) {
	base := []Option{WithoutDefaults(), WithAuthn(NoAuthn{}), WithLedger(NewMemLedger()), WithBus(NewMemBus()), WithLogger(quietLogger())}
	cfg := DefaultConfig()
	cfg.Policy.External = &ExternalPolicyConfig{MCP: "cp", Bus: "fw.policy"}
	if _, err := New(append(base, WithConfig(cfg))...); err == nil || !strings.Contains(err.Error(), "exactly one of mcp and bus") {
		t.Fatalf("both transports = %v", err)
	}
	cfg = DefaultConfig()
	cfg.Policy.External = &ExternalPolicyConfig{MCP: "cp"}
	if _, err := New(append(base, WithConfig(cfg))...); err == nil || !strings.Contains(err.Error(), "tools/mcp") {
		t.Fatalf("mcp without the driver = %v", err)
	}
	// The control servers are the one of policy.external and the ones that
	// linked packages register, from their own blocks.
	RegisterControlServers(func(c *Config) []string {
		if c.Listen == "control-test" {
			return []string{"cp"}
		}
		return nil
	})
	cfg = DefaultConfig()
	cfg.Listen = "control-test"
	cfg.Policy.External = &ExternalPolicyConfig{MCP: "cp2", Tool: "check"}
	if got := cfg.ControlServers(); len(got) != 2 || !got["cp"] || !got["cp2"] || !cfg.IsControlServer("CP") {
		t.Fatalf("control servers = %v", got)
	}
	// An option wins over the configuration.
	cfg = DefaultConfig()
	cfg.Policy.External = &ExternalPolicyConfig{MCP: "cp"}
	a, err := New(append(base, WithConfig(cfg), WithPolicy(PolicyFunc(func(context.Context, ToolCall) Decision { return Decision{} })))...)
	if err != nil {
		t.Fatalf("options did not win: %v", err)
	}
	a.Close(context.Background())
}

func TestProbeExternal(t *testing.T) {
	a := externalAgent(t, newScript().Model(), NewMemBus(), nil)
	st := a.ProbeExternal(testCtx(t))
	if len(st) != 1 || st[0].Use != "policy" || st[0].Probed || st[0].Target != "the bus subject fw.policy" {
		t.Fatalf("status = %+v", st)
	}
}
