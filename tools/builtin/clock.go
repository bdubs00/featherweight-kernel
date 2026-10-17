package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func init() { fw.RegisterTool("clock", newClock) }

const clockSchema = `{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`

const clockDescription = "Return the current time in the time zone of this run, the time since the run started, " +
	"and the working time left before its deadline. " +
	"Use it when the exact time matters, for example to timestamp a report or to plan the remaining work."

// Texts of the brief. The brief gives the date and not the time, because it
// is part of the system prompt. Every run of one day gets the same text, so
// the runs share the prompt cache.
const (
	clockBriefHeading = "# Time\n\n"
	clockBriefNote    = " When time passes between two of your responses, a system message gives the new time."
	clockDateLayout   = "Monday, 2006-01-02"
)

// clockTool tells the model the time of its run.
type clockTool struct {
	loc  *time.Location // the zone of the configuration, for a call outside a run
	zone string
}

func newClock(cfg *fw.Config) (fw.Tool, error) {
	loc, zone, err := fw.LoadZone(cfg.Time.Zone)
	if err != nil {
		return nil, fmt.Errorf("builtin: clock: %w", err)
	}
	return &clockTool{loc: loc, zone: zone}, nil
}

var _ fw.Briefer = (*clockTool)(nil)

// Spec implements fw.Tool. The tool changes nothing, so it is safe to run
// twice and beside other calls.
func (t *clockTool) Spec() fw.ToolSpec {
	s := newSpec("clock", clockDescription, clockSchema)
	s.ReadOnly = true
	s.Idempotent = true
	s.ParallelSafe = true
	return s
}

// Call implements fw.Tool. It reads the clock of the run through the agent.
// Outside a run, it reads the clock of the system in the zone of the
// configuration.
func (t *clockTool) Call(ctx context.Context, input json.RawMessage) (fw.ToolResult, error) {
	var in struct{}
	if err := decode(input, &in); err != nil {
		return badInput("clock", err), nil
	}
	a := fw.AgentFrom(ctx)
	id := fw.RunInfoFrom(ctx).RunID
	if a == nil || id == "" {
		now := fw.SystemClock{}.Now()
		return fw.TextResult(fmt.Sprintf("%s The time zone is %s. No run is in this context, so the clock cannot report the time of a run or a deadline.",
			itIs(now.In(t.loc)), t.zone)), nil
	}
	rc, err := a.RunClock(ctx, id)
	if err != nil {
		if ctx.Err() != nil {
			return canceled(ctx, "clock")
		}
		return fw.ErrorResult(fmt.Sprintf("clock: cannot read the time of run %s: %v", id, err)), nil
	}
	var sb strings.Builder
	sb.WriteString(itIs(rc.Now))
	fmt.Fprintf(&sb, " The time zone of this run is %s.", rc.Zone)
	fmt.Fprintf(&sb, " The run started at %s and has worked for %s.", fw.FormatTime(rc.Started), fw.FormatDuration(rc.Working))
	if rc.Limited {
		fmt.Fprintf(&sb, " It has %s of working time left before its deadline.", fw.FormatDuration(rc.Left))
		sb.WriteString(" Time that the run spends waiting for a person or a follow-up does not count.")
	} else {
		sb.WriteString(" No deadline applies to this run.")
	}
	return fw.TextResult(sb.String()), nil
}

// itIs writes the current time in its zone and in UTC.
func itIs(now time.Time) string {
	return fmt.Sprintf("It is %s (%s UTC).", fw.FormatTime(now), now.UTC().Format(time.RFC3339))
}

// Brief implements fw.Briefer. It returns the time section of the system
// prompt: the date of the start of the run in its zone, and how the model
// learns the exact time.
func (t *clockTool) Brief(ctx context.Context, run fw.RunInfo) (string, error) {
	a := fw.AgentFrom(ctx)
	if a == nil || run.RunID == "" {
		return "", nil
	}
	rc, err := a.RunClock(ctx, run.RunID)
	if err != nil {
		return "", fmt.Errorf("builtin: clock: time of run %s: %w", run.RunID, err)
	}
	var sb strings.Builder
	sb.WriteString(clockBriefHeading)
	fmt.Fprintf(&sb, "Today is %s, in the time zone %s.", rc.Started.Format(clockDateLayout), rc.Zone)
	sb.WriteString(" The clock tool gives the exact time, the time since this run started, and the working time left before its deadline.")
	if a.Config().Time.NoteAfter > 0 {
		sb.WriteString(clockBriefNote)
	}
	return sb.String(), nil
}
