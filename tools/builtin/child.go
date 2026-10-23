package builtin

// This file holds what spawn_task and await_task share: the wait loop that
// relays the questions of a child run to the caller of this run, the check
// that a run descends from this run, and the result that describes a
// finished child.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// approvalRelaySchema is the form that the caller of the parent answers
// when a child run waits for an approval of a tool call.
const approvalRelaySchema = `{"type":"object","properties":{"decision":{"type":"string","enum":["allow","deny"]},"note":{"type":"string"}},"required":["decision"]}`

// maxRelayInput bounds the tool input that a relayed approval question shows.
const maxRelayInput = 4 << 10

// maxAncestors bounds the walk from a run up to its ancestors.
const maxAncestors = 64

// maxSummary bounds the output summary of a child in a list.
const maxSummary = 200

// childStatus is the structured part of a result about a child run.
type childStatus struct {
	RunID     string `json:"runId"`
	State     string `json:"state"`
	Turns     int    `json:"turns"`
	ToolCalls int    `json:"toolCalls"`
	Error     string `json:"error,omitempty"`
}

func statusOf(r *fw.Run) childStatus {
	return childStatus{
		RunID:     r.ID,
		State:     string(r.State),
		Turns:     r.Usage.Turns,
		ToolCalls: r.Usage.ToolCalls,
		Error:     r.Error,
	}
}

// structured returns the status of a run as the Structured field of a result.
func structured(r *fw.Run) json.RawMessage {
	b, _ := json.Marshal(statusOf(r))
	return b
}

// waitChild blocks until the child run is terminal. Each time the child
// needs input, it relays the question to the caller of this run and records
// the answer on the child, so a child never hangs on a question that nobody
// sees. When ctx ends first, it returns the status so far and the context
// error, and the caller decides what happens to the child.
func waitChild(ctx context.Context, a *fw.Agent, tool, id string) (*fw.Run, error) {
	parent := fw.RunInfoFrom(ctx).RunID
	relayed := ""
	for {
		r, err := a.WaitFor(ctx, id, func(r *fw.Run) bool {
			return r.State.Terminal() || (r.Pending != nil && r.Pending.ID != relayed)
		})
		if err != nil || r.State.Terminal() {
			return r, err
		}
		relayed = r.Pending.ID
		if err := relay(ctx, a, tool, parent, r); err != nil {
			return r, err
		}
	}
}

// relay asks the caller of the parent run the pending question of a child
// and records the answer on the child. A question from a tool, which
// carries a schema, passes through unchanged. An approval of a tool call
// becomes a question that names the child, the tool, the input, and the
// reason, with a form that holds a decision and a note. When the caller
// cannot be asked, or declines, the child's approval is denied with a note
// that says why.
func relay(ctx context.Context, a *fw.Agent, tool, parent string, child *fw.Run) error {
	p := child.Pending
	d := fw.ApprovalDecision{ApprovalID: p.ID, Decider: parent, DeciderKind: fw.ActorParent}
	question := len(p.Schema) > 0
	var answer []byte
	var err error
	if question {
		answer, err = fw.AskCaller(ctx, p.Reason, p.Schema)
	} else {
		answer, err = fw.AskCaller(ctx, approvalQuestion(child), []byte(approvalRelaySchema))
	}
	switch {
	case err == nil && question:
		d.Decision, d.Content = "allow", answer
	case err == nil:
		d.Decision, d.Note = decisionOf(answer)
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, fw.ErrNoAsker):
		d.Decision = "deny"
		d.Note = fmt.Sprintf("run %s relayed this to its caller, but it has no caller to ask", parent)
	default:
		d.Decision = "deny"
		d.Note = fmt.Sprintf("run %s relayed this to its caller: %s", parent, strings.TrimPrefix(err.Error(), "fw: "))
	}
	err = a.Decide(ctx, child.ID, d)
	switch {
	case errors.Is(err, fw.ErrNoPendingApproval):
		// Someone else answered the child first.
		return nil
	case errors.Is(err, fw.ErrInvalidRequest):
		bad := d.Decision
		d.Decision = "deny"
		d.Note = fmt.Sprintf("the caller of run %s answered %q, which is not allow or deny", parent, bad)
		err = a.Decide(ctx, child.ID, d)
	}
	if err != nil {
		return fmt.Errorf("builtin: %s: decide for child run %s: %w", tool, child.ID, err)
	}
	return nil
}

// decisionOf reads the decision and the note of a relayed approval answer.
// An empty answer means allow: the caller allowed the question on the
// parent, and that is the only way an answer comes back at all.
func decisionOf(answer []byte) (decision, note string) {
	var v struct {
		Decision string `json:"decision"`
		Answer   string `json:"answer"`
		Note     string `json:"note"`
	}
	_ = json.Unmarshal(answer, &v)
	decision = strings.TrimSpace(v.Decision)
	if decision == "" {
		decision = strings.TrimSpace(v.Answer)
	}
	if decision == "" {
		decision = "allow"
	}
	return decision, v.Note
}

// approvalQuestion renders the pending approval of a child as a question for
// the caller of the parent.
func approvalQuestion(r *fw.Run) string {
	p := r.Pending
	var sb strings.Builder
	fmt.Fprintf(&sb, "Child run %s (agent %s) waits for an approval to call the tool %s.", r.ID, r.Agent, p.Tool)
	if p.Reason != "" {
		fmt.Fprintf(&sb, "\nReason: %s", p.Reason)
	}
	if len(p.Input) > 0 {
		in, cut := truncate(string(p.Input), maxRelayInput)
		fmt.Fprintf(&sb, "\nInput: %s", in)
		if cut {
			fmt.Fprintf(&sb, " [... cut; the input is %d bytes]", len(p.Input))
		}
	}
	sb.WriteString("\nAnswer allow or deny. The note goes back to the child.")
	return sb.String()
}

// isDescendant reports whether the run with that id is a child of ancestor,
// or a child of one of its children, at any depth. It returns
// fw.ErrRunNotFound when the run does not exist.
func isDescendant(ctx context.Context, a *fw.Agent, id, ancestor string) (bool, error) {
	cur := id
	for range maxAncestors {
		r, err := a.Get(ctx, cur)
		if err != nil {
			return false, err
		}
		if r.Parent == "" {
			return false, nil
		}
		if r.Parent == ancestor {
			return true, nil
		}
		cur = r.Parent
	}
	return false, nil
}

// childResult describes a child run that reached a terminal state. The text
// is the output of a completed child. A failed or canceled child gives an
// error result that names the state and the error. Structured holds the
// run id, the state, the turns, the tool calls, and the error.
func childResult(tool string, r *fw.Run) fw.ToolResult {
	var res fw.ToolResult
	switch r.State {
	case fw.StateCompleted:
		out := r.Output
		if strings.TrimSpace(out) == "" {
			out = fmt.Sprintf("Child run %s completed without an answer.", r.ID)
		}
		res = fw.TextResult(out)
	case fw.StateCanceled:
		msg := fmt.Sprintf("%s: child run %s was canceled.", tool, r.ID)
		if e := strings.TrimPrefix(r.Error, "canceled: "); e != "" && e != "canceled" {
			// A stop of the tree says what ran out.
			msg = fmt.Sprintf("%s: child run %s was canceled: %s.", tool, r.ID, e)
		}
		res = fw.ErrorResult(msg)
	case fw.StateFailed:
		msg := fmt.Sprintf("%s: child run %s failed: %s", tool, r.ID, r.Error)
		if o := strings.TrimSpace(r.Output); o != "" {
			msg += "\n\nIts output before the failure:\n" + o
		}
		res = fw.ErrorResult(msg)
	default:
		res = fw.ErrorResult(fmt.Sprintf("%s: child run %s is in state %s and did not finish.", tool, r.ID, r.State))
	}
	res.Structured = structured(r)
	return res
}

// summary returns the first line or so of an output for a list.
func summary(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	out, cut := truncate(s, maxSummary)
	if cut {
		out += " ..."
	}
	return out
}
