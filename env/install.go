package env

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/env/pkglist"
	"github.com/bdubs00/featherweight-kernel/fw"
)

// Events of the environment. They belong to the agent, not to a run, so
// they go to the bus subject Subject, as fw.connection.connected goes to
// tokens.subject, and to the log. The ledger of a run records the call of
// the tool env that changed the environment.
const (
	EvReady   = "fw.env.ready"   // the data is a Marker
	EvFailed  = "fw.env.failed"  // the data is a Failure
	EvChanged = "fw.env.changed" // the data is a ChangedData
	// Subject is the bus subject of the events.
	Subject = "fw.environment"
)

// ChangedData is the data of fw.env.changed: the tool env changed a
// manifest and its lock file, and the environment of Digest is ready.
type ChangedData struct {
	Digest   string `json:"digest"`
	Previous string `json:"previous"`
	Manager  string `json:"manager"` // python or tools
	Command  string `json:"command"` // add or remove
	Package  string `json:"package"`
	Run      string `json:"run,omitempty"`
	Agent    string `json:"agent,omitempty"`
}

// Limits of the work of a manager.
const (
	pollEvery  = 250 * time.Millisecond // how often a wait reads the state again
	tailBytes  = 4 << 10                // the end of the output in a failure
	keepBytes  = 64 << 10               // the output that a run keeps
	jsonBytes  = 16 << 20               // the largest JSON output of a manager
	waitDelay  = 5 * time.Second        // how long a killed manager has to exit
	publishFor = 5 * time.Second        // the bound of one publish on the bus
)

// installer installs the environment of a spec for an agent. The agent can
// be nil, for a tool process outside an agent: then no event goes to the
// bus.
type installer struct {
	s   *spec
	cfg *fw.Config
	a   *fw.Agent
	log *slog.Logger
	// announced is true once the installer sent fw.env.ready or
	// fw.env.failed. Only the goroutine of the installer reads it.
	announced bool
}

func newInstaller(a *fw.Agent, cfg *fw.Config, s *spec) *installer {
	log := slog.Default()
	if a != nil {
		log = a.Logger()
	}
	return &installer{s: s, cfg: cfg, a: a, log: log.With("component", "env")}
}

// stepError is a failed step of an install.
type stepError struct {
	step   string
	err    error
	output string
}

func (e *stepError) Error() string { return fmt.Sprintf("%s: %v", e.step, e.err) }

// ready waits until the environment of the current digest is ready, or
// until its install failed, bounded by ctx. When no process installs it,
// the caller installs it, so a verb such as fw run, which starts no
// service, gets the environment too.
func (in *installer) ready(ctx context.Context) error {
	if in.s.placement == PlacementImage {
		return nil
	}
	for {
		st := in.s.state()
		switch st.Kind {
		case StateReady:
			return nil
		case StateFailed:
			if !in.due(st.Failure) {
				return failureError(st.Failure)
			}
		}
		held, f, err := in.attempt(ctx)
		if err != nil {
			return err
		}
		if f != nil {
			return failureError(f)
		}
		if held {
			continue
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("env: the environment is not ready yet (%s): %w", st.Kind, context.Cause(ctx))
		case <-time.After(pollEvery):
		}
	}
}

// failureError is the error of a tool process while the environment of
// its digest failed.
func failureError(f *Failure) error {
	msg := fmt.Sprintf("env: the environment failed to install at the step %s: %s", f.Step, f.Error)
	if wait := time.Until(f.RetryAt).Round(time.Second); wait > 0 {
		msg += fmt.Sprintf(". It tries again in %s", wait)
	}
	if f.Output != "" {
		msg += ". The output of the manager ends with:\n" + f.Output
	}
	return errors.New(msg)
}

// attempt takes the lock without waiting and, with it, installs the
// environment of the current digest once, unless it is ready or waits for
// its next try. It reports whether it held the lock, and the failure of
// the install that it ran.
func (in *installer) attempt(ctx context.Context) (bool, *Failure, error) {
	if err := os.MkdirAll(in.s.dir, 0o755); err != nil {
		return false, nil, fmt.Errorf("env: create %s: %w", in.s.dir, err)
	}
	unlock, ok, err := tryLock(in.s.lockPath())
	if err != nil {
		return false, nil, fmt.Errorf("env: lock %s: %w", in.s.lockPath(), err)
	}
	if !ok {
		return false, nil, nil
	}
	defer unlock()
	digest := in.s.digest()
	if m := in.s.readMarker(); m != nil && m.Digest == digest && in.s.intact(m) {
		return true, nil, nil
	}
	prev := in.s.readFailure()
	if prev != nil && prev.Digest == digest && !in.due(prev) {
		return true, nil, nil
	}
	f := in.install(ctx, digest, prev, true)
	return true, f, ctx.Err()
}

// due reports whether the next try after a failure is due: its time has
// come, or the failure is older than this agent. A new process, such as
// the cold boot after a resume, tries at once, because the egress policy
// can have landed meanwhile.
func (in *installer) due(f *Failure) bool {
	if !time.Now().Before(f.RetryAt) {
		return true
	}
	return in.a != nil && f.Time.Before(in.a.Started())
}

// lock waits for the lock, bounded by ctx.
func (in *installer) lock(ctx context.Context) (func(), error) {
	if err := os.MkdirAll(in.s.dir, 0o755); err != nil {
		return nil, fmt.Errorf("env: create %s: %w", in.s.dir, err)
	}
	for {
		unlock, ok, err := tryLock(in.s.lockPath())
		if err != nil {
			return nil, fmt.Errorf("env: lock %s: %w", in.s.lockPath(), err)
		}
		if ok {
			return unlock, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("env: another process installs the environment: %w", context.Cause(ctx))
		case <-time.After(pollEvery):
		}
	}
}

// install installs the environment of digest and records the result: the
// marker and fw.env.ready, or the failure and fw.env.failed. Without
// record, a failure writes no file and sends no event, because the caller
// puts the previous list back. The caller holds the lock. A canceled
// context records nothing, because the next try goes on from the cache.
// It returns the failure, or nil.
func (in *installer) install(ctx context.Context, digest string, prev *Failure, record bool) *Failure {
	start := time.Now()
	if in.a != nil {
		// The process is not idle while it installs, so the front does
		// not suspend the Task in the middle of a download.
		release := in.a.Hold()
		defer release()
	}
	m, serr := in.materialize(ctx, digest)
	if ctx.Err() != nil {
		return nil
	}
	if serr == nil {
		m.Ready = time.Now().UTC()
		m.Seconds = m.Ready.Sub(start).Round(time.Millisecond).Seconds()
		if err := writeFile(in.s.markerPath(), m); err != nil {
			serr = &stepError{step: "marker", err: err}
		}
	}
	if serr != nil {
		f := &Failure{Digest: digest, Time: time.Now().UTC(), Step: serr.step, Error: in.redact(serr.err.Error()), Output: in.redact(serr.output), Attempt: 1}
		if prev != nil && prev.Digest == digest {
			f.Attempt = prev.Attempt + 1
		}
		f.RetryAt = f.Time.Add(backoff(f.Attempt))
		if !record {
			in.log.Info("a change of the environment failed to install", "digest", short(digest), "step", f.Step, "error", f.Error)
			return f
		}
		if err := writeFile(in.s.failurePath(), f); err != nil {
			in.log.Warn("env: write the failure", "error", err)
		}
		// A retry that fails the same way stays quiet: the first failure
		// is the one a person reads.
		if prev == nil || prev.Digest != digest || prev.Error != f.Error {
			in.log.Warn("the environment failed to install", "digest", short(digest), "step", f.Step, "error", f.Error, "attempt", f.Attempt, "retryIn", backoff(f.Attempt))
			in.publish(EvFailed, f)
		} else {
			in.log.Debug("the environment failed to install again", "digest", short(digest), "step", f.Step, "attempt", f.Attempt)
		}
		return f
	}
	if err := removeFile(in.s.failurePath()); err != nil {
		in.log.Warn("env: remove the failure", "error", err)
	}
	in.log.Info("the environment is ready", "digest", short(digest), "seconds", m.Seconds, "dir", in.s.dir)
	in.publish(EvReady, m)
	return nil
}

// materialize runs the managers and the prepare commands for digest and
// returns the marker without its time.
func (in *installer) materialize(ctx context.Context, digest string) (*Marker, *stepError) {
	s := in.s
	if err := in.layout(); err != nil {
		return nil, &stepError{step: "layout", err: err}
	}
	// A record of local changes for other configured lists goes, because
	// the front put the approved changes into the configured lists.
	if err := s.dropStaleLocal(); err != nil {
		in.log.Warn("env: remove the stale local changes", "error", err)
	}
	m := &Marker{Digest: digest, Placement: s.placement, Prepare: len(s.prepare)}
	env := in.env(ctx, nil)
	if s.python != "" {
		var serr *stepError
		if s.isInline(pkglist.Python) {
			m.Python, serr = in.pythonList(ctx, env)
		} else {
			m.Python, serr = in.pythonFile(ctx, env)
		}
		if serr != nil {
			return nil, serr
		}
		if m.Python != nil {
			m.UV = in.version(ctx, env, "uv")
		}
	}
	if s.tools != "" {
		var serr *stepError
		if s.isInline(pkglist.Tools) {
			m.Tools, serr = in.toolsList(ctx, env)
		} else {
			m.Tools, serr = in.toolsFile(ctx, env)
		}
		if serr != nil {
			return nil, serr
		}
		if m.Tools != nil {
			m.Pixi = in.version(ctx, env, "pixi")
		}
	}
	env = in.env(ctx, m)
	for i, c := range s.prepare {
		if out, err := in.run(ctx, env, s.root, "/bin/sh", "-c", c); err != nil {
			return nil, &stepError{step: fmt.Sprintf("prepare %d", i+1), err: err, output: out}
		}
	}
	return m, nil
}

// pythonFile installs the uv project of environment.python from its lock
// file, as it is.
func (in *installer) pythonFile(ctx context.Context, env []string) (*PythonInfo, *stepError) {
	s := in.s
	if err := present(s.python, s.uvLock(), "uv lock"); err != nil {
		return nil, &stepError{step: "python", err: err}
	}
	dir := filepath.Dir(s.python)
	if out, err := in.run(ctx, env, dir, "uv", "sync", "--frozen", "--no-install-project", "--project", dir); err != nil {
		return nil, &stepError{step: "python", err: fmt.Errorf("uv sync --frozen: %w", err), output: out}
	}
	return in.pythonInfo(ctx, env, nil)
}

// pythonList writes the uv project of the list of environment.packages,
// locks it, and installs the lock. An empty list removes the virtual
// environment and needs no network.
func (in *installer) pythonList(ctx context.Context, env []string) (*PythonInfo, *stepError) {
	s := in.s
	list := s.effective(pkglist.Python)
	if len(list) == 0 {
		if err := errors.Join(os.RemoveAll(s.venv()), os.RemoveAll(s.srcDir(pkglist.Python))); err != nil {
			return nil, &stepError{step: "python", err: err}
		}
		return nil, nil
	}
	python, serr := in.lockPython(ctx, env, list)
	if serr != nil {
		return nil, serr
	}
	dir := s.srcDir(pkglist.Python)
	if out, err := in.run(ctx, env, dir, "uv", "sync", "--locked", "--no-install-project", "--project", dir, "--python", python); err != nil {
		return nil, &stepError{step: "python", err: fmt.Errorf("uv sync --locked: %w", err), output: out}
	}
	return in.pythonInfo(ctx, env, list)
}

// lockPython writes the manifest of list for the Python of the image and
// locks it. It returns the path of that Python. uv downloads no
// interpreter, so the image must carry python3.
func (in *installer) lockPython(ctx context.Context, env []string, list []string) (string, *stepError) {
	s := in.s
	python, err := exec.LookPath("python3")
	if err != nil {
		return "", &stepError{step: "python", err: fmt.Errorf("python3 is not on PATH; the image must carry it for %s.packages.python, because uv downloads no interpreter", Block)}
	}
	out, err := in.output(ctx, env, s.root, python, "--version")
	if err != nil {
		return "", &stepError{step: "python", err: fmt.Errorf("python3 --version: %w", err)}
	}
	minor, err := pythonMinor(string(out))
	if err != nil {
		return "", &stepError{step: "python", err: err}
	}
	dir := s.srcDir(pkglist.Python)
	if err := in.makeSrc(dir); err != nil {
		return "", &stepError{step: "python", err: err}
	}
	if err := writeText(s.python, pyproject(list, minor)); err != nil {
		return "", &stepError{step: "python", err: err}
	}
	if err := s.freshLock(pkglist.Python, s.uvLock()); err != nil {
		return "", &stepError{step: "python", err: err}
	}
	if out, err := in.run(ctx, env, dir, "uv", "lock", "--project", dir, "--python", python); err != nil {
		return "", &stepError{step: "python", err: fmt.Errorf("uv lock: %w", err), output: out}
	}
	if err := s.recordLock(pkglist.Python); err != nil {
		return "", &stepError{step: "python", err: err}
	}
	return python, nil
}

// pythonInfo reads the packages of the virtual environment. With a list,
// a package that the list names is explicit.
func (in *installer) pythonInfo(ctx context.Context, env []string, list []string) (*PythonInfo, *stepError) {
	s := in.s
	py := &PythonInfo{Manifest: s.rel(s.python), Env: s.venv(), Version: pythonVersion(s.venv())}
	out, err := in.output(ctx, env, filepath.Dir(s.python), "uv", "pip", "list", "--format", "json", "--python", s.venv())
	if err == nil {
		err = json.Unmarshal(out, &py.Packages)
	}
	if err != nil {
		return nil, &stepError{step: "python", err: fmt.Errorf("uv pip list: %w", err)}
	}
	if list != nil {
		named := map[string]bool{}
		for _, req := range list {
			named[pkglist.Name(pkglist.Python, req)] = true
		}
		for i := range py.Packages {
			py.Packages[i].Explicit = named[pkglist.Name(pkglist.Python, py.Packages[i].Name)]
		}
	}
	return py, nil
}

// toolsFile installs the pixi manifest of environment.tools from its
// lock file, as it is.
func (in *installer) toolsFile(ctx context.Context, env []string) (*ToolsInfo, *stepError) {
	s := in.s
	if err := present(s.tools, s.pixiLock(), "pixi lock"); err != nil {
		return nil, &stepError{step: "tools", err: err}
	}
	if out, err := in.run(ctx, env, filepath.Dir(s.tools), "pixi", "install", "--frozen", "--manifest-path", s.tools); err != nil {
		return nil, &stepError{step: "tools", err: fmt.Errorf("pixi install --frozen: %w", err), output: out}
	}
	return in.toolsInfo(ctx, env)
}

// toolsList writes the pixi manifest of the list of environment.packages,
// locks it, and installs the lock. An empty list removes the environment
// and needs no network.
func (in *installer) toolsList(ctx context.Context, env []string) (*ToolsInfo, *stepError) {
	s := in.s
	list := s.effective(pkglist.Tools)
	if len(list) == 0 {
		old, _ := filepath.Glob(filepath.Join(s.pixiEnvs(), inlineName+"-*"))
		errs := []error{os.RemoveAll(s.srcDir(pkglist.Tools))}
		for _, d := range old {
			errs = append(errs, os.RemoveAll(d))
		}
		if err := errors.Join(errs...); err != nil {
			return nil, &stepError{step: "tools", err: err}
		}
		return nil, nil
	}
	if serr := in.lockTools(ctx, env, list); serr != nil {
		return nil, serr
	}
	if out, err := in.run(ctx, env, s.srcDir(pkglist.Tools), "pixi", "install", "--locked", "--manifest-path", s.tools); err != nil {
		return nil, &stepError{step: "tools", err: fmt.Errorf("pixi install --locked: %w", err), output: out}
	}
	return in.toolsInfo(ctx, env)
}

// lockTools writes the manifest of list and locks it.
func (in *installer) lockTools(ctx context.Context, env []string, list []string) *stepError {
	s := in.s
	text, err := pixiManifest(list)
	if err != nil {
		return &stepError{step: "tools", err: err}
	}
	dir := s.srcDir(pkglist.Tools)
	if err := in.makeSrc(dir); err != nil {
		return &stepError{step: "tools", err: err}
	}
	if err := writeText(s.tools, text); err != nil {
		return &stepError{step: "tools", err: err}
	}
	if err := s.freshLock(pkglist.Tools, s.pixiLock()); err != nil {
		return &stepError{step: "tools", err: err}
	}
	if out, err := in.run(ctx, env, dir, "pixi", "lock", "--manifest-path", s.tools); err != nil {
		return &stepError{step: "tools", err: fmt.Errorf("pixi lock: %w", err), output: out}
	}
	if err := s.recordLock(pkglist.Tools); err != nil {
		return &stepError{step: "tools", err: err}
	}
	return nil
}

// toolsInfo reads the directory and the packages of the pixi environment.
func (in *installer) toolsInfo(ctx context.Context, env []string) (*ToolsInfo, *stepError) {
	s := in.s
	dir := filepath.Dir(s.tools)
	tools := &ToolsInfo{Manifest: s.rel(s.tools)}
	var info struct {
		Environments []struct {
			Name   string `json:"name"`
			Prefix string `json:"prefix"`
		} `json:"environments_info"`
	}
	out, err := in.output(ctx, env, dir, "pixi", "info", "--json", "--manifest-path", s.tools)
	if err == nil {
		err = json.Unmarshal(out, &info)
	}
	if err != nil {
		return nil, &stepError{step: "tools", err: fmt.Errorf("pixi info: %w", err)}
	}
	for _, e := range info.Environments {
		if e.Name == "default" {
			tools.Prefix = e.Prefix
		}
	}
	out, err = in.output(ctx, env, dir, "pixi", "list", "--json", "--frozen", "--manifest-path", s.tools)
	if err == nil {
		err = json.Unmarshal(out, &tools.Packages)
	}
	if err != nil {
		return nil, &stepError{step: "tools", err: fmt.Errorf("pixi list: %w", err)}
	}
	return tools, nil
}

// lockList writes the manifest of a new list of manager and locks it,
// without an install, as uv add --no-sync and pixi add --no-install do.
// When the lock fails, the manifest is as it was. An empty list locks
// nothing.
func (in *installer) lockList(ctx context.Context, manager string, list []string) *stepError {
	if len(list) == 0 {
		return nil
	}
	if err := in.layout(); err != nil {
		return &stepError{step: "layout", err: err}
	}
	path := map[string]string{pkglist.Python: in.s.python, pkglist.Tools: in.s.tools}[manager]
	old := readBack(path)
	env := in.env(ctx, in.s.readMarker())
	var serr *stepError
	if manager == pkglist.Python {
		_, serr = in.lockPython(ctx, env, list)
	} else {
		serr = in.lockTools(ctx, env, list)
	}
	if serr != nil {
		if err := putBack(path, old); err != nil {
			in.log.Warn("env: put the manifest back", "error", err)
		}
	}
	return serr
}

// makeSrc creates the directory of a manifest that fw writes, for the
// tool user, because the manager writes the lock file next to it.
func (in *installer) makeSrc(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := in.s.own(dir); err != nil {
		return fmt.Errorf("give %s to the tool user: %w", dir, err)
	}
	return nil
}

// present checks that a manifest and its lock file exist.
func present(manifest, lock, how string) error {
	if _, err := os.Stat(manifest); err != nil {
		return fmt.Errorf("the manifest %s is missing", manifest)
	}
	if _, err := os.Stat(lock); err != nil {
		return fmt.Errorf("the lock file %s is missing; run %s next to %s and keep the lock file", lock, how, filepath.Base(manifest))
	}
	return nil
}

// layout creates the directories of the environment: dir for fw, guarded
// against the tool user, and the directories that the managers write for
// the tool user. It also writes the configuration of pixi, which keeps
// each environment under dir and not next to its manifest, and names the
// mirror of conda-forge.
func (in *installer) layout() error {
	s := in.s
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	if err := s.guard(s.dir); err != nil {
		return fmt.Errorf("give %s to fw: %w", s.dir, err)
	}
	for _, d := range s.owned() {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
		if err := s.own(d); err != nil {
			return fmt.Errorf("give %s to the tool user: %w", d, err)
		}
	}
	if s.tools != "" || s.mirrors[pkglist.Tools] != "" {
		if err := writeText(filepath.Join(s.pixiHome(), "config.toml"), s.pixiConfig()); err != nil {
			return err
		}
	}
	return nil
}

// env returns the environment of a manager: the environment of a tool
// process (fw.ToolEnv) with the variables of the environment, and quiet
// output.
func (in *installer) env(ctx context.Context, m *Marker) []string {
	if in.a != nil && fw.AgentFrom(ctx) == nil {
		ctx = fw.WithAgent(ctx, in.a)
	}
	s := in.s
	env := s.apply(fw.ToolEnv(ctx, in.cfg, os.Environ(), nil), m)
	env = append(env, "UV_NO_PROGRESS=1", "PIXI_NO_PROGRESS=true", "PIXI_COLOR=never", "NO_COLOR=1")
	if s.python != "" && lookupEnv(env, "UV_PROJECT_ENVIRONMENT") == "" {
		// A list that was empty has no virtual environment yet, and uv
		// must create it at its place under dir.
		env = append(env, "UV_PROJECT_ENVIRONMENT="+s.venv(), "VIRTUAL_ENV="+s.venv())
	}
	if s.isInline(pkglist.Python) {
		// The Python of the image serves uv, so no install needs a host
		// for interpreters. The index is always named, so uv locks again
		// when the mirror changes or goes away.
		env = append(env, "UV_PYTHON_DOWNLOADS=never")
		if s.mirrors[pkglist.Python] == "" {
			env = append(env, "UV_DEFAULT_INDEX="+defaultIndex)
		}
	}
	return env
}

// command builds a manager process as the tool user.
func (in *installer) command(ctx context.Context, env []string, dir, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append([]string(nil), env...)
	cmd.WaitDelay = waitDelay
	processGroup(cmd)
	in.s.user.Apply(cmd)
	return cmd
}

// run runs a manager and returns the end of its output.
func (in *installer) run(ctx context.Context, env []string, dir, name string, args ...string) (string, error) {
	var out tail
	cmd := in.command(ctx, env, dir, name, args...)
	cmd.Stdout, cmd.Stderr = &out, &out
	in.log.Debug("env: run", "command", name, "args", args)
	err := missing(name, cmd.Run())
	return out.end(), err
}

// missing says that the image must carry a manager that is not on PATH.
func missing(name string, err error) error {
	if errors.Is(err, exec.ErrNotFound) {
		return fmt.Errorf("%s is not on PATH; the image must carry it, because fw does not download it: %w", name, err)
	}
	return err
}

// output runs a manager and returns its standard output, for JSON.
func (in *installer) output(ctx context.Context, env []string, dir, name string, args ...string) ([]byte, error) {
	var stdout bytes.Buffer
	var stderr tail
	cmd := in.command(ctx, env, dir, name, args...)
	cmd.Stdout, cmd.Stderr = &limited{w: &stdout, n: jsonBytes}, &stderr
	if err := missing(name, cmd.Run()); err != nil {
		if e := stderr.end(); e != "" {
			return nil, fmt.Errorf("%w: %s", err, firstLine(e))
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

// version returns the first line of `<name> --version`, or "".
func (in *installer) version(ctx context.Context, env []string, name string) string {
	out, err := in.output(ctx, env, in.s.root, name, "--version")
	if err != nil {
		return ""
	}
	return firstLine(string(out))
}

// pythonVersion reads the version of the interpreter of a virtual
// environment from its pyvenv.cfg.
func pythonVersion(venv string) string {
	f, err := os.Open(filepath.Join(venv, "pyvenv.cfg"))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if ok && (strings.TrimSpace(k) == "version_info" || strings.TrimSpace(k) == "version") {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// urlPassword matches the user information of a URL, such as the token
// of a private index.
var urlPassword = regexp.MustCompile(`://[^/\s:@]+:[^/\s@]+@`)

// redact removes the secrets of the process and the passwords of URLs
// from the output of a manager.
func (in *installer) redact(s string) string {
	if in.a != nil {
		s = in.a.Redact(s)
	}
	return urlPassword.ReplaceAllString(s, "://***@")
}

// publish sends an event of the environment to the bus of the agent.
func (in *installer) publish(typ string, data any) {
	if in.a == nil || in.a.Bus() == nil {
		return
	}
	e, err := fw.NewEvent("", typ, data)
	if err != nil {
		in.log.Warn("env: event", "type", typ, "error", err)
		return
	}
	e.Node = in.a.Node()
	ctx, cancel := context.WithTimeout(context.Background(), publishFor)
	defer cancel()
	if err := in.a.Bus().Publish(ctx, Subject, e); err != nil {
		in.log.Warn("env: publish an event", "type", typ, "subject", Subject, "error", err)
		return
	}
	if typ == EvReady || typ == EvFailed {
		in.announced = true
	}
}

// announce sends the state of the environment once, for the front, which
// keeps the last event of each Task: fw.env.ready with the marker, or
// fw.env.failed with the failure. A pending state sends nothing, because
// its install sends the result.
func (in *installer) announce(st state) {
	if in.announced {
		return
	}
	switch {
	case st.Kind == StateReady && st.Marker != nil:
		in.publish(EvReady, st.Marker)
	case st.Kind == StateFailed && st.Failure != nil:
		in.publish(EvFailed, st.Failure)
	}
}

// serve installs the environment in the background until it is ready,
// with a pause that doubles after each failure. It returns when the
// environment is ready or ctx ends. It sends the state that it finds
// once, so a Task that is ready at its start says so too, and a retry
// that fails the same way stays quiet after that.
func (in *installer) serve(ctx context.Context) {
	for first := true; ; first = false {
		st := in.s.state()
		switch st.Kind {
		case StateReady:
			if first {
				// An install logs its own result, so only a start that
				// finds the environment ready logs here.
				in.log.Info("the environment is ready", "digest", short(st.Digest), "dir", in.s.dir)
			}
			in.announce(st)
			return
		case StateFailed:
			in.announce(st)
			if !in.due(st.Failure) {
				if !sleep(ctx, time.Until(st.Failure.RetryAt)) {
					return
				}
				continue
			}
		}
		held, f, err := in.attempt(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			in.log.Warn("env: the install cannot start", "error", err)
			if !sleep(ctx, backoff(1)) {
				return
			}
		case f != nil:
			if !sleep(ctx, time.Until(f.RetryAt)) {
				return
			}
		case !held:
			if !sleep(ctx, 4*pollEvery) {
				return
			}
		}
	}
}

// sleep waits for d or the end of ctx, and reports whether d passed.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// tail keeps the last keepBytes of an output.
type tail struct{ b []byte }

func (t *tail) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > keepBytes {
		t.b = append([]byte(nil), t.b[len(t.b)-keepBytes:]...)
	}
	return len(p), nil
}

// end returns the last tailBytes of the output, from the start of a line.
func (t *tail) end() string {
	b := bytes.TrimSpace(t.b)
	if len(b) > tailBytes {
		b = b[len(b)-tailBytes:]
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	return string(b)
}

// limited fails a write past n bytes, so a broken manager cannot fill the
// memory of fw.
type limited struct {
	w *bytes.Buffer
	n int
}

func (l *limited) Write(p []byte) (int, error) {
	if l.w.Len()+len(p) > l.n {
		return 0, fmt.Errorf("env: the output is larger than %d bytes", l.n)
	}
	return l.w.Write(p)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}
