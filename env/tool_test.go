//go:build unix

package env

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func callTool(t *testing.T, ctx context.Context, tl fw.Tool, input string) fw.ToolResult {
	t.Helper()
	res, err := tl.Call(ctx, json.RawMessage(input))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// add runs the manager with the package, installs the change, and records
// fw.env.changed with the new digest; remove takes it out again; list
// names the packages.
func TestToolAddRemove(t *testing.T) {
	f := newFixture(t, fullBlock)
	tl, err := newTool(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := f.runCtx()
	before := f.s.digest()
	res := callTool(t, ctx, tl, `{"command":"add","manager":"python","package":"six>=1.16"}`)
	if res.IsError || !strings.Contains(fw.TextOf(res.Content), "Added six>=1.16 in pyproject.toml") {
		t.Fatalf("add = %+v", res)
	}
	if !strings.Contains(f.read(f.s.python), "# dependency six>=1.16") {
		t.Fatal("the manifest lacks the package")
	}
	if calls := strings.Join(f.calls(), "\n"); !strings.Contains(calls, "uv add --no-sync --project "+f.root+" -- six>=1.16") {
		t.Fatalf("calls = %s", calls)
	}
	m := f.s.readMarker()
	if m == nil || m.Digest == before || m.Digest != f.s.digest() || len(m.Python.Packages) != 2 {
		t.Fatalf("marker = %+v", m)
	}
	evs := f.events(2)
	if len(evs) != 2 || evs[0].Type != EvReady || evs[1].Type != EvChanged {
		t.Fatalf("events = %+v", evs)
	}
	ch := decode[ChangedData](t, evs[1])
	if ch.Digest != m.Digest || ch.Previous != before || ch.Manager != "python" || ch.Command != "add" || ch.Package != "six>=1.16" || ch.Run != "run-1" || ch.Agent != "worker" {
		t.Fatalf("changed = %+v", ch)
	}
	res = callTool(t, ctx, tl, `{"command":"remove","manager":"tools","package":"jq"}`)
	if res.IsError || !strings.Contains(f.read(f.s.tools), "# remove jq") {
		t.Fatalf("remove = %+v", res)
	}
	res = callTool(t, ctx, tl, `{"command":"list"}`)
	text := fw.TextOf(res.Content)
	for _, want := range []string{"State: ready", "pyyaml 6.0.2", "six 1.0.0", "jq 1.7.1", f.s.venv()} {
		if !strings.Contains(text, want) {
			t.Errorf("list lacks %q: %s", want, text)
		}
	}
	for input, want := range map[string]string{
		`{"command":"add","manager":"apt","package":"curl"}`:     "is not python or tools",
		`{"command":"add","manager":"python","package":"--pre"}`: "starts with a dash",
		`{"command":"add","manager":"python"}`:                   "the package is empty",
		`{"command":"upgrade"}`:                                  "is not list, add, or remove",
	} {
		if res := callTool(t, ctx, tl, input); !res.IsError || !strings.Contains(fw.TextOf(res.Content), want) {
			t.Errorf("%s = %+v, want %q", input, res, want)
		}
	}
}

// A failed add keeps the environment as it was and says why; an image
// refuses every change.
func TestToolRefusals(t *testing.T) {
	f := newFixture(t, "python: pyproject.toml\n")
	tl, _ := newTool(f.cfg)
	ctx := f.runCtx()
	if res := callTool(t, ctx, tl, `{"command":"add","manager":"tools","package":"jq"}`); !res.IsError || !strings.Contains(fw.TextOf(res.Content), "has no tools packages") {
		t.Fatalf("res = %+v", res)
	}
	f.write("fail", "")
	res := callTool(t, ctx, tl, `{"command":"add","manager":"python","package":"six"}`)
	if !res.IsError || !strings.Contains(fw.TextOf(res.Content), "the install failed") || !strings.Contains(fw.TextOf(res.Content), "step python") {
		t.Fatalf("res = %+v", res)
	}
	img := newFixture(t, "placement: image\ndir: "+t.TempDir()+"\npython: pyproject.toml\n")
	tl, _ = newTool(img.cfg)
	if res := callTool(t, img.runCtx(), tl, `{"command":"add","manager":"python","package":"six"}`); !res.IsError || !strings.Contains(fw.TextOf(res.Content), "part of the image") {
		t.Fatalf("res = %+v", res)
	}
	none, _ := newTool(fw.DefaultConfig())
	if !none.Spec().OptIn {
		t.Fatal("the tool without a block is not opt-in")
	}
	if tl.Spec().OptIn {
		t.Fatal("the tool with a block is opt-in")
	}
	if res := callTool(t, context.Background(), none, `{"command":"list"}`); !res.IsError || !strings.Contains(fw.TextOf(res.Content), "no environment") {
		t.Fatalf("res = %+v", res)
	}
}

// The default rules ask before add and remove and allow list. A profile
// that keeps its packages denies the tool with "env: *". The policy names
// a tool and a field, so the rule "env.add" names a tool that does not
// exist and matches nothing.
func TestPolicy(t *testing.T) {
	f := newFixture(t, fullBlock)
	tl, _ := newTool(f.cfg)
	call := func(input string) fw.ToolCall {
		return fw.ToolCall{Spec: tl.Spec(), Input: json.RawMessage(input)}
	}
	add, remove, list := call(`{"command":"add","manager":"python","package":"six"}`), call(`{"command":"remove","manager":"python","package":"six"}`), call(`{"command":"list"}`)
	def := fw.NewRulesPolicy(fw.DefaultConfig().Policy.Rules)
	ctx := context.Background()
	for c, want := range map[*fw.ToolCall]fw.Verdict{&add: fw.VerdictAsk, &remove: fw.VerdictAsk, &list: fw.VerdictAllow} {
		if got := def.Decide(ctx, *c).Verdict; got != want {
			t.Errorf("default %s = %v, want %v", c.Input, got, want)
		}
	}
	locked := fw.NewRulesPolicy(fw.Rules{Deny: []string{"env: *"}})
	for _, c := range []fw.ToolCall{add, remove, list} {
		if got := locked.Decide(ctx, c).Verdict; got != fw.VerdictDeny {
			t.Errorf("env: * on %s = %v", c.Input, got)
		}
	}
	addOnly := fw.NewRulesPolicy(fw.Rules{Ask: []string{"env: command=add"}})
	if addOnly.Decide(ctx, add).Verdict != fw.VerdictAsk || addOnly.Decide(ctx, remove).Verdict != fw.VerdictAllow {
		t.Error("env: command=add does not single out add")
	}
	if got := fw.NewRulesPolicy(fw.Rules{Ask: []string{"env.add"}}).Decide(ctx, add).Verdict; got != fw.VerdictAllow {
		t.Errorf("env.add = %v; the documentation says it matches nothing", got)
	}
}
