package fw

// Tests here prove that MergeRules and NewRulesPolicy compute the exact
// conjunction of independent policies: for every pair of rules sides and
// every call, the verdict of the merged rules is exactly the stricter of
// the two verdicts, never looser and never stricter, and nesting keeps that
// property. A later check covers the JSON shape of All.

import (
	"encoding/json"
	"testing"
)

// conjunctionCalls holds calls that cover tool names, flags, tool globs,
// and argument rules with different fields.
func conjunctionCalls() []ToolCall {
	return []ToolCall{
		{Spec: ToolSpec{Name: "files"}},
		{Spec: ToolSpec{Name: "files"}, Input: json.RawMessage(`{"path":"docs/a.md"}`)},
		{Spec: ToolSpec{Name: "files"}, Input: json.RawMessage(`{"path":"secrets/k.env"}`)},
		{Spec: ToolSpec{Name: "files"}, Input: json.RawMessage(`{"path":"docs/../secrets/k.env"}`)},
		{Spec: ToolSpec{Name: "bash", Destructive: true}},
		{Spec: ToolSpec{Name: "bash", Destructive: true}, Input: json.RawMessage(`{"command":"git push origin"}`)},
		{Spec: ToolSpec{Name: "bash", Destructive: true}, Input: json.RawMessage(`{"command":"ls"}`)},
		{Spec: ToolSpec{Name: "http", ReadOnly: true, ParallelSafe: true}, Input: json.RawMessage(`{"url":"https://ci.example.com/job"}`)},
		{Spec: ToolSpec{Name: "git.fetch", ReadOnly: true}},
		{Spec: ToolSpec{Name: "send"}},
		{Spec: ToolSpec{Name: "clock", ReadOnly: true, Idempotent: true, ParallelSafe: true}},
		{Spec: ToolSpec{Name: "env"}, Input: json.RawMessage(`{"user":"ci","token":"t"}`)},
	}
}

// conjunctionRulesSides holds sides for the conjunction table: every
// default verdict, empty rules, flag rules, tool globs of different widths,
// and argument rules of different fields.
func conjunctionRulesSides() []struct {
	name string
	r    Rules
} {
	return []struct {
		name string
		r    Rules
	}{
		{"empty", Rules{}},
		{"allowDefault", Rules{Default: "allow"}},
		{"askDefault", Rules{Default: "ask"}},
		{"denyDefault", Rules{Default: "deny"}},
		{"upperDeny", Rules{Default: "DENY"}},
		{"allowDocs", Rules{Default: "deny", Allow: []string{"files: path=docs/*", "http", "clock"}}},
		{"allowGitGlob", Rules{Default: "deny", Allow: []string{"git.*"}}},
		{"allowGitName", Rules{Default: "deny", Allow: []string{"git.fetch"}}},
		{"askPath", Rules{Allow: []string{"files: path=docs/*", "clock"}, Ask: []string{"files: path=*"}}},
		{"denyURL", Rules{Ask: []string{"http"}, Deny: []string{"http: url=https://ci.example.com/*"}}},
		{"denyEnvField", Rules{Deny: []string{"env: token=T*"}}},
		{"denyPush", Rules{Default: "allow", Ask: []string{"destructive"}, Deny: []string{"bash: git push*"}}},
		{"wrappedDeny", Rules{All: []Rules{{Default: "deny", Allow: []string{"clock"}, Deny: []string{"send"}}}}},
	}
}

// TestMergeRulesIsTheExactConjunction proves that for every pair of sides
// and every call, the merged verdict is exactly the stricter verdict of the
// two sides, and that the flattened form keeps the property.
func TestMergeRulesIsTheExactConjunction(t *testing.T) {
	sides := conjunctionRulesSides()
	calls := conjunctionCalls()
	for _, sa := range sides {
		for _, sb := range sides {
			m := MergeRules(sa.r, sb.r)
			merged := [2]Rules{m, MergeRules(m, Rules{})}
			for _, call := range calls {
				da := decideRules(t, sa.r, call)
				db := decideRules(t, sb.r, call)
				want := da.Verdict
				if verdictRank(db.Verdict) > verdictRank(da.Verdict) {
					want = db.Verdict
				}
				for i, mr := range merged {
					if d := decideRules(t, mr, call); d.Verdict != want {
						t.Fatalf("%s and %s, merged form %d: call %+v gives %s, want the stricter of %s and %s, %s",
							sa.name, sb.name, i, call, d.Verdict, da.Verdict, db.Verdict, want)
					}
				}
			}
		}
	}
}

// TestNestingKeepsTheExactConjunction proves the property for composed
// rules of three sides in both association orders, with nested All.
func TestNestingKeepsTheExactConjunction(t *testing.T) {
	sides := conjunctionRulesSides()
	calls := conjunctionCalls()
	pick := []int{0, 3, 5, 9, 11} // empty, denyDefault, allowDocs, denyURL, denyPush
	for _, i := range pick {
		for _, j := range pick {
			for _, k := range pick {
				a, b, c := sides[i].r, sides[j].r, sides[k].r
				nested := Rules{All: []Rules{MergeRules(a, b), c}} // one more level
				m1 := MergeRules(MergeRules(a, b), c)
				m2 := MergeRules(a, MergeRules(b, c))
				for _, call := range calls {
					want := verdictRank(decideRules(t, a, call).Verdict)
					if db := verdictRank(decideRules(t, b, call).Verdict); db > want {
						want = db
					}
					if dc := verdictRank(decideRules(t, c, call).Verdict); dc > want {
						want = dc
					}
					for _, r := range []Rules{m1, m2, nested} {
						if d := decideRules(t, r, call); verdictRank(d.Verdict) != want {
							t.Fatalf("merge of %s, %s, %s: call %+v gives %s, want the stricter of the three", sides[i].name, sides[j].name, sides[k].name, call, d.Verdict)
						}
					}
				}
			}
		}
	}
}

// TestConjunctionOfAskAndDenyDefaultDenies proves the reported case: an ask
// in one side holds, and a deny default of the other side still denies, so
// the merged rules deny exactly once for the call.
func TestConjunctionOfAskAndDenyDefaultDenies(t *testing.T) {
	a := Rules{Ask: []string{"bash"}}
	b := Rules{Default: "deny"}
	m := MergeRules(a, b)
	call := ToolCall{Spec: ToolSpec{Name: "bash", Destructive: true}}
	d := decideRules(t, m, call)
	if d.Verdict != VerdictDeny || d.Reason == "" {
		t.Fatalf("merged on bash = %s (%s), want deny with a reason", d.Verdict, d.Reason)
	}
	// The empty rules side cannot loosen the ask of the other side.
	if d := decideRules(t, MergeRules(a, Rules{}), call); d.Verdict != VerdictAsk {
		t.Fatalf("merged with empty rules = %s, want ask", d.Verdict)
	}
}

// TestRulesJSONShapeAll checks the JSON shape of the All field, so the run
// queue and the transport keep it unchanged.
func TestRulesJSONShapeAll(t *testing.T) {
	if got, _ := json.Marshal(Rules{}); string(got) != "{}" {
		t.Fatalf("empty rules marshal to %s", got)
	}
	r := Rules{All: []Rules{{Default: "deny"}, {Ask: []string{"http"}, All: []Rules{{Allow: []string{"*"}}}}}}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var back Rules
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.All) != 2 || back.Default != "" || back.Allow != nil ||
		back.All[1].Default != "" || back.All[1].All[0].Allow[0] != "*" {
		t.Fatalf("round trip = %+v from %s", back, b)
	}
}
