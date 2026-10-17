package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func init() { fw.RegisterTool("await_task", newAwaitTask) }

const awaitTaskSchema = `{
  "type": "object",
  "properties": {
    "run_id": {
      "type": "string",
      "description": "The id of the child run, from spawn_task with wait false. Leave it out to list the child runs of this run with their state."
    }
  },
  "additionalProperties": false
}`

const awaitTaskDescription = "Wait for a child run that spawn_task started with wait false, and return its output. " +
	"Without run_id, list the child runs of this run with their state and a summary of their output. " +
	"While you wait, a child that needs an approval or asks a question is relayed to the caller of this run. " +
	"You can wait for several children in one response."

// awaitTaskTool collects the result of a child run.
type awaitTaskTool struct{}

func newAwaitTask(*fw.Config) (fw.Tool, error) { return awaitTaskTool{}, nil }

// Spec implements fw.Tool. The tool is idempotent, so a resume after a
// restart waits again instead of asking for an approval.
func (awaitTaskTool) Spec() fw.ToolSpec {
	s := newSpec("await_task", awaitTaskDescription, awaitTaskSchema)
	s.ReadOnly = true
	s.Idempotent = true
	s.ParallelSafe = true
	return s
}

// Call implements fw.Tool. Unlike spawn_task, it leaves the child alone
// when the context of this run ends: the child was started on purpose in
// the background, and the next await_task call collects it. A canceled
// parent still cancels its children through the kernel.
func (awaitTaskTool) Call(ctx context.Context, input json.RawMessage) (fw.ToolResult, error) {
	var in struct {
		RunID string `json:"run_id"`
	}
	if err := decode(input, &in); err != nil {
		return badInput("await_task", err), nil
	}
	a := fw.AgentFrom(ctx)
	if a == nil {
		return fw.ErrorResult("await_task: no agent is in this context, so there is no run to wait for."), nil
	}
	self := fw.RunInfoFrom(ctx).RunID
	if self == "" {
		return fw.ErrorResult("await_task: no run is in this context, so there is no child run to wait for."), nil
	}
	id := strings.TrimSpace(in.RunID)
	if id == "" {
		return listChildren(ctx, a, self)
	}
	ok, err := isDescendant(ctx, a, id, self)
	switch {
	case errors.Is(err, fw.ErrRunNotFound):
		return fw.ErrorResult(fmt.Sprintf("await_task: there is no run with id %q. Call await_task without run_id to list your child runs.", id)), nil
	case err != nil:
		return fw.ErrorResult(fmt.Sprintf("await_task: cannot look up run %s: %v", id, err)), nil
	case !ok:
		return fw.ErrorResult(fmt.Sprintf("await_task: run %s is not a child of this run. await_task waits only for the runs that this run started, and for their children.", id)), nil
	}
	done, err := waitChild(ctx, a, "await_task", id)
	if err != nil {
		if ctx.Err() != nil {
			return canceled(ctx, "await_task")
		}
		return fw.ErrorResult(fmt.Sprintf("await_task: waiting for run %s failed: %v", id, err)), nil
	}
	return childResult("await_task", done), nil
}

// childEntry is one child in the list that await_task returns without a
// run id.
type childEntry struct {
	childStatus
	Output string `json:"output,omitempty"`
}

// listChildren returns the child runs of a run, newest first, with their
// state and a summary of their output.
func listChildren(ctx context.Context, a *fw.Agent, self string) (fw.ToolResult, error) {
	kids, err := a.Children(ctx, self)
	if err != nil {
		return fw.ErrorResult(fmt.Sprintf("await_task: cannot list the child runs: %v", err)), nil
	}
	if len(kids) == 0 {
		return fw.TextResult("This run has no child runs. Start one with spawn_task."), nil
	}
	var sb strings.Builder
	if len(kids) == 1 {
		sb.WriteString("This run has 1 child run:\n")
	} else {
		fmt.Fprintf(&sb, "This run has %d child runs, newest first:\n", len(kids))
	}
	entries := make([]childEntry, 0, len(kids))
	for _, k := range kids {
		fmt.Fprintf(&sb, "- %s: %s", k.ID, k.State)
		switch {
		case k.State == fw.StateInputRequired && k.Pending != nil:
			fmt.Fprintf(&sb, " (waits for an answer about %s)", k.Pending.Tool)
		case k.Error != "":
			fmt.Fprintf(&sb, " (%s)", k.Error)
		}
		fmt.Fprintf(&sb, ", %d turns, %d tool calls", k.Usage.Turns, k.Usage.ToolCalls)
		s := summary(k.Output)
		if s != "" {
			fmt.Fprintf(&sb, ". Output: %s", s)
		}
		sb.WriteByte('\n')
		entries = append(entries, childEntry{childStatus: statusOf(k), Output: s})
	}
	res := fw.TextResult(strings.TrimRight(sb.String(), "\n"))
	res.Structured, _ = json.Marshal(entries)
	return res, nil
}
