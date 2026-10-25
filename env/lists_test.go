//go:build unix

package env

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/env/pkglist"
	"github.com/bdubs00/featherweight-kernel/fw"
)

const listBlock = `
packages:
  python: [ansible-lint, " pyyaml>=6 "]
  tools: ["nodejs>=22", jq]
mirrors:
  python: http://pkg-cache.fw-fleet.svc.cluster.local:8080/pypi/simple/
  tools: http://pkg-cache.fw-fleet.svc.cluster.local:8080/conda-forge
`

// The keys packages and mirrors resolve to the lists and the mirrors of
// the spec, and a list or a mirror that does not fit stops the start.
func TestLoadLists(t *testing.T) {
	f := newFixture(t, listBlock)
	s := f.s
	if s == nil {
		t.Fatal("the block did not load")
	}
	if !slices.Equal(s.inline[pkglist.Python], []string{"ansible-lint", "pyyaml>=6"}) || !slices.Equal(s.inline[pkglist.Tools], []string{"nodejs>=22", "jq"}) {
		t.Fatalf("inline = %v", s.inline)
	}
	if s.mirrors[pkglist.Python] != "http://pkg-cache.fw-fleet.svc.cluster.local:8080/pypi/simple" || s.mirrors[pkglist.Tools] != "http://pkg-cache.fw-fleet.svc.cluster.local:8080/conda-forge" {
		t.Fatalf("mirrors = %v", s.mirrors)
	}
	empty := newFixture(t, "packages:\n  python: []\n")
	if l, ok := empty.s.inline[pkglist.Python]; !ok || len(l) != 0 {
		t.Fatalf("an empty list: %v %v", l, ok)
	}
	if _, ok := empty.s.inline[pkglist.Tools]; ok {
		t.Fatal("an absent list is present")
	}
	long := "packages:\n  tools: [" + strings.Repeat("a,", pkglist.MaxItems) + "a]\n"
	for block, want := range map[string]string{
		"packages:\n  python: [six]\npython: pyproject.toml":      "packages.python and environment.python cannot both exist",
		"packages:\n  tools: [jq]\ntools: pixi.toml":              "packages.tools and environment.tools cannot both exist",
		"packages:\n  pythn: [six]":                               "environment.packages.pythn is not a key",
		"mirrors:\n  pip: http://x/simple":                        "environment.mirrors.pip is not a key",
		"packages:\n  python: [six, \"--index-url=http://evil\"]": "packages.python item 1 starts with a dash",
		"packages:\n  tools: [\"jq\\u0007\"]":                     "packages.tools item 0 holds a control character",
		"packages:\n  python: [" + strings.Repeat("a", 201) + "]": "longer than 200 characters",
		long: "holds 101 items; the limit is 100",
		"mirrors:\n  python: ftp://mirror/simple":                       "mirrors.python is not an http or an https URL",
		"mirrors:\n  tools: http://bot:s3cr3t@mirror/conda-forge":       "mirrors.tools holds a user or a password",
		"placement: image\ndir: /opt/env\npackages:\n  python: [six]\n": "needs placement durable",
	} {
		g := newFixture(t, block)
		_, err := load(g.cfg)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", block, err, want)
		}
		if err != nil && strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("%q: the error holds the password", block)
		}
	}
}

const mirrorBlock = `
packages:
  python: ["pyyaml>=6", ansible-lint]
  tools: [jq]
mirrors:
  python: http://mirror.test:8080/pypi/simple
  tools: http://mirror.test:8080/conda-forge
`

// The lists install through generated manifests: uv locks and syncs with
// the Python of the image and the mirror as its index, and pixi locks and
// installs for this platform with the mirror of conda-forge. Every tool
// process gets the mirror.
func TestListInstall(t *testing.T) {
	f := newFixture(t, mirrorBlock)
	s := f.s
	src := filepath.Join(s.dir, "src", "python")
	if s.python != filepath.Join(src, "pyproject.toml") || s.tools != filepath.Join(s.dir, "src", "tools", "pixi.toml") {
		t.Fatalf("manifests %s, %s", s.python, s.tools)
	}
	env, err := fw.ExtendToolEnv(f.runCtx(), f.cfg, []string{"PATH=/usr/bin:/bin"})
	if err != nil {
		t.Fatal(err)
	}
	py := filepath.Join(filepath.Dir(f.logs), "bin", "python3")
	calls := strings.Join(f.calls(), "\n")
	for _, want := range []string{
		"python3 --version",
		"uv lock --project " + src + " --python " + py,
		"uv sync --locked --no-install-project --project " + src + " --python " + py,
		"pixi lock --manifest-path " + s.tools,
		"pixi install --locked --manifest-path " + s.tools,
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("calls lack %q:\n%s", want, calls)
		}
	}
	if got := f.read(s.python); !strings.Contains(got, "name = \"fw-environment\"") || !strings.Contains(got, "requires-python = \"==3.12.*\"") || !strings.Contains(got, "dependencies = [\n  \"ansible-lint\",\n  \"pyyaml>=6\",\n]") {
		t.Errorf("pyproject.toml:\n%s", got)
	}
	if got := f.read(s.tools); !strings.Contains(got, "channels = [\"conda-forge\"]") || !strings.Contains(got, "platforms = [\""+condaPlatform()+"\"]") || !strings.Contains(got, "\n\"jq\" = \"*\"\n") {
		t.Errorf("pixi.toml:\n%s", got)
	}
	if got := f.read(filepath.Join(s.pixiHome(), "config.toml")); !strings.Contains(got, "[mirrors]\n\"https://conda.anaconda.org/conda-forge\" = [\"http://mirror.test:8080/conda-forge\"]") {
		t.Errorf("config.toml:\n%s", got)
	}
	uvEnv := f.read(filepath.Join(f.logs, "uv-env"))
	for _, want := range []string{"UV_DEFAULT_INDEX=http://mirror.test:8080/pypi/simple", "PIP_INDEX_URL=http://mirror.test:8080/pypi/simple", "PIP_TRUSTED_HOST=mirror.test", "UV_PYTHON_DOWNLOADS=never", "UV_PROJECT_ENVIRONMENT=" + s.venv()} {
		if !strings.Contains(uvEnv, want+"\n") {
			t.Errorf("uv lacks %s", want)
		}
	}
	for name, want := range map[string]string{"UV_DEFAULT_INDEX": "http://mirror.test:8080/pypi/simple", "PIP_INDEX_URL": "http://mirror.test:8080/pypi/simple", "PIP_TRUSTED_HOST": "mirror.test", "VIRTUAL_ENV": s.venv()} {
		if got := envValue(env, name); got != want {
			t.Errorf("tool process %s = %q, want %q", name, got, want)
		}
	}
	m := s.readMarker()
	if m == nil || m.Python == nil || m.Tools == nil || m.Tools.Prefix != filepath.Join(s.pixiEnvs(), "fw-environment-123", "envs", "default") {
		t.Fatalf("marker = %+v", m)
	}
	explicit := map[string]bool{}
	for _, p := range m.Python.Packages {
		explicit[p.Name] = p.Explicit
	}
	if !explicit["pyyaml"] || !explicit["ansible-lint"] {
		t.Errorf("python packages = %+v", m.Python.Packages)
	}
	// The digest covers the sorted lists and the mirrors, not the files
	// that the kernel writes.
	d := s.digest()
	if err := os.WriteFile(s.uvLock(), []byte("version = 1\npackage pyyaml 9.9.9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if s.digest() != d || s.state().Kind != StateReady {
		t.Fatal("a change of the generated lock file changed the digest")
	}
	f.reblock(strings.Replace(mirrorBlock, `["pyyaml>=6", ansible-lint]`, `[ansible-lint, "pyyaml>=6"]`, 1))
	if f.s.digest() != d {
		t.Fatal("the order of the list changed the digest")
	}
	f.reblock(strings.Replace(mirrorBlock, "8080/conda-forge", "8081/conda-forge", 1))
	if f.s.digest() == d {
		t.Fatal("a new mirror kept the digest")
	}
	f.reblock(strings.Replace(mirrorBlock, "[jq]", "[jq, nodejs>=22]", 1))
	if f.s.digest() == d {
		t.Fatal("a new package kept the digest")
	}
}

// Empty lists are ready with no manager, no network, and no virtual
// environment.
func TestListEmpty(t *testing.T) {
	f := newFixture(t, "packages:\n  python: []\n  tools: []\n")
	t.Setenv("PATH", filepath.Join(t.TempDir(), "empty"))
	env, err := fw.ExtendToolEnv(f.runCtx(), f.cfg, []string{"PATH=/bin"})
	if err != nil {
		t.Fatal(err)
	}
	if st := f.s.state(); st.Kind != StateReady || st.Marker.Python != nil || st.Marker.Tools != nil {
		t.Fatalf("state = %+v", st)
	}
	if envValue(env, "VIRTUAL_ENV") != "" || envValue(env, "UV_DEFAULT_INDEX") != "" {
		t.Fatalf("env = %v", env)
	}
	if calls := f.calls(); len(calls) != 1 || calls[0] != "" {
		t.Fatalf("calls = %v", calls)
	}
}

// add and remove change the file of the local changes and install the
// effective list. A package that does not resolve or does not install
// leaves the list as it was. A change of the configured lists drops the
// local changes.
func TestListToolAddRemove(t *testing.T) {
	f := newFixture(t, "packages:\n  python: [pyyaml]\n  tools: [jq]\n")
	tl, err := newTool(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := f.runCtx()
	if _, err := fw.ExtendToolEnv(ctx, f.cfg, nil); err != nil {
		t.Fatal(err)
	}
	s := f.s
	before := s.digest()
	res := callTool(t, ctx, tl, `{"command":"add","manager":"python","package":"six>=1.16"}`)
	if res.IsError || !strings.Contains(fw.TextOf(res.Content), "Added six>=1.16 in the python list") {
		t.Fatalf("add = %+v", res)
	}
	if got := s.effective(pkglist.Python); !slices.Equal(got, []string{"pyyaml", "six>=1.16"}) {
		t.Fatalf("effective = %v", got)
	}
	loc := s.readLocal()
	if loc == nil || loc.Configured != s.listsDigest() || len(loc.Changes) != 1 || loc.Changes[0].Run != "run-1" || loc.Changes[0].Package != "six>=1.16" {
		t.Fatalf("local = %+v", loc)
	}
	if !strings.Contains(f.read(s.python), `"six>=1.16"`) {
		t.Fatal("the manifest lacks the package")
	}
	m := s.readMarker()
	if m.Digest == before || m.Digest != s.digest() || len(m.Python.Packages) != 2 {
		t.Fatalf("marker = %+v", m)
	}
	// The install of the first list and of the change send fw.env.ready,
	// and the change sends fw.env.changed after it.
	evs := f.events(3)
	if len(evs) != 3 || evs[1].Type != EvReady || evs[2].Type != EvChanged {
		t.Fatalf("events = %+v", evs)
	}
	if ch := decode[ChangedData](t, evs[2]); ch.Previous != before || ch.Digest != m.Digest || ch.Manager != "python" || ch.Command != "add" || ch.Package != "six>=1.16" || ch.Agent != "worker" {
		t.Fatalf("changed = %+v", ch)
	}
	// An add of a package in the list takes the place of its requirement.
	if res := callTool(t, ctx, tl, `{"command":"add","manager":"python","package":"PyYAML>=6"}`); res.IsError {
		t.Fatalf("add = %+v", res)
	}
	if got := s.effective(pkglist.Python); !slices.Equal(got, []string{"six>=1.16", "PyYAML>=6"}) {
		t.Fatalf("effective = %v", got)
	}
	for _, again := range []string{"PyYAML>=6", "six>=1.16"} {
		if res := callTool(t, ctx, tl, `{"command":"add","manager":"python","package":"`+again+`"}`); res.IsError || !strings.Contains(fw.TextOf(res.Content), "holds "+again+" already") {
			t.Fatalf("same add = %+v", res)
		}
	}
	n := len(f.settled())
	for input, want := range map[string]string{
		`{"command":"remove","manager":"python","package":"requests"}`:   "holds no package requests. The list is as it was.",
		`{"command":"add","manager":"python","package":"nosuch-pkg"}`:    "does not resolve: python: uv lock: exit status 1. The list and the environment are as they were. The output of the manager ends with:\nerror: No solution found",
		`{"command":"add","manager":"tools","package":"nosuch"}`:         "does not resolve: tools: pixi lock: exit status 1",
		`{"command":"add","manager":"tools","package":"conda-forge::x"}`: "a channel or a bracket is not available",
		`{"command":"add","manager":"python","package":"nobuild"}`:       "failed to install, so the list is as it was",
		`{"command":"add","manager":"python","package":"-e ."}`:          "starts with a dash",
	} {
		res := callTool(t, ctx, tl, input)
		if !res.IsError || !strings.Contains(fw.TextOf(res.Content), want) {
			t.Errorf("%s = %s, want %q", input, fw.TextOf(res.Content), want)
		}
		if strings.Contains(fw.TextOf(res.Content), "hunter2") {
			t.Errorf("%s: the result holds a password", input)
		}
	}
	if got := s.effective(pkglist.Python); !slices.Equal(got, []string{"six>=1.16", "PyYAML>=6"}) {
		t.Fatalf("a refused change changed the list: %v", got)
	}
	if !strings.Contains(f.read(s.python), `"six>=1.16"`) || strings.Contains(f.read(s.python), "nosuch") {
		t.Fatalf("a refused change kept the manifest:\n%s", f.read(s.python))
	}
	if st := s.state(); st.Kind != StateReady || s.readFailure() != nil {
		t.Fatalf("after the refusals: %s, failure %+v", st.Kind, s.readFailure())
	}
	if got := len(f.settled()); got != n {
		t.Fatalf("a refused change sent %d events", got-n)
	}
	// A remove of the last tool removes the pixi environment.
	prefix := s.readMarker().Tools.Prefix
	if res := callTool(t, ctx, tl, `{"command":"remove","manager":"tools","package":"jq"}`); res.IsError {
		t.Fatalf("remove = %+v", res)
	}
	if isDir(prefix) || s.readMarker().Tools != nil || isDir(s.srcDir(pkglist.Tools)) {
		t.Fatal("the environment of the empty list stayed")
	}
	list := fw.TextOf(callTool(t, ctx, tl, `{"command":"list"}`).Content)
	for _, want := range []string{"Python list (the configuration with the changes of this agent): six>=1.16, PyYAML>=6.", "Tools list (the configuration with the changes of this agent): no packages."} {
		if !strings.Contains(list, want) {
			t.Errorf("list lacks %q:\n%s", want, list)
		}
	}
	// The front writes the approved changes into the configured lists. The
	// local changes then no longer count, and the effective list is the
	// same, so the environment stays ready without an install.
	syncs := f.count("uv sync")
	f.reblock("packages:\n  python: [six>=1.16, PyYAML>=6]\n  tools: []\n")
	if f.s.readLocal() != nil || f.s.state().Kind != StateReady {
		t.Fatalf("after the new revision: local %+v, state %s", f.s.readLocal(), f.s.state().Kind)
	}
	if _, err := fw.ExtendToolEnv(ctx, f.cfg, nil); err != nil {
		t.Fatal(err)
	}
	if f.count("uv sync") != syncs {
		t.Fatal("the same effective list installed again")
	}
	// The next install removes the stale file.
	f.reblock("packages:\n  python: [six>=1.16]\n  tools: []\n")
	if _, err := fw.ExtendToolEnv(ctx, f.cfg, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.s.localPath()); err == nil {
		t.Fatal("the stale local changes stayed")
	}
	if got := f.s.effective(pkglist.Python); !slices.Equal(got, []string{"six>=1.16"}) {
		t.Fatalf("effective = %v", got)
	}
}

// A start that finds a failure of an earlier process sends it once, and
// the retry that fails the same way stays quiet.
func TestStartAnnouncesFailure(t *testing.T) {
	f := newFixture(t, fullBlock)
	f.write("fail", "")
	a := f.agent()
	if err := os.MkdirAll(f.s.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := fw.ExtendToolEnv(f.runCtx(), f.cfg, nil); err == nil {
		t.Fatal("no failure")
	}
	if evs := f.events(1); len(evs) != 1 {
		t.Fatalf("events = %+v", evs)
	}
	fl := f.s.readFailure()
	fl.Time = a.Started().Add(-time.Minute)
	if err := writeFile(f.s.failurePath(), fl); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.evs = nil
	f.mu.Unlock()
	syncs := f.count("uv sync")
	c, err := Start(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	deadline := time.Now().Add(5 * time.Second)
	for f.count("uv sync") == syncs && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// The notifier sends the state after the retry, which fails the same
	// way and sends nothing itself.
	evs := f.events(1)
	if len(evs) != 1 || evs[0].Type != EvFailed || decode[Failure](t, evs[0]).Step != "python" {
		t.Fatalf("events = %+v", evs)
	}
	time.Sleep(retryFirst + 200*time.Millisecond)
	if evs := f.settled(); len(evs) != 1 {
		t.Fatalf("the failure went out %d times", len(evs))
	}
}

// A change of a list locks again from the lock file that exists, and a
// change of the mirrors removes the lock files first, because uv.lock
// holds the URL of the mirror for each file.
func TestListMirrorChangeLocksAgain(t *testing.T) {
	f := newFixture(t, mirrorBlock)
	install := func() {
		t.Helper()
		if _, err := fw.ExtendToolEnv(f.runCtx(), f.cfg, nil); err != nil {
			t.Fatal(err)
		}
	}
	install()
	if f.count("uv.lock present") != 0 || f.count("pixi.lock present") != 0 {
		t.Fatalf("calls = %v", f.calls())
	}
	f.reblock(strings.Replace(mirrorBlock, "[jq]", "[jq, nodejs>=22]", 1))
	install()
	if f.count("uv.lock present") != 1 || f.count("pixi.lock present") != 1 {
		t.Fatalf("a new list did not keep the lock files: %v", f.calls())
	}
	f.reblock(strings.ReplaceAll(mirrorBlock, "mirror.test:8080", "cache.test:9090"))
	install()
	if f.count("uv.lock present") != 1 || f.count("pixi.lock present") != 1 {
		t.Fatalf("a new mirror kept the lock files: %v", f.calls())
	}
	f.reblock("packages:\n  python: [\"pyyaml>=6\", ansible-lint]\n  tools: [jq]\n")
	install()
	if f.count("uv.lock present") != 1 || f.count("pixi.lock present") != 1 {
		t.Fatalf("a removed mirror kept the lock files: %v", f.calls())
	}
	if got := envValue(strings.Split(f.read(filepath.Join(f.logs, "uv-env")), "\n"), "UV_DEFAULT_INDEX"); got != defaultIndex {
		t.Fatalf("without a mirror, uv locks with the index %q", got)
	}
}

// The section names the lists, the tool env, and the mirror, and it
// says when a list has no package.
func TestListSection(t *testing.T) {
	f := newFixture(t, mirrorBlock)
	a := f.agent()
	pending := section(a, fw.AgentProfile{})
	if !strings.Contains(pending, "being installed from the list of Python packages and the list of programs") {
		t.Errorf("pending section:\n%s", pending)
	}
	if _, err := fw.ExtendToolEnv(f.runCtx(), f.cfg, nil); err != nil {
		t.Fatal(err)
	}
	ready := section(a, fw.AgentProfile{Tools: []string{"bash", "env"}})
	for _, want := range []string{
		"from the list of Python packages of this agent",
		"Packages: ansible-lint 1.0.0, pyyaml 1.0.0.",
		"from the list of programs of this agent. Their programs are on PATH. Packages: jq 1.7.1.",
		"A person allows each change.",
		"do not edit the files under " + filepath.Join(f.s.dir, "src"),
		"Packages come from the mirror of this deployment: UV_DEFAULT_INDEX and PIP_INDEX_URL name it for Python, and pixi uses it for conda-forge.",
	} {
		if !strings.Contains(ready, want) {
			t.Errorf("ready section lacks %q:\n%s", want, ready)
		}
	}
	if strings.Contains(ready, "pyproject.toml") || strings.Contains(ready, "uv.lock") {
		t.Errorf("the section names a file that fw writes:\n%s", ready)
	}
	empty := newFixture(t, "packages:\n  python: []\n  tools: []\n")
	if _, err := fw.ExtendToolEnv(empty.runCtx(), empty.cfg, nil); err != nil {
		t.Fatal(err)
	}
	text := section(empty.agent(), fw.AgentProfile{})
	if !strings.Contains(text, "no Python packages yet") || !strings.Contains(text, "no programs from conda-forge yet") || strings.Contains(text, "mirror") {
		t.Errorf("empty section:\n%s", text)
	}
}

// The doctor line names the mirror host as the egress need, checks the
// python3 of the image for a Python list, and never names the host of the
// Python downloads of uv for a list.
func TestListDoctor(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, mirrorBlock)
	ok, line := doctor(ctx, f.cfg, nil, time.Second)
	for _, want := range []string{"uv 0.0.0-fake on PATH", "pixi 0.0.0-fake on PATH", "Python 3.12.7 on PATH", "the egress policy needs http mirror.test:8080"} {
		if !strings.Contains(line, want) {
			t.Errorf("line lacks %q: %s", want, line)
		}
	}
	for _, not := range []string{"pypi.org", "conda.anaconda.org", pythonHost, "mirror.test:8080,"} {
		if strings.Contains(line, not) {
			t.Errorf("line names %q: %s", not, line)
		}
	}
	if !ok {
		t.Errorf("not ok: %s", line)
	}
	t.Setenv("PATH", filepath.Join(t.TempDir(), "empty"))
	ok, line = doctor(ctx, f.cfg, nil, time.Second)
	if ok || !strings.Contains(line, "python3 is not on PATH; the image must carry it for environment.packages.python") || strings.Contains(line, pythonHost) {
		t.Errorf("no python3: %v %s", ok, line)
	}
	plain := newFixture(t, "packages:\n  python: [six]\n  tools: [jq]\n")
	_, line = doctor(ctx, plain.cfg, nil, time.Second)
	for _, want := range []string{"tls pypi.org", "tls files.pythonhosted.org", "tls conda.anaconda.org"} {
		if !strings.Contains(line, want) {
			t.Errorf("without a mirror, the line lacks %q: %s", want, line)
		}
	}
	file := newFixture(t, fullBlock+"mirrors:\n  python: https://mirror.test/simple\n")
	_, line = doctor(ctx, file.cfg, nil, time.Second)
	if !strings.Contains(line, "tls mirror.test") || !strings.Contains(line, "tls conda.anaconda.org") {
		t.Errorf("a manifest with a mirror: %s", line)
	}
}
