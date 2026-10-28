//go:build !lean || propose

package routine

import (
	"context"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// LeaveOneOutResult counts how a proposal held up on the runs that it did
// not see. For each run, LeaveOneOut proposes a routine from the other
// runs and replays it on the one that it left out. Incomplete counts the
// runs whose proposal had a free hole or a problem, so nothing was
// replayed on them. Diverged lists the runs that did not match, with the
// first step that differs.
type LeaveOneOutResult struct {
	Runs       int          `json:"runs"`
	Matched    int          `json:"matched"`
	Handed     int          `json:"handed"`
	Diverged   []Divergence `json:"diverged,omitempty"`
	Unreadable int          `json:"unreadable"`
	Incomplete int          `json:"incomplete"`
}

// Holds is true when every run that was replayed matched or was handed to
// the agent, and every proposal was complete.
func (r LeaveOneOutResult) Holds() bool {
	return r.Runs > 0 && len(r.Diverged) == 0 && r.Unreadable == 0 && r.Incomplete == 0
}

// LeaveOneOut checks that a routine proposed from runs works on a run that
// it did not see (docs/KNOWHOW.md section 4.8). It is a function of the
// events alone, like Propose and Replay without a decider.
func LeaveOneOut(ctx context.Context, runs [][]fw.Event, opts ProposeOptions) LeaveOneOutResult {
	res := LeaveOneOutResult{Runs: len(runs)}
	for i := range runs {
		if ctx.Err() != nil {
			break
		}
		rest := make([][]fw.Event, 0, len(runs)-1)
		rest = append(rest, runs[:i]...)
		rest = append(rest, runs[i+1:]...)
		r, prop := Propose(rest, opts)
		if !prop.Complete {
			res.Incomplete++
			continue
		}
		rep := Replay(ctx, r, runs[i:i+1], ReplayOptions{})
		res.Matched += len(rep.Matched)
		res.Handed += len(rep.Handed)
		res.Diverged = append(res.Diverged, rep.Diverged...)
		res.Unreadable += len(rep.Unreadable)
	}
	return res
}
