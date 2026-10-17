package view

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

var testTitles = map[string]string{"bash": "Run shell commands", "files": "Read and change files in the workspace", "http": "Fetch web addresses on its allowlist"}

func abilityTool(name, source, desc string, destructive bool) fw.Tool {
	spec := fw.ToolSpec{Name: name, Source: source, Description: desc, Destructive: destructive, Title: testTitles[name]}
	return fw.NewTool(spec, func(context.Context, json.RawMessage) (fw.ToolResult, error) { return fw.TextResult("ok"), nil })
}

func TestAbilities(t *testing.T) {
	cfg := fw.DefaultConfig()
	cfg.Policy.Rules = fw.Rules{Ask: []string{"destructive"}, Deny: []string{"bash: git push*"}}
	a := newAgent(t, fw.WithConfig(cfg), fw.WithTools(
		abilityTool("bash", "builtin", "Run a command.", true),
		abilityTool("files", "builtin", "View and edit files.", false),
		abilityTool("http", "builtin", "Fetch a URL.", false),
		abilityTool("await_task", "builtin", "Wait.", false),
		abilityTool("github.create_pr", "github", "Create a pull request. It opens a branch first.", false),
		abilityTool("github.merge", "github", "Merge a pull request.", true),
	))
	p := fw.AgentProfile{Name: "reviewer", Tools: []string{"bash", "files", "github.*"}, Policy: &fw.Rules{Deny: []string{"github.merge"}}}
	abs, err := Abilities(context.Background(), a, p)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Ability{}
	for _, ab := range abs {
		got[ab.Tool] = ab
	}
	if _, ok := got["await_task"]; ok {
		t.Error("await_task has an ability of its own")
	}
	if b := got["bash"]; b.Text != "Run shell commands" || b.Verdict != "ask" || len(b.Notes) != 1 || b.Notes[0] != `Never runs "git push*"` {
		t.Errorf("bash = %+v", b)
	}
	if f := got["files"]; f.Verdict != "allow" || f.Text != "Read and change files in the workspace" {
		t.Errorf("files = %+v", f)
	}
	if h := got["http"]; h.Verdict != "none" {
		t.Errorf("http = %+v, want none", h)
	}
	if pr := got["github.create_pr"]; pr.Text != "Create a pull request" || pr.Verdict != "allow" {
		t.Errorf("create_pr = %+v", pr)
	}
	if m := got["github.merge"]; m.Verdict != "deny" {
		t.Errorf("merge = %+v", m)
	}

	text := AbilityText(abs)
	for _, want := range []string{
		"Does on its own: read and change files in the workspace; create a pull request.",
		`Asks you first: run shell commands (never runs "git push*").`,
		"Cannot: fetch web addresses on its allowlist; merge a pull request.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
}

func TestAgentViewLockedAndAbilities(t *testing.T) {
	a := newAgent(t, fw.WithProfile(fw.AgentProfile{Name: "fixed"}), fw.WithProfileStore(lockedStore{}),
		fw.WithTools(abilityTool("files", "builtin", "", false)))
	v, err := Agent(context.Background(), a, "fixed")
	if err != nil {
		t.Fatal(err)
	}
	if !v.Locked || len(v.Abilities) != 1 || v.Abilities[0].Verdict != "allow" {
		t.Errorf("view = %+v", v)
	}
	list, err := Agents(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || !list[0].Locked || list[0].Abilities != nil {
		t.Errorf("list = %+v", list)
	}
}

func TestLowerFirst(t *testing.T) {
	for in, want := range map[string]string{"Run it": "run it", "GitHub sync": "GitHub sync", "URL check": "URL check", "": "", "É": "é"} {
		if got := lowerFirst(in); got != want {
			t.Errorf("lowerFirst(%q) = %q, want %q", in, got, want)
		}
	}
}

type lockedStore struct{}

func (lockedStore) Put(context.Context, fw.AgentProfile) error { return nil }
func (lockedStore) Delete(context.Context, string) error       { return nil }
func (lockedStore) Locked(string) bool                         { return true }
