package fw

import (
	"context"
	"encoding/json"
)

// Native tool shapes that a provider defines. The Anthropic adapter sends
// these by type instead of by schema.
const (
	NativeBash       = "bash_20250124"
	NativeTextEditor = "text_editor_20250728"
	NativeMemory     = "memory_20250818"
)

// ToolSpec describes a tool to the model and to the policy.
type ToolSpec struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	ReadOnly     bool            `json:"readOnly,omitempty"`     // never changes state
	Idempotent   bool            `json:"idempotent,omitempty"`   // safe to run twice
	Destructive  bool            `json:"destructive,omitempty"`  // hard to reverse
	ParallelSafe bool            `json:"parallelSafe,omitempty"` // can run beside other calls
	Native       string          `json:"native,omitempty"`       // provider-defined shape, see Native*
	Source       string          `json:"source,omitempty"`       // "builtin", or the MCP server or peer name
	Trusted      bool            `json:"trusted,omitempty"`      // annotations come from a trusted source
	// Title is a short phrase for a person, such as "Run shell commands".
	Title string `json:"title,omitempty"`
	// Action is the label of the button that allows a call of the tool,
	// such as "Run command" or "Open pull request". It is set only when
	// the label describes every call of the tool truly. An empty Action
	// gets a neutral label, such as "Allow".
	Action string `json:"action,omitempty"`
	// OptIn keeps the tool from a profile unless the tool list of the
	// profile names it. An empty list and "*" leave it out.
	OptIn bool `json:"optIn,omitempty"`
}

// ToolResult is what a tool returns.
type ToolResult struct {
	Content    []Block         `json:"content"`
	IsError    bool            `json:"isError,omitempty"`
	Structured json.RawMessage `json:"structured,omitempty"`
}

// TextResult returns a text result.
func TextResult(s string) ToolResult { return ToolResult{Content: []Block{Text(s)}} }

// ErrorResult returns an error result that the model can read.
func ErrorResult(s string) ToolResult { return ToolResult{Content: []Block{Text(s)}, IsError: true} }

// Tool is one callable capability.
type Tool interface {
	Spec() ToolSpec
	Call(ctx context.Context, input json.RawMessage) (ToolResult, error)
}

// Briefer is a tool that adds a section to the system prompt of a run, such
// as the index of the memory tool. The kernel calls Brief once, when the run
// starts, for each tool of the run that implements it, and records the text
// in fw.run.started. The prompt therefore stays the same for the life of the
// run, after a resume, and in a replay, and its cached prefix stays valid. An
// error or an empty text adds nothing. The context carries the agent
// (AgentFrom) and the run (RunInfoFrom). The kernel finds a Briefer behind
// middleware that has an Unwrap method.
type Briefer interface {
	Brief(ctx context.Context, run RunInfo) (string, error)
}

// unwrapTool returns the tool behind a middleware, or nil.
func unwrapTool(t Tool) Tool {
	if u, ok := t.(interface{ Unwrap() Tool }); ok {
		return u.Unwrap()
	}
	return nil
}

// Resolver is a tool that names the real target of a call. For a file tool,
// that is the file that a path leads to after "./", an absolute path, and
// symbolic links. Before the policy decides a call, the kernel calls
// Resolve and puts the result in ToolCall.Resolved, which has the shape of
// the input. An error leaves Resolved empty, and the tool then refuses the
// call by itself. The kernel finds a Resolver behind middleware that has an
// Unwrap method.
type Resolver interface {
	Resolve(ctx context.Context, input json.RawMessage) (json.RawMessage, error)
}

// ResolveCall returns call with Resolved set when t, or a tool behind its
// middleware, is a Resolver.
func ResolveCall(ctx context.Context, t Tool, call ToolCall) ToolCall {
	for depth := 0; t != nil && depth < 16; depth++ {
		if r, ok := t.(Resolver); ok {
			if res, err := r.Resolve(ctx, call.Input); err == nil && json.Valid(res) {
				call.Resolved = res
			}
			return call
		}
		t = unwrapTool(t)
	}
	return call
}

// brieferOf returns the Briefer behind t and its middleware, if any.
func brieferOf(t Tool) (Briefer, bool) {
	for depth := 0; t != nil && depth < 16; depth++ {
		if b, ok := t.(Briefer); ok {
			return b, true
		}
		t = unwrapTool(t)
	}
	return nil, false
}

// AnsweredQuestion is one recorded question of a tool call and the decision
// that answered it. Request is the stored fw.approval.requested with a form
// schema, and Decision is its fw.approval.decided. Both come from the ledger
// of the run.
type AnsweredQuestion struct {
	Request  ApprovalRequestedData `json:"request"`
	Decision ApprovalDecidedData   `json:"decision"`
}

// Continuer is a tool that can complete a call whose question a restart cut
// off. A call that waits for an answer dies with the process, and the answer
// lands in the ledger without a tool result. On resume the kernel offers
// such a call to the tool with the questions of the call and their decided
// answers, so the tool completes the call from the ledger alone.
//
// Continue must answer the call from the answers and nothing else. It must
// not run the work of the call again, must not ask a new question, and must
// not start anything that runs past the call. The kernel gives Continue a
// context with no asker, so a tool that asks again there fails. A tool that
// cannot answer a call this way does not implement the interface, and the
// kernel then follows the rerun-after-resume gate of a non-idempotent tool.
//
// The kernel finds a Continuer behind middleware that has an Unwrap method.
type Continuer interface {
	Continue(ctx context.Context, input json.RawMessage, answered []AnsweredQuestion) (ToolResult, error)
}

// continuerOf returns the Continuer behind t and its middleware, if any.
func continuerOf(t Tool) (Continuer, bool) {
	for depth := 0; t != nil && depth < 16; depth++ {
		if c, ok := t.(Continuer); ok {
			return c, true
		}
		t = unwrapTool(t)
	}
	return nil, false
}

// ToolSource provides tools that change at runtime, such as an MCP server.
type ToolSource interface {
	Tools(ctx context.Context) ([]Tool, error)
}

// ToolSourceFunc adapts a function to ToolSource.
type ToolSourceFunc func(ctx context.Context) ([]Tool, error)

// Tools implements ToolSource.
func (f ToolSourceFunc) Tools(ctx context.Context) ([]Tool, error) { return f(ctx) }

// ToolMiddleware wraps a tool.
type ToolMiddleware func(Tool) Tool

// ToolCall is one requested call, as the policy sees it. Resolved is the
// input with each target named as the tool resolves it (see Resolver), or
// empty when the tool resolves nothing.
type ToolCall struct {
	RunID    string          `json:"runId"`
	Agent    string          `json:"agent"`
	CallID   string          `json:"callId"`
	Spec     ToolSpec        `json:"spec"`
	Input    json.RawMessage `json:"input"`
	Resolved json.RawMessage `json:"resolved,omitempty"`
}

// FuncTool is a Tool built from a function.
type FuncTool struct {
	spec ToolSpec
	fn   func(ctx context.Context, input json.RawMessage) (ToolResult, error)
}

// NewTool builds a tool from a spec and a function.
func NewTool(spec ToolSpec, fn func(ctx context.Context, input json.RawMessage) (ToolResult, error)) *FuncTool {
	if len(spec.InputSchema) == 0 {
		spec.InputSchema = json.RawMessage(`{"type":"object","additionalProperties":false}`)
	}
	return &FuncTool{spec: spec, fn: fn}
}

// Spec implements Tool.
func (t *FuncTool) Spec() ToolSpec { return t.spec }

// Call implements Tool.
func (t *FuncTool) Call(ctx context.Context, input json.RawMessage) (ToolResult, error) {
	return t.fn(ctx, input)
}

// WrapTool returns a tool with a different spec and the same function.
func WrapTool(t Tool, spec ToolSpec) Tool {
	return &FuncTool{spec: spec, fn: t.Call}
}
