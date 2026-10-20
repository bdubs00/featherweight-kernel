package harness

// This file is the driver of Claude Code. Each exchange of a run is one
// process of claude -p with a stream of JSON lines in both directions, in
// the session of the run, whose transcript lives in the home of the
// harness. A PreToolUse hook of the type http asks the kernel about each
// tool call on a loopback route of the native API. The kernel answers
// allow or deny from the policy of the run, and defer where the policy
// asks a person: the process then exits, the run waits as an idle run,
// and after the decision claude -p --resume continues the session, and the
// hook answers the same call from the decision.

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// hookPath is the route of the hook, before its secret.
const hookPath = "/v1/harness/hook/"

// claude is the driver of Claude Code.
type claude struct {
	h *host

	mu    sync.Mutex
	procs map[*exec.Cmd]bool
}

// claudeProc is one running process of claude and what its hook needs.
type claudeProc struct {
	r         *run
	decisions map[string]decision // the decisions of the run, by call id

	mu       sync.Mutex
	verdicts map[string]fw.Decision // the calls that the hook deferred, by call id
}

// Close stops every process of claude.
func (c *claude) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for cmd := range c.procs {
		stop(cmd, syscall.SIGKILL)
	}
	return nil
}

// canceled has nothing to stop: a run that waits has no process, and the
// context of a working run ends its process.
func (c *claude) canceled(string) {}

// outcomes of one process
type ccOutcome int

const (
	ccIdle    ccOutcome = iota // the process finished the exchange
	ccWaiting                  // a deferred call waits for a person
	ccOver                     // the run reached a limit of its budget and finished
	ccFailed                   // the run failed and finished
)

// drive implements driver.
func (c *claude) drive(ctx context.Context, r *run) error {
	m, err := r.modelOf(ctx)
	if err != nil {
		return r.fail(ctx, err.Error())
	}
	if m.provider != "anthropic" {
		return r.fail(ctx, fmt.Sprintf("harness: claude-code needs a model of the provider anthropic, such as anthropic/claude-sonnet-4-5; the model of the run is %s/%s", m.provider, m.name))
	}
	if m.key == "" {
		return r.fail(ctx, "harness: claude-code needs the key of its model in "+m.keyEnv)
	}
	hook, err := c.hookURL(ctx)
	if err != nil {
		return r.fail(ctx, err.Error())
	}
	dir, rel, err := r.hr.Dir()
	if err != nil {
		return r.fail(ctx, "harness: the directory of the run: "+err.Error())
	}
	files := newTracker(c.h, dir, rel)
	for {
		st := r.rescan()
		if st.pending != nil {
			return nil
		}
		answered := false
		for _, d := range st.decided {
			// The hook answers the call from the decision in the ledger
			// when the resumed process asks again.
			if err := r.mark(ctx, EventData{Kind: KindAnswer, Approval: d.req.ApprovalID}); err != nil {
				return err
			}
			answered = true
		}
		open := st.openExchange()
		st = r.rescan()
		text, through := st.prompt()
		var lines []string
		switch {
		case text != "":
			lines = []string{text}
		case answered:
		case open:
			lines, through = []string{restartNote}, st.prompted
		case through > st.prompted:
			// The new messages hold no text: the session gets nothing.
			if err := r.skip(ctx, through); err != nil {
				return err
			}
			continue
		case !st.terminal:
			if done, err := r.hr.Finish(ctx, fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Output: st.output}, st.prompted); err != nil || done {
				return err
			}
			continue
		default:
			return nil
		}
		rec := newRecorder(r, "anthropic", m.name, files)
		if open && !answered {
			// The process stopped during the exchange. Its open calls have
			// no result that the kernel can read.
			if err := rec.interrupt(ctx, "not finished: the harness stopped before the call ended"); err != nil {
				if errors.Is(err, errOver) {
					return nil
				}
				return err
			}
		}
		sid, resume := st.session, st.session != ""
		if sid == "" {
			sid = newUUID()
			if err := r.mark(ctx, EventData{Kind: KindSession, Session: sid}); err != nil {
				return err
			}
		}
		out, err := c.process(ctx, r, rec, m, hook, dir, sid, resume, lines, through)
		if err != nil {
			return err
		}
		switch out {
		case ccWaiting, ccOver, ccFailed:
			return nil
		}
		st = r.rescan()
		if done, err := r.hr.Finish(ctx, fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Output: st.output}, st.prompted); err != nil || done {
			return err
		}
	}
}

// hookURL returns the loopback URL of the hook route, before its secret.
// It waits for the native API to mount the route, since a run that a
// restart resumes can start first.
func (c *claude) hookURL(ctx context.Context) (string, error) {
	cfg := c.h.a.Config()
	if cfg.TLS.Provider != "" {
		return "", errors.New("harness: claude-code calls its hook over plain HTTP on the loopback address, and the listener of fw uses TLS")
	}
	wait := time.NewTimer(30 * time.Second)
	defer wait.Stop()
	for {
		c.h.mu.Lock()
		n := c.h.mounts
		c.h.mu.Unlock()
		if n > 0 {
			break
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-wait.C:
			return "", errors.New("harness: claude-code needs the native API of fw, which serves its hook route; run fw serve with the surface native")
		case <-time.After(100 * time.Millisecond):
		}
	}
	host, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return "", fmt.Errorf("harness: the listen address %q of fw: %w", cfg.Listen, err)
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + hookPath, nil
}

// process runs one process of claude for one exchange.
func (c *claude) process(ctx context.Context, r *run, rec *recorder, m modelConf, hook, dir, sid string, resume bool, lines []string, through uint64) (ccOutcome, error) {
	p := &claudeProc{r: r, decisions: r.st.decisions, verdicts: map[string]fw.Decision{}}
	key := secret()
	c.h.mu.Lock()
	c.h.hooks[key] = p
	c.h.mu.Unlock()
	defer func() {
		c.h.mu.Lock()
		delete(c.h.hooks, key)
		c.h.mu.Unlock()
	}()
	settings := mustJSON(map[string]any{"hooks": map[string]any{"PreToolUse": []any{map[string]any{
		"matcher": "*",
		"hooks":   []any{map[string]any{"type": "http", "url": hook + key, "timeout": 120, "onFailure": "block"}},
	}}}})
	argv := append([]string(nil), c.h.cfg.Command...)
	if len(argv) == 0 {
		argv = []string{"claude"}
	}
	argv = append(argv, "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--model", m.name, "--settings", settings)
	if resume {
		argv = append(argv, "--resume", sid)
	} else {
		argv = append(argv, "--session-id", sid)
	}
	b, u := r.hr.Budget(), r.hr.Usage()
	if b.MaxTurns > 0 {
		argv = append(argv, "--max-turns", strconv.Itoa(max(b.MaxTurns-r.st.turns, 1)))
	}
	if b.MaxCostUSD > 0 {
		argv = append(argv, "--max-budget-usd", strconv.FormatFloat(max(b.MaxCostUSD-u.CostUSD, 0.0001), 'f', 4, 64))
	}
	if in := strings.TrimSpace(r.hr.Queued().Instructions); in != "" {
		argv = append(argv, "--append-system-prompt", in)
	}
	extra := []string{
		"CLAUDE_CONFIG_DIR=" + c.h.home,
		"ANTHROPIC_API_KEY=" + m.key,
		"DISABLE_AUTOUPDATER=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"CLAUDE_CODE_RESUME_INTERRUPTED_TURN=1",
	}
	if m.baseURL != "" {
		extra = append(extra, "ANTHROPIC_BASE_URL="+m.baseURL)
	}
	cmd, err := c.h.command(ctx, argv, dir, extra...)
	if err != nil {
		return 0, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 0, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return 0, err
	}
	if err := cmd.Start(); err != nil {
		return ccFailed, r.fail(ctx, fmt.Sprintf("harness: start %s: %v", argv[0], err))
	}
	c.mu.Lock()
	if c.procs == nil {
		c.procs = map[*exec.Cmd]bool{}
	}
	c.procs[cmd] = true
	c.mu.Unlock()
	exited := make(chan error, 1)
	go c.h.logLines(stderr, "stderr")
	out := make(chan []byte, 64)
	go func() {
		// Wait closes the pipe of stdout, so it comes after the last line.
		readLines(stdout, out)
		exited <- cmd.Wait()
	}()
	defer func() {
		c.mu.Lock()
		delete(c.procs, cmd)
		c.mu.Unlock()
	}()
	kill := func() {
		_ = stdin.Close()
		stop(cmd, syscall.SIGINT)
		select {
		case <-exited:
		case <-time.After(3 * time.Second):
			stop(cmd, syscall.SIGKILL)
			<-exited
		}
	}
	for _, l := range lines {
		if err := writeUser(stdin, l); err != nil {
			kill()
			return ccFailed, r.fail(ctx, "harness: send the message to claude: "+err.Error())
		}
	}
	if len(lines) > 0 {
		if err := r.mark(ctx, EventData{Kind: KindPrompt, Seq: through}); err != nil {
			kill()
			return 0, err
		}
	}
	m2 := &ccMapper{c: c, r: r, rec: rec, p: p, sub: map[string]fw.Usage{}}
	var result *ccResult
	for {
		select {
		case <-ctx.Done():
			kill()
			return 0, ctx.Err()
		case line, ok := <-out:
			if !ok {
				err := <-exited
				return c.ended(ctx, r, rec, m2, result, err)
			}
			res, err := m2.line(ctx, line)
			if err != nil {
				kill()
				if errors.Is(err, errOver) {
					return ccOver, nil
				}
				return 0, err
			}
			if res == nil {
				continue
			}
			result = res
			// A message that came during the exchange goes to the same
			// process; without one, the input ends and the process exits.
			st := r.rescan()
			if res.deferred == nil && res.ok() {
				if text, last := st.prompt(); text != "" {
					if err := writeUser(stdin, text); err == nil {
						if err := r.mark(ctx, EventData{Kind: KindPrompt, Seq: last}); err != nil {
							kill()
							return 0, err
						}
						result = nil
						continue
					}
				}
			}
			_ = stdin.Close()
		}
	}
}

// ended handles the end of a process.
func (c *claude) ended(ctx context.Context, r *run, rec *recorder, m *ccMapper, res *ccResult, exit error) (ccOutcome, error) {
	if err := m.flush(ctx); err != nil {
		if errors.Is(err, errOver) {
			return ccOver, nil
		}
		return 0, err
	}
	if err := r.mark(ctx, EventData{Kind: KindIdle}); err != nil {
		return 0, err
	}
	if res == nil {
		if err := rec.interrupt(ctx, "not finished: claude exited without a result"); err != nil && !errors.Is(err, errOver) {
			return 0, err
		}
		msg := "harness: claude exited without a result"
		if exit != nil {
			msg += ": " + exit.Error()
		}
		return ccFailed, r.fail(ctx, msg)
	}
	if d := res.deferred; d != nil {
		tool := strings.ToLower(d.Name)
		if err := rec.request(ctx, d.ID, tool, d.Input); err != nil {
			return 0, err
		}
		m.p.mu.Lock()
		v := m.p.verdicts[d.ID]
		m.p.mu.Unlock()
		if _, err := r.approval(ctx, d.ID, tool, d.Input, v.Reason); err != nil {
			return 0, err
		}
		return ccWaiting, nil
	}
	if err := rec.end(ctx); err != nil {
		if errors.Is(err, errOver) {
			return ccOver, nil
		}
		return 0, err
	}
	switch res.Subtype {
	case "success":
		if res.IsError {
			return ccFailed, r.fail(ctx, "claude: "+res.Result)
		}
		if t := strings.TrimSpace(res.Result); t != "" {
			r.st.output = t
		}
		if err := rec.files.record(ctx, r); err != nil {
			return 0, err
		}
		return ccIdle, nil
	case "error_max_turns", "error_max_budget_usd":
		if err := rec.check(ctx); err != nil {
			if errors.Is(err, errOver) {
				return ccOver, nil
			}
			return 0, err
		}
		limit := "maxTurns"
		d := fw.BudgetExceededData{Limit: limit, Value: float64(rec.turn), Max: float64(r.hr.Budget().MaxTurns)}
		if res.Subtype == "error_max_budget_usd" {
			limit = "maxCostUSD"
			d = fw.BudgetExceededData{Limit: limit, Value: r.hr.Usage().CostUSD, Max: r.hr.Budget().MaxCostUSD}
		}
		if _, err := r.hr.Emit(ctx, fw.EvBudgetExceeded, d); err != nil {
			return 0, err
		}
		if err := rec.files.record(ctx, r); err != nil {
			return 0, err
		}
		_, err := r.hr.Emit(ctx, fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeFailed, Output: r.st.output, Error: "budget exceeded: " + limit})
		return ccOver, err
	}
	msg := strings.Join(res.Errors, "; ")
	if msg == "" {
		msg = res.Subtype
	}
	if err := rec.files.record(ctx, r); err != nil {
		return 0, err
	}
	return ccFailed, r.fail(ctx, "claude: "+msg)
}

// ccResult is the result line of a process.
type ccResult struct {
	Subtype    string   `json:"subtype"`
	IsError    bool     `json:"is_error"`
	Result     string   `json:"result"`
	StopReason string   `json:"stop_reason"`
	NumTurns   int      `json:"num_turns"`
	Cost       float64  `json:"total_cost_usd"`
	Errors     []string `json:"errors"`
	SessionID  string   `json:"session_id"`
	deferred   *ccDeferred
}

type ccDeferred struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

func (r *ccResult) ok() bool { return r.Subtype == "success" && !r.IsError }

// ccMapper maps the lines of one process onto the run.
type ccMapper struct {
	c   *claude
	r   *run
	rec *recorder
	p   *claudeProc
	cur *ccMessage          // the model call of the session that is not recorded yet
	sub map[string]fw.Usage // the usage of the model calls of subagents, by message id
}

// ccMessage is one model call: the assistant lines with one message id.
type ccMessage struct {
	id      string
	content []fw.Block
	usage   fw.Usage
	stop    string
}

type ccUsage struct {
	Input      int `json:"input_tokens"`
	Output     int `json:"output_tokens"`
	CacheWrite int `json:"cache_creation_input_tokens"`
	CacheRead  int `json:"cache_read_input_tokens"`
}

func (u ccUsage) usage() fw.Usage {
	return fw.Usage{InputTokens: u.Input, OutputTokens: u.Output, CacheReadTokens: u.CacheRead, CacheWriteTokens: u.CacheWrite}
}

// line maps one line. It returns the result when the line is one.
func (m *ccMapper) line(ctx context.Context, line []byte) (*ccResult, error) {
	var ev struct {
		Type    string  `json:"type"`
		Subtype string  `json:"subtype"`
		Parent  *string `json:"parent_tool_use_id"`
		Message struct {
			ID         string          `json:"id"`
			Model      string          `json:"model"`
			Content    json.RawMessage `json:"content"`
			StopReason string          `json:"stop_reason"`
			Usage      ccUsage         `json:"usage"`
		} `json:"message"`
		Deferred *ccDeferred `json:"deferred_tool_use"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		m.c.h.log.Debug("claude wrote a line that is not JSON", "line", m.c.h.a.Redact(oneLine(string(line), 200)))
		return nil, nil
	}
	sub := ev.Parent != nil && *ev.Parent != ""
	switch ev.Type {
	case "assistant":
		blocks := ccBlocks(ev.Message.Content)
		var tools []string
		for _, b := range blocks {
			if b.Type == fw.BlockToolUse {
				tools = append(tools, b.Name)
			}
		}
		if err := m.r.raw(ctx, "assistant", map[string]any{"message": ev.Message.ID, "stopReason": ev.Message.StopReason, "tools": tools, "subagent": sub}); err != nil {
			return nil, err
		}
		u := ev.Message.Usage.usage()
		if sub {
			m.sub[ev.Message.ID] = maxUsage(m.sub[ev.Message.ID], u)
			return nil, nil
		}
		if err := m.subUsage(ctx); err != nil {
			return nil, err
		}
		if m.cur != nil && m.cur.id != ev.Message.ID {
			if err := m.flush(ctx); err != nil {
				return nil, err
			}
		}
		if m.cur == nil {
			m.cur = &ccMessage{id: ev.Message.ID}
		}
		m.cur.content = append(m.cur.content, blocks...)
		m.cur.usage = maxUsage(m.cur.usage, u)
		if ev.Message.StopReason != "" {
			m.cur.stop = ev.Message.StopReason
		}
		return nil, nil
	case "user":
		if sub {
			return nil, nil
		}
		if err := m.flush(ctx); err != nil {
			return nil, err
		}
		var blocks []struct {
			Type      string          `json:"type"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
			IsError   bool            `json:"is_error"`
		}
		if json.Unmarshal(ev.Message.Content, &blocks) != nil {
			return nil, nil
		}
		n := 0
		for _, b := range blocks {
			if b.Type != "tool_result" {
				continue
			}
			if _, open := m.rec.calls[b.ToolUseID]; !open {
				continue
			}
			n++
			if err := m.rec.result(ctx, b.ToolUseID, "", resultText(b.Content), b.IsError); err != nil {
				return nil, err
			}
		}
		if n > 0 {
			return nil, m.r.raw(ctx, "user", map[string]int{"results": n})
		}
		return nil, nil
	case "result":
		if err := m.flush(ctx); err != nil {
			return nil, err
		}
		if err := m.subUsage(ctx); err != nil {
			return nil, err
		}
		var res ccResult
		_ = json.Unmarshal(line, &res)
		if res.StopReason == "tool_deferred" && ev.Deferred != nil {
			res.deferred = ev.Deferred
		}
		d := map[string]any{"subtype": res.Subtype, "stopReason": res.StopReason, "isError": res.IsError, "numTurns": res.NumTurns, "costUSD": res.Cost}
		if res.deferred != nil {
			d["deferred"] = map[string]string{"id": res.deferred.ID, "name": res.deferred.Name}
		}
		return &res, m.r.raw(ctx, "result", d)
	case "system":
		var sys struct {
			Model   string `json:"model"`
			Version string `json:"claude_code_version"`
			Mode    string `json:"permissionMode"`
			Session string `json:"session_id"`
		}
		_ = json.Unmarshal(line, &sys)
		return nil, m.r.raw(ctx, "system", map[string]string{"subtype": ev.Subtype, "model": sys.Model, "version": sys.Version, "permissionMode": sys.Mode, "session": sys.Session})
	case "stream_event":
		return nil, nil
	}
	return nil, m.r.raw(ctx, ev.Type, map[string]string{"subtype": ev.Subtype})
}

// flush records the model call that is not recorded yet: a turn with its
// response and its tool calls.
func (m *ccMapper) flush(ctx context.Context) error {
	cur := m.cur
	if cur == nil {
		return nil
	}
	m.cur = nil
	if err := m.rec.begin(ctx); err != nil {
		return err
	}
	stop := fw.StopEndTurn
	switch cur.stop {
	case "tool_use":
		stop = fw.StopToolUse
	case "max_tokens":
		stop = fw.StopMaxTokens
	}
	if err := m.rec.respond(ctx, cur.content, cur.usage, stop); err != nil {
		return err
	}
	for _, b := range cur.content {
		if b.Type == fw.BlockToolUse {
			if err := m.rec.request(ctx, b.ID, b.Name, b.Input); err != nil {
				return err
			}
		}
	}
	return nil
}

// subUsage records the usage of the model calls of subagents, which the
// budget of the run counts.
func (m *ccMapper) subUsage(ctx context.Context) error {
	for id, u := range m.sub {
		delete(m.sub, id)
		if err := m.rec.usage(ctx, u); err != nil {
			return err
		}
	}
	return nil
}

// ccBlocks returns the text and the tool calls of the content of an
// assistant line. The kernel names the tools in lower case.
func ccBlocks(raw json.RawMessage) []fw.Block {
	var blocks []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	var out []fw.Block
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) != "" {
				out = append(out, fw.Text(b.Text))
			}
		case "tool_use":
			input := b.Input
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			out = append(out, fw.Block{Type: fw.BlockToolUse, ID: b.ID, Name: strings.ToLower(b.Name), Input: input})
		}
	}
	return out
}

// resultText returns the text of the content of a tool result: a string,
// or a list of blocks.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return string(raw)
}

func maxUsage(a, b fw.Usage) fw.Usage {
	return fw.Usage{InputTokens: max(a.InputTokens, b.InputTokens), OutputTokens: max(a.OutputTokens, b.OutputTokens),
		CacheReadTokens: max(a.CacheReadTokens, b.CacheReadTokens), CacheWriteTokens: max(a.CacheWriteTokens, b.CacheWriteTokens)}
}

// writeUser writes one message of the person as a line of stream-json.
func writeUser(w io.Writer, text string) error {
	b, err := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}})
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// readLines sends each line of r to out and closes out at the end.
func readLines(r io.Reader, out chan<- []byte) {
	defer close(out)
	rd := bufio.NewReaderSize(r, 256*1024)
	for {
		line, err := rd.ReadBytes('\n')
		if len(line) > 0 {
			if l := strings.TrimSpace(string(line)); l != "" {
				out <- []byte(l)
			}
		}
		if err != nil {
			return
		}
	}
}

// hookInput is the body that the PreToolUse hook posts.
type hookInput struct {
	Event     string          `json:"hook_event_name"`
	Tool      string          `json:"tool_name"`
	Input     json.RawMessage `json:"tool_input"`
	ToolUseID string          `json:"tool_use_id"`
	Session   string          `json:"session_id"`
}

// hook answers the PreToolUse hook of a process of claude from the policy
// of its run. Only a process that this kernel started knows the secret of
// its URL, and only a caller on the loopback address reaches it.
func (h *host) hook(w http.ResponseWriter, req *http.Request) {
	ip, _, err := net.SplitHostPort(req.RemoteAddr)
	if addr := net.ParseIP(ip); err != nil || addr == nil || !addr.IsLoopback() {
		http.NotFound(w, req)
		return
	}
	h.mu.Lock()
	p := h.hooks[req.PathValue("secret")]
	h.mu.Unlock()
	if p == nil {
		answerHook(w, "deny", "no session of fw waits for this hook")
		return
	}
	var in hookInput
	if err := json.NewDecoder(io.LimitReader(req.Body, 4<<20)).Decode(&in); err != nil {
		answerHook(w, "deny", "the hook input is not JSON")
		return
	}
	verdict, reason := p.decide(req.Context(), in)
	h.log.Debug("harness hook", "run", p.r.hr.ID(), "tool", in.Tool, "call", in.ToolUseID, "decision", verdict)
	answerHook(w, verdict, reason)
}

// decide answers one call: from the decision of a person for a call that
// waited, else from the policy, with defer where the policy asks.
func (p *claudeProc) decide(ctx context.Context, in hookInput) (string, string) {
	tool := strings.ToLower(in.Tool)
	if d, ok := p.decisions[in.ToolUseID]; ok {
		if allow, why := p.r.recheck(ctx, d); !allow {
			return "deny", why
		}
		return "allow", ""
	}
	v := p.r.decide(ctx, in.ToolUseID, tool, in.Input)
	switch v.Verdict {
	case fw.VerdictAllow:
		return "allow", ""
	case fw.VerdictDeny:
		return "deny", "denied by policy: " + v.Reason
	}
	p.mu.Lock()
	p.verdicts[in.ToolUseID] = v
	p.mu.Unlock()
	return "defer", v.Reason
}

// answerHook writes the answer of a PreToolUse hook.
func answerHook(w http.ResponseWriter, decision, reason string) {
	out := map[string]any{"hookEventName": "PreToolUse", "permissionDecision": decision}
	if reason != "" {
		out["permissionDecisionReason"] = reason
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"hookSpecificOutput": out})
}

// newUUID returns a random UUID of version 4.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
