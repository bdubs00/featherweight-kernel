package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// stdioEnv tells the re-executed test binary to run as a stdio MCP server.
const stdioEnv = "FW_MCP_TEST_STDIO_SERVER"

func TestMain(m *testing.M) {
	if os.Getenv(stdioEnv) == "1" {
		srv := newTestMCPServer(nil)
		if err := srv.Run(context.Background(), &sdk.StdioTransport{}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type echoIn struct {
	Msg string `json:"msg" jsonschema:"the text to echo"`
}

type echoOut struct {
	Msg string `json:"msg"`
}

func boolPtr(b bool) *bool { return &b }

// newTestMCPServer builds an MCP server with these tools:
//
//	echo   read-only; returns text and structured content
//	count  idempotent, not destructive; reports progress three times
//	fail   no annotations; returns isError
//	ask    asks the client through an input_required result
//	media  returns an image, a resource link, and an embedded resource
func newTestMCPServer(opts *sdk.ServerOptions) *sdk.Server {
	srv := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "v1"}, opts)
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "echo",
		Description: "Echo a message",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, func(_ context.Context, _ *sdk.CallToolRequest, in echoIn) (*sdk.CallToolResult, echoOut, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "echo: " + in.Msg}}}, echoOut{Msg: in.Msg}, nil
	})
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "count",
		Description: "Count to three",
		Annotations: &sdk.ToolAnnotations{IdempotentHint: true, DestructiveHint: boolPtr(false)},
	}, func(ctx context.Context, req *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
		tok := req.Params.GetProgressToken()
		for i := 1; i <= 3; i++ {
			if tok != nil {
				_ = req.Session.NotifyProgress(ctx, &sdk.ProgressNotificationParams{
					ProgressToken: tok, Progress: float64(i), Total: 3, Message: fmt.Sprintf("step %d", i),
				})
			}
		}
		// Give the client time to handle the notifications before the result.
		time.Sleep(100 * time.Millisecond)
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "counted 3"}}}, nil, nil
	})
	sdk.AddTool(srv, &sdk.Tool{Name: "fail", Description: "Always fails"},
		func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: "boom"}}}, nil, nil
		})
	sdk.AddTool(srv, &sdk.Tool{Name: "ask", Description: "Asks for confirmation"},
		func(_ context.Context, req *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
			resp, ok := req.Params.InputResponses["confirm"]
			if !ok {
				return &sdk.CallToolResult{
					InputRequests: sdk.InputRequestMap{"confirm": &sdk.ElicitParams{
						Mode:    "form",
						Message: "Proceed?",
						RequestedSchema: map[string]any{
							"type":       "object",
							"properties": map[string]any{"ok": map[string]any{"type": "boolean"}},
							"required":   []any{"ok"},
						},
					}},
					RequestState: "state-1",
				}, nil, nil
			}
			er, _ := resp.(*sdk.ElicitResult)
			if er == nil {
				return nil, nil, errors.New("unexpected input response")
			}
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{
				Text: fmt.Sprintf("action=%s ok=%v state=%s", er.Action, er.Content["ok"], req.Params.RequestState),
			}}}, nil, nil
		})
	sdk.AddTool(srv, &sdk.Tool{Name: "media", Description: "Returns media"},
		func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{
				&sdk.ImageContent{MIMEType: "image/png", Data: []byte{0x89, 'P', 'N', 'G'}},
				&sdk.ResourceLink{URI: "file:///report.txt", Name: "report"},
				&sdk.EmbeddedResource{Resource: &sdk.ResourceContents{URI: "file:///note.txt", MIMEType: "text/plain", Text: "note body"}},
			}}, nil, nil
		})
	return srv
}

// headerLog records request headers that reach the MCP server.
type headerLog struct {
	mu   sync.Mutex
	seen []http.Header
}

func (h *headerLog) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.seen = append(h.seen, r.Header.Clone())
		h.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

func (h *headerLog) all() []http.Header {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]http.Header(nil), h.seen...)
}

// serveHandler returns the Streamable HTTP handler for an MCP server.
func serveHandler(srv *sdk.Server) http.Handler {
	return sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, nil)
}

// serveHTTP serves an MCP server over Streamable HTTP on a test listener.
func serveHTTP(t *testing.T, srv *sdk.Server, wrap func(http.Handler) http.Handler) *httptest.Server {
	t.Helper()
	h := serveHandler(srv)
	if wrap != nil {
		h = wrap(h)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

func mapSecrets(m map[string]string) fw.Secrets {
	return fw.SecretsFunc(func(_ context.Context, name string) (string, error) {
		if v, ok := m[name]; ok {
			return v, nil
		}
		return "", fmt.Errorf("%w: %s", fw.ErrSecretNotFound, name)
	})
}

func newServer(t *testing.T, cfg fw.MCPServerConfig, secrets fw.Secrets) *Server {
	t.Helper()
	s, err := New(context.Background(), cfg, secrets, slog.New(slog.NewTextHandler(testWriter{t}, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// testWriter sends log output to the test log.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Helper()
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func toolNames(ts []fw.Tool) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Spec().Name
	}
	return out
}

func findTool(t *testing.T, ts []fw.Tool, name string) fw.Tool {
	t.Helper()
	for _, x := range ts {
		if x.Spec().Name == name {
			return x
		}
	}
	t.Fatalf("tool %s not found in %v", name, toolNames(ts))
	return nil
}

func TestToolsAndCall(t *testing.T) {
	ctx := context.Background()
	hl := &headerLog{}
	// PageSize 2 makes the client follow nextCursor.
	ts := serveHTTP(t, newTestMCPServer(&sdk.ServerOptions{PageSize: 2}), hl.wrap)
	s := newServer(t, fw.MCPServerConfig{
		Name:     "test",
		Endpoint: ts.URL,
		Auth:     fw.MCPAuthConfig{Bearer: "TEST_MCP_TOKEN"},
		Headers:  map[string]string{"X-Team": "TEAM_NAME", "X-Plain": "literal value", "X-Missing": "NOT_A_SECRET"},
		Trusted:  true,
	}, mapSecrets(map[string]string{"TEST_MCP_TOKEN": "s3cret", "TEAM_NAME": "blue"}))

	tools, err := s.Tools(ctx)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	want := []string{"test.ask", "test.count", "test.echo", "test.fail", "test.media"}
	if got := toolNames(tools); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("tool names = %v, want %v", got, want)
	}

	echo := findTool(t, tools, "test.echo").Spec()
	if !echo.ReadOnly || !echo.ParallelSafe || echo.Destructive || echo.Idempotent {
		t.Errorf("echo flags = %+v, want read-only, parallel-safe, not destructive", echo)
	}
	if echo.Description != "Echo a message" || echo.Source != "test" || !echo.Trusted {
		t.Errorf("echo spec = %+v", echo)
	}
	if !strings.Contains(string(echo.InputSchema), `"msg"`) {
		t.Errorf("echo schema = %s, want the msg property", echo.InputSchema)
	}
	count := findTool(t, tools, "test.count").Spec()
	if count.ReadOnly || !count.Idempotent || count.Destructive || count.ParallelSafe {
		t.Errorf("count flags = %+v, want idempotent and not destructive", count)
	}
	fail := findTool(t, tools, "test.fail").Spec()
	if fail.ReadOnly || fail.Idempotent || !fail.Destructive {
		t.Errorf("fail flags = %+v, want destructive by default", fail)
	}

	// A text result with structured content, through the fw.Tool.
	res, err := findTool(t, tools, "test.echo").Call(ctx, json.RawMessage(`{"msg":"hi"}`))
	if err != nil {
		t.Fatalf("Call echo: %v", err)
	}
	if res.IsError || fw.TextOf(res.Content) != "echo: hi" {
		t.Errorf("echo result = %+v", res)
	}
	if string(res.Structured) != `{"msg":"hi"}` {
		t.Errorf("echo structured = %s", res.Structured)
	}

	// isError.
	res, err = s.Call(ctx, "fail", nil)
	if err != nil {
		t.Fatalf("Call fail: %v", err)
	}
	if !res.IsError || fw.TextOf(res.Content) != "boom" {
		t.Errorf("fail result = %+v, want isError with boom", res)
	}

	// Image, resource link, and embedded resource.
	res, err = s.Call(ctx, "media", nil)
	if err != nil {
		t.Fatalf("Call media: %v", err)
	}
	if len(res.Content) != 3 {
		t.Fatalf("media content = %+v, want 3 blocks", res.Content)
	}
	if b := res.Content[0]; b.Type != fw.BlockImage || b.MediaType != "image/png" || !bytes.Equal(b.Data, []byte{0x89, 'P', 'N', 'G'}) {
		t.Errorf("image block = %+v", b)
	}
	if b := res.Content[1]; b.Type != fw.BlockText || !strings.Contains(b.Text, "file:///report.txt") {
		t.Errorf("resource link block = %+v", b)
	}
	if b := res.Content[2]; b.Type != fw.BlockText || b.Text != "note body" {
		t.Errorf("embedded resource block = %+v", b)
	}

	// The bearer token and the headers reach the server.
	seen := hl.all()
	if len(seen) == 0 {
		t.Fatal("no request reached the server")
	}
	for _, h := range seen {
		if got := h.Get("Authorization"); got != "Bearer s3cret" {
			t.Fatalf("Authorization = %q, want the bearer token", got)
		}
		if h.Get("X-Team") != "blue" || h.Get("X-Plain") != "literal value" || h.Get("X-Missing") != "NOT_A_SECRET" {
			t.Fatalf("headers = %v", h)
		}
	}
	if !s.Up() {
		t.Error("Up() = false, want true")
	}
}

func TestProgressForwarding(t *testing.T) {
	ts := serveHTTP(t, newTestMCPServer(nil), nil)
	s := newServer(t, fw.MCPServerConfig{Name: "test", Endpoint: ts.URL}, nil)

	type report struct {
		progress, total float64
		msg             string
	}
	var mu sync.Mutex
	var got []report
	ctx := fw.WithProgress(context.Background(), func(p, total float64, msg string) {
		mu.Lock()
		got = append(got, report{p, total, msg})
		mu.Unlock()
	})
	res, err := s.Call(ctx, "count", json.RawMessage(`{}`))
	if err != nil || res.IsError {
		t.Fatalf("Call count = %+v, %v", res, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("progress reports = %+v, want 3", got)
	}
	for i, r := range got {
		if r.progress != float64(i+1) || r.total != 3 || r.msg != fmt.Sprintf("step %d", i+1) {
			t.Errorf("report %d = %+v", i, r)
		}
	}
}

func TestElicitation(t *testing.T) {
	ts := serveHTTP(t, newTestMCPServer(nil), nil)
	s := newServer(t, fw.MCPServerConfig{Name: "test", Endpoint: ts.URL, Timeout: time.Second}, nil)

	var question string
	var schema []byte
	ctx := fw.WithAsker(context.Background(), func(_ context.Context, q string, sc []byte) ([]byte, error) {
		question, schema = q, sc
		// A slow answer: the call timer stops while fw waits for the caller.
		time.Sleep(1500 * time.Millisecond)
		return []byte(`{"ok":true}`), nil
	})
	res, err := s.Call(ctx, "ask", nil)
	if err != nil {
		t.Fatalf("Call ask: %v", err)
	}
	if res.IsError || fw.TextOf(res.Content) != "action=accept ok=true state=state-1" {
		t.Errorf("ask result = %+v", res)
	}
	if question != "Proceed?" || !strings.Contains(string(schema), `"ok"`) {
		t.Errorf("asked %q with schema %s", question, schema)
	}

	// Without an asker the client cancels the elicitation.
	res, err = s.Call(context.Background(), "ask", nil)
	if err != nil {
		t.Fatalf("Call ask without asker: %v", err)
	}
	if !strings.HasPrefix(fw.TextOf(res.Content), "action=cancel") {
		t.Errorf("ask without asker = %+v, want action=cancel", res)
	}
}

func TestListChanged(t *testing.T) {
	ctx := context.Background()
	srv := newTestMCPServer(nil)
	ts := serveHTTP(t, srv, nil)
	s := newServer(t, fw.MCPServerConfig{Name: "test", Endpoint: ts.URL}, nil)
	if _, err := s.Tools(ctx); err != nil {
		t.Fatalf("Tools: %v", err)
	}
	srv.AddTool(&sdk.Tool{Name: "aaa", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "a"}}}, nil
		})
	srv.RemoveTools("fail")
	want := "test.ask,test.count,test.echo,test.media,test.aaa"
	deadline := time.Now().Add(5 * time.Second)
	for {
		tools, err := s.Tools(ctx)
		if err != nil {
			t.Fatalf("Tools: %v", err)
		}
		got := strings.Join(toolNames(tools), ",")
		if got == want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tool names = %s, want %s (new tools appended, old order kept)", got, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestUnknownToolRefreshes(t *testing.T) {
	ctx := context.Background()
	// No listChanged capability, so only the unknown-tool error refreshes.
	srv := newTestMCPServer(&sdk.ServerOptions{Capabilities: &sdk.ServerCapabilities{Tools: &sdk.ToolCapabilities{}}})
	ts := serveHTTP(t, srv, nil)
	s := newServer(t, fw.MCPServerConfig{Name: "test", Endpoint: ts.URL}, nil)
	if _, err := s.Tools(ctx); err != nil {
		t.Fatalf("Tools: %v", err)
	}
	srv.AddTool(&sdk.Tool{Name: "late", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{}}, nil
		})
	res, err := s.Call(ctx, "missing", nil)
	if err != nil {
		t.Fatalf("Call missing: %v", err)
	}
	if !res.IsError || !strings.Contains(fw.TextOf(res.Content), "unknown tool") {
		t.Errorf("missing tool result = %+v", res)
	}
	tools, err := s.Tools(ctx)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if names := toolNames(tools); names[len(names)-1] != "test.late" {
		t.Errorf("tool names after refresh = %v, want test.late appended", names)
	}
	if !s.Up() {
		t.Error("a protocol error marked the server down")
	}
}

func TestCallTimeout(t *testing.T) {
	// The server never cancels the context of a call, so the handler also
	// returns when the test ends. Otherwise the test server waits forever
	// for the hanging request when it closes.
	over := make(chan struct{})
	srv := newTestMCPServer(nil)
	srv.AddTool(&sdk.Tool{Name: "hang", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, _ *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-over:
				return nil, errors.New("the test is over")
			}
		})
	ts := serveHTTP(t, srv, nil)
	t.Cleanup(func() { close(over) })
	s := newServer(t, fw.MCPServerConfig{Name: "test", Endpoint: ts.URL, Timeout: 300 * time.Millisecond}, nil)
	start := time.Now()
	res, err := s.Call(context.Background(), "hang", nil)
	if err != nil {
		t.Fatalf("Call hang: %v", err)
	}
	if !res.IsError || !strings.Contains(fw.TextOf(res.Content), "did not answer") {
		t.Errorf("hang result = %+v", res)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("call took %s, want about 300ms", d)
	}

	// A canceled caller gets an error, not a result.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Call(ctx, "hang", nil); !errors.Is(err, context.Canceled) {
		t.Errorf("Call with canceled context error = %v, want context.Canceled", err)
	}
}

func TestCircuitBreaker(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	s := newServer(t, fw.MCPServerConfig{Name: "gone", Endpoint: "http://" + addr + "/mcp"}, nil)
	s.rt.retryBase = time.Millisecond
	ctx := context.Background()
	for i := 1; i <= failureLimit; i++ {
		if !s.Up() {
			t.Fatalf("server down after %d failures, want %d", i-1, failureLimit)
		}
		res, err := s.Call(ctx, "anything", nil)
		if err != nil {
			t.Fatalf("Call %d: %v", i, err)
		}
		if !res.IsError || !strings.Contains(fw.TextOf(res.Content), "gone") {
			t.Fatalf("Call %d result = %+v, want an error result naming the server", i, res)
		}
	}
	if s.Up() {
		t.Fatal("Up() = true after five failures")
	}
	start := time.Now()
	res, err := s.Call(ctx, "anything", nil)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Errorf("call while down took %s, want an immediate answer", d)
	}
	text := fw.TextOf(res.Content)
	if !res.IsError || !strings.Contains(text, "gone") || !strings.Contains(text, "down") {
		t.Errorf("call while down = %+v", res)
	}
	if _, err := s.Tools(ctx); err == nil || !strings.Contains(err.Error(), "down") {
		t.Errorf("Tools while down with no cache: error = %v", err)
	}

	// After the down time the server is up again.
	later := time.Now().Add(downTime + time.Second)
	s.mu.Lock()
	s.now = func() time.Time { return later }
	s.mu.Unlock()
	if !s.Up() {
		t.Error("Up() = false after the down time")
	}
}

func TestBreakerKeepsCachedTools(t *testing.T) {
	ctx := context.Background()
	ts := serveHTTP(t, newTestMCPServer(nil), nil)
	s := newServer(t, fw.MCPServerConfig{Name: "test", Endpoint: ts.URL}, nil)
	before, err := s.Tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for range failureLimit {
		s.failed("test", errors.New("injected"))
	}
	s.listChanged(ctx, nil)
	after, err := s.Tools(ctx)
	if err != nil {
		t.Fatalf("Tools while down: %v", err)
	}
	if strings.Join(toolNames(after), ",") != strings.Join(toolNames(before), ",") {
		t.Errorf("Tools while down = %v, want the cached list", toolNames(after))
	}
	res, _ := s.Call(ctx, "echo", json.RawMessage(`{"msg":"x"}`))
	if !res.IsError || !strings.Contains(fw.TextOf(res.Content), "test") {
		t.Errorf("Call while down = %+v", res)
	}
}

func TestStdio(t *testing.T) {
	t.Setenv(stdioEnv, "1")
	s := newServer(t, fw.MCPServerConfig{
		Name:    "local",
		Command: os.Args[0],
		Args:    []string{"-test.run=^$"},
	}, nil)
	ctx := context.Background()
	tools, err := s.Tools(ctx)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(tools) != 5 || tools[0].Spec().Name != "local.ask" {
		t.Fatalf("tools = %v", toolNames(tools))
	}
	res, err := findTool(t, tools, "local.echo").Call(ctx, json.RawMessage(`{"msg":"over stdio"}`))
	if err != nil || fw.TextOf(res.Content) != "echo: over stdio" {
		t.Fatalf("Call echo = %+v, %v", res, err)
	}
}

func TestConfigErrors(t *testing.T) {
	cases := []struct {
		name string
		cfg  fw.MCPServerConfig
		want string
	}{
		{"no name", fw.MCPServerConfig{Endpoint: "https://x"}, "name is required"},
		{"no transport", fw.MCPServerConfig{Name: "a"}, "set endpoint or command"},
		{"both transports", fw.MCPServerConfig{Name: "a", Endpoint: "https://x", Command: "srv"}, "not both"},
		{"bad scheme", fw.MCPServerConfig{Name: "a", Endpoint: "ftp://x"}, "not http or https"},
		{"no host", fw.MCPServerConfig{Name: "a", Endpoint: "https:///mcp"}, "no host"},
		{"negative timeout", fw.MCPServerConfig{Name: "a", Endpoint: "https://x", Timeout: -1}, "negative"},
		{"cert without key", fw.MCPServerConfig{Name: "a", Endpoint: "https://x", TLS: fw.TLSConfig{Cert: "c.pem"}}, "tls.cert and tls.key"},
		{"missing ca", fw.MCPServerConfig{Name: "a", Endpoint: "https://x", TLS: fw.TLSConfig{CA: "/no/such/ca.pem"}}, "tls.ca"},
		{"bearer and oauth", fw.MCPServerConfig{Name: "a", Endpoint: "https://x", Auth: fw.MCPAuthConfig{
			Bearer: "T", OAuth: &fw.OAuthClientConfig{ClientID: "c", ClientSecret: "S"}}}, "not both"},
		{"oauth without secret", fw.MCPServerConfig{Name: "a", Endpoint: "https://x", Auth: fw.MCPAuthConfig{
			OAuth: &fw.OAuthClientConfig{ClientID: "c"}}}, "clientSecret"},
		{"plain http token url", fw.MCPServerConfig{Name: "a", Endpoint: "https://x", Auth: fw.MCPAuthConfig{
			OAuth: &fw.OAuthClientConfig{ClientID: "c", ClientSecret: "S", TokenURL: "http://auth.example.com/token"}}}, "https"},
		{"auth on command", fw.MCPServerConfig{Name: "a", Command: "srv", Auth: fw.MCPAuthConfig{Bearer: "T"}}, "endpoint only"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := New(context.Background(), c.cfg, nil, slog.New(slog.DiscardHandler))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("New error = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

func TestRegisteredSource(t *testing.T) {
	ctx := context.Background()
	cfg := fw.DefaultConfig()
	cfg.Tools.MCP = []fw.MCPServerConfig{
		{Name: "up", Endpoint: "https://mcp.example.com/mcp"},
		// A server that is down does not fail the constructor.
		{Name: "down", Endpoint: "http://127.0.0.1:1/mcp"},
	}
	srcs, err := newSources(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("newSources: %v", err)
	}
	if len(srcs) != 2 {
		t.Fatalf("sources = %d, want 2", len(srcs))
	}
	for _, src := range srcs {
		_ = src.(*Server).Close()
	}

	cfg.Tools.MCP = append(cfg.Tools.MCP, fw.MCPServerConfig{Name: "broken", Endpoint: "ftp://x"})
	if _, err := newSources(ctx, cfg, nil); err == nil || !strings.Contains(err.Error(), `"broken"`) {
		t.Errorf("newSources error = %v, want it to name the entry", err)
	}
	cfg.Tools.MCP = []fw.MCPServerConfig{{Name: "a", Command: "x"}, {Name: "a", Command: "y"}}
	if _, err := newSources(ctx, cfg, nil); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("newSources error = %v, want a duplicate name error", err)
	}
}

func TestPlainHTTPWarning(t *testing.T) {
	cases := map[string]bool{
		"http://mcp.example.com/mcp":             true,
		"https://mcp.example.com/mcp":            false,
		"http://localhost:8080/mcp":              false,
		"http://127.0.0.1:8080/mcp":              false,
		"http://[::1]:8080/mcp":                  false,
		"http://tools.ns.svc/mcp":                false,
		"http://tools.ns.svc.cluster.local/mcp":  false,
		"http://printer.local/mcp":               false,
		"http://tools.ns.svc.cluster.example/mc": true,
	}
	for endpoint, warn := range cases {
		var buf bytes.Buffer
		_, err := New(context.Background(), fw.MCPServerConfig{Name: "w", Endpoint: endpoint}, nil, slog.New(slog.NewTextHandler(&buf, nil)))
		if err != nil {
			t.Fatalf("New(%s): %v", endpoint, err)
		}
		if got := strings.Contains(buf.String(), "plain http"); got != warn {
			t.Errorf("%s: warning = %v, want %v (log: %s)", endpoint, got, warn, buf.String())
		}
	}
}

func TestMergeToolsKeepsOrder(t *testing.T) {
	mk := func(names ...string) []*tool {
		out := make([]*tool, len(names))
		for i, n := range names {
			out[i] = &tool{name: n, spec: fw.ToolSpec{Name: "s." + n}}
		}
		return out
	}
	got := mergeTools(mk("b", "d", "a"), mk("z", "a", "c", "d"))
	var names []string
	for _, t := range got {
		names = append(names, t.name)
	}
	if strings.Join(names, ",") != "d,a,c,z" {
		t.Errorf("merge = %v, want d,a,c,z", names)
	}
}

func TestLegacyElicitationAndProgress(t *testing.T) {
	ts := serveHTTP(t, newTestMCPServer(nil), nil)
	s := newServer(t, fw.MCPServerConfig{Name: "test", Endpoint: ts.URL}, nil)
	// Revision 2025-11-25: the server sends elicitation/create as its own
	// request, with no link to the call, and fw maps it to the only
	// in-flight call.
	s.protocol = "2025-11-25"
	var mu sync.Mutex
	var asked string
	var reports int
	ctx := fw.WithAsker(context.Background(), func(_ context.Context, q string, _ []byte) ([]byte, error) {
		mu.Lock()
		asked = q
		mu.Unlock()
		return []byte(`{"ok":false}`), nil
	})
	ctx = fw.WithProgress(ctx, func(float64, float64, string) {
		mu.Lock()
		reports++
		mu.Unlock()
	})
	res, err := s.Call(ctx, "ask", nil)
	if err != nil {
		t.Fatalf("Call ask: %v", err)
	}
	if got := fw.TextOf(res.Content); !strings.HasPrefix(got, "action=accept ok=false") {
		t.Errorf("ask result = %q", got)
	}
	if res, err := s.Call(ctx, "count", nil); err != nil || res.IsError {
		t.Fatalf("Call count = %+v, %v", res, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if asked != "Proceed?" {
		t.Errorf("asked %q", asked)
	}
	if reports != 3 {
		t.Errorf("progress reports = %d, want 3", reports)
	}
	s.mu.Lock()
	cs := s.session
	s.mu.Unlock()
	if r := cs.InitializeResult(); r == nil || r.ProtocolVersion != "2025-11-25" {
		t.Errorf("negotiated %+v, want 2025-11-25", r)
	}
}

func TestURLElicitation(t *testing.T) {
	srv := newTestMCPServer(nil)
	// Revision 2026-07-28: a URL request inside an input_required result.
	srv.AddTool(&sdk.Tool{Name: "login", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			if r, ok := req.Params.InputResponses["visit"].(*sdk.ElicitResult); ok {
				return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "visited: " + r.Action}}}, nil
			}
			return &sdk.CallToolResult{InputRequests: sdk.InputRequestMap{"visit": &sdk.ElicitParams{
				Mode: "url", Message: "Sign in", URL: "https://idp.example.com/login", ElicitationID: "e1",
			}}}, nil
		})
	// Revision 2025-11-25: the error -32042.
	var mu sync.Mutex
	tries := 0
	srv.AddTool(&sdk.Tool{Name: "legacy_login", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			mu.Lock()
			tries++
			n := tries
			mu.Unlock()
			if n == 1 {
				return nil, sdk.URLElicitationRequiredError([]*sdk.ElicitParams{{
					Mode: "url", Message: "Authorize", URL: "https://idp.example.com/authorize", ElicitationID: "e2",
				}})
			}
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "authorized"}}}, nil
		})
	ts := serveHTTP(t, srv, nil)
	s := newServer(t, fw.MCPServerConfig{Name: "test", Endpoint: ts.URL}, nil)

	var questions []string
	var schemas [][]byte
	ctx := fw.WithAsker(context.Background(), func(_ context.Context, q string, sc []byte) ([]byte, error) {
		mu.Lock()
		questions = append(questions, q)
		schemas = append(schemas, sc)
		mu.Unlock()
		return []byte(`{"answer":"done"}`), nil
	})
	res, err := s.Call(ctx, "login", nil)
	if err != nil || fw.TextOf(res.Content) != "visited: accept" {
		t.Fatalf("Call login = %+v, %v", res, err)
	}
	res, err = s.Call(ctx, "legacy_login", nil)
	if err != nil || fw.TextOf(res.Content) != "authorized" {
		t.Fatalf("Call legacy_login = %+v, %v", res, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(questions) != 2 {
		t.Fatalf("questions = %q", questions)
	}
	if !strings.Contains(questions[0], "Sign in") || !strings.Contains(questions[0], "https://idp.example.com/login") {
		t.Errorf("question = %q, want the message and the URL", questions[0])
	}
	if !strings.Contains(questions[1], "https://idp.example.com/authorize") {
		t.Errorf("question = %q, want the URL", questions[1])
	}
	for _, sc := range schemas {
		if sc != nil {
			t.Errorf("URL mode sent schema %s, want none", sc)
		}
	}
}

func TestReconnect(t *testing.T) {
	ctx := context.Background()
	var broken atomic.Bool
	ts := serveHTTP(t, newTestMCPServer(nil), func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if broken.Load() {
				http.Error(w, "broken", http.StatusBadRequest)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	s := newServer(t, fw.MCPServerConfig{Name: "test", Endpoint: ts.URL}, nil)
	if res, err := s.Call(ctx, "echo", json.RawMessage(`{"msg":"a"}`)); err != nil || res.IsError {
		t.Fatalf("first call = %+v, %v", res, err)
	}
	broken.Store(true)
	if res, _ := s.Call(ctx, "echo", json.RawMessage(`{"msg":"b"}`)); !res.IsError {
		t.Fatalf("call to a broken server = %+v, want an error result", res)
	}
	broken.Store(false)
	deadline := time.Now().Add(5 * time.Second)
	for {
		res, err := s.Call(ctx, "echo", json.RawMessage(`{"msg":"c"}`))
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError {
			if fw.TextOf(res.Content) != "echo: c" {
				t.Fatalf("result = %+v", res)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no reconnect: %+v", res)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestActionFromTitle checks that a trusted server gives the allow-button
// label from the title of a tool, and that an untrusted server never does.
func TestActionFromTitle(t *testing.T) {
	long := "Create a pull request in the repository of the current workspace"
	cases := []struct {
		name    string
		tool    sdk.Tool
		trusted bool
		want    string
	}{
		{"title", sdk.Tool{Name: "open_pr", Title: "  Open pull request \n"}, true, "Open pull request"},
		{"annotation title", sdk.Tool{Name: "open_pr", Annotations: &sdk.ToolAnnotations{Title: "Open pull request"}}, true, "Open pull request"},
		{"title wins", sdk.Tool{Name: "open_pr", Title: "Open pull request", Annotations: &sdk.ToolAnnotations{Title: "Other"}}, true, "Open pull request"},
		{"blank title", sdk.Tool{Name: "open_pr", Title: " ", Annotations: &sdk.ToolAnnotations{Title: "Merge"}}, true, "Merge"},
		{"no title", sdk.Tool{Name: "open_pr"}, true, ""},
		{"untrusted", sdk.Tool{Name: "open_pr", Title: "Open pull request"}, false, ""},
		{"untrusted annotation", sdk.Tool{Name: "open_pr", Annotations: &sdk.ToolAnnotations{Title: "Open pull request"}}, false, ""},
		{"controls", sdk.Tool{Name: "x", Title: "Delete‮​  branch\tnow"}, true, "Delete branch now"},
		{"long", sdk.Tool{Name: "x", Title: long}, true, "Create a pull request in the repository…"},
		{"long mid-word", sdk.Tool{Name: "x", Title: "Open pull requests against the default branch"}, true, "Open pull requests against the default…"},
	}
	for _, c := range cases {
		s := &Server{cfg: fw.MCPServerConfig{Name: "git", Trusted: c.trusted}}
		spec := s.newTool(&c.tool).spec
		if spec.Action != c.want {
			t.Errorf("%s: action %q, want %q", c.name, spec.Action, c.want)
		}
		if n := len([]rune(spec.Action)); n > maxActionRunes {
			t.Errorf("%s: action has %d runes, want at most %d", c.name, n, maxActionRunes)
		}
	}
}
