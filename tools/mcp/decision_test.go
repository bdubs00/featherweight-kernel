package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// controlPlane is an MCP server that decides like a control plane. It
// records every request that reaches it.
type controlPlane struct {
	mu     sync.Mutex
	authz  []fw.AuthzRequest
	policy []fw.PolicyRequest
}

func (c *controlPlane) authzRequests() []fw.AuthzRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]fw.AuthzRequest(nil), c.authz...)
}

func (c *controlPlane) policyRequests() []fw.PolicyRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]fw.PolicyRequest(nil), c.policy...)
}

// authzAnswer allows alice and refuses everyone else.
func (c *controlPlane) authzAnswer(raw json.RawMessage) (fw.AuthzAnswer, error) {
	var req fw.AuthzRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return fw.AuthzAnswer{}, err
	}
	c.mu.Lock()
	c.authz = append(c.authz, req)
	c.mu.Unlock()
	var ans fw.AuthzAnswer
	for range req.Checks {
		if req.Principal.Subject == "alice" {
			ans.Decisions = append(ans.Decisions, fw.AuthzDecision{Allow: true})
		} else {
			ans.Decisions = append(ans.Decisions, fw.AuthzDecision{Reason: req.Principal.Subject + " is not on the list"})
		}
	}
	return ans, nil
}

// server builds the MCP server with these tools:
//
//	authorize       answers in structuredContent
//	authorize_text  answers in a text block only
//	authorize_error returns isError
//	chatter         answers with text that holds no JSON object
//	ask             asks the client through an input_required result
//	decide          answers a policy request: deny rm, allow the rest
func (c *controlPlane) server() *sdk.Server {
	srv := sdk.NewServer(&sdk.Implementation{Name: "control-plane", Version: "v1"}, nil)
	obj := &sdk.Tool{InputSchema: json.RawMessage(`{"type":"object"}`)}
	tool := func(name string, h sdk.ToolHandler) {
		t := *obj
		t.Name = name
		srv.AddTool(&t, h)
	}
	text := func(s string) []sdk.Content { return []sdk.Content{&sdk.TextContent{Text: s}} }
	tool("authorize", func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		ans, err := c.authzAnswer(req.Params.Arguments)
		if err != nil {
			return nil, err
		}
		return &sdk.CallToolResult{Content: text("decided"), StructuredContent: ans}, nil
	})
	tool("authorize_text", func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		ans, err := c.authzAnswer(req.Params.Arguments)
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(ans)
		return &sdk.CallToolResult{Content: text("  " + string(b) + "\n")}, nil
	})
	tool("authorize_error", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{IsError: true, Content: text("the policy store is offline")}, nil
	})
	tool("chatter", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: text("sure, go ahead")}, nil
	})
	tool("ask", func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		if resp, ok := req.Params.InputResponses["confirm"]; ok {
			er, _ := resp.(*sdk.ElicitResult)
			action := "none"
			if er != nil {
				action = er.Action
			}
			return &sdk.CallToolResult{Content: text("action=" + action)}, nil
		}
		return &sdk.CallToolResult{
			InputRequests: sdk.InputRequestMap{"confirm": &sdk.ElicitParams{Mode: "form", Message: "Who are you?",
				RequestedSchema: map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}}}},
			RequestState: "state-1",
		}, nil
	})
	tool("decide", func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var p fw.PolicyRequest
		if err := json.Unmarshal(req.Params.Arguments, &p); err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.policy = append(c.policy, p)
		c.mu.Unlock()
		ans := fw.PolicyAnswer{Verdict: "allow"}
		if p.Tool.Name == "rm" {
			ans = fw.PolicyAnswer{Verdict: "deny", Reason: "no deletes in prod"}
		}
		return &sdk.CallToolResult{Content: text(ans.Verdict), StructuredContent: ans}, nil
	})
	return srv
}

var (
	alice = &fw.Principal{Subject: "alice", Kind: fw.KindUser}
	bob   = &fw.Principal{Subject: "bob", Kind: fw.KindUser}
)

// fakeModel ends every turn at once.
func fakeModel() fw.Model {
	return fw.ModelFunc{StreamFn: func(context.Context, *fw.Request, func(fw.Delta)) (*fw.Response, error) {
		return &fw.Response{Content: []fw.Block{fw.Text("done")}, StopReason: fw.StopEndTurn}, nil
	}}
}

// newDecisionAgent builds an agent with the MCP server cp at endpoint. The
// mutate function sets policy.external, and the options add the
// authorization over a decision point, as the workbench does.
func newDecisionAgent(t *testing.T, endpoint string, mutate func(*fw.Config), opts ...fw.Option) (*fw.Agent, error) {
	t.Helper()
	t.Setenv("FW_ATESPACE", "")
	cfg := fw.DefaultConfig()
	cfg.Tools.MCP = []fw.MCPServerConfig{{Name: "cp", Endpoint: endpoint, Timeout: 5 * time.Second}}
	if mutate != nil {
		mutate(cfg)
	}
	base := []fw.Option{
		fw.WithConfig(cfg),
		fw.WithoutDefaults(),
		fw.WithAuthn(fw.NoAuthn{}),
		fw.WithModel(fakeModel()),
		fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()),
		fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	}
	a, err := fw.New(append(base, opts...)...)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	return a, nil
}

func mustDecisionAgent(t *testing.T, endpoint string, mutate func(*fw.Config), opts ...fw.Option) *fw.Agent {
	t.Helper()
	a, err := newDecisionAgent(t, endpoint, mutate, opts...)
	if err != nil {
		t.Fatalf("fw.New: %v", err)
	}
	return a
}

// planeAuthz is the smallest authorization plug over a decision point, as
// the workbench builds one: it sends the checks of one call in one request
// and refuses when the control plane does not answer.
type planeAuthz struct {
	a  *fw.Agent
	dp fw.DecisionPoint
}

func (z *planeAuthz) DecidesLineage() {}

func (z *planeAuthz) Allow(ctx context.Context, p *fw.Principal, action string, t fw.Target) error {
	return z.AllowAll(ctx, p, []fw.AuthzCheck{{Action: action, Target: t}})[0]
}

func (z *planeAuthz) AllowAll(ctx context.Context, p *fw.Principal, checks []fw.AuthzCheck) []error {
	out := make([]error, len(checks))
	req := fw.AuthzRequest{Kind: fw.DecisionAuthz, Version: fw.DecisionVersion, Principal: *p}
	for _, c := range checks {
		agent := c.Target.Agent
		if agent == "" && c.Target.Run != nil {
			agent = c.Target.Run.Agent
		}
		req.Checks = append(req.Checks, fw.AuthzQuery{Action: c.Action, Agent: agent, Run: z.a.RunFacts(c.Target.Run)})
	}
	body, _ := json.Marshal(req)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	raw, err := z.dp.Decide(ctx, body)
	var ans fw.AuthzAnswer
	if err == nil {
		if json.Unmarshal(raw, &ans) != nil || len(ans.Decisions) != len(checks) {
			err = errors.New("a bad answer")
		}
	}
	for i := range out {
		switch {
		case err != nil:
			out[i] = fmt.Errorf("%w: %v", fw.ErrAuthzUnavailable, err)
		case !ans.Decisions[i].Allow:
			out[i] = fmt.Errorf("%w: %s", fw.ErrForbidden, ans.Decisions[i].Reason)
		}
	}
	return out
}

// controlPlaneAuthz hands the authorization to the tool of the server cp,
// through a decision point of the agent, as the workbench does.
func controlPlaneAuthz(tool string) fw.Option {
	return fw.WithAuthzFrom(func(a *fw.Agent) (fw.Authz, string, error) {
		dp, err := a.NewDecisionPoint(context.Background(), "workbench.access", fw.ExternalPolicyConfig{MCP: "cp", Tool: tool, Timeout: 2 * time.Second})
		if err != nil {
			return nil, "", err
		}
		return &planeAuthz{a: a, dp: dp}, fw.AccessModeExternal, nil
	})
}

func TestDecisionAuthz(t *testing.T) {
	ctx := context.Background()
	cp := &controlPlane{}
	ts := serveHTTP(t, cp.server(), nil)
	// The default tool of an authorization decision point is "authorize".
	a := mustDecisionAgent(t, ts.URL, nil, controlPlaneAuthz(""))

	r, err := a.Run(fw.WithPrincipal(ctx, alice), "hi")
	if err != nil || r.State != fw.StateCompleted {
		t.Fatalf("alice run = %+v, %v", r, err)
	}
	_, err = a.Start(fw.WithPrincipal(ctx, bob), "hi")
	if !errors.Is(err, fw.ErrForbidden) || !strings.Contains(err.Error(), "bob is not on the list") {
		t.Fatalf("bob = %v, want a refusal with the reason of the control plane", err)
	}
	reqs := cp.authzRequests()
	if len(reqs) < 2 || reqs[0].Kind != fw.DecisionAuthz || reqs[0].Principal.Subject != "alice" ||
		len(reqs[0].Checks) != 1 || reqs[0].Checks[0].Action != fw.ActionAgentRun {
		t.Fatalf("requests = %+v", reqs)
	}
}

func TestDecisionTextAnswer(t *testing.T) {
	ctx := context.Background()
	cp := &controlPlane{}
	ts := serveHTTP(t, cp.server(), nil)
	a := mustDecisionAgent(t, ts.URL, nil, controlPlaneAuthz("authorize_text"))
	if _, err := a.Start(fw.WithPrincipal(ctx, alice), "hi"); err != nil {
		t.Fatalf("alice = %v", err)
	}
	if _, err := a.Start(fw.WithPrincipal(ctx, bob), "hi"); !errors.Is(err, fw.ErrForbidden) {
		t.Fatalf("bob = %v", err)
	}
}

func TestDecisionUnavailable(t *testing.T) {
	ctx := fw.WithPrincipal(context.Background(), alice)
	cp := &controlPlane{}
	ts := serveHTTP(t, cp.server(), nil)

	for tool, want := range map[string]string{
		"authorize_error": "the policy store is offline",
		"chatter":         "no JSON object",
	} {
		a := mustDecisionAgent(t, ts.URL, nil, controlPlaneAuthz(tool))
		_, err := a.Start(ctx, "hi")
		if !errors.Is(err, fw.ErrAuthzUnavailable) || errors.Is(err, fw.ErrForbidden) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: Start = %v, want ErrAuthzUnavailable with %q", tool, err, want)
		}
	}

	// A server that stopped answers nothing, so every request is refused.
	gone := serveHTTP(t, cp.server(), nil)
	url := gone.URL
	gone.Close()
	a := mustDecisionAgent(t, url, nil, controlPlaneAuthz("authorize"))
	if _, err := a.Start(ctx, "hi"); !errors.Is(err, fw.ErrAuthzUnavailable) {
		t.Fatalf("stopped server: Start = %v, want ErrAuthzUnavailable", err)
	}
	// A call from the process itself needs no decision.
	if r, err := a.Run(context.Background(), "hi"); err != nil || r.State != fw.StateCompleted {
		t.Fatalf("internal run = %+v, %v", r, err)
	}
}

func TestDecisionNeverAsks(t *testing.T) {
	cp := &controlPlane{}
	ts := serveHTTP(t, cp.server(), nil)
	a := mustDecisionAgent(t, ts.URL, nil, controlPlaneAuthz("ask"))
	var asked atomic.Int32
	ctx := fw.WithAsker(fw.WithPrincipal(context.Background(), alice), func(ctx context.Context, _ string, _ []byte) ([]byte, error) {
		asked.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	start := time.Now()
	_, err := a.Start(ctx, "hi")
	// The client cancels the elicitation, and the answer holds no decision.
	if !errors.Is(err, fw.ErrAuthzUnavailable) || !strings.Contains(err.Error(), "action=cancel") {
		t.Fatalf("Start = %v, want ErrAuthzUnavailable after a canceled elicitation", err)
	}
	if asked.Load() != 0 || time.Since(start) > time.Second {
		t.Fatalf("the decision asked a person (%d) or waited %v", asked.Load(), time.Since(start))
	}
}

func TestDecisionPolicy(t *testing.T) {
	ctx := context.Background()
	cp := &controlPlane{}
	ts := serveHTTP(t, cp.server(), nil)
	// The default tool of policy.external is "decide".
	a := mustDecisionAgent(t, ts.URL, func(c *fw.Config) { c.Policy.External = &fw.ExternalPolicyConfig{MCP: "cp"} })

	d := a.Policy().Decide(ctx, fw.ToolCall{CallID: "c1", Spec: fw.ToolSpec{Name: "rm", Destructive: true}, Input: json.RawMessage(`{"path":"/"}`)})
	if d.Verdict != fw.VerdictDeny || !strings.Contains(d.Reason, "no deletes in prod") {
		t.Fatalf("rm = %+v", d)
	}
	if d := a.Policy().Decide(ctx, fw.ToolCall{CallID: "c2", Spec: fw.ToolSpec{Name: "read"}, Input: json.RawMessage(`{}`)}); d.Verdict != fw.VerdictAllow {
		t.Fatalf("read = %+v", d)
	}
	reqs := cp.policyRequests()
	if len(reqs) != 2 || reqs[0].Kind != fw.DecisionPolicy || reqs[0].Tool.Name != "rm" || !reqs[0].Tool.Destructive ||
		reqs[0].CallID != "c1" || string(reqs[0].Input) != `{"path":"/"}` {
		t.Fatalf("policy requests = %+v", reqs)
	}

	// A decide tool that fails denies the call.
	a = mustDecisionAgent(t, ts.URL, func(c *fw.Config) {
		c.Policy.External = &fw.ExternalPolicyConfig{MCP: "cp", Tool: "authorize_error"}
	})
	if d := a.Policy().Decide(ctx, fw.ToolCall{Spec: fw.ToolSpec{Name: "read", ReadOnly: true}}); d.Verdict != fw.VerdictDeny || !strings.Contains(d.Reason, "the policy store is offline") {
		t.Fatalf("failed decide = %+v", d)
	}
}

func TestDecisionProbe(t *testing.T) {
	ctx := context.Background()
	ts := serveHTTP(t, (&controlPlane{}).server(), nil)
	a := mustDecisionAgent(t, ts.URL, nil, controlPlaneAuthz("authorize"))
	st := a.ProbeExternal(ctx)
	if len(st) != 1 || st[0].Use != "workbench.access" || !st[0].Probed || st[0].Err != nil {
		t.Fatalf("status = %+v", st)
	}
	a = mustDecisionAgent(t, ts.URL, nil, controlPlaneAuthz("missing"))
	st = a.ProbeExternal(ctx)
	if len(st) != 1 || st[0].Err == nil || !strings.Contains(st[0].Err.Error(), `has no tool "missing"`) || !strings.Contains(st[0].Err.Error(), "authorize") {
		t.Fatalf("status = %+v", st)
	}
}

func TestDecisionServerIsNotAToolSource(t *testing.T) {
	cfg := fw.DefaultConfig()
	cfg.Tools.MCP = []fw.MCPServerConfig{
		{Name: "control-plane", Endpoint: "https://cp.example.com/mcp"},
		{Name: "git", Endpoint: "https://git.example.com/mcp"},
	}
	// The names match loosely, as in the validation of the configuration,
	// so a different spelling still hides the server from the model. A
	// linked package names its control server through the registry.
	fw.RegisterControlServers(func(c *fw.Config) []string {
		if c == cfg {
			return []string{"Control_Plane"}
		}
		return nil
	})
	cfg.Policy.External = &fw.ExternalPolicyConfig{MCP: "control-plane", Tool: "decide"}
	srcs, err := newSources(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 1 || srcs[0].(*Server).Name() != "git" {
		t.Fatalf("sources = %v, want only git", srcs)
	}
	_ = srcs[0].(*Server).Close()
}

func TestDecisionMissingEntry(t *testing.T) {
	_, err := newDecisionAgent(t, "https://cp.example.com/mcp", nil, fw.WithAuthzFrom(func(a *fw.Agent) (fw.Authz, string, error) {
		_, err := a.NewDecisionPoint(context.Background(), "workbench.access", fw.ExternalPolicyConfig{MCP: "nope"})
		return nil, "", err
	}))
	if err == nil || !strings.Contains(err.Error(), `names the server "nope"`) || !strings.Contains(err.Error(), "the entries are cp") ||
		!strings.Contains(err.Error(), "workbench.access.external") {
		t.Fatalf("fw.New = %v", err)
	}
}
