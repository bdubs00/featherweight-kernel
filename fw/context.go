package fw

import "context"

// History is the message view of a run at one epoch.
type History struct {
	Epoch    int       `json:"epoch"`
	Messages []Message `json:"messages"`
}

// Compactor shortens a history that is near the context limit. It returns a
// summary block and the messages to keep after it. The kernel appends
// fw.context.compacted with the summary and starts a new epoch.
//
// m is the model of the run, bound to the run: a request that leaves System,
// Tools, Effort, MaxTokens, or Metadata empty gets the value of the next turn.
// A summary call that sends the history with an instruction after it
// therefore has the same prefix as a turn. It reads the prompt cache, and the
// thinking blocks in the history stay valid for the provider.
//
// A kept message must not carry an opaque block, such as a thinking block,
// because the provider binds such a block to the history before it, and
// that history is gone after the compaction.
type Compactor interface {
	Compact(ctx context.Context, m Model, h History) (summary Block, keep []Message, err error)
}

// CompactorFunc adapts a function to Compactor.
type CompactorFunc func(ctx context.Context, m Model, h History) (Block, []Message, error)

// Compact implements Compactor.
func (f CompactorFunc) Compact(ctx context.Context, m Model, h History) (Block, []Message, error) {
	return f(ctx, m, h)
}

// ProgressFunc reports progress of a tool call.
type ProgressFunc func(progress, total float64, message string)

type progressKey struct{}

// WithProgress stores a progress reporter in the context.
func WithProgress(ctx context.Context, fn ProgressFunc) context.Context {
	return context.WithValue(ctx, progressKey{}, fn)
}

// ReportProgress calls the progress reporter in the context, if any.
func ReportProgress(ctx context.Context, progress, total float64, message string) {
	if fn, ok := ctx.Value(progressKey{}).(ProgressFunc); ok && fn != nil {
		fn(progress, total, message)
	}
}

// RunInfo identifies the run that a tool or model call belongs to.
type RunInfo struct {
	RunID  string
	Agent  string
	CallID string
	// Principal is the subject of the person or workload that started the
	// run, which a child run inherits. PrincipalKind is its kind, such as
	// KindUser. A tool that acts for a person reads both (see PersonTokens).
	Principal     string
	PrincipalKind string
	// Dir is the absolute directory where the tools of the run work: the
	// root of the files tool, or the directory of the run with
	// tools.files.perRun (section 7.9). bash starts there.
	Dir string
}

// Keys of the _meta object that the MCP client of the kernel puts on each
// tools/call of a run (docs/ARCHITECTURE.md section 7.3): the id of the
// run, the name of its profile, and the node of the process, and for a
// server that tools.mcp marks trusted, the subject and the kind of the
// principal of the run. A server reads them as data about its caller. The
// kernel reads nothing back from them.
const (
	MCPMetaRun           = "io.featherweight/run"
	MCPMetaAgent         = "io.featherweight/agent"
	MCPMetaNode          = "io.featherweight/node"
	MCPMetaPrincipal     = "io.featherweight/principal"
	MCPMetaPrincipalKind = "io.featherweight/principalKind"
)

type runKey struct{}

// WithRunInfo stores run info in the context.
func WithRunInfo(ctx context.Context, info RunInfo) context.Context {
	return context.WithValue(ctx, runKey{}, info)
}

// RunInfoFrom returns the run info in the context.
func RunInfoFrom(ctx context.Context) RunInfo {
	info, _ := ctx.Value(runKey{}).(RunInfo)
	return info
}

type agentKey struct{}

// WithAgent stores the agent in the context. The kernel sets it for every
// tool call, so tools such as send and spawn_task reach the bus and the runs.
func WithAgent(ctx context.Context, a *Agent) context.Context {
	return context.WithValue(ctx, agentKey{}, a)
}

// AgentFrom returns the agent stored in the context, or nil.
func AgentFrom(ctx context.Context) *Agent {
	a, _ := ctx.Value(agentKey{}).(*Agent)
	return a
}

// Asker asks the caller of the run a question and waits for the answer. The
// kernel implements it as an approval with a form schema: the run moves to
// input-required, every surface shows the question, and the answer content
// comes back. A nil schema means a free-text answer under the key "answer".
type Asker func(ctx context.Context, question string, schema []byte) (answer []byte, err error)

type askerKey struct{}

// WithAsker stores an asker in the context.
func WithAsker(ctx context.Context, fn Asker) context.Context {
	return context.WithValue(ctx, askerKey{}, fn)
}

// AskCaller asks the caller through the asker in the context. It returns an
// error when no asker is present.
func AskCaller(ctx context.Context, question string, schema []byte) ([]byte, error) {
	fn, ok := ctx.Value(askerKey{}).(Asker)
	if !ok || fn == nil {
		return nil, ErrNoAsker
	}
	return fn(ctx, question, schema)
}
