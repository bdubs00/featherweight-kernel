package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func init() { fw.RegisterTool("ask_user", newAskUser) }

const askUserSchema = `{
  "type": "object",
  "properties": {
    "question": {
      "type": "string",
      "description": "The question for the caller. Make it short and complete."
    },
    "schema": {
      "type": "object",
      "description": "Optional JSON schema of a form for the answer: an object whose properties are strings, numbers, integers, booleans, or strings with an enum. One property with an enum, or one boolean, shows the person one button per choice. Without a schema, the answer is free text under the key \"answer\"."
    }
  },
  "required": ["question"],
  "additionalProperties": false
}`

const askUserDescription = "Ask the caller of this run a question and wait for the answer. " +
	"Use it only when you cannot continue without information that only the caller has. " +
	"The result is the answer as JSON."

// askUserTool asks the caller through fw.AskCaller.
type askUserTool struct{}

func newAskUser(*fw.Config) (fw.Tool, error) { return askUserTool{}, nil }

// askUserInput is the input of a call: the question and the optional form
// schema.
type askUserInput struct {
	Question string          `json:"question"`
	Schema   json.RawMessage `json:"schema"`
}

// Spec implements fw.Tool.
func (askUserTool) Spec() fw.ToolSpec {
	s := newSpec("ask_user", askUserDescription, askUserSchema)
	s.ReadOnly = true
	return s
}

// Call implements fw.Tool.
func (askUserTool) Call(ctx context.Context, input json.RawMessage) (fw.ToolResult, error) {
	var in askUserInput
	if err := decode(input, &in); err != nil {
		return badInput("ask_user", err), nil
	}
	if strings.TrimSpace(in.Question) == "" {
		return fw.ErrorResult("ask_user: the question is empty."), nil
	}
	var schema []byte
	if !isNull(in.Schema) {
		if !isObject(in.Schema) {
			return fw.ErrorResult("ask_user: the schema must be a JSON object."), nil
		}
		schema = in.Schema
	}
	answer, err := fw.AskCaller(ctx, in.Question, schema)
	switch {
	case errors.Is(err, fw.ErrNoAsker):
		return fw.ErrorResult("ask_user: the caller cannot be asked here. Continue with your best judgment, or finish and say what you need."), nil
	case err != nil && ctx.Err() != nil:
		return canceled(ctx, "ask_user")
	case err != nil:
		return fw.ErrorResult(fmt.Sprintf("ask_user: no answer: %v", err)), nil
	}
	return answerResult(answer), nil
}

// Continue implements fw.Continuer. A restart cut the call off while it
// waited for an answer. The kernel offers the recorded question of the call
// and the answer that the ledger already holds, and Continue completes the
// call from them instead of asking again. It only accepts the recorded
// question of this call, so it never turns the answer of one question into
// the answer of another. Continue runs no work and asks no question: the
// kernel offers it no asker.
func (askUserTool) Continue(_ context.Context, input json.RawMessage, answered []fw.AnsweredQuestion) (fw.ToolResult, error) {
	var in askUserInput
	if err := decode(input, &in); err != nil {
		return badInput("ask_user", err), nil
	}
	if len(answered) == 0 {
		return fw.ErrorResult("ask_user: the restart of this run left no recorded answer, so the question needs the caller again."), nil
	}
	for _, q := range answered {
		if q.Request.Reason != in.Question {
			continue
		}
		if q.Decision.Decision != "allow" {
			return fw.ErrorResult("ask_user: no answer: fw: " + deniedByQuestion(&q.Decision)), nil
		}
		content := []byte(q.Decision.Content)
		if len(content) == 0 && isNull(in.Schema) {
			// A free-text question falls back to the note exactly as the
			// live path does, blank note included.
			content, _ = json.Marshal(map[string]string{"answer": q.Decision.Note})
		}
		return answerResult(content), nil
	}
	return fw.ErrorResult("ask_user: the recorded question does not match this call, so the caller must answer it again."), nil
}

// answerResult builds the result for an answer. The answer travels as text
// and, when it is valid JSON, also as structured content.
func answerResult(answer []byte) fw.ToolResult {
	res := fw.TextResult(string(answer))
	if json.Valid(answer) {
		res.Structured = json.RawMessage(answer)
	}
	return res
}

// deniedByQuestion words a denied question the way the kernel words it.
func deniedByQuestion(d *fw.ApprovalDecidedData) string {
	who := d.Decider
	if who == "" {
		who = "caller"
	}
	if d.Note == "" {
		return "denied by " + who
	}
	return "denied by " + who + ": " + d.Note
}
