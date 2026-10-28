package fw

// This file compares the steps and the checks of a routine with the calls
// that the agent made on a run (docs/ROUTINES.md sections 3.5 and 3.6).
// The runner compares a rehearsal this way at the end of each run, so a
// routine without questions gains agreement too, and Replay of package
// routine compares recorded runs the same way.

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// maxCallShown bounds an input that a comparison of the calls shows.
const maxCallShown = 200

// KernelCall reports whether a call id is the id of a call that the kernel
// made itself, not the model: a call of the routine runner, of a check of
// the definition of done, or of a check run. Such a call is no call of
// the agent, so a comparison, an observe block, and a proposal leave it
// out.
func KernelCall(callID string) bool {
	return strings.HasPrefix(callID, routineCallPrefix) || strings.HasPrefix(callID, doneCallPrefix) ||
		strings.HasPrefix(callID, checkCallPrefix)
}

// RoutineCall is one call of the agent on a run, as a comparison reads it:
// the tool, the input, the result text, and whether the call failed, with
// an error result or a command that failed.
type RoutineCall struct {
	Tool    string
	Input   json.RawMessage
	Result  string
	IsError bool
}

// ModelCalls returns the calls of the model in the events of one run, in
// order, with their results. It leaves out the calls that the kernel made
// itself (KernelCall, or a call that names a routine), and every event
// that a branch kept from its source.
func ModelCalls(events []Event) []RoutineCall {
	var out []RoutineCall
	var kept uint64
	byID := map[string]int{}
	for _, e := range events {
		if e.Seq != 0 && e.Seq <= kept {
			continue
		}
		switch e.Type {
		case EvRunQueued:
			var q RunQueuedData
			if kept == 0 && e.Decode(&q) == nil {
				kept = q.Kept
			}
		case EvToolRequested:
			var d ToolRequestedData
			if e.Decode(&d) != nil || d.Routine != "" || KernelCall(d.CallID) {
				continue
			}
			byID[d.CallID] = len(out)
			out = append(out, RoutineCall{Tool: d.Tool, Input: d.Input})
		case EvToolResponded:
			var d ToolRespondedData
			if e.Decode(&d) != nil {
				continue
			}
			if i, ok := byID[d.CallID]; ok {
				// A command that failed counts as a failed call, as in the
				// runner.
				out[i].Result, out[i].IsError = TextOf(d.Content), d.IsError || failedExit(d.ExitCode)
			}
		}
	}
	return out
}

// CallKey names a key of the input of a step or of a check, by its place
// among the steps and then the checks, from 1. Key is empty for a check
// that the agent made no call for.
type CallKey struct {
	Step int
	Tool string
	Key  string
}

// CallsCompared is what CompareRoutineCalls found on one run. Handed says
// why the routine hands the run to the agent: a find that finds no value.
// Step is the place of the first step or check that differs, from 1 among
// the steps and then the checks, or 0 when none differs. Tool is its
// tool, Reason says how it differs, and Rendered and Recorded are the
// input of the routine and of the agent, each bounded to 200 bytes. Extra
// lists the keys that only a call of the agent holds, which no step
// compares, and Unchecked the checks that the agent made no call for,
// which the comparison skips.
type CallsCompared struct {
	Handed    string
	Step      int
	Tool      string
	Reason    string
	Rendered  string
	Recorded  string
	Extra     []CallKey
	Unchecked []CallKey
}

// Agree reports whether every step and every check with a call matched.
func (c CallsCompared) Agree() bool { return c.Handed == "" && c.Step == 0 }

// differ records the first step that differs.
func (c *CallsCompared) differ(place int, tool, reason, rendered, recorded string) {
	c.Step, c.Tool, c.Reason = place, tool, reason
	c.Rendered, c.Recorded = ClipText(rendered, maxCallShown, "the text"), ClipText(recorded, maxCallShown, "the text")
}

// CompareRoutineCalls compares the steps and the checks of a routine with
// the calls of the agent on one run. data is the template data of the run
// with the answers to the questions, and the comparison adds to it the
// result of each step with as. Each step renders with data and matches the
// call of the agent to the same tool at the same place among the calls of
// that tool: each key that it compares, every key of the rendered input or
// the keys of Compare, holds the recorded value as canonical JSON, and the
// call did not fail. The result of that call is what the later templates
// read. A check afterward matches the next call of its tool in the same
// way, and the recorded result holds every contains and no lacks. An
// agent that made no such call leaves the check unchecked. A find that
// finds no value hands the run to the agent.
func CompareRoutineCalls(r Routine, data map[string]any, calls []RoutineCall) CallsCompared {
	var out CallsCompared
	byTool := map[string][]RoutineCall{}
	for _, c := range calls {
		byTool[c.Tool] = append(byTool[c.Tool], c)
	}
	seen := map[string]int{}
	// match renders a step or a check and compares it with the next call
	// of the agent to its tool. It returns the call, or false once it set
	// why the run is handed or differs.
	match := func(place int, tool, input string, keys []string) (RoutineCall, bool) {
		j := seen[tool]
		seen[tool]++
		rendered, err := RenderRoutineInput(input, data)
		switch {
		case errors.Is(err, ErrEmptyHole):
			pattern := strings.TrimSuffix(strings.TrimPrefix(err.Error(), "hole "), " empty")
			out.Handed = fmt.Sprintf("step %d %s: the routine finds no value for %s", place, tool, pattern)
			return RoutineCall{}, false
		case err != nil:
			out.differ(place, tool, err.Error(), "", "")
			return RoutineCall{}, false
		}
		agent := byTool[tool]
		if j >= len(agent) {
			out.differ(place, tool, fmt.Sprintf("the agent made %d calls of %s, and this is call %d", len(agent), tool, j+1), CanonicalJSON(rendered), "")
			return RoutineCall{}, false
		}
		c := agent[j]
		key, ok := sameKeys(rendered, c.Input, keys, func(k string) {
			out.Extra = append(out.Extra, CallKey{Step: place, Tool: tool, Key: k})
		})
		switch {
		case !ok:
			out.differ(place, tool, "the key "+key+" differs", CanonicalJSON(rendered), CanonicalJSON(c.Input))
			return RoutineCall{}, false
		case c.IsError:
			out.differ(place, tool, "the call of the agent failed", CanonicalJSON(rendered), CanonicalJSON(c.Input))
			return RoutineCall{}, false
		}
		return c, true
	}
	for i, s := range r.Steps {
		c, ok := match(i+1, s.Tool, s.Input, s.Compare)
		if !ok {
			return out
		}
		if s.As != "" {
			data[s.As] = c.Result
		}
	}
	for j, ck := range r.After {
		place := len(r.Steps) + j + 1
		if seen[ck.Tool] >= len(byTool[ck.Tool]) {
			out.Unchecked = append(out.Unchecked, CallKey{Step: place, Tool: ck.Tool})
			seen[ck.Tool]++
			continue
		}
		c, ok := match(place, ck.Tool, ck.Input, nil)
		if !ok {
			return out
		}
		for kind, list := range [][]string{ck.Contains, ck.Lacks} {
			for _, t := range list {
				text, err := RenderRoutine(t, data)
				switch {
				case errors.Is(err, ErrEmptyHole):
					out.Handed = fmt.Sprintf("check %d %s: the routine finds no value for its check", j+1, ck.Tool)
					return out
				case err != nil:
					out.differ(place, ck.Tool, err.Error(), "", "")
					return out
				case kind == 0 && !strings.Contains(c.Result, text):
					out.differ(place, ck.Tool, fmt.Sprintf("the result lacks %q", text), "", "")
					return out
				case kind == 1 && strings.Contains(c.Result, text):
					out.differ(place, ck.Tool, fmt.Sprintf("the result holds %q", text), "", "")
					return out
				}
			}
		}
	}
	return out
}

// sameKeys reports whether the recorded input holds the value of each key
// of the rendered input, or of each key of keys when it names some, as
// canonical JSON. It returns the first key that differs. It calls extra
// for each key that only the recorded input holds.
func sameKeys(rendered, recorded []byte, keys []string, extra func(string)) (string, bool) {
	var want, got map[string]json.RawMessage
	if json.Unmarshal(rendered, &want) != nil || json.Unmarshal(recorded, &got) != nil {
		ok := CanonicalJSON(rendered) == CanonicalJSON(recorded)
		return "(the whole input)", ok
	}
	for _, k := range slices.Sorted(maps.Keys(got)) {
		if _, ok := want[k]; !ok {
			extra(k)
		}
	}
	if len(keys) == 0 {
		keys = slices.Sorted(maps.Keys(want))
	}
	for _, k := range keys {
		w, inWant := want[k]
		g, inGot := got[k]
		if inWant != inGot || (inWant && CanonicalJSON(w) != CanonicalJSON(g)) {
			return k, false
		}
	}
	return "", true
}
