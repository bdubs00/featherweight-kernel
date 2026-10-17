package view

import (
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func TestGlance(t *testing.T) {
	now := time.Now()
	run := func(agent string, st fw.RunState, ago time.Duration, parent string) *fw.Run {
		return &fw.Run{Agent: agent, State: st, UpdatedAt: now.Add(-ago), Parent: parent}
	}
	for _, tc := range []struct {
		runs []*fw.Run
		want string
	}{
		{nil, "Nothing is running right now."},
		{[]*fw.Run{run("a", fw.StateCompleted, time.Hour, "")}, "Nothing is running right now."},
		{[]*fw.Run{run("a", fw.StateRunning, 0, ""), run("a", fw.StateQueued, 0, "")}, "One agent is working."},
		{[]*fw.Run{run("a", fw.StateRunning, 0, ""), run("b", fw.StateRunning, 0, ""), run("c", fw.StateInputRequired, 0, "")}, "Two agents are working. One run needs you."},
		{[]*fw.Run{run("a", fw.StateInputRequired, 0, ""), run("b", fw.StateInputRequired, 0, ""), run("a", fw.StateRunning, 0, "p")}, "Two runs need you."},
	} {
		if got := GlanceOf(tc.runs, now).Sentence; got != tc.want {
			t.Errorf("sentence = %q, want %q", got, tc.want)
		}
	}
	g := GlanceOf([]*fw.Run{
		run("a", fw.StateCompleted, time.Hour, ""), run("a", fw.StateCompleted, 48*time.Hour, ""),
		run("a", fw.StateFailed, time.Hour, ""), run("a", fw.StateCanceled, time.Hour, ""),
	}, now)
	if g.Finished != 1 || g.Failed != 1 || g.Working != 0 || g.Waiting != 0 {
		t.Errorf("glance = %+v", g)
	}
}
