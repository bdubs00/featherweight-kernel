package cli

import (
	"context"
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

	"github.com/bdubs00/featherweight-kernel/ax"
	"github.com/bdubs00/featherweight-kernel/fw"
)

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestBootHandlerSwap(t *testing.T) {
	sw := newSwitchHandler(bootHandler())
	srv := httptest.NewServer(sw)
	defer srv.Close()

	for path, want := range map[string]int{"/healthz": 200, "/readyz": 503, "/v1/runs": 503, "/mcp": 503} {
		if code, _ := get(t, srv.URL+path); code != want {
			t.Errorf("boot %s: %d, want %d", path, code, want)
		}
	}

	a, err := fw.New(fw.WithoutDefaults(), fw.WithAuthn(fw.NoAuthn{}), fw.WithModel(&scriptModel{}),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()),
		fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	h, err := a.Handler()
	if err != nil {
		t.Fatal(err)
	}
	a.SetReady(true)
	sw.Set(h)
	if code, _ := get(t, srv.URL+"/readyz"); code != 200 {
		t.Errorf("readyz after the swap: %d", code)
	}
	if code, _ := get(t, srv.URL+"/healthz"); code != 200 {
		t.Errorf("healthz after the swap: %d", code)
	}
}

// freeAddr returns a local address that nothing listens on.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// waitFor polls url until it answers want or the time runs out.
func waitFor(t *testing.T, url string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if code, _ := get(t, url); code == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never answered %d", url, want)
}

// serveInBackground runs the command line until the returned stop function
// is called, and then returns its exit code and streams.
func serveInBackground(t *testing.T, args ...string) func() (int, string, string) {
	t.Helper()
	stop, _ := serveWatched(t, args...)
	return stop
}

// waitOutput polls the output of a command line until it holds want, or
// the time runs out. A count of model calls says only that a call began,
// not that its run recorded the answer and the command printed it.
func waitOutput(t *testing.T, stdout func() string, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(stdout(), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the output never held %q:\n%s", want, stdout())
}

// serveWatched is serveInBackground that also returns a reader of the
// output while the command line runs.
func serveWatched(t *testing.T, args ...string) (func() (int, string, string), func() string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr syncBuffer
	done := make(chan int, 1)
	go func() { done <- Run(ctx, args, strings.NewReader(""), &stdout, &stderr) }()
	t.Cleanup(cancel)
	return func() (int, string, string) {
		cancel()
		select {
		case code := <-done:
			return code, stdout.String(), stderr.String()
		case <-time.After(20 * time.Second):
			t.Fatal("serve did not stop")
			return -1, "", ""
		}
	}, stdout.String
}

func TestServePlain(t *testing.T) {
	env(t, fw.WithModel(&scriptModel{}))
	t.Setenv("FW_AUTH_MODE", "none")
	addr := freeAddr(t)
	stop := serveInBackground(t, "--listen", addr, "serve")
	waitFor(t, "http://"+addr+"/readyz", 200)
	if code, _, errOut := stop(); code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
}

func TestServeNeedsTokenInTokenMode(t *testing.T) {
	env(t, fw.WithModel(&scriptModel{}))
	code, _, errOut := run(t, "", "--listen", freeAddr(t), "serve")
	if code != 1 || !strings.Contains(errOut, "FW_API_TOKEN") {
		t.Errorf("exit %d\n%s", code, errOut)
	}
}

// writeManifests writes a Task and a Workspace for a workspace under dir.
// The Workspace ships fw.yaml and a profile, and the binding has a goal.
func writeManifests(t *testing.T, dir, command string) (taskFile, wsFile, wsPath string) {
	t.Helper()
	wsPath = filepath.Join(dir, "ws")
	task := `apiVersion: ax.io/v1alpha1
kind: Task
metadata: {name: tester, atespace: team}
spec:
  image: fw
` + command + `
  workspaces:
    - name: tester-ws
      path: ` + wsPath + `
      goal: "Install the tools."
`
	ws := `apiVersion: ax.io/v1alpha1
kind: Workspace
metadata: {name: tester-ws, atespace: team}
spec:
  files:
    - path: .fw/fw.yaml
      content: |
        auth: {mode: none}
        a2a: {name: tester}
        tools: {user: none}     # as root, the boot takes the tool user of the images
    - path: .fw/agents/helper.yaml
      content: |
        system: You help.
`
	taskFile = filepath.Join(dir, "task.yaml")
	wsFile = filepath.Join(dir, "ws.yaml")
	if err := os.WriteFile(taskFile, []byte(task), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wsFile, []byte(ws), 0o644); err != nil {
		t.Fatal(err)
	}
	return taskFile, wsFile, wsPath
}

func TestServeAXBoot(t *testing.T) {
	m := &scriptModel{resps: []*fw.Response{endTurn("bootstrapped"), endTurn("command output")}}
	env(t, fw.WithModel(m))
	dir := t.TempDir()
	taskFile, wsFile, wsPath := writeManifests(t, dir, `  command: ["fw", "run", "--agent", "helper", "do the task"]`)
	home := filepath.Join(wsPath, ".fw")
	addr := freeAddr(t)

	stop, stdout := serveWatched(t, "serve", "--task-file", taskFile, "--workspace-file", wsFile, "--listen", addr, "--metadata-port", "0", "--home", home)
	waitFor(t, "http://"+addr+"/readyz", 200)

	// Step 3 prepared the workspace and wrote the marker under the home.
	if _, err := os.Stat(filepath.Join(ax.MarkerDir(home), ax.MarkerName(wsPath))); err != nil {
		t.Errorf("no marker: %v", err)
	}
	// Step 5 ran the goal with the bootstrap profile, and step 7 ran the
	// command in process with the profile from the agents directory.
	deadline := time.Now().Add(10 * time.Second)
	for len(m.requests()) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	reqs := m.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d model calls, want 2", len(reqs))
	}
	if got := fw.TextOf(reqs[0].Messages[0].Content); got != ax.BootstrapInput("Install the tools.", wsPath) || reqs[0].System != ax.BootstrapSystem {
		t.Errorf("bootstrap request: system %q input %q", reqs[0].System, got)
	}
	if got := fw.TextOf(reqs[1].Messages[0].Content); got != "do the task" || reqs[1].System != "You help." {
		t.Errorf("command request: system %q input %q", reqs[1].System, got)
	}
	// The metadata of the Task is stashed for the metadata endpoints.
	if task, _ := ax.Stashed(); task == nil || task.Metadata.Name != "tester" {
		t.Errorf("stashed task %+v", task)
	}
	waitOutput(t, stdout, "command output\n")

	code, out, errOut := stop()
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "command output\n") {
		t.Errorf("stdout %q", out)
	}
	if !strings.Contains(errOut, "fw: ax boot: ready") || !strings.Contains(errOut, "fw.run.finished outcome=completed") {
		t.Errorf("stderr:\n%s", errOut)
	}

	// A second boot finds the marker, skips the goal, and follows the same
	// command run instead of starting it again.
	stop, stdout = serveWatched(t, "serve", "--task-file", taskFile, "--workspace-file", wsFile, "--listen", addr, "--metadata-port", "0", "--home", home)
	waitFor(t, "http://"+addr+"/readyz", 200)
	waitOutput(t, stdout, "command output\n")
	code, out, errOut = stop()
	if code != 0 {
		t.Fatalf("second boot: exit %d\n%s", code, errOut)
	}
	if n := len(m.requests()); n != 2 {
		t.Errorf("second boot: %d model calls, want 2", n)
	}
	if !strings.Contains(out, "command output\n") {
		t.Errorf("second boot stdout %q", out)
	}
}

func TestServeAXSupervisesCommand(t *testing.T) {
	env(t, fw.WithModel(&scriptModel{resps: []*fw.Response{endTurn("bootstrapped")}}))
	dir := t.TempDir()
	out := filepath.Join(dir, "metadata-url")
	taskFile, wsFile, wsPath := writeManifests(t, dir, `  command: ["sh", "-c", "pwd > `+out+`.dir; echo $AX_METADATA_URL > `+out+`"]`)
	addr := freeAddr(t)
	stop := serveInBackground(t, "serve", "--task-file", taskFile, "--workspace-file", wsFile, "--listen", addr, "--metadata-port", "0", "--home", filepath.Join(wsPath, ".fw"))
	waitFor(t, "http://"+addr+"/readyz", 200)

	deadline := time.Now().Add(10 * time.Second)
	var got []byte
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(out); err == nil && len(b) > 0 {
			got = b
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The metadata endpoints answer on a loopback port of their own, not on
	// the listener of the surfaces, as the stock runner does.
	metaURL := strings.TrimSpace(string(got))
	if !strings.HasPrefix(metaURL, "http://127.0.0.1:") || metaURL == "http://"+addr {
		t.Fatalf("AX_METADATA_URL %q, listener %s", metaURL, addr)
	}
	if code, body := get(t, metaURL+"/metadata/v1alpha1/ax/task"); code != 200 || !strings.Contains(body, "name: tester") {
		t.Errorf("task metadata: %d %q", code, body)
	}
	if code, _ := get(t, "http://"+addr+"/metadata/v1alpha1/ax/task"); code != 404 {
		t.Errorf("the listener of the surfaces serves the metadata: %d", code)
	}
	if b, _ := os.ReadFile(out + ".dir"); strings.TrimSpace(string(b)) != wsPath {
		t.Errorf("working directory %q, want %q", b, wsPath)
	}
	// fw keeps serving after the command exits.
	time.Sleep(100 * time.Millisecond)
	if code, _ := get(t, "http://"+addr+"/healthz"); code != 200 {
		t.Errorf("healthz after the command exited: %d", code)
	}
	if code, _, errOut := stop(); code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
}
