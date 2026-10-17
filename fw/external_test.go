package fw

import (
	"context"
	"encoding/json"
	"errors"
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

// externalAgent builds an agent whose access and policy go to the bus.
func externalAgent(t *testing.T, m Model, bus Bus, mutate func(*Config), opts ...Option) *Agent {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Access.External = &ExternalPolicyConfig{Bus: "fw.authz", Timeout: 2 * time.Second}
	if mutate != nil {
		mutate(cfg)
	}
	return newTestAgent(t, m, append([]Option{WithConfig(cfg), WithBus(bus)}, opts...)...)
}

var (
	alice = &Principal{Subject: "alice", Kind: KindUser, Groups: []string{"platform"}}
	bob   = &Principal{Subject: "bob", Kind: KindUser}
)

func TestExternalAuthzDecides(t *testing.T) {
	ctx := testCtx(t)
	bus := NewMemBus()
	cp := &controlPlane{decide: func(p Principal, q AuthzQuery) AuthzDecision {
		if p.Subject == "alice" {
			return AuthzDecision{Allow: true}
		}
		return AuthzDecision{Reason: "only the platform group runs agents"}
	}}
	cp.serve(t, bus, "fw.authz")
	a := externalAgent(t, newScript(endTurn("hi")).Model(), bus, func(c *Config) {
		// The local rules would refuse alice and allow bob. The control
		// plane is the only source of truth, so they do not apply.
		c.Access.Default = "closed"
		c.Access.Admins = []string{"bob"}
	})
	r, err := a.Run(WithPrincipal(ctx, alice), "hi")
	mustState(t, r, err, StateCompleted)
	_, err = a.Start(WithPrincipal(ctx, bob), "hi")
	if !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "only the platform group runs agents") {
		t.Fatalf("bob = %v", err)
	}
	reqs := cp.authzRequests()
	if len(reqs) < 2 || reqs[0].Kind != DecisionAuthz || reqs[0].Version != DecisionVersion || reqs[0].Principal.Subject != "alice" ||
		len(reqs[0].Checks) != 1 || reqs[0].Checks[0].Action != ActionAgentRun || reqs[0].Checks[0].Agent != "default" {
		t.Fatalf("requests = %+v", reqs)
	}
	// A call from the process itself carries no principal and never asks.
	n := len(cp.authzRequests())
	if _, err := a.List(ctx); err != nil || len(cp.authzRequests()) != n {
		t.Fatalf("an internal call asked the control plane: %v", err)
	}
}

func TestExternalAuthzBatchesAndSendsLineage(t *testing.T) {
	ctx := testCtx(t)
	bus := NewMemBus()
	cp := &controlPlane{decide: func(p Principal, q AuthzQuery) AuthzDecision {
		// The control plane implements the tree rule itself: the one who
		// started the root run can view every run under it.
		if q.Run == nil {
			return AuthzDecision{Allow: true}
		}
		owner := q.Run.Principal
		if n := len(q.Run.Lineage); n > 0 {
			owner = q.Run.Lineage[n-1].Principal
		}
		return AuthzDecision{Allow: owner == p.Subject}
	}}
	cp.serve(t, bus, "fw.authz")
	a := externalAgent(t, newScript(endTurn("p"), endTurn("c"), endTurn("x")).Model(), bus, nil)
	parent, err := a.Run(WithPrincipal(ctx, alice), "parent")
	mustState(t, parent, err, StateCompleted)
	child, err := a.Run(ctx, "child", WithParent(parent.ID))
	mustState(t, child, err, StateCompleted)
	other, err := a.Run(ctx, "other", WithPrincipalName("bob"))
	mustState(t, other, err, StateCompleted)

	before := len(cp.authzRequests())
	runs, err := a.List(WithPrincipal(ctx, alice))
	if err != nil {
		t.Fatal(err)
	}
	reqs := cp.authzRequests()[before:]
	if len(reqs) != 1 || len(reqs[0].Checks) != 3 {
		t.Fatalf("listing sent %d requests: %+v", len(reqs), reqs)
	}
	if len(runs) != 2 {
		t.Fatalf("alice sees %d runs, want the parent and the child", len(runs))
	}
	for _, q := range reqs[0].Checks {
		if q.Run.ID == child.ID && (len(q.Run.Lineage) != 1 || q.Run.Lineage[0].ID != parent.ID || q.Run.Lineage[0].Principal != "alice") {
			t.Fatalf("child facts = %+v", q.Run)
		}
	}
	// A refusal is final: fw does not ask again for the ancestors.
	before = len(cp.authzRequests())
	if _, err := a.Get(WithPrincipal(ctx, bob), child.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("bob reads the child: %v", err)
	}
	if got := len(cp.authzRequests()) - before; got != 1 {
		t.Fatalf("one refusal sent %d requests", got)
	}
}

func TestExternalAuthzCache(t *testing.T) {
	ctx := WithPrincipal(testCtx(t), alice)
	bus := NewMemBus()
	var ttl atomic.Int64
	ttl.Store(60_000)
	cp := &controlPlane{decide: func(Principal, AuthzQuery) AuthzDecision {
		return AuthzDecision{Allow: true, TTLMs: ttl.Load()}
	}}
	cp.serve(t, bus, "fw.authz")
	clock := NewManualClock(t0)
	a := externalAgent(t, newScript().Model(), bus, nil, WithClock(clock), WithProfile(AgentProfile{Name: "reviewer"}))
	for i := 0; i < 3; i++ {
		_ = a.ProfilesFor(ctx)
	}
	if n := len(cp.authzRequests()); n != 1 {
		t.Fatalf("cached decision sent %d requests", n)
	}
	clock.Advance(2 * time.Minute)
	ttl.Store(0)
	_ = a.ProfilesFor(ctx)
	_ = a.ProfilesFor(ctx)
	if n := len(cp.authzRequests()); n != 3 {
		t.Fatalf("expired and uncached decisions sent %d requests, want 3", n)
	}
}

func TestExternalAuthzUnavailable(t *testing.T) {
	ctx := testCtx(t)
	a := externalAgent(t, newScript(endTurn("internal")).Model(), NewMemBus(), func(c *Config) {
		c.Access.External.Timeout = 30 * time.Millisecond
	}, WithProfile(AgentProfile{Name: "reviewer"}))
	_, err := a.Start(WithPrincipal(ctx, alice), "hi")
	if !errors.Is(err, ErrAuthzUnavailable) || errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "fw.authz") {
		t.Fatalf("Start without a control plane = %v", err)
	}
	// The process itself keeps working while the control plane is down.
	r, err := a.Run(ctx, "hi")
	mustState(t, r, err, StateCompleted)
	// A listing reports the outage instead of an empty list.
	if _, err := a.List(WithPrincipal(ctx, alice)); !errors.Is(err, ErrAuthzUnavailable) {
		t.Fatalf("List = %v", err)
	}
	if _, err := a.Children(WithPrincipal(ctx, alice), r.ID); !errors.Is(err, ErrAuthzUnavailable) {
		t.Fatalf("Children = %v", err)
	}
	if _, err := a.ListProfiles(WithPrincipal(ctx, alice)); !errors.Is(err, ErrAuthzUnavailable) {
		t.Fatalf("ListProfiles = %v", err)
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
	cfg.Access.External = &ExternalPolicyConfig{MCP: "cp", Bus: "fw.authz"}
	if _, err := New(append(base, WithConfig(cfg))...); err == nil || !strings.Contains(err.Error(), "exactly one of mcp and bus") {
		t.Fatalf("both transports = %v", err)
	}
	cfg = DefaultConfig()
	cfg.Policy.External = &ExternalPolicyConfig{MCP: "cp"}
	if _, err := New(append(base, WithConfig(cfg))...); err == nil || !strings.Contains(err.Error(), "tools/mcp") {
		t.Fatalf("mcp without the driver = %v", err)
	}
	cfg = DefaultConfig()
	cfg.Access.External = &ExternalPolicyConfig{MCP: "cp"}
	cfg.Policy.External = &ExternalPolicyConfig{MCP: "cp2", Tool: "check"}
	if got := cfg.ControlServers(); len(got) != 2 || !got["cp"] || !got["cp2"] {
		t.Fatalf("control servers = %v", got)
	}
	// An option wins over the configuration.
	cfg = DefaultConfig()
	cfg.Access.External = &ExternalPolicyConfig{MCP: "cp"}
	cfg.Policy.External = &ExternalPolicyConfig{MCP: "cp"}
	a, err := New(append(base, WithConfig(cfg), WithAuthz(AuthzFunc(func(context.Context, *Principal, string, Target) error { return nil })),
		WithPolicy(PolicyFunc(func(context.Context, ToolCall) Decision { return Decision{} })))...)
	if err != nil {
		t.Fatalf("options did not win: %v", err)
	}
	a.Close(context.Background())
}

func TestProbeExternal(t *testing.T) {
	a := externalAgent(t, newScript().Model(), NewMemBus(), nil)
	st := a.ProbeExternal(testCtx(t))
	if len(st) != 1 || st[0].Use != "access" || st[0].Probed || st[0].Target != "the bus subject fw.authz" {
		t.Fatalf("status = %+v", st)
	}
}
