package builtin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func init() { fw.RegisterTool("bash", newBash) }

const bashSchema = `{
  "type": "object",
  "description": "The input of the Anthropic bash tool (bash_20250124). Send a command, or send restart: true to start a new shell.",
  "properties": {
    "command": {
      "type": "string",
      "description": "The bash command to run. One shell serves every call of this run, so the working directory and exported variables carry over to the next call of the run. Another run never sees them."
    },
    "restart": {
      "type": "boolean",
      "description": "Set to true to kill the shell and start a new one. The working directory and the variables go back to their start values."
    }
  },
  "additionalProperties": false
}`

const bashDescription = "Run a command in the persistent bash shell of this run. " +
	"The working directory and exported variables carry over between the calls of the run, and another run never sees them. " +
	"Each call starts with set -e, set -u, and pipefail off, and a failure under set -e ends that call, not the shell. " +
	"The result holds stdout, then stderr, then the exit code. " +
	"A non-zero exit code is a normal result. " +
	"An output over the limit of the configuration keeps its head and its tail, and the result names a file with the full output. " +
	"The command reads no input, so do not run interactive programs."

// bashTimeout is the time limit of one call when tools.bash.timeout is not
// set.
const bashTimeout = 120 * time.Second

// tailKeep is the end of a stream that bash keeps past the cap, so the
// last lines of a long output, such as the verdict of a check, survive,
// and the end mark is still found.
const tailKeep = 32 << 10

// bashKeep is the head of a stream that bash keeps when the output limit
// of the configuration cuts the results of bash (fw.LimitTools). The limit
// then keeps the head and the tail of the result for the model and the
// full text in a file of the run. Without that limit, bash keeps
// maxOutput.
const bashKeep = 1 << 20

// sessionIdle is how long the shell of a run stays after its last call
// when no CloseRun came, for example for a call outside a run.
const sessionIdle = 30 * time.Minute

// bashTool keeps one bash process per run. The calls of one run take turns
// on its shell, and runs never share a shell, so the working directory,
// the variables, and the background jobs of one run never reach another
// run or another person. The kernel closes the shell of a run when the run
// ends (fw.RunCloser), and a shell that no call used for sessionIdle goes
// too. A call outside a run, as from a test or the command line, shares
// one shell under the empty run id.
type bashTool struct {
	cfg     *fw.Config // nil for a tool without a configuration
	dir     string
	home    string       // FW_HOME of the shell
	env     []string     // the variables of tools.bash.env, as NAME=value
	user    *fw.ToolUser // the user of the shell (tools.user), or nil for the user of fw
	timeout time.Duration
	keep    int // the head of a stream that a call keeps: maxOutput or bashKeep

	mu       sync.Mutex
	sessions map[string]*bashSession // by run id
	closed   bool
	done     chan struct{} // closed by Close, which ends the reaper
}

// bashSession is the shell of one run. sem holds one token while a call
// uses the shell, so the calls of a run take turns.
type bashSession struct {
	sem  chan struct{}
	sh   *shell // nil until the first call, and after a kill
	used time.Time
}

func newBash(cfg *fw.Config) (fw.Tool, error) {
	t := &bashTool{cfg: cfg, timeout: bashTimeout, keep: maxOutput, home: fw.DefaultHome(), sessions: map[string]*bashSession{}, done: make(chan struct{})}
	if cfg != nil {
		if n := cfg.Tools.Limits.For("bash").MaxOutputBytes; n > 0 {
			t.keep = max(bashKeep, n)
		}
		dir, err := filepath.Abs(cfg.WorkspaceRoot())
		if err != nil {
			return nil, fmt.Errorf("builtin: bash: root %s: %w", cfg.WorkspaceRoot(), err)
		}
		t.dir = dir
		if cfg.Tools.Bash.Timeout > 0 {
			t.timeout = cfg.Tools.Bash.Timeout
		}
		t.env = configEnv(cfg)
		if cfg.Home != "" {
			t.home = cfg.Home
		}
		u, err := fw.LookupToolUser(cfg.Tools.User)
		if err != nil {
			return nil, fmt.Errorf("builtin: bash: %w", err)
		}
		t.user = u
	}
	go t.reap()
	return t, nil
}

// Spec implements fw.Tool. The description names the time limit of a call.
func (t *bashTool) Spec() fw.ToolSpec {
	desc := bashDescription + fmt.Sprintf(" Each call has a limit of %s.", t.timeout)
	s := newSpec("bash", desc, bashSchema)
	s.Native = fw.NativeBash
	s.Destructive = true
	return s
}

// Close kills every shell and ends the reaper. The kernel calls it, when
// it knows io.Closer, as the agent shuts down.
func (t *bashTool) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	close(t.done)
	sessions := t.sessions
	t.sessions = map[string]*bashSession{}
	t.mu.Unlock()
	for _, s := range sessions {
		s.sem <- struct{}{}
		s.stop()
		<-s.sem
	}
	return nil
}

// CloseRun implements fw.RunCloser: the shell of the run goes when the run
// ends. A call that still holds the shell ends first; the kill waits for
// it in the background.
func (t *bashTool) CloseRun(runID string) {
	t.mu.Lock()
	s := t.sessions[runID]
	delete(t.sessions, runID)
	t.mu.Unlock()
	if s == nil {
		return
	}
	go func() {
		s.sem <- struct{}{}
		defer func() { <-s.sem }()
		s.stop()
	}()
}

// open returns how many runs hold a shell session, for tests.
func (t *bashTool) open() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.sessions)
}

// session returns the session of the run in ctx, and makes one on the
// first call. It returns nil after Close.
func (t *bashTool) session(ctx context.Context) *bashSession {
	id := fw.RunInfoFrom(ctx).RunID
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	s := t.sessions[id]
	if s == nil {
		s = &bashSession{sem: make(chan struct{}, 1)}
		t.sessions[id] = s
	}
	s.used = time.Now()
	return s
}

// reap kills the shells that no call used for sessionIdle, once a minute,
// until Close. A shell in use is left alone.
func (t *bashTool) reap() {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-t.done:
			return
		case <-tick.C:
		}
		var idle []*bashSession
		t.mu.Lock()
		for id, s := range t.sessions {
			if time.Since(s.used) >= sessionIdle {
				idle = append(idle, s)
				delete(t.sessions, id)
			}
		}
		t.mu.Unlock()
		for _, s := range idle {
			select {
			case s.sem <- struct{}{}:
				s.stop()
				<-s.sem
			default:
				// A call holds the shell after all; it ends on its own.
			}
		}
	}
}

// Call implements fw.Tool.
func (t *bashTool) Call(ctx context.Context, input json.RawMessage) (fw.ToolResult, error) {
	var in struct {
		Command string `json:"command"`
		Restart bool   `json:"restart"`
	}
	if err := decode(input, &in); err != nil {
		return badInput("bash", err), nil
	}
	s := t.session(ctx)
	if s == nil {
		return fw.ErrorResult("bash: the tool is closed."), nil
	}
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return canceled(ctx, "bash")
	}
	defer func() { <-s.sem }()

	if in.Restart {
		s.stop()
		if strings.TrimSpace(in.Command) == "" {
			if err := t.start(ctx, s); err != nil {
				return fw.ErrorResult(err.Error()), nil
			}
			return fw.TextResult("The bash shell has been restarted."), nil
		}
	}
	if strings.TrimSpace(in.Command) == "" {
		return fw.ErrorResult("bash: the command is empty. Send a command, or send restart: true."), nil
	}
	if s.sh == nil {
		if err := t.start(ctx, s); err != nil {
			return fw.ErrorResult(err.Error()), nil
		}
	}
	return t.run(ctx, s, in.Command)
}

// start starts a new shell for a session in the directory of the run,
// which the kernel names in the run info, or in the root of the workspace.
// The caller holds its token.
func (t *bashTool) start(ctx context.Context, s *bashSession) error {
	dir := fw.RunInfoFrom(ctx).Dir
	if dir == "" {
		dir = t.dir
	}
	sh, err := startShell(dir, t.shellEnv(ctx, dir), t.user)
	if err != nil {
		return fmt.Errorf("bash: the shell cannot start: %v", err)
	}
	s.sh = sh
	return nil
}

// stop kills the shell of the session, if any. The caller holds its token.
func (s *bashSession) stop() {
	if s.sh != nil {
		s.sh.kill()
		s.sh = nil
	}
}

// run sends one command and reads both streams up to the end marks.
func (t *bashTool) run(ctx context.Context, s *bashSession, command string) (fw.ToolResult, error) {
	sh := s.sh
	eof, mark := randomToken("__FW_EOF_"), randomToken("")
	// The command goes through a quoted heredoc and eval, so a syntax error
	// fails the command and leaves the shell running. The end mark is
	// printed in two parts, so a trace with set -x or set -v does not
	// contain it.
	//
	// Each call starts with set -e, set -u, and pipefail off, so an option
	// of one call never stops the wrapper of the next. A failure under a
	// set -e of the call ends the command, not the shell: the ERR trap
	// returns from each function of the shell, and at the top it turns
	// set -e off and leaves the loop around eval. The command keeps its
	// exit code. A subshell exits as bash makes it exit. A command that
	// sets its own ERR trap, or that reads an unset variable under set -u,
	// can still end the shell, as it ends a script.
	script := fmt.Sprintf("set +euo pipefail\n"+
		"__fw_pid=$BASHPID __fw_fail=\n"+
		"set -E\n"+
		"trap '__fw_s=$?; if [[ $- == *e* && $BASHPID == \"$__fw_pid\" ]]; then if (( ${#FUNCNAME[@]} )); then return $__fw_s; fi; set +e; __fw_fail=$__fw_s; break 1000; fi' ERR\n"+
		"IFS= read -r -d '' __fw_cmd <<'%[1]s' || true\n%[2]s\n%[1]s\n"+
		"for __fw_once in 1; do eval \"$__fw_cmd\" </dev/null; done\n"+
		"__fw_ec=${__fw_fail:-$?}\n"+
		"printf '%%s%%s %%d\\n' __FW_END_ '%[3]s' \"$__fw_ec\"\n"+
		"printf '%%s%%s\\n' __FW_END_ '%[3]s' >&2\n",
		eof, command, mark)
	go func() { _, _ = io.WriteString(sh.stdin, script) }()

	out := newCollector("__FW_END_"+mark, sh.outLeft, t.keep)
	errs := newCollector("__FW_END_"+mark, sh.errLeft, t.keep)
	errCh := sh.stderr
	if sh.errClosed {
		// A command such as "exec 2>&1" closed the stderr pipe earlier.
		errCh, errs.done = nil, true
	}
	timer := time.NewTimer(t.timeout)
	defer timer.Stop()
	for !out.done || !errs.done {
		select {
		case b, ok := <-sh.stdout:
			if !ok {
				return t.exited(ctx, s, out, errs), nil
			}
			out.add(b)
		case b, ok := <-errCh:
			if !ok {
				sh.errClosed, errCh, errs.done = true, nil, true
				continue
			}
			errs.add(b)
		case <-timer.C:
			s.stop()
			logger(ctx).Warn("bash: command timed out; shell killed", "timeout", t.timeout.String())
			msg := fmt.Sprintf("The command timed out after %s. The shell was killed, and the next call starts a new shell.", t.timeout)
			return fw.ToolResult{Content: []fw.Block{fw.Text(bashText(out, errs, msg))}, IsError: true}, nil
		case <-ctx.Done():
			s.stop()
			return canceled(ctx, "bash")
		}
	}
	sh.outLeft, sh.errLeft = out.left, errs.left
	code, err := strconv.Atoi(strings.TrimSpace(out.tail))
	if err != nil {
		code = -1
	}
	res := fw.TextResult(bashText(out, errs, fmt.Sprintf("exit code %d", code)))
	if err == nil {
		res.ExitCode = &code
	}
	return res, nil
}

// exited handles a shell that closed its output before the end mark, for
// example after the command "exit".
func (t *bashTool) exited(ctx context.Context, s *bashSession, out, errs *collector) fw.ToolResult {
	sh := s.sh
	s.sh = nil
	killGroup(sh.cmd)
	_ = sh.stdin.Close()
	status := sh.cmd.Wait()
	for b := range sh.stdout {
		out.add(b)
	}
	for b := range sh.stderr {
		errs.add(b)
	}
	msg := "The shell closed its standard output, so it was stopped. The next call starts a new shell."
	var code *int
	if ps := sh.cmd.ProcessState; ps != nil && ps.Exited() {
		msg = fmt.Sprintf("The shell exited (exit status %d). The next call starts a new shell.", ps.ExitCode())
		c := ps.ExitCode()
		code = &c
	}
	logger(ctx).Warn("bash: shell ended", "status", fmt.Sprint(status))
	return fw.ToolResult{Content: []fw.Block{fw.Text(bashText(out, errs, msg))}, IsError: true, ExitCode: code}
}

// bashText builds "<stdout>\n<stderr>\n<footer>" and leaves out empty parts.
// A body longer than the head that the collectors keep loses its middle.
func bashText(out, errs *collector, footer string) string {
	var parts []string
	total, cut := 0, false
	for _, c := range []*collector{out, errs} {
		s := strings.TrimRight(string(c.output()), "\n")
		total += len(c.buf) + c.dropped
		cut = cut || c.dropped > 0
		if s != "" {
			parts = append(parts, s)
		}
	}
	body := strings.Join(parts, "\n")
	over := len(body) > out.limit
	if over {
		body = fw.CutMiddle(body, out.limit, out.limit)
	}
	if cut || over {
		body += fmt.Sprintf("\n[output truncated: %d bytes in total; the head and the tail are shown]", total)
	}
	if body == "" {
		return footer
	}
	return body + "\n" + footer
}

// shell is one running bash process.
type shell struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    <-chan []byte
	stderr    <-chan []byte
	outLeft   []byte // output after the last end mark, such as from a background job
	errLeft   []byte
	errClosed bool // the shell closed its stderr pipe
}

// startShell starts bash in dir with the environment env (shellEnv), as
// the tool user u when u is not nil.
func startShell(dir string, env []string, u *fw.ToolUser) (*shell, error) {
	cmd := exec.Command("bash", "--noprofile", "--norc")
	cmd.Dir = dir
	cmd.Env = env
	setGroup(cmd)
	u.Apply(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &shell{cmd: cmd, stdin: stdin, stdout: pump(stdout), stderr: pump(stderr)}, nil
}

// kill ends the process group and releases the pipes.
func (s *shell) kill() {
	killGroup(s.cmd)
	_ = s.stdin.Close()
	_ = s.cmd.Wait()
	go drain(s.stdout)
	go drain(s.stderr)
}

// pump copies a pipe into a channel and closes the channel at the end.
func pump(r io.Reader) <-chan []byte {
	ch := make(chan []byte, 16)
	go func() {
		defer close(ch)
		for {
			b := make([]byte, 32<<10)
			n, err := r.Read(b)
			if n > 0 {
				ch <- b[:n]
			}
			if err != nil {
				return
			}
		}
	}()
	return ch
}

func drain(ch <-chan []byte) {
	for range ch {
	}
}

// collector gathers one stream up to an end mark and the line end after it.
type collector struct {
	mark    []byte
	buf     []byte
	at      int // index of the mark in buf, or -1
	dropped int // bytes dropped from the middle to respect the cap
	limit   int // the head that the cap keeps, before a tail of tailKeep
	done    bool
	tail    string // text between the mark and the line end
	left    []byte // bytes after the line end
}

func newCollector(mark string, left []byte, limit int) *collector {
	c := &collector{mark: []byte(mark), at: -1, limit: limit}
	if len(left) > 0 {
		c.add(left)
	}
	return c
}

func (c *collector) add(b []byte) {
	if c.done {
		if len(c.left) < maxOutput {
			c.left = append(c.left, b...)
		}
		return
	}
	from := max(0, len(c.buf)-len(c.mark))
	c.buf = append(c.buf, b...)
	if c.at < 0 {
		if i := bytes.Index(c.buf[from:], c.mark); i >= 0 {
			c.at = from + i
		}
	}
	if c.at >= 0 {
		rest := c.buf[c.at+len(c.mark):]
		if j := bytes.IndexByte(rest, '\n'); j >= 0 {
			c.done = true
			c.tail = string(rest[:j])
			c.left = append([]byte(nil), rest[j+1:]...)
			c.buf = c.buf[:c.at]
		}
		return
	}
	if extra := len(c.buf) - c.limit - tailKeep; extra > 0 {
		n := copy(c.buf[c.limit:], c.buf[c.limit+extra:])
		c.buf = c.buf[:c.limit+n]
		c.dropped += extra
	}
}

// staleMark matches an end mark of an earlier call. It shows up when a
// command merges stdout and stderr, for example with "exec 2>&1".
var staleMark = regexp.MustCompile(`__FW_END_[0-9a-f]{32}(?: -?[0-9]+)?\n?`)

// output returns the collected text before the mark, without stale marks.
// After a drop, a note between the head and the tail says how many bytes
// are gone.
func (c *collector) output() []byte {
	b := c.buf
	if c.dropped > 0 && len(b) > c.limit {
		gap := fmt.Sprintf("\n[... %d bytes dropped here ...]\n", c.dropped)
		b = append(append(append([]byte(nil), b[:c.limit]...), gap...), b[c.limit:]...)
	}
	return staleMark.ReplaceAll(b, nil)
}

// randomToken returns prefix and 32 random hex digits.
func randomToken(prefix string) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}
