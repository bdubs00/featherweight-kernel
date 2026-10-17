package a2a

import (
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
	surface "github.com/bdubs00/featherweight-kernel/surface/a2a"
)

type script struct {
	mu    sync.Mutex
	resps []*fw.Response
}

func (s *script) model() fw.Model {
	return fw.ModelFunc{StreamFn: func(context.Context, *fw.Request, func(fw.Delta)) (*fw.Response, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.resps) == 0 {
			return nil, errors.New("script: no response left")
		}
		r := s.resps[0]
		s.resps = s.resps[1:]
		return r, nil
	}}
}

func endTurn(text string) *fw.Response {
	return &fw.Response{Content: []fw.Block{fw.Text(text)}, StopReason: fw.StopEndTurn}
}

func callTool(id, name, input string) *fw.Response {
	return &fw.Response{
		Content:    []fw.Block{{Type: fw.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(input)}},
		StopReason: fw.StopToolUse,
	}
}

// startPeer serves an fw agent with the A2A surface and returns its URL.
func startPeer(t *testing.T, resps []*fw.Response, tools ...fw.Tool) string {
	t.Helper()
	t.Setenv("FW_ATESPACE", "")
	a, err := fw.New(fw.WithTitles(fw.TitlesInput),
		fw.WithoutDefaults(),
		fw.WithAuthn(fw.NoAuthn{}),
		fw.WithModel((&script{resps: resps}).model()),
		fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()),
		fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		fw.WithSurfaces(surface.New()),
		fw.WithTools(tools...),
	)
	if err != nil {
		t.Fatal(err)
	}
	h, err := a.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	return srv.URL
}

func onePeer(t *testing.T, pc fw.PeerConfig, router string) *Peer {
	t.Helper()
	cfg := fw.DefaultConfig()
	cfg.AX.Router = router
	cfg.Tools.Peers = []fw.PeerConfig{pc}
	peers, err := Peers(cfg)
	if err != nil || len(peers) != 1 {
		t.Fatalf("Peers = %v, %v", peers, err)
	}
	t.Cleanup(peers[0].Close)
	return peers[0]
}

// progressCtx collects the progress messages of a call.
func progressCtx(t *testing.T) (context.Context, func() []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	var mu sync.Mutex
	var msgs []string
	ctx = fw.WithProgress(ctx, func(_, _ float64, m string) {
		mu.Lock()
		defer mu.Unlock()
		msgs = append(msgs, m)
	})
	return ctx, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), msgs...)
	}
}

func call(t *testing.T, ctx context.Context, p *Peer, input string) (string, bool) {
	t.Helper()
	res, err := p.Call(ctx, json.RawMessage(input))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	return fw.TextOf(res.Content), res.IsError
}

// splitTaskID returns the text before the task id line and the id.
func splitTaskID(t *testing.T, text string) (string, string) {
	t.Helper()
	body, id, ok := strings.Cut(text, "\n\ntask_id: ")
	if !ok || id == "" || strings.Contains(id, "\n") {
		t.Fatalf("no task id on the last line: %q", text)
	}
	return body, id
}

func TestAskCardPeer(t *testing.T) {
	echo := fw.NewTool(fw.ToolSpec{Name: "echo", ReadOnly: true}, func(context.Context, json.RawMessage) (fw.ToolResult, error) {
		return fw.TextResult("x"), nil
	})
	base := startPeer(t, []*fw.Response{callTool("c1", "echo", `{}`), endTurn("hello from peer"), endTurn("second answer")}, echo)
	p := onePeer(t, fw.PeerConfig{Name: "helper", Card: base + "/.well-known/agent-card.json"}, "")

	spec := p.Spec()
	if spec.Name != "ask_helper" || spec.Source != "helper" || !spec.Destructive || !spec.ParallelSafe || spec.ReadOnly {
		t.Fatalf("spec = %+v", spec)
	}
	var schema struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if err := json.Unmarshal(spec.InputSchema, &schema); err != nil || schema.Properties["message"] == nil ||
		schema.Properties["continue"] == nil || strings.Join(schema.Required, ",") != "message" {
		t.Fatalf("schema = %s, %v", spec.InputSchema, err)
	}

	ctx, progress := progressCtx(t)
	text, isErr := call(t, ctx, p, `{"message":"hi"}`)
	body, id := splitTaskID(t, text)
	if isErr || body != "hello from peer" {
		t.Fatalf("result = %q (error %v)", text, isErr)
	}
	if got := progress(); len(got) == 0 || got[0] != "Calling the tool echo." {
		t.Fatalf("progress = %q", got)
	}
	text, _ = call(t, ctx, p, `{"message":"more","continue":"`+id+`"}`)
	body, id2 := splitTaskID(t, text)
	if body != "second answer" || id2 != id {
		t.Fatalf("follow-up = %q", text)
	}
	// A continue with an unknown task is an error the model can read.
	text, isErr = call(t, ctx, p, `{"message":"more","continue":"nope"}`)
	if !isErr || !strings.Contains(text, "task not found") {
		t.Fatalf("unknown task = %q", text)
	}
}

func TestAskInputRequired(t *testing.T) {
	var calls atomic.Int32
	rm := fw.NewTool(fw.ToolSpec{Name: "rm", Destructive: true}, func(context.Context, json.RawMessage) (fw.ToolResult, error) {
		calls.Add(1)
		return fw.TextResult("removed"), nil
	})
	base := startPeer(t, []*fw.Response{callTool("c1", "rm", `{"path":"tmp"}`), endTurn("cleaned")}, rm)
	p := onePeer(t, fw.PeerConfig{Name: "janitor", Card: base}, "")
	ctx, _ := progressCtx(t)
	text, isErr := call(t, ctx, p, `{"message":"clean up"}`)
	body, id := splitTaskID(t, text)
	if isErr || !strings.HasPrefix(body, "The peer needs input:\n") || !strings.Contains(body, "tool rm") {
		t.Fatalf("result = %q", text)
	}
	text, isErr = call(t, ctx, p, `{"message":"allow","continue":"task_id: `+id+`"}`)
	if body, _ := splitTaskID(t, text); isErr || body != "cleaned" || calls.Load() != 1 {
		t.Fatalf("after allow = %q, calls %d", text, calls.Load())
	}
}

// axServer is a canned A2A agent behind a fake atenet router. It answers
// SendMessage with a working task and GetTask with a completed one after
// pending GetTask calls.
type axServer struct {
	t        *testing.T
	pending  int // GetTask calls that still answer working
	requests atomic.Int32
	mu       sync.Mutex
	methods  []string
}

func (s *axServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.requests.Add(1)
	if got := r.Header.Get(HeaderTargetActor); got != "space/reviewer" {
		s.t.Errorf("%s %s: %s = %q", r.Method, r.URL.Path, HeaderTargetActor, got)
		http.Error(w, "no actor", http.StatusBadRequest)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/.well-known/agent-card.json":
		card := &sdk.AgentCard{
			Name:        "reviewer",
			Description: "canned",
			Version:     "1",
			// The Task names an address only it can reach. The client
			// must go through the router instead.
			SupportedInterfaces: []*sdk.AgentInterface{sdk.NewAgentInterface("http://reviewer.invalid:9999/a2a/jsonrpc", sdk.TransportProtocolJSONRPC)},
			DefaultInputModes:   []string{"text/plain"},
			DefaultOutputModes:  []string{"text/plain"},
			Skills:              []sdk.AgentSkill{},
		}
		_ = json.NewEncoder(w).Encode(card)
	case r.Method == http.MethodPost && r.URL.Path == "/a2a/jsonrpc":
		var req struct {
			ID     any             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.t.Errorf("decode: %v", err)
			return
		}
		s.mu.Lock()
		s.methods = append(s.methods, req.Method)
		pending := s.pending
		if req.Method == MethodGetTask && s.pending > 0 {
			s.pending--
		}
		s.mu.Unlock()
		task := &sdk.Task{ID: "t-1", ContextID: "c-1", Status: sdk.TaskStatus{State: sdk.TaskStateWorking,
			Message: sdk.NewMessage(sdk.MessageRoleAgent, sdk.NewTextPart("thinking"))}}
		var result any
		switch req.Method {
		case MethodSendMessage:
			var p sdk.SendMessageRequest
			if err := json.Unmarshal(req.Params, &p); err != nil || p.Message.Parts[0].Text() != "review it" {
				s.t.Errorf("params = %s", req.Params)
			}
			result = sdk.StreamResponse{Event: task}
		case MethodGetTask:
			if pending == 0 {
				task.Status = sdk.TaskStatus{State: sdk.TaskStateCompleted}
				task.Artifacts = []*sdk.Artifact{{ID: "a", Parts: sdk.ContentParts{sdk.NewTextPart("canned answer")}}}
			}
			result = task
		default:
			s.t.Errorf("method %s", req.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	default:
		s.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

func TestAskAXPeer(t *testing.T) {
	s := &axServer{t: t, pending: 1}
	srv := httptest.NewServer(s)
	defer srv.Close()
	p := onePeer(t, fw.PeerConfig{Name: "rev", AX: "space/reviewer"}, strings.TrimPrefix(srv.URL, "http://"))
	p.pollEvery = 10 * time.Millisecond
	ctx, progress := progressCtx(t)
	text, isErr := call(t, ctx, p, `{"message":"review it"}`)
	body, id := splitTaskID(t, text)
	if isErr || body != "canned answer" || id != "t-1" {
		t.Fatalf("result = %q", text)
	}
	if got := progress(); len(got) != 1 || got[0] != "thinking" {
		t.Fatalf("progress = %q", got)
	}
	s.mu.Lock()
	methods := strings.Join(s.methods, ",")
	s.mu.Unlock()
	if methods != "SendMessage,GetTask,GetTask" || s.requests.Load() != 4 {
		t.Fatalf("methods = %s, requests = %d", methods, s.requests.Load())
	}
	// The card is cached: a second call does not fetch it again.
	s.mu.Lock()
	s.pending = 0
	s.mu.Unlock()
	call(t, ctx, p, `{"message":"review it"}`)
	if s.requests.Load() != 6 {
		t.Fatalf("requests = %d", s.requests.Load())
	}
}

func TestAskTimeout(t *testing.T) {
	s := &axServer{t: t, pending: 1 << 30}
	srv := httptest.NewServer(s)
	defer srv.Close()
	p := onePeer(t, fw.PeerConfig{Name: "slow", AX: "space/reviewer"}, strings.TrimPrefix(srv.URL, "http://"))
	p.pollEvery, p.timeout = 5*time.Millisecond, 100*time.Millisecond
	ctx, _ := progressCtx(t)
	text, isErr := call(t, ctx, p, `{"message":"review it"}`)
	if !isErr || !strings.Contains(text, "no final answer within 100ms") || !strings.HasSuffix(text, "task_id: t-1") {
		t.Fatalf("result = %q", text)
	}
}

func TestAskErrors(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	p := onePeer(t, fw.PeerConfig{Name: "gone", Card: srv.URL}, "")
	ctx, _ := progressCtx(t)
	if text, isErr := call(t, ctx, p, `{"message":"hi"}`); !isErr || !strings.Contains(text, "resolve the agent card") {
		t.Fatalf("missing card = %q", text)
	}
	if text, isErr := call(t, ctx, p, `{"message":"  "}`); !isErr || !strings.Contains(text, "message is required") {
		t.Fatalf("empty message = %q", text)
	}
	if text, isErr := call(t, ctx, p, `[]`); !isErr || !strings.Contains(text, "invalid input") {
		t.Fatalf("bad input = %q", text)
	}
}

func TestPeersConfig(t *testing.T) {
	for _, c := range []struct {
		name   string
		peers  []fw.PeerConfig
		router string
		want   string
	}{
		{"no name", []fw.PeerConfig{{Card: "http://x"}}, "r", "name is required"},
		{"both", []fw.PeerConfig{{Name: "a", Card: "http://x", AX: "s/t"}}, "r", "exactly one"},
		{"neither", []fw.PeerConfig{{Name: "a"}}, "r", "exactly one"},
		{"bad ax", []fw.PeerConfig{{Name: "a", AX: "nope"}}, "r", "<atespace>/<task>"},
		{"deep ax", []fw.PeerConfig{{Name: "a", AX: "s/t/u"}}, "r", "<atespace>/<task>"},
		{"no router", []fw.PeerConfig{{Name: "a", AX: "s/t"}}, "", "ax.router is empty"},
		{"relative card", []fw.PeerConfig{{Name: "a", Card: "/card.json"}}, "r", "not an absolute URL"},
		{"twice", []fw.PeerConfig{{Name: "a.b", Card: "http://x"}, {Name: "a_b", Card: "http://y"}}, "r", "used twice"},
	} {
		cfg := fw.DefaultConfig()
		cfg.AX.Router = c.router
		cfg.Tools.Peers = c.peers
		if _, err := Peers(cfg); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
	cfg := fw.DefaultConfig()
	cfg.Tools.Peers = []fw.PeerConfig{{Name: "my.peer", Card: "https://peer.example.com"}, {Name: "rev", AX: "default/reviewer"}}
	srcs, err := newSources(context.Background(), cfg, nil)
	if err != nil || len(srcs) != 1 {
		t.Fatalf("sources = %v, %v", srcs, err)
	}
	tools, _ := srcs[0].Tools(context.Background())
	if len(tools) != 2 || tools[0].Spec().Name != "ask_my_peer" || tools[0].Spec().Source != "my.peer" || tools[1].Spec().Name != "ask_rev" {
		t.Fatalf("tools = %v", tools)
	}
	if c, ok := srcs[0].(io.Closer); !ok || c.Close() != nil {
		t.Fatal("the source does not close")
	}
	if srcs, err := newSources(context.Background(), fw.DefaultConfig(), nil); err != nil || len(srcs) != 0 {
		t.Fatalf("no peers = %v, %v", srcs, err)
	}
}
