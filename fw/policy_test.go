package fw

// Tests here prove how Rules merge: the merged rules hold the exact
// conjunction of the two sides, so two policies that come from independent
// authors, a configuration and a profile, a parent and a child, give every
// call exactly the stricter of the two verdicts. The merged rules carry the
// two sides as independent clauses in All, so no list surgery can change a
// verdict.

import (
	"context"
	"encoding/json"
	"testing"
)

func decideRules(t *testing.T, r Rules, call ToolCall) Decision {
	t.Helper()
	return NewRulesPolicy(r).Decide(context.Background(), call)
}

// rankOf orders verdicts the way policyFor does.
func rankOf(d Decision) int { return verdictRank(d.Verdict) }

func files(input string) ToolCall {
	return ToolCall{Spec: ToolSpec{Name: "files"}, Input: json.RawMessage(input)}
}

func TestMergeRulesNeverWidensADefaultDeny(t *testing.T) {
	// Two parents with disjoint default-deny allowlists. Either side alone
	// allows its own calls, and the conjunction allows none of them, not
	// even a call that one side allows.
	a := Rules{Default: "deny", Allow: []string{"files: path=docs/*"}}
	b := Rules{Default: "deny", Allow: []string{"files: path=tmp/*", "http"}}
	if d := decideRules(t, a, files(`{"path":"docs/a.md"}`)); d.Verdict != VerdictAllow {
		t.Fatalf("a alone on docs = %s", d.Verdict)
	}
	if d := decideRules(t, b, files(`{"path":"tmp/x"}`)); d.Verdict != VerdictAllow {
		t.Fatalf("b alone on tmp = %s", d.Verdict)
	}
	m := MergeRules(a, b)
	for _, in := range []string{`{"path":"docs/a.md"}`, `{"path":"tmp/x"}`, `{}`} {
		if d := decideRules(t, m, files(in)); d.Verdict != VerdictDeny {
			t.Fatalf("merged policy on files %s = %s, want deny", in, d.Verdict)
		}
	}
}

func TestMergeRulesKeepsTheSharedRules(t *testing.T) {
	// The merged rules hold both sides as clauses. A call that both sides
	// allow keeps its verdict, and the ask and deny lists of either side
	// hold through the own clause of that side.
	a := Rules{Default: "deny", Allow: []string{"files: path=docs/*"}, Ask: []string{"http"}, Deny: []string{"send"}}
	b := Rules{Default: "deny", Allow: []string{"files: path=docs/*", "git.*"}}
	m := MergeRules(a, b)
	want := []struct {
		call    ToolCall
		a, b    Verdict
		verdict Verdict
	}{
		{files(`{"path":"docs/a.md"}`), VerdictAllow, VerdictAllow, VerdictAllow},
		{files(`{"path":"src/x.md"}`), VerdictDeny, VerdictDeny, VerdictDeny},
		{ToolCall{Spec: ToolSpec{Name: "send"}}, VerdictDeny, VerdictDeny, VerdictDeny},
		{ToolCall{Spec: ToolSpec{Name: "http"}}, VerdictAsk, VerdictDeny, VerdictDeny},
		{ToolCall{Spec: ToolSpec{Name: "git.fetch"}}, VerdictDeny, VerdictAllow, VerdictDeny},
	}
	for _, w := range want {
		if d := decideRules(t, a, w.call); d.Verdict != w.a {
			t.Fatalf("a on %+v = %s, want %s", w.call, d.Verdict, w.a)
		}
		if d := decideRules(t, b, w.call); d.Verdict != w.b {
			t.Fatalf("b on %+v = %s, want %s", w.call, d.Verdict, w.b)
		}
		if d := decideRules(t, m, w.call); d.Verdict != w.verdict {
			t.Fatalf("merged on %+v = %s, want %s", w.call, d.Verdict, w.verdict)
		}
	}
}

// TestMergeRulesIsNeverLooser proves the invariant for a mix of defaults:
// for every call, the verdict of the merged rules is at least as strict as
// the stricter verdict of the two sides.
func TestMergeRulesIsNeverLooser(t *testing.T) {
	a := Rules{Default: "ask", Allow: []string{"files: path=docs/*"}, Ask: []string{"send"}}
	b := Rules{Default: "allow", Deny: []string{"files: path=secrets/*"}, Ask: []string{"bash"}}
	m := MergeRules(a, b)
	calls := []ToolCall{
		files(`{"path":"docs/a.md"}`),
		files(`{"path":"secrets/s"}`),
		files(`{"path":"src"}`),
		{Spec: ToolSpec{Name: "send"}},
		{Spec: ToolSpec{Name: "bash", Destructive: true}, Input: json.RawMessage(`{"command":"ls"}`)},
		{Spec: ToolSpec{Name: "clock", ReadOnly: true}},
	}
	for _, call := range calls {
		d1 := decideRules(t, a, call)
		d2 := decideRules(t, b, call)
		d := decideRules(t, m, call)
		strictest := d1.Verdict
		if rankOf(d2) > rankOf(d1) {
			strictest = d2.Verdict
		}
		if rankOf(d) < rankOf(Decision{Verdict: strictest}) {
			t.Fatalf("merged verdict %s is looser than the stricter verdict %s of the two sides for %+v", d.Verdict, strictest, call)
		}
	}
}

func TestMergeRulesWithAllowDefaultsKeepsTheVerdicts(t *testing.T) {
	// When both defaults are allow, every call that either side allows
	// keeps an allow verdict, and a call that a side asks stays ask.
	a := Rules{Default: "allow", Allow: []string{"files"}}
	b := Rules{Default: "allow", Allow: []string{"bash", "files"}}
	m := MergeRules(a, b)
	for _, name := range []string{"files", "bash", "clock"} {
		call := ToolCall{Spec: ToolSpec{Name: name}}
		d := decideRules(t, m, call)
		da, db := decideRules(t, a, call), decideRules(t, b, call)
		strictest := da.Verdict
		if rankOf(db) > rankOf(da) {
			strictest = db.Verdict
		}
		if d.Verdict != strictest {
			t.Fatalf("merged %s = %s, want %s (a %s, b %s)", name, d.Verdict, strictest, da.Verdict, db.Verdict)
		}
	}
}

func join(rules []string) string {
	out := ""
	for i, r := range rules {
		if i > 0 {
			out += ","
		}
		out += r
	}
	return out
}
