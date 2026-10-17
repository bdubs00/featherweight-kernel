package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/bdubs00/featherweight-kernel/fw"
	peer "github.com/bdubs00/featherweight-kernel/tools/a2a"
)

// script returns canned model responses in order and records the requests.
type script struct {
	mu    sync.Mutex
	resps []*fw.Response
	reqs  []*fw.Request
}

func newScript(resps ...*fw.Response) *script { return &script{resps: resps} }

func (s *script) model() fw.Model {
	return fw.ModelFunc{StreamFn: func(_ context.Context, req *fw.Request, _ func(fw.Delta)) (*fw.Response, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.reqs = append(s.reqs, req)
		if len(s.resps) == 0 {
			return nil, errors.New("script: no response left")
		}
		r := s.resps[0]
		s.resps = s.resps[1:]
		return r, nil
	}}
}

func (s *script) requests() []*fw.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*fw.Request(nil), s.reqs...)
}

func endTurn(text string) *fw.Response {
	return &fw.Response{Content: []fw.Block{fw.Text(text)}, StopReason: fw.StopEndTurn}
}

func callTool(id, name, input string) *fw.Response {
	return &fw.Response{
		Content:    []fw.Block{fw.Text("calling"), {Type: fw.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(input)}},
		StopReason: fw.StopToolUse,
	}
}

func echoTool() fw.Tool {
	return fw.NewTool(fw.ToolSpec{Name: "echo", ReadOnly: true}, func(_ context.Context, in json.RawMessage) (fw.ToolResult, error) {
		var v struct{ Text string }
		_ = json.Unmarshal(in, &v)
		return fw.TextResult(v.Text), nil
	})
}

func rmTool(calls *atomic.Int32) fw.Tool {
	return fw.NewTool(fw.ToolSpec{Name: "rm", Destructive: true}, func(context.Context, json.RawMessage) (fw.ToolResult, error) {
		calls.Add(1)
		return fw.TextResult("removed"), nil
	})
}

type env struct {
	a   *fw.Agent
	srv *httptest.Server
	rpc *peer.Client
}

func newEnv(t *testing.T, m fw.Model, opts ...fw.Option) *env {
	return newEnvWith(t, New(), m, opts...)
}

func newEnvWith(t *testing.T, s *Surface, m fw.Model, opts ...fw.Option) *env {
	t.Helper()
	t.Setenv("FW_ATESPACE", "")
	base := []fw.Option{
		fw.WithoutDefaults(),
		fw.WithAuthn(fw.NoAuthn{}),
		fw.WithModel(m),
		fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()),
		fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		fw.WithSurfaces(s),
	}
	a, err := fw.New(append(append(base, fw.WithTitles(fw.TitlesInput)), opts...)...)
	if err != nil {
		t.Fatalf("fw.New: %v", err)
	}
	h, err := a.Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	return &env{a: a, srv: srv, rpc: &peer.Client{URL: srv.URL + PathJSONRPC, HTTP: srv.Client()}}
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func userMsg(text string) *sdk.Message {
	return sdk.NewMessage(sdk.MessageRoleUser, sdk.NewTextPart(text))
}

func taskMsg(id sdk.TaskID, text string) *sdk.Message {
	m := userMsg(text)
	m.TaskID = id
	return m
}

func send(t *testing.T, e *env, req *sdk.SendMessageRequest) *sdk.Task {
	t.Helper()
	res, err := e.rpc.SendMessage(testCtx(t), req)
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	task, ok := res.(*sdk.Task)
	if !ok {
		t.Fatalf("SendMessage result is %T", res)
	}
	return task
}

func artifactText(t *testing.T, task *sdk.Task) string {
	t.Helper()
	if len(task.Artifacts) != 1 || len(task.Artifacts[0].Parts) != 1 {
		t.Fatalf("artifacts = %+v", task.Artifacts)
	}
	return task.Artifacts[0].Parts[0].Text()
}

func mustState(t *testing.T, task *sdk.Task, want sdk.TaskState) {
	t.Helper()
	if task.Status.State != want {
		msg := ""
		if task.Status.Message != nil {
			msg = textOf(task.Status.Message.Parts)
		}
		t.Fatalf("state = %s, want %s (message %q)", task.Status.State, want, msg)
	}
}

func collect(t *testing.T, seq func(func(sdk.Event, error) bool)) []sdk.Event {
	t.Helper()
	var out []sdk.Event
	for ev, err := range seq {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		out = append(out, ev)
	}
	return out
}

func TestCard(t *testing.T) {
	e := newEnv(t, newScript().model(),
		fw.WithProfile(fw.AgentProfile{Name: "reviewer", System: "# Reviews code\n\nMore text."}),
		fw.WithProfile(fw.AgentProfile{Name: "plain"}))
	resp, err := http.Get(e.srv.URL + PathCard)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("card response = %s %v", resp.Status, resp.Header)
	}
	var raw map[string]any
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"name", "description", "supportedInterfaces", "version", "capabilities", "defaultInputModes", "defaultOutputModes", "skills"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("card has no %q: %s", k, body)
		}
	}
	var card sdk.AgentCard
	if err := json.Unmarshal(body, &card); err != nil {
		t.Fatal(err)
	}
	if card.Name != "featherweight" || card.Description != "A featherweight agent" || card.Version != "dev" {
		t.Fatalf("card = %+v", card)
	}
	if !card.Capabilities.Streaming || !card.Capabilities.PushNotifications {
		t.Fatalf("capabilities = %+v", card.Capabilities)
	}
	if strings.Join(card.DefaultInputModes, ",") != "text/plain" || strings.Join(card.DefaultOutputModes, ",") != "text/plain" {
		t.Fatalf("modes = %v %v", card.DefaultInputModes, card.DefaultOutputModes)
	}
	if len(card.Skills) != 2 || card.Skills[0].ID != "plain" || card.Skills[0].Description != "plain" ||
		card.Skills[1].ID != "reviewer" || card.Skills[1].Name != "reviewer" || card.Skills[1].Description != "Reviews code" ||
		strings.Join(card.Skills[1].Tags, ",") != "fw" {
		t.Fatalf("skills = %+v", card.Skills)
	}
	ifs := card.SupportedInterfaces
	if len(ifs) != 2 ||
		ifs[0].URL != e.srv.URL+"/a2a/jsonrpc" || ifs[0].ProtocolBinding != sdk.TransportProtocolJSONRPC || ifs[0].ProtocolVersion != sdk.Version ||
		ifs[1].URL != e.srv.URL+"/a2a" || ifs[1].ProtocolBinding != sdk.TransportProtocolHTTPJSON {
		t.Fatalf("interfaces = %+v %+v", ifs[0], ifs[1])
	}
	if len(card.SecuritySchemes) != 0 || len(card.SecurityRequirements) != 0 {
		t.Fatalf("security = %+v %+v", card.SecuritySchemes, card.SecurityRequirements)
	}

	// The command line card takes the base URL from the configuration.
	cfg := e.a.Config()
	for _, c := range []struct{ listen, url, want string }{
		{":8080", "", "http://localhost:8080/a2a/jsonrpc"},
		{":80", "", "http://localhost/a2a/jsonrpc"},
		{"10.0.0.7:80", "", "http://10.0.0.7/a2a/jsonrpc"},
		{"[::1]:9000", "", "http://[::1]:9000/a2a/jsonrpc"},
		{":8080", "https://agent.example.com/", "https://agent.example.com/a2a/jsonrpc"},
	} {
		cfg.Listen, cfg.A2A.URL = c.listen, c.url
		card, err := Card(e.a)
		if err != nil {
			t.Fatal(err)
		}
		if got := card.SupportedInterfaces[0].URL; got != c.want {
			t.Errorf("listen %q url %q: interface = %q, want %q", c.listen, c.url, got, c.want)
		}
	}
	cfg.Version = "1.2.3"
	b, err := CardJSON(e.a)
	if err != nil || !json.Valid(b) || !bytes.Contains(b, []byte(`"version": "1.2.3"`)) {
		t.Fatalf("CardJSON = %s, %v", b, err)
	}
	// A configured URL wins over the request host on the served card.
	resp2, err := http.Get(e.srv.URL + PathCard)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var card2 sdk.AgentCard
	if err := json.NewDecoder(resp2.Body).Decode(&card2); err != nil {
		t.Fatal(err)
	}
	if card2.SupportedInterfaces[1].URL != "https://agent.example.com/a2a" {
		t.Fatalf("served interfaces = %+v", card2.SupportedInterfaces[1])
	}
	cfg.A2A.URL = "not a url"
	if _, err := Card(e.a); err == nil {
		t.Fatal("Card accepted a relative a2a.url")
	}
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func TestBearerAuth(t *testing.T) {
	e := newEnv(t, newScript(endTurn("hello")).model(), fw.WithAuthn(fw.TokenAuthn{Token: "s3cret"}))
	ctx := testCtx(t)
	// The card stays public and names the bearer scheme.
	resp, err := http.Get(e.srv.URL + PathCard)
	if err != nil {
		t.Fatal(err)
	}
	var card sdk.AgentCard
	err = json.NewDecoder(resp.Body).Decode(&card)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("card: %s %v", resp.Status, err)
	}
	s, ok := card.SecuritySchemes["bearer"].(sdk.HTTPAuthSecurityScheme)
	if !ok || s.Scheme != "Bearer" || len(card.SecurityRequirements) != 1 {
		t.Fatalf("security = %+v %+v", card.SecuritySchemes, card.SecurityRequirements)
	}
	// Both bindings refuse a request without the token.
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, PathJSONRPC},
		{http.MethodGet, "/a2a/tasks"},
		{http.MethodPost, "/a2a/message:send"},
	} {
		req, _ := http.NewRequestWithContext(ctx, c.method, e.srv.URL+c.path, strings.NewReader(`{}`))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") == "" {
			t.Fatalf("%s %s = %s", c.method, c.path, resp.Status)
		}
	}
	if _, err := e.rpc.SendMessage(ctx, &sdk.SendMessageRequest{Message: userMsg("hi")}); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("SendMessage without a token = %v", err)
	}
	e.rpc.HTTP = &http.Client{Transport: bearer{"s3cret"}}
	task := send(t, e, &sdk.SendMessageRequest{Message: userMsg("hi")})
	mustState(t, task, sdk.TaskStateCompleted)
	r, err := e.a.Get(ctx, string(task.ID))
	if err != nil || r.Principal != "token" || r.Origin != fw.OriginA2A {
		t.Fatalf("run = %+v, %v", r, err)
	}
}

func TestSendMessage(t *testing.T) {
	s := newScript(endTurn("hello"), endTurn("in context"))
	e := newEnv(t, s.model())
	task := send(t, e, &sdk.SendMessageRequest{Message: userMsg("say hi")})
	mustState(t, task, sdk.TaskStateCompleted)
	if got := artifactText(t, task); got != "hello" {
		t.Fatalf("artifact = %q", got)
	}
	if task.ContextID != string(task.ID) || len(task.History) != 1 || task.History[0].Parts[0].Text() != "say hi" {
		t.Fatalf("task = %+v", task)
	}
	r, err := e.a.Get(testCtx(t), string(task.ID))
	if err != nil || r.Origin != fw.OriginA2A || r.Principal != "anonymous" || r.Agent != "default" || r.Input != "say hi" {
		t.Fatalf("run = %+v, %v", r, err)
	}
	// A context id from the client stays on the task.
	m := userMsg("again")
	m.ContextID = "ctx-1"
	task = send(t, e, &sdk.SendMessageRequest{Message: m})
	if task.ContextID != "ctx-1" || artifactText(t, task) != "in context" {
		t.Fatalf("task = %+v", task)
	}
}

func TestSendMessageRetryIsIdempotent(t *testing.T) {
	e := newEnv(t, newScript(endTurn("once")).model())
	m := userMsg("do it once")
	first := send(t, e, &sdk.SendMessageRequest{Message: m})
	second := send(t, e, &sdk.SendMessageRequest{Message: m})
	if first.ID != second.ID || artifactText(t, second) != "once" {
		t.Fatalf("first %s, second %s", first.ID, second.ID)
	}
}

func TestProfileSelection(t *testing.T) {
	e := newEnv(t, newScript(endTurn("a"), endTurn("b")).model(),
		fw.WithProfile(fw.AgentProfile{Name: "reviewer", System: "Review."}))
	ctx := testCtx(t)
	m := userMsg("review this")
	m.Metadata = map[string]any{"agent": "reviewer"}
	task := send(t, e, &sdk.SendMessageRequest{Message: m})
	if r, _ := e.a.Get(ctx, string(task.ID)); r.Agent != "reviewer" {
		t.Fatalf("agent = %q", r.Agent)
	}
	task = send(t, e, &sdk.SendMessageRequest{Message: userMsg("by skill"), Metadata: map[string]any{"skillId": "reviewer"}})
	if r, _ := e.a.Get(ctx, string(task.ID)); r.Agent != "reviewer" {
		t.Fatalf("agent = %q", r.Agent)
	}
	m = userMsg("who")
	m.Metadata = map[string]any{"agent": "missing"}
	if _, err := e.rpc.SendMessage(ctx, &sdk.SendMessageRequest{Message: m}); !errors.Is(err, sdk.ErrInvalidParams) {
		t.Fatalf("unknown profile = %v", err)
	}
}

func TestInstructionsMetadata(t *testing.T) {
	m := newScript(endTurn("a"), endTurn("b"))
	e := newEnv(t, m.model())
	ctx := testCtx(t)
	msg := userMsg("write it")
	msg.Metadata = map[string]any{"instructions": "Use British spelling."}
	task := send(t, e, &sdk.SendMessageRequest{Message: msg})
	var q fw.RunQueuedData
	for ev, err := range e.a.Ledger().Read(ctx, string(task.ID), 0) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Type == fw.EvRunQueued {
			_ = ev.Decode(&q)
		}
	}
	if q.Instructions != "Use British spelling." {
		t.Fatalf("queued instructions = %q", q.Instructions)
	}
	if reqs := m.requests(); len(reqs) != 1 || !strings.Contains(reqs[0].System, "Use British spelling.") {
		t.Fatalf("the system prompt lacks the instructions: %+v", reqs)
	}
	// The request metadata works too, and a non-string value is ignored.
	task = send(t, e, &sdk.SendMessageRequest{Message: userMsg("again"), Metadata: map[string]any{"instructions": 42}})
	if r, _ := e.a.Get(ctx, string(task.ID)); r == nil || r.State != fw.StateCompleted {
		t.Fatalf("run = %+v", r)
	}
}

func TestNoTextPart(t *testing.T) {
	e := newEnv(t, newScript().model())
	ctx := testCtx(t)
	m := sdk.NewMessage(sdk.MessageRoleUser, sdk.NewDataPart(map[string]any{"x": 1}))
	if _, err := e.rpc.SendMessage(ctx, &sdk.SendMessageRequest{Message: m}); !errors.Is(err, sdk.ErrInvalidParams) {
		t.Fatalf("SendMessage = %v", err)
	}
	var err error
	for _, err = range e.rpc.SendStreamingMessage(ctx, &sdk.SendMessageRequest{Message: m}) {
		break
	}
	if !errors.Is(err, sdk.ErrInvalidParams) {
		t.Fatalf("SendStreamingMessage = %v", err)
	}
	// A bad push config starts no run.
	bad := &sdk.SendMessageRequest{Message: userMsg("hi"), Config: &sdk.SendMessageConfig{PushConfig: &sdk.PushConfig{URL: "not a url"}}}
	if _, err := e.rpc.SendMessage(ctx, bad); !errors.Is(err, sdk.ErrInvalidParams) {
		t.Fatalf("bad push config = %v", err)
	}
	if runs, _ := e.a.List(ctx); len(runs) != 0 {
		t.Fatalf("runs = %d", len(runs))
	}
}

// states lists the task states of a stream in order, and checks the shape:
// a task first, then status and artifact updates.
func states(t *testing.T, evs []sdk.Event) []sdk.TaskState {
	t.Helper()
	var out []sdk.TaskState
	for i, ev := range evs {
		switch v := ev.(type) {
		case *sdk.Task:
			if i != 0 {
				t.Fatalf("event %d is a task", i)
			}
			out = append(out, v.Status.State)
		case *sdk.TaskStatusUpdateEvent:
			if i == 0 {
				t.Fatal("the stream starts with a status update")
			}
			out = append(out, v.Status.State)
		case *sdk.TaskArtifactUpdateEvent:
		default:
			t.Fatalf("event %d is %T", i, ev)
		}
	}
	return out
}

func TestStream(t *testing.T) {
	s := newScript(callTool("c1", "echo", `{"text":"x"}`), endTurn("streamed"))
	e := newEnv(t, s.model(), fw.WithTools(echoTool()))
	evs := collect(t, e.rpc.SendStreamingMessage(testCtx(t), &sdk.SendMessageRequest{Message: userMsg("go")}))
	got := states(t, evs)
	want := []sdk.TaskState{sdk.TaskStateSubmitted, sdk.TaskStateWorking, sdk.TaskStateWorking, sdk.TaskStateCompleted}
	if len(got) != len(want) {
		t.Fatalf("states = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("states = %v", got)
		}
	}
	tool := evs[2].(*sdk.TaskStatusUpdateEvent)
	if tool.Status.Message == nil || textOf(tool.Status.Message.Parts) != "Calling the tool echo." {
		t.Fatalf("tool update = %+v", tool.Status)
	}
	art, ok := evs[len(evs)-2].(*sdk.TaskArtifactUpdateEvent)
	if !ok || art.Artifact.ID != outputID || art.Artifact.Parts[0].Text() != "streamed" || !art.LastChunk {
		t.Fatalf("artifact event = %+v", evs[len(evs)-2])
	}
	if task := evs[0].(*sdk.Task); art.TaskID != task.ID || art.ContextID != task.ContextID {
		t.Fatalf("artifact task = %s, task = %s", art.TaskID, task.ID)
	}
}

func TestFollowUp(t *testing.T) {
	s := newScript(endTurn("first"), endTurn("second"), endTurn("third"))
	e := newEnv(t, s.model())
	task := send(t, e, &sdk.SendMessageRequest{Message: userMsg("hi")})
	mustState(t, task, sdk.TaskStateCompleted)
	again := send(t, e, &sdk.SendMessageRequest{Message: taskMsg(task.ID, "again")})
	mustState(t, again, sdk.TaskStateCompleted)
	if again.ID != task.ID || artifactText(t, again) != "second" {
		t.Fatalf("follow-up = %+v", again)
	}
	// A streamed follow-up starts from the reopened run, not from the old
	// completed state.
	evs := collect(t, e.rpc.SendStreamingMessage(testCtx(t), &sdk.SendMessageRequest{Message: taskMsg(task.ID, "once more")}))
	got := states(t, evs)
	if got[0] != sdk.TaskStateWorking || got[len(got)-1] != sdk.TaskStateCompleted {
		t.Fatalf("states = %v", got)
	}
	if len(evs[0].(*sdk.Task).Artifacts) != 0 {
		t.Fatal("the reopened task still carries the old artifact")
	}
	if art := evs[len(evs)-2].(*sdk.TaskArtifactUpdateEvent); art.Artifact.Parts[0].Text() != "third" {
		t.Fatalf("artifact = %+v", art.Artifact)
	}
	msgs := s.requests()[2].Messages
	if len(msgs) != 5 || fw.TextOf(msgs[2].Content) != "again" || fw.TextOf(msgs[4].Content) != "once more" {
		t.Fatalf("history = %+v", msgs)
	}
	// A follow-up to an unknown task fails.
	if _, err := e.rpc.SendMessage(testCtx(t), &sdk.SendMessageRequest{Message: taskMsg("nope", "x")}); !errors.Is(err, sdk.ErrTaskNotFound) {
		t.Fatalf("unknown task = %v", err)
	}
}

func TestApprovalDeny(t *testing.T) {
	var calls atomic.Int32
	s := newScript(callTool("c1", "rm", `{"path":"x"}`), endTurn("done"))
	e := newEnv(t, s.model(), fw.WithTools(rmTool(&calls)))
	task := send(t, e, &sdk.SendMessageRequest{Message: userMsg("clean up")})
	mustState(t, task, sdk.TaskStateInputRequired)
	msg := task.Status.Message
	if msg == nil || msg.Role != sdk.MessageRoleAgent {
		t.Fatalf("status = %+v", task.Status)
	}
	text := textOf(msg.Parts)
	if !strings.Contains(text, "tool rm") || !strings.Contains(text, `{"path":"x"}`) || !strings.Contains(text, `"allow"`) {
		t.Fatalf("status text = %q", text)
	}
	ap, _ := msg.Metadata["approval"].(map[string]any)
	if ap["tool"] != "rm" || ap["id"] == "" {
		t.Fatalf("approval metadata = %+v", msg.Metadata)
	}
	// Text that is not a decision waits with the run.
	held := send(t, e, &sdk.SendMessageRequest{Message: taskMsg(task.ID, "why?")})
	mustState(t, held, sdk.TaskStateInputRequired)
	done := send(t, e, &sdk.SendMessageRequest{Message: taskMsg(task.ID, "Deny: too risky")})
	mustState(t, done, sdk.TaskStateCompleted)
	if calls.Load() != 0 {
		t.Fatal("a denied tool ran")
	}
	last := s.requests()[1].Messages
	var result string
	for _, m := range last {
		for _, b := range m.Content {
			if b.Type == fw.BlockToolResult {
				result = fw.TextOf(b.Content)
			}
		}
	}
	if result != "denied by anonymous: too risky" {
		t.Fatalf("tool result = %q", result)
	}
}

// postREST sends one request to the REST binding.
func postREST(t *testing.T, e *env, path string, body any, out any) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(e.srv.URL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST %s = %s %s", path, resp.Status, msg)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatal(err)
	}
}

func TestApprovalAllowREST(t *testing.T) {
	var calls atomic.Int32
	s := newScript(callTool("c1", "rm", `{}`), endTurn("done"))
	e := newEnv(t, s.model(), fw.WithTools(rmTool(&calls)))
	var sr sdk.StreamResponse
	postREST(t, e, "/a2a/message:send", &sdk.SendMessageRequest{Message: userMsg("clean up")}, &sr)
	task, ok := sr.Event.(*sdk.Task)
	if !ok {
		t.Fatalf("result = %T", sr.Event)
	}
	mustState(t, task, sdk.TaskStateInputRequired)
	postREST(t, e, "/a2a/message:send", &sdk.SendMessageRequest{Message: taskMsg(task.ID, "ALLOW")}, &sr)
	task = sr.Event.(*sdk.Task)
	mustState(t, task, sdk.TaskStateCompleted)
	if calls.Load() != 1 || artifactText(t, task) != "done" {
		t.Fatalf("calls = %d, task = %+v", calls.Load(), task)
	}
	resp, err := http.Get(e.srv.URL + "/a2a/tasks/" + string(task.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got sdk.Task
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || got.ID != task.ID || got.Status.State != sdk.TaskStateCompleted {
		t.Fatalf("GET task = %+v, %v", got, err)
	}
}

func TestQuestionAnswer(t *testing.T) {
	ask := fw.NewTool(fw.ToolSpec{Name: "ask", ReadOnly: true}, func(ctx context.Context, _ json.RawMessage) (fw.ToolResult, error) {
		ans, err := fw.AskCaller(ctx, "What is your name?", nil)
		if err != nil {
			return fw.ToolResult{}, err
		}
		return fw.TextResult(string(ans)), nil
	})
	s := newScript(callTool("c1", "ask", `{}`), endTurn("thanks"))
	e := newEnv(t, s.model(), fw.WithTools(ask))
	task := send(t, e, &sdk.SendMessageRequest{Message: userMsg("greet me")})
	mustState(t, task, sdk.TaskStateInputRequired)
	if text := textOf(task.Status.Message.Parts); !strings.HasPrefix(text, "What is your name?") {
		t.Fatalf("question = %q", text)
	}
	task = send(t, e, &sdk.SendMessageRequest{Message: taskMsg(task.ID, "Ada")})
	mustState(t, task, sdk.TaskStateCompleted)
	var result string
	for _, m := range s.requests()[1].Messages {
		for _, b := range m.Content {
			if b.Type == fw.BlockToolResult {
				result = fw.TextOf(b.Content)
			}
		}
	}
	if result != `{"answer":"Ada"}` {
		t.Fatalf("answer = %q", result)
	}
}

// gatedModel answers "ok" to every request, after the gate opens for an
// input of "wait".
func gatedModel(started chan<- struct{}, gate <-chan struct{}) fw.Model {
	return fw.ModelFunc{StreamFn: func(ctx context.Context, req *fw.Request, _ func(fw.Delta)) (*fw.Response, error) {
		if fw.TextOf(req.Messages[0].Content) == "wait" {
			select {
			case started <- struct{}{}:
			default:
			}
			select {
			case <-gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return endTurn("ok"), nil
	}}
}

func TestGetListCancel(t *testing.T) {
	ctx := testCtx(t)
	started := make(chan struct{}, 1)
	e := newEnv(t, gatedModel(started, make(chan struct{})))
	done := send(t, e, &sdk.SendMessageRequest{Message: userMsg("quick")})
	mustState(t, done, sdk.TaskStateCompleted)

	got, err := e.rpc.GetTask(ctx, &sdk.GetTaskRequest{ID: done.ID})
	if err != nil || got.Status.State != sdk.TaskStateCompleted || artifactText(t, got) != "ok" || len(got.History) != 1 {
		t.Fatalf("GetTask = %+v, %v", got, err)
	}
	zero := 0
	if got, err = e.rpc.GetTask(ctx, &sdk.GetTaskRequest{ID: done.ID, HistoryLength: &zero}); err != nil || len(got.History) != 0 {
		t.Fatalf("GetTask historyLength 0 = %+v, %v", got, err)
	}
	if _, err := e.rpc.GetTask(ctx, &sdk.GetTaskRequest{ID: "nope"}); !errors.Is(err, sdk.ErrTaskNotFound) {
		t.Fatalf("GetTask unknown = %v", err)
	}

	waiting := send(t, e, &sdk.SendMessageRequest{Message: userMsg("wait"), Config: &sdk.SendMessageConfig{ReturnImmediately: true}})
	if waiting.Status.State.Terminal() {
		t.Fatalf("returnImmediately task = %s", waiting.Status.State)
	}
	<-started

	var list sdk.ListTasksResponse
	if err := e.rpc.Call(ctx, peer.MethodListTasks, &sdk.ListTasksRequest{}, &list); err != nil {
		t.Fatal(err)
	}
	if list.TotalSize != 2 || len(list.Tasks) != 2 || list.Tasks[0].ID != waiting.ID || list.Tasks[1].ID != done.ID || list.NextPageToken != "" {
		t.Fatalf("list = %+v", list)
	}
	if len(list.Tasks[1].Artifacts) != 0 {
		t.Fatal("ListTasks returned artifacts without includeArtifacts")
	}
	if err := e.rpc.Call(ctx, peer.MethodListTasks, &sdk.ListTasksRequest{Status: sdk.TaskStateCompleted, IncludeArtifacts: true}, &list); err != nil {
		t.Fatal(err)
	}
	if list.TotalSize != 1 || list.Tasks[0].ID != done.ID || len(list.Tasks[0].Artifacts) != 1 {
		t.Fatalf("filtered list = %+v", list)
	}
	if err := e.rpc.Call(ctx, peer.MethodListTasks, &sdk.ListTasksRequest{PageSize: 1}, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Tasks) != 1 || list.Tasks[0].ID != waiting.ID || list.NextPageToken != string(waiting.ID) {
		t.Fatalf("page 1 = %+v", list)
	}
	if err := e.rpc.Call(ctx, peer.MethodListTasks, &sdk.ListTasksRequest{PageSize: 1, PageToken: list.NextPageToken}, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Tasks) != 1 || list.Tasks[0].ID != done.ID || list.NextPageToken != "" {
		t.Fatalf("page 2 = %+v", list)
	}

	var canceled sdk.Task
	if err := e.rpc.Call(ctx, peer.MethodCancelTask, &sdk.CancelTaskRequest{ID: waiting.ID}, &canceled); err != nil {
		t.Fatal(err)
	}
	mustState(t, &canceled, sdk.TaskStateCanceled)
	if err := e.rpc.Call(ctx, peer.MethodCancelTask, &sdk.CancelTaskRequest{ID: waiting.ID}, &canceled); !errors.Is(err, sdk.ErrTaskNotCancelable) {
		t.Fatalf("second cancel = %v", err)
	}
	if r, _ := e.a.Get(ctx, string(waiting.ID)); r.State != fw.StateCanceled {
		t.Fatalf("run state = %s", r.State)
	}
}

func TestSubscribe(t *testing.T) {
	ctx := testCtx(t)
	started, gate := make(chan struct{}, 1), make(chan struct{})
	e := newEnv(t, gatedModel(started, gate))
	task := send(t, e, &sdk.SendMessageRequest{Message: userMsg("wait"), Config: &sdk.SendMessageConfig{ReturnImmediately: true}})
	<-started
	var evs []sdk.Event
	for ev, err := range e.rpc.Stream(ctx, peer.MethodSubscribeToTask, &sdk.SubscribeToTaskRequest{ID: task.ID}) {
		if err != nil {
			t.Fatal(err)
		}
		if len(evs) == 0 {
			close(gate)
		}
		evs = append(evs, ev)
	}
	got := states(t, evs)
	if got[0] != sdk.TaskStateWorking || got[len(got)-1] != sdk.TaskStateCompleted {
		t.Fatalf("states = %v", got)
	}
	for _, err := range e.rpc.Stream(ctx, peer.MethodSubscribeToTask, &sdk.SubscribeToTaskRequest{ID: task.ID}) {
		if !errors.Is(err, sdk.ErrUnsupportedOperation) {
			t.Fatalf("subscribe to a final task = %v", err)
		}
		break
	}
}

func TestPushNotifications(t *testing.T) {
	type note struct {
		token string
		task  *sdk.Task
	}
	notes := make(chan note, 16)
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var sr sdk.StreamResponse
		if err := json.NewDecoder(r.Body).Decode(&sr); err != nil {
			t.Errorf("push body: %v", err)
		}
		task, _ := sr.Event.(*sdk.Task)
		notes <- note{token: r.Header.Get("A2A-Notification-Token"), task: task}
	}))
	defer recv.Close()

	ctx := testCtx(t)
	started, gate := make(chan struct{}, 1), make(chan struct{})
	e := newEnvWith(t, &Surface{AllowPrivatePush: true}, gatedModel(started, gate))
	task := send(t, e, &sdk.SendMessageRequest{
		Message: userMsg("wait"),
		Config:  &sdk.SendMessageConfig{ReturnImmediately: true, PushConfig: &sdk.PushConfig{ID: "p1", URL: recv.URL, Token: "tok"}},
	})
	<-started
	var cfgs sdk.ListTaskPushConfigResponse
	if err := e.rpc.Call(ctx, "ListTaskPushNotificationConfigs", &sdk.ListTaskPushConfigRequest{TaskID: task.ID}, &cfgs); err != nil {
		t.Fatal(err)
	}
	if len(cfgs.Configs) != 1 || cfgs.Configs[0].ID != "p1" || cfgs.Configs[0].TaskID != task.ID {
		t.Fatalf("configs = %+v", cfgs)
	}
	close(gate)
	for {
		select {
		case n := <-notes:
			if n.token != "tok" || n.task == nil || n.task.ID != task.ID {
				t.Fatalf("push = %+v", n)
			}
			if n.task.Status.State == sdk.TaskStateCompleted {
				if artifactText(t, n.task) != "ok" {
					t.Fatalf("pushed task = %+v", n.task)
				}
				return
			}
		case <-ctx.Done():
			t.Fatal("no push with the completed state")
		}
	}
}

func TestTaskStateMapping(t *testing.T) {
	want := map[fw.RunState]sdk.TaskState{
		fw.StateQueued:        sdk.TaskStateSubmitted,
		fw.StateRunning:       sdk.TaskStateWorking,
		fw.StateInputRequired: sdk.TaskStateInputRequired,
		fw.StateCompleted:     sdk.TaskStateCompleted,
		fw.StateFailed:        sdk.TaskStateFailed,
		fw.StateCanceled:      sdk.TaskStateCanceled,
		fw.StateSuspended:     sdk.TaskStateWorking,
	}
	for s, ts := range want {
		if got := TaskState(s); got != ts {
			t.Errorf("TaskState(%s) = %s, want %s", s, got, ts)
		}
	}
}

func TestParseDecision(t *testing.T) {
	for _, c := range []struct {
		in, verdict, note string
		ok                bool
	}{
		{"allow", "allow", "", true},
		{" DENY ", "deny", "", true},
		{"Deny: too risky", "deny", "too risky", true},
		{"allow:ok: really", "allow", "ok: really", true},
		{"allowed", "", "", false},
		{"please allow", "", "", false},
	} {
		v, n, ok := parseDecision(c.in)
		if v != c.verdict || n != c.note || ok != c.ok {
			t.Errorf("parseDecision(%q) = %q %q %v", c.in, v, n, ok)
		}
	}
}
