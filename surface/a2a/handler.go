package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"net/url"
	"strings"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/push"
	"github.com/bdubs00/featherweight-kernel/fw"
)

const (
	defaultPageSize = 50
	maxPageSize     = 100
)

// handler maps A2A requests to runs. It implements a2asrv.RequestHandler.
type handler struct {
	a    *fw.Agent
	log  *slog.Logger
	push *pusher
}

var _ a2asrv.RequestHandler = (*handler)(nil)

// SendMessage starts a run or continues one, then waits until the run ends
// or needs input. With returnImmediately it returns the task at once.
func (h *handler) SendMessage(ctx context.Context, req *sdk.SendMessageRequest) (sdk.SendMessageResult, error) {
	id, _, err := h.accept(ctx, req)
	if err != nil {
		return nil, err
	}
	var r *fw.Run
	if req.Config != nil && req.Config.ReturnImmediately {
		r, err = h.a.Get(ctx, id)
	} else {
		r, err = h.a.Wait(ctx, id)
	}
	if err != nil {
		return nil, mapErr(err)
	}
	return toTask(r, historyLength(req.Config)), nil
}

// SendStreamingMessage starts a run or continues one and streams the task:
// first the task, then one status update per state change, and the output
// as an artifact before the final status.
func (h *handler) SendStreamingMessage(ctx context.Context, req *sdk.SendMessageRequest) iter.Seq2[sdk.Event, error] {
	return func(yield func(sdk.Event, error) bool) {
		id, base, err := h.accept(ctx, req)
		if err != nil {
			yield(nil, err)
			return
		}
		h.follow(ctx, id, base, false, historyLength(req.Config), yield)
	}
}

// SubscribeToTask streams a task that is not final: the task now, then its
// updates until it ends or needs input.
func (h *handler) SubscribeToTask(ctx context.Context, req *sdk.SubscribeToTaskRequest) iter.Seq2[sdk.Event, error] {
	return func(yield func(sdk.Event, error) bool) {
		if req == nil || req.ID == "" {
			yield(nil, fmt.Errorf("a2a: the task id is required: %w", sdk.ErrInvalidParams))
			return
		}
		r, err := h.a.Get(ctx, string(req.ID))
		if err != nil {
			yield(nil, mapErr(err))
			return
		}
		if r.State.Terminal() {
			yield(nil, fmt.Errorf("a2a: task %s is %s: %w", r.ID, r.State, sdk.ErrUnsupportedOperation))
			return
		}
		h.follow(ctx, r.ID, r, true, nil, yield)
	}
}

// GetTask returns the task of a run.
func (h *handler) GetTask(ctx context.Context, req *sdk.GetTaskRequest) (*sdk.Task, error) {
	if req == nil || req.ID == "" {
		return nil, fmt.Errorf("a2a: the task id is required: %w", sdk.ErrInvalidParams)
	}
	r, err := h.a.Get(ctx, string(req.ID))
	if err != nil {
		return nil, mapErr(err)
	}
	return toTask(r, req.HistoryLength), nil
}

// ListTasks lists runs, newest first. The page token is the id of the last
// task of the previous page.
func (h *handler) ListTasks(ctx context.Context, req *sdk.ListTasksRequest) (*sdk.ListTasksResponse, error) {
	if req == nil {
		req = &sdk.ListTasksRequest{}
	}
	size := req.PageSize
	if size == 0 {
		size = defaultPageSize
	}
	if size < 1 || size > maxPageSize {
		return nil, fmt.Errorf("a2a: pageSize %d is not between 1 and %d: %w", req.PageSize, maxPageSize, sdk.ErrInvalidParams)
	}
	runs, err := h.a.List(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	var match []*fw.Run
	for _, r := range runs {
		if req.ContextID != "" && contextOf(r) != req.ContextID {
			continue
		}
		if req.Status != sdk.TaskStateUnspecified && TaskState(r.State) != req.Status {
			continue
		}
		if req.StatusTimestampAfter != nil && !r.UpdatedAt.After(*req.StatusTimestampAfter) {
			continue
		}
		match = append(match, r)
	}
	start := 0
	if req.PageToken != "" {
		start = -1
		for i, r := range match {
			if r.ID == req.PageToken {
				start = i + 1
				break
			}
		}
		if start < 0 {
			return nil, fmt.Errorf("a2a: unknown page token %q: %w", req.PageToken, sdk.ErrInvalidParams)
		}
	}
	end := min(start+size, len(match))
	tasks := make([]*sdk.Task, 0, end-start)
	for _, r := range match[start:end] {
		t := toTask(r, req.HistoryLength)
		if !req.IncludeArtifacts {
			t.Artifacts = nil
		}
		tasks = append(tasks, t)
	}
	next := ""
	if end < len(match) {
		next = match[end-1].ID
	}
	return &sdk.ListTasksResponse{Tasks: tasks, TotalSize: len(match), PageSize: size, NextPageToken: next}, nil
}

// CancelTask cancels a run that is not final.
func (h *handler) CancelTask(ctx context.Context, req *sdk.CancelTaskRequest) (*sdk.Task, error) {
	if req == nil || req.ID == "" {
		return nil, fmt.Errorf("a2a: the task id is required: %w", sdk.ErrInvalidParams)
	}
	id := string(req.ID)
	r, err := h.a.Get(ctx, id)
	if err != nil {
		return nil, mapErr(err)
	}
	if r.State.Terminal() {
		return nil, fmt.Errorf("a2a: task %s is %s: %w", id, r.State, sdk.ErrTaskNotCancelable)
	}
	if err := h.a.Cancel(ctx, id); err != nil {
		return nil, mapErr(err)
	}
	if r, err = h.a.Get(ctx, id); err != nil {
		return nil, mapErr(err)
	}
	return toTask(r, nil), nil
}

// CreateTaskPushConfig stores a push config for a task, in memory.
func (h *handler) CreateTaskPushConfig(ctx context.Context, req *sdk.PushConfig) (*sdk.PushConfig, error) {
	if req == nil || req.TaskID == "" {
		return nil, fmt.Errorf("a2a: the task id is required: %w", sdk.ErrInvalidParams)
	}
	if _, err := h.a.Get(ctx, string(req.TaskID)); err != nil {
		return nil, mapErr(err)
	}
	return h.push.save(ctx, string(req.TaskID), req)
}

// GetTaskPushConfig returns one push config of a task.
func (h *handler) GetTaskPushConfig(ctx context.Context, req *sdk.GetTaskPushConfigRequest) (*sdk.PushConfig, error) {
	if _, err := h.a.Get(ctx, string(req.TaskID)); err != nil {
		return nil, mapErr(err)
	}
	c, err := h.push.store.Get(ctx, req.TaskID, req.ID)
	if errors.Is(err, push.ErrPushConfigNotFound) {
		return nil, fmt.Errorf("a2a: push config %q of task %s: %w: %w", req.ID, req.TaskID, sdk.ErrInvalidParams, err)
	}
	return c, err
}

// ListTaskPushConfigs returns the push configs of a task.
func (h *handler) ListTaskPushConfigs(ctx context.Context, req *sdk.ListTaskPushConfigRequest) (*sdk.ListTaskPushConfigResponse, error) {
	if _, err := h.a.Get(ctx, string(req.TaskID)); err != nil {
		return nil, mapErr(err)
	}
	cs, err := h.push.store.List(ctx, req.TaskID)
	if err != nil {
		return nil, err
	}
	return &sdk.ListTaskPushConfigResponse{Configs: cs}, nil
}

// DeleteTaskPushConfig removes one push config of a task.
func (h *handler) DeleteTaskPushConfig(ctx context.Context, req *sdk.DeleteTaskPushConfigRequest) error {
	if _, err := h.a.Get(ctx, string(req.TaskID)); err != nil {
		return mapErr(err)
	}
	return h.push.store.Delete(ctx, req.TaskID, req.ID)
}

// GetExtendedAgentCard is not offered: the public card says everything.
func (h *handler) GetExtendedAgentCard(context.Context, *sdk.GetExtendedAgentCardRequest) (*sdk.AgentCard, error) {
	return nil, fmt.Errorf("a2a: %w", sdk.ErrExtendedCardNotConfigured)
}

// accept turns a message into work. A message without a task id starts a
// run. A message with a task id continues that run: it decides a pending
// approval when it reads "allow" or "deny", answers a pending question, or
// else becomes a follow-up. accept returns the run id and the run status
// before the change, from which a stream follows.
func (h *handler) accept(ctx context.Context, req *sdk.SendMessageRequest) (string, *fw.Run, error) {
	if req == nil || req.Message == nil {
		return "", nil, fmt.Errorf("a2a: the message is required: %w", sdk.ErrInvalidParams)
	}
	msg := req.Message
	text := strings.TrimSpace(textOf(msg.Parts))
	if text == "" {
		return "", nil, fmt.Errorf("a2a: the message has no text part: %w", sdk.ErrInvalidParams)
	}
	if req.Config != nil && req.Config.PushConfig != nil {
		// Check the config first, so that a bad one starts no run.
		if _, err := url.ParseRequestURI(req.Config.PushConfig.URL); err != nil {
			return "", nil, fmt.Errorf("a2a: push config url %q: %w", req.Config.PushConfig.URL, sdk.ErrInvalidParams)
		}
	}
	var id string
	var before *fw.Run
	if msg.TaskID == "" {
		r, err := h.start(ctx, req, text)
		if err != nil {
			return "", nil, err
		}
		id, before = r.ID, &fw.Run{}
	} else {
		r, err := h.a.Get(ctx, string(msg.TaskID))
		if err != nil {
			return "", nil, mapErr(err)
		}
		if err := h.continueRun(ctx, r, text); err != nil {
			return "", nil, err
		}
		id, before = r.ID, r
	}
	if req.Config != nil && req.Config.PushConfig != nil {
		if _, err := h.push.save(ctx, id, req.Config.PushConfig); err != nil {
			return "", nil, err
		}
	}
	return id, before, nil
}

func (h *handler) start(ctx context.Context, req *sdk.SendMessageRequest, text string) (*fw.Run, error) {
	msg := req.Message
	opts := []fw.RunOption{fw.WithOrigin(fw.OriginA2A)}
	if agent := profileOf(req); agent != "" {
		opts = append(opts, fw.AsAgent(agent))
	}
	who := ""
	if p := fw.PrincipalFrom(ctx); p != nil {
		who = p.Subject
		opts = append(opts, fw.WithPrincipalName(who))
	}
	if msg.ID != "" {
		// A retried message returns the same run instead of a second one.
		opts = append(opts, fw.WithIdempotencyKey("a2a:"+who+":"+msg.ID))
	}
	if msg.ContextID != "" {
		opts = append(opts, fw.WithMetadata(map[string]string{metaContextID: msg.ContextID}))
	}
	if ins := metaString(req, "instructions"); ins != "" {
		opts = append(opts, fw.WithInstructions(ins))
	}
	r, err := h.a.Start(ctx, text, opts...)
	if err != nil {
		return nil, mapErr(err)
	}
	return r, nil
}

func (h *handler) continueRun(ctx context.Context, r *fw.Run, text string) error {
	if r.Pending != nil {
		if d, ok := decisionFor(r.Pending, text); ok {
			return mapErr(h.a.Decide(ctx, r.ID, d))
		}
	}
	return mapErr(h.a.Send(ctx, r.ID, fw.UserText(text), fw.OriginA2A))
}

// decisionFor reads a message as the answer to a pending approval. For an
// approval of a tool call, the message is "allow" or "deny", in any case,
// with an optional note after a colon. Any other text is a follow-up. For a
// question from a tool, which carries a schema, "deny" declines, a JSON
// object is the answer, and any other text is the answer under "answer".
func decisionFor(p *fw.Approval, text string) (fw.ApprovalDecision, bool) {
	verdict, note, ok := parseDecision(text)
	if len(p.Schema) > 0 {
		if ok && verdict == "deny" {
			return fw.ApprovalDecision{ApprovalID: p.ID, Decision: "deny", Note: note}, true
		}
		content := json.RawMessage(text)
		var obj map[string]any
		if json.Unmarshal(content, &obj) != nil {
			content, _ = json.Marshal(map[string]string{"answer": text})
		}
		return fw.ApprovalDecision{ApprovalID: p.ID, Decision: "allow", Content: content}, true
	}
	if !ok {
		return fw.ApprovalDecision{}, false
	}
	return fw.ApprovalDecision{ApprovalID: p.ID, Decision: verdict, Note: note}, true
}

// parseDecision reads "allow", "deny", "allow: note", or "deny: note".
func parseDecision(text string) (verdict, note string, ok bool) {
	word, note, _ := strings.Cut(strings.TrimSpace(text), ":")
	switch v := strings.ToLower(strings.TrimSpace(word)); v {
	case "allow", "deny":
		return v, strings.TrimSpace(note), true
	}
	return "", "", false
}

// profileOf returns the profile that a request names: the metadata key
// "agent", or a skill id under "skill" or "skillId", on the message first
// and on the request second.
func profileOf(req *sdk.SendMessageRequest) string {
	return metaString(req, "agent", "skill", "skillId")
}

// metaString returns the first string value under one of the keys, on the
// message metadata first and on the request metadata second. The key
// "instructions" carries text for this run that follows the system prompt.
func metaString(req *sdk.SendMessageRequest, keys ...string) string {
	for _, m := range []map[string]any{req.Message.Metadata, req.Metadata} {
		for _, k := range keys {
			if v, ok := m[k].(string); ok && v != "" {
				return v
			}
		}
	}
	return ""
}

func historyLength(c *sdk.SendMessageConfig) *int {
	if c == nil {
		return nil
	}
	return c.HistoryLength
}

// follow streams a run from the status run, which is the status before the
// events that follow it. With first set, the task in that status goes out
// before any event. Else the first event out is the task after the first
// ledger event. Then each state change sends a status update, a tool call
// or a tool progress message sends a working update, and a completed run
// sends its output as an artifact before the final status. The stream ends
// when the run ends or needs input.
func (h *handler) follow(ctx context.Context, id string, run *fw.Run, first bool, hist *int, yield func(sdk.Event, error) bool) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	events, err := h.a.Events(ctx, id, run.Seq+1)
	if err != nil {
		yield(nil, mapErr(err))
		return
	}
	if first {
		if !yield(toTask(run, hist), nil) || stops(run.State) {
			return
		}
	}
	sent := first
	for e := range events {
		prev := run.State
		run.Apply(e, nil)
		if !sent {
			sent = true
			if !yield(toTask(run, hist), nil) || stops(run.State) {
				return
			}
			continue
		}
		var out []sdk.Event
		switch {
		case run.State != prev:
			if a := outputArtifact(run); a != nil {
				out = append(out, artifactEvent(run, a))
			}
			out = append(out, statusEvent(run))
		case e.Type == fw.EvToolRequested:
			var d fw.ToolRequestedData
			if e.Decode(&d) == nil {
				out = append(out, workingEvent(run, "Calling the tool "+d.Tool+"."))
			}
		case e.Type == fw.EvToolProgress:
			var d fw.ToolProgressData
			if e.Decode(&d) == nil && d.Message != "" {
				out = append(out, workingEvent(run, d.Message))
			}
		}
		for _, ev := range out {
			if !yield(ev, nil) {
				return
			}
		}
		if stops(run.State) {
			return
		}
	}
}

// mapErr maps a kernel error to the A2A error that names it on the wire.
func mapErr(err error) error {
	var target error
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, fw.ErrAuthzUnavailable):
		return unavailable(err)
	case errors.Is(err, fw.ErrForbidden):
		target = sdk.ErrUnauthorized // the A2A permission-denied error
	case errors.Is(err, fw.ErrRunNotFound):
		target = sdk.ErrTaskNotFound
	case errors.Is(err, fw.ErrAgentNotFound), errors.Is(err, fw.ErrNoPendingApproval):
		target = sdk.ErrInvalidParams
	case errors.Is(err, fw.ErrClosed):
		target = sdk.ErrServerError
	default:
		target = sdk.ErrInternalError
	}
	return fmt.Errorf("a2a: %w: %w", target, err)
}
