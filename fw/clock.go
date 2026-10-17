package fw

// This file is the time of an agent (docs/ARCHITECTURE.md section 5.8): the
// Clock plug, the time zone of a run, the working time that the deadline
// counts, and the run clock that the clock tool reads. Every time comes from
// the wall-clock times in the ledger and from the Clock, never from the
// uptime of the process, so the values stay right after a restart, after a
// resume in a fresh sandbox, and after a restore that moves the clock.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Clock tells the time. The kernel reads every time through the Clock of
// the agent: the time of each event, the deadline, and the time notes. A
// test replaces it with a ManualClock and moves time forward without
// sleeping.
type Clock interface {
	Now() time.Time
}

// SystemClock is the default Clock. It reads the clock of the system.
type SystemClock struct{}

// Now implements Clock.
func (SystemClock) Now() time.Time { return time.Now() }

// ClockFunc adapts a function to Clock.
type ClockFunc func() time.Time

// Now implements Clock.
func (f ClockFunc) Now() time.Time { return f() }

// ManualClock is a Clock that moves only when it is told to. It is safe for
// concurrent use.
type ManualClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewManualClock returns a ManualClock that starts at t.
func NewManualClock(t time.Time) *ManualClock { return &ManualClock{now: t} }

// Now implements Clock.
func (c *ManualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d.
func (c *ManualClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// Set moves the clock to t.
func (c *ManualClock) Set(t time.Time) {
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
}

// WithClock replaces the clock of the agent. The default is SystemClock.
func WithClock(c Clock) Option { return func(o *options) { o.clock = c } }

// Clock returns the clock of the agent.
func (a *Agent) Clock() Clock { return a.clock }

// now is the current time from the clock of the agent.
func (a *Agent) now() time.Time { return a.clock.Now() }

// DefaultNoteAfter is the default of time.noteAfter.
const DefaultNoteAfter = 10 * time.Minute

// TimeConfig configures the time of the agent.
//
// Zone is the time zone in which the agent reads and writes times: an IANA
// name such as "America/Chicago", "UTC", or "Local". Empty means "Local":
// the zone of TZ, else the zone of /etc/localtime, else UTC. A sandbox under
// ax sets neither, so its zone is UTC unless FW_TIME_ZONE names one. A
// profile overrides the zone with AgentProfile.TimeZone.
//
// NoteAfter is the gap that starts a time note. Before a model call, when
// more than NoteAfter passed since the latest model response of the run,
// the kernel adds a system message that gives the time and the gap. Zero
// turns the notes off.
type TimeConfig struct {
	Zone      string        `yaml:"zone,omitempty" json:"zone,omitempty"`
	NoteAfter time.Duration `yaml:"noteAfter" json:"noteAfter"`
}

// LoadZone resolves the name of a time zone. It returns the location and
// the name to show and record: the IANA name when it is known, so that a
// resume on another host reads the same zone. "" and "Local" resolve to the
// zone of the system. The binary embeds the zone database, so a name
// resolves in an image without zone files.
func LoadZone(name string) (*time.Location, string, error) {
	name = strings.TrimSpace(name)
	switch name {
	case "", "Local", "local":
		local := localZoneName()
		if local != "Local" {
			if loc, err := time.LoadLocation(local); err == nil {
				return loc, local, nil
			}
		}
		return time.Local, local, nil
	case "UTC", "utc", "Z":
		return time.UTC, "UTC", nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, "", fmt.Errorf("fw: unknown time zone %q: use an IANA name such as America/Chicago, or UTC: %w", name, err)
	}
	return loc, loc.String(), nil
}

// localZoneName names the zone of the system: the value of TZ, else the
// target of /etc/localtime under a zoneinfo directory, else UTC when the
// system zone has no offset, else "Local".
func localZoneName() string {
	if tz, ok := os.LookupEnv("TZ"); ok {
		tz = strings.TrimPrefix(tz, ":")
		if tz == "" {
			return "UTC"
		}
		if _, after, ok := strings.Cut(tz, "zoneinfo/"); ok {
			return after
		}
		if !strings.HasPrefix(tz, "/") {
			return tz
		}
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if _, after, ok := strings.Cut(target, "zoneinfo/"); ok && after != "" {
			return after
		}
	}
	if _, off := time.Now().In(time.Local).Zone(); off == 0 {
		return "UTC"
	}
	return "Local"
}

// zone returns the location of a zone name from the cache of the agent.
// An unknown name falls back to the zone of the configuration.
func (a *Agent) zone(name string) (*time.Location, string) {
	a.zoneMu.Lock()
	defer a.zoneMu.Unlock()
	if z, ok := a.zones[name]; ok {
		return z.loc, z.name
	}
	loc, shown, err := LoadZone(name)
	if err != nil {
		a.log.Warn("time zone not available, using the configured zone", "zone", name, "error", err)
		if loc, shown, err = LoadZone(a.cfg.Time.Zone); err != nil {
			loc, shown = time.UTC, "UTC"
		}
	}
	if a.zones == nil {
		a.zones = map[string]zoneEntry{}
	}
	a.zones[name] = zoneEntry{loc: loc, name: shown}
	return loc, shown
}

type zoneEntry struct {
	loc  *time.Location
	name string
}

// FormatTime writes a time for a model: the weekday, the date, the time to
// the minute, and the zone, for example "Thursday 2026-10-15 09:12 CDT".
func FormatTime(t time.Time) string { return t.Format("Monday 2006-01-02 15:04 MST") }

// FormatDuration writes a duration in words with its two largest units, for
// example "2 days 3 hours", "21 hours 5 minutes", or "40 seconds".
func FormatDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	if d < time.Second {
		return "less than a second"
	}
	units := []struct {
		size time.Duration
		name string
	}{
		{24 * time.Hour, "day"},
		{time.Hour, "hour"},
		{time.Minute, "minute"},
		{time.Second, "second"},
	}
	var parts []string
	for _, u := range units {
		n := d / u.size
		d -= n * u.size
		if n == 0 {
			if len(parts) > 0 {
				break
			}
			continue
		}
		name := u.name
		if n != 1 {
			name += "s"
		}
		parts = append(parts, fmt.Sprintf("%d %s", n, name))
		if len(parts) == 2 {
			break
		}
	}
	return strings.Join(parts, " ")
}

// WorkingTime returns the time that a run spent working, from its events
// and the current time. A run works from its input, a follow-up message,
// an approval decision, or a resume, until it finishes, waits for a
// decision or an answer, or is suspended. A gap with no event before a
// fw.run.resumed is a process that stopped without a word, such as a
// sandbox killed on suspend, and it ends the working period at the last
// event before the gap. The deadline of a run counts this time, so the time
// that a run waits for a person, for a follow-up, or in a suspended sandbox
// does not count.
func WorkingTime(events []Event, now time.Time) time.Duration {
	var total time.Duration
	var start, prev time.Time
	working, terminal, suspended := false, false, false
	open := map[string]bool{}
	span := func(from, to time.Time) {
		if d := to.Sub(from); d > 0 {
			total += d
		}
	}
	for _, e := range events {
		if e.Type == EvRunResumed && working {
			span(start, prev)
			working = false
		}
		switch e.Type {
		case EvRunQueued, EvMessageReceived:
			terminal = false
		case EvApprovalRequested:
			var d ApprovalRequestedData
			if e.Decode(&d) == nil {
				open[d.ApprovalID] = true
			}
		case EvApprovalDecided:
			var d ApprovalDecidedData
			if e.Decode(&d) == nil {
				delete(open, d.ApprovalID)
			}
		case EvRunFinished:
			terminal = true
			clear(open)
		case EvRunSuspended:
			suspended = true
		case EvRunResumed:
			suspended = false
		}
		busy := !terminal && !suspended && len(open) == 0
		switch {
		case busy && !working:
			start, working = e.Time, true
		case !busy && working:
			span(start, e.Time)
			working = false
		}
		prev = e.Time
	}
	if working {
		span(start, now)
	}
	return total
}

// RunClock is the time of one run.
type RunClock struct {
	Now     time.Time     // the current time, in the zone of the run
	Started time.Time     // when the run was queued, in the zone of the run
	Zone    string        // the name of the zone of the run, such as "America/Chicago"
	Working time.Duration // the working time of the run so far (see WorkingTime)
	// Left is the working time that remains before the run, or one of its
	// ancestors, reaches its deadline. It is zero when Limited is false,
	// because no deadline applies.
	Left    time.Duration
	Limited bool
}

// RunClock returns the time of a run. With a principal in ctx, the
// principal needs ActionRunView on the run.
func (a *Agent) RunClock(ctx context.Context, id string) (RunClock, error) {
	rs, err := a.k.lookup(id)
	if err != nil {
		return RunClock{}, err
	}
	if err := a.Authorize(ctx, ActionRunView, Target{Agent: rs.agent, Run: rs.snapshot()}); err != nil {
		return RunClock{}, err
	}
	evs, err := a.eventsOf(ctx, rs)
	if err != nil {
		return RunClock{}, err
	}
	now := a.now()
	loc, name := a.zone(a.zoneNameIn(ctx, rs, evs))
	rc := RunClock{
		Now:     now.In(loc),
		Started: rs.createdAt().In(loc),
		Zone:    name,
		Working: WorkingTime(evs, now),
	}
	rc.Left, rc.Limited = a.timeLeft(ctx, rs, now)
	return rc, nil
}

// zoneNameOf returns the zone of a run: the zone that fw.run.started
// recorded, else the zone of its profile, else the zone of its parent, else
// the zone of the configuration.
func (a *Agent) zoneNameOf(ctx context.Context, rs *runState) string {
	evs, err := a.eventsOf(ctx, rs)
	if err != nil {
		evs = nil
	}
	return a.zoneNameIn(ctx, rs, evs)
}

// zoneNameIn is zoneNameOf with the events of the run already read.
func (a *Agent) zoneNameIn(ctx context.Context, rs *runState, evs []Event) string {
	for _, e := range evs {
		if e.Type != EvRunStarted {
			continue
		}
		var d RunStartedData
		if e.Decode(&d) == nil && d.TimeZone != "" {
			return d.TimeZone
		}
		break
	}
	if z := a.profileOf(rs.queuedData()).TimeZone; z != "" {
		return z
	}
	if rs.parent != "" {
		if p, err := a.k.lookup(rs.parent); err == nil {
			return a.zoneNameOf(ctx, p)
		}
	}
	return a.cfg.Time.Zone
}

// eventsOf returns the events of a run: the log in memory while a driver
// holds it, else the ledger.
func (a *Agent) eventsOf(ctx context.Context, rs *runState) ([]Event, error) {
	rs.emitMu.Lock()
	if rs.eventsRef > 0 {
		evs := rs.events[:len(rs.events):len(rs.events)]
		rs.emitMu.Unlock()
		return evs, nil
	}
	rs.emitMu.Unlock()
	return readAll(ctx, a.ledger, rs.id)
}

// timeLeft returns the working time that remains before the run or one of
// its ancestors reaches its deadline, and whether any deadline applies.
func (a *Agent) timeLeft(ctx context.Context, rs *runState, now time.Time) (time.Duration, bool) {
	var left time.Duration
	limited := false
	for _, r := range a.lineage(rs) {
		b := a.ownBudget(r)
		if b.Deadline <= 0 {
			continue
		}
		evs, err := a.eventsOf(ctx, r)
		if err != nil {
			a.log.Warn("read run for its deadline", "run", r.id, "error", err)
			continue
		}
		l := b.Deadline - WorkingTime(evs, now)
		if l < 0 {
			l = 0
		}
		if !limited || l < left {
			left, limited = l, true
		}
	}
	return left, limited
}

// ownBudget returns the budget of one run without the cap of its
// ancestors: the request, then the profile, then the configuration.
func (a *Agent) ownBudget(rs *runState) Budget {
	q := rs.queuedData()
	var b Budget
	if q.Budget != nil {
		b = *q.Budget
	}
	if prof := a.profileOf(q); prof.Budget != nil {
		b = b.Merge(*prof.Budget)
	}
	return b.Merge(a.cfg.Budget)
}

// checkZones makes sure that every zone in the configuration resolves, so
// a wrong name stops the process at start instead of at the first run.
func checkZones(cfg *Config) error {
	if _, _, err := LoadZone(cfg.Time.Zone); err != nil {
		return fmt.Errorf("fw: time.zone: %w", err)
	}
	for name, p := range cfg.Agents {
		if p.TimeZone == "" {
			continue
		}
		if _, _, err := LoadZone(p.TimeZone); err != nil {
			return fmt.Errorf("fw: agents.%s.timeZone: %w", name, err)
		}
	}
	return nil
}
