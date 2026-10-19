package fw

// This file answers "what can this agent do?" for a profile: the tools
// that it can use, the verdict that the policy gives each one, and the
// rules with a pattern that narrow a verdict. Surfaces turn the answer
// into plain words, so a person never reads a glob to learn what an
// agent does. It is a projection of the configuration and runs nothing.

import (
	"context"
	"encoding/json"
	"strings"
)

// Verdicts of a ToolGrant that are not a Verdict of the policy.
const (
	// GrantExternal means that the control plane of policy.external
	// decides each call, so no verdict is known ahead of the call.
	GrantExternal = "external"
	// GrantNone means that the tool list of the profile leaves the tool
	// out, so the agent never sees it.
	GrantNone = "none"
)

// ToolGrant is one tool and what a profile can do with it.
//
// Verdict is "allow", "ask", or "deny" for a call with no input, the
// stricter of the policy of the agent and the rules of the profile. It is
// GrantExternal when a control plane decides, and GrantNone when the
// profile does not list the tool. Patterns lists the rules with a command
// or a field pattern that change the verdict of some calls, for example
// a deny of "bash: git push*".
type ToolGrant struct {
	Spec     ToolSpec      `json:"spec"`
	Verdict  string        `json:"verdict"`
	Patterns []PatternRule `json:"patterns,omitempty"`
}

// PatternRule is a policy rule for one tool that matches a field of the
// input. Field is "command" for the command form, as in "bash: git push*".
type PatternRule struct {
	Verdict string `json:"verdict"` // allow, ask, or deny
	Field   string `json:"field"`
	Pattern string `json:"pattern"`
}

// ProfileGrants returns a ToolGrant for every tool of the agent, in the
// order of Tools. A rule of the agent policy appears in Patterns only when
// the policy comes from policy in the configuration. A policy from a Go
// option still decides the verdict, but its rules are not known.
func (a *Agent) ProfileGrants(ctx context.Context, p AgentProfile) ([]ToolGrant, error) {
	tools, err := a.Tools(ctx)
	if err != nil {
		return nil, err
	}
	_, external := a.policy.(*externalPolicy)
	var rules []Rules
	if a.rulesPolicy {
		rules = append(rules, a.cfg.Policy.Rules)
	}
	var own Policy
	if p.Policy != nil {
		rules = append(rules, *p.Policy)
		own = NewRulesPolicy(*p.Policy)
	}
	out := make([]ToolGrant, 0, len(tools))
	for _, t := range tools {
		spec := t.Spec()
		g := ToolGrant{Spec: spec}
		if !toolAllowed(p.Tools, spec) {
			g.Verdict = GrantNone
			out = append(out, g)
			continue
		}
		// The control plane is not asked here. The rules of the profile
		// can only make its verdict stricter, so a deny of the profile
		// still holds.
		call := ToolCall{Agent: p.Name, Spec: spec, Input: json.RawMessage(`{}`)}
		d := Decision{Verdict: VerdictAllow}
		if !external {
			d = a.policy.Decide(ctx, call)
		}
		if own != nil {
			if d2 := own.Decide(ctx, call); verdictRank(d2.Verdict) > verdictRank(d.Verdict) {
				d = d2
			}
		}
		g.Verdict = d.Verdict.String()
		if external && d.Verdict != VerdictDeny {
			g.Verdict = GrantExternal
		}
		if d.Verdict != VerdictDeny {
			g.Patterns = patternRules(rules, spec.Name)
		}
		out = append(out, g)
	}
	return out, nil
}

// patternRules returns the rules with a pattern whose tool part matches the
// tool name, deny first, then ask, then allow, without duplicates.
func patternRules(rules []Rules, tool string) []PatternRule {
	var clauses []Rules
	var collect func(Rules)
	collect = func(r Rules) {
		clauses = append(clauses, r)
		for _, child := range r.All {
			collect(child)
		}
	}
	for _, r := range rules {
		collect(r)
	}
	var out []PatternRule
	seen := map[PatternRule]bool{}
	add := func(verdict string, list []string) {
		for _, rule := range list {
			name, rest, ok := strings.Cut(strings.TrimSpace(rule), ":")
			if !ok || !globMatch(strings.TrimSpace(name), tool) {
				continue
			}
			rest = strings.TrimSpace(rest)
			pr := PatternRule{Verdict: verdict, Field: "command", Pattern: rest}
			if field, pattern, ok := strings.Cut(rest, "="); ok && isFieldName(field) {
				pr.Field, pr.Pattern = field, strings.TrimSpace(pattern)
			}
			if !seen[pr] {
				seen[pr] = true
				out = append(out, pr)
			}
		}
	}
	for _, r := range clauses {
		add("deny", r.Deny)
	}
	for _, r := range clauses {
		add("ask", r.Ask)
	}
	for _, r := range clauses {
		add("allow", r.Allow)
	}
	return out
}

// extraTools names the tools that the inline profile of a request gives
// the run beyond the tool list of its named profile, in the order of Tools.
// A tool that an ancestor does not allow is left out, because the run never
// gets it. named is the profile that the caller read for the request.
func (a *Agent) extraTools(ctx context.Context, req RunRequest, named AgentProfile, parent *runState) []string {
	if req.Profile == nil || req.Profile.Tools == nil {
		return nil
	}
	run := named.Overlay(*req.Profile)
	var ancestors [][]string
	if parent != nil {
		for _, anc := range a.lineage(parent) {
			ancestors = append(ancestors, a.profileOf(anc.queuedData()).Tools)
		}
	}
	specs, err := a.ToolSpecs(ctx)
	if err != nil {
		return nil
	}
	var out []string
	for _, s := range specs {
		if !toolAllowed(run.Tools, s) || toolAllowed(named.Tools, s) {
			continue
		}
		ok := true
		for _, globs := range ancestors {
			if !toolAllowed(globs, s) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, s.Name)
		}
	}
	return out
}
