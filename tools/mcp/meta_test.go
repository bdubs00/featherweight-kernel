package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// metaServer is an MCP server with one tool, whoami, that answers with the
// keys of _meta of the call that start with io.featherweight/, as JSON.
func metaServer() *sdk.Server {
	srv := sdk.NewServer(&sdk.Implementation{Name: "front", Version: "v1"}, nil)
	sdk.AddTool(srv, &sdk.Tool{Name: "whoami", Description: "Names the caller", Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true}},
		func(_ context.Context, req *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
			keys := map[string]any{}
			for k, v := range req.Params.Meta {
				if strings.HasPrefix(k, "io.featherweight/") {
					keys[k] = v
				}
			}
			b, _ := json.Marshal(keys)
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: string(b)}}}, nil, nil
		})
	return srv
}

// callerKeys calls whoami and returns the keys that the server read.
func callerKeys(t *testing.T, s *Server, ctx context.Context) map[string]string {
	t.Helper()
	res, err := s.Call(ctx, "whoami", nil)
	if err != nil || res.IsError {
		t.Fatalf("Call whoami = %+v, %v", res, err)
	}
	var keys map[string]string
	if err := json.Unmarshal([]byte(fw.TextOf(res.Content)), &keys); err != nil {
		t.Fatalf("answer %q: %v", fw.TextOf(res.Content), err)
	}
	return keys
}

// TestCallCarriesTheRun checks the _meta keys of a tools/call: the run,
// the agent, and the node for every server, the principal and its kind
// only for a trusted one, and nothing for a call without a run.
func TestCallCarriesTheRun(t *testing.T) {
	ts := serveHTTP(t, metaServer(), nil)
	plain := newServer(t, fw.MCPServerConfig{Name: "front", Endpoint: ts.URL}, nil)
	trusted := newServer(t, fw.MCPServerConfig{Name: "front", Endpoint: ts.URL, Trusted: true}, nil)
	a := tokenAgent(t, nil)
	if a.Node() == "" {
		t.Fatal("the agent has no node")
	}
	run := fw.RunInfo{RunID: "01RUN", Agent: "manager", Principal: "alice", PrincipalKind: fw.KindUser}
	ctx := fw.WithRunInfo(fw.WithAgent(context.Background(), a), run)

	if got := callerKeys(t, plain, context.Background()); len(got) != 0 {
		t.Fatalf("a call without a run sent %v", got)
	}
	want := map[string]string{fw.MCPMetaRun: "01RUN", fw.MCPMetaAgent: "manager", fw.MCPMetaNode: a.Node()}
	if got := callerKeys(t, plain, ctx); !equalKeys(got, want) {
		t.Fatalf("a server that is not trusted read %v, want %v", got, want)
	}
	want[fw.MCPMetaPrincipal], want[fw.MCPMetaPrincipalKind] = "alice", fw.KindUser
	if got := callerKeys(t, trusted, ctx); !equalKeys(got, want) {
		t.Fatalf("a trusted server read %v, want %v", got, want)
	}
	// A run without a principal sends no principal keys, also to a trusted
	// server.
	alone := fw.WithRunInfo(fw.WithAgent(context.Background(), a), fw.RunInfo{RunID: "01RUN", Agent: "manager"})
	if got := callerKeys(t, trusted, alone); got[fw.MCPMetaPrincipal] != "" || got[fw.MCPMetaPrincipalKind] != "" || got[fw.MCPMetaRun] != "01RUN" {
		t.Fatalf("a run without a principal sent %v", got)
	}
}

func equalKeys(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestRunCallCarriesItsRun checks the keys through the turn loop: the
// tool call of a run sends the id of that run and the name of its agent.
func TestRunCallCarriesItsRun(t *testing.T) {
	ctx := context.Background()
	ts := serveHTTP(t, metaServer(), nil)
	s := newServer(t, fw.MCPServerConfig{Name: "front", Endpoint: ts.URL, Trusted: true}, nil)
	tools, err := s.Tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	model := fw.ModelFunc{StreamFn: func(context.Context, *fw.Request, func(fw.Delta)) (*fw.Response, error) {
		calls++
		if calls == 1 {
			return &fw.Response{StopReason: fw.StopToolUse, Content: []fw.Block{{Type: fw.BlockToolUse, ID: "c1", Name: "front.whoami", Input: json.RawMessage(`{}`)}}}, nil
		}
		return &fw.Response{StopReason: fw.StopEndTurn, Content: []fw.Block{fw.Text("done")}}, nil
	}}
	a, err := fw.New(fw.WithoutDefaults(), fw.WithAuthn(fw.NoAuthn{}), fw.WithModel(model), fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()), fw.WithTools(tools...), fw.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(context.Background()) })
	alice := fw.WithPrincipal(ctx, &fw.Principal{Subject: "alice", Kind: fw.KindUser})
	r, err := a.Run(alice, "who am I")
	if err != nil || r.State != fw.StateCompleted {
		t.Fatalf("run = %+v, %v", r, err)
	}
	var answer string
	for e, err := range a.Ledger().Read(ctx, r.ID, 0) {
		var d fw.ToolRespondedData
		if err == nil && e.Type == fw.EvToolResponded && e.Decode(&d) == nil {
			answer = fw.TextOf(d.Content)
		}
	}
	var keys map[string]string
	if err := json.Unmarshal([]byte(answer), &keys); err != nil {
		t.Fatalf("answer %q: %v", answer, err)
	}
	want := map[string]string{fw.MCPMetaRun: r.ID, fw.MCPMetaAgent: r.Agent, fw.MCPMetaNode: a.Node(), fw.MCPMetaPrincipal: "alice", fw.MCPMetaPrincipalKind: fw.KindUser}
	if !equalKeys(keys, want) {
		t.Fatalf("the server read %v, want %v", keys, want)
	}
}
