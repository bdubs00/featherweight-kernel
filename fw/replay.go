package fw

// This file is replay (docs/ARCHITECTURE.md section 4.6). A replay runs the
// input of a recorded run again, and answers every tool call from the
// recorded results instead of running the tool. A diff of the two ledgers
// shows what a model change or a prompt change does.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	// replayMaxInputs lists at most this many remaining recorded inputs of
	// a tool in a divergence result.
	replayMaxInputs = 8
	// replayMaxInputShown bounds the input text that a divergence result
	// shows, so a large input cannot flood the context.
	replayMaxInputShown = 200
)

// Replay starts a new run on a with the agent profile, the inline profile,
// the instructions, the input, and the budget of the recorded run runID in
// src, with its brief and its time zone, and blocks like Run until the new
// run reaches a terminal state or needs input. Every tool of the new run
// returns the next recorded fw.tool.responded content for a call of the same
// tool name with the same canonical JSON input, instead of running the tool.
// Repeated identical calls consume one recorded result each, in the order
// that the ledger holds them. A call that no recorded call matches gets an
// explicit error result that names the call and what the ledger holds. The
// policy allows every call, because no tool runs. The new run carries the
// metadata key "replayOf" with the recorded run id. Options apply after the
// recorded values.
func Replay(ctx context.Context, a *Agent, src Ledger, runID string, opts ...RunOption) (*Run, error) {
	evs, err := readAll(ctx, src, runID)
	if err != nil {
		return nil, err
	}
	var queued *RunQueuedData
	var brief, runContext, zone *string
	calls := map[string]replayCall{} // call id to tool name and canonical input
	results := map[replayCall][]ToolResult{}
	stray := map[string]int{} // recorded results whose call input the ledger does not hold
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
				brief, runContext, zone = &d.Brief, &d.Context, &d.TimeZone
			}
		case EvModelResponded:
			var d ModelRespondedData
			if e.Decode(&d) == nil {
				for _, u := range toolUses(d.Content) {
					calls[u.ID] = replayCall{name: u.Name, input: canonicalJSON(u.Input)}
				}
			}
		case EvToolRequested:
			var d ToolRequestedData
			if e.Decode(&d) == nil {
				calls[d.CallID] = replayCall{name: d.Tool, input: canonicalJSON(d.Input)}
			}
		case EvToolResponded:
			var d ToolRespondedData
			if e.Decode(&d) != nil {
				continue
			}
			res := ToolResult{Content: d.Content, IsError: d.IsError, ExitCode: d.ExitCode}
			if c, ok := calls[d.CallID]; ok {
				results[c] = append(results[c], res)
				continue
			}
			if d.Tool != "" {
				stray[d.Tool]++
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

	rp := &replayTools{results: results, stray: stray}
	k := a.k
	k.mu.Lock()
	k.overrides[req.ID] = &runOverride{
		wrap:    rp.wrap,
		brief:   brief,
		context: runContext,
		zone:    zone,
		policy:  PolicyFunc(func(context.Context, ToolCall) Decision { return Decision{Verdict: VerdictAllow, Reason: "replay"} }),
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

// replayCall identifies one recorded tool call: the tool name and the
// canonical JSON of its input. Two calls match in a replay when their
// replayCall values are equal.
type replayCall struct {
	name  string
	input string // canonical JSON, or raw bytes behind a marker byte
}

// canonicalJSON returns one canonical serialization of raw: object keys in
// sorted order, no insignificant whitespace, and number literals unchanged,
// so a value such as 9007199254740993 does not lose precision through a
// float64. An empty input counts as an empty object. Invalid JSON returns
// the raw bytes behind a marker byte that no canonical form contains, so an
// unparsable input matches only itself. Both recorded and live inputs go
// through this function, so the escaping choices of the encoder do not
// affect matching either.
func canonicalJSON(raw []byte) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "{}"
	}
	if !json.Valid(trimmed) {
		return "\x00" + string(trimmed)
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "\x00" + string(trimmed)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "\x00" + string(trimmed)
	}
	return string(b)
}

// replayShow renders a canonical input for a result to the model. A long
// input is cut at a rune boundary, and an input that was not valid JSON
// loses its marker byte.
func replayShow(input string) string {
	if len(input) > 0 && input[0] == 0 {
		input = input[1:]
	}
	if len(input) <= replayMaxInputShown {
		return input
	}
	cut := input[:replayMaxInputShown]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "... (cut)"
}

// replayTools answers tool calls from recorded results. A recorded result
// matches a call only when the tool name and the canonical JSON input of the
// call both match. Repeated identical calls consume one recorded result
// each, in the order that the ledger holds them. A call that no recorded
// result matches gets an explicit divergence result. No live tool ever
// runs.
type replayTools struct {
	mu      sync.Mutex
	results map[replayCall][]ToolResult
	stray   map[string]int // recorded results whose input is not recorded, per tool
}

func (r *replayTools) wrap(t Tool) Tool {
	spec := t.Spec()
	// A replay lookup is an in-memory read of a fixture, and identical calls
	// must consume their recorded results in model call order, not in the
	// order that parallel goroutines reach the queue. A spec without
	// ParallelSafe makes the kernel batch such calls one at a time, in the
	// order of the model response. The spec that the replayed model sees
	// therefore drops the parallel-safe hint of the live tool, which no
	// replay call uses, because no tool runs.
	spec.ParallelSafe = false
	return NewTool(spec, func(_ context.Context, input json.RawMessage) (ToolResult, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		call := replayCall{name: spec.Name, input: canonicalJSON(input)}
		if q := r.results[call]; len(q) > 0 {
			r.results[call] = q[1:]
			return q[0], nil
		}
		return ErrorResult(r.divergence(spec.Name, call.input)), nil
	})
}

// divergence builds the result for a call that no recorded call matches. It
// names the call, the recorded results of the tool that remain unusable, and
// the recorded results whose input the ledger does not hold. The caller must
// hold r.mu.
func (r *replayTools) divergence(name, input string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "replay: no recorded result is left for tool %s with input %s.", name, replayShow(input))
	sb.WriteString(" Replay matches a recorded result only when it recorded a call of the same tool with the same input.")
	var inputs []string // canonical inputs of this tool with results left, sorted below
	perInput := map[string]int{}
	left := 0
	for key, q := range r.results {
		if key.name != name || len(q) == 0 {
			continue
		}
		left += len(q)
		perInput[key.input] += len(q)
	}
	if left > 0 {
		// Sort before bounding, so a long list shows a stable subset and
		// the result does not depend on the map order.
		inputs = slices.Sorted(maps.Keys(perInput))
		if len(inputs) > replayMaxInputs {
			inputs = inputs[:replayMaxInputs]
		}
		shown := make([]string, len(inputs))
		for i, in := range inputs {
			shown[i] = fmt.Sprintf("%s (left: %d)", replayShow(in), perInput[in])
		}
		fmt.Fprintf(&sb, " The recorded run made %d call(s) of this tool with other inputs: %s.", left, strings.Join(shown, ", "))
	} else {
		sb.WriteString(" The recorded run has no unused result for this tool.")
	}
	if n := r.stray[name]; n > 0 {
		fmt.Fprintf(&sb, " The ledger also holds %d recorded result(s) of this tool whose call input is not recorded, so replay cannot tell which call they belong to.", n)
	}
	sb.WriteString(" This call did not run, and no live tool ran.")
	return sb.String()
}
