package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// clockStart is 09:40 in Chicago on a Wednesday.
var clockStart = time.Date(2026, 10, 14, 14, 40, 0, 0, time.UTC)

// clockAgent is an agent with a manual clock, the clock tool, and a model
// that answers from a script. It records every request to the model.
type clockAgent struct {
	a     *fw.Agent
	clock *fw.ManualClock
	tool  fw.Tool

	mu   sync.Mutex
	reqs []*fw.Request
}

// clockStep is one model call of the script: it moves the clock by advance
// and returns resp.
type clockStep struct {
	advance time.Duration
	resp    *fw.Response
}

// newClockAgent builds a clockAgent. After the script, the model ends the
// turn.
func newClockAgent(t *testing.T, zone string, noteAfter, deadline time.Duration, steps ...clockStep) *clockAgent {
	t.Helper()
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Time.Zone = zone
	cfg.Time.NoteAfter = noteAfter
	cfg.Budget.Deadline = deadline
	tool, err := fw.NewBuiltinTool("clock", cfg)
	if err != nil {
		t.Fatal(err)
	}
	c := &clockAgent{clock: fw.NewManualClock(clockStart), tool: tool}
	model := fw.ModelFunc{StreamFn: func(_ context.Context, req *fw.Request, _ func(fw.Delta)) (*fw.Response, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.reqs = append(c.reqs, req)
		if len(steps) == 0 {
			return &fw.Response{Content: []fw.Block{fw.Text("done")}, StopReason: fw.StopEndTurn}, nil
		}
		s := steps[0]
		steps = steps[1:]
		c.clock.Advance(s.advance)
		return s.resp, nil
	}}
	c.a, err = fw.New(
		fw.WithConfig(cfg),
		fw.WithoutDefaults(),
		fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()),
		fw.WithAuthn(fw.NoAuthn{}),
		fw.WithModel(model),
		fw.WithClock(c.clock),
		fw.WithTools(tool),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.a.Close(context.Background()) })
	return c
}

func (c *clockAgent) requests() []*fw.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*fw.Request(nil), c.reqs...)
}

// callClock is a response that calls the clock tool.
func callClock(id string) *fw.Response {
	return &fw.Response{
		Content:    []fw.Block{{Type: fw.BlockToolUse, ID: id, Name: "clock", Input: json.RawMessage(`{}`)}},
		StopReason: fw.StopToolUse,
	}
}

// clockResult returns the text of the result of the tool call id from the
// requests that the model saw.
func clockResult(t *testing.T, reqs []*fw.Request, id string) string {
	t.Helper()
	for _, req := range reqs {
		for _, m := range req.Messages {
			for _, b := range m.Content {
				if b.Type == fw.BlockToolResult && b.ToolUseID == id {
					if b.IsError {
						t.Fatalf("clock call %s failed: %s", id, fw.TextOf(b.Content))
					}
					return fw.TextOf(b.Content)
				}
			}
		}
	}
	t.Fatalf("no result for the clock call %s", id)
	return ""
}

func runClock(t *testing.T, c *clockAgent, opts ...fw.RunOption) *fw.Run {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := c.a.Run(ctx, "What time is it?", opts...)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != fw.StateCompleted {
		t.Fatalf("run state %s, error %q", r.State, r.Error)
	}
	return r
}

func TestClockBrief(t *testing.T) {
	const tail = "The clock tool gives the exact time, the time since this run started, and the working time left before its deadline."
	cases := []struct {
		name      string
		start     time.Time
		zone      string
		profile   string
		noteAfter time.Duration
		want      string
	}{
		{
			name:      "notes on",
			start:     clockStart,
			zone:      "America/Chicago",
			noteAfter: 10 * time.Minute,
			want: "# Time\n\nToday is Wednesday, 2026-10-14, in the time zone America/Chicago. " + tail +
				" When time passes between two of your responses, a system message gives the new time.",
		},
		{
			name:  "notes off",
			start: clockStart,
			zone:  "America/Chicago",
			want:  "# Time\n\nToday is Wednesday, 2026-10-14, in the time zone America/Chicago. " + tail,
		},
		{
			// 03:30 UTC on Wednesday is still Tuesday evening in Chicago.
			name:  "date in the zone",
			start: time.Date(2026, 10, 14, 3, 30, 0, 0, time.UTC),
			zone:  "America/Chicago",
			want:  "# Time\n\nToday is Tuesday, 2026-10-13, in the time zone America/Chicago. " + tail,
		},
		{
			// 20:00 UTC on Wednesday is already Thursday in Tokyo.
			name:    "profile zone",
			start:   time.Date(2026, 10, 14, 20, 0, 0, 0, time.UTC),
			zone:    "UTC",
			profile: "Asia/Tokyo",
			want:    "# Time\n\nToday is Thursday, 2026-10-15, in the time zone Asia/Tokyo. " + tail,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newClockAgent(t, tc.zone, tc.noteAfter, 0)
			c.clock.Set(tc.start)
			var opts []fw.RunOption
			if tc.profile != "" {
				opts = append(opts, fw.WithRunProfile(fw.AgentProfile{TimeZone: tc.profile}))
			}
			r := runClock(t, c, opts...)

			b, ok := c.tool.(fw.Briefer)
			if !ok {
				t.Fatal("the clock tool is not an fw.Briefer")
			}
			got, err := b.Brief(fw.WithAgent(context.Background(), c.a), fw.RunInfo{RunID: r.ID, Agent: r.Agent})
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("brief:\n%q\nwant:\n%q", got, tc.want)
			}
			// The kernel puts the brief at the end of the system prompt.
			reqs := c.requests()
			if len(reqs) == 0 || !strings.HasSuffix(reqs[0].System, tc.want) {
				t.Fatalf("the system prompt does not end with the brief: %q", reqs[0].System)
			}
			if strings.Contains(got, ":") {
				t.Fatalf("the brief names a time of day, which breaks the shared prompt cache: %q", got)
			}
		})
	}
}

func TestClockBriefOutsideARun(t *testing.T) {
	tool, err := fw.NewBuiltinTool("clock", fw.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	got, err := tool.(fw.Briefer).Brief(context.Background(), fw.RunInfo{RunID: "r1"})
	if err != nil || got != "" {
		t.Fatalf("brief without an agent = %q, %v", got, err)
	}
}

func TestClockCallWithDeadline(t *testing.T) {
	c := newClockAgent(t, "America/Chicago", 0, 2*time.Hour,
		clockStep{advance: 22 * time.Minute, resp: callClock("c1")},
		clockStep{advance: time.Hour, resp: callClock("c2")},
	)
	runClock(t, c)
	reqs := c.requests()

	want := "It is Wednesday 2026-10-14 10:02 CDT (2026-10-14T15:02:00Z UTC). " +
		"The time zone of this run is America/Chicago. " +
		"The run started at Wednesday 2026-10-14 09:40 CDT and has worked for 22 minutes. " +
		"It has 1 hour 38 minutes of working time left before its deadline. " +
		"Time that the run spends waiting for a person or a follow-up does not count."
	if got := clockResult(t, reqs, "c1"); got != want {
		t.Fatalf("first call:\n%q\nwant:\n%q", got, want)
	}
	second := clockResult(t, reqs, "c2")
	for _, part := range []string{
		"It is Wednesday 2026-10-14 11:02 CDT (2026-10-14T16:02:00Z UTC).",
		"has worked for 1 hour 22 minutes.",
		"It has 38 minutes of working time left before its deadline.",
	} {
		if !strings.Contains(second, part) {
			t.Fatalf("second call %q does not contain %q", second, part)
		}
	}
}

func TestClockCallWithoutDeadline(t *testing.T) {
	c := newClockAgent(t, "UTC", 0, 0, clockStep{advance: 90 * time.Second, resp: callClock("c1")})
	runClock(t, c)
	want := "It is Wednesday 2026-10-14 14:41 UTC (2026-10-14T14:41:30Z UTC). " +
		"The time zone of this run is UTC. " +
		"The run started at Wednesday 2026-10-14 14:40 UTC and has worked for 1 minute 30 seconds. " +
		"No deadline applies to this run."
	if got := clockResult(t, c.requests(), "c1"); got != want {
		t.Fatalf("call:\n%q\nwant:\n%q", got, want)
	}
}

func TestClockCallOutsideARun(t *testing.T) {
	cfg := fw.DefaultConfig()
	cfg.Time.Zone = "America/Chicago"
	tool, err := fw.NewBuiltinTool("clock", cfg)
	if err != nil {
		t.Fatal(err)
	}
	got := mustOK(t, call(t, context.Background(), tool, `{}`))
	if !strings.HasPrefix(got, "It is ") || !strings.Contains(got, "The time zone is America/Chicago.") ||
		!strings.Contains(got, "No run is in this context") {
		t.Fatalf("result outside a run: %q", got)
	}
	if !strings.Contains(got, " CST (") && !strings.Contains(got, " CDT (") {
		t.Fatalf("the time is not in the zone of the configuration: %q", got)
	}
}

func TestClockCallUnknownRun(t *testing.T) {
	c := newClockAgent(t, "UTC", 0, 0)
	ctx := fw.WithAgent(fw.WithRunInfo(context.Background(), fw.RunInfo{RunID: "nope"}), c.a)
	mustFail(t, call(t, ctx, c.tool, `{}`), "clock: cannot read the time of run nope")
}

func TestClockUnknownZone(t *testing.T) {
	cfg := fw.DefaultConfig()
	cfg.Time.Zone = "Mars/Olympus_Mons"
	_, err := fw.NewBuiltinTool("clock", cfg)
	if err == nil || !strings.Contains(err.Error(), "builtin: clock:") {
		t.Fatalf("NewBuiltinTool with an unknown zone: %v", err)
	}
}
