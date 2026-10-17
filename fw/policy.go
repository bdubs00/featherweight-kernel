package fw

import (
	"context"
	"encoding/json"
	"path"
	"strings"
)

// Verdict of a policy.
type Verdict int

const (
	VerdictAllow Verdict = iota
	VerdictDeny
	VerdictAsk
)

// String implements fmt.Stringer.
func (v Verdict) String() string {
	switch v {
	case VerdictAllow:
		return "allow"
	case VerdictDeny:
		return "deny"
	case VerdictAsk:
		return "ask"
	}
	return "unknown"
}

// Decision is a verdict with a reason.
type Decision struct {
	Verdict Verdict
	Reason  string
}

// Policy decides whether a tool call runs.
type Policy interface {
	Decide(ctx context.Context, call ToolCall) Decision
}

// PolicyFunc adapts a function to Policy.
type PolicyFunc func(ctx context.Context, call ToolCall) Decision

// Decide implements Policy.
func (f PolicyFunc) Decide(ctx context.Context, call ToolCall) Decision { return f(ctx, call) }

// Rules is the declarative policy from configuration.
//
// Each entry in Allow, Ask, and Deny is one of:
//   - a flag: "readonly", "idempotent", "destructive", "parallel"
//   - a tool name glob: "git.*", "bash"
//   - a tool name and a command glob: "bash: git push*", matched against the
//     "command" field of the input
//   - a tool name, a field, and a glob: "files: path=*.env", matched against
//     that string field of the input. The field form applies when the input
//     holds that field; otherwise the text matches the command, so
//     "bash: FOO=bar*" still matches a command.
//
// When the tool resolves its input (see Resolver), a field rule also sees
// the resolved value, such as the real path of a file relative to the root
// of the files tool. A deny or an ask rule matches the value of the model
// or the resolved value. An allow rule matches the resolved value only, so
// "docs/../secrets/a" does not pass a rule that allows "docs/*".
//
// Deny wins over Ask, and Ask wins over Allow. Default applies when no rule
// matches; it is "allow" when empty.
type Rules struct {
	Default string   `yaml:"default,omitempty" json:"default,omitempty"`
	Allow   []string `yaml:"allow,omitempty" json:"allow,omitempty"`
	Ask     []string `yaml:"ask,omitempty" json:"ask,omitempty"`
	Deny    []string `yaml:"deny,omitempty" json:"deny,omitempty"`
}

// DefaultRules allow read-only tools, ask before destructive tools, and deny nothing.
func DefaultRules() Rules {
	return Rules{Default: "allow", Ask: []string{"destructive"}}
}

// MergeRules returns the rules that are at least as strict as a and as b:
// the union of the deny lists, the union of the ask lists, the allow list of
// a followed by the allow list of b, and the stricter default.
func MergeRules(a, b Rules) Rules {
	out := Rules{
		Allow: appendUnique(append([]string(nil), a.Allow...), b.Allow...),
		Ask:   appendUnique(append([]string(nil), a.Ask...), b.Ask...),
		Deny:  appendUnique(append([]string(nil), a.Deny...), b.Deny...),
	}
	da, db := defaultVerdict(a.Default), defaultVerdict(b.Default)
	if verdictRank(db) > verdictRank(da) {
		out.Default = strings.ToLower(b.Default)
	} else {
		out.Default = strings.ToLower(a.Default)
	}
	return out
}

func appendUnique(dst []string, more ...string) []string {
	for _, m := range more {
		found := false
		for _, d := range dst {
			if d == m {
				found = true
				break
			}
		}
		if !found {
			dst = append(dst, m)
		}
	}
	return dst
}

func defaultVerdict(s string) Verdict {
	switch strings.ToLower(s) {
	case "deny":
		return VerdictDeny
	case "ask":
		return VerdictAsk
	}
	return VerdictAllow
}

// NewRulesPolicy builds a Policy from Rules.
func NewRulesPolicy(r Rules) Policy {
	return PolicyFunc(func(_ context.Context, call ToolCall) Decision {
		for _, rule := range r.Deny {
			if ruleMatches(rule, call, false) {
				return Decision{Verdict: VerdictDeny, Reason: "rule: deny " + rule}
			}
		}
		for _, rule := range r.Ask {
			if ruleMatches(rule, call, false) {
				return Decision{Verdict: VerdictAsk, Reason: "rule: ask " + rule}
			}
		}
		for _, rule := range r.Allow {
			if ruleMatches(rule, call, true) {
				return Decision{Verdict: VerdictAllow, Reason: "rule: allow " + rule}
			}
		}
		switch strings.ToLower(r.Default) {
		case "deny":
			return Decision{Verdict: VerdictDeny, Reason: "default"}
		case "ask":
			return Decision{Verdict: VerdictAsk, Reason: "default"}
		}
		return Decision{Verdict: VerdictAllow, Reason: "default"}
	})
}

// ruleMatches reports whether a rule matches a call. allow selects the
// field rule of an allow list, which matches only the resolved value of a
// field when the tool resolves it.
func ruleMatches(rule string, call ToolCall, allow bool) bool {
	rule = strings.TrimSpace(rule)
	if tool, rest, ok := strings.Cut(rule, ":"); ok {
		if !globMatch(strings.TrimSpace(tool), call.Spec.Name) {
			return false
		}
		rest = strings.TrimSpace(rest)
		var in map[string]json.RawMessage
		_ = json.Unmarshal(call.Input, &in)
		if field, pattern, ok := strings.Cut(rest, "="); ok && isFieldName(field) {
			if raw, present := in[field]; present {
				pattern = strings.TrimSpace(pattern)
				var res map[string]json.RawMessage
				if json.Unmarshal(call.Resolved, &res) == nil {
					if r, known := res[field]; known {
						if allow {
							return wildcardMatch(pattern, stringOf(r))
						}
						return wildcardMatch(pattern, stringOf(raw)) || wildcardMatch(pattern, stringOf(r))
					}
				}
				return wildcardMatch(pattern, stringOf(raw))
			}
		}
		return wildcardMatch(rest, stringOf(in["command"]))
	}
	switch strings.ToLower(rule) {
	case "readonly", "read-only":
		return call.Spec.ReadOnly
	case "idempotent":
		return call.Spec.Idempotent
	case "destructive":
		return call.Spec.Destructive
	case "parallel", "parallelsafe":
		return call.Spec.ParallelSafe
	}
	return globMatch(rule, call.Spec.Name)
}

// isFieldName reports whether s is a JSON field name of the simple kind:
// letters, digits, and underscores, and a letter or an underscore first.
func isFieldName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// stringOf returns a JSON string as text, and any other value as its JSON.
func stringOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// wildcardMatch matches a value against a pattern in which "*" matches any
// text, slashes included, and "?" matches one character. Values such as a
// command, a path, or a URL hold slashes, so the path rules of globMatch do
// not fit them.
func wildcardMatch(pattern, s string) bool {
	if pattern == "*" {
		return true
	}
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return matchChars(pattern, s)
	}
	// The first part anchors the start, the last part anchors the end, and
	// the parts between appear in order.
	first, last := parts[0], parts[len(parts)-1]
	if len(s) < len(first)+len(last) || !matchChars(first, s[:len(first)]) || !matchChars(last, s[len(s)-len(last):]) {
		return false
	}
	rest := s[len(first) : len(s)-len(last)]
	for _, part := range parts[1 : len(parts)-1] {
		if part == "" {
			continue
		}
		found := -1
		for i := 0; i+len(part) <= len(rest); i++ {
			if matchChars(part, rest[i:i+len(part)]) {
				found = i
				break
			}
		}
		if found < 0 {
			return false
		}
		rest = rest[found+len(part):]
	}
	return true
}

// matchChars compares a pattern without "*" to a text of the same length,
// with "?" matching any one byte.
func matchChars(pattern, s string) bool {
	if len(pattern) != len(s) {
		return false
	}
	for i := 0; i < len(pattern); i++ {
		if pattern[i] != '?' && pattern[i] != s[i] {
			return false
		}
	}
	return true
}

func globMatch(pattern, s string) bool {
	if pattern == "*" {
		return true
	}
	ok, err := path.Match(pattern, s)
	if err != nil {
		return pattern == s
	}
	if ok {
		return true
	}
	// "git push*" style patterns with spaces: path.Match handles them, but a
	// trailing "*" must also match the bare prefix.
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(s, strings.TrimSuffix(pattern, "*"))
	}
	return false
}
