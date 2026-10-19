package fw

// This file builds the system prompt of a run from the profile, the
// instructions of the run, and the skills that the profile can use.

import (
	"fmt"
	"strings"
)

// DefaultSystemPrompt is the system prompt of a profile that has none. It
// says what the agent is and how it works, and it names the rules that keep
// a run from looping.
const DefaultSystemPrompt = `You are a featherweight agent. You complete the task in the user message with the tools you have, and then you answer in plain sentences with what you did and what you found.

Work in small steps. Read before you write. Run a command before you claim its result. When a step fails, read the error and change your approach. Do not run the same tool call with the same input again when it already failed or already gave you the answer. If you cannot make progress after a few attempts, stop and say what blocks you.

Keep every tool call small. Ask for a part of a large file or a large result, not the whole of it. Do not take an action that is hard to reverse unless the task asks for it. Do not push, merge, delete, or send anything to an external service on your own judgment.`

// Headings of the composed sections.
const (
	instructionsHeading = "\n\n# Instructions\n\n"
	runHeading          = "\n\n# Instructions for this run\n\n"
	skillsHeading       = "\n\n# Skills\n\n"
	skillsIntro         = "These skills hold instructions for specific tasks. When a task matches one, read the skill with the skill tool before you start.\n\n"
)

// SystemPrompt composes the system prompt of a run: the system prompt of
// the profile, or DefaultSystemPrompt when the profile has none, then the
// instructions of the profile, then the instructions of the run, then the
// list of the skills that the profile can use.
func (a *Agent) SystemPrompt(prof AgentProfile, instructions string) string {
	var sb strings.Builder
	system := strings.TrimSpace(prof.System)
	if system == "" {
		system = DefaultSystemPrompt
	}
	sb.WriteString(system)
	if extra := joinInstructions(prof.Instructions); extra != "" {
		sb.WriteString(instructionsHeading)
		sb.WriteString(extra)
	}
	if s := strings.TrimSpace(instructions); s != "" {
		sb.WriteString(runHeading)
		sb.WriteString(s)
	}
	if list := SkillsIndex(a.Skills(), prof.Skills); list != "" {
		sb.WriteString(skillsHeading)
		sb.WriteString(skillsIntro)
		sb.WriteString(list)
	}
	return sb.String()
}

// joinInstructions returns the instructions of a profile as the system
// prompt holds them: each without the space around it, the empty ones left
// out, joined by a blank line.
func joinInstructions(instructions []string) string {
	var out []string
	for _, s := range instructions {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, "\n\n")
}

// SkillsIndex renders the skills that the globs allow as one line each:
// the name, then the description. It returns "" when no skill is allowed.
func SkillsIndex(skills []Skill, globs []string) string {
	var sb strings.Builder
	for _, s := range skills {
		if !SkillAllowed(globs, s.Name) {
			continue
		}
		fmt.Fprintf(&sb, "- %s", s.Name)
		if s.Description != "" {
			fmt.Fprintf(&sb, ": %s", s.Description)
		}
		sb.WriteByte('\n')
	}
	return strings.TrimRight(sb.String(), "\n")
}
