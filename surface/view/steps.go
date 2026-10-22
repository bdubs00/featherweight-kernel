package view

// This file words a tool call for a person: the step phrase that a result
// lists, such as `Ran "go test ./..."`, and the activity of a run that
// works (fw.Run.Activity).

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Inputs of the built-in tools that the step phrases read.
type (
	bashInput struct {
		Command string `json:"command"`
	}
	filesInput struct {
		Command string `json:"command"`
		Path    string `json:"path"`
	}
	httpInput struct {
		URL string `json:"url"`
	}
)

// StepPhrase names a tool call for a person: the command of bash, such as
// `Ran "go test ./..."`, the operation and the path of files, such as
// "Read docs/ARCHITECTURE.md", the address of http, and else the title of
// the tool from spec, or its name. input is the input of the call, as
// fw.tool.requested records it.
func StepPhrase(tool string, input json.RawMessage, spec fw.ToolSpec) string {
	switch tool {
	case "bash":
		var in bashInput
		if json.Unmarshal(input, &in) == nil {
			if line := firstLine(in.Command); line != "" {
				return `Ran "` + cutRunes(line, 80) + `"`
			}
		}
	case "files":
		var in filesInput
		if json.Unmarshal(input, &in) == nil && in.Path != "" {
			switch in.Command {
			case "view", "read":
				return "Read " + in.Path
			case "create", "write":
				return "Wrote " + in.Path
			case "str_replace", "insert", "edit":
				return "Edited " + in.Path
			case "delete", "remove":
				return "Removed " + in.Path
			}
		}
	case "http":
		var in httpInput
		if json.Unmarshal(input, &in) == nil && strings.TrimSpace(in.URL) != "" {
			return "Fetched " + strings.TrimSpace(in.URL)
		}
	}
	if spec.Title != "" {
		return spec.Title
	}
	return tool
}

// ActivityPhrase words what a run that works does now (fw.Run.Activity):
// the step phrase of the tool call that it waits for, such as
// `Ran "go test ./..."`, or "Thinking in turn 7" while the model works.
// input is the input of the call: the input of the fw.tool.requested of
// the run whose call id is Activity.CallID. Without it, the phrase names
// the tool. ActivityPhrase returns "" for a run without an activity.
func ActivityPhrase(r *fw.Run, input json.RawMessage) string {
	if r == nil || r.Activity == nil {
		return ""
	}
	act := r.Activity
	if act.Tool == "" {
		if act.Turn > 0 {
			return fmt.Sprintf("Thinking in turn %d", act.Turn)
		}
		return "Thinking"
	}
	return StepPhrase(act.Tool, input, fw.ToolSpec{})
}

// firstLine is the first line of s that has text, trimmed.
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// cutRunes shortens s to n runes, the last of them an ellipsis.
func cutRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}
