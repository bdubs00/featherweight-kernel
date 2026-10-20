package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func newTestBash(t *testing.T) (*bashTool, string) {
	t.Helper()
	root := t.TempDir()
	cfg := fw.DefaultConfig()
	cfg.Tools.Files.Root = root
	tool, err := newBash(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b := tool.(*bashTool)
	t.Cleanup(func() { _ = b.Close() })
	return b, root
}

func TestBashEcho(t *testing.T) {
	b, _ := newTestBash(t)
	ctx := context.Background()
	got := mustOK(t, call(t, ctx, b, `{"command":"echo hello; echo oops >&2"}`))
	if got != "hello\noops\nexit code 0" {
		t.Fatalf("got %q", got)
	}
	got = mustOK(t, call(t, ctx, b, `{"command":"true"}`))
	if got != "exit code 0" {
		t.Fatalf("empty output: got %q", got)
	}
	got = mustOK(t, call(t, ctx, b, `{"command":"printf 'no newline'"}`))
	if got != "no newline\nexit code 0" {
		t.Fatalf("no newline: got %q", got)
	}
}

func TestBashStartsInRootAndKeepsState(t *testing.T) {
	b, root := newTestBash(t)
	ctx := context.Background()
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(root)
	got := mustOK(t, call(t, ctx, b, `{"command":"pwd -P"}`))
	if got != real+"\nexit code 0" {
		t.Fatalf("start dir: got %q, want %q", got, real)
	}
	mustOK(t, call(t, ctx, b, `{"command":"cd sub && export FW_TEST_VAR=kept"}`))
	got = mustOK(t, call(t, ctx, b, `{"command":"pwd -P; echo $FW_TEST_VAR"}`))
	if got != filepath.Join(real, "sub")+"\nkept\nexit code 0" {
		t.Fatalf("persisted state: got %q", got)
	}
}

func TestBashNonZeroExit(t *testing.T) {
	b, _ := newTestBash(t)
	ctx := context.Background()
	got := mustOK(t, call(t, ctx, b, `{"command":"echo before; false"}`))
	if got != "before\nexit code 1" {
		t.Fatalf("got %q", got)
	}
	got = mustOK(t, call(t, ctx, b, `{"command":"(exit 42)"}`))
	if got != "exit code 42" {
		t.Fatalf("got %q", got)
	}
	// A syntax error fails the command and keeps the shell.
	res := call(t, ctx, b, `{"command":"echo \"unclosed"}`)
	if res.IsError || !strings.Contains(text(res), "exit code 2") {
		t.Fatalf("syntax error: got %+v", res)
	}
	mustOK(t, call(t, ctx, b, `{"command":"echo still here"}`))
}

func TestBashRestart(t *testing.T) {
	b, _ := newTestBash(t)
	ctx := context.Background()
	mustOK(t, call(t, ctx, b, `{"command":"export FW_TEST_VAR=set; cd /"}`))
	got := mustOK(t, call(t, ctx, b, `{"restart":true}`))
	if !strings.Contains(got, "restarted") {
		t.Fatalf("restart: got %q", got)
	}
	got = mustOK(t, call(t, ctx, b, `{"command":"echo ${FW_TEST_VAR:-unset}; pwd"}`))
	if strings.HasPrefix(got, "set") || strings.Contains(got, "\n/\n") {
		t.Fatalf("state survived restart: %q", got)
	}
}

func TestBashExitAndTimeout(t *testing.T) {
	b, _ := newTestBash(t)
	ctx := context.Background()
	mustFail(t, call(t, ctx, b, `{"command":"echo bye; exit 3"}`), "exit status 3")
	mustOK(t, call(t, ctx, b, `{"command":"echo back"}`))
	mustFail(t, call(t, ctx, b, `{"command":"exec >&-"}`), "closed its standard output")
	mustOK(t, call(t, ctx, b, `{"command":"echo back"}`))

	b.timeout = 300 * time.Millisecond
	start := time.Now()
	mustFail(t, call(t, ctx, b, `{"command":"echo started; sleep 30"}`), "timed out")
	if time.Since(start) > 10*time.Second {
		t.Fatalf("timeout took %s", time.Since(start))
	}
	b.timeout = bashTimeout
	got := mustOK(t, call(t, ctx, b, `{"command":"echo fresh"}`))
	if got != "fresh\nexit code 0" {
		t.Fatalf("after timeout: got %q", got)
	}
}

func TestBashTruncates(t *testing.T) {
	b, _ := newTestBash(t)
	got := mustOK(t, call(t, context.Background(), b, `{"command":"head -c 3000000 /dev/zero | tr '\\0' a"}`))
	if !strings.Contains(got, "[output truncated: 3000000 bytes") || !strings.HasSuffix(got, "exit code 0") {
		t.Fatalf("truncation note missing: %q", got[max(0, len(got)-200):])
	}
	if len(got) > maxOutput+200 {
		t.Fatalf("output is %d bytes", len(got))
	}
}

func TestBashEmptyCommand(t *testing.T) {
	b, _ := newTestBash(t)
	mustFail(t, call(t, context.Background(), b, `{}`), "empty")
}

func TestBashProtocolIsRobust(t *testing.T) {
	b, _ := newTestBash(t)
	ctx := context.Background()
	// A command that reads stdin gets end of file, not the protocol.
	if got := mustOK(t, call(t, ctx, b, `{"command":"cat"}`)); got != "exit code 0" {
		t.Fatalf("cat: %q", got)
	}
	// A heredoc in the command works.
	if got := mustOK(t, call(t, ctx, b, `{"command":"cat <<'X'\nline one\nX"}`)); got != "line one\nexit code 0" {
		t.Fatalf("heredoc: %q", got)
	}
	// Tracing does not confuse the end marks.
	mustOK(t, call(t, ctx, b, `{"command":"set -xv"}`))
	got := mustOK(t, call(t, ctx, b, `{"command":"echo traced"}`))
	if !strings.HasPrefix(got, "traced\n") || !strings.HasSuffix(got, "exit code 0") {
		t.Fatalf("traced: %q", got)
	}
	mustOK(t, call(t, ctx, b, `{"command":"set +xv"}`))
	if got := mustOK(t, call(t, ctx, b, `{"command":"echo clean"}`)); got != "clean\nexit code 0" {
		t.Fatalf("after trace: %q", got)
	}
}

func TestBashMergedStreams(t *testing.T) {
	b, _ := newTestBash(t)
	ctx := context.Background()
	mustOK(t, call(t, ctx, b, `{"command":"exec 2>&1"}`))
	for i := 0; i < 3; i++ {
		got := mustOK(t, call(t, ctx, b, `{"command":"echo out; echo err >&2"}`))
		if got != "out\nerr\nexit code 0" {
			t.Fatalf("call %d after exec 2>&1: %q", i, got)
		}
	}
}

func TestBashConfigEnvAndTimeout(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "secret")
	t.Setenv("FW_API_TOKEN", "token")
	t.Setenv("FW_TEST_OVERRIDE", "process")
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Tools.Files.Root = t.TempDir()
	cfg.Tools.Bash.Timeout = 5 * time.Second
	cfg.Tools.Bash.Env = map[string]string{
		"FW_TEST_ENDPOINT": "http://api.test",
		"FW_TEST_OVERRIDE": "config",
		"FW_API_TOKEN":     "leak",
	}
	tool, err := newBash(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b := tool.(*bashTool)
	t.Cleanup(func() { _ = b.Close() })
	if b.timeout != 5*time.Second {
		t.Fatalf("timeout = %s", b.timeout)
	}
	if desc := b.Spec().Description; !strings.Contains(desc, "limit of 5s") || strings.Contains(desc, "100 KB") {
		t.Fatalf("description = %q", desc)
	}
	got := mustOK(t, call(t, context.Background(), b, `{"command":"echo \"$FW_TEST_ENDPOINT $FW_TEST_OVERRIDE $FW_HOME ${ANTHROPIC_API_KEY:-hidden} ${FW_API_TOKEN:-hidden}\""}`))
	if got != "http://api.test config "+cfg.Home+" hidden hidden\nexit code 0" {
		t.Fatalf("got %q", got)
	}
	// A restart keeps the configured variables.
	mustOK(t, call(t, context.Background(), b, `{"restart":true}`))
	if got := mustOK(t, call(t, context.Background(), b, `{"command":"echo $FW_TEST_ENDPOINT"}`)); got != "http://api.test\nexit code 0" {
		t.Fatalf("after restart: %q", got)
	}
}

// Each run has its own shell: the variables and the directory of one run
// never reach another, and the shell of a run that ended goes.
func TestBashShellPerRun(t *testing.T) {
	b, root := newTestBash(t)
	runA := fw.WithRunInfo(context.Background(), fw.RunInfo{RunID: "run-a"})
	runB := fw.WithRunInfo(context.Background(), fw.RunInfo{RunID: "run-b"})
	mustOK(t, call(t, runA, b, `{"command":"export FW_SHELL_TEST=one; mkdir -p sub; cd sub"}`))
	if got := mustOK(t, call(t, runA, b, `{"command":"echo [$FW_SHELL_TEST]; pwd"}`)); got != "[one]\n"+filepath.Join(root, "sub")+"\nexit code 0" {
		t.Fatalf("the same run lost its state: %q", got)
	}
	if got := mustOK(t, call(t, runB, b, `{"command":"echo [$FW_SHELL_TEST]; pwd"}`)); got != "[]\n"+root+"\nexit code 0" {
		t.Fatalf("another run saw the state of the first: %q", got)
	}
	if n := b.open(); n != 2 {
		t.Fatalf("sessions = %d, want 2", n)
	}
	b.CloseRun("run-a")
	if got := mustOK(t, call(t, runA, b, `{"command":"echo [$FW_SHELL_TEST]"}`)); got != "[]\nexit code 0" {
		t.Fatalf("the shell of a closed run kept its state: %q", got)
	}
	b.CloseRun("never-ran")
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if res, err := b.Call(runB, []byte(`{"command":"true"}`)); err != nil || !res.IsError {
		t.Fatalf("a call after Close = %+v, %v", res, err)
	}
}

// The shell gets no manifest of ax and no variable that holds a secret,
// so a model that runs env finds no credential of fw (section 7.2).
func TestBashEnvHidesSecrets(t *testing.T) {
	t.Setenv("AX_TASK_YAML", "spec: {}")
	t.Setenv("AX_WORKSPACES_YAML", "spec: {}")
	t.Setenv("GITHUB_TOKEN", "ghp_cccccccccccccccccccccccccccccccccccc")
	t.Setenv("FW_API_TOKEN_FILE", "/run/secrets/token")
	t.Setenv("DATABASE_URL", "postgres://app:s3cretpassw0rd@db:5432/app")
	t.Setenv("FW_TEST_PLAIN", "visible")
	b, _ := newTestBash(t)
	got := mustOK(t, call(t, context.Background(), b, `{"command":"env"}`))
	for _, name := range []string{"AX_TASK_YAML=", "AX_WORKSPACES_YAML=", "GITHUB_TOKEN=", "FW_API_TOKEN_FILE=", "DATABASE_URL="} {
		if strings.Contains(got, "\n"+name) || strings.HasPrefix(got, name) {
			t.Errorf("the shell got %s", strings.TrimSuffix(name, "="))
		}
	}
	if !strings.Contains(got, "FW_TEST_PLAIN=visible") {
		t.Errorf("the shell lost a plain variable:\n%s", got)
	}
}
