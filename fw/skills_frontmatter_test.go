package fw

import (
	"strings"
	"testing"
)

// A description that is a YAML block scalar, folded or literal, with any
// chomping, an indentation digit, or a comment, reads as its text on one
// line, and not as its indicator. The marketplace skills academy-guide,
// claude-api, and discernment-nudge have such descriptions.
func TestSkillFrontMatterBlockScalar(t *testing.T) {
	long := strings.Repeat("long ", 100)
	for _, c := range []struct{ fm, want string }{
		{"description: >\n  Use this skill when\n  you write Go.\n", "Use this skill when you write Go."},
		{"description: >-\n  Folded and\n  stripped.\n", "Folded and stripped."},
		{"description: >+\n  Folded and kept.\n\n", "Folded and kept."},
		{"description: |\n  Line one.\n  Line two.\n", "Line one. Line two."},
		{"description: |-\n  Literal, stripped.\nlicense: MIT\n", "Literal, stripped."},
		{"description: |2+\n    Indented two.\n", "Indented two."},
		{"description: > # a comment\n  With a comment.\n", "With a comment."},
		{"description: A plain line.\n", "A plain line."},
		{"description: '>'\n", ">"},
		{"description: >\n  " + long + "\n", strings.TrimSpace(CutSkillDescription(long))},
	} {
		name, got := SkillFrontMatter("---\nname: a\n" + c.fm + "---\nBody.\n")
		if name != "a" || got != c.want {
			t.Errorf("front matter %q = %q, %q, want %q", c.fm, name, got, c.want)
		}
	}

	root := t.TempDir()
	writeSkill(t, root, "claude-api", "---\nname: claude-api\ndescription: |-\n  Reference for\n  the API.\n---\n", "Body.\n")
	skills, err := LoadSkills(root)
	if err != nil || len(skills) != 1 || !strings.Contains(SkillsIndex(skills, nil), "- claude-api: Reference for the API.") {
		t.Fatalf("skills = %+v, %v", skills, err)
	}
}
