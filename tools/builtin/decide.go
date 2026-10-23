package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func init() { fw.RegisterTool("decide", newDecide) }

const decideSchema = `{
  "type": "object",
  "description": "Ask typed questions about a state and get one answer per question, with probabilities.",
  "properties": {
    "decider": {
      "type": "string",
      "description": "The name of a decider of the configuration. Optional when the configuration has one."
    },
    "state": {
      "type": "string",
      "description": "The text to decide about. The input of the run by default."
    },
    "questions": {
      "type": "object",
      "description": "Question id to question, at most 64. A choice maps each option to a description in criteria, a score lists 2 to 10 ordered levels in criteria, and a noul asks for the probability of yes.",
      "additionalProperties": {
        "type": "object",
        "properties": {
          "type": {"type": "string", "enum": ["choice", "noul", "score"]},
          "instructions": {"type": "string"},
          "criteria": {}
        },
        "required": ["type"]
      }
    }
  },
  "required": ["questions"],
  "additionalProperties": false
}`

const decideDescription = "Ask typed questions about a state and get one answer per question, with probabilities. " +
	"A choice picks one of named options, a score picks one of ordered levels, and a noul gives the probability of yes. " +
	"The answer never holds text beyond the options. Read the probabilities, not only the choice."

// questionID is the shape of a question id that a System One server
// accepts.
var questionID = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)

// decideTool asks a decider of the agent (docs/ROUTINES.md section 3.2).
// The routine runner asks the decider through this tool too, so a decision
// of the model and a decision of a routine look the same in the ledger.
type decideTool struct{}

// newDecide returns the tool only when the configuration has a decider.
// Without one, the tool is off, and the kernel leaves it out.
func newDecide(cfg *fw.Config) (fw.Tool, error) {
	if cfg == nil || len(cfg.Deciders) == 0 {
		return nil, nil
	}
	return decideTool{}, nil
}

// Spec implements fw.Tool.
func (decideTool) Spec() fw.ToolSpec {
	s := newSpec("decide", decideDescription, decideSchema)
	s.ReadOnly = true
	s.Idempotent = true
	s.ParallelSafe = true
	return s
}

type decideInput struct {
	Decider   string                 `json:"decider"`
	State     string                 `json:"state"`
	Questions map[string]fw.Question `json:"questions"`
}

// Call implements fw.Tool. The output is the fw.DecideResult as JSON.
func (decideTool) Call(ctx context.Context, input json.RawMessage) (fw.ToolResult, error) {
	var in decideInput
	if err := decode(input, &in); err != nil {
		return badInput("decide", err), nil
	}
	if len(in.Questions) == 0 {
		return fw.ErrorResult("decide: ask at least one question."), nil
	}
	if len(in.Questions) > fw.MaxDeciderQuestions {
		return fw.ErrorResult(fmt.Sprintf("decide: %d questions; one call asks at most %d.", len(in.Questions), fw.MaxDeciderQuestions)), nil
	}
	for id, q := range in.Questions {
		if !questionID.MatchString(id) {
			return fw.ErrorResult(fmt.Sprintf("decide: the question id %q is not 1 to 100 of letters, digits, _, ., and -.", id)), nil
		}
		if _, err := fw.QuestionOptions(q); err != nil {
			return fw.ErrorResult(fmt.Sprintf("decide: question %s: %v.", id, err)), nil
		}
	}
	a := fw.AgentFrom(ctx)
	if a == nil {
		return fw.ErrorResult("decide: no agent in the context of the call."), nil
	}
	name := strings.TrimSpace(in.Decider)
	if name == "" {
		names := a.Deciders()
		if len(names) != 1 {
			return fw.ErrorResult("decide: name a decider: " + strings.Join(names, ", ") + "."), nil
		}
		name = names[0]
	}
	state := in.State
	if state == "" {
		r, err := a.Get(ctx, fw.RunInfoFrom(ctx).RunID)
		if err != nil || r.Input == "" {
			return fw.ErrorResult("decide: give a state; this call has no run input to decide about."), nil
		}
		state = r.Input
	}
	d, err := a.DeciderNamed(ctx, name)
	if err != nil {
		return fw.ErrorResult("decide: " + err.Error()), nil
	}
	res, err := d.Decide(ctx, state, in.Questions)
	if err != nil {
		if ctx.Err() != nil {
			return canceled(ctx, "decide")
		}
		return fw.ErrorResult("decide: " + strings.TrimPrefix(err.Error(), "fw: decider: ")), nil
	}
	b, err := json.Marshal(res)
	if err != nil {
		return fw.ErrorResult("decide: " + err.Error()), nil
	}
	return fw.TextResult(string(b)), nil
}
