// Package builtin holds the built-in tools of fw: bash, files, http,
// memory, recall, skill, clock, ask_user, send, spawn_task, await_task, and
// decide.
// Each tool registers itself in init through fw.RegisterTool, so a blank
// import of this package makes them available.
//
// A tool returns an error result, which the model reads, for every problem
// that the model can fix. Call returns a Go error only when the context ends.
package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Source is the ToolSpec.Source of every tool in this package. A tool of
// another package that belongs with the built-in tools uses it too.
const Source = "builtin"

// maxOutput caps the text that bash, files, and memory return before the
// output limit of the configuration applies.
const maxOutput = 100 << 10

// titles words each built-in tool for a person.
var titles = map[string]string{
	"bash":       "Run shell commands",
	"files":      "Read and change files in the workspace",
	"http":       "Fetch web addresses on its allowlist",
	"memory":     "Keep notes between runs",
	"recall":     "Search its memory",
	"skill":      "Read its skills",
	"clock":      "Read the time",
	"ask_user":   "Ask you questions",
	"spawn_task": "Hand work to other agents",
	"await_task": "Wait for the work that it handed off",
	"send":       "Send messages to the message bus",
	"decide":     "Answer typed questions with probabilities",
}

// Title returns the words for a person of a built-in tool, or "" for a
// name that is not a tool of this package.
func Title(name string) string { return titles[name] }

// actions holds the allow-button label of each built-in tool that one label
// describes for every call. A tool with operations of different weight,
// such as files or memory, has no entry and gets the neutral label.
// ask_user has no entry, because it asks a question. A tool that changes
// nothing, such as clock, skill, or await_task, has no
// entry either, because a person seldom allows its calls.
var actions = map[string]string{
	"bash":       "Run command",
	"http":       "Send request",
	"spawn_task": "Hand off task",
	"send":       "Send message",
}

// newSpec returns a spec with the fields that every built-in tool shares.
func newSpec(name, description, schema string) fw.ToolSpec {
	return fw.ToolSpec{
		Name:        name,
		Title:       titles[name],
		Action:      actions[name],
		Description: description,
		InputSchema: json.RawMessage(schema),
		Source:      Source,
		Trusted:     true,
	}
}

// decode reads the tool input into v. An empty input is an empty object.
func decode(input json.RawMessage, v any) error {
	if len(bytes.TrimSpace(input)) == 0 {
		return nil
	}
	return json.Unmarshal(input, v)
}

// badInput is the error result for input that does not decode.
func badInput(tool string, err error) fw.ToolResult {
	return fw.ErrorResult(fmt.Sprintf("%s: the input is not valid JSON for this tool: %v", tool, err))
}

// canceled returns an error result and the wrapped context error.
func canceled(ctx context.Context, tool string) (fw.ToolResult, error) {
	err := context.Cause(ctx)
	return fw.ErrorResult(fmt.Sprintf("%s: canceled: %v", tool, err)), fmt.Errorf("builtin: %s: %w", tool, err)
}

// truncate cuts s to at most n bytes on a rune boundary. It reports whether
// it cut anything.
func truncate(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}

// logger returns the agent logger from the context, or the default logger.
func logger(ctx context.Context) *slog.Logger {
	if a := fw.AgentFrom(ctx); a != nil {
		return a.Logger()
	}
	return slog.Default()
}

// isObject reports whether raw holds a JSON object.
func isObject(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '{'
}

// isNull reports whether raw is absent or the JSON null.
func isNull(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) == 0 || bytes.Equal(raw, []byte("null"))
}
