// Package routine reads recorded runs against routines (docs/ROUTINES.md
// section 3.6). Replay checks a routine against the runs that an agent
// made, and Propose makes a routine from them. Both are functions of the
// events alone; only Replay with a decider makes a call. The verb fw
// routine of package cli runs them over a ledger. Propose works over many
// runs, which is the work of the workbench or of a librarian, not of a
// worker: a build with the tag lean leaves it out, and the tag propose
// brings it back.
package routine

import (
	"encoding/json"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// maxShown bounds an input that a result shows.
const maxShown = 200

// bound cuts a text that a result shows (fw.ClipText).
func bound(s string) string { return fw.ClipText(s, maxShown, "the text") }

// record is what Replay and Propose read from the events of one run.
type record struct {
	id     string
	agent  string
	input  string
	meta   map[string]string
	output string
	done   bool // the last fw.run.finished has the outcome completed
	calls  []call
	events []fw.Event
}

// call is one tool call of the model, with its result.
type call struct {
	tool      string
	input     json.RawMessage
	result    string
	isError   bool
	responded bool
}

// readRun reads the events of one run. The calls of the routine runner
// (fw.ToolRequestedData.Routine) are not calls of the agent, and neither
// are the events that a branch kept from its source.
func readRun(events []fw.Event) record {
	rec := record{events: events}
	var kept uint64
	byID := map[string]int{}
	for _, e := range events {
		if e.Seq != 0 && e.Seq <= kept {
			continue
		}
		switch e.Type {
		case fw.EvRunQueued:
			var q fw.RunQueuedData
			if e.Decode(&q) == nil && rec.id == "" {
				rec.id, rec.agent, rec.input, rec.meta, kept = e.Subject, q.Agent, q.Input, q.Metadata, q.Kept
			}
		case fw.EvToolRequested:
			var d fw.ToolRequestedData
			if e.Decode(&d) != nil || d.Routine != "" {
				continue
			}
			byID[d.CallID] = len(rec.calls)
			rec.calls = append(rec.calls, call{tool: d.Tool, input: d.Input})
		case fw.EvToolResponded:
			var d fw.ToolRespondedData
			if e.Decode(&d) != nil {
				continue
			}
			if i, ok := byID[d.CallID]; ok {
				c := &rec.calls[i]
				c.result, c.isError, c.responded = fw.TextOf(d.Content), d.IsError, true
			}
		case fw.EvRunFinished:
			var d fw.RunFinishedData
			if e.Decode(&d) == nil {
				rec.done, rec.output = d.Outcome == fw.OutcomeCompleted, d.Output
			}
		case fw.EvMessageReceived:
			rec.done = false
		}
	}
	if rec.id == "" && len(events) > 0 {
		rec.id = events[0].Subject
	}
	return rec
}

// callsOf returns the calls of the agent of one tool, in order.
func (r record) callsOf(tool string) []call {
	var out []call
	for _, c := range r.calls {
		if c.tool == tool {
			out = append(out, c)
		}
	}
	return out
}
