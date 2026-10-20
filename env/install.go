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
	f := in.install(ctx, digest, prev)
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
// marker and fw.env.ready, or the failure and fw.env.failed. The caller
// holds the lock. A canceled context records nothing, because the next
// try goes on from the cache. It returns the failure, or nil.
func (in *installer) install(ctx context.Context, digest string, prev *Failure) *Failure {
	start := time.Now()
	m, serr := in.materialize(ctx, digest)
	if ctx.Err() != nil {
		return nil
	}
	if serr == nil {
		m.Seconds = time.Since(start).Round(time.Millisecond).Seconds()
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
	m := &Marker{Digest: digest, Placement: s.placement, Ready: time.Now().UTC(), Prepare: len(s.prepare)}
	env := in.env(ctx, nil)
	if s.python != "" {
		if err := present(s.python, s.uvLock(), "uv lock"); err != nil {
			return nil, &stepError{step: "python", err: err}
		}
		dir := filepath.Dir(s.python)
		if out, err := in.run(ctx, env, dir, "uv", "sync", "--frozen", "--no-install-project", "--project", dir); err != nil {
			return nil, &stepError{step: "python", err: fmt.Errorf("uv sync --frozen: %w", err), output: out}
		}
		py := &PythonInfo{Manifest: s.rel(s.python), Env: s.venv(), Version: pythonVersion(s.venv())}
		out, err := in.output(ctx, env, dir, "uv", "pip", "list", "--format", "json", "--python", s.venv())
		if err == nil {
			err = json.Unmarshal(out, &py.Packages)
		}
		if err != nil {
			return nil, &stepError{step: "python", err: fmt.Errorf("uv pip list: %w", err)}
		}
		m.Python = py
		m.UV = in.version(ctx, env, "uv")
	}
	if s.tools != "" {
		if err := present(s.tools, s.pixiLock(), "pixi lock"); err != nil {
			return nil, &stepError{step: "tools", err: err}
		}
		dir := filepath.Dir(s.tools)
		if out, err := in.run(ctx, env, dir, "pixi", "install", "--frozen", "--manifest-path", s.tools); err != nil {
			return nil, &stepError{step: "tools", err: fmt.Errorf("pixi install --frozen: %w", err), output: out}
		}
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
		m.Tools = tools
		m.Pixi = in.version(ctx, env, "pixi")
	}
	env = in.env(ctx, m)
	for i, c := range s.prepare {
		if out, err := in.run(ctx, env, s.root, "/bin/sh", "-c", c); err != nil {
			return nil, &stepError{step: fmt.Sprintf("prepare %d", i+1), err: err, output: out}
		}
	}
	return m, nil
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
// each environment under dir and not next to its manifest.
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
	if s.tools != "" {
		conf := fmt.Sprintf("# Written by fw (environment.tools). Each environment lives under %s.\ndetached-environments = %q\n", s.dir, s.pixiEnvs())
		if err := os.WriteFile(filepath.Join(s.pixiHome(), "config.toml"), []byte(conf), 0o644); err != nil {
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
	env := in.s.apply(fw.ToolEnv(ctx, in.cfg, os.Environ(), nil), m)
	return append(env, "UV_NO_PROGRESS=1", "PIXI_NO_PROGRESS=true", "PIXI_COLOR=never", "NO_COLOR=1")
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
	err := cmd.Run()
	return out.end(), err
}

// output runs a manager and returns its standard output, for JSON.
func (in *installer) output(ctx context.Context, env []string, dir, name string, args ...string) ([]byte, error) {
	var stdout bytes.Buffer
	var stderr tail
	cmd := in.command(ctx, env, dir, name, args...)
	cmd.Stdout, cmd.Stderr = &limited{w: &stdout, n: jsonBytes}, &stderr
	if err := cmd.Run(); err != nil {
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
	}
}

// serve installs the environment in the background until it is ready,
// with a pause that doubles after each failure. It returns when the
// environment is ready or ctx ends.
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
			return
		case StateFailed:
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
