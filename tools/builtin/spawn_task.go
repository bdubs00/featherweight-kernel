package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func init() { fw.RegisterTool("spawn_task", newSpawnTask) }

const spawnTaskSchema = `{
  "type": "object",
  "properties": {
    "input": {
      "type": "string",
      "description": "The task for the child, complete and self-contained: what to do, what it needs to know, where to find things, and what to return. The child does not see this conversation."
    },
    "agent": {
      "type": "string",
      "description": "The agent profile of the child. Leave it out for the default profile."
    },
    "system": {
      "type": "string",
      "description": "A system prompt for the child. Without it, the child uses the system prompt of its profile."
    },
    "instructions": {
      "type": "string",
      "description": "Text that follows the system prompt of the child: rules or notes for this task."
    },
    "tools": {
      "type": "array",
      "items": {"type": "string"},
      "description": "The tools the child can use, as names or globs such as files or git.*. The child never gets a tool that you do not have. Leave it out for every tool you have."
    },
    "skills": {
      "type": "array",
      "items": {"type": "string"},
      "description": "The skills the child can use, as names or globs. An empty list means no skill. Leave it out for the skills of the profile."
    },
    "model": {
      "type": "string",
      "description": "The model of the child, as provider/model or as a name from the models table of the configuration. Leave it out for the model of the profile."
    },
    "effort": {
      "type": "string",
      "enum": ["low", "medium", "high", "xhigh", "max"],
      "description": "The reasoning effort of the child."
    },
    "max_turns": {
      "type": "integer",
      "minimum": 1,
      "description": "The most model turns the child can take."
    },
    "deadline": {
      "type": "string",
      "description": "The most time the child can take, as a duration such as 10m or 1h30m."
    },
    "wait": {
      "type": "boolean",
      "description": "true waits for the child and returns its output. false returns the run id at once, and await_task collects the result later. The default is true."
    }
  },
  "required": ["input"],
  "additionalProperties": false
}`

const spawnTaskDescription = "Start a child run that works on a task by itself and returns its answer. " +
	"Use it to hand off a subtask: a search, a review, a draft, or any piece of work that is better done in a fresh context. " +
	"Give the child a complete, self-contained task, because it does not see this conversation: say what to do, what it needs to know, where to find things, and what to return. " +
	"By default the tool waits for the child and returns its output. With wait false it returns the run id at once, and await_task collects the result later. " +
	"You can start several children in one response, and they run at the same time. " +
	"A child gets at most the tools you have; tools, skills, model, effort, max_turns, and deadline narrow it further, and it runs at most as long as this run. " +
	"When a child needs an approval or asks a question, the question goes to the caller of this run."

// spawnTaskTool starts a child run: in this process, or in another one
// through the Spawner of the agent (docs/ARCHITECTURE.md section 11.9).
type spawnTaskTool struct{}

func newSpawnTask(*fw.Config) (fw.Tool, error) { return spawnTaskTool{}, nil }

// Spec implements fw.Tool. The tool is parallel-safe, so the kernel runs
// several spawn_task calls of one response at the same time.
func (spawnTaskTool) Spec() fw.ToolSpec {
	s := newSpec("spawn_task", spawnTaskDescription, spawnTaskSchema)
	s.ParallelSafe = true
	return s
}

// spawnInput is the decoded input of spawn_task.
type spawnInput struct {
	Input        string   `json:"input"`
	Agent        string   `json:"agent"`
	System       string   `json:"system"`
	Instructions string   `json:"instructions"`
	Tools        []string `json:"tools"`
	Skills       []string `json:"skills"`
	Model        string   `json:"model"`
	Effort       string   `json:"effort"`
	MaxTurns     int      `json:"max_turns"`
	Deadline     string   `json:"deadline"`
	Wait         *bool    `json:"wait"`
}

// Call implements fw.Tool.
func (spawnTaskTool) Call(ctx context.Context, input json.RawMessage) (fw.ToolResult, error) {
	var in spawnInput
	if err := decode(input, &in); err != nil {
		return badInput("spawn_task", err), nil
	}
	a := fw.AgentFrom(ctx)
	if a == nil {
		return fw.ErrorResult("spawn_task: no agent is in this context, so no child run can start."), nil
	}
	parent := fw.RunInfoFrom(ctx).RunID
	if parent == "" {
		return fw.ErrorResult("spawn_task: no run is in this context, so no child run can start."), nil
	}
	if strings.TrimSpace(in.Input) == "" {
		return fw.ErrorResult("spawn_task: the input is empty. Give the child a complete task."), nil
	}
	opts, bad, ok := spawnOptions(in, parent)
	if !ok {
		return bad, nil
	}
	run, err := a.SpawnChild(ctx, parent, fw.NewRunRequest(in.Input, opts...))
	if err != nil {
		if ctx.Err() != nil {
			return canceled(ctx, "spawn_task")
		}
		return startError(a, in, err), nil
	}
	if in.Wait != nil && !*in.Wait {
		res := fw.TextResult(fmt.Sprintf("Started child run %s. It works in the background. Call await_task with run_id %s to collect its result.", run.ID, run.ID))
		res.Structured = structured(run)
		return res, nil
	}
	done, err := waitChild(ctx, a, "spawn_task", run.ID)
	if err != nil {
		if ctx.Err() != nil {
			return cancelChild(ctx, a, run.ID)
		}
		return fw.ErrorResult(fmt.Sprintf("spawn_task: waiting for child run %s failed: %v", run.ID, err)), nil
	}
	return childResult("spawn_task", done), nil
}

// spawnOptions builds the run options of a child. It returns an error
// result and false when the input cannot be used.
func spawnOptions(in spawnInput, parent string) ([]fw.RunOption, fw.ToolResult, bool) {
	opts := []fw.RunOption{fw.WithOrigin(fw.OriginTool)}
	if in.Agent != "" {
		opts = append(opts, fw.AsAgent(in.Agent))
	}
	if s := strings.TrimSpace(in.Instructions); s != "" {
		opts = append(opts, fw.WithInstructions(s))
	}
	var prof fw.AgentProfile
	set := false
	if in.System != "" {
		prof.System, set = in.System, true
	}
	if in.Tools != nil {
		if len(in.Tools) == 0 {
			return nil, fw.ErrorResult("spawn_task: the tools list is empty. Leave tools out for every tool you have, or name the tools the child needs."), false
		}
		prof.Tools, set = in.Tools, true
	}
	if in.Skills != nil {
		prof.Skills, set = in.Skills, true
	}
	if in.Model != "" {
		prof.Model, set = strings.TrimSpace(in.Model), true
	}
	if in.Effort != "" {
		prof.Effort, set = strings.ToLower(strings.TrimSpace(in.Effort)), true
	}
	if set {
		opts = append(opts, fw.WithRunProfile(prof))
	}
	var b fw.Budget
	if in.MaxTurns < 0 {
		return nil, fw.ErrorResult("spawn_task: max_turns must be a positive integer."), false
	}
	b.MaxTurns = in.MaxTurns
	if s := strings.TrimSpace(in.Deadline); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return nil, fw.ErrorResult(fmt.Sprintf("spawn_task: the deadline %q is not a duration such as 10m or 1h30m.", in.Deadline)), false
		}
		b.Deadline = d
	}
	if b != (fw.Budget{}) {
		opts = append(opts, fw.WithRunBudget(b))
	}
	return opts, fw.ToolResult{}, true
}

// startError maps an error of Agent.Start to an error result that tells
// the model what to change.
func startError(a *fw.Agent, in spawnInput, err error) fw.ToolResult {
	msg := strings.TrimPrefix(err.Error(), "fw: ")
	switch {
	case errors.Is(err, fw.ErrSpawnLimit):
		return fw.ErrorResult(fmt.Sprintf("spawn_task: the spawn limit stops this child: %s. Wait for the children you have, or do the work yourself.", msg))
	case errors.Is(err, fw.ErrAgentNotFound):
		var names []string
		for _, p := range a.Profiles() {
			names = append(names, p.Name)
		}
		return fw.ErrorResult(fmt.Sprintf("spawn_task: there is no agent profile named %q. Known profiles: %s. Leave agent out for the default profile.", in.Agent, strings.Join(names, ", ")))
	case errors.Is(err, fw.ErrInvalidRequest):
		return fw.ErrorResult(fmt.Sprintf("spawn_task: the request is not valid: %s. Fix the input and try again.", msg))
	case errors.Is(err, fw.ErrClosed):
		return fw.ErrorResult("spawn_task: the agent is shutting down, so no child run can start.")
	}
	return fw.ErrorResult(fmt.Sprintf("spawn_task: the child run did not start: %s", msg))
}

// cancelChild cancels a child because the context of the parent ended, and
// returns the canceled result with the context error.
func cancelChild(ctx context.Context, a *fw.Agent, id string) (fw.ToolResult, error) {
	cause := context.Cause(ctx)
	if err := a.Cancel(context.Background(), id); err != nil {
		logger(ctx).Warn("spawn_task: cancel child run", "run", id, "error", err)
	}
	res := fw.ErrorResult(fmt.Sprintf("spawn_task: canceled: child run %s was canceled because this run stopped: %v", id, cause))
	if r, err := a.Get(context.Background(), id); err == nil {
		res.Structured = structured(r)
	}
	return res, fmt.Errorf("builtin: spawn_task: %w", cause)
}
