package builtin

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func TestSkillTool(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "greet", "SKILL.md"), "---\nname: greet\ndescription: Says hello in style\n---\n# Greet\n\nSay hello, then run scripts/hello.sh.\n")
	writeFile(t, filepath.Join(dir, "greet", "scripts", "hello.sh"), "echo hello\n")
	writeFile(t, filepath.Join(dir, "greet", "reference.md"), "notes\n")
	writeFile(t, filepath.Join(dir, "farewell", "SKILL.md"), "# Farewell\n\nSay goodbye.\n")
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Tools.Skills = []string{dir}
	s := newScripted()
	a := newSpawnAgent(t, s.Model(), fw.WithConfig(cfg))
	tool := builtinTool(t, "skill", cfg)
	ctx := fw.WithAgent(context.Background(), a)

	got := mustOK(t, call(t, ctx, tool, `{}`))
	if !strings.Contains(got, "- greet: Says hello in style") || !strings.Contains(got, "- farewell") {
		t.Fatalf("list = %q", got)
	}

	res := call(t, ctx, tool, `{"name":"greet"}`)
	got = mustOK(t, res)
	for _, want := range []string{"name: greet", "Say hello, then run scripts/hello.sh.", "- scripts/hello.sh", "- reference.md", filepath.Join(dir, "greet")} {
		if !strings.Contains(got, want) {
			t.Fatalf("skill text %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "- SKILL.md") {
		t.Fatalf("SKILL.md is listed as another file: %q", got)
	}
	var st struct {
		Name  string   `json:"name"`
		Files []string `json:"files"`
	}
	if err := json.Unmarshal(res.Structured, &st); err != nil || st.Name != "greet" || len(st.Files) != 2 {
		t.Fatalf("structured = %s", res.Structured)
	}

	got = mustOK(t, call(t, ctx, tool, `{"name":"farewell"}`))
	if !strings.Contains(got, "Say goodbye.") || !strings.Contains(got, "no other files") {
		t.Fatalf("farewell = %q", got)
	}

	res = call(t, ctx, tool, `{"name":"nope"}`)
	mustFail(t, res, `no skill named "nope"`)
	mustFail(t, res, "greet")
	mustFail(t, call(t, context.Background(), tool, `{}`), "no agent")
}

func TestSkillToolWithoutSkills(t *testing.T) {
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Tools.Skills = []string{filepath.Join(t.TempDir(), "missing")}
	a := newSpawnAgent(t, newScripted().Model(), fw.WithConfig(cfg))
	got := mustOK(t, call(t, fw.WithAgent(context.Background(), a), builtinTool(t, "skill", cfg), `{}`))
	if !strings.Contains(got, "No skill is installed") {
		t.Fatalf("empty list = %q", got)
	}
}
