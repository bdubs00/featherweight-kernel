package view

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func TestStepPhrase(t *testing.T) {
	long := strings.Repeat("é", 100)
	for _, tc := range []struct {
		tool, input string
		spec        fw.ToolSpec
		want        string
	}{
		{"bash", `{"command":"go test ./..."}`, fw.ToolSpec{}, `Ran "go test ./..."`},
		{"bash", `{"command":"\n  cd x\ngo build"}`, fw.ToolSpec{}, `Ran "cd x"`},
		{"bash", `{"command":"echo ` + long + `"}`, fw.ToolSpec{}, `Ran "echo ` + strings.Repeat("é", 74) + `…"`},
		{"bash", `{"restart":true}`, fw.ToolSpec{Title: "Run shell commands"}, "Run shell commands"},
		{"files", `{"command":"view","path":"docs/a.md"}`, fw.ToolSpec{}, "Read docs/a.md"},
		{"files", `{"command":"create","path":"a.go","file_text":"x"}`, fw.ToolSpec{}, "Wrote a.go"},
		{"files", `{"command":"str_replace","path":"a.go"}`, fw.ToolSpec{}, "Edited a.go"},
		{"files", `{"command":"delete","path":"a.go"}`, fw.ToolSpec{}, "Removed a.go"},
		{"files", `{"command":"chmod","path":"a.go"}`, fw.ToolSpec{Title: "Read and change files"}, "Read and change files"},
		{"http", `{"url":"https://example.com/a"}`, fw.ToolSpec{}, "Fetched https://example.com/a"},
		{"deploy", `{"env":"prod"}`, fw.ToolSpec{Title: "Deploy the app"}, "Deploy the app"},
		{"deploy", `{"env":"prod"}`, fw.ToolSpec{}, "deploy"},
		{"bash", `"{\"command\":\"go te`, fw.ToolSpec{}, "bash"},
	} {
		if got := StepPhrase(tc.tool, json.RawMessage(tc.input), tc.spec); got != tc.want {
			t.Errorf("StepPhrase(%s, %s) = %q, want %q", tc.tool, tc.input, got, tc.want)
		}
	}
}

func TestActivityPhrase(t *testing.T) {
	in := json.RawMessage(`{"command":"go test ./..."}`)
	for _, tc := range []struct {
		run   *fw.Run
		input json.RawMessage
		want  string
	}{
		{nil, nil, ""},
		{&fw.Run{}, in, ""},
		{&fw.Run{Activity: &fw.RunActivity{Turn: 7}}, nil, "Thinking in turn 7"},
		{&fw.Run{Activity: &fw.RunActivity{Turn: 7, Tool: "bash", CallID: "c1"}}, in, `Ran "go test ./..."`},
		{&fw.Run{Activity: &fw.RunActivity{Turn: 7, Tool: "bash", CallID: "c1"}}, nil, "bash"},
		{&fw.Run{Activity: &fw.RunActivity{Turn: 2, Tool: "front.bridge_post", CallID: "c2"}}, nil, "front.bridge_post"},
	} {
		if got := ActivityPhrase(tc.run, tc.input); got != tc.want {
			t.Errorf("ActivityPhrase(%+v) = %q, want %q", tc.run, got, tc.want)
		}
	}
}
