package harness

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// testKey is the key of the model in the tests. No event may hold it.
const testKey = "sk-test-harness-0123456789abcdef"

// testDirs are the directories of one test: the home of fw, the
// workspace, and the home of the harness, which outlive a restart of the
// agent.
type testDirs struct {
	root, fwHome, ws, home string
}

func newDirs(t *testing.T) testDirs {
	t.Helper()
	root := t.TempDir()
	d := testDirs{root: root, fwHome: filepath.Join(root, ".fw"), ws: filepath.Join(root, "ws"), home: filepath.Join(root, ".harness")}
	if err := os.MkdirAll(d.ws, 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

// testConfig returns the configuration of a test agent with the block
// harness.
func testConfig(t *testing.T, d testDirs, block string) *fw.Config {
	t.Helper()
	t.Setenv("FW_TEST_MODEL_KEY", testKey)
	t.Setenv("FW_ATESPACE", "")
	cfg := fw.DefaultConfig()
	cfg.Home = d.fwHome
	cfg.Tools.Files.Root = d.ws
	cfg.Tools.User = fw.ToolUserNone
	cfg.Tools.Builtin = nil
	cfg.Surfaces = nil
	cfg.Model = "openai/test-model"
	cfg.Providers = map[string]fw.ProviderConfig{
		"openai":    {BaseURL: "http://127.0.0.1:9/v1", APIKeyEnv: "FW_TEST_MODEL_KEY"},
		"anthropic": {BaseURL: "http://127.0.0.1:9", APIKeyEnv: "FW_TEST_MODEL_KEY"},
	}
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(block), &n); err != nil {
		t.Fatal(err)
	}
	cfg.Extensions = map[string]yaml.Node{"harness": *n.Content[0]}
	return cfg
}

// newAgent builds an agent with the registered runner of this package.
// The model of the kernel fails the test when the kernel calls it: a
// hosted run calls the model through the harness alone.
func newAgent(t *testing.T, cfg *fw.Config, l fw.Ledger) *fw.Agent {
	t.Helper()
	model := fw.ModelFunc{StreamFn: func(context.Context, *fw.Request, func(fw.Delta)) (*fw.Response, error) {
		t.Error("the kernel called its model for a hosted run")
		return nil, errors.New("no model")
	}}
	a, err := fw.New(fw.WithConfig(cfg), fw.WithLedger(l), fw.WithBus(fw.NewMemBus()), fw.WithModel(model),
		fw.WithAuthn(fw.NoAuthn{}), fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { closeAgent(a) })
	return a
}

func closeAgent(a *fw.Agent) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.Close(ctx)
}

// serveNative serves the native API of a on the listener of its
// configuration.
func serveNative(t *testing.T, a *fw.Agent, ln net.Listener) {
	t.Helper()
	h, err := a.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func waitState(t *testing.T, a *fw.Agent, id string, done func(*fw.Run) bool) *fw.Run {
	t.Helper()
	r, err := a.WaitFor(testCtx(t), id, done)
	if err != nil {
		t.Fatalf("wait for run %s: %v", id, err)
	}
	return r
}

func terminal(r *fw.Run) bool      { return r.State.Terminal() }
func inputRequired(r *fw.Run) bool { return r.State == fw.StateInputRequired }

// events reads the ledger of a run.
func events(t *testing.T, l fw.Ledger, id string) []fw.Event {
	t.Helper()
	var out []fw.Event
	for e, err := range l.Read(context.Background(), id, 0) {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

// ofType returns the events of one type.
func ofType(evs []fw.Event, typ string) []fw.Event {
	var out []fw.Event
	for _, e := range evs {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// harnessKinds returns the kinds of the fw.harness.event events, in order.
func harnessKinds(evs []fw.Event) []string {
	var out []string
	for _, e := range ofType(evs, EvEvent) {
		var d EventData
		_ = e.Decode(&d)
		out = append(out, d.Kind)
	}
	return out
}

// noSecret fails the test when an event of the run holds the key.
func noSecret(t *testing.T, evs []fw.Event) {
	t.Helper()
	for _, e := range evs {
		if strings.Contains(string(e.Data), testKey) {
			t.Fatalf("the event %s holds the key: %s", e.Type, e.Data)
		}
	}
}

func TestHomeOf(t *testing.T) {
	d := newDirs(t)
	cfg := testConfig(t, d, "kind: opencode")
	home, err := homeOf(cfg, "")
	if err != nil || home != d.home {
		t.Fatalf("default home = %q, %v; want %q", home, err, d.home)
	}
	if _, err := homeOf(cfg, filepath.Join(d.fwHome, "h")); err == nil || !strings.Contains(err.Error(), "inside the home of fw") {
		t.Fatalf("a home inside FW_HOME: %v", err)
	}
	on := true
	cfg.Tools.Files.PerRun = &on
	if _, err := homeOf(cfg, filepath.Join(d.ws, "runs", "x")); err == nil || !strings.Contains(err.Error(), "directories of the runs") {
		t.Fatalf("a home inside a run directory: %v", err)
	}
}

func TestConfigErrors(t *testing.T) {
	d := newDirs(t)
	for block, want := range map[string]string{
		"kind: vim":                     "unknown kind",
		"home: /tmp/x":                  "needs kind",
		"kind: opencode\nport: 70000":   "not a TCP port",
		"kind: claude-code\nport: 4096": "setting of opencode",
	} {
		cfg := testConfig(t, d, block)
		_, err := fw.New(fw.WithConfig(cfg), fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()),
			fw.WithAuthn(fw.NoAuthn{}), fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error = %v, want %q", block, err, want)
		}
	}
	// Without the block, the agent has no runner.
	cfg := testConfig(t, d, "kind: opencode")
	cfg.Extensions = nil
	a := newAgent(t, cfg, fw.NewMemLedger())
	if a.Runner() != nil {
		t.Fatal("an agent without the block harness has a runner")
	}
}

func TestHookRoute(t *testing.T) {
	d := newDirs(t)
	cfg := testConfig(t, d, "kind: claude-code")
	a := newAgent(t, cfg, fw.NewMemLedger())
	h := a.Runner().(*host)
	// A caller outside the loopback address gets nothing.
	req := httptest.NewRequest(http.MethodPost, hookPath+"abc", strings.NewReader(`{}`))
	req.RemoteAddr = "10.1.2.3:4000"
	req.SetPathValue("secret", "abc")
	w := httptest.NewRecorder()
	h.hook(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("a remote caller: %d", w.Code)
	}
	// A secret that no process holds gets a deny.
	req = httptest.NewRequest(http.MethodPost, hookPath+"abc", strings.NewReader(`{"tool_name":"Bash"}`))
	req.RemoteAddr = "127.0.0.1:4000"
	req.SetPathValue("secret", "abc")
	w = httptest.NewRecorder()
	h.hook(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"permissionDecision":"deny"`) {
		t.Fatalf("an unknown secret: %d %s", w.Code, w.Body)
	}
}

// waitFor waits until cond holds, for ten seconds at most.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("the condition did not hold within ten seconds")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestPrivatePath denies a tool call with a path inside the home of the
// harness, the home of fw, or a reserved path, with or without a link,
// and passes a path of the workspace and a file of cut tool output.
func TestPrivatePath(t *testing.T) {
	d := newDirs(t)
	if err := os.Symlink(d.home, filepath.Join(d.ws, "link")); err != nil {
		t.Fatal(err)
	}
	for kind, cases := range map[string][]struct {
		tool, input string
		denied      bool
	}{
		KindClaudeCode: {
			{"read", `{"file_path":"` + d.home + `/projects/p/s.jsonl"}`, true},
			{"read", `{"file_path":"../.harness/settings.json"}`, true},
			{"read", `{"file_path":"~/settings.json"}`, true},
			{"read", `{"file_path":"link/projects/p/s.jsonl"}`, true},
			{"read", `{"file_path":"` + d.fwHome + `/ledger.db"}`, true},
			{"notebookread", `{"notebook_path":"` + d.home + `/n.ipynb"}`, true},
			{"write", `{"file_path":"` + d.home + `/settings.json","content":"{}"}`, true},
			{"grep", `{"pattern":"secret","path":"` + d.home + `"}`, true},
			{"glob", `{"pattern":"` + d.home + `/**/*.jsonl"}`, true},
			{"grep", `{"pattern":"` + d.home + `/.*"}`, false},
			{"read", `{"file_path":"notes.txt"}`, false},
			{"read", `{"file_path":"` + d.ws + `/src/main.go"}`, false},
			{"read", `{"file_path":"` + d.home + `/projects/p/s/tool-results/toolu_1.txt"}`, false},
			{"glob", `{"pattern":"*","path":"` + d.home + `/projects/p/s/tool-results"}`, true},
			{"grep", `{"pattern":"x","path":"` + d.home + `/projects/p/s/tool-results"}`, true},
		},
		KindOpencode: {
			{"read", `{"filePath":"` + d.home + `/.local/share/opencode/storage/session/x.json"}`, true},
			{"list", `{"path":"` + d.home + `"}`, true},
			{"read", `{"filePath":"` + d.home + `/.local/share/opencode/tool-output/tool_1"}`, false},
			{"list", `{"path":"` + d.home + `/.local/share/opencode/tool-output"}`, true},
			{"edit", `{"filePath":"src/main.go","oldString":"a","newString":"b"}`, false},
		},
	} {
		cfg := testConfig(t, d, "kind: "+kind)
		a := newAgent(t, cfg, fw.NewMemLedger())
		r := &run{h: a.Runner().(*host), dir: d.ws}
		for _, c := range cases {
			if got := r.privatePath(c.tool, json.RawMessage(c.input)) != ""; got != c.denied {
				t.Errorf("%s: %s %s: denied = %v, want %v", kind, c.tool, c.input, got, c.denied)
			}
		}
	}
}
