package mcp

// This file is the MCP transport of the external decision point
// (docs/ARCHITECTURE.md section 13.5). policy.external, or the control
// plane of the workbench, names an entry of tools.mcp and a tool on it. fw
// calls that tool with the decision request as its arguments and reads the
// answer from the result.
// The model never sees the tools of that server (see newSources).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func init() {
	fw.RegisterDecisionPoint("mcp", newDecisionPoint)
}

// ErrNoDecision means that the decision tool answered without a decision:
// an error result, a server that is down, or a result with no JSON object.
var ErrNoDecision = errors.New("mcp: no decision")

// maxQuote bounds the result text that an error quotes.
const maxQuote = 500

// decisionPoint calls one tool on one MCP server for each decision. It
// implements fw.DecisionPoint, fw.Prober, and io.Closer.
type decisionPoint struct {
	s    *Server
	tool string // the plain name of the tool on the server
}

var (
	_ fw.DecisionPoint = (*decisionPoint)(nil)
	_ fw.Prober        = (*decisionPoint)(nil)
)

// newDecisionPoint implements fw.DecisionPointCtor. It finds the tools.mcp
// entry that ext names and opens a connection of its own, so the decision
// calls never share the circuit breaker or the tool list of a tool source.
func newDecisionPoint(ctx context.Context, a *fw.Agent, ext fw.ExternalPolicyConfig) (fw.DecisionPoint, error) {
	cfg := a.Config()
	var entry *fw.MCPServerConfig
	names := make([]string, 0, len(cfg.Tools.MCP))
	for i := range cfg.Tools.MCP {
		names = append(names, cfg.Tools.MCP[i].Name)
		if fw.SameServerName(cfg.Tools.MCP[i].Name, ext.MCP) {
			entry = &cfg.Tools.MCP[i]
		}
	}
	if entry == nil {
		have := "tools.mcp has no entries"
		if len(names) > 0 {
			sort.Strings(names)
			have = "the entries are " + strings.Join(names, ", ")
		}
		return nil, fmt.Errorf("mcp: the external decision point names the server %q, but tools.mcp has no entry with that name; %s", ext.MCP, have)
	}
	if entry.Auth.OAuth.GrantOf() == fw.GrantUser {
		return nil, fmt.Errorf("mcp: the external decision point names the server %q, which acts for the person who started a run; a decision point acts as the agent itself, so give it auth.oauth.grant client", entry.Name)
	}
	c := *entry
	// One decision has the time limit of the decision point. A shorter limit
	// of the entry wins.
	if c.Timeout <= 0 || c.Timeout > ext.Timeout {
		c.Timeout = ext.Timeout
	}
	s, err := New(ctx, c, a.Secrets(), a.Logger().With("component", "mcp", "use", "decisions"))
	if err != nil {
		return nil, err
	}
	s.useSPIFFESocket(cfg)
	return &decisionPoint{s: s, tool: ext.Tool}, nil
}

// Decide implements fw.DecisionPoint. The answer is the structured content
// of the result, or else the first text block that holds a JSON object.
func (d *decisionPoint) Decide(ctx context.Context, request json.RawMessage) (json.RawMessage, error) {
	// No caller can answer a question from the control plane. An empty
	// asker hides the asker of a surrounding run, so an elicitation fails
	// at once instead of waiting for a person.
	ctx = fw.WithAsker(ctx, nil)
	name := d.s.cfg.Name
	res, err := d.s.Call(ctx, d.tool, request)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			// The time limit of the decision ended the call before the
			// timer of the server did, so the failure counts here.
			d.s.failed("tools/call", err)
		}
		return nil, fmt.Errorf("mcp: server %s: decision tool %s: %w", name, d.tool, err)
	}
	text := strings.TrimSpace(fw.TextOf(res.Content))
	if res.IsError {
		return nil, fmt.Errorf("%w: server %s: the tool %s failed: %s", ErrNoDecision, name, d.tool, quote(text))
	}
	if s := bytes.TrimSpace(res.Structured); len(s) > 0 && s[0] == '{' && json.Valid(s) {
		return json.RawMessage(s), nil
	}
	for _, b := range res.Content {
		if b.Type != fw.BlockText {
			continue
		}
		if t := strings.TrimSpace(b.Text); strings.HasPrefix(t, "{") && json.Valid([]byte(t)) {
			return json.RawMessage(t), nil
		}
	}
	return nil, fmt.Errorf("%w: server %s: the tool %s returned no JSON object: %s", ErrNoDecision, name, d.tool, quote(text))
}

// Probe implements fw.Prober. It lists the tools of the server and makes
// sure that the decision tool is one of them.
func (d *decisionPoint) Probe(ctx context.Context) error {
	name := d.s.cfg.Name
	tools, err := d.s.Tools(ctx)
	if err != nil {
		return fmt.Errorf("mcp: probe server %s: %w", name, err)
	}
	have := make([]string, 0, len(tools))
	for _, t := range tools {
		plain := strings.TrimPrefix(t.Spec().Name, name+".")
		if plain == d.tool {
			return nil
		}
		have = append(have, plain)
	}
	if len(have) == 0 {
		return fmt.Errorf("mcp: server %s has no tool %q, and it lists no tools", name, d.tool)
	}
	return fmt.Errorf("mcp: server %s has no tool %q; it has %s", name, d.tool, strings.Join(have, ", "))
}

// Close implements io.Closer.
func (d *decisionPoint) Close() error { return d.s.Close() }

// quote returns the text of a result for an error, cut to maxQuote bytes.
func quote(text string) string {
	if text == "" {
		return "(no text)"
	}
	if cut, ok := cutText(text, maxQuote); ok {
		return fmt.Sprintf("%q (cut)", cut)
	}
	return fmt.Sprintf("%q", text)
}

// cutText cuts s to at most n bytes on a rune boundary.
func cutText(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n], true
}
