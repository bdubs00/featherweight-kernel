package a2a

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/bdubs00/featherweight-kernel/fw"
)

// Keys of the run metadata and of the task metadata.
const (
	metaContextID = "a2a.contextId" // run metadata: the context id the client chose
	outputID      = "output"        // the artifact id of the run output
	maxInputShown = 2000            // bytes of tool input in an approval message
)

// TaskState maps a run state to an A2A task state, as section 8.3 of the
// architecture says. A suspended run is working, because it resumes on its
// own when the process starts again.
func TaskState(s fw.RunState) sdk.TaskState {
	switch s {
	case fw.StateQueued:
		return sdk.TaskStateSubmitted
	case fw.StateRunning, fw.StateSuspended:
		return sdk.TaskStateWorking
	case fw.StateInputRequired:
		return sdk.TaskStateInputRequired
	case fw.StateCompleted:
		return sdk.TaskStateCompleted
	case fw.StateFailed:
		return sdk.TaskStateFailed
	case fw.StateCanceled:
		return sdk.TaskStateCanceled
	}
	return sdk.TaskStateWorking
}

// stops reports whether a stream ends at this run state: a final state, or
// a wait for input that only a new message can end.
func stops(s fw.RunState) bool { return s.Terminal() || s == fw.StateInputRequired }

func contextOf(r *fw.Run) string {
	if c := r.Metadata[metaContextID]; c != "" {
		return c
	}
	return r.ID
}

func taskInfo(r *fw.Run) sdk.TaskInfo {
	return sdk.TaskInfo{TaskID: sdk.TaskID(r.ID), ContextID: contextOf(r)}
}

// toTask builds the task of a run. The history holds the first input. A
// completed run with output carries one artifact with one text part.
func toTask(r *fw.Run, historyLength *int) *sdk.Task {
	t := &sdk.Task{
		ID:        sdk.TaskID(r.ID),
		ContextID: contextOf(r),
		Status:    statusOf(r),
		Metadata:  map[string]any{"agent": r.Agent, "fwState": string(r.State)},
	}
	if a := outputArtifact(r); a != nil {
		t.Artifacts = []*sdk.Artifact{a}
	}
	if r.Input != "" {
		in := sdk.NewMessageForTask(sdk.MessageRoleUser, taskInfo(r), sdk.NewTextPart(r.Input))
		in.ID = r.ID + "-input"
		t.History = []*sdk.Message{in}
	}
	if historyLength != nil && *historyLength < len(t.History) {
		n := max(*historyLength, 0)
		t.History = t.History[len(t.History)-n:]
	}
	return t
}

func outputArtifact(r *fw.Run) *sdk.Artifact {
	if r.State != fw.StateCompleted || r.Output == "" {
		return nil
	}
	return &sdk.Artifact{ID: outputID, Name: outputID, Parts: sdk.ContentParts{sdk.NewTextPart(r.Output)}}
}

func statusOf(r *fw.Run) sdk.TaskStatus {
	ts := r.UpdatedAt
	st := sdk.TaskStatus{State: TaskState(r.State), Message: statusMessage(r)}
	if !ts.IsZero() {
		st.Timestamp = &ts
	}
	return st
}

// statusMessage explains a state that needs words: the question or the
// approval of an input-required run, and the error of a failed or canceled
// run.
func statusMessage(r *fw.Run) *sdk.Message {
	var text string
	var meta map[string]any
	switch {
	case r.State == fw.StateInputRequired && r.Pending != nil:
		text, meta = approvalText(r.Pending), map[string]any{"approval": approvalMeta(r.Pending)}
	case r.State == fw.StateFailed || r.State == fw.StateCanceled:
		text = r.Error
	}
	if text == "" {
		return nil
	}
	return agentMessage(r, text, meta)
}

func agentMessage(r *fw.Run, text string, meta map[string]any) *sdk.Message {
	m := sdk.NewMessageForTask(sdk.MessageRoleAgent, taskInfo(r), sdk.NewTextPart(text))
	m.ID = fmt.Sprintf("%s-%d", r.ID, r.Seq)
	m.Metadata = meta
	return m
}

func approvalText(p *fw.Approval) string {
	if len(p.Schema) > 0 {
		return p.Reason + "\n\nReply with the answer as text or as a JSON object, or reply \"deny\" to decline."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Approval needed: the agent wants to call the tool %s", p.Tool)
	if in := compact(p.Input); in != "" {
		fmt.Fprintf(&sb, " with the input %s", in)
	}
	sb.WriteString(".")
	if p.Reason != "" {
		fmt.Fprintf(&sb, " Reason: %s.", p.Reason)
	}
	sb.WriteString(" Reply \"allow\" or \"deny\". Add a note after a colon, for example \"deny: not now\".")
	return sb.String()
}

func approvalMeta(p *fw.Approval) map[string]any {
	m := map[string]any{"id": p.ID, "tool": p.Tool}
	if p.CallID != "" {
		m["callId"] = p.CallID
	}
	if p.Reason != "" {
		m["reason"] = p.Reason
	}
	if len(p.Input) > 0 && json.Valid(p.Input) {
		m["input"] = p.Input
	}
	if len(p.Schema) > 0 && json.Valid(p.Schema) {
		m["schema"] = p.Schema
	}
	return m
}

// compact returns JSON on one line, cut to maxInputShown bytes.
func compact(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		buf.Reset()
		buf.Write(raw)
	}
	s := buf.String()
	if len(s) > maxInputShown {
		s = strings.ToValidUTF8(s[:maxInputShown], "") + "..."
	}
	return s
}

func statusEvent(r *fw.Run) *sdk.TaskStatusUpdateEvent {
	return &sdk.TaskStatusUpdateEvent{TaskID: sdk.TaskID(r.ID), ContextID: contextOf(r), Status: statusOf(r)}
}

// workingEvent reports activity inside the working state, such as a tool
// call, with a message.
func workingEvent(r *fw.Run, text string) *sdk.TaskStatusUpdateEvent {
	ev := statusEvent(r)
	ev.Status.Message = agentMessage(r, text, nil)
	return ev
}

func artifactEvent(r *fw.Run, a *sdk.Artifact) *sdk.TaskArtifactUpdateEvent {
	return &sdk.TaskArtifactUpdateEvent{TaskID: sdk.TaskID(r.ID), ContextID: contextOf(r), Artifact: a, LastChunk: true}
}

// textOf joins the text parts of a message with newlines.
func textOf(parts sdk.ContentParts) string {
	var out []string
	for _, p := range parts {
		if p == nil {
			continue
		}
		if t, ok := p.Content.(sdk.Text); ok {
			out = append(out, string(t))
		}
	}
	return strings.Join(out, "\n")
}
