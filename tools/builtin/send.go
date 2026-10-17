package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func init() { fw.RegisterTool("send", newSend) }

// evSend is the type of the event that the send tool publishes.
const evSend = "fw.tool.send"

const sendSchema = `{
  "type": "object",
  "properties": {
    "subject": {
      "type": "string",
      "description": "The bus subject: tokens separated by dots, for example orders.created. Wildcards are not allowed."
    },
    "data": {
      "type": "object",
      "description": "The JSON payload of the message."
    }
  },
  "required": ["subject"],
  "additionalProperties": false
}`

const sendDescription = "Publish a message to a subject on the message bus. " +
	"Every subscriber of the subject receives it. The message is an event of type fw.tool.send that carries data."

// sendTool publishes to the agent bus.
type sendTool struct{}

func newSend(*fw.Config) (fw.Tool, error) { return sendTool{}, nil }

// Spec implements fw.Tool.
func (sendTool) Spec() fw.ToolSpec { return newSpec("send", sendDescription, sendSchema) }

// Call implements fw.Tool.
func (sendTool) Call(ctx context.Context, input json.RawMessage) (fw.ToolResult, error) {
	var in struct {
		Subject string          `json:"subject"`
		Data    json.RawMessage `json:"data"`
	}
	if err := decode(input, &in); err != nil {
		return badInput("send", err), nil
	}
	if msg := checkSubject(in.Subject); msg != "" {
		return fw.ErrorResult("send: " + msg), nil
	}
	a := fw.AgentFrom(ctx)
	if a == nil || a.Bus() == nil {
		return fw.ErrorResult("send: no agent is in this context, so there is no bus to publish to."), nil
	}
	var data any
	if !isNull(in.Data) {
		data = in.Data
	}
	ev, err := fw.NewEvent(fw.RunInfoFrom(ctx).RunID, evSend, data)
	if err != nil {
		return fw.ErrorResult(fmt.Sprintf("send: the data is not valid JSON: %v", err)), nil
	}
	if err := a.Bus().Publish(ctx, in.Subject, ev); err != nil {
		if ctx.Err() != nil {
			return canceled(ctx, "send")
		}
		return fw.ErrorResult(fmt.Sprintf("send: publish to %s failed: %v", in.Subject, err)), nil
	}
	return fw.TextResult("sent to " + in.Subject), nil
}

// checkSubject returns a message for a subject that cannot be published to,
// or "".
func checkSubject(s string) string {
	switch {
	case s == "":
		return "the subject is empty."
	case strings.ContainsAny(s, " \t\r\n"):
		return "the subject must not contain whitespace."
	case strings.ContainsAny(s, "*>"):
		return "the subject must not contain the wildcards * or >."
	}
	for _, tok := range strings.Split(s, ".") {
		if tok == "" {
			return "the subject must not have an empty token between dots."
		}
	}
	return ""
}
