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
      "description": "The name of the skill, as the system prompt lists it. Leave it out to list every skill."
    }
  },
  "additionalProperties": false
}`

const skillDescription = "Read a skill. A skill is a set of instructions for one kind of task, kept in a SKILL.md file beside its own scripts and references. " +
	"The system prompt lists the skills you can use, one line each. " +
	"Before you start a task that matches a skill, read the skill with this tool and follow it. " +
	"The result is the text of SKILL.md, then the other files of the skill as paths relative to the skill directory; " +
	"read those with the files tool or with bash when the skill points you to them. " +
	"Without a name, the tool lists every skill with its description."

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
	}
	if err := decode(input, &in); err != nil {
		return badInput("skill", err), nil
	}
	a := fw.AgentFrom(ctx)
	if a == nil {
		return fw.ErrorResult("skill: no agent is in this context, so no skill can be read."), nil
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return listSkills(a), nil
	}
	s, ok := a.SkillNamed(name)
	if !ok {
		return fw.ErrorResult(fmt.Sprintf("skill: there is no skill named %q. %s", name, skillNames(a))), nil
	}
	body, err := s.Body()
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
	if len(files) == 0 {
		fmt.Fprintf(&sb, "The skill directory %s holds no other files.", s.Dir)
	} else {
		fmt.Fprintf(&sb, "Other files of the skill, under %s:\n", s.Dir)
		for _, f := range files {
			fmt.Fprintf(&sb, "- %s\n", f)
		}
	}
	res := fw.TextResult(strings.TrimRight(sb.String(), "\n"))
	res.Structured, _ = json.Marshal(struct {
		fw.Skill
		Files []string `json:"files"`
	}{Skill: s, Files: files})
	return res, nil
}

// listSkills returns every skill with its description.
func listSkills(a *fw.Agent) fw.ToolResult {
	skills := a.Skills()
	if len(skills) == 0 {
		return fw.TextResult("No skill is installed. A skill is a directory with a SKILL.md file under one of " + strings.Join(a.Config().SkillDirs(), ", ") + ".")
	}
	var sb strings.Builder
	for _, s := range skills {
		fmt.Fprintf(&sb, "- %s", s.Name)
		if s.Description != "" {
			fmt.Fprintf(&sb, ": %s", s.Description)
		}
		sb.WriteByte('\n')
	}
	res := fw.TextResult(strings.TrimRight(sb.String(), "\n"))
	res.Structured, _ = json.Marshal(skills)
	return res
}

// skillNames returns a sentence that names the installed skills.
func skillNames(a *fw.Agent) string {
	skills := a.Skills()
	if len(skills) == 0 {
		return "No skill is installed."
	}
	names := make([]string, 0, len(skills))
	for _, s := range skills {
		names = append(names, s.Name)
	}
	return "The installed skills are: " + strings.Join(names, ", ") + "."
}
