// Package a2a turns the A2A peers under tools.peers in fw.yaml into tools.
// Each peer becomes one tool named "ask_<peer>". See docs/ARCHITECTURE.md
// sections 7.4 and 11.4.
//
// A peer with card is resolved from that agent card URL. A peer with ax,
// "<atespace>/<task>", is an fw Task in the same ax cluster: every request
// goes to the atenet router at Config.AX.Router and carries the header
// "ate-target-actor: <atespace>/<task>". The card resolves on the first
// call and stays cached.
package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/bdubs00/featherweight-kernel/fw"
)

const (
	// HeaderTargetActor routes a request through the atenet router.
	HeaderTargetActor = "ate-target-actor"

	callTimeout    = 10 * time.Minute // limit of one tool call
	resolveTimeout = 30 * time.Second // limit of the card request
	pollEvery      = time.Second      // GetTask interval when the peer does not stream
)

const inputSchema = `{
  "type": "object",
  "properties": {
    "message": {"type": "string", "description": "The message to the peer agent."},
    "continue": {"type": "string", "description": "The task id from an earlier answer of this peer. Set it to answer a question of the peer or to follow up on that task."}
  },
  "required": ["message"],
  "additionalProperties": false
}`

func init() {
	fw.RegisterToolSource("a2a", newSources)
}

func newSources(_ context.Context, cfg *fw.Config, _ fw.Secrets) ([]fw.ToolSource, error) {
	peers, err := Peers(cfg)
	if err != nil || len(peers) == 0 {
		return nil, err
	}
	return []fw.ToolSource{&source{peers: peers}}, nil
}

// source serves the peer tools. Close releases idle connections.
type source struct{ peers []*Peer }

func (s *source) Tools(context.Context) ([]fw.Tool, error) {
	out := make([]fw.Tool, len(s.peers))
	for i, p := range s.peers {
		out[i] = p
	}
	return out, nil
}

func (s *source) Close() error {
	for _, p := range s.peers {
		p.Close()
	}
	return nil
}

// Peers builds one tool per entry under cfg.Tools.Peers. A configuration
// error fails the whole set. No peer is contacted until its first call.
func Peers(cfg *fw.Config) ([]*Peer, error) {
	seen := map[string]bool{}
	out := make([]*Peer, 0, len(cfg.Tools.Peers))
	for i, pc := range cfg.Tools.Peers {
		p, err := NewPeer(pc, cfg.AX.Router)
		if err != nil {
			return nil, fmt.Errorf("a2a: tools.peers[%d]: %w", i, err)
		}
		if seen[p.spec.Name] {
			return nil, fmt.Errorf("a2a: tools.peers[%d]: the tool name %s is used twice", i, p.spec.Name)
		}
		seen[p.spec.Name] = true
		out = append(out, p)
	}
	return out, nil
}

// Peer is the tool "ask_<name>" of one A2A peer. It is safe for concurrent
// use.
type Peer struct {
	cfg       fw.PeerConfig
	spec      fw.ToolSpec
	cardURL   string   // the card URL, or the router base URL for an ax peer
	router    *url.URL // the router base for an ax peer, else nil
	http      *http.Client
	timeout   time.Duration
	pollEvery time.Duration

	mu     sync.Mutex
	client *Client
	card   *sdk.AgentCard
}

// NewPeer validates a peer entry and returns its tool. router is the host of
// the atenet router, used when the entry sets ax.
func NewPeer(pc fw.PeerConfig, router string) (*Peer, error) {
	if pc.Name == "" {
		return nil, errors.New("name is required")
	}
	if (pc.Card == "") == (pc.AX == "") {
		return nil, fmt.Errorf("peer %q: set exactly one of card and ax", pc.Name)
	}
	p := &Peer{cfg: pc, timeout: callTimeout, pollEvery: pollEvery}
	var where string
	if pc.AX != "" {
		space, task, ok := strings.Cut(pc.AX, "/")
		if !ok || space == "" || task == "" || strings.Contains(task, "/") {
			return nil, fmt.Errorf("peer %q: ax is %q, not <atespace>/<task>", pc.Name, pc.AX)
		}
		if router == "" {
			return nil, fmt.Errorf("peer %q: ax.router is empty", pc.Name)
		}
		base, err := url.Parse("http://" + router + "/")
		if err != nil || base.Host == "" {
			return nil, fmt.Errorf("peer %q: ax.router %q is not a host", pc.Name, router)
		}
		p.router, p.cardURL = base, base.String()
		p.http = &http.Client{Transport: &headerTransport{base: newTransport(), key: HeaderTargetActor, value: pc.AX}}
		where = fmt.Sprintf("the ax Task %s", pc.AX)
	} else {
		u, err := url.Parse(pc.Card)
		if err != nil || u.Scheme == "" || (u.Host == "" && u.Scheme != "file") {
			return nil, fmt.Errorf("peer %q: card %q is not an absolute URL", pc.Name, pc.Card)
		}
		p.cardURL = pc.Card
		p.http = &http.Client{Transport: newTransport()}
		where = "the agent card at " + pc.Card
	}
	p.spec = fw.ToolSpec{
		Name: "ask_" + toolName(pc.Name),
		Description: fmt.Sprintf("Ask the peer agent %q (%s) by A2A. The tool sends the message and waits up to 10 minutes "+
			"for the final answer. The result ends with the line \"task_id: <id>\". When the peer asks a question, or to "+
			"follow up on the same task, call this tool again with continue set to that id.", pc.Name, where),
		InputSchema:  json.RawMessage(inputSchema),
		Destructive:  true,
		ParallelSafe: true,
		Source:       pc.Name,
	}
	return p, nil
}

// toolName keeps the characters that a model provider accepts in a tool name.
func toolName(s string) string {
	var sb strings.Builder
	for _, c := range s {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
		if !ok {
			c = '_'
		}
		sb.WriteRune(c)
	}
	return sb.String()
}

// Spec implements fw.Tool.
func (p *Peer) Spec() fw.ToolSpec { return p.spec }

// Close releases idle connections.
func (p *Peer) Close() { p.http.CloseIdleConnections() }

// newTransport returns a transport of its own for one peer, so that Close
// drops only the idle connections of that peer. The client has no overall
// timeout: the call context limits each request, and a stream lasts as long
// as the task.
func newTransport() http.RoundTripper {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		return t.Clone()
	}
	return http.DefaultTransport
}

// headerTransport adds one header to every request.
type headerTransport struct {
	base       http.RoundTripper
	key, value string
}

func (t *headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(t.key, t.value)
	return t.base.RoundTrip(r)
}

// CloseIdleConnections lets http.Client.CloseIdleConnections reach the base.
func (t *headerTransport) CloseIdleConnections() {
	if c, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// connect resolves the card on first use and caches the client. A failure is
// not cached, so the next call tries again.
func (p *Peer) connect(ctx context.Context) (*Client, *sdk.AgentCard, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil {
		return p.client, p.card, nil
	}
	rctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	card, err := agentcard.NewResolver(p.http).Resolve(rctx, p.cardURL)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve the agent card: %w", err)
	}
	if p.router != nil {
		card = p.throughRouter(card)
	}
	endpoint, err := JSONRPCURL(card)
	if err != nil {
		return nil, nil, err
	}
	p.client, p.card = &Client{URL: endpoint, HTTP: p.http}, card
	return p.client, p.card, nil
}

// throughRouter points every interface of the card at the router. The card
// of a Task names addresses that only the Task itself can reach.
func (p *Peer) throughRouter(card *sdk.AgentCard) *sdk.AgentCard {
	c := *card
	c.SupportedInterfaces = make([]*sdk.AgentInterface, 0, len(card.SupportedInterfaces))
	for _, i := range card.SupportedInterfaces {
		if i == nil {
			continue
		}
		cp := *i
		if u, err := url.Parse(i.URL); err == nil {
			u.Scheme, u.Host, u.User = p.router.Scheme, p.router.Host, nil
			cp.URL = u.String()
		}
		c.SupportedInterfaces = append(c.SupportedInterfaces, &cp)
	}
	return &c
}

// Call implements fw.Tool. It sends the message, follows the stream when
// the card offers streaming, else polls GetTask, and returns the final text.
func (p *Peer) Call(ctx context.Context, input json.RawMessage) (fw.ToolResult, error) {
	var in struct {
		Message  string `json:"message"`
		Continue string `json:"continue"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return fw.ErrorResult(fmt.Sprintf("%s: invalid input: %v", p.spec.Name, err)), nil
	}
	if strings.TrimSpace(in.Message) == "" {
		return fw.ErrorResult(p.spec.Name + ": message is required"), nil
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	c, card, err := p.connect(ctx)
	if err != nil {
		return fw.ErrorResult(fmt.Sprintf("%s: %v", p.spec.Name, err)), nil
	}
	msg := sdk.NewMessage(sdk.MessageRoleUser, sdk.NewTextPart(in.Message))
	if id := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(in.Continue), "task_id:")); id != "" {
		msg.TaskID = sdk.TaskID(id)
	}
	req := &sdk.SendMessageRequest{Message: msg}
	o := &outcome{}
	n := 0
	o.report = func(text string) {
		n++
		fw.ReportProgress(ctx, float64(n), 0, text)
	}
	if card.Capabilities.Streaming {
		err = p.stream(ctx, c, req, o)
	} else {
		var ev sdk.Event
		if ev, err = c.SendMessage(ctx, req); err == nil {
			o.apply(ev)
		}
	}
	// A stream that breaks, or a task that is not final yet, goes on by
	// polling, as long as the task id is known.
	if !o.done() && o.taskID != "" && ctx.Err() == nil {
		err = p.poll(ctx, c, o)
	}
	if err == nil && !o.done() {
		err = errors.New("the peer stopped before the task reached a final state")
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil {
			err = fmt.Errorf("no final answer within %s", p.timeout)
		}
		return fw.ErrorResult(withTaskID(fmt.Sprintf("%s: %v", p.spec.Name, err), o.taskID)), nil
	}
	return o.result(), nil
}

func (p *Peer) stream(ctx context.Context, c *Client, req *sdk.SendMessageRequest, o *outcome) error {
	for ev, err := range c.SendStreamingMessage(ctx, req) {
		if err != nil {
			return err
		}
		o.apply(ev)
		if o.done() {
			return nil
		}
	}
	return nil
}

func (p *Peer) poll(ctx context.Context, c *Client, o *outcome) error {
	t := time.NewTicker(p.pollEvery)
	defer t.Stop()
	for !o.done() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
		task, err := c.GetTask(ctx, &sdk.GetTaskRequest{ID: sdk.TaskID(o.taskID)})
		if err != nil {
			return err
		}
		o.apply(task)
	}
	return nil
}

// outcome folds the events of one call.
type outcome struct {
	taskID  string
	state   sdk.TaskState
	status  string // text of the latest status message
	reply   string // text of a message reply
	replied bool
	arts    []*sdk.Artifact
	report  func(text string)
}

func (o *outcome) apply(ev sdk.Event) {
	switch v := ev.(type) {
	case *sdk.Message:
		o.reply, o.replied = partsText(v.Parts), true
		if v.TaskID != "" {
			o.taskID = string(v.TaskID)
		}
	case *sdk.Task:
		o.taskID, o.state = string(v.ID), v.Status.State
		o.setStatus(v.Status.Message)
		if len(v.Artifacts) > 0 {
			o.arts = nil
			for _, a := range v.Artifacts {
				o.addArtifact(a, false)
			}
		}
	case *sdk.TaskStatusUpdateEvent:
		o.taskID, o.state = string(v.TaskID), v.Status.State
		o.setStatus(v.Status.Message)
	case *sdk.TaskArtifactUpdateEvent:
		o.taskID = string(v.TaskID)
		o.addArtifact(v.Artifact, v.Append)
	}
}

// setStatus keeps the text of a status message and reports a new one as
// progress of the tool call.
func (o *outcome) setStatus(m *sdk.Message) {
	if m == nil {
		return
	}
	text := partsText(m.Parts)
	if text == "" || text == o.status {
		return
	}
	o.status = text
	if o.report != nil {
		o.report(text)
	}
}

func (o *outcome) addArtifact(a *sdk.Artifact, appendParts bool) {
	if a == nil {
		return
	}
	for i, old := range o.arts {
		if old.ID != a.ID {
			continue
		}
		cp := *a
		if appendParts {
			cp.Parts = append(append(sdk.ContentParts(nil), old.Parts...), a.Parts...)
		}
		o.arts[i] = &cp
		return
	}
	cp := *a
	o.arts = append(o.arts, &cp)
}

func (o *outcome) done() bool {
	return o.replied || o.state.Terminal() || o.state == sdk.TaskStateInputRequired || o.state == sdk.TaskStateAuthRequired
}

func (o *outcome) result() fw.ToolResult {
	var text string
	isErr := false
	status := func(head string) string {
		if o.status == "" {
			return head + "."
		}
		return head + ":\n" + o.status
	}
	switch {
	case o.replied:
		text = o.reply
	case o.state == sdk.TaskStateCompleted:
		var parts []string
		for _, a := range o.arts {
			if t := partsText(a.Parts); t != "" {
				parts = append(parts, t)
			}
		}
		text = strings.Join(parts, "\n\n")
		if text == "" {
			text = o.status
		}
		if text == "" {
			text = "The peer finished with no text."
		}
	case o.state == sdk.TaskStateInputRequired:
		text = status("The peer needs input")
	case o.state == sdk.TaskStateAuthRequired:
		text = status("The peer needs authentication")
	case o.state == sdk.TaskStateFailed:
		text, isErr = status("The peer task failed"), true
	case o.state == sdk.TaskStateCanceled:
		text, isErr = status("The peer task was canceled"), true
	case o.state == sdk.TaskStateRejected:
		text, isErr = status("The peer rejected the task"), true
	default:
		text = status("The peer task is " + string(o.state))
	}
	return fw.ToolResult{Content: []fw.Block{fw.Text(withTaskID(text, o.taskID))}, IsError: isErr}
}

// withTaskID puts the task id on the last line, so the model can continue.
func withTaskID(text, id string) string {
	if id == "" {
		return text
	}
	return text + "\n\ntask_id: " + id
}

func partsText(parts sdk.ContentParts) string {
	var out []string
	for _, p := range parts {
		if p == nil {
			continue
		}
		if t, ok := p.Content.(sdk.Text); ok && t != "" {
			out = append(out, string(t))
		}
	}
	return strings.Join(out, "\n")
}
