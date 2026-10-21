package harness

// This file is the driver of opencode. The kernel starts one opencode
// serve on a loopback port for the process and gives each run a session
// of it. It follows the event stream of the server, records each model
// call as a turn, and answers each permission request of opencode from
// the policy of the run. The configuration of opencode makes every tool
// ask, so the kernel decides every call. The driver speaks the HTTP API of
// opencode 1 (https://opencode.ai/docs/server/).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// opencode is the driver of opencode.
type opencode struct {
	h *host
	// starting holds a token while one driver stops or starts the server.
	// The start of a server takes up to 90 seconds, and mu stays free
	// meanwhile, so a cancel of another run never waits for it.
	starting chan struct{}

	mu       sync.Mutex
	srv      *ocServer
	sessions map[string]*ocSession // by run id
	closed   bool
}

func newOpencode(h *host) *opencode {
	return &opencode{h: h, starting: make(chan struct{}, 1), sessions: map[string]*ocSession{}}
}

// ocServer is one opencode serve.
type ocServer struct {
	cmd    *exec.Cmd
	base   string
	pass   string
	mem    string          // the secret of its memory route, or "" when it has no memory server
	memWhy string          // why it has no memory server, for an agent with memory
	models map[string]bool // the selectors that its configuration holds
	env    string          // the digest of its environment (envDigest)
	client *http.Client
	done   chan struct{} // closed when the process exits
}

func (s *ocServer) dead() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// ocSession is the state in memory of the session of one run.
type ocSession struct {
	srv   *ocServer
	id    string
	dir   string
	queue *ocQueue
	stop  context.CancelFunc // ends the subscription
	rec   *recorder
	model modelConf

	family     map[string]bool    // the session and the sessions of its subagents
	assistants map[string]bool    // the assistant messages of the session
	tools      map[string]*ocTool // the tool calls, by call id
	perms      map[string]ocPerm  // the permissions that wait, by approval id
	carried    []decision         // decisions that a restart kept from the harness
	step       *ocStep            // the model call that runs
	busy       bool               // the session worked in this exchange
	failure    string             // the latest error of the session
	// memWhy says why the memory server of the server does not serve this
	// session, such as a project configuration that changes its entry
	// (ocMemEntry), or "".
	memWhy string
}

type ocTool struct {
	tool   string // the name of the tool in lower case
	name   string // the name of the tool as opencode reports it
	input  json.RawMessage
	state  string
	result string // the output or the error of a call that ended
	done   bool   // the run recorded the result
}

// ocPerm is a permission that waits for its answer, with the tool call
// that asked for it. tool is the name of the tool in lower case, and name
// the name as opencode reports it. input is nil when the host did not see
// the input of the call.
type ocPerm struct {
	id, session, callID, tool, name string
	input                           json.RawMessage
}

// ocStep is one model call of the session: one turn of the run.
type ocStep struct {
	texts   map[string]string
	order   []string // the text parts and the tool calls, in order; a tool call is "tool:" and its id
	pending []string // the tool calls that ended before the response was recorded
}

// ocEvent is one event of the event stream.
type ocEvent struct {
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}

// Close stops the server.
func (o *opencode) Close() error {
	o.mu.Lock()
	srv := o.srv
	o.srv, o.closed = nil, true
	for id, s := range o.sessions {
		s.stop()
		delete(o.sessions, id)
	}
	o.mu.Unlock()
	if srv != nil {
		stop(srv.cmd, syscall.SIGTERM)
		select {
		case <-srv.done:
		case <-time.After(5 * time.Second):
			stop(srv.cmd, syscall.SIGKILL)
		}
	}
	return nil
}

// askAll is the permission configuration that makes opencode ask about
// every tool, so the kernel decides each call. It names every permission
// key, because a key of a project configuration wins over "*". The tools
// of the memory server are keys too: the memory server makes only the
// calls that the kernel allowed (memory.go).
var askAll = func() map[string]string {
	m := map[string]string{"*": "ask"}
	for _, k := range []string{"read", "edit", "write", "patch", "glob", "grep", "list", "bash", "task", "skill", "lsp",
		"question", "webfetch", "websearch", "codesearch", "external_directory", "doom_loop", "todowrite", "todoread"} {
		m[k] = "ask"
	}
	for _, t := range memTools {
		m[memPrefix(KindOpencode)+t] = "ask"
	}
	return m
}()

// ocProvider returns the provider of opencode for a provider of the
// kernel: its name, its npm package, and its base URL.
func ocProvider(m modelConf) (id, npm, base string, err error) {
	id = "fw-" + m.provider
	switch m.provider {
	case "openai":
		npm, base = "@ai-sdk/openai-compatible", m.baseURL
		if base == "" {
			base = "https://api.openai.com/v1"
		}
	case "anthropic":
		npm, base = "@ai-sdk/anthropic", strings.TrimRight(m.baseURL, "/")
		if base != "" && !strings.HasSuffix(base, "/v1") {
			base += "/v1"
		}
	case "gemini":
		npm, base = "@ai-sdk/google", m.baseURL
	default:
		return "", "", "", fmt.Errorf("harness: opencode cannot use the provider %q; use openai, anthropic, or gemini", m.provider)
	}
	return id, npm, base, nil
}

// ocConfig returns the inline configuration of opencode: one provider of
// opencode for each provider of the kernel that the models use, with the
// endpoint of the kernel and the variable of its key, every agent on ask,
// and the memory server at memURL unless it is "". The key itself never
// enters the configuration.
func ocConfig(models []modelConf, def modelConf, memURL string) ([]byte, error) {
	providers := map[string]any{}
	var enabled []string
	for _, m := range models {
		id, npm, base, err := ocProvider(m)
		if err != nil {
			return nil, err
		}
		p, ok := providers[id].(map[string]any)
		if !ok {
			opts := map[string]any{"apiKey": "none"}
			if m.key != "" {
				opts["apiKey"] = "{env:" + m.keyEnv + "}"
			}
			if base != "" {
				opts["baseURL"] = base
			}
			p = map[string]any{"npm": npm, "name": "fw " + m.provider, "options": opts, "models": map[string]any{}}
			providers[id] = p
			enabled = append(enabled, id)
		}
		p["models"].(map[string]any)[m.name] = map[string]any{"name": m.name}
	}
	sort.Strings(enabled)
	defID, _, _, _ := ocProvider(def)
	agents := map[string]any{}
	for _, name := range []string{"build", "plan", "general", "explore"} {
		agents[name] = map[string]any{"permission": askAll}
	}
	conf := map[string]any{
		"$schema":           "https://opencode.ai/config.json",
		"provider":          providers,
		"enabled_providers": enabled,
		"model":             defID + "/" + def.name,
		"autoupdate":        false,
		"share":             "disabled",
		"permission":        askAll,
		"agent":             agents,
	}
	if memURL != "" {
		conf["mcp"] = ocMCPConfig(memURL)
	}
	return json.Marshal(conf)
}

// models returns the models that the configuration of the agent names,
// with the model of the run, so one server serves the runs of every
// profile.
func (o *opencode) models(cur modelConf) []modelConf {
	cfg := o.h.a.Config()
	seen := map[string]bool{cur.provider + "/" + cur.name: true}
	out := []modelConf{cur}
	add := func(sel string) {
		sel = cfg.ModelSelector(sel)
		p, n := fw.SplitSelector(sel)
		if n == "" || seen[sel] {
			return
		}
		if _, _, _, err := ocProvider(modelConf{provider: p}); err != nil {
			return
		}
		seen[sel] = true
		pc := cfg.ProviderNamed(p)
		m := modelConf{provider: p, name: n, baseURL: strings.TrimSpace(pc.BaseURL), keyEnv: strings.TrimSpace(pc.APIKeyEnv)}
		if m.keyEnv == "" {
			m.keyEnv = defaultKeyEnv[p]
		}
		if m.keyEnv != "" {
			if key, err := o.h.a.Secrets().Get(context.Background(), m.keyEnv); err == nil {
				m.key = key
			}
		}
		out = append(out, m)
	}
	add(cfg.Model)
	for _, sel := range cfg.Models {
		add(sel)
	}
	for _, p := range o.h.a.Profiles() {
		add(p.Model)
	}
	return out
}

// server returns the running server, or starts one. The environment of
// the server comes from the run r that needs it, so the hooks of the
// linked packages wait for that run, such as for the install of the
// environment of section 18. A server whose configuration lacks the model
// of the run, or whose environment changed since it started, starts again
// when no session works. A changed environment with a session at work
// keeps the server as it is.
func (o *opencode) server(ctx context.Context, r *run, m modelConf) (*ocServer, error) {
	root, err := absRoot(o.h)
	if err != nil {
		return nil, err
	}
	argv := append([]string(nil), o.h.cfg.Command...)
	if len(argv) == 0 {
		argv = []string{"opencode", "serve"}
	}
	env, err := o.h.environ(ctx, r.info(r.dir), root, argv[0])
	if err != nil {
		return nil, err
	}
	digest := envDigest(env)
	select {
	case o.starting <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-o.starting }()
	sel := m.provider + "/" + m.name
	o.mu.Lock()
	s, busy := o.srv, len(o.sessions) > 0
	o.mu.Unlock()
	if s != nil && !s.dead() {
		switch {
		case s.models[sel] && (s.env == digest || busy):
			return s, nil
		case busy:
			return nil, fmt.Errorf("harness: the model %s is not in the configuration of the running opencode serve; name it in models of fw.yaml", sel)
		}
		if s.env != digest {
			o.h.log.Info("the environment of opencode serve changed; it starts again")
		}
		o.mu.Lock()
		if o.srv == s {
			o.srv = nil
		}
		o.mu.Unlock()
		s.retire()
	}
	models := o.models(m)
	// The memory server of the process has a secret of its own, which maps
	// to no run: each call that the kernel allowed names its run (memory.go).
	memKey, memURL := "", ""
	base, memWhy := o.h.memBase(ctx)
	if base != "" {
		memKey = secret()
		memURL = base + memPath + memKey
	}
	conf, err := ocConfig(models, m, memURL)
	if err != nil {
		return nil, err
	}
	port := o.h.cfg.Port
	if port == 0 {
		if port, err = freePort(); err != nil {
			return nil, err
		}
	}
	argv = append(argv, "--port", strconv.Itoa(port), "--hostname", "127.0.0.1")
	pass := secret()
	o.h.a.AddSecret(pass)
	if memKey != "" {
		// The URL of the memory server is in the environment of the
		// process, which a tool call of the harness can print.
		o.h.a.AddSecret(memKey)
		o.h.bindMemory(memKey)
	}
	extra := []string{
		"OPENCODE_SERVER_PASSWORD=" + pass,
		"OPENCODE_CONFIG_CONTENT=" + string(conf),
		"OPENCODE_PERMISSION=" + mustJSON(askAll),
		"OPENCODE_DISABLE_AUTOUPDATE=1",
		"OPENCODE_DISABLE_MODELS_FETCH=1",
	}
	keys := map[string]bool{}
	for _, mc := range models {
		if mc.key != "" && !keys[mc.keyEnv] {
			keys[mc.keyEnv] = true
			extra = append(extra, mc.keyEnv+"="+mc.key)
		}
	}
	cmd := o.h.command(argv, root, env, extra...)
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		o.h.unbindMemory(memKey)
		return nil, fmt.Errorf("harness: start %s: %w", argv[0], err)
	}
	go o.h.logLines(stdout, "stdout")
	go o.h.logLines(stderr, "stderr")
	s = &ocServer{cmd: cmd, base: "http://127.0.0.1:" + strconv.Itoa(port), pass: pass, mem: memKey, memWhy: memWhy, models: map[string]bool{}, env: digest, done: make(chan struct{}),
		client: &http.Client{Transport: &http.Transport{Proxy: nil, DisableCompression: true}}}
	for _, mc := range models {
		s.models[mc.provider+"/"+mc.name] = true
	}
	go func() {
		err := cmd.Wait()
		// A server that crashed leaves the processes that it started, such
		// as a shell of a tool call, in its process group.
		stop(cmd, syscall.SIGKILL)
		o.h.unbindMemory(memKey)
		o.h.log.Info("opencode serve ended", "error", err)
		close(s.done)
	}()
	if err := s.ready(ctx); err != nil {
		stop(cmd, syscall.SIGKILL)
		return nil, err
	}
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		stop(cmd, syscall.SIGKILL)
		return nil, errors.New("harness: the host of opencode stopped")
	}
	o.srv = s
	o.mu.Unlock()
	o.h.log.Info("opencode serve started", "port", port, "pid", cmd.Process.Pid)
	return s, nil
}

// retire stops the server and waits for its end, for a bounded time: a
// server that ignores SIGTERM gets SIGKILL after ten seconds.
func (s *ocServer) retire() {
	stop(s.cmd, syscall.SIGTERM)
	select {
	case <-s.done:
		return
	case <-time.After(10 * time.Second):
	}
	stop(s.cmd, syscall.SIGKILL)
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
	}
}

// ready waits until the server answers its health route.
func (s *ocServer) ready(ctx context.Context) error {
	deadline := time.Now().Add(90 * time.Second)
	for {
		// A server that starts can take a connection and answer nothing,
		// so each probe has a time limit of its own.
		pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		req, _ := http.NewRequestWithContext(pctx, http.MethodGet, s.base+"/global/health", nil)
		req.SetBasicAuth("opencode", s.pass)
		resp, err := s.client.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		cancel()
		if err == nil && resp.StatusCode == http.StatusOK {
			return nil
		}
		select {
		case <-s.done:
			return errors.New("harness: opencode serve ended before it was ready; the log of fw holds its output at the debug level")
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return errors.New("harness: opencode serve did not answer within 90 seconds")
		}
	}
}

// call sends one request to the server and decodes the answer into out
// when out is not nil.
func (s *ocServer) call(ctx context.Context, method, p string, q url.Values, body, out any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	u := s.base + p
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return 0, err
	}
	req.SetBasicAuth("opencode", s.pass)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("harness: opencode %s %s: %w", method, p, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("harness: opencode %s %s: %s: %s", method, p, resp.Status, strings.TrimSpace(string(b)))
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return resp.StatusCode, fmt.Errorf("harness: opencode %s %s: %w", method, p, err)
		}
	}
	return resp.StatusCode, nil
}

// subscribe follows the event stream of the directory dir until ctx ends.
// It returns once the server confirmed the stream, so no event of a
// prompt sent after it is lost.
func (s *ocServer) subscribe(ctx context.Context, dir string, q *ocQueue) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/event?"+url.Values{"directory": {dir}}.Encode(), nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth("opencode", s.pass)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("harness: opencode events: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return fmt.Errorf("harness: opencode events: %s", resp.Status)
	}
	connected := make(chan struct{})
	go func() {
		defer resp.Body.Close()
		rd := bufio.NewReaderSize(resp.Body, 64*1024)
		var data []byte
		once := sync.Once{}
		for {
			line, err := rd.ReadBytes('\n')
			if len(line) > 0 {
				line = bytes.TrimRight(line, "\r\n")
				switch {
				case len(line) == 0:
					if len(data) > 0 {
						var e ocEvent
						if json.Unmarshal(data, &e) == nil {
							if e.Type == "server.connected" {
								once.Do(func() { close(connected) })
							} else {
								q.push(e)
							}
						}
						data = data[:0]
					}
				case bytes.HasPrefix(line, []byte("data:")):
					if len(data) > 0 {
						data = append(data, '\n')
					}
					data = append(data, bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" "))...)
				}
			}
			if err != nil {
				once.Do(func() { close(connected) })
				q.close(err)
				return
			}
		}
	}()
	select {
	case <-connected:
		return nil
	case <-time.After(30 * time.Second):
		return errors.New("harness: opencode events: no server.connected within 30 seconds")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ocQueue holds the events of a session until the driver takes them, also
// while the run waits for a decision.
type ocQueue struct {
	mu     sync.Mutex
	items  []ocEvent
	wake   chan struct{}
	err    error
	closed bool
	last   time.Time // the latest event of the stream, a heartbeat included
}

func newQueue() *ocQueue { return &ocQueue{wake: make(chan struct{}, 1), last: time.Now()} }

func (q *ocQueue) push(e ocEvent) {
	q.mu.Lock()
	q.last = time.Now()
	q.mu.Unlock()
	switch e.Type {
	case "server.heartbeat", "message.part.delta":
		return
	}
	q.mu.Lock()
	q.items = append(q.items, e)
	q.mu.Unlock()
	q.signal()
}

// silence returns the time since the latest event of the stream.
func (q *ocQueue) silence() time.Duration {
	q.mu.Lock()
	defer q.mu.Unlock()
	return time.Since(q.last)
}

func (q *ocQueue) close(err error) {
	q.mu.Lock()
	if !q.closed {
		q.closed, q.err = true, err
	}
	q.mu.Unlock()
	q.signal()
}

func (q *ocQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// errLost means that the event stream or the server ended.
var errLost = errors.New("harness: the event stream of opencode ended")

// errTick means that the tick of next came before an event.
var errTick = errors.New("harness: no event before the tick")

// next returns the next event, or errTick when tick fires first. It fails
// with errLost once the stream ended and every event is taken.
func (q *ocQueue) next(ctx context.Context, tick <-chan time.Time) (ocEvent, error) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			e := q.items[0]
			q.items = q.items[1:]
			q.mu.Unlock()
			return e, nil
		}
		closed := q.closed
		q.mu.Unlock()
		if closed {
			return ocEvent{}, errLost
		}
		select {
		case <-q.wake:
		case <-tick:
			return ocEvent{}, errTick
		case <-ctx.Done():
			return ocEvent{}, ctx.Err()
		}
	}
}

// session returns the session of the run in memory, or makes it: it
// creates the session of opencode for a new run, follows its events, and
// continues the turns of the ledger. fresh is true for a session that
// this process did not follow before.
func (o *opencode) session(ctx context.Context, r *run, srv *ocServer, m modelConf) (*ocSession, bool, error) {
	o.mu.Lock()
	s := o.sessions[r.hr.ID()]
	o.mu.Unlock()
	if s != nil && s.srv == srv && !srv.dead() && !s.queue.isClosed() {
		s.rec.r = r
		return s, false, nil
	}
	if s != nil {
		o.drop(r.hr.ID())
	}
	dir, rel, err := r.hr.Dir()
	if err != nil {
		return nil, false, err
	}
	sid := r.st.session
	q := url.Values{"directory": {dir}}
	if sid == "" {
		var info struct {
			ID string `json:"id"`
		}
		title := r.st.run.Title
		if title == "" {
			title = "fw run " + r.hr.ID()
		}
		if _, err := srv.call(ctx, http.MethodPost, "/session", q, map[string]any{"title": title}, &info); err != nil {
			return nil, false, err
		}
		if info.ID == "" {
			return nil, false, errors.New("harness: opencode created a session without an id")
		}
		sid = info.ID
		if err := r.mark(ctx, EventData{Kind: KindSession, Session: sid}); err != nil {
			return nil, false, err
		}
	}
	sctx, cancel := context.WithCancel(context.Background())
	queue := newQueue()
	if err := srv.subscribe(sctx, dir, queue); err != nil {
		cancel()
		return nil, false, err
	}
	s = &ocSession{srv: srv, id: sid, dir: dir, queue: queue, stop: cancel, model: m,
		family: map[string]bool{sid: true}, assistants: map[string]bool{}, tools: map[string]*ocTool{}, perms: map[string]ocPerm{}}
	if srv.mem != "" {
		s.memWhy = o.memCheck(ctx, srv, dir)
	}
	// The events name the provider of the kernel, so the price table of
	// the kernel prices the calls.
	s.rec = newRecorder(r, m.provider, m.name, newTracker(o.h, dir, rel))
	o.mu.Lock()
	o.sessions[r.hr.ID()] = s
	o.mu.Unlock()
	return s, true, nil
}

// memCheck checks that opencode serves the directory dir with the memory
// server of the inline configuration of the kernel, and with nothing that
// a configuration of the project added to it (ocMemEntry). It returns why
// the session gets no memory calls, or "". The URL of the server holds its
// secret, so no log line holds the configuration.
func (o *opencode) memCheck(ctx context.Context, srv *ocServer, dir string) string {
	var conf json.RawMessage
	why := ""
	if _, err := srv.call(ctx, http.MethodGet, "/config", url.Values{"directory": {dir}}, nil, &conf); err != nil {
		why = "fw cannot read the configuration of opencode: " + o.h.a.Redact(err.Error())
	} else if base, err := o.h.listenBase(); err != nil {
		why = err.Error()
	} else {
		why = ocMemEntry(conf, base+memPath+srv.mem)
	}
	if why != "" {
		o.h.log.Warn("harness: opencode does not serve this session with the memory server of fw; its memory calls get a deny", "dir", dir, "reason", why)
	}
	return why
}

func (q *ocQueue) isClosed() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.closed
}

// canceled stops the session of a run that was canceled, such as a run
// that waited for a person: the permission that waits gets no answer.
func (o *opencode) canceled(id string) {
	o.mu.Lock()
	s := o.sessions[id]
	delete(o.sessions, id)
	o.mu.Unlock()
	if s == nil {
		return
	}
	s.stop()
	go o.abort(s)
}

// drop forgets the session of a run and ends its subscription.
func (o *opencode) drop(id string) {
	o.mu.Lock()
	s := o.sessions[id]
	delete(o.sessions, id)
	o.mu.Unlock()
	if s != nil {
		s.stop()
	}
}

// outcomes of follow
type ocOutcome int

const (
	ocIdle    ocOutcome = iota // the session finished the exchange
	ocWaiting                  // a permission waits for a person
	ocOver                     // the run reached a limit of its budget and finished
	ocLost                     // the server or the event stream ended
)

// drive implements driver.
func (o *opencode) drive(ctx context.Context, r *run) error {
	m, err := r.modelOf(ctx)
	if err != nil {
		return r.fail(ctx, err.Error())
	}
	for tries := 0; ; tries++ {
		srv, err := o.server(ctx, r, m)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return r.fail(ctx, err.Error())
		}
		s, fresh, err := o.session(ctx, r, srv, m)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return r.fail(ctx, err.Error())
		}
		out, err := o.exchange(ctx, r, s, fresh)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				o.abort(s)
				o.drop(r.hr.ID())
			}
			return err
		case out == ocLost:
			o.drop(r.hr.ID())
			if tries >= 2 {
				return r.fail(ctx, "harness: opencode stopped, or sent no event for "+o.h.quiet.String()+", three times during the run")
			}
			o.h.log.Warn("opencode stopped during a run; it starts again", "run", r.hr.ID())
			continue
		case out == ocOver:
			o.abort(s)
			o.drop(r.hr.ID())
		case out == ocIdle:
			o.drop(r.hr.ID())
		}
		return nil
	}
}

// exchange delivers what the session did not get yet, follows the session
// until it is idle or waits, and finishes the run.
func (o *opencode) exchange(ctx context.Context, r *run, s *ocSession, fresh bool) (ocOutcome, error) {
	for {
		st := r.rescan()
		if st.pending != nil {
			return ocWaiting, nil
		}
		answered, restart := false, false
		// A decision that goes in the note of a restart is marked as
		// answered once the prompt with the note reached the session, so a
		// stop of the process before then carries it again.
		var noted []decision
		for _, d := range st.decided {
			p, live := s.perms[d.req.ApprovalID]
			if live {
				ok, why := r.recheck(ctx, d)
				if err := o.reply(ctx, r, s, p, ok, why); err != nil {
					o.h.log.Warn("opencode lost a permission; the session continues with a note", "run", r.hr.ID(), "error", err)
					live = false
				}
				delete(s.perms, d.req.ApprovalID)
			}
			if !live {
				if !slices.ContainsFunc(s.carried, func(c decision) bool { return c.req.ApprovalID == d.req.ApprovalID }) {
					s.carried = append(s.carried, d)
				}
				noted = append(noted, d)
				restart = true
				continue
			}
			if err := r.mark(ctx, EventData{Kind: KindAnswer, Approval: d.req.ApprovalID}); err != nil {
				return 0, err
			}
			answered = true
		}
		st = r.rescan()
		text, through := st.prompt()
		if restart || (fresh && st.openExchange()) {
			if err := s.rec.interrupt(ctx, "not finished: the harness stopped before the call ended"); err != nil {
				return o.over(err)
			}
			text = joinText(restartNote, carriedText(s.carried), text)
			through = max(through, st.prompted)
		}
		fresh = false
		switch {
		case text != "":
			if notes, recall := o.h.memoryTools(r.hr.Queued()); (notes || recall) && s.srv.mem == "" {
				o.h.noServer(s.srv.memWhy)
			}
			// The index of the notes goes to the first prompt of the
			// session alone, as data, never to the system text.
			text, briefed, err := r.brief(ctx, st, text, s.memServed())
			if err != nil {
				return 0, err
			}
			if err := o.prompt(ctx, r, s, text); err != nil {
				return 0, err
			}
			for _, d := range noted {
				if err := r.mark(ctx, EventData{Kind: KindAnswer, Approval: d.req.ApprovalID}); err != nil {
					return 0, err
				}
			}
			if err := r.mark(ctx, EventData{Kind: KindPrompt, Seq: through, Context: briefed}); err != nil {
				return 0, err
			}
		case answered, st.openExchange():
			// The session goes on after the answer, or it works.
		case through > st.prompted:
			// The new messages hold no text: the session gets nothing.
			if err := r.skip(ctx, through); err != nil {
				return 0, err
			}
			continue
		case !st.terminal:
			// The exchange ended, but the run did not record its end.
			if done, err := r.finish(ctx, st); err != nil || done {
				return ocIdle, err
			}
			continue
		default:
			return ocIdle, nil
		}
		out, err := o.follow(ctx, r, s)
		if err != nil || out != ocIdle {
			return out, err
		}
		if err := s.rec.end(ctx); err != nil {
			return o.over(err)
		}
		if err := s.rec.files.record(ctx, r); err != nil {
			return 0, err
		}
		if err := r.mark(ctx, EventData{Kind: KindIdle}); err != nil {
			return 0, err
		}
		st = r.rescan()
		if s.failure != "" && !s.rec.open {
			msg := "opencode: " + s.failure
			s.failure = ""
			if len(st.unseen) == 0 {
				return ocIdle, r.fail(ctx, msg)
			}
		}
		if done, err := r.finish(ctx, st); err != nil || done {
			return ocIdle, err
		}
	}
}

// over turns the stop at a limit of the budget into its outcome.
func (o *opencode) over(err error) (ocOutcome, error) {
	if errors.Is(err, errOver) {
		return ocOver, nil
	}
	return 0, err
}

// prompt sends a message to the session and returns at once.
func (o *opencode) prompt(ctx context.Context, r *run, s *ocSession, text string) error {
	pid, _, _, _ := ocProvider(s.model)
	body := map[string]any{
		"model": map[string]string{"providerID": pid, "modelID": s.model.name},
		"agent": "build",
		"parts": []map[string]string{{"type": "text", "text": text}},
	}
	if sys := r.systemText(s.memServed()); sys != "" {
		body["system"] = sys
	}
	s.busy, s.failure = false, ""
	_, err := s.srv.call(ctx, http.MethodPost, "/session/"+url.PathEscape(s.id)+"/prompt_async", url.Values{"directory": {s.dir}}, body, nil)
	return err
}

// memServed reports whether the memory server of fw serves the session.
func (s *ocSession) memServed() bool { return s.srv.mem != "" && s.memWhy == "" }

// reply answers a permission of opencode. An allowed call of the memory
// server is armed first, with its canonical input, so the memory server
// makes it for the run r. A call that cannot be armed gets a reject with
// the reason, at once.
func (o *opencode) reply(ctx context.Context, r *run, s *ocSession, p ocPerm, allow bool, why string) error {
	var drop func()
	if t := memToolOf(KindOpencode, p.name); allow && t != "" {
		b := o.h.binding(s.srv.mem)
		switch {
		case b == nil:
			allow, why = false, "harness: the memory server of fw is not available in this session"
		case s.memWhy != "":
			allow, why = false, "harness: the memory server of fw does not serve this session: "+s.memWhy
		case p.input == nil:
			// The policy decided without the input of the call, so the host
			// cannot arm it. The harness makes it again.
			allow, why = false, memUnseen
		default:
			canon, err := memCanon(t, p.input)
			if err == nil {
				drop, err = b.arm(&memArm{tool: t, input: canon, callID: p.callID, run: r.memRun()})
			}
			if err != nil {
				allow, why = false, "harness: "+strings.TrimPrefix(err.Error(), "harness: ")
			}
		}
	}
	err := o.answer(ctx, s, p, allow, why)
	if err != nil && drop != nil {
		drop()
	}
	return err
}

// memUnseen is the reason of a reject of a memory call whose input did not
// reach the host before its permission.
const memUnseen = "harness: the input of this memory call did not reach fw before its permission; make the call again"

// answer sends the answer of a permission to opencode.
func (o *opencode) answer(ctx context.Context, s *ocSession, p ocPerm, allow bool, why string) error {
	q := url.Values{"directory": {s.dir}}
	answer := "once"
	if !allow {
		answer = "reject"
	}
	body := map[string]string{"reply": answer}
	if !allow && why != "" {
		body["message"] = why
	}
	code, err := s.srv.call(ctx, http.MethodPost, "/permission/"+url.PathEscape(p.id)+"/reply", q, body, nil)
	if err != nil && (code == http.StatusNotFound || code == http.StatusMethodNotAllowed) {
		// An older server answers on the route of the session.
		_, err = s.srv.call(ctx, http.MethodPost, "/session/"+url.PathEscape(p.session)+"/permissions/"+url.PathEscape(p.id), q, map[string]string{"response": answer}, nil)
	}
	return err
}

// abort stops the work of the session.
func (o *opencode) abort(s *ocSession) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.srv.call(ctx, http.MethodPost, "/session/"+url.PathEscape(s.id)+"/abort", url.Values{"directory": {s.dir}}, nil, nil); err != nil {
		o.h.log.Warn("abort the opencode session", "session", s.id, "error", err)
	}
}

// follow maps the events of the session onto the run until the session is
// idle, a permission waits, the run reaches its budget, or the stream ends.
// A tick checks the budget, such as the deadline, also while no event
// comes, and a stream without any event for the time quiet of the host,
// a heartbeat included, counts as lost.
func (o *opencode) follow(ctx context.Context, r *run, s *ocSession) (ocOutcome, error) {
	tick := time.NewTicker(o.h.tick)
	defer tick.Stop()
	for {
		e, err := s.queue.next(ctx, tick.C)
		if errors.Is(err, errTick) {
			if err := s.rec.check(ctx); err != nil {
				return o.over(err)
			}
			if d := s.queue.silence(); d > o.h.quiet {
				o.h.log.Warn("opencode sent no event; the session counts as lost", "run", r.hr.ID(), "silence", d.Round(time.Second))
				o.abort(s)
				return ocLost, nil
			}
			continue
		}
		if err != nil {
			if errors.Is(err, errLost) {
				return ocLost, nil
			}
			return 0, err
		}
		out, done, err := o.handle(ctx, r, s, e)
		if err != nil {
			return o.over(err)
		}
		if done {
			return out, nil
		}
	}
}

// handle maps one event. It reports true when follow ends.
func (o *opencode) handle(ctx context.Context, r *run, s *ocSession, e ocEvent) (ocOutcome, bool, error) {
	var p struct {
		SessionID string          `json:"sessionID"`
		Info      json.RawMessage `json:"info"`
		Part      json.RawMessage `json:"part"`
		Status    struct {
			Type string `json:"type"`
		} `json:"status"`
		Error json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal(e.Properties, &p)
	switch e.Type {
	case "session.created", "session.updated":
		var info struct {
			ID       string `json:"id"`
			ParentID string `json:"parentID"`
		}
		_ = json.Unmarshal(p.Info, &info)
		if info.ParentID != "" && s.family[info.ParentID] && !s.family[info.ID] {
			s.family[info.ID] = true
			return 0, false, r.raw(ctx, e.Type, map[string]string{"session": info.ID, "parent": info.ParentID})
		}
		return 0, false, nil
	case "message.updated":
		if !s.family[p.SessionID] {
			return 0, false, nil
		}
		var info struct {
			ID     string `json:"id"`
			Role   string `json:"role"`
			Finish string `json:"finish"`
			Error  *struct {
				Name string `json:"name"`
				Data struct {
					Message string `json:"message"`
				} `json:"data"`
			} `json:"error"`
		}
		_ = json.Unmarshal(p.Info, &info)
		if info.Role != "assistant" {
			return 0, false, nil
		}
		if p.SessionID == s.id {
			s.assistants[info.ID] = true
			s.busy = true
		}
		if info.Error != nil && p.SessionID == s.id {
			s.failure = strings.TrimSpace(info.Error.Name + ": " + info.Error.Data.Message)
			return 0, false, r.raw(ctx, e.Type, map[string]string{"message": info.ID, "error": info.Error.Name})
		}
		return 0, false, nil
	case "message.part.updated":
		if !s.family[p.SessionID] {
			return 0, false, nil
		}
		return 0, false, o.part(ctx, r, s, p.SessionID, p.Part)
	case "permission.asked", "permission.updated":
		var ask struct {
			ID         string          `json:"id"`
			SessionID  string          `json:"sessionID"`
			Permission string          `json:"permission"`
			Type       string          `json:"type"`
			Patterns   []string        `json:"patterns"`
			Metadata   json.RawMessage `json:"metadata"`
			Tool       *struct {
				MessageID string `json:"messageID"`
				CallID    string `json:"callID"`
			} `json:"tool"`
			CallID string `json:"callID"`
		}
		_ = json.Unmarshal(e.Properties, &ask)
		if !s.family[ask.SessionID] {
			return 0, false, nil
		}
		if ask.Permission == "" {
			ask.Permission = ask.Type
		}
		if ask.Tool != nil {
			ask.CallID = ask.Tool.CallID
		}
		return o.ask(ctx, r, s, ask.ID, ask.SessionID, ask.Permission, ask.CallID, ask.Patterns, ask.Metadata)
	case "permission.replied":
		return 0, false, nil
	case "session.status":
		if p.SessionID != s.id {
			return 0, false, nil
		}
		switch p.Status.Type {
		case "busy":
			s.busy = true
		case "idle":
			if s.busy || s.failure != "" {
				return ocIdle, true, nil
			}
		case "retry":
			return 0, false, r.raw(ctx, e.Type, json.RawMessage(e.Properties))
		}
		return 0, false, nil
	case "session.idle":
		if p.SessionID == s.id && (s.busy || s.failure != "") {
			return ocIdle, true, nil
		}
		return 0, false, nil
	case "session.error":
		if p.SessionID != "" && p.SessionID != s.id {
			return 0, false, nil
		}
		var er struct {
			Name string `json:"name"`
			Data struct {
				Message string `json:"message"`
			} `json:"data"`
		}
		_ = json.Unmarshal(p.Error, &er)
		s.failure = strings.TrimSpace(er.Name + ": " + er.Data.Message)
		return 0, false, r.raw(ctx, e.Type, map[string]string{"error": er.Name, "message": er.Data.Message})
	}
	if p.SessionID != "" && s.family[p.SessionID] {
		return 0, false, r.raw(ctx, e.Type, map[string]string{"session": p.SessionID})
	}
	return 0, false, nil
}

// part maps one part of a message.
func (o *opencode) part(ctx context.Context, r *run, s *ocSession, session string, raw json.RawMessage) error {
	var pt struct {
		ID        string `json:"id"`
		MessageID string `json:"messageID"`
		Type      string `json:"type"`
		Text      string `json:"text"`
		CallID    string `json:"callID"`
		Tool      string `json:"tool"`
		State     struct {
			Status string          `json:"status"`
			Input  json.RawMessage `json:"input"`
			Output string          `json:"output"`
			Error  string          `json:"error"`
		} `json:"state"`
		Reason string  `json:"reason"`
		Cost   float64 `json:"cost"`
		Tokens struct {
			Input     int `json:"input"`
			Output    int `json:"output"`
			Reasoning int `json:"reasoning"`
			Cache     struct {
				Read  int `json:"read"`
				Write int `json:"write"`
			} `json:"cache"`
		} `json:"tokens"`
		Time *struct {
			End *int64 `json:"end"`
		} `json:"time"`
	}
	if json.Unmarshal(raw, &pt) != nil {
		return nil
	}
	if session != s.id {
		// A subagent works in a session of its own. Its model calls count
		// in the budget of the run; its turns are not turns of the run.
		if pt.Type == "step-finish" {
			if err := s.rec.usage(ctx, fw.Usage{InputTokens: pt.Tokens.Input, OutputTokens: pt.Tokens.Output + pt.Tokens.Reasoning,
				CacheReadTokens: pt.Tokens.Cache.Read, CacheWriteTokens: pt.Tokens.Cache.Write}); err != nil {
				return err
			}
			return r.raw(ctx, "message.part.updated", map[string]any{"session": session, "type": pt.Type, "reason": pt.Reason, "cost": pt.Cost})
		}
		if pt.Type == "tool" && pt.CallID != "" {
			s.tools[pt.CallID] = &ocTool{tool: strings.ToLower(pt.Tool), name: pt.Tool, input: pt.State.Input, state: pt.State.Status}
		}
		return nil
	}
	if !s.assistants[pt.MessageID] {
		return nil
	}
	rec := s.rec
	switch pt.Type {
	case "step-start":
		if err := rec.begin(ctx); err != nil {
			return err
		}
		s.step = &ocStep{texts: map[string]string{}}
		return r.raw(ctx, "message.part.updated", map[string]string{"type": pt.Type, "message": pt.MessageID})
	case "text":
		st, err := o.stepOf(ctx, s)
		if err != nil {
			return err
		}
		if _, ok := st.texts[pt.ID]; !ok {
			st.order = append(st.order, pt.ID)
		}
		st.texts[pt.ID] = pt.Text
		return nil
	case "tool":
		st, err := o.stepOf(ctx, s)
		if err != nil {
			return err
		}
		t := s.tools[pt.CallID]
		if t == nil {
			t = &ocTool{}
			s.tools[pt.CallID] = t
			st.order = append(st.order, "tool:"+pt.CallID)
		}
		t.tool, t.name = strings.ToLower(pt.Tool), pt.Tool
		if len(pt.State.Input) > 0 && string(pt.State.Input) != "null" {
			t.input = pt.State.Input
		}
		changed := t.state != pt.State.Status
		t.state = pt.State.Status
		switch t.state {
		case "completed":
			t.result = pt.State.Output
		case "error":
			t.result = pt.State.Error
		}
		if changed {
			if err := r.raw(ctx, "message.part.updated", map[string]string{"type": pt.Type, "callID": pt.CallID, "tool": t.tool, "status": t.state}); err != nil {
				return err
			}
		}
		switch pt.State.Status {
		case "completed", "error":
			if !rec.flushed {
				st.pending = append(st.pending, pt.CallID)
				return nil
			}
			return o.toolDone(ctx, s, pt.CallID)
		}
		return nil
	case "step-finish":
		st, err := o.stepOf(ctx, s)
		if err != nil {
			return err
		}
		u := fw.Usage{InputTokens: pt.Tokens.Input, OutputTokens: pt.Tokens.Output + pt.Tokens.Reasoning,
			CacheReadTokens: pt.Tokens.Cache.Read, CacheWriteTokens: pt.Tokens.Cache.Write}
		if err := r.raw(ctx, "message.part.updated", map[string]any{"type": pt.Type, "reason": pt.Reason, "cost": pt.Cost,
			"tokens": map[string]int{"input": pt.Tokens.Input, "output": pt.Tokens.Output, "reasoning": pt.Tokens.Reasoning, "cacheRead": pt.Tokens.Cache.Read, "cacheWrite": pt.Tokens.Cache.Write}}); err != nil {
			return err
		}
		if !rec.flushed {
			if err := rec.respond(ctx, o.content(s, st), u, stopOf(pt.Reason)); err != nil {
				return err
			}
		} else if err := rec.usage(ctx, u); err != nil {
			return err
		}
		for _, id := range st.pending {
			if err := o.toolDone(ctx, s, id); err != nil {
				return err
			}
		}
		s.step = nil
		return rec.end(ctx)
	}
	return nil
}

// stepOf returns the model call that runs, and opens one for a part that
// came without a step-start.
func (o *opencode) stepOf(ctx context.Context, s *ocSession) (*ocStep, error) {
	if s.step == nil {
		s.step = &ocStep{texts: map[string]string{}}
		if !s.rec.open {
			if err := s.rec.begin(ctx); err != nil {
				return nil, err
			}
		}
	}
	return s.step, nil
}

// content returns the response of the model call so far: its text and its
// tool calls.
func (o *opencode) content(s *ocSession, st *ocStep) []fw.Block {
	var out []fw.Block
	for _, key := range st.order {
		if id, ok := strings.CutPrefix(key, "tool:"); ok {
			t := s.tools[id]
			input := t.input
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			out = append(out, fw.Block{Type: fw.BlockToolUse, ID: id, Name: t.tool, Input: input})
			continue
		}
		if t := st.texts[key]; strings.TrimSpace(t) != "" {
			out = append(out, fw.Text(t))
		}
	}
	return out
}

// toolDone records a tool call that ended and its result.
func (o *opencode) toolDone(ctx context.Context, s *ocSession, id string) error {
	t := s.tools[id]
	if t == nil || t.done {
		return nil
	}
	if err := s.rec.request(ctx, id, t.tool, t.input); err != nil {
		return err
	}
	t.done = true
	return s.rec.result(ctx, id, t.tool, t.result, t.state == "error")
}

// ask answers a permission of opencode from the policy of the run.
func (o *opencode) ask(ctx context.Context, r *run, s *ocSession, id, session, permission, callID string, patterns []string, meta json.RawMessage) (ocOutcome, bool, error) {
	tool, name := strings.ToLower(permission), permission
	var input, seen json.RawMessage
	if t := s.tools[callID]; t != nil {
		if tool != "external_directory" && tool != "doom_loop" {
			tool, name = t.tool, t.name
		}
		input = t.input
	}
	if len(input) == 0 {
		input, _ = json.Marshal(map[string]any{"patterns": patterns, "metadata": meta})
	} else {
		seen = input
	}
	if callID == "" {
		callID = id
	}
	perm := ocPerm{id: id, session: session, callID: callID, tool: tool, name: name, input: seen}
	if err := r.raw(ctx, "permission.asked", map[string]string{"id": id, "permission": permission, "callID": callID}); err != nil {
		return 0, false, err
	}
	if session == s.id {
		st, err := o.stepOf(ctx, s)
		if err != nil {
			return 0, false, err
		}
		if !s.rec.flushed {
			// The call waits for a decision before it runs, so the
			// response and the call go to the ledger first. The usage of
			// the model call follows when the call ends.
			if err := s.rec.respond(ctx, o.content(s, st), fw.Usage{}, fw.StopToolUse); err != nil {
				return 0, false, err
			}
		}
		if t := s.tools[callID]; t != nil {
			if err := s.rec.request(ctx, callID, t.tool, t.input); err != nil {
				return 0, false, err
			}
		}
	}
	mem := memToolOf(KindOpencode, name)
	if mem != "" && seen == nil {
		// The policy cannot decide a memory call on the patterns of the
		// permission, and the host never arms a call without its input.
		return 0, false, o.reply(ctx, r, s, perm, false, memUnseen)
	}
	for i, d := range s.carried {
		if d.req.Tool == tool && (mem == "" && sameJSON(d.req.Input, input) || mem != "" && memSame(mem, d.req.Input, input)) {
			s.carried = append(s.carried[:i], s.carried[i+1:]...)
			ok, why := r.recheck(ctx, d)
			return 0, false, o.reply(ctx, r, s, perm, ok, why)
		}
	}
	v := r.decide(ctx, callID, name, input)
	switch v.Verdict {
	case fw.VerdictAllow:
		return 0, false, o.reply(ctx, r, s, perm, true, "")
	case fw.VerdictDeny:
		return 0, false, o.reply(ctx, r, s, perm, false, "denied by policy: "+v.Reason)
	}
	approval, err := r.approval(ctx, callID, tool, input, v.Reason)
	if err != nil {
		return 0, false, err
	}
	s.perms[approval] = perm
	return ocWaiting, true, nil
}

// stopOf maps the finish reason of a step onto a stop reason.
func stopOf(reason string) fw.StopReason {
	switch reason {
	case "tool-calls":
		return fw.StopToolUse
	case "length":
		return fw.StopMaxTokens
	}
	return fw.StopEndTurn
}

// carriedText tells the harness the decisions that it could not get,
// because it stopped while it waited.
func carriedText(ds []decision) string {
	var lines []string
	for _, d := range ds {
		call := d.req.Tool + " " + oneLine(string(d.req.Input), 400)
		if d.dec.Decision == "allow" {
			lines = append(lines, "A person allowed the call "+call+". Make the call again if you still need it.")
		} else {
			lines = append(lines, "The call "+call+" was "+deniedBy(d.dec)+". Do not make it again.")
		}
	}
	return strings.Join(lines, "\n")
}

// joinText joins the parts that are not empty with blank lines.
func joinText(parts ...string) string {
	var out []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n\n")
}

// oneLine returns s on one line, cut to n bytes.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}

// sameJSON reports whether two JSON values are equal.
func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return bytes.Equal(a, b)
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return bytes.Equal(xb, yb)
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// freePort returns a free port on the loopback address.
func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("harness: find a free port: %w", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// absRoot returns the absolute root of the workspace.
func absRoot(h *host) (string, error) {
	root, err := filepath.Abs(h.a.Config().WorkspaceRoot())
	if err != nil {
		return "", fmt.Errorf("harness: the root of the workspace: %w", err)
	}
	return root, nil
}
