package fw

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// t0 is a fixed start for the manual clocks of these tests: a Wednesday.
var t0 = time.Date(2026, 10, 14, 15, 2, 0, 0, time.UTC)

func TestFormatDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                                "less than a second",
		40 * time.Second:                 "40 seconds",
		time.Minute:                      "1 minute",
		45*time.Minute + 10*time.Second:  "45 minutes 10 seconds",
		2 * time.Hour:                    "2 hours",
		21*time.Hour + 3*time.Minute:     "21 hours 3 minutes",
		51*time.Hour + 5*time.Minute:     "2 days 3 hours",
		24*time.Hour + 5*time.Minute:     "1 day",
		-90 * time.Second:                "1 minute 30 seconds",
		100*time.Minute + 59*time.Second: "1 hour 40 minutes",
	} {
		if got := FormatDuration(d); got != want {
			t.Errorf("FormatDuration(%v) = %q, want %q", d, got, want)
		}
	}
	if got := FormatTime(time.Date(2026, 10, 15, 9, 12, 0, 0, mustZone(t, "America/Chicago"))); got != "Thursday 2026-10-15 09:12 CDT" {
		t.Errorf("FormatTime = %q", got)
	}
}

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, _, err := LoadZone(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestLoadZone(t *testing.T) {
	if _, name, err := LoadZone("America/Chicago"); err != nil || name != "America/Chicago" {
		t.Fatalf("America/Chicago = %q, %v", name, err)
	}
	if loc, name, err := LoadZone("UTC"); err != nil || loc != time.UTC || name != "UTC" {
		t.Fatalf("UTC = %v %q, %v", loc, name, err)
	}
	t.Setenv("TZ", "Asia/Tokyo")
	if loc, name, err := LoadZone(""); err != nil || name != "Asia/Tokyo" || loc.String() != "Asia/Tokyo" {
		t.Fatalf("local with TZ = %v %q, %v", loc, name, err)
	}
	t.Setenv("TZ", "")
	if _, name, err := LoadZone("Local"); err != nil || name != "UTC" {
		t.Fatalf("local with an empty TZ = %q, %v", name, err)
	}
	if _, _, err := LoadZone("Mars/Olympus"); err == nil || !strings.Contains(err.Error(), "Mars/Olympus") {
		t.Fatalf("unknown zone error = %v", err)
	}
}

func TestBadZoneStopsNew(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Time.Zone = "Mars/Olympus"
	if _, err := New(WithConfig(cfg), WithoutDefaults(), WithAuthn(NoAuthn{}), WithLedger(NewMemLedger()), WithBus(NewMemBus()), WithLogger(quietLogger())); err == nil || !strings.Contains(err.Error(), "time.zone") {
		t.Fatalf("New with a bad zone = %v", err)
	}
	cfg = DefaultConfig()
	cfg.Agents = map[string]AgentProfile{"reviewer": {TimeZone: "Nowhere"}}
	if _, err := New(WithConfig(cfg), WithoutDefaults(), WithAuthn(NoAuthn{}), WithLedger(NewMemLedger()), WithBus(NewMemBus()), WithLogger(quietLogger())); err == nil || !strings.Contains(err.Error(), "agents.reviewer.timeZone") {
		t.Fatalf("New with a bad profile zone = %v", err)
	}
}

func TestInlineProfileBadZone(t *testing.T) {
	a := newTestAgent(t, newScript().Model())
	_, err := a.Start(testCtx(t), "hi", WithRunProfile(AgentProfile{TimeZone: "Nowhere/City"}))
	if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "time zone") {
		t.Fatalf("Start = %v", err)
	}
}

// timed returns e with its time set to t0 plus d.
func timed(d time.Duration, e Event) Event {
	e.Time = t0.Add(d)
	return e
}

func TestWorkingTime(t *testing.T) {
	id := "r"
	evs := []Event{
		timed(0, MustEvent(id, EvRunQueued, RunQueuedData{Input: "hi"})),
		timed(time.Minute, MustEvent(id, EvModelResponded, ModelRespondedData{})),
		// Waiting for a decision for three hours does not count.
		timed(10*time.Minute, MustEvent(id, EvApprovalRequested, ApprovalRequestedData{ApprovalID: "a1"})),
		timed(3*time.Hour, MustEvent(id, EvApprovalDecided, ApprovalDecidedData{ApprovalID: "a1", Decision: "allow"})),
		timed(3*time.Hour+20*time.Minute, MustEvent(id, EvRunFinished, RunFinishedData{Outcome: OutcomeCompleted})),
		// A follow-up the next day works again.
		timed(24*time.Hour, MustEvent(id, EvMessageReceived, MessageReceivedData{Role: RoleUser})),
		timed(24*time.Hour+5*time.Minute, MustEvent(id, EvModelRequested, ModelRequestedData{})),
		// The sandbox was killed with no event, and the run resumed five
		// hours later. The gap ends the working period at the last event.
		timed(29*time.Hour+5*time.Minute, MustEvent(id, EvRunResumed, RunResumedData{})),
		timed(29*time.Hour+7*time.Minute, MustEvent(id, EvRunSuspended, RunSuspendedData{Reason: "shutdown"})),
		timed(40*time.Hour, MustEvent(id, EvRunResumed, RunResumedData{})),
	}
	now := t0.Add(40*time.Hour + 3*time.Minute)
	want := 10*time.Minute + 20*time.Minute + 5*time.Minute + 2*time.Minute + 3*time.Minute
	if got := WorkingTime(evs, now); got != want {
		t.Fatalf("WorkingTime = %v, want %v", got, want)
	}
	// A run that waits for a decision at the end does not count the wait.
	if got := WorkingTime(evs[:3], t0.Add(5*time.Hour)); got != 10*time.Minute {
		t.Fatalf("WorkingTime while waiting = %v", got)
	}
}

func clockAgent(t *testing.T, m Model, clock *ManualClock, mutate func(*Config), opts ...Option) *Agent {
	t.Helper()
	cfg := DefaultConfig()
	if mutate != nil {
		mutate(cfg)
	}
	return newTestAgent(t, m, append([]Option{WithConfig(cfg), WithClock(clock)}, opts...)...)
}

func TestEventTimesComeFromTheClock(t *testing.T) {
	clock := NewManualClock(t0)
	a := clockAgent(t, newScript(endTurn("hi")).Model(), clock, nil)
	r, err := a.Run(testCtx(t), "hi")
	mustState(t, r, err, StateCompleted)
	for _, e := range ledgerEvents(t, a.Ledger(), r.ID) {
		if !e.Time.Equal(t0) {
			t.Fatalf("%s at %v, want %v", e.Type, e.Time, t0)
		}
	}
	if !r.CreatedAt.Equal(t0) {
		t.Fatalf("createdAt = %v", r.CreatedAt)
	}
}

// The defect that the working time fixes: a follow-up after the deadline
// used to fail the run before any model call.
func TestFollowUpAfterTheDeadline(t *testing.T) {
	ctx := testCtx(t)
	clock := NewManualClock(t0)
	s := newScript(endTurn("first"), endTurn("second"))
	a := clockAgent(t, s.Model(), clock, nil)
	r, err := a.Run(ctx, "hi", WithRunBudget(Budget{Deadline: time.Hour}))
	mustState(t, r, err, StateCompleted)
	clock.Advance(3 * time.Hour)
	if err := a.Send(ctx, r.ID, UserText("again"), OriginCLI); err != nil {
		t.Fatal(err)
	}
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)
	if r.Output != "second" {
		t.Fatalf("run = %+v", r)
	}
}

func TestApprovalWaitDoesNotCountTowardTheDeadline(t *testing.T) {
	ctx := testCtx(t)
	clock := NewManualClock(t0)
	var calls atomic.Int32
	s := newScript(callTools(use("c1", "rm", `{"path":"x"}`)), endTurn("done"))
	a := clockAgent(t, s.Model(), clock, nil, WithTools(destructiveTool(&calls)))
	r, err := a.Run(ctx, "clean up", WithRunBudget(Budget{Deadline: time.Hour}))
	mustState(t, r, err, StateInputRequired)
	clock.Advance(5 * time.Hour)
	if err := a.Decide(ctx, r.ID, ApprovalDecision{ApprovalID: r.Pending.ID, Decision: "allow"}); err != nil {
		t.Fatal(err)
	}
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)
	if calls.Load() != 1 || r.Output != "done" {
		t.Fatalf("calls = %d, run = %+v", calls.Load(), r)
	}
}

func TestDeadlineCountsWork(t *testing.T) {
	ctx := testCtx(t)
	clock := NewManualClock(t0)
	slow := NewTool(ToolSpec{Name: "slow", ReadOnly: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		clock.Advance(2 * time.Hour)
		return TextResult("done after two hours"), nil
	})
	s := newScript(callTools(use("c1", "slow", `{}`)), endTurn("never"))
	a := clockAgent(t, s.Model(), clock, nil, WithTools(slow))
	r, err := a.Run(ctx, "work", WithRunBudget(Budget{Deadline: time.Hour}))
	mustState(t, r, err, StateFailed)
	if r.Error != "budget exceeded: deadline" || len(s.requests()) != 1 {
		t.Fatalf("run = %+v, requests = %d", r, len(s.requests()))
	}
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	var d BudgetExceededData
	for _, e := range evs {
		if e.Type == EvBudgetExceeded {
			_ = e.Decode(&d)
		}
	}
	if d.Limit != "deadline" || d.Value != (2*time.Hour).Seconds() {
		t.Fatalf("budget exceeded = %+v", d)
	}
}

// timeNotes returns the time notes of a run in order.
func timeNotes(evs []Event) []string {
	var out []string
	for _, e := range evs {
		var d MessageReceivedData
		if e.Type == EvMessageReceived && e.Decode(&d) == nil && d.Note == NoteTime {
			if d.Role != RoleSystem || d.Origin != OriginKernel {
				return []string{"bad note: " + string(d.Role) + " " + d.Origin}
			}
			out = append(out, TextOf(d.Content))
		}
	}
	return out
}

func TestTimeNoteAfterAGap(t *testing.T) {
	ctx := testCtx(t)
	clock := NewManualClock(t0)
	s := newScript(endTurn("first"), endTurn("second"), endTurn("third"))
	a := clockAgent(t, s.Model(), clock, nil)
	r, err := a.Run(ctx, "hi", WithRunProfile(AgentProfile{TimeZone: "America/Chicago"}), WithRunBudget(Budget{Deadline: 2 * time.Hour}))
	mustState(t, r, err, StateCompleted)

	// The next day, a follow-up gets a note with the time and the gap.
	clock.Advance(21*time.Hour + 3*time.Minute)
	if err := a.Send(ctx, r.ID, UserText("again"), OriginCLI); err != nil {
		t.Fatal(err)
	}
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)
	want := "Time note: it is now Thursday 2026-10-15 07:05 CDT, 21 hours 3 minutes after your last response. This run has 2 hours of working time left before its deadline."
	if got := timeNotes(ledgerEvents(t, a.Ledger(), r.ID)); len(got) != 1 || got[0] != want {
		t.Fatalf("notes = %q", got)
	}
	msgs := s.requests()[1].Messages
	if last := msgs[len(msgs)-1]; last.Role != RoleSystem || TextOf(last.Content) != want {
		t.Fatalf("last message = %+v", last)
	}

	// A follow-up within time.noteAfter gets no note.
	clock.Advance(5 * time.Minute)
	if err := a.Send(ctx, r.ID, UserText("more"), OriginCLI); err != nil {
		t.Fatal(err)
	}
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)
	if got := timeNotes(ledgerEvents(t, a.Ledger(), r.ID)); len(got) != 1 {
		t.Fatalf("notes = %q", got)
	}
	var started RunStartedData
	for _, e := range ledgerEvents(t, a.Ledger(), r.ID) {
		if e.Type == EvRunStarted {
			_ = e.Decode(&started)
		}
	}
	if started.TimeZone != "America/Chicago" {
		t.Fatalf("fw.run.started zone = %q", started.TimeZone)
	}
}

func TestTimeNoteAfterASlowTool(t *testing.T) {
	ctx := testCtx(t)
	clock := NewManualClock(t0)
	slow := NewTool(ToolSpec{Name: "slow", ReadOnly: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		clock.Advance(15 * time.Minute)
		return TextResult("slow done"), nil
	})
	fast := NewTool(ToolSpec{Name: "fast", ReadOnly: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		return TextResult("fast done"), nil
	})
	s := newScript(callTools(use("c1", "slow", `{}`)), callTools(use("c2", "fast", `{}`)), endTurn("done"))
	a := clockAgent(t, s.Model(), clock, func(c *Config) { c.Budget.Deadline, c.Time.Zone = 0, "UTC" }, WithTools(slow, fast))
	r, err := a.Run(ctx, "work")
	mustState(t, r, err, StateCompleted)
	notes := timeNotes(ledgerEvents(t, a.Ledger(), r.ID))
	if len(notes) != 1 || notes[0] != "Time note: it is now Wednesday 2026-10-14 15:17 UTC, 15 minutes after your last response." {
		t.Fatalf("notes = %q", notes)
	}
}

func TestTimeNotesOff(t *testing.T) {
	ctx := testCtx(t)
	clock := NewManualClock(t0)
	s := newScript(endTurn("first"), endTurn("second"))
	a := clockAgent(t, s.Model(), clock, func(c *Config) { c.Time.NoteAfter = 0 })
	r, err := a.Run(ctx, "hi")
	mustState(t, r, err, StateCompleted)
	clock.Advance(48 * time.Hour)
	if err := a.Send(ctx, r.ID, UserText("again"), OriginCLI); err != nil {
		t.Fatal(err)
	}
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)
	if got := timeNotes(ledgerEvents(t, a.Ledger(), r.ID)); len(got) != 0 {
		t.Fatalf("notes = %q", got)
	}
}

func TestRunClockAndZoneOfAChild(t *testing.T) {
	ctx := testCtx(t)
	clock := NewManualClock(t0)
	a := clockAgent(t, newScript(endTurn("parent"), endTurn("child")).Model(), clock, func(c *Config) { c.Time.Zone = "UTC" })
	p, err := a.Run(ctx, "parent", WithRunProfile(AgentProfile{TimeZone: "Asia/Tokyo"}), WithRunBudget(Budget{Deadline: time.Hour}))
	mustState(t, p, err, StateCompleted)
	c, err := a.Run(ctx, "child", WithParent(p.ID))
	mustState(t, c, err, StateCompleted)
	clock.Advance(10 * time.Minute)
	rc, err := a.RunClock(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Zone != "Asia/Tokyo" || rc.Now.Location().String() != "Asia/Tokyo" || !rc.Started.Equal(t0) || rc.Working != 0 {
		t.Fatalf("child clock = %+v", rc)
	}
	// The child inherits the deadline of its parent, and neither worked.
	if !rc.Limited || rc.Left != time.Hour {
		t.Fatalf("child left = %v, limited = %v", rc.Left, rc.Limited)
	}
	if _, err := a.RunClock(ctx, "missing"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("RunClock(missing) = %v", err)
	}
}

func TestReplayKeepsTheRecordedZone(t *testing.T) {
	ctx := testCtx(t)
	src := NewMemLedger()
	id := "recorded"
	seed(t, src, id,
		MustEvent(id, EvRunQueued, RunQueuedData{Agent: "default", Input: "hi"}),
		MustEvent(id, EvRunStarted, RunStartedData{TimeZone: "Asia/Tokyo"}),
		MustEvent(id, EvRunFinished, RunFinishedData{Outcome: OutcomeCompleted, Output: "hi"}),
	)
	a := clockAgent(t, newScript(endTurn("replayed")).Model(), NewManualClock(t0), func(c *Config) { c.Time.Zone = "UTC" })
	r, err := Replay(ctx, a, src, id)
	mustState(t, r, err, StateCompleted)
	for _, e := range ledgerEvents(t, a.Ledger(), r.ID) {
		var d RunStartedData
		if e.Type == EvRunStarted && (e.Decode(&d) != nil || d.TimeZone != "Asia/Tokyo") {
			t.Fatalf("replay zone = %q", d.TimeZone)
		}
	}
}
