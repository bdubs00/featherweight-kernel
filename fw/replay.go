package fw

// This file is replay (docs/ARCHITECTURE.md section 4.6). A replay runs the
// input of a recorded run again, and answers every tool call from the
// recorded results instead of running the tool. A diff of the two ledgers
// shows what a model change or a prompt change does.

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"sync"
)

// Replay starts a new run on a with the agent profile, the inline profile,
// the instructions, the input, and the budget of the recorded run runID in
// src, with its brief and its time zone, and blocks like Run until the new run
// reaches a terminal state or needs input. Every tool of the new run returns
// the next recorded fw.tool.responded content for its tool name instead of
// running. The policy allows every call, because no tool runs. The new run
// carries the metadata key "replayOf" with the recorded run id. Options
// apply after the recorded values.
func Replay(ctx context.Context, a *Agent, src Ledger, runID string, opts ...RunOption) (*Run, error) {
	evs, err := readAll(ctx, src, runID)
	if err != nil {
		return nil, err
	}
	var queued *RunQueuedData
	var brief, zone *string
	names := map[string]string{} // call id to tool name
	results := map[string][]ToolResult{}
	for _, e := range evs {
		switch e.Type {
		case EvRunQueued:
			if queued == nil {
				var d RunQueuedData
				if err := e.Decode(&d); err != nil {
					return nil, fmt.Errorf("fw: replay %s: %w", runID, err)
				}
				queued = &d
			}
		case EvRunStarted:
			var d RunStartedData
			if brief == nil && e.Decode(&d) == nil {
				brief, zone = &d.Brief, &d.TimeZone
			}
		case EvModelResponded:
			var d ModelRespondedData
			if e.Decode(&d) == nil {
				for _, u := range toolUses(d.Content) {
					names[u.ID] = u.Name
				}
			}
		case EvToolResponded:
			var d ToolRespondedData
			if e.Decode(&d) != nil {
				continue
			}
			if name, ok := names[d.CallID]; ok {
				results[name] = append(results[name], ToolResult{Content: d.Content, IsError: d.IsError})
			}
		}
	}
	if queued == nil {
		return nil, fmt.Errorf("%w: %s has no fw.run.queued event", ErrRunNotFound, runID)
	}

	meta := maps.Clone(queued.Metadata)
	delete(meta, "idempotencyKey")
	if meta == nil {
		meta = map[string]string{}
	}
	meta["replayOf"] = runID
	req := RunRequest{
		Agent:        queued.Agent,
		Input:        queued.Input,
		Instructions: queued.Instructions,
		Profile:      queued.Profile,
		Budget:       queued.Budget,
		Metadata:     meta,
	}
	for _, o := range opts {
		o(&req)
	}
	if req.ID == "" {
		req.ID = NewID()
	}

	rp := &replayTools{results: results}
	k := a.k
	k.mu.Lock()
	k.overrides[req.ID] = &runOverride{
		wrap:   rp.wrap,
		brief:  brief,
		zone:   zone,
		policy: PolicyFunc(func(context.Context, ToolCall) Decision { return Decision{Verdict: VerdictAllow, Reason: "replay"} }),
	}
	k.mu.Unlock()
	r, err := a.start(ctx, req)
	if err != nil || r.ID != req.ID {
		// The run did not start, or an idempotency key returned another run.
		k.mu.Lock()
		delete(k.overrides, req.ID)
		k.mu.Unlock()
		if err != nil {
			return nil, err
		}
	}
	return a.Wait(ctx, r.ID)
}

// replayTools answers tool calls from recorded results, in order, per tool
// name.
type replayTools struct {
	mu      sync.Mutex
	results map[string][]ToolResult
}

func (r *replayTools) wrap(t Tool) Tool {
	spec := t.Spec()
	return NewTool(spec, func(context.Context, json.RawMessage) (ToolResult, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		q := r.results[spec.Name]
		if len(q) == 0 {
			return ErrorResult("replay: no recorded result is left for tool " + spec.Name), nil
		}
		r.results[spec.Name] = q[1:]
		return q[0], nil
	})
}
