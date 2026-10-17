package view

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Ability is one thing that an agent can do, in plain words. Verdict is
// "allow" when the agent does it on its own, "ask" when it asks first,
// "deny" when the policy blocks it, "external" when a control plane
// decides each call, and "none" when the profile leaves the tool out.
// Notes are the pattern rules that change the verdict of some calls, such
// as `Never runs "git push*"`.
type Ability struct {
	Tool    string   `json:"tool"`
	Text    string   `json:"text"`
	Verdict string   `json:"verdict"`
	Notes   []string `json:"notes,omitempty"`
}

// Abilities words what a profile can do, in the order of the tools of the
// agent. A built-in tool that the profile leaves out appears with the
// verdict "none", so a person sees what the agent cannot do. An opt-in
// tool or any other tool that the profile leaves out does not appear.
func Abilities(ctx context.Context, a *fw.Agent, p fw.AgentProfile) ([]Ability, error) {
	grants, err := a.ProfileGrants(ctx, p)
	if err != nil {
		return nil, err
	}
	out := make([]Ability, 0, len(grants))
	for _, g := range grants {
		builtin := g.Spec.Source == "" || g.Spec.Source == "builtin"
		// await_task only waits for the work that spawn_task handed off.
		if g.Spec.Name == "await_task" && builtin {
			continue
		}
		if g.Verdict == fw.GrantNone && (!builtin || g.Spec.OptIn) {
			continue
		}
		ab := Ability{Tool: g.Spec.Name, Text: toolPhrase(g.Spec), Verdict: g.Verdict}
		for _, r := range g.Patterns {
			if noteMatters(r.Verdict, g.Verdict) {
				ab.Notes = append(ab.Notes, patternNote(r))
			}
		}
		out = append(out, ab)
	}
	return out, nil
}

// toolPhrase is the title of the tool, else the first sentence of its
// description, else its name.
func toolPhrase(spec fw.ToolSpec) string {
	if spec.Title != "" {
		return spec.Title
	}
	d := strings.TrimSpace(spec.Description)
	if i := strings.IndexAny(d, ".\n"); i >= 0 {
		d = d[:i]
	}
	if d == "" {
		return spec.Name
	}
	if utf8.RuneCountInString(d) > 80 {
		d = string([]rune(d)[:79]) + "…"
	}
	return d
}

// noteMatters reports whether a pattern rule changes what a person reads:
// a stricter rule on a looser tool, or an allow rule on a tool that asks
// or is denied.
func noteMatters(rule, tool string) bool {
	rank := map[string]int{"allow": 0, "ask": 1, "deny": 2}
	if tool == fw.GrantExternal {
		return rule != "allow"
	}
	if rule == "allow" {
		return rank[tool] > 0
	}
	return rank[rule] > rank[tool]
}

// patternNote words one pattern rule.
func patternNote(r fw.PatternRule) string {
	q := `"` + r.Pattern + `"`
	switch r.Field {
	case "command":
		switch r.Verdict {
		case "deny":
			return "Never runs " + q
		case "ask":
			return "Asks before it runs " + q
		}
		return "Runs " + q + " on its own"
	case "path":
		switch r.Verdict {
		case "deny":
			return "Never touches " + q
		case "ask":
			return "Asks before it touches " + q
		}
		return "Touches " + q + " on its own"
	}
	switch r.Verdict {
	case "deny":
		return "Never when " + r.Field + " is " + q
	case "ask":
		return "Asks first when " + r.Field + " is " + q
	}
	return "On its own when " + r.Field + " is " + q
}

// AbilityText words the abilities in three sentences or fewer, for a
// surface that shows text.
func AbilityText(abs []Ability) string {
	groups := []struct {
		lead  string
		match func(string) bool
	}{
		{"Does on its own: ", func(v string) bool { return v == "allow" }},
		{"Asks you first: ", func(v string) bool { return v == "ask" }},
		{"Your control plane decides: ", func(v string) bool { return v == fw.GrantExternal }},
		{"Cannot: ", func(v string) bool { return v == "deny" || v == fw.GrantNone }},
	}
	var lines []string
	for _, g := range groups {
		var items []string
		for _, ab := range abs {
			if !g.match(ab.Verdict) {
				continue
			}
			item := lowerFirst(ab.Text)
			if len(ab.Notes) > 0 {
				notes := make([]string, len(ab.Notes))
				for i, n := range ab.Notes {
					notes[i] = lowerFirst(n)
				}
				item += " (" + strings.Join(notes, ", ") + ")"
			}
			items = append(items, item)
		}
		if len(items) > 0 {
			lines = append(lines, g.lead+strings.Join(items, "; ")+".")
		}
	}
	return strings.Join(lines, "\n")
}

// lowerFirst lowers the first letter of a phrase for the middle of a
// sentence. A first word with another capital, such as GitHub or URL,
// keeps its case.
func lowerFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError {
		return s
	}
	word, _, _ := strings.Cut(s[n:], " ")
	if strings.IndexFunc(word, unicode.IsUpper) >= 0 {
		return s
	}
	return string(unicode.ToLower(r)) + s[n:]
}
