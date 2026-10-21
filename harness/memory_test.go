package harness

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/memory"
)

// oldNote is the text of the note that a test writes before its run. No
// event and no added system text can hold it.
const oldNote = "Old fact about the build\nThe build runs make twice."

// seedNote writes a note in the memory of the profile default, as an
// earlier run did.
func seedNote(t *testing.T, cfg *fw.Config) {
	t.Helper()
	store, err := memory.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := store.Scope(fw.RunInfo{RunID: "earlier"})
	if _, err := store.Remember(context.Background(), scope, fw.MemoryRecord{Ref: memory.NoteRef(scope, "old.md"), Body: oldNote}, ""); err != nil {
		t.Fatal(err)
	}
}

// memAgent builds an agent with the memory tools that serves its native
// API on a loopback port, as fw serve does.
func memAgent(t *testing.T, d testDirs, block string, l fw.Ledger, model string, seed bool, opts ...func(*fw.Config)) (*fw.Agent, net.Listener) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, d, block)
	cfg.Model = model
	cfg.Listen = ln.Addr().String()
	cfg.Surfaces = []string{"native"}
	cfg.Tools.Builtin = []string{"memory", "recall"}
	for _, o := range opts {
		o(cfg)
	}
	if seed {
		seedNote(t, cfg)
	}
	a := newAgent(t, cfg, l)
	serveNative(t, a, ln)
	return a, ln
}

func readJSONL(t *testing.T, p string) []map[string]any {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// memChanges returns the fw.memory.changed events of a run, and fails the
// test when one holds the text of a note.
func memChanges(t *testing.T, evs []fw.Event, texts ...string) []fw.MemoryChangedData {
	t.Helper()
	var out []fw.MemoryChangedData
	for _, e := range ofType(evs, fw.EvMemoryChanged) {
		for _, text := range texts {
			if strings.Contains(string(e.Data), text) {
				t.Fatalf("fw.memory.changed holds the text of a note: %s", e.Data)
			}
		}
		var d fw.MemoryChangedData
		if err := e.Decode(&d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

// checkSystem checks the added system text of a harness: the instructions,
// the memory section, the names of the tools, and the sentence about the
// context blocks, and never a note.
func checkSystem(t *testing.T, sys, instructions string) {
	t.Helper()
	want := memory.SystemSection
	if instructions != "" {
		want = instructions + "\n\n" + want
	}
	if !strings.HasPrefix(sys, want) || !strings.Contains(sys, memToolsNote) ||
		!strings.HasSuffix(sys, fw.ContextNote) {
		t.Fatalf("the added system text = %q", sys)
	}
	if strings.Contains(sys, "Old fact") || strings.Contains(sys, "old.md") || strings.Contains(sys, memory.ContextList) {
		t.Fatalf("the added system text holds a note: %q", sys)
	}
}

// checkFirstPrompt checks that the first prompt starts with the index of
// the notes as a context block and ends with the input of the run.
func checkFirstPrompt(t *testing.T, text, input string) {
	t.Helper()
	if !strings.HasPrefix(text, `<context source="memory">`+"\n"+memory.ContextList+"\n- /memories/old.md: Old fact about the build\n</context>\n\n") ||
		!strings.HasSuffix(text, input) {
		t.Fatalf("the first prompt = %q", text)
	}
}

func TestClaudeCodeMemory(t *testing.T) {
	d := newDirs(t)
	block := fakeClaude(t)
	l := fw.NewMemLedger()
	a, ln := memAgent(t, d, block, l, "anthropic/claude-test", true)
	input := "Please remember that I like tea"
	r, err := a.Start(testCtx(t), input, fw.WithInstructions("Be brief."))
	if err != nil {
		t.Fatal(err)
	}
	r = waitState(t, a, r.ID, terminal)
	if r.State != fw.StateCompleted || r.Output != "Remembered." {
		t.Fatalf("run = %+v", r)
	}
	calls := fakeCalls(t, d.home)
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	args := calls[0].Args
	checkSystem(t, argOf(args, "--append-system-prompt"), "Be brief.")
	// The memory server is a server of the type http on the native API,
	// with the secret of the hook of the process.
	var conf struct {
		MCPServers map[string]struct{ Type, URL string } `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(argOf(args, "--mcp-config")), &conf); err != nil {
		t.Fatalf("--mcp-config: %v", err)
	}
	srv := conf.MCPServers[memServer]
	_, key, _ := strings.Cut(srv.URL, memPath)
	if len(conf.MCPServers) != 1 || srv.Type != "http" || srv.URL != "http://"+ln.Addr().String()+memPath+key || len(key) != 64 ||
		!strings.Contains(argOf(args, "--settings"), hookPath+key) {
		t.Fatalf("--mcp-config = %s", argOf(args, "--mcp-config"))
	}
	if slices.Contains(args, "--strict-mcp-config") {
		t.Fatal("the process left out the MCP servers of the repository")
	}
	prompts := readJSONL(t, filepath.Join(d.home, "fake-prompts.jsonl"))
	checkFirstPrompt(t, prompts[0]["text"].(string), input)

	// The write went through the memory server in the scope of the run,
	// and the ledger of the run holds its record without the text.
	evs := events(t, l, r.ID)
	noSecret(t, evs)
	ch := memChanges(t, evs, "tea", key)
	if len(ch) != 1 || ch[0].Tool != memRemember || ch[0].CallID != "toolu_m" || ch[0].Op != fw.FileCreated ||
		ch[0].Ref != "mem://agent/default/prefs.md" || ch[0].Size != 19 || ch[0].SHA256 != sha("The user likes tea.") {
		t.Fatalf("memory changes = %+v", ch)
	}
	var req fw.ToolRequestedData
	ofType(evs, fw.EvToolRequested)[0].Decode(&req)
	var res fw.ToolRespondedData
	ofType(evs, fw.EvToolResponded)[0].Decode(&res)
	if req.Tool != "mcp__fw-memory__remember" || res.IsError || !strings.HasPrefix(fw.TextOf(res.Content), "Saved /memories/prefs.md: 19 bytes") {
		t.Fatalf("tool call = %+v, result = %+v", req, res)
	}
	var first EventData
	for _, e := range ofType(evs, EvEvent) {
		var d EventData
		e.Decode(&d)
		if d.Kind == KindPrompt {
			first = d
			break
		}
	}
	if !first.Context {
		t.Fatalf("the first fw.prompt = %+v", first)
	}
	h := a.Runner().(*host)
	scope := h.mem.Scope(fw.RunInfo{RunID: r.ID})
	if rec, err := h.mem.Get(testCtx(t), scope, memory.NoteRef(scope, "prefs.md")); err != nil || rec.Body != "The user likes tea." {
		t.Fatalf("the note = %+v, %v", rec, err)
	}
	// The memory server ended with its process, and no event holds its
	// secret.
	h.mu.Lock()
	left := len(h.memBinds)
	h.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d memory servers after the process", left)
	}
	for _, e := range evs {
		if strings.Contains(string(e.Data), key) {
			t.Fatalf("the event %s holds the secret of the memory server", e.Type)
		}
	}

	// A follow-up resumes the session without the index of the notes, with
	// a new secret.
	if err := a.Send(testCtx(t), r.ID, fw.Message{Content: []fw.Block{fw.Text("Say it again")}}, fw.OriginCLI); err != nil {
		t.Fatal(err)
	}
	waitState(t, a, r.ID, func(r *fw.Run) bool { return terminal(r) && r.Output == "Again." })
	prompts = readJSONL(t, filepath.Join(d.home, "fake-prompts.jsonl"))
	if len(prompts) != 2 || prompts[1]["text"] != "Say it again" {
		t.Fatalf("prompts = %+v", prompts)
	}
	calls = fakeCalls(t, d.home)
	if len(calls) != 2 || argOf(calls[1].Args, "--mcp-config") == argOf(args, "--mcp-config") {
		t.Fatalf("calls = %+v", calls)
	}
	checkSystem(t, argOf(calls[1].Args, "--append-system-prompt"), "Be brief.")
}

// TestClaudeCodeMemoryDenied runs a profile without the memory tools: the
// process gets no memory server and no memory section, and the hook
// denies a call of the memory server.
func TestClaudeCodeMemoryDenied(t *testing.T) {
	d := newDirs(t)
	block := fakeClaude(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, d, block)
	cfg.Model = "anthropic/claude-test"
	cfg.Listen = ln.Addr().String()
	cfg.Surfaces = []string{"native"}
	cfg.Tools.Builtin = []string{"memory", "recall"}
	cfg.Agents = map[string]fw.AgentProfile{"default": {Tools: []string{"bash", "files"}}}
	seedNote(t, cfg)
	l := fw.NewMemLedger()
	a := newAgent(t, cfg, l)
	serveNative(t, a, ln)
	r, err := a.Start(testCtx(t), "Please remember that I like tea")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, a, r.ID, terminal)
	args := fakeCalls(t, d.home)[0].Args
	if slices.Contains(args, "--mcp-config") || slices.Contains(args, "--append-system-prompt") {
		t.Fatalf("args = %q", args)
	}
	if p := readJSONL(t, filepath.Join(d.home, "fake-prompts.jsonl")); p[0]["text"] != "Please remember that I like tea" {
		t.Fatalf("prompts = %+v", p)
	}
	decisions := readFile(t, filepath.Join(d.home, "fake-decisions.jsonl"))
	if !strings.Contains(decisions, `"decision": "deny"`) || !strings.Contains(decisions, "does not allow the memory tool") {
		t.Fatalf("decisions = %s", decisions)
	}
	if n := len(ofType(events(t, l, r.ID), fw.EvMemoryChanged)); n != 0 {
		t.Fatalf("%d memory changes", n)
	}
}

// ocMemoryScript is the script of a session that calls the remember tool
// of the memory server: opencode asks for the permission of the tool, and
// after the answer once it calls the memory server that its configuration
// names, as an MCP client of the revision 2025-06-18.
func ocMemoryScript(envFile string) func(f *fakeOC, p fakePrompt) {
	return func(f *fakeOC, p fakePrompt) {
		sid := p.Session
		f.status(sid, "busy")
		call := f.nextCall()
		m1, m2 := "msg_a"+call, "msg_b"+call
		f.assistant(sid, m1)
		f.part(sid, map[string]any{"id": "prt_s1", "messageID": m1, "type": "step-start"})
		input := map[string]any{"path": "prefs.md", "text": "The user likes tea."}
		tool := map[string]any{"id": "prt_" + call, "messageID": m1, "type": "tool", "callID": call, "tool": "fw-memory_remember",
			"state": map[string]any{"status": "running", "input": input}}
		f.part(sid, tool)
		rep := f.ask(sid, "fw-memory_remember", m1, call)
		if rep.lost {
			return
		}
		if rep.Reply == "once" {
			text, isErr, err := ocCallMemory(envFile, "remember", input)
			switch {
			case err != nil:
				tool["state"] = map[string]any{"status": "error", "input": input, "error": err.Error()}
			case isErr:
				tool["state"] = map[string]any{"status": "error", "input": input, "error": text}
			default:
				tool["state"] = map[string]any{"status": "completed", "input": input, "output": text}
			}
		} else {
			tool["state"] = map[string]any{"status": "error", "input": input, "error": "rejected: " + rep.Message}
		}
		f.part(sid, tool)
		f.part(sid, map[string]any{"id": "prt_f1", "messageID": m1, "type": "step-finish", "reason": "tool-calls", "cost": 0, "tokens": tokens(10, 2)})
		f.assistant(sid, m2)
		f.part(sid, map[string]any{"id": "prt_s2", "messageID": m2, "type": "step-start"})
		f.part(sid, map[string]any{"id": "prt_t2", "messageID": m2, "type": "text", "text": "Remembered."})
		f.part(sid, map[string]any{"id": "prt_f2", "messageID": m2, "type": "step-finish", "reason": "stop", "cost": 0, "tokens": tokens(10, 2)})
		f.status(sid, "idle")
	}
}

// ocConfigOf returns the inline configuration of opencode from the
// environment that the fake command wrote.
func ocConfigOf(envFile string) (map[string]any, error) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, err := os.ReadFile(envFile)
		if err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if v, ok := strings.CutPrefix(line, "OPENCODE_CONFIG_CONTENT="); ok {
					var conf map[string]any
					return conf, json.Unmarshal([]byte(v), &conf)
				}
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("no configuration in %s", envFile)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ocCallMemory calls one tool of the memory server of the configuration.
func ocCallMemory(envFile, tool string, input map[string]any) (string, bool, error) {
	conf, err := ocConfigOf(envFile)
	if err != nil {
		return "", false, err
	}
	srv, _ := conf["mcp"].(map[string]any)[memServer].(map[string]any)
	url, _ := srv["url"].(string)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "fake-opencode", Version: "1"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url, DisableStandaloneSSE: true, MaxRetries: -1},
		&mcp.ClientSessionOptions{ProtocolVersion: "2025-06-18"})
	if err != nil {
		return "", false, err
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: input})
	if err != nil {
		return "", false, err
	}
	return resultText2(res), res.IsError, nil
}

func resultText2(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func TestOpencodeMemory(t *testing.T) {
	d := newDirs(t)
	envFile := filepath.Join(d.root, "oc-env")
	f := newFakeOC(t, ocMemoryScript(envFile))
	l := fw.NewMemLedger()
	a, ln := memAgent(t, d, ocBlock(d, f), l, "openai/test-model", true)
	input := "Please remember that I like tea"
	r, err := a.Start(testCtx(t), input, fw.WithInstructions("Be brief."))
	if err != nil {
		t.Fatal(err)
	}
	r = waitState(t, a, r.ID, terminal)
	if r.State != fw.StateCompleted || r.Output != "Remembered." {
		t.Fatalf("run = %+v", r)
	}
	// The inline configuration holds the memory server and asks the kernel
	// about its tools.
	conf, err := ocConfigOf(envFile)
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := conf["mcp"].(map[string]any)[memServer].(map[string]any)
	url, _ := srv["url"].(string)
	_, key, _ := strings.Cut(url, memPath)
	if srv["type"] != "remote" || srv["enabled"] != true || srv["oauth"] != false || url != "http://"+ln.Addr().String()+memPath+key || len(key) != 64 {
		t.Fatalf("the memory server of opencode = %v", srv)
	}
	if perm, _ := conf["permission"].(map[string]any); perm["fw-memory_remember"] != "ask" || perm["fw-memory_recall"] != "ask" {
		t.Fatalf("permission = %v", perm)
	}
	if a.Redact("key "+key) != "key "+fw.Redacted {
		t.Fatal("the redactor does not know the secret of the memory server")
	}
	f.mu.Lock()
	first := f.prompts[0]
	f.mu.Unlock()
	checkSystem(t, first.System, "Be brief.")
	checkFirstPrompt(t, first.Text, input)

	evs := events(t, l, r.ID)
	noSecret(t, evs)
	var call string
	for _, e := range ofType(evs, fw.EvToolRequested) {
		var req fw.ToolRequestedData
		e.Decode(&req)
		if req.Tool == "fw-memory_remember" {
			call = req.CallID
		}
	}
	ch := memChanges(t, evs, "tea", key)
	if call == "" || len(ch) != 1 || ch[0].Tool != memRemember || ch[0].CallID != call || ch[0].Op != fw.FileCreated ||
		ch[0].Ref != "mem://agent/default/prefs.md" || ch[0].SHA256 != sha("The user likes tea.") {
		t.Fatalf("call %q, memory changes = %+v", call, ch)
	}
	var res fw.ToolRespondedData
	ofType(evs, fw.EvToolResponded)[0].Decode(&res)
	if res.IsError || !strings.HasPrefix(fw.TextOf(res.Content), "Saved /memories/prefs.md: 19 bytes") {
		t.Fatalf("result = %+v", res)
	}

	// A follow-up goes to the same session without the index of the notes,
	// with the same added system text.
	f.script = answerOnly
	if err := a.Send(testCtx(t), r.ID, fw.Message{Content: []fw.Block{fw.Text("Say it again")}}, fw.OriginCLI); err != nil {
		t.Fatal(err)
	}
	waitState(t, a, r.ID, func(r *fw.Run) bool { return terminal(r) && r.Output == "Again." })
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.prompts) != 2 || f.prompts[1].Text != "Say it again" || f.prompts[1].System != first.System {
		t.Fatalf("prompts = %+v", f.prompts)
	}
}

// TestOpencodeMemoryAfterColdBoot stops the agent while a call of the
// first exchange waits for a person, as a suspend of the Task does. The
// session after the cold boot gets the note of the restart without the
// index of the notes, which the first prompt carried already.
func TestOpencodeMemoryAfterColdBoot(t *testing.T) {
	d := newDirs(t)
	f := newFakeOC(t, writeHello)
	block := ocBlock(d, f)
	l := fw.NewMemLedger()
	ask := func(cfg *fw.Config) { cfg.Policy.Rules = fw.Rules{Default: "allow", Ask: []string{"write"}} }
	a, _ := memAgent(t, d, block, l, "openai/test-model", true, ask)
	input := "Create hello.txt"
	r, err := a.Start(testCtx(t), input)
	if err != nil {
		t.Fatal(err)
	}
	r = waitState(t, a, r.ID, inputRequired)
	closeAgent(a)
	f.restart()

	b, _ := memAgent(t, d, block, l, "openai/test-model", false, ask)
	if err := b.Decide(testCtx(t), r.ID, fw.ApprovalDecision{ApprovalID: r.Pending.ID, Decision: "allow"}); err != nil {
		t.Fatal(err)
	}
	r = waitState(t, b, r.ID, terminal)
	if r.State != fw.StateCompleted || r.Output != "Done." {
		t.Fatalf("run = %+v", r)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.prompts) != 2 || !strings.Contains(f.prompts[1].Text, "stopped and started again") ||
		strings.Contains(f.prompts[1].Text, memory.ContextList) || strings.Contains(f.prompts[1].Text, "Old fact") {
		t.Fatalf("prompts = %+v", f.prompts)
	}
	checkFirstPrompt(t, f.prompts[0].Text, input)
	checkSystem(t, f.prompts[1].System, "")
	var marks []bool
	for _, e := range ofType(events(t, l, r.ID), EvEvent) {
		var d EventData
		e.Decode(&d)
		if d.Kind == KindPrompt {
			marks = append(marks, d.Context)
		}
	}
	if !slices.Equal(marks, []bool{true, false}) {
		t.Fatalf("the context of the fw.prompt events = %v", marks)
	}
}

// TestOpencodeMemoryOff runs opencode without the memory tools: the
// configuration has no memory server, and the prompt and the system text
// are as before.
func TestOpencodeMemoryOff(t *testing.T) {
	d := newDirs(t)
	f := newFakeOC(t, answerOnly)
	cfg := testConfig(t, d, ocBlock(d, f))
	cfg.Surfaces = []string{"native"}
	cfg.Listen = "127.0.0.1:9"
	a := newAgent(t, cfg, fw.NewMemLedger())
	r, err := a.Start(testCtx(t), "hello", fw.WithInstructions("Be brief."))
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, a, r.ID, terminal)
	conf, err := ocConfigOf(filepath.Join(d.root, "oc-env"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := conf["mcp"]; ok {
		t.Fatalf("mcp = %v", conf["mcp"])
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.prompts[0].Text != "hello" || f.prompts[0].System != "Be brief." {
		t.Fatalf("prompts = %+v", f.prompts)
	}
}

// testRun returns a run of the memory server in the scope of an agent
// profile, whose changes go to *changes.
func testRun(h *host, id, agent string, mu *sync.Mutex, changes *[]fw.MemoryChangedData) *memRun {
	info := fw.RunInfo{RunID: id, Agent: agent}
	return &memRun{id: id, info: info, scope: h.mem.Scope(info), notes: true, recall: true,
		emit: func(_ context.Context, d fw.MemoryChangedData) {
			mu.Lock()
			*changes = append(*changes, d)
			mu.Unlock()
		}}
}

// TestMemoryRoute drives the memory route with an MCP client: it lists the
// tools, makes a round trip of remember and recall in the scope of a run,
// keeps the notes of another profile apart, records each change without
// the text, and refuses a call that no run allowed, a secret that no
// process holds, and a caller outside the loopback address.
func TestMemoryRoute(t *testing.T) {
	d := newDirs(t)
	a, ln := memAgent(t, d, "kind: claude-code", fw.NewMemLedger(), "anthropic/claude-test", false)
	h := a.Runner().(*host)
	key := secret()
	b := h.bindMemory(key, false)
	url := "http://" + ln.Addr().String() + memPath + key
	var mu sync.Mutex
	var changes []fw.MemoryChangedData
	runA := testRun(h, "run-a", "", &mu, &changes)
	runB := testRun(h, "run-b", "reviewer", &mu, &changes)
	ctx := testCtx(t)

	for _, version := range []string{"", "2025-06-18"} {
		var opts *mcp.ClientSessionOptions
		if version != "" {
			opts = &mcp.ClientSessionOptions{ProtocolVersion: version}
		}
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).
			Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url, DisableStandaloneSSE: true, MaxRetries: -1}, opts)
		if err != nil {
			t.Fatalf("connect %q: %v", version, err)
		}
		list, err := cs.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, tool := range list.Tools {
			names = append(names, tool.Name)
			if ro := tool.Annotations != nil && tool.Annotations.ReadOnlyHint; ro != (tool.Name == memRecall || tool.Name == memList) {
				t.Errorf("%s: read-only = %v", tool.Name, ro)
			}
		}
		slices.Sort(names)
		if !slices.Equal(names, []string{memForget, memList, memRecall, memRemember}) {
			t.Fatalf("tools = %v", names)
		}
		cs.Close()
	}

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	callTool := func(run *memRun, armed bool, tool string, input map[string]any) (string, bool) {
		t.Helper()
		if armed {
			raw, _ := json.Marshal(input)
			if _, err := b.arm(ctx, &memArm{tool: tool, input: raw, callID: "call_" + tool, run: run}); err != nil {
				t.Fatal(err)
			}
		}
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: input})
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		return resultText2(res), res.IsError
	}

	if text, isErr := callTool(runA, false, memList, map[string]any{}); !isErr || !strings.Contains(text, "no tool call of a run of fw allowed") {
		t.Fatalf("a call that no run allowed: %q", text)
	}
	body := "Tea, not coffee.\nThe user said so twice."
	if text, isErr := callTool(runA, true, memRemember, map[string]any{"path": "/memories/prefs.md", "text": body}); isErr ||
		text != fmt.Sprintf("Saved /memories/prefs.md: %d bytes, SHA-256 %s.", len(body), sha(body)) {
		t.Fatalf("remember: %q", text)
	}
	if text, isErr := callTool(runA, true, memRecall, map[string]any{"query": "coffee"}); isErr || !strings.Contains(text, "[note] mem://agent/default/prefs.md") {
		t.Fatalf("recall a query: %q", text)
	}
	if text, isErr := callTool(runA, true, memRecall, map[string]any{"ref": "/memories/prefs.md"}); isErr || !strings.Contains(text, body) ||
		!strings.Contains(text, "SHA-256: "+sha(body)) {
		t.Fatalf("recall a note: %q", text)
	}
	if text, isErr := callTool(runA, true, memList, map[string]any{}); isErr || !strings.Contains(text, "- /memories/prefs.md: Tea, not coffee.") {
		t.Fatalf("list: %q", text)
	}
	if text, isErr := callTool(runA, true, memRemember, map[string]any{"path": "prefs.md", "text": "x", "ifHash": "absent"}); !isErr || !strings.Contains(text, "exists already") {
		t.Fatalf("remember over a note with absent: %q", text)
	}
	if text, isErr := callTool(runA, true, memRemember, map[string]any{"path": ".hidden/x.md", "text": "x"}); !isErr || !strings.Contains(text, "hidden") {
		t.Fatalf("remember a hidden note: %q", text)
	}
	// The run of another profile sees none of the notes of run A.
	if text, isErr := callTool(runB, true, memList, map[string]any{}); isErr || text != "No notes under /memories." {
		t.Fatalf("list of another profile: %q", text)
	}
	if text, isErr := callTool(runB, true, memRecall, map[string]any{"ref": "mem://agent/default/prefs.md"}); !isErr || !strings.Contains(text, "outside the memory of this run") {
		t.Fatalf("recall of another profile: %q", text)
	}
	if text, isErr := callTool(runA, true, memForget, map[string]any{"path": "prefs.md"}); isErr || text != "Deleted /memories/prefs.md." {
		t.Fatalf("forget: %q", text)
	}
	mu.Lock()
	got := append([]fw.MemoryChangedData(nil), changes...)
	mu.Unlock()
	if len(got) != 2 || got[0] != (fw.MemoryChangedData{CallID: "call_remember", Tool: memRemember, Op: fw.FileCreated, Ref: "mem://agent/default/prefs.md",
		Scope: fw.MemoryAgentScope, Agent: "default", Size: int64(len(body)), SHA256: sha(body)}) ||
		got[1] != (fw.MemoryChangedData{CallID: "call_forget", Tool: memForget, Op: fw.FileRemoved, Ref: "mem://agent/default/prefs.md", Scope: fw.MemoryAgentScope, Agent: "default"}) {
		t.Fatalf("changes = %+v", got)
	}

	// The server is stateless: it opens no stream for GET.
	greq, _ := http.NewRequest(http.MethodGet, url, nil)
	greq.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(greq)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", resp.StatusCode)
	}
	// A secret that no process holds gets 404.
	resp, err = http.Post("http://"+ln.Addr().String()+memPath+secret(), "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("an unknown secret: %d", resp.StatusCode)
	}
	// A caller outside the loopback address gets 404, also with a secret
	// that a process holds.
	req := httptest.NewRequest(http.MethodPost, memPath+key, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.RemoteAddr = "10.1.2.3:4000"
	req.SetPathValue("secret", key)
	w := httptest.NewRecorder()
	h.memRoute(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("a remote caller: %d", w.Code)
	}
	// The secret of a process that ended gets 404.
	h.unbindMemory(key)
	resp, err = http.Post(url, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("the secret of a process that ended: %d", resp.StatusCode)
	}
}

// TestMemoryArms checks that a call finds the run that armed it, when the
// runs of one opencode serve arm calls at once.
func TestMemoryArms(t *testing.T) {
	ctx := testCtx(t)
	b := &memBinding{freed: make(chan struct{})}
	ra, rb := &memRun{id: "a"}, &memRun{id: "b"}
	x, y, z := json.RawMessage(`{"path":"x.md","text":"x"}`), json.RawMessage(`{"text":"y","path":"y.md"}`), json.RawMessage(`{"path":"z.md","text":"z"}`)
	arm := func(run *memRun, input json.RawMessage) {
		t.Helper()
		if _, err := b.arm(ctx, &memArm{tool: memRemember, input: input, run: run}); err != nil {
			t.Fatal(err)
		}
	}
	// Two runs with two inputs: each call finds its run, whatever the order
	// of the keys.
	arm(ra, x)
	arm(rb, y)
	if got := b.take(memRemember, json.RawMessage(`{"path":"y.md","text":"y"}`)); got == nil || got.run != rb {
		t.Fatalf("take y = %+v", got)
	}
	// A call that matches no input can belong to either run.
	arm(rb, y)
	if got := b.take(memRemember, z); got != nil {
		t.Fatalf("take z = %+v", got)
	}
	if got := b.take(memList, json.RawMessage(`{}`)); got != nil {
		t.Fatalf("take list = %+v", got)
	}
	b.take(memRemember, x)
	// A process that serves many runs never takes a call whose input
	// differs from the input that the policy saw, also with the calls of
	// one run alone.
	if got := b.take(memRemember, z); got != nil {
		t.Fatalf("take z of one run = %+v", got)
	}
	b.take(memRemember, y)
	// A process that serves one run also takes only the input that the
	// policy saw, so a process that read the secret cannot make an allowed
	// call with other arguments.
	one := &memBinding{single: true, freed: make(chan struct{})}
	for _, in := range []json.RawMessage{x, y} {
		if _, err := one.arm(ctx, &memArm{tool: memRemember, input: in, run: ra}); err != nil {
			t.Fatal(err)
		}
	}
	if got := one.take(memRemember, z); got != nil {
		t.Fatalf("take z of a process of one run = %+v", got)
	}
	if got := one.take(memRemember, y); got == nil || !sameJSON(got.input, y) {
		t.Fatalf("take y of a process of one run = %+v", got)
	}
	// The same input from two runs: the second run waits until the server
	// took the call of the first.
	arm(ra, x)
	done := make(chan struct{})
	go func() {
		arm(rb, x)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("two runs armed the same call at once")
	case <-time.After(100 * time.Millisecond):
	}
	if got := b.take(memRemember, x); got == nil || got.run != ra {
		t.Fatalf("take x = %+v", got)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the second run still waits")
	}
	if got := b.take(memRemember, x); got == nil || got.run != rb {
		t.Fatalf("take x of the second run = %+v", got)
	}
	// An input that the host did not see clashes with any input of
	// another run.
	if _, err := b.arm(ctx, &memArm{tool: memRemember, run: ra}); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := b.arm(short, &memArm{tool: memRemember, input: y, run: rb}); err == nil {
		t.Fatal("a run armed a call beside a call of another run with an input that the host did not see")
	}
	if got := b.take(memRemember, z); got == nil || got.run != ra {
		t.Fatalf("take of an unseen input = %+v", got)
	}
	// An armed call expires.
	stale, err := b.arm(ctx, &memArm{tool: memForget, input: x, run: ra})
	if err != nil {
		t.Fatal(err)
	}
	_ = stale
	b.mu.Lock()
	b.arms[0].expires = time.Now().Add(-time.Second)
	b.mu.Unlock()
	if got := b.take(memForget, x); got != nil {
		t.Fatalf("take of an expired call = %+v", got)
	}
}

// TestMemoryToolNames checks the names of the tools of the memory server
// as each kind names them for the policy, and their flags.
func TestMemoryToolNames(t *testing.T) {
	for _, c := range []struct {
		kind, name, tool string
		readOnly         bool
	}{
		{KindClaudeCode, "mcp__fw-memory__recall", memRecall, true},
		{KindClaudeCode, "mcp__fw-memory__list", memList, true},
		{KindClaudeCode, "mcp__fw-memory__remember", memRemember, false},
		{KindClaudeCode, "mcp__fw-memory__forget", memForget, false},
		{KindOpencode, "fw-memory_recall", memRecall, true},
		{KindOpencode, "fw-memory_list", memList, true},
		{KindOpencode, "fw-memory_remember", memRemember, false},
		{KindOpencode, "fw-memory_forget", memForget, false},
		{KindOpencode, "mcp__fw-memory__recall", "", false},
		{KindClaudeCode, "fw-memory_recall", "", false},
		{KindOpencode, "fw-memory_view", "", false},
	} {
		if got := memToolOf(c.kind, c.name); got != c.tool {
			t.Errorf("%s %s: tool %q, want %q", c.kind, c.name, got, c.tool)
		}
		if c.tool == "" {
			continue
		}
		if s := toolSpec(c.name); s.ReadOnly != c.readOnly || s.Destructive {
			t.Errorf("%s: spec %+v", c.name, s)
		}
	}
}
