package fw

// Tests here prove how a rule with a command pattern matches a shell
// command: a deny or an ask rule matches the whole text and each simple
// command of a compound command, with quotes and backslashes respected,
// and an allow rule matches the whole text only.

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func bashCall(command string) ToolCall {
	in, _ := json.Marshal(map[string]string{"command": command})
	return ToolCall{Spec: ToolSpec{Name: "bash", Destructive: true}, Input: in}
}

// The command of the finding: a model ran "git push" after "cd" in one
// command, and the rule "bash: git push*" did not ask.
const pushAfterCd = `cd /workspace/runs/01M5H96EKV1W1TCETMNZZKDYVE && git push origin main 2>&1; echo "exit=$?"`

func TestCommandFormsOfACompoundCommand(t *testing.T) {
	forms, complete := commandForms(pushAfterCd)
	want := []string{
		"cd /workspace/runs/01M5H96EKV1W1TCETMNZZKDYVE",
		"git push origin main 2>&1",
		`echo "exit=$?"`,
		"echo exit=$?",
	}
	if !complete || !slices.Equal(forms, want) {
		t.Fatalf("forms = %q, complete %v\nwant %q", forms, complete, want)
	}
}

func TestCommandRuleMatchesEachSimpleCommand(t *testing.T) {
	cases := []struct {
		rule    string
		command string
		match   bool
	}{
		// A single command matches as before.
		{"bash: git push*", "git push origin", true},
		{"bash: git push*", "git status", false},
		{"bash: git push", "git pushy", false},
		{"bash: git push", "git push", true},
		{"bash: git ??sh*", "git push", true},
		{"bash: FOO=bar*", "FOO=bar make", true},
		{"bash: *rm -rf*", "cd /x && rm -rf ./build", true},
		{"bash: git push*", "echo git push", false},
		{"bash: git push*", "  git push", true},

		// Each separator starts a simple command.
		{"bash: git push*", pushAfterCd, true},
		{"bash: git push*", "make || git push", true},
		{"bash: git push*", "make; git push", true},
		{"bash: git push*", "make | git push", true},
		{"bash: git push*", "make |& git push", true},
		{"bash: git push*", "make\ngit push", true},
		{"bash: git push*", "make\r\n  git push", true},
		{"bash: git push*", "sleep 1 & git push", true},
		{"bash: git push", "cd x && git push ; ls", true},
		{"bash: git push origin main 2>&1", pushAfterCd, true},

		// A separator inside quotes or after a backslash does not split.
		{"bash: git push*", `echo "a && git push"`, false},
		{"bash: git push*", `echo 'a; git push'`, false},
		{"bash: git push*", `echo a\;git push`, false},
		{"bash: git push*", "echo a \\\n&& git push", true},
		{"bash: git push*", `ls 2>&1 | grep x`, false},
		{"bash: ls 2>&1", `ls 2>&1 && make`, true},
		{"bash: git push*", `ls &>/dev/null & git push`, true},
		{"bash: git push*", `echo x >| f; git push`, true},
		// A quote that does not close hides nothing.
		{"bash: git push*", `echo it's; git push`, true},
		// Quotes and doubled blanks go from a form.
		{"bash: git push*", `git 'push' origin`, true},
		{"bash: git push*", `git  push`, true},
		{"bash: git push*", "git \\\npush", true},
		{"bash: git push*", `$'git' push`, true},

		// Groups and substitutions run their commands.
		{"bash: git push*", "(cd x && git push)", true},
		{"bash: git push*", "{ cd x; git push; }", true},
		{"bash: git push*", `echo "$(git push)"`, true},
		{"bash: git push*", "echo `git push`", true},
		{"bash: git push*", `diff <(git push) f`, true},
		{"bash: git push*", `x=$(cd y && git push origin)`, true},
		{"bash: git push*", `echo '$(git push)'`, false},

		// Assignments, reserved words, and functions come before a command.
		{"bash: git push*", "GIT_SSH_COMMAND=x git push", true},
		{"bash: git push*", "if git push; then echo ok; fi", true},
		{"bash: git push*", "for r in a b; do git push $r; done", true},
		{"bash: git push*", "! git push", true},
		{"bash: git push*", "f() { git push; }; f", true},
		{"bash: git push*", "case $x in a) git push;; esac", true},

		// A runner runs the command in its arguments.
		{"bash: git push*", "sudo -u ci git push", true},
		{"bash: git push*", "/usr/bin/sudo git push", true},
		{"bash: git push*", "env -i PATH=/bin git push", true},
		{"bash: git push*", "time git push", true},
		{"bash: git push*", "nohup git push &", true},
		{"bash: git push*", "echo origin | xargs git push", true},
		{"bash: git push*", "timeout 10 git push", true},
		{"bash: git push*", `bash -c 'cd x && git push'`, true},
		{"bash: git push*", `xargs -I{} sh -c "git push {}"`, true},
		{"bash: git push*", `eval "git push"`, true},
		{"bash: git push*", `find . -name x -exec git push \;`, true},
		{"bash: git push", "sudo git push", true},
		// Each later word of a runner can start the command, so a
		// runner errs on the side of a match.
		{"bash: push", "git push", false},
		{"bash: push", "sudo git push", true},

		// A here-document: each line of the body counts, and a quote of
		// the body does not reach past its line.
		{"bash: git push*", "cat <<EOF\nit's\nEOF\ngit push && echo 'done'", true},
		{"bash: rm -rf*", "cat <<EOF\n$(rm -rf /)\nEOF", true},
		{"bash: git push*", "cat <<-'END'\n\tgit push\n\tEND\nls", true},
		{"bash: git push*", `cat <<< "git push"`, false},

		// The field form on the field command splits too.
		{"bash: command=git push*", "cd x && git push", true},
		// The field form on another field does not.
		{"files: path=secrets/*", "", false},
	}
	for _, c := range cases {
		if got := ruleMatches(c.rule, bashCall(c.command), false); got != c.match {
			t.Errorf("rule %q on %q: %v, want %v", c.rule, c.command, got, c.match)
		}
	}
	// A path that holds a separator is not a command.
	if ruleMatches("files: path=secrets/*", files(`{"path":"x;secrets/a"}`), false) {
		t.Fatal("a field rule on path split the path")
	}
}

// The rule of the finding asks about a push after cd, and a deny rule
// denies a command after a separator, in the policy of rules.
func TestCommandRuleDecisions(t *testing.T) {
	p := NewRulesPolicy(Rules{Default: "allow", Ask: []string{"bash: git push*"}, Deny: []string{"bash: rm -rf*"}})
	cases := []struct {
		command string
		verdict Verdict
		reason  string
	}{
		{pushAfterCd, VerdictAsk, "rule: ask bash: git push*"},
		{"git push origin main", VerdictAsk, "rule: ask bash: git push*"},
		{"make clean || rm -rf build", VerdictDeny, "rule: deny bash: rm -rf*"},
		{"git status && go test ./...", VerdictAllow, "default"},
	}
	for _, c := range cases {
		d := p.Decide(context.Background(), bashCall(c.command))
		if d.Verdict != c.verdict || d.Reason != c.reason {
			t.Errorf("%q: %s (%s), want %s (%s)", c.command, d.Verdict, d.Reason, c.verdict, c.reason)
		}
	}
	// The merged rules of a profile keep the split in their clause.
	m := MergeRules(Rules{Default: "allow"}, Rules{Default: "allow", Ask: []string{"bash: git push*"}})
	if d := NewRulesPolicy(m).Decide(context.Background(), bashCall(pushAfterCd)); d.Verdict != VerdictAsk {
		t.Fatalf("merged rules on the push after cd: %s (%s)", d.Verdict, d.Reason)
	}
}

// An allow rule matches the whole text of the command only, so one part
// of a compound command never allows the other parts, and an ask rule on
// a part still wins over an allow rule on the whole text.
func TestAllowRuleMatchesTheWholeCommand(t *testing.T) {
	p := NewRulesPolicy(Rules{Default: "ask", Allow: []string{"bash: git status*"}})
	cases := []struct {
		command string
		verdict Verdict
	}{
		{"git status", VerdictAllow},
		{"git status --short", VerdictAllow},
		{"cd x && git status", VerdictAsk},
		{"sudo git status", VerdictAsk},
		{"(git status)", VerdictAsk},
	}
	for _, c := range cases {
		if d := p.Decide(context.Background(), bashCall(c.command)); d.Verdict != c.verdict {
			t.Errorf("%q: %s (%s), want %s", c.command, d.Verdict, d.Reason, c.verdict)
		}
	}
	p = NewRulesPolicy(Rules{Default: "allow", Allow: []string{"bash: git status*"}, Ask: []string{"bash: git push*"}})
	if d := p.Decide(context.Background(), bashCall("git status && git push")); d.Verdict != VerdictAsk {
		t.Fatalf("an allow of the whole text won over an ask of a part: %s (%s)", d.Verdict, d.Reason)
	}
}

// A command past a bound of the split matches every deny and ask rule with
// a command pattern for its tool, and no rule of another tool.
func TestCommandPastABoundMatches(t *testing.T) {
	deep := "echo hi"
	for range maxCommandDepth + 2 {
		deep = "echo $(" + deep + ")"
	}
	if _, complete := commandForms(deep); complete {
		t.Fatal("a command nested past the bound is complete")
	}
	if !ruleMatches("bash: rm -rf*", bashCall(deep), false) {
		t.Fatal("a deny rule does not match a command nested past the bound")
	}
	if ruleMatches("bash: rm -rf*", bashCall(deep), true) {
		t.Fatal("an allow rule matches a part of a command nested past the bound")
	}
	if ruleMatches("http: rm -rf*", bashCall(deep), false) {
		t.Fatal("a rule of another tool matches")
	}
	long := "sudo" + strings.Repeat(" package", 5000)
	if _, complete := commandForms(long); complete {
		t.Fatal("a command whose forms pass the size bound is complete")
	}
	if !ruleMatches("bash: git push*", bashCall(long), false) {
		t.Fatal("an ask rule does not match a command past the size bound")
	}
	// A deep nesting within the bound is complete and finds its command.
	ok := "git push"
	for range maxCommandDepth - 1 {
		ok = "echo $(" + ok + ")"
	}
	if forms, complete := commandForms(ok); !complete || !slices.Contains(forms, "git push") {
		t.Fatalf("forms within the bound = %q, complete %v", forms, complete)
	}
}

// The split of a long here-document stays complete, so a script that a
// command writes to a file does not trip the bound.
func TestCommandFormsOfALongHereDocument(t *testing.T) {
	cmd := "cat > notes.md <<'EOF'\n" + strings.Repeat("A line of text; with | some \"quotes\" and it's $(fine)\n", 4000) + "EOF\ngit push"
	forms, complete := commandForms(cmd)
	if !complete || forms[len(forms)-1] != "git push" {
		t.Fatalf("complete %v, last form %q", complete, forms[len(forms)-1])
	}
}
