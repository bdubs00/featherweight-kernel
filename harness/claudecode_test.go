package harness

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// fakeClaude puts the fake claude of testdata in a directory of the test,
// first on PATH, and returns the block harness that runs it.
func fakeClaude(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("the fake claude needs python3")
	}
	src, err := os.ReadFile(filepath.Join("testdata", "fake-claude.py"))
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	p := filepath.Join(bin, "claude")
	if err := os.WriteFile(p, src, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd, _ := json.Marshal([]string{p})
	return "kind: claude-code\ncommand: " + string(cmd) + "\n"
}

// claudeAgent builds an agent of Claude Code that serves its native API on
// a loopback port, as fw serve does.
func claudeAgent(t *testing.T, d testDirs, block string, l fw.Ledger, rules *fw.Rules) *fw.Agent {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, d, block)
	cfg.Model = "anthropic/claude-test"
	cfg.Listen = ln.Addr().String()
	cfg.Surfaces = []string{"native"}
	if rules != nil {
		cfg.Policy.Rules = *rules
	}
	a := newAgent(t, cfg, l)
	serveNative(t, a, ln)
	return a
}

type fakeCall struct {
	Args []string          `json:"args"`
	Env  map[string]string `json:"env"`
	Cwd  string            `json:"cwd"`
}

func fakeCalls(t *testing.T, home string) []fakeCall {
	t.Helper()
	f, err := os.Open(filepath.Join(home, "fake-calls.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []fakeCall
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var c fakeCall
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

func argOf(args []string, name string) string {
	if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

func TestClaudeCodeRun(t *testing.T) {
	d := newDirs(t)
	block := fakeClaude(t)
	l := fw.NewMemLedger()
	a := claudeAgent(t, d, block, l, nil)
	r, err := a.Start(testCtx(t), "Create hello.txt with the text hello", fw.WithRunBudget(fw.Budget{MaxTurns: 7}))
	if err != nil {
		t.Fatal(err)
	}
	r = waitState(t, a, r.ID, terminal)
	if r.State != fw.StateCompleted || r.Output != "Done." {
		t.Fatalf("run = %+v", r)
	}
	if r.Usage.Turns != 2 || r.Usage.InputTokens != 100 || r.Usage.OutputTokens != 20 || r.Usage.ToolCalls != 1 {
		t.Fatalf("usage = %+v", r.Usage)
	}
	evs := events(t, l, r.ID)
	noSecret(t, evs)
	// The two assistant lines of one message are one model call.
	var first fw.ModelRespondedData
	ofType(evs, fw.EvModelResponded)[0].Decode(&first)
	if len(first.Content) != 2 || first.Content[1].Name != "write" || first.StopReason != fw.StopToolUse || first.Usage.InputTokens != 50 {
		t.Fatalf("first response = %+v", first)
	}
	var res fw.ToolRespondedData
	ofType(evs, fw.EvToolResponded)[0].Decode(&res)
	if res.CallID != "toolu_1" || res.IsError || fw.TextOf(res.Content) != "File created" {
		t.Fatalf("result = %+v", res)
	}
	var fc fw.FileChangedData
	if ch := ofType(evs, fw.EvFileChanged); len(ch) != 1 || ch[0].Decode(&fc) != nil || fc.Path != "hello.txt" || fc.Tool != KindClaudeCode {
		t.Fatalf("file events = %v", ch)
	}
	var sid string
	for _, e := range ofType(evs, EvEvent) {
		var d EventData
		e.Decode(&d)
		if d.Kind == KindSession {
			sid = d.Session
		}
	}
	calls := fakeCalls(t, d.home)
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	c := calls[0]
	if sid == "" || argOf(c.Args, "--session-id") != sid || argOf(c.Args, "--model") != "claude-test" || argOf(c.Args, "--max-turns") != "7" ||
		argOf(c.Args, "--input-format") != "stream-json" || argOf(c.Args, "--output-format") != "stream-json" || !slices.Contains(c.Args, "-p") {
		t.Fatalf("args = %q, session %q", c.Args, sid)
	}
	if c.Env["CLAUDE_CONFIG_DIR"] != d.home || c.Env["HOME"] != d.home || c.Env["ANTHROPIC_API_KEY"] != testKey || c.Env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:9" || c.Cwd != d.ws {
		t.Fatalf("env = %+v, cwd = %s", c.Env, c.Cwd)
	}
	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type, URL, OnFailure string
			}
		}
	}
	if err := json.Unmarshal([]byte(argOf(c.Args, "--settings")), &settings); err != nil {
		t.Fatal(err)
	}
	h := settings.Hooks["PreToolUse"][0].Hooks[0]
	if h.Type != "http" || !strings.HasPrefix(h.URL, "http://127.0.0.1:") || !strings.Contains(h.URL, hookPath) || h.OnFailure != "block" {
		t.Fatalf("hook = %+v", h)
	}

	// A follow-up resumes the same session in a new process.
	if err := a.Send(testCtx(t), r.ID, fw.Message{Content: []fw.Block{fw.Text("Say it again")}}, fw.OriginCLI); err != nil {
		t.Fatal(err)
	}
	r = waitState(t, a, r.ID, func(r *fw.Run) bool { return terminal(r) && r.Output == "Again." })
	calls = fakeCalls(t, d.home)
	if len(calls) != 2 || argOf(calls[1].Args, "--resume") != sid {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestClaudeCodeDeny(t *testing.T) {
	d := newDirs(t)
	block := fakeClaude(t)
	l := fw.NewMemLedger()
	a := claudeAgent(t, d, block, l, &fw.Rules{Default: "allow", Deny: []string{"write: file_path=hello.txt"}})
	r, err := a.Start(testCtx(t), "Create hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	r = waitState(t, a, r.ID, terminal)
	if r.State != fw.StateCompleted {
		t.Fatalf("run = %+v", r)
	}
	var res fw.ToolRespondedData
	ofType(events(t, l, r.ID), fw.EvToolResponded)[0].Decode(&res)
	if !res.IsError || !strings.Contains(fw.TextOf(res.Content), "denied by policy") {
		t.Fatalf("result = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(d.ws, "hello.txt")); err == nil {
		t.Fatal("a denied call wrote the file")
	}
}

// TestClaudeCodeDeferAfterColdBoot asks a person about a call: the hook
// defers, claude exits, and the run waits. A new agent, as after a cold
// boot of the Task, takes the decision and resumes the session; the hook
// allows the call from the decision.
func TestClaudeCodeDeferAfterColdBoot(t *testing.T) {
	d := newDirs(t)
	block := fakeClaude(t)
	l := fw.NewMemLedger()
	rules := &fw.Rules{Default: "allow", Ask: []string{"write"}}
	a := claudeAgent(t, d, block, l, rules)
	r, err := a.Start(testCtx(t), "Create hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	r = waitState(t, a, r.ID, inputRequired)
	if r.Pending == nil || r.Pending.Tool != "write" || r.Pending.CallID != "toolu_1" || !strings.Contains(string(r.Pending.Input), "hello.txt") {
		t.Fatalf("pending = %+v", r.Pending)
	}
	if _, err := os.Stat(filepath.Join(d.ws, "hello.txt")); err == nil {
		t.Fatal("a deferred call wrote the file")
	}
	closeAgent(a)

	b := claudeAgent(t, d, block, l, rules)
	if err := b.Decide(testCtx(t), r.ID, fw.ApprovalDecision{ApprovalID: r.Pending.ID, Decision: "allow"}); err != nil {
		t.Fatal(err)
	}
	r = waitState(t, b, r.ID, terminal)
	if r.State != fw.StateCompleted || r.Output != "Done." {
		t.Fatalf("run = %+v", r)
	}
	calls := fakeCalls(t, d.home)
	if len(calls) != 2 || argOf(calls[1].Args, "--resume") != argOf(calls[0].Args, "--session-id") {
		t.Fatalf("calls = %+v", calls)
	}
	// The second process passed the hook again, with a new secret.
	if argOf(calls[1].Args, "--settings") == argOf(calls[0].Args, "--settings") {
		t.Fatal("the resumed process got the hook of the first process")
	}
	decisions := readFile(t, filepath.Join(d.home, "fake-decisions.jsonl"))
	if !strings.Contains(decisions, `"decision": "defer"`) || !strings.Contains(decisions, `"decision": "allow"`) {
		t.Fatalf("decisions = %s", decisions)
	}
	evs := events(t, l, r.ID)
	if n := len(ofType(evs, fw.EvApprovalRequested)); n != 1 {
		t.Fatalf("%d approvals", n)
	}
	if got := readFile(t, filepath.Join(d.ws, "hello.txt")); got != "hello" {
		t.Fatalf("hello.txt = %q", got)
	}
	kinds := strings.Join(harnessKinds(evs), " ")
	if !strings.Contains(kinds, KindAnswer) || strings.Count(kinds, KindIdle) != 2 {
		t.Fatalf("harness events = %s", kinds)
	}
	noSecret(t, evs)
}

func TestClaudeCodeNeedsAnthropic(t *testing.T) {
	d := newDirs(t)
	block := fakeClaude(t)
	cfg := testConfig(t, d, block)
	a := newAgent(t, cfg, fw.NewMemLedger())
	r, err := a.Start(testCtx(t), "hello")
	if err != nil {
		t.Fatal(err)
	}
	r = waitState(t, a, r.ID, terminal)
	if r.State != fw.StateFailed || !strings.Contains(r.Error, "provider anthropic") {
		t.Fatalf("run = %+v", r)
	}
}

// TestClaudeCodeBlankFollowUp sends a blank follow-up: no process starts,
// and the run finishes again at once.
func TestClaudeCodeBlankFollowUp(t *testing.T) {
	d := newDirs(t)
	block := fakeClaude(t)
	l := fw.NewMemLedger()
	a := claudeAgent(t, d, block, l, nil)
	r, err := a.Start(testCtx(t), "Create hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, a, r.ID, terminal)
	if err := a.Send(testCtx(t), r.ID, fw.Message{Content: []fw.Block{fw.Text("\n")}}, fw.OriginCLI); err != nil {
		t.Fatal(err)
	}
	r = waitState(t, a, r.ID, terminal)
	if r.State != fw.StateCompleted || r.Output != "Done." {
		t.Fatalf("run = %+v", r)
	}
	time.Sleep(200 * time.Millisecond)
	n := len(events(t, l, r.ID))
	time.Sleep(200 * time.Millisecond)
	if m := len(events(t, l, r.ID)); m != n {
		t.Fatalf("the run recorded %d events after its end", m-n)
	}
	if calls := fakeCalls(t, d.home); len(calls) != 1 {
		t.Fatalf("%d processes of claude", len(calls))
	}
}

// TestClaudeCodeKillWithFullChannel stops a process at the budget while
// it wrote more lines than the driver holds: the driver drains them, the
// process ends, and the agent closes.
func TestClaudeCodeKillWithFullChannel(t *testing.T) {
	bin := t.TempDir()
	p := filepath.Join(bin, "claude")
	script := `#!/bin/sh
i=0
while [ $i -lt 3000 ]; do
  echo '{"type":"assistant","parent_tool_use_id":null,"message":{"id":"msg_'$i'","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"x"}],"usage":{"input_tokens":1,"output_tokens":1}}}'
  i=$((i+1))
done
exec sleep 30
`
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd, _ := json.Marshal([]string{p})
	d := newDirs(t)
	cfg := testConfig(t, d, "kind: claude-code\ncommand: "+string(cmd)+"\n")
	cfg.Model = "anthropic/claude-test"
	cfg.Budget.MaxTurns = 1
	a := newAgent(t, cfg, fw.NewMemLedger())
	h := a.Runner().(*host)
	h.mounts = 1
	r, err := a.Start(testCtx(t), "hello")
	if err != nil {
		t.Fatal(err)
	}
	r = waitState(t, a, r.ID, terminal)
	if r.State != fw.StateFailed || !strings.Contains(r.Error, "maxTurns") {
		t.Fatalf("run = %+v", r)
	}
	c := h.drv.(*claude)
	waitFor(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.procs) == 0
	})
	done := make(chan struct{})
	go func() { closeAgent(a); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the agent did not close within 15 seconds")
	}
}
