package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/config"
	"github.com/bdubs00/featherweight-kernel/fw"

	// The binary embeds the zone database. The tests embed it too, so the
	// zone names resolve on a host without zone files.
	_ "time/tzdata"
)

// syncBuffer is a buffer that background goroutines can write to while the
// test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// scriptModel returns canned responses in order and records every request.
type scriptModel struct {
	mu    sync.Mutex
	resps []*fw.Response
	reqs  []*fw.Request
}

func (s *scriptModel) Generate(ctx context.Context, req *fw.Request) (*fw.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, req)
	if len(s.resps) == 0 {
		return nil, errors.New("script: no response left")
	}
	r := s.resps[0]
	s.resps = s.resps[1:]
	return r, nil
}

func (s *scriptModel) Stream(ctx context.Context, req *fw.Request, _ func(fw.Delta)) (*fw.Response, error) {
	return s.Generate(ctx, req)
}

func (s *scriptModel) requests() []*fw.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.reqs)
}

func endTurn(text string) *fw.Response {
	return &fw.Response{Content: []fw.Block{fw.Text(text)}, StopReason: fw.StopEndTurn, Provider: "fake", Model: "script", Usage: fw.Usage{InputTokens: 10, OutputTokens: 5}}
}

func callTool(name, input string) *fw.Response {
	return &fw.Response{
		Content:    []fw.Block{fw.Text("calling"), {Type: fw.BlockToolUse, ID: "call-1", Name: name, Input: json.RawMessage(input)}},
		StopReason: fw.StopToolUse, Provider: "fake", Model: "script",
	}
}

// toolResults returns the text of every tool result in a request.
func toolResults(req *fw.Request) string {
	var sb strings.Builder
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == fw.BlockToolResult {
				sb.WriteString(fw.TextOf(b.Content))
				if b.IsError {
					sb.WriteString(" [error]")
				}
			}
		}
	}
	return sb.String()
}

// countingTool returns a tool that counts its calls.
func countingTool(name string, destructive bool, calls *atomic.Int32) fw.Tool {
	spec := fw.ToolSpec{
		Name:        name,
		Description: "Changes " + name + ".\nA second line.",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Destructive: destructive,
		ReadOnly:    !destructive,
		Source:      "test",
	}
	return fw.NewTool(spec, func(context.Context, json.RawMessage) (fw.ToolResult, error) {
		calls.Add(1)
		return fw.TextResult(name + " done"), nil
	})
}

// env prepares a clean environment and a temporary working directory and
// home, and makes every agent use the options.
func env(t *testing.T, opts ...fw.Option) string {
	t.Helper()
	for _, n := range config.EnvNames() {
		t.Setenv(n, "")
	}
	for _, n := range []string{"FW_LOG", "FW_CONFIG", "FW_AGENT", "FW_GOAL", "AX_TASK_YAML", "AX_WORKSPACES_YAML", "AX_BOOTSTRAP_TIMEOUT"} {
		t.Setenv(n, "")
	}
	t.Setenv("FW_ATESPACE", "test")
	// A scripted model answers in order, so the runs of these tests make
	// no title call.
	t.Chdir(t.TempDir())
	home := t.TempDir()
	t.Setenv("FW_HOME", home)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	AgentOptions = func() []fw.Option { return append([]fw.Option{fw.WithLogger(quiet)}, opts...) }
	prevDefault := slog.Default()
	t.Cleanup(func() {
		AgentOptions = nil
		slog.SetDefault(prevDefault)
	})
	return home
}

// run runs the command line and returns the exit code and the streams.
func run(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	return runCtx(context.Background(), t, stdin, args...)
}

func runCtx(ctx context.Context, t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr syncBuffer
	code := Run(ctx, args, strings.NewReader(stdin), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func setTerminal(t *testing.T, on bool) {
	t.Helper()
	prev := isTerminal
	isTerminal = func(io.Reader) bool { return on }
	t.Cleanup(func() { isTerminal = prev })
}

func TestRunPrintsOutputAndSummaries(t *testing.T) {
	m := &scriptModel{resps: []*fw.Response{endTurn("hello world")}}
	env(t, fw.WithModel(m))
	code, out, errOut := run(t, "", "run", "say", "hi")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if out != "hello world\n" {
		t.Errorf("stdout %q", out)
	}
	for _, want := range []string{`fw.run.queued title="say hi" agent=default origin=cli`, "fw.model.requested", "fw.model.responded model=fake/script latency=", "fw.run.finished outcome=completed"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut)
		}
	}
	if reqs := m.requests(); len(reqs) != 1 || fw.TextOf(reqs[0].Messages[0].Content) != "say hi" {
		t.Errorf("model requests: %+v", reqs)
	}
}

func TestRunJSONAndAgentFlag(t *testing.T) {
	m := &scriptModel{resps: []*fw.Response{endTurn("ok")}}
	env(t, fw.WithModel(m), fw.WithProfile(fw.AgentProfile{Name: "writer", System: "You write."}))
	code, out, errOut := run(t, "", "run", "--json", "write it", "--agent", "writer")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	var r fw.Run
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("stdout is not a run: %v\n%s", err, out)
	}
	if r.State != fw.StateCompleted || r.Output != "ok" || r.Agent != "writer" || r.Origin != fw.OriginCLI || r.Title != "write it" {
		t.Errorf("run %+v", r)
	}
	if reqs := m.requests(); len(reqs) != 1 || reqs[0].System != "You write." {
		t.Errorf("the profile was not used: %+v", reqs)
	}
}

func TestRunInputFromStdin(t *testing.T) {
	m := &scriptModel{resps: []*fw.Response{endTurn("ok")}}
	env(t, fw.WithModel(m))
	if code, _, errOut := run(t, "from stdin\n", "run", "-"); code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if got := fw.TextOf(m.requests()[0].Messages[0].Content); got != "from stdin\n" {
		t.Errorf("input %q", got)
	}
}

func TestRunFailedExits1(t *testing.T) {
	m := fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
		return nil, errors.New("provider down")
	}}
	env(t, fw.WithModel(m))
	code, _, errOut := run(t, "", "run", "hello")
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, errOut)
	}
	if !strings.Contains(errOut, "outcome=failed") {
		t.Errorf("stderr:\n%s", errOut)
	}
}

func TestRunCanceledExits2(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := fw.ModelFunc{GenerateFn: func(mctx context.Context, _ *fw.Request) (*fw.Response, error) {
		cancel() // the user presses Ctrl-C during the model call
		<-mctx.Done()
		return nil, mctx.Err()
	}}
	env(t, fw.WithModel(m))
	code, _, errOut := runCtx(ctx, t, "", "run", "hello")
	if code != 2 {
		t.Fatalf("exit %d, want 2\n%s", code, errOut)
	}
}

func TestRunUsageErrors(t *testing.T) {
	env(t)
	if code, _, _ := run(t, "", "run"); code != exitUsage {
		t.Errorf("run without input: exit %d", code)
	}
	if code, _, _ := run(t, "", "run", "--bogus", "x"); code != exitUsage {
		t.Errorf("unknown flag: exit %d", code)
	}
	if code, _, errOut := run(t, "", "nosuch"); code != exitUsage || !strings.Contains(errOut, "unknown command") {
		t.Errorf("unknown verb: exit %d\n%s", code, errOut)
	}
	if code, _, errOut := run(t, "", "help"); code != 0 || !strings.Contains(errOut, "replay") {
		t.Errorf("help: exit %d\n%s", code, errOut)
	}
	if code, _, _ := run(t, "", "run", "-h"); code != 0 {
		t.Errorf("run -h: exit %d", code)
	}
}

func TestRunUnknownAgent(t *testing.T) {
	env(t, fw.WithModel(&scriptModel{}))
	code, _, errOut := run(t, "", "run", "--agent", "nosuch", "hi")
	if code != 1 || !strings.Contains(errOut, "nosuch") {
		t.Errorf("exit %d\n%s", code, errOut)
	}
}

func TestRunDeniesApprovalWithoutTerminal(t *testing.T) {
	var calls atomic.Int32
	m := &scriptModel{resps: []*fw.Response{callTool("deploy", `{}`), endTurn("gave up")}}
	env(t, fw.WithModel(m), fw.WithTools(countingTool("deploy", true, &calls)))
	setTerminal(t, false)
	code, out, errOut := run(t, "y\n", "run", "ship it")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if calls.Load() != 0 {
		t.Error("the destructive tool ran without approval")
	}
	if !strings.Contains(errOut, "denied approval for tool deploy: standard input is not a terminal") || !strings.Contains(errOut, "decision=deny decider=cli") {
		t.Errorf("stderr:\n%s", errOut)
	}
	if out != "gave up\n" {
		t.Errorf("stdout %q", out)
	}
	if reqs := m.requests(); len(reqs) != 2 || !strings.Contains(toolResults(reqs[1]), "[error]") {
		t.Errorf("the model did not see a denial: %q", toolResults(reqs[len(reqs)-1]))
	}
}

func TestRunAsksOnTerminal(t *testing.T) {
	for _, tc := range []struct {
		name      string
		stdin     string
		args      []string
		wantCalls int32
	}{
		{"yes", "y\n", nil, 1},
		{"no", "n\n", nil, 0},
		{"empty", "\n", nil, 0},
		{"no-approve", "y\n", []string{"--no-approve"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			m := &scriptModel{resps: []*fw.Response{callTool("deploy", `{"env":"prod"}`), endTurn("done")}}
			env(t, fw.WithModel(m), fw.WithTools(countingTool("deploy", true, &calls)))
			setTerminal(t, true)
			args := append([]string{"run", "ship it"}, tc.args...)
			code, _, errOut := run(t, tc.stdin, args...)
			if code != 0 {
				t.Fatalf("exit %d\n%s", code, errOut)
			}
			if calls.Load() != tc.wantCalls {
				t.Errorf("tool calls %d, want %d\n%s", calls.Load(), tc.wantCalls, errOut)
			}
			asked := strings.Contains(errOut, "allow? [y/N]")
			if asked != (tc.name != "no-approve") {
				t.Errorf("asked %v\n%s", asked, errOut)
			}
			if asked && !strings.Contains(errOut, `input: {"env":"prod"}`) {
				t.Errorf("the prompt does not show the input:\n%s", errOut)
			}
		})
	}
}

func TestRunAnswersQuestionOnTerminal(t *testing.T) {
	var answer atomic.Value
	ask := fw.NewTool(fw.ToolSpec{Name: "ask", InputSchema: json.RawMessage(`{"type":"object"}`), ReadOnly: true},
		func(ctx context.Context, _ json.RawMessage) (fw.ToolResult, error) {
			b, err := fw.AskCaller(ctx, "Which name?", nil)
			if err != nil {
				return fw.ErrorResult(err.Error()), nil
			}
			answer.Store(string(b))
			return fw.TextResult(string(b)), nil
		})
	m := &scriptModel{resps: []*fw.Response{callTool("ask", `{}`), endTurn("thanks")}}
	env(t, fw.WithModel(m), fw.WithTools(ask))
	setTerminal(t, true)
	code, _, errOut := run(t, "Bob\n", "run", "greet")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if !strings.Contains(errOut, "question from tool ask: Which name?") {
		t.Errorf("stderr:\n%s", errOut)
	}
	if got, _ := answer.Load().(string); got != `{"answer":"Bob"}` {
		t.Errorf("answer %q", got)
	}
}

// runID runs one input and returns the run id from the JSON output.
func runID(t *testing.T, args ...string) string {
	t.Helper()
	code, out, errOut := run(t, "", append([]string{"run", "--json"}, args...)...)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	var r fw.Run
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatal(err)
	}
	return r.ID
}

func TestEventsFromFileLedger(t *testing.T) {
	var calls atomic.Int32
	m := &scriptModel{resps: []*fw.Response{callTool("lookup", `{}`), endTurn("found")}}
	home := env(t, fw.WithModel(m), fw.WithTools(countingTool("lookup", false, &calls)))
	id := runID(t, "find it")
	if _, err := os.Stat(filepath.Join(home, "runs", id, "ledger.jsonl")); err != nil {
		t.Fatalf("the run is not in the file ledger: %v", err)
	}

	code, out, errOut := run(t, "", "events", id)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var types []string
	for i, l := range lines {
		var e fw.Event
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatalf("line %d is not an event: %v", i, err)
		}
		if e.Subject != id || e.Seq != uint64(i+1) {
			t.Errorf("line %d: subject %s seq %d", i, e.Subject, e.Seq)
		}
		types = append(types, e.Type)
	}
	if types[0] != fw.EvRunQueued || types[len(types)-1] != fw.EvRunFinished || !slices.Contains(types, fw.EvToolResponded) {
		t.Errorf("types %v", types)
	}

	code, out, _ = run(t, "", "events", "--from", "3", id)
	if code != 0 || len(strings.Split(strings.TrimSpace(out), "\n")) != len(lines)-2 {
		t.Errorf("--from 3: exit %d, %d lines of %d", code, len(strings.Split(strings.TrimSpace(out), "\n")), len(lines))
	}

	code, out, _ = run(t, "", "events", id, "--format", "text", "--follow")
	if code != 0 || !strings.Contains(out, "fw.tool.responded tool=lookup latency=") || !strings.Contains(out, "fw.run.finished outcome=completed") {
		t.Errorf("text: exit %d\n%s", code, out)
	}

	if code, _, errOut := run(t, "", "events", "01NOSUCHRUN"); code != 1 || !strings.Contains(errOut, "not found") {
		t.Errorf("unknown run: exit %d\n%s", code, errOut)
	}
	if code, _, _ := run(t, "", "events", id, "--format", "xml"); code != exitUsage {
		t.Errorf("bad format: exit %d", code)
	}
}

// A one-shot verb does not resume the runs that a server left in the ledger.
func TestRunDoesNotResumeOtherRuns(t *testing.T) {
	m := &scriptModel{resps: []*fw.Response{endTurn("first"), endTurn("second")}}
	home := env(t, fw.WithModel(m))
	// Leave a run unfinished in the ledger, as a server killed mid-run does.
	l, err := fw.OpenFileLedger(filepath.Join(home, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(context.Background(), fw.MustEvent("01OLDRUN", fw.EvRunQueued, fw.RunQueuedData{Agent: "default", Input: "old", Origin: "http"})); err != nil {
		t.Fatal(err)
	}
	l.Close()
	runID(t, "new")
	_, out, _ := run(t, "", "events", "01OLDRUN")
	if strings.Count(strings.TrimSpace(out), "\n") != 0 {
		t.Errorf("the old run changed:\n%s", out)
	}
	if n := len(m.requests()); n != 1 {
		t.Errorf("%d model calls, want 1", n)
	}
}

func TestReplay(t *testing.T) {
	var calls atomic.Int32
	tool := countingTool("lookup", false, &calls)
	m := &scriptModel{resps: []*fw.Response{callTool("lookup", `{}`), endTurn("first answer")}}
	home := env(t, fw.WithModel(m), fw.WithTools(tool))
	id := runID(t, "find it")
	if calls.Load() != 1 {
		t.Fatalf("tool calls %d", calls.Load())
	}

	m2 := &scriptModel{resps: []*fw.Response{callTool("lookup", `{}`), endTurn("second answer")}}
	AgentOptions = func() []fw.Option {
		return []fw.Option{fw.WithModel(m2), fw.WithTools(tool), fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))}
	}
	code, out, errOut := run(t, "", "replay", id, "--model", "fake/other", "--ledger", filepath.Join(home, "runs"), "--json")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	var r fw.Run
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if r.ID == id || r.Output != "second answer" || r.Metadata["replayOf"] != id {
		t.Errorf("replay run %+v", r)
	}
	if !strings.Contains(errOut, "run "+r.ID+" replays "+id+` "find it" with fake/other: completed`) {
		t.Errorf("stderr:\n%s", errOut)
	}
	if calls.Load() != 1 {
		t.Errorf("the replay ran the tool: %d calls", calls.Load())
	}
	if reqs := m2.requests(); len(reqs) != 2 || !strings.Contains(toolResults(reqs[1]), "lookup done") {
		t.Errorf("the replay did not answer from the recording: %+v", reqs)
	}
	if code, _, _ := run(t, "", "replay", "01NOSUCHRUN"); code != 1 {
		t.Errorf("unknown run: exit %d", code)
	}
}

func TestTools(t *testing.T) {
	var calls atomic.Int32
	env(t, fw.WithModel(&scriptModel{}), fw.WithTools(countingTool("lookup", false, &calls), countingTool("deploy", true, &calls)))
	code, out, errOut := run(t, "", "tools")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || strings.Join(strings.Fields(lines[0]), " ") != "NAME FLAGS SOURCE DESCRIPTION" {
		t.Fatalf("table:\n%s", out)
	}
	if got := strings.Join(strings.Fields(lines[1]), " "); got != "lookup readonly test Changes lookup." {
		t.Errorf("row %q", got)
	}
	if got := strings.Join(strings.Fields(lines[2]), " "); got != "deploy destructive test Changes deploy." {
		t.Errorf("row %q", got)
	}

	code, out, _ = run(t, "", "tools", "--json")
	var specs []fw.ToolSpec
	if code != 0 || json.Unmarshal([]byte(out), &specs) != nil || len(specs) != 2 || specs[1].Name != "deploy" || !specs[1].Destructive {
		t.Errorf("json: exit %d\n%s", code, out)
	}
}

func TestConfigVerb(t *testing.T) {
	env(t)
	path := filepath.Join(t.TempDir(), "fw.yaml")
	if err := os.WriteFile(path, []byte("auth:\n  mode: token\n  tokenEnv: MY_TOKEN\nbus:\n  url: nats://file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FW_BUS_URL", "nats://env")
	code, out, errOut := run(t, "", "--config", path, "--listen", ":9999", "config")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	// tokenEnv names a variable, so it stays visible; values under secret keys are masked.
	for _, want := range []string{"listen: :9999", "tokenEnv: MY_TOKEN", "url: nats://env", "home: " + os.Getenv("FW_HOME")} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// A flag after the verb works too, and a bad file fails with exit 1.
	code, out, _ = run(t, "", "config", "--config", path, "--log", "debug")
	if code != 0 || !strings.Contains(out, "log: debug") {
		t.Errorf("flag after the verb: exit %d\n%s", code, out)
	}
	if err := os.WriteFile(path, []byte("bogus: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := run(t, "", "--config", path, "config"); code != 1 || !strings.Contains(errOut, "bogus") {
		t.Errorf("bad file: exit %d\n%s", code, errOut)
	}
}

func TestVersion(t *testing.T) {
	env(t)
	prevV, prevC := Version, Commit
	Version, Commit = "1.2.3", "abc123"
	t.Cleanup(func() { Version, Commit = prevV, prevC })
	code, out, _ := run(t, "", "version")
	if code != 0 || !strings.Contains(out, "fw 1.2.3\n") || !strings.Contains(out, "commit abc123\n") || !strings.Contains(out, "go1.") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestCardAndMCPNeedTheirSurfaces(t *testing.T) {
	env(t, fw.WithModel(&scriptModel{}))
	if code, _, errOut := run(t, "", "card"); code != 1 || !strings.Contains(errOut, "not linked") {
		t.Errorf("card: exit %d\n%s", code, errOut)
	}
	prev := CardJSON
	CardJSON = func(a *fw.Agent) ([]byte, error) { return json.Marshal(map[string]string{"name": a.Config().A2A.Name}) }
	t.Cleanup(func() { CardJSON = prev })
	if code, out, _ := run(t, "", "card"); code != 0 || out != `{"name":"featherweight"}`+"\n" {
		t.Errorf("card: exit %d %q", code, out)
	}

	prevMCP := ServeMCPStdio
	var served atomic.Bool
	ServeMCPStdio = func(_ context.Context, a *fw.Agent) error { served.Store(a != nil); return nil }
	t.Cleanup(func() { ServeMCPStdio = prevMCP })
	if code, out, _ := run(t, "", "mcp"); code != 0 || out != "" || !served.Load() {
		t.Errorf("mcp: exit %d, stdout %q, served %v", code, out, served.Load())
	}
}

func TestParseArgs(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	a := fs.String("agent", "", "")
	j := fs.Bool("json", false, "")
	pos, err := parseArgs(fs, []string{"one", "--agent", "x", "two", "--json", "--", "--three"})
	if err != nil {
		t.Fatal(err)
	}
	if *a != "x" || !*j || !slices.Equal(pos, []string{"one", "two", "--three"}) {
		t.Errorf("agent %q json %v pos %q", *a, *j, pos)
	}
}

func TestSummarizeHoldsNoToolIO(t *testing.T) {
	tools := map[string]string{}
	req := fw.MustEvent("r", fw.EvToolRequested, fw.ToolRequestedData{Tool: "bash", CallID: "c1", Input: json.RawMessage(`{"cmd":"echo SECRET"}`)})
	res := fw.MustEvent("r", fw.EvToolResponded, fw.ToolRespondedData{CallID: "c1", Content: []fw.Block{fw.Text("SECRET")}, LatencyMS: 7})
	s := summarize(req, tools) + "\n" + summarize(res, tools)
	if strings.Contains(s, "SECRET") || !strings.Contains(s, "tool=bash latency=7ms") {
		t.Errorf("summary:\n%s", s)
	}
}

// The summary of fw.run.queued names the work by its title: the metadata
// title when the request set one, else a title from the input.
func TestSummarizeNamesTheWork(t *testing.T) {
	cases := []struct {
		name string
		data fw.RunQueuedData
		want string
	}{
		{"input", fw.RunQueuedData{Agent: "reviewer", Input: "# Fix checkout retries\n\nThe retries double-charge.", Origin: "http"},
			`fw.run.queued title="Fix checkout retries" agent=reviewer origin=http`},
		{"metadata", fw.RunQueuedData{Agent: "reviewer", Input: "long text", Origin: "mcp", Metadata: map[string]string{fw.MetaTitle: " Ship the\nrelease "}},
			`fw.run.queued title="Ship the release" agent=reviewer origin=mcp`},
		{"no text", fw.RunQueuedData{Agent: "default", Input: " ", Origin: "cli"},
			"fw.run.queued agent=default origin=cli"},
	}
	for _, c := range cases {
		e := fw.MustEvent("r", fw.EvRunQueued, c.data)
		if got := summarize(e, map[string]string{}); !strings.HasSuffix(got, c.want) {
			t.Errorf("%s: summary %q, want the suffix %q", c.name, got, c.want)
		}
	}
	if got := runTitle(&fw.Run{Input: "Draft the notes. Then send them."}); got != "Draft the notes" {
		t.Errorf("runTitle from the input = %q", got)
	}
	if got := runTitle(&fw.Run{Title: "Set by hand", Input: "other"}); got != "Set by hand" {
		t.Errorf("runTitle from Title = %q", got)
	}
}

func TestRunProfileFlags(t *testing.T) {
	m := &scriptModel{resps: []*fw.Response{endTurn("shaped")}}
	home := env(t, fw.WithModel(m), fw.WithProfile(fw.AgentProfile{Name: "writer", System: "You write.", Instructions: []string{"House rule."}}))
	rules := filepath.Join(t.TempDir(), "rules.md")
	if err := os.WriteFile(rules, []byte("From a file.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := run(t, "", "run", "--json", "--agent", "writer", "--system", "You edit.", "-i", "Be brief.", "--instructions", "@"+rules,
		"--tools", "files, git.*", "--skills", "none", "--effort", "LOW", "--time-zone", "America/Chicago", "--max-turns", "3", "--deadline", "90s", "--max-tool-calls", "7", "go")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	var r fw.Run
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	reqs := m.requests()
	if len(reqs) != 1 {
		t.Fatalf("model calls = %d", len(reqs))
	}
	sys := reqs[0].System
	for _, want := range []string{"You edit.", "House rule.", "Be brief.", "From a file."} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt lacks %q:\n%s", want, sys)
		}
	}
	if strings.Contains(sys, "You write.") || reqs[0].Effort != "low" {
		t.Errorf("the inline profile was not applied: effort %q\n%s", reqs[0].Effort, sys)
	}
	// The ledger holds the profile, so a replay sees it.
	l, err := fw.OpenFileLedger(filepath.Join(home, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var q fw.RunQueuedData
	for e, err := range l.Read(context.Background(), r.ID, 0) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Type == fw.EvRunQueued {
			_ = e.Decode(&q)
		}
	}
	if q.Profile == nil || q.Profile.System != "You edit." || !slices.Equal(q.Profile.Tools, []string{"files", "git.*"}) ||
		len(q.Profile.Skills) != 0 || q.Profile.Budget == nil || q.Profile.Budget.MaxTurns != 3 ||
		q.Profile.Budget.Deadline != 90*time.Second || q.Profile.Budget.MaxToolCalls != 7 || len(q.Profile.Instructions) != 2 ||
		q.Profile.TimeZone != "America/Chicago" {
		t.Errorf("queued profile = %+v budget %+v", q.Profile, q.Profile.Budget)
	}
}

func TestRunProfileFlagErrors(t *testing.T) {
	env(t, fw.WithModel(&scriptModel{}))
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--effort", "extreme"}, "-effort"},
		{[]string{"--time-zone", "Mars/Olympus_Mons"}, "-time-zone \"Mars/Olympus_Mons\""},
		{[]string{"--system", "@/nonexistent/prompt.md"}, "-system"},
		{[]string{"--max-turns", "-1"}, "negative"},
		{[]string{"--model", "nowhere"}, "model"},
	} {
		code, _, errOut := run(t, "", append([]string{"run", "x"}, tc.args...)...)
		if code == 0 || !strings.Contains(errOut, tc.want) {
			t.Errorf("%v: exit %d\n%s", tc.args, code, errOut)
		}
	}
}

func TestRunBudgetHint(t *testing.T) {
	m := &scriptModel{resps: []*fw.Response{endTurn("one"), endTurn("two")}}
	env(t, fw.WithModel(m), fw.WithTools(countingTool("lookup", false, new(atomic.Int32))))
	m.resps = []*fw.Response{callTool("lookup", `{}`), callTool("lookup", `{}`), callTool("lookup", `{}`)}
	code, _, errOut := run(t, "", "run", "--max-turns", "2", "loop")
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, errOut)
	}
	for _, want := range []string{"fw.budget.exceeded limit=maxTurns", "stopped at the limit maxTurns", "-max-turns", "budget.maxTurns"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut)
		}
	}
	for limit, want := range map[string]string{
		"maxToolCalls":    "-max-tool-calls",
		"deadline":        "-deadline",
		"maxTokens":       "budget.maxTokens",
		"maxRepeats":      "repeated the same tool call",
		"maxCallsPerTurn": "budget.maxCallsPerTurn",
	} {
		if h := budgetHint(limit); !strings.Contains(h, want) || !strings.Contains(h, limit) {
			t.Errorf("hint for %s = %q", limit, h)
		}
	}
}

func TestSummarizeLoopDetected(t *testing.T) {
	e := fw.MustEvent("r", fw.EvLoopDetected, fw.LoopDetectedData{Tool: "bash", Count: 4, Period: 2, Action: "warned"})
	if got := summarize(e, map[string]string{}); !strings.HasSuffix(got, "fw.loop.detected tool=bash count=4 period=2 action=warned") {
		t.Errorf("summary = %q", got)
	}
}

// writeSkill writes a skill with a description into dir.
func writeSkill(t *testing.T, dir, name, description string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, name, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: " + description + "\n---\n# " + name + "\n"
	if err := os.WriteFile(filepath.Join(dir, name, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name, "scripts", "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestSkillsVerb(t *testing.T) {
	home := env(t)
	code, out, errOut := run(t, "", "skills")
	if code != 0 || out != "" || !strings.Contains(errOut, "no skill under") {
		t.Errorf("no skills: exit %d\n%s\n%s", code, out, errOut)
	}
	writeSkill(t, filepath.Join(home, "skills"), "review", "Review a pull request")
	writeSkill(t, filepath.Join(home, "skills"), "deploy", "Ship a release")
	code, out, errOut = run(t, "", "skills")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || strings.Join(strings.Fields(lines[0]), " ") != "NAME DESCRIPTION" ||
		strings.Join(strings.Fields(lines[1]), " ") != "deploy Ship a release" || strings.Join(strings.Fields(lines[2]), " ") != "review Review a pull request" {
		t.Errorf("table:\n%s", out)
	}
	code, out, _ = run(t, "", "skills", "review")
	if code != 0 || !strings.HasPrefix(out, "---\nname: review\n") || !strings.HasSuffix(out, "# review\n") {
		t.Errorf("body: exit %d\n%s", code, out)
	}
	code, out, _ = run(t, "", "skills", "--json", "review")
	var doc skillJSON
	if code != 0 || json.Unmarshal([]byte(out), &doc) != nil || doc.Name != "review" || !strings.Contains(doc.Body, "# review") || !slices.Equal(doc.Files, []string{"scripts/run.sh"}) {
		t.Errorf("json: exit %d\n%s", code, out)
	}
	if code, _, errOut := run(t, "", "skills", "nope"); code != 1 || !strings.Contains(errOut, `"nope"`) || !strings.Contains(errOut, "deploy, review") {
		t.Errorf("unknown skill: exit %d\n%s", code, errOut)
	}
	if code, _, _ := run(t, "", "skills", "a", "b"); code != exitUsage {
		t.Errorf("two names: exit %d", code)
	}
}

func TestInitVerb(t *testing.T) {
	m := &scriptModel{resps: []*fw.Response{endTurn("hi")}}
	env(t, fw.WithModel(m))
	dir := filepath.Join(t.TempDir(), "proj")
	code, out, errOut := run(t, "", "init", dir)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	for _, p := range []string{"fw.yaml", filepath.Join("prompts", "default.md"), "agents", "skills"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("%s was not written: %v", p, err)
		}
		if !strings.Contains(out, filepath.Join(dir, p)) {
			t.Errorf("the output does not name %s:\n%s", p, out)
		}
	}
	prompt, _ := os.ReadFile(filepath.Join(dir, "prompts", "default.md"))
	if strings.TrimSpace(string(prompt)) != fw.DefaultSystemPrompt {
		t.Errorf("prompt = %q", prompt)
	}
	cfg, err := config.Load(filepath.Join(dir, "fw.yaml"))
	if err != nil {
		t.Fatalf("the starter does not load: %v", err)
	}
	if cfg.Agents["default"].System != fw.DefaultSystemPrompt+"\n" || cfg.Budget.MaxRepeats != 5 || cfg.Model != "anthropic/claude-opus-5-5" {
		t.Errorf("loaded = model %q budget %+v system %q", cfg.Model, cfg.Budget, cfg.Agents["default"].System)
	}
	if !slices.Equal(cfg.Tools.Builtin, fw.DefaultConfig().Tools.Builtin) || cfg.Time != fw.DefaultConfig().Time {
		t.Errorf("starter builtin %v, time %+v; want the defaults", cfg.Tools.Builtin, cfg.Time)
	}
	// A second init refuses and changes nothing.
	before, _ := os.ReadFile(filepath.Join(dir, "fw.yaml"))
	if code, _, errOut := run(t, "", "init", dir); code != 1 || !strings.Contains(errOut, "does not overwrite") {
		t.Errorf("second init: exit %d\n%s", code, errOut)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "fw.yaml"))
	if string(before) != string(after) {
		t.Error("the second init changed fw.yaml")
	}
	// The starter runs: the default profile uses the prompt file.
	t.Chdir(dir)
	t.Setenv("FW_HOME", "")
	code, _, errOut = run(t, "", "run", "hello")
	if code != 0 {
		t.Fatalf("run in the new directory: exit %d\n%s", code, errOut)
	}
	if reqs := m.requests(); len(reqs) != 1 || strings.TrimSpace(reqs[0].System) != fw.DefaultSystemPrompt {
		t.Errorf("the prompt file was not used: %+v", reqs)
	}
	if _, err := os.Stat(filepath.Join(dir, "runs")); err != nil {
		t.Errorf("home is not the project directory: %v", err)
	}
}

func TestEnvVerb(t *testing.T) {
	env(t)
	t.Setenv("FW_MODEL", "fake/one")
	t.Setenv("FW_API_TOKEN", "tok-secret")
	t.Setenv("ANTHROPIC_API_KEY", "sk-secret")
	t.Setenv("FW_AUTH_TOKEN_ENV", "MY_TOKEN")
	code, out, errOut := run(t, "", "env")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	for _, want := range []string{"\nFW_MODEL=fake/one\n", "\nFW_API_TOKEN=***\n", "\nANTHROPIC_API_KEY=***\n", "\nFW_AUTH_TOKEN_ENV=MY_TOKEN\n", "\nFW_BUS_URL\n", "\nFW_HOME="} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "secret") {
		t.Errorf("a secret leaked:\n%s", out)
	}
	code, out, _ = run(t, "", "env", "--set")
	if code != 0 || strings.Contains(out, "\nFW_BUS_URL\n") || !strings.Contains(out, "FW_MODEL=fake/one") {
		t.Errorf("--set: exit %d\n%s", code, out)
	}
}

// listingModel is a fake model that lists the models of its provider.
type listingModel struct {
	fw.Model
	models []string
	err    error
}

func (l listingModel) Models(context.Context) ([]string, error) { return l.models, l.err }

// namedSource is a fake tool source with a name.
type namedSource struct {
	name  string
	tools []fw.Tool
	err   error
}

func (s namedSource) Name() string { return s.name }

func (s namedSource) Tools(context.Context) ([]fw.Tool, error) { return s.tools, s.err }

// doctorSeams replaces the model and the tool sources of fw doctor.
func doctorSeams(t *testing.T, m fw.Model, sources []fw.ToolSource, srcErr error) {
	t.Helper()
	prevModel, prevSources := openModel, openToolSources
	openModel = func(_ context.Context, selector string, _ *fw.Config, _ fw.Secrets) (fw.Model, error) {
		if m == nil {
			return nil, errors.New("no provider for " + selector)
		}
		return m, nil
	}
	openToolSources = func(context.Context, *fw.Config, fw.Secrets) ([]fw.ToolSource, error) { return sources, srcErr }
	t.Cleanup(func() { openModel, openToolSources = prevModel, prevSources })
}

func TestDoctorAllOK(t *testing.T) {
	home := env(t)
	t.Setenv("FW_TIME_ZONE", "America/Chicago")
	t.Setenv("ANTHROPIC_API_KEY", "sk-doctor-secret")
	t.Setenv("ANTHROPIC_API_KEY_FILE", "")
	writeSkill(t, filepath.Join(home, "skills"), "review", "Review")
	doctorSeams(t, listingModel{models: []string{"claude-opus-5-5", "claude-haiku-5-5"}},
		[]fw.ToolSource{namedSource{name: "git", tools: []fw.Tool{countingTool("git.log", false, new(atomic.Int32))}}}, nil)
	code, out, errOut := run(t, "", "doctor")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 14 {
		t.Errorf("%d lines, want 14:\n%s", len(lines), out)
	}
	timeLine := regexp.MustCompile(`(?m)^ok    time: America/Chicago, [A-Z][a-z]+day \d{4}-\d{2}-\d{2} \d{2}:\d{2} C[SD]T, notes after 10m$`)
	if !timeLine.MatchString(out) {
		t.Errorf("no time line:\n%s", out)
	}
	for _, want := range []string{
		"ok    configuration: no fw.yaml",
		"ok    home: " + home + " is writable",
		"ok    auth: modes token",
		"ok    auth token: FW_API_TOKEN is not set; fw serve needs it, and fw run does not",
		"ok    tls: off; the listener speaks plain HTTP",
		"ok    access: default open, 0 admins",
		"ok    credential: ANTHROPIC_API_KEY is set",
		"ok    model: anthropic/claude-opus-5-5 opens and is one of the 2 models",
		"ok    tool source git: 1 tools",
		"ok    tokens: off; set tokens.url (FW_TOKENS_URL) to get the tokens of people from a broker",
		"ok    bus: mem opens",
		"ok    ledger: file ledger " + filepath.Join(home, "runs") + " appends and reads",
		"ok    skills: 1 in " + filepath.Join(home, "skills"),
	} {
		if !strings.Contains(out, want+"\n") && !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "sk-doctor") || strings.Contains(errOut, "sk-doctor") {
		t.Error("the credential value leaked")
	}
	if strings.Contains(out, "FAIL") {
		t.Errorf("a check failed:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(home, "runs", ".doctor")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the doctor ledger directory was left behind: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(home, "runs")); len(entries) != 0 {
		t.Errorf("the doctor left runs behind: %v", entries)
	}
}

func TestDoctorReportsEveryProblem(t *testing.T) {
	home := env(t)
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY_FILE", "")
	bad := filepath.Join(t.TempDir(), "fw.yaml")
	if err := os.WriteFile(bad, []byte("bogus: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "nowhere")
	t.Setenv("FW_TOOLS_SKILLS", missing)
	doctorSeams(t, listingModel{models: []string{"gpt-x", "claude-haiku-5-5"}},
		[]fw.ToolSource{
			namedSource{name: "git", tools: nil},
			namedSource{name: "down", err: errors.New("connection refused")},
		}, nil)
	code, out, errOut := run(t, "", "--config", bad, "doctor")
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s%s", code, out, errOut)
	}
	for _, want := range []string{
		"FAIL  configuration: ",
		"the checks below use the defaults",
		"ok    home: " + home,
		"FAIL  credential: ANTHROPIC_API_KEY is not set",
		"FAIL  model: anthropic/claude-opus-5-5 is not one of the 2 models",
		"claude-haiku-5-5",
		"ok    tool source git: 0 tools",
		"FAIL  tool source down: tools/list failed: connection refused",
		"ok    bus: mem opens",
		"ok    ledger: ",
		"FAIL  skills: " + missing + " does not exist",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(errOut, "fw doctor: 5 problems") {
		t.Errorf("stderr:\n%s", errOut)
	}

	// A model that does not open, a provider without a model list, and a
	// source constructor that fails.
	t.Setenv("FW_TOOLS_SKILLS", "")
	t.Setenv("OPENAI_API_KEY", "x")
	t.Setenv("OPENAI_API_KEY_FILE", "")
	t.Setenv("FW_MODEL", "openai/gpt-x")
	doctorSeams(t, nil, nil, errors.New("mcp: server git: bad endpoint"))
	code, out, _ = run(t, "", "doctor")
	if code != 1 || !strings.Contains(out, "FAIL  model: openai/gpt-x does not open") || !strings.Contains(out, "FAIL  tool sources: mcp: server git: bad endpoint") ||
		!strings.Contains(out, "ok    credential: OPENAI_API_KEY is set") || !strings.Contains(out, "ok    skills: no directory at") {
		t.Errorf("exit %d\n%s", code, out)
	}
	doctorSeams(t, &scriptModel{}, nil, nil)
	if code, out, _ = run(t, "", "doctor"); code != 0 || !strings.Contains(out, "ok    model: openai/gpt-x opens\n") || !strings.Contains(out, "ok    tool sources: none configured") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestHelpListsNewVerbs(t *testing.T) {
	env(t)
	_, _, errOut := run(t, "", "help")
	for _, want := range []string{"init", "doctor", "skills", "env", "Start here", "agents", "whoami", "Identity:", "local:<user>"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("help lacks %q:\n%s", want, errOut)
		}
	}
	if code, _, errOut := run(t, "", "agents", "-h"); code != 0 || !strings.Contains(errOut, "fw agents put <file.yaml>") || !strings.Contains(errOut, "next start") {
		t.Errorf("agents -h: exit %d\n%s", code, errOut)
	}
	if code, _, errOut := run(t, "", "mcp", "-h"); code != 0 || !strings.Contains(errOut, "local principal") || !strings.Contains(errOut, "kind:local") {
		t.Errorf("mcp -h: exit %d\n%s", code, errOut)
	}
	if code, _, errOut := run(t, "", "run", "-h"); code != 0 || !strings.Contains(errOut, "-max-tool-calls") || !strings.Contains(errOut, "-skills") {
		t.Errorf("run -h: exit %d\n%s", code, errOut)
	}
}

func TestDoctorTime(t *testing.T) {
	env(t)
	doctorSeams(t, &scriptModel{}, nil, nil)
	t.Setenv("FW_TIME_ZONE", "UTC")
	t.Setenv("FW_TIME_NOTE_AFTER", "0")
	_, out, _ := run(t, "", "doctor")
	if !regexp.MustCompile(`(?m)^ok    time: UTC, .* UTC, notes off$`).MatchString(out) {
		t.Errorf("no time line for UTC:\n%s", out)
	}
	// A zone that does not resolve fails the configuration and names the key.
	t.Setenv("FW_TIME_ZONE", "Mars/Olympus_Mons")
	code, out, _ := run(t, "", "doctor")
	if code != 1 || !strings.Contains(out, "FAIL  configuration: ") || !strings.Contains(out, `time.zone "Mars/Olympus_Mons"`) {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestRunRefusesBadZone(t *testing.T) {
	env(t, fw.WithModel(&scriptModel{resps: []*fw.Response{endTurn("never")}}))
	t.Setenv("FW_TIME_ZONE", "Mars/Olympus_Mons")
	code, _, errOut := run(t, "", "run", "hello")
	if code == 0 || !strings.Contains(errOut, "time.zone") {
		t.Errorf("exit %d\n%s", code, errOut)
	}
}
