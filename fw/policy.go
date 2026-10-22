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
// A rule that matches the field "command", in the command form or as
// "command=", matches the whole text of the command. A deny or an ask rule
// also matches each simple command of a compound command, such as the
// "git push origin main" of "cd /work && git push origin main", and the
// other forms that commandForms gives. A command that passes a bound of
// the split matches every such deny and ask rule. An allow rule matches
// the whole text only, so one part of a compound command never allows
// the other parts.
//
// When the tool resolves its input (see Resolver), a field rule also sees
// the resolved value, such as the real path of a file relative to the root
// of the files tool. A deny or an ask rule matches the value of the model
// or the resolved value. An allow rule matches the resolved value only, so
// "docs/../secrets/a" does not pass a rule that allows "docs/*".
//
// Deny wins over Ask, and Ask wins over Allow. Default applies when no rule
// matches; it is "allow" when empty. Empty Rules allow every call.
//
// All holds independent policy clauses. Each clause decides the call by
// itself, with its own lists and its own nested clauses. The effective
// verdict of the rules is the stricter of the verdict that the own fields
// give the call and of the verdict of every clause: a call runs only when
// the own fields and every clause allow it, and one clause that denies or
// asks holds. Empty All adds nothing.
type Rules struct {
	Default string   `yaml:"default,omitempty" json:"default,omitempty"`
	Allow   []string `yaml:"allow,omitempty" json:"allow,omitempty"`
	Ask     []string `yaml:"ask,omitempty" json:"ask,omitempty"`
	Deny    []string `yaml:"deny,omitempty" json:"deny,omitempty"`
	// All holds the independent clauses of a conjunction, see the type doc.
	All []Rules `yaml:"all,omitempty" json:"all,omitempty"`
}

// DefaultRules allow read-only tools, ask before destructive tools, and deny nothing.
func DefaultRules() Rules {
	return Rules{Default: "allow", Ask: []string{"destructive"}}
}

// MergeRules returns the rules of the conjunction of a and b: for every
// tool call, the verdict of the merged rules is exactly the stricter of the
// verdict that a gives the call and the verdict that b gives the call.
//
// The merged rules keep the two sides as independent clauses in All, each
// with its own lists and defaults. No list intersection or default
// summarization happens: globs overlap and differ, and an own default and
// an ask in the same clause keep their exact meaning. An own Default of
// "allow" in a clause stays allow; it can never widen the conjunction,
// because the merged verdict is the stricter one.
//
// A side that holds no own fields, only clauses, contributes its clauses
// instead of a wrapper, so a chain of merges does not nest. When a and b
// hold policies of independent authors, such as the base policy of the
// configuration and the policy of a profile, a parent and a child, the
// merged rules preserve their conjunction.
func MergeRules(a, b Rules) Rules {
	return Rules{All: append(rulesClauses(a), rulesClauses(b)...)}
}

// rulesClauses returns the independent clauses of the conjunction of r. A
// wrapper that holds no own fields reduces to its clauses, nested ones
// included, so a chain of merges stays shallow. A side with own fields
// stays one clause; its own fields keep their exact meaning inside it.
func rulesClauses(r Rules) []Rules {
	if r.Default == "" && len(r.Allow) == 0 && len(r.Ask) == 0 && len(r.Deny) == 0 {
		var out []Rules
		for _, c := range r.All {
			out = append(out, rulesClauses(c)...)
		}
		return out
	}
	return []Rules{r}
}

// NewRulesPolicy builds a Policy from Rules. The policy of the own fields
// decides first, and every clause of All must agree: the effective verdict
// is the stricter of the own verdict and of the verdict of every clause,
// and clauses nest recursively. The reason names the clause that decided.
func NewRulesPolicy(r Rules) Policy {
	own := PolicyFunc(func(_ context.Context, call ToolCall) Decision {
		m := newCallMatcher(call)
		for _, rule := range r.Deny {
			if m.matches(rule, false) {
				return Decision{Verdict: VerdictDeny, Reason: "rule: deny " + rule}
			}
		}
		for _, rule := range r.Ask {
			if m.matches(rule, false) {
				return Decision{Verdict: VerdictAsk, Reason: "rule: ask " + rule}
			}
		}
		for _, rule := range r.Allow {
			if m.matches(rule, true) {
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
	if len(r.All) == 0 {
		return own
	}
	compilers := make([]Policy, 0, len(r.All))
	for _, c := range r.All {
		compilers = append(compilers, NewRulesPolicy(c))
	}
	return PolicyFunc(func(ctx context.Context, call ToolCall) Decision {
		d := own.Decide(ctx, call)
		for _, c := range compilers {
			if dc := c.Decide(ctx, call); verdictRank(dc.Verdict) > verdictRank(d.Verdict) {
				d = dc
			}
		}
		return d
	})
}

// ruleMatches reports whether a rule matches a call. allow selects the
// field rule of an allow list, which matches only the resolved value of a
// field when the tool resolves it.
func ruleMatches(rule string, call ToolCall, allow bool) bool {
	return newCallMatcher(call).matches(rule, allow)
}

// callMatcher matches the rules of a policy against one tool call. It
// decodes the input once, and it splits a command once for all the rules.
type callMatcher struct {
	call  ToolCall
	in    map[string]json.RawMessage
	res   map[string]json.RawMessage
	resOK bool
	split map[string]commandSplit
}

// commandSplit is the result of commandForms for one text.
type commandSplit struct {
	forms    []string
	complete bool
}

func newCallMatcher(call ToolCall) *callMatcher {
	m := &callMatcher{call: call}
	_ = json.Unmarshal(call.Input, &m.in)
	m.resOK = json.Unmarshal(call.Resolved, &m.res) == nil
	return m
}

// matches reports whether a rule matches the call. allow selects the rule
// of an allow list: its field rule matches only the resolved value of a
// field when the tool resolves it, and its command rule matches only the
// whole text of the command.
func (m *callMatcher) matches(rule string, allow bool) bool {
	rule = strings.TrimSpace(rule)
	if tool, rest, ok := strings.Cut(rule, ":"); ok {
		if !globMatch(strings.TrimSpace(tool), m.call.Spec.Name) {
			return false
		}
		rest = strings.TrimSpace(rest)
		if field, pattern, ok := strings.Cut(rest, "="); ok && isFieldName(field) {
			if raw, present := m.in[field]; present {
				pattern = strings.TrimSpace(pattern)
				if m.resOK {
					if r, known := m.res[field]; known {
						if allow {
							return m.valueMatches(field, pattern, stringOf(r), allow)
						}
						return m.valueMatches(field, pattern, stringOf(raw), allow) || m.valueMatches(field, pattern, stringOf(r), allow)
					}
				}
				return m.valueMatches(field, pattern, stringOf(raw), allow)
			}
		}
		return m.valueMatches("command", rest, stringOf(m.in["command"]), allow)
	}
	switch strings.ToLower(rule) {
	case "readonly", "read-only":
		return m.call.Spec.ReadOnly
	case "idempotent":
		return m.call.Spec.Idempotent
	case "destructive":
		return m.call.Spec.Destructive
	case "parallel", "parallelsafe":
		return m.call.Spec.ParallelSafe
	}
	return globMatch(rule, m.call.Spec.Name)
}

// valueMatches matches a pattern against the value of a field of the
// input. A deny or an ask rule on the field "command" also matches each
// form of the command (commandForms), and every form of a command that
// passes a bound of the split.
func (m *callMatcher) valueMatches(field, pattern, value string, allow bool) bool {
	if wildcardMatch(pattern, value) {
		return true
	}
	if allow || field != "command" || value == "" {
		return false
	}
	sp, ok := m.split[value]
	if !ok {
		sp.forms, sp.complete = commandForms(value)
		if m.split == nil {
			m.split = map[string]commandSplit{}
		}
		m.split[value] = sp
	}
	if !sp.complete {
		return true
	}
	for _, f := range sp.forms {
		if wildcardMatch(pattern, f) {
			return true
		}
	}
	return false
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
