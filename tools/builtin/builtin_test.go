package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// calls numbers the tool calls of call.
var calls atomic.Int64

// call runs a tool with a JSON input and fails the test on a Go error.
// Each call in the context of a run gets a call id of its own, as each
// tool call of a model has one.
func call(t *testing.T, ctx context.Context, tool fw.Tool, input string) fw.ToolResult {
	t.Helper()
	if info := fw.RunInfoFrom(ctx); info.CallID != "" {
		info.CallID = fmt.Sprintf("%s-%d", info.CallID, calls.Add(1))
		ctx = fw.WithRunInfo(ctx, info)
	}
	res, err := tool.Call(ctx, json.RawMessage(input))
	if err != nil {
		t.Fatalf("%s %s: unexpected Go error: %v", tool.Spec().Name, input, err)
	}
	return res
}

// text returns the text of a result.
func text(res fw.ToolResult) string { return fw.TextOf(res.Content) }

// mustOK fails unless the result is not an error, and returns its text.
func mustOK(t *testing.T, res fw.ToolResult) string {
	t.Helper()
	if res.IsError {
		t.Fatalf("unexpected error result: %s", text(res))
	}
	return text(res)
}

// mustFail fails unless the result is an error that contains want.
func mustFail(t *testing.T, res fw.ToolResult, want string) {
	t.Helper()
	if !res.IsError {
		t.Fatalf("want an error result, got: %s", text(res))
	}
	if !strings.Contains(text(res), want) {
		t.Fatalf("error result %q does not contain %q", text(res), want)
	}
}

func TestRegisteredSpecs(t *testing.T) {
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Tools.Files.Root = t.TempDir()
	type flags struct {
		native                                        string
		readOnly, idempotent, destructive, parallelOK bool
	}
	want := map[string]flags{
		"bash":       {native: fw.NativeBash, destructive: true},
		"files":      {native: fw.NativeTextEditor},
		"http":       {readOnly: true, idempotent: true, parallelOK: true},
		"memory":     {native: fw.NativeMemory},
		"recall":     {readOnly: true, idempotent: true, parallelOK: true},
		"ask_user":   {readOnly: true},
		"send":       {},
		"spawn_task": {parallelOK: true},
		"await_task": {readOnly: true, idempotent: true, parallelOK: true},
		"skill":      {readOnly: true, idempotent: true, parallelOK: true},
		"clock":      {readOnly: true, idempotent: true, parallelOK: true},
	}
	registered := fw.RegisteredTools()
	for name, w := range want {
		if !slices.Contains(registered, name) {
			t.Fatalf("tool %q is not registered (have %v)", name, registered)
		}
		tool, err := fw.NewBuiltinTool(name, cfg)
		if err != nil {
			t.Fatalf("NewBuiltinTool(%q): %v", name, err)
		}
		if c, ok := tool.(interface{ Close() error }); ok {
			t.Cleanup(func() { _ = c.Close() })
		}
		s := tool.Spec()
		got := flags{s.Native, s.ReadOnly, s.Idempotent, s.Destructive, s.ParallelSafe}
		if s.Name != name || s.Source != "builtin" || !s.Trusted || got != w {
			t.Errorf("%s: spec %+v, want flags %+v", name, s, w)
		}
		var schema map[string]any
		if err := json.Unmarshal(s.InputSchema, &schema); err != nil || schema["type"] != "object" {
			t.Errorf("%s: input schema is not a JSON object schema: %v", name, err)
		}
		if s.Description == "" {
			t.Errorf("%s: empty description", name)
		}
	}
}

// TestActionLabels checks the allow-button label of every built-in tool. A
// new tool fails the test until it names its label here, so each label is
// a decision.
func TestActionLabels(t *testing.T) {
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Tools.Files.Root = t.TempDir()
	want := map[string]string{
		"bash":       "Run command",
		"http":       "Send request",
		"spawn_task": "Hand off task",
		"send":       "Send message",
		"files":      "", // reads and writes
		"memory":     "", // reads, writes, and deletes
		"recall":     "", // reads only
		"ask_user":   "", // a question, not an action
		"await_task": "", // waits or lists
		"skill":      "",
		"clock":      "",
	}
	for name := range titles {
		if _, ok := want[name]; !ok {
			t.Errorf("tool %s has no label decision in this test", name)
		}
	}
	for name, w := range want {
		tool, err := fw.NewBuiltinTool(name, cfg)
		if err != nil {
			t.Fatalf("NewBuiltinTool(%q): %v", name, err)
		}
		if c, ok := tool.(interface{ Close() error }); ok {
			t.Cleanup(func() { _ = c.Close() })
		}
		if got := tool.Spec().Action; got != w {
			t.Errorf("%s: action %q, want %q", name, got, w)
		}
	}
}
