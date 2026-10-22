package native

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// fakeSkills is a tool source with one skill, as an MCP server with the
// skills extension gives it.
type fakeSkills struct{}

func (fakeSkills) Tools(context.Context) ([]fw.Tool, error) { return nil, nil }
func (fakeSkills) SourceSkills(context.Context) []fw.Skill {
	return []fw.Skill{{Name: "front/review", Description: "Review a pull request.", Origin: fw.SkillOriginMCP, Server: "front", URI: "skill://acme/review/SKILL.md"}}
}
func (fakeSkills) ReadSkill(_ context.Context, _ fw.Skill, file string) (string, error) {
	return "# Review " + file, nil
}
func (fakeSkills) SkillFiles(fw.Skill) []string { return []string{"ref.md"} }

// TestSkillsOfASource checks that GET /v1/skills and GET /v1/skills/{name}
// list and serve a skill of a source, with its origin and its URI.
func TestSkillsOfASource(t *testing.T) {
	a := newAgent(t, newScript().model(), fw.WithToolSources(fakeSkills{}))
	srv := serve(t, a)
	resp, b := do(t, "GET", srv.URL+"/v1/skills", "")
	var list struct {
		Skills []skillInfo `json:"skills"`
	}
	decodeJSON(t, resp, b, http.StatusOK, &list)
	want := skillInfo{Name: "front/review", Description: "Review a pull request.", Origin: "mcp", Server: "front", URI: "skill://acme/review/SKILL.md"}
	if len(list.Skills) != 1 || list.Skills[0] != want {
		t.Fatalf("skills = %s", b)
	}
	resp, b = do(t, "GET", srv.URL+"/v1/skills/front/review", "")
	if resp.StatusCode != http.StatusOK || string(b) != "# Review " {
		t.Fatalf("SKILL.md = %d %q", resp.StatusCode, b)
	}
	resp, b = do(t, "GET", srv.URL+"/v1/skills/front/review", "", "Accept", "application/json")
	var doc struct {
		Origin string   `json:"origin"`
		URI    string   `json:"uri"`
		Files  []string `json:"files"`
	}
	if err := json.Unmarshal(b, &doc); err != nil || resp.StatusCode != http.StatusOK || doc.Origin != "mcp" || doc.URI != want.URI || len(doc.Files) != 1 {
		t.Fatalf("doc = %d %s", resp.StatusCode, b)
	}
}
