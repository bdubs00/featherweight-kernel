//go:build unix

package env

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	_ "github.com/bdubs00/featherweight-kernel/tools/builtin"
)

const fullBlock = `
python: pyproject.toml
tools: pixi.toml
prepare:
  - jq --version > prepared.txt && echo "$VIRTUAL_ENV" >> prepared.txt
`

// The block resolves against the root of the workspace, and a directory
// that the kernel or a run owns is refused.
func TestLoad(t *testing.T) {
	f := newFixture(t, fullBlock)
	s := f.s
	if s.dir != filepath.Join(f.root, ".environment") || s.cache != filepath.Join(f.root, ".cache") {
		t.Fatalf("dir %s, cache %s", s.dir, s.cache)
	}
	if s.python != filepath.Join(f.root, "pyproject.toml") || s.tools != filepath.Join(f.root, "pixi.toml") || s.placement != PlacementDurable || !s.brief {
		t.Fatalf("spec = %+v", s)
	}
	if got, err := load(fw.DefaultConfig()); got != nil || err != nil {
		t.Fatalf("no block: %v, %v", got, err)
	}
	for block, want := range map[string]string{
		"pyhton: pyproject.toml":            "is not a key",
		"placement: cloud":                  "is not durable or image",
		"python: requirements.txt":          "not a pyproject.toml",
		"tools: environment.yml":            "not a pixi.toml",
		"dir: .fw/env":                      "overlaps FW_HOME",
		"dir: .":                            "overlaps FW_HOME",
		"dir: x\ncache: x":                  "the same directory",
		"prepare: [\"  \"]":                 "prepare[0] is empty",
		"dir: runs/env":                     "directories of the runs",
		"placement: image\ndir: /workspace": "hides the image",
	} {
		g := newFixture(t, block)
		on := true
		g.cfg.Tools.Files.PerRun = &on
		if _, err := load(g.cfg); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", block, err, want)
		}
	}
	h := newFixture(t, "dir: .")
	h.cfg.Home = t.TempDir()
	if _, err := load(h.cfg); err == nil || !strings.Contains(err.Error(), "holds the root of the workspace") {
		t.Errorf("dir: . with FW_HOME elsewhere: err = %v", err)
	}
	img := newFixture(t, "placement: image\npython: pyproject.toml\n")
	if img.s.dir != imageDir {
		t.Fatalf("image dir = %s", img.s.dir)
	}
}

// The digest covers the manifests, the lock files, and the prepare
// commands, and nothing else.
func TestDigest(t *testing.T) {
	f := newFixture(t, fullBlock)
	d := f.s.digest()
	if !strings.HasPrefix(d, "sha256:") || f.s.digest() != d {
		t.Fatalf("digest = %s", d)
	}
	for _, name := range []string{"pyproject.toml", "uv.lock", "pixi.toml", "pixi.lock"} {
		old := f.read(filepath.Join(f.root, name))
		f.write(name, old+"# changed\n")
		if f.s.digest() == d {
			t.Errorf("a change of %s keeps the digest", name)
		}
		f.write(name, old)
	}
	if f.s.digest() != d {
		t.Fatal("the digest of the same files changed")
	}
	f.write("README.md", "other files do not count")
	if f.s.digest() != d {
		t.Fatal("another file changed the digest")
	}
	g := *f.s
	g.prepare = append(slices.Clone(g.prepare), "true")
	if g.digest() == d {
		t.Fatal("a prepare command keeps the digest")
	}
	_ = os.Remove(filepath.Join(f.root, "uv.lock"))
	if f.s.digest() == d {
		t.Fatal("a missing lock file keeps the digest")
	}
}

// The first tool process of a run installs the environment, writes the
// marker with the versions, runs the prepare commands with the variables
// of the environment, and records fw.env.ready. The next one finds it
// ready and runs no manager.
func TestInstallMarkerAndReady(t *testing.T) {
	t.Setenv("BILLING_SERVICE_TOKEN", "tok-0123456789abcdef")
	f := newFixture(t, fullBlock)
	ctx := f.runCtx()
	env, err := fw.ExtendToolEnv(ctx, f.cfg, []string{"PATH=/usr/bin:/bin", "HOME=/home/x", "VIRTUAL_ENV=/old"})
	if err != nil {
		t.Fatal(err)
	}
	m := f.s.readMarker()
	if m == nil || m.Digest != f.s.digest() || m.Placement != PlacementDurable || m.UV != "uv 0.0.0-fake" || m.Pixi != "pixi 0.0.0-fake" || m.Prepare != 1 {
		t.Fatalf("marker = %+v", m)
	}
	if m.Python == nil || m.Python.Version != "3.12.7" || len(m.Python.Packages) != 1 || m.Python.Packages[0] != (Package{Name: "pyyaml", Version: "6.0.2"}) {
		t.Fatalf("python = %+v", m.Python)
	}
	prefix := filepath.Join(f.s.pixiEnvs(), "example-123", "envs", "default")
	if m.Tools == nil || m.Tools.Prefix != prefix || len(m.Tools.Packages) != 2 || !m.Tools.Packages[0].Explicit {
		t.Fatalf("tools = %+v", m.Tools)
	}
	if got := f.read(filepath.Join(f.root, "prepared.txt")); got != "jq-1.7.1\n"+f.s.venv()+"\n" {
		t.Fatalf("prepare wrote %q", got)
	}
	// The variables of the environment reach the tool process.
	wantPath := strings.Join([]string{filepath.Join(f.s.venv(), "bin"), filepath.Join(prefix, "bin"), f.s.binDir(), filepath.Join(f.s.pixiHome(), "bin"), "/usr/bin:/bin"}, ":")
	for name, want := range map[string]string{
		"PATH":                   wantPath,
		"VIRTUAL_ENV":            f.s.venv(),
		"UV_PROJECT_ENVIRONMENT": f.s.venv(),
		"UV_CACHE_DIR":           filepath.Join(f.s.cache, "uv"),
		"UV_PYTHON_INSTALL_DIR":  filepath.Join(f.s.dir, "python"),
		"UV_TOOL_BIN_DIR":        f.s.binDir(),
		"PIXI_HOME":              f.s.pixiHome(),
		"PIXI_CACHE_DIR":         filepath.Join(f.s.cache, "pixi"),
		"CONDA_PREFIX":           prefix,
		"HOME":                   "/home/x",
	} {
		if got := envValue(env, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if n := strings.Count(strings.Join(env, "\n"), "VIRTUAL_ENV="); n != 1 {
		t.Errorf("VIRTUAL_ENV appears %d times", n)
	}
	// A manager gets the scrubbed environment of a tool process.
	uvEnv := f.read(filepath.Join(f.logs, "uv-env"))
	if strings.Contains(uvEnv, "tok-0123456789abcdef") || strings.Contains(uvEnv, "BILLING_SERVICE_TOKEN") {
		t.Fatal("a manager got a secret of fw")
	}
	if !strings.Contains(uvEnv, "UV_PROJECT_ENVIRONMENT="+f.s.venv()) || !strings.Contains(f.read(filepath.Join(f.logs, "pixi-env")), "PIXI_HOME="+f.s.pixiHome()) {
		t.Fatal("a manager missed the variables of the environment")
	}
	if !strings.Contains(f.read(filepath.Join(f.s.pixiHome(), "config.toml")), `detached-environments = "`+f.s.pixiEnvs()+`"`) {
		t.Fatal("pixi is not configured for detached environments")
	}
	evs := f.events(1)
	if len(evs) != 1 || evs[0].Type != EvReady || evs[0].Subject != "" || evs[0].Node == "" {
		t.Fatalf("events = %+v", evs)
	}
	if got := decode[Marker](t, evs[0]); got.Digest != m.Digest || got.Python == nil {
		t.Fatalf("ready data = %+v", got)
	}
	syncs := f.count("uv sync")
	if _, err := fw.ExtendToolEnv(ctx, f.cfg, nil); err != nil {
		t.Fatal(err)
	}
	if f.count("uv sync") != syncs || len(f.settled()) != 1 {
		t.Fatal("a ready environment installed again")
	}
	if st := f.s.state(); st.Kind != StateReady {
		t.Fatalf("state = %s", st.Kind)
	}
	// A new digest installs again.
	f.write("uv.lock", "version = 1\npackage pyyaml 6.0.2\npackage six 1.17.0\n")
	if _, err := fw.ExtendToolEnv(ctx, f.cfg, nil); err != nil {
		t.Fatal(err)
	}
	if f.count("uv sync") != syncs+1 || len(f.s.readMarker().Python.Packages) != 2 {
		t.Fatal("a new lock file did not install again")
	}
}

// A tool process outside a run gets the variables and waits for nothing.
func TestToolEnvOutsideRun(t *testing.T) {
	f := newFixture(t, fullBlock)
	env, err := fw.ExtendToolEnv(context.Background(), f.cfg, []string{"PATH=/bin"})
	if err != nil {
		t.Fatal(err)
	}
	if envValue(env, "VIRTUAL_ENV") != f.s.venv() || !strings.HasPrefix(envValue(env, "PATH"), filepath.Join(f.s.venv(), "bin")+":") {
		t.Fatalf("env = %v", env)
	}
	if len(f.calls()) != 1 || f.calls()[0] != "" {
		t.Fatalf("calls = %v", f.calls())
	}
	// Without the block the environment stays as it is.
	env, err = fw.ExtendToolEnv(context.Background(), fw.DefaultConfig(), []string{"PATH=/bin"})
	if err != nil || !slices.Equal(env, []string{"PATH=/bin"}) {
		t.Fatalf("env = %v, err = %v", env, err)
	}
}

// A failed install records fw.env.failed with the end of the output and
// no password, and a tool process fails with the reason until the next
// try is due. The next try that succeeds removes the failure.
func TestFailureAndRetry(t *testing.T) {
	f := newFixture(t, fullBlock)
	f.write("fail", "")
	ctx := f.runCtx()
	_, err := fw.ExtendToolEnv(ctx, f.cfg, nil)
	if err == nil || !strings.Contains(err.Error(), "step python") || !strings.Contains(err.Error(), "uv sync --frozen") || !strings.Contains(err.Error(), "Failed to fetch") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatal("the error holds the password of a URL")
	}
	fl := f.s.readFailure()
	if fl == nil || fl.Step != "python" || fl.Attempt != 1 || !fl.RetryAt.After(time.Now()) || strings.Contains(fl.Output, "hunter2") || !strings.Contains(fl.Output, "@index.example/simple/pyyaml") {
		t.Fatalf("failure = %+v", fl)
	}
	evs := f.events(1)
	if len(evs) != 1 || evs[0].Type != EvFailed {
		t.Fatalf("events = %+v", evs)
	}
	if got := decode[Failure](t, evs[0]); got.Step != "python" || strings.Contains(got.Output, "hunter2") {
		t.Fatalf("failed data = %+v", got)
	}
	if st := f.s.state(); st.Kind != StateFailed {
		t.Fatalf("state = %s", st.Kind)
	}
	// Before the next try, a tool process fails at once.
	syncs := f.count("uv sync")
	if _, err := fw.ExtendToolEnv(ctx, f.cfg, nil); err == nil || !strings.Contains(err.Error(), "tries again in") {
		t.Fatalf("err = %v", err)
	}
	if f.count("uv sync") != syncs {
		t.Fatal("a failed environment installed again before its next try")
	}
	// The next try that fails the same way stays quiet.
	fl.RetryAt = time.Now().Add(-time.Second)
	if err := writeFile(f.s.failurePath(), fl); err != nil {
		t.Fatal(err)
	}
	if _, err := fw.ExtendToolEnv(ctx, f.cfg, nil); err == nil {
		t.Fatal("the failure went away")
	}
	if got := f.s.readFailure(); got.Attempt != 2 || got.RetryAt.Sub(got.Time) != backoff(2) {
		t.Fatalf("failure = %+v", got)
	}
	if len(f.settled()) != 1 {
		t.Fatal("a repeated failure recorded a second event")
	}
	// A try after the fix succeeds.
	_ = os.Remove(filepath.Join(f.root, "fail"))
	fl = f.s.readFailure()
	fl.RetryAt = time.Now().Add(-time.Second)
	if err := writeFile(f.s.failurePath(), fl); err != nil {
		t.Fatal(err)
	}
	if _, err := fw.ExtendToolEnv(ctx, f.cfg, nil); err != nil {
		t.Fatal(err)
	}
	if f.s.readFailure() != nil || f.s.state().Kind != StateReady {
		t.Fatal("a successful try kept the failure")
	}
	if evs := f.events(2); len(evs) != 2 || evs[1].Type != EvReady {
		t.Fatalf("events = %+v", evs)
	}
}

// A tool process waits while another holder installs the environment, and
// its context bounds the wait.
func TestWaitForAnotherInstall(t *testing.T) {
	f := newFixture(t, fullBlock)
	if err := os.MkdirAll(f.s.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	unlock, ok, err := tryLock(f.s.lockPath())
	if err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	if st := f.s.state(); st.Kind != StateMaterializing {
		t.Fatalf("state = %s", st.Kind)
	}
	ctx, cancel := context.WithTimeout(f.runCtx(), 300*time.Millisecond)
	defer cancel()
	if _, err := fw.ExtendToolEnv(ctx, f.cfg, nil); err == nil || !strings.Contains(err.Error(), "not ready yet (materializing)") {
		t.Fatalf("err = %v", err)
	}
	if f.count("uv") != 0 {
		t.Fatal("a waiter installed beside the holder")
	}
	done := make(chan error, 1)
	go func() {
		_, err := fw.ExtendToolEnv(f.runCtx(), f.cfg, nil)
		done <- err
	}()
	time.Sleep(2 * pollEvery)
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the waiter did not go on after the holder")
	}
	if f.s.state().Kind != StateReady {
		t.Fatal("not ready")
	}
}

// The shell of bash waits for the environment, finds its programs on
// PATH, and fails with the reason when the install failed.
func TestBashGetsTheEnvironment(t *testing.T) {
	f := newFixture(t, fullBlock)
	bash, err := fw.NewBuiltinTool("bash", f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bash.(interface{ Close() error }).Close() })
	res, err := bash.Call(f.runCtx(), json.RawMessage(`{"command":"echo \"$VIRTUAL_ENV\"; jq --version; python --version"}`))
	if err != nil || res.IsError {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
	out := fw.TextOf(res.Content)
	for _, want := range []string{f.s.venv(), "jq-1.7.1", "Python 3.12.7"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output %q lacks %q", out, want)
		}
	}
	g := newFixture(t, fullBlock)
	g.write("fail", "")
	bash2, err := fw.NewBuiltinTool("bash", g.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bash2.(interface{ Close() error }).Close() })
	res, err = bash2.Call(g.runCtx(), json.RawMessage(`{"command":"true"}`))
	if err != nil || !res.IsError || !strings.Contains(fw.TextOf(res.Content), "the environment failed to install at the step python") {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
}

// The service installs the environment in the background and stops on
// Close. With placement image it starts nothing.
func TestService(t *testing.T) {
	f := newFixture(t, fullBlock)
	c, err := Start(context.Background(), f.agent())
	if err != nil || c == nil {
		t.Fatalf("Start = %v, %v", c, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for f.s.state().Kind != StateReady {
		if time.Now().After(deadline) {
			t.Fatal("the service did not install the environment")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	g := newFixture(t, "placement: image\ndir: "+filepath.Join(t.TempDir(), "img")+"\npython: pyproject.toml\n")
	if c, err := Start(context.Background(), g.agent()); c != nil || err != nil {
		t.Fatalf("image Start = %v, %v", c, err)
	}
	bad := newFixture(t, "python: pyproject.toml\n")
	bad.cfg.Extensions[Block].Content[1].Value = "setup.py"
	if _, err := Start(context.Background(), bad.agent()); err == nil {
		t.Fatal("a bad block started")
	}
}

// Without an agent, the package still removes the password of a URL from
// the output of a manager.
func TestRedactWithoutAgent(t *testing.T) {
	in := &installer{}
	if got := in.redact("GET https://bot:s3cr3t@pkgs.example/simple ok"); got != "GET https://***@pkgs.example/simple ok" {
		t.Fatalf("redact = %q", got)
	}
}

// A marker that the tool user could have written counts as absent: the
// state files must belong to fw.
func TestMarkerOwner(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root owns every file it writes")
	}
	f := newFixture(t, fullBlock)
	if _, err := fw.ExtendToolEnv(f.runCtx(), f.cfg, nil); err != nil {
		t.Fatal(err)
	}
	if f.s.readMarker() == nil {
		t.Fatal("no marker")
	}
	// With a tool user, the file of another user would not count. The
	// test cannot change the owner, so it checks the rule on itself.
	info, err := os.Stat(f.s.markerPath())
	if err != nil || !ownedBySelf(info) {
		t.Fatalf("the marker is not owned by this process: %v", err)
	}
	other, err := os.Stat("/")
	if err != nil || ownedBySelf(other) {
		t.Fatal("ownedBySelf trusts a directory of root")
	}
}
