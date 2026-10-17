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

// Spec implements fw.Tool.
func (askUserTool) Spec() fw.ToolSpec {
	s := newSpec("ask_user", askUserDescription, askUserSchema)
	s.ReadOnly = true
	return s
}

// Call implements fw.Tool.
func (askUserTool) Call(ctx context.Context, input json.RawMessage) (fw.ToolResult, error) {
	var in struct {
		Question string          `json:"question"`
		Schema   json.RawMessage `json:"schema"`
	}
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
	res := fw.TextResult(string(answer))
	if json.Valid(answer) {
		res.Structured = json.RawMessage(answer)
	}
	return res, nil
}
