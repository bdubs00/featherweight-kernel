package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func init() { fw.RegisterTool("skill", newSkill) }

const skillSchema = `{
  "type": "object",
  "properties": {
    "name": {
      "type": "string",
      "description": "The name of the skill, as the system prompt lists it, such as deploy or github/review for a skill of an MCP server. Leave it out to list every skill."
    },
    "file": {
      "type": "string",
      "description": "A file of the skill other than SKILL.md, by its path relative to the skill, as the result of reading the skill lists it. Leave it out to read SKILL.md."
    }
  },
  "additionalProperties": false
}`

const skillDescription = "Read a skill. A skill is a set of instructions for one kind of task, kept in a SKILL.md file beside its own scripts and references. " +
	"The system prompt lists the skills you can use, one line each. " +
	"Before you start a task that matches a skill, read the skill with this tool and follow it. " +
	"The result is the text of SKILL.md, then the other files of the skill as paths relative to the skill; " +
	"read one with this tool and the field file when the skill points you to it. " +
	"A skill of an MCP server has the name <server>/<name>, and the tool reads its files from the server when you ask for them. " +
	"Without a name, the tool lists every skill that you can use, with its description."

// skillTool returns skills from the skill directories of the agent.
type skillTool struct{}

func newSkill(*fw.Config) (fw.Tool, error) { return skillTool{}, nil }

// Spec implements fw.Tool.
func (skillTool) Spec() fw.ToolSpec {
	s := newSpec("skill", skillDescription, skillSchema)
	s.ReadOnly = true
	s.Idempotent = true
	s.ParallelSafe = true
	return s
}

// Call implements fw.Tool.
func (skillTool) Call(ctx context.Context, input json.RawMessage) (fw.ToolResult, error) {
	var in struct {
		Name string `json:"name"`
		File string `json:"file"`
	}
	if err := decode(input, &in); err != nil {
		return badInput("skill", err), nil
	}
	a := fw.AgentFrom(ctx)
	if a == nil {
		return fw.ErrorResult("skill: no agent is in this context, so no skill can be read."), nil
	}
	skills := runSkills(ctx, a)
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return listSkills(a, skills), nil
	}
	s, ok := skillNamed(skills, name)
	if !ok {
		return fw.ErrorResult(fmt.Sprintf("skill: there is no skill named %q that this run can use. %s", name, skillNames(skills))), nil
	}
	if file := strings.TrimSpace(in.File); file != "" && file != fw.SkillFile {
		text, err := s.ReadFile(ctx, file)
		if err != nil {
			return fw.ErrorResult(fmt.Sprintf("skill: cannot read %s of %s: %v", file, name, err)), nil
		}
		return fw.TextResult(text), nil
	}
	body, err := s.ReadFile(ctx, "")
	if err != nil {
		return fw.ErrorResult(fmt.Sprintf("skill: cannot read %s: %v", name, err)), nil
	}
	files, err := s.Files()
	if err != nil {
		logger(ctx).Warn("skill: list files", "skill", name, "error", err)
	}
	var sb strings.Builder
	sb.WriteString(strings.TrimRight(body, "\n"))
	sb.WriteString("\n\n")
	where := "in the skill directory " + s.Dir
	if s.Origin == fw.SkillOriginMCP {
		where = "on the MCP server " + s.Server
	}
	switch {
	case len(files) > 0:
		fmt.Fprintf(&sb, "Other files of the skill, %s; read one with this tool and the field file:\n", where)
		for _, f := range files {
			fmt.Fprintf(&sb, "- %s\n", f)
		}
	case s.Origin == fw.SkillOriginMCP:
		fmt.Fprintf(&sb, "The MCP server %s lists no other files of the skill; read a file that the skill names with this tool and the field file.", s.Server)
	default:
		fmt.Fprintf(&sb, "The skill directory %s holds no other files.", s.Dir)
	}
	res := fw.TextResult(strings.TrimRight(sb.String(), "\n"))
	res.Structured, _ = json.Marshal(struct {
		fw.Skill
		Files []string `json:"files"`
	}{Skill: s, Files: files})
	return res, nil
}

// runSkills returns the skills that the run in ctx can use, as the system
// prompt of the run lists them (fw.Agent.SkillsOfRun), or every skill of
// the agent outside a run.
func runSkills(ctx context.Context, a *fw.Agent) []fw.Skill {
	if info := fw.RunInfoFrom(ctx); info.RunID != "" || info.Agent != "" {
		return a.SkillsOfRun(info)
	}
	return a.Skills()
}

// skillNamed finds a skill by its name.
func skillNamed(skills []fw.Skill, name string) (fw.Skill, bool) {
	for _, s := range skills {
		if s.Name == name {
			return s, true
		}
	}
	return fw.Skill{}, false
}

// listSkills returns every skill with its description.
func listSkills(a *fw.Agent, skills []fw.Skill) fw.ToolResult {
	if len(skills) == 0 {
		return fw.TextResult("No skill is installed for this run. A skill is a directory with a SKILL.md file under one of " + strings.Join(a.Config().SkillDirs(), ", ") + ", or a skill of an MCP server.")
	}
	res := fw.TextResult(fw.SkillsIndex(skills, nil))
	res.Structured, _ = json.Marshal(skills)
	return res
}

// skillNames returns a sentence that names the skills.
func skillNames(skills []fw.Skill) string {
	if len(skills) == 0 {
		return "No skill is installed."
	}
	names := make([]string, 0, len(skills))
	for _, s := range skills {
		names = append(names, s.Name)
	}
	return "The installed skills are: " + strings.Join(names, ", ") + "."
}
