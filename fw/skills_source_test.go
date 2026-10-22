package fw

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

// tools.mcp[].skills takes auto, off, or a list, in YAML and in JSON.
func TestMCPSkillsDecode(t *testing.T) {
	for _, c := range []struct {
		yaml   string
		auto   bool
		off    bool
		pinned int
	}{
		{"skills: auto", true, false, 0},
		{"skills: off", false, true, 0},
		{"skills: [skill://a/SKILL.md]", false, false, 1},
		{"skills: [auto, skill://a/SKILL.md, skill://b/SKILL.md]", true, false, 2},
		{"{}", true, false, 0},
	} {
		var v struct {
			Skills MCPSkills `yaml:"skills"`
		}
		if err := yaml.Unmarshal([]byte(c.yaml), &v); err != nil {
			t.Fatalf("%s: %v", c.yaml, err)
		}
		if v.Skills.Auto() != c.auto || v.Skills.Off() != c.off || len(v.Skills.Pinned()) != c.pinned {
			t.Errorf("%s = %v", c.yaml, v.Skills)
		}
	}
	var j struct {
		Skills MCPSkills `json:"skills"`
	}
	if err := json.Unmarshal([]byte(`{"skills":"off"}`), &j); err != nil || !j.Skills.Off() {
		t.Fatalf("JSON off = %v, %v", j.Skills, err)
	}
	if err := json.Unmarshal([]byte(`{"skills":["skill://a/SKILL.md"]}`), &j); err != nil || j.Skills.Auto() || len(j.Skills.Pinned()) != 1 {
		t.Fatalf("JSON list = %v, %v", j.Skills, err)
	}
}

// A path of a supporting file stays inside its skill.
func TestSkillFilePath(t *testing.T) {
	for in, want := range map[string]string{"ref.md": "ref.md", "docs/./a.md": "docs/a.md", `docs\b.md`: "docs/b.md"} {
		if got, err := SkillFilePath(in); err != nil || got != want {
			t.Errorf("SkillFilePath(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "/etc/passwd", "../x", "a/../../x", ".git/config", "docs/.secret"} {
		if _, err := SkillFilePath(bad); err == nil {
			t.Errorf("SkillFilePath(%q) passed", bad)
		}
	}
}
