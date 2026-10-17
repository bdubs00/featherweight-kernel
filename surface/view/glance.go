package view

// This file is "At a glance": the state of the runs that a person can see,
// as four counts and one plain sentence. The page, the MCP tool text, and a
// host that shows only text all say the same thing.

import (
	"strconv"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// GlanceWindow is how far back Finished and Failed count.
const GlanceWindow = 24 * time.Hour

// Glance counts the top-level runs: a child run waits and works for its
// parent, and its parent shows it. Working counts the runs that are queued,
// running, or paused. Waiting counts the runs that wait for a person.
// Finished and Failed count the runs that ended in the last GlanceWindow.
// Agents counts the agents with a working run.
type Glance struct {
	Working  int    `json:"working"`
	Waiting  int    `json:"waiting"`
	Finished int    `json:"finished"`
	Failed   int    `json:"failed"`
	Agents   int    `json:"agents"`
	Sentence string `json:"sentence"`
}

// GlanceOf builds the glance of runs at the time now.
func GlanceOf(runs []*fw.Run, now time.Time) Glance {
	var g Glance
	agents := map[string]bool{}
	for _, r := range runs {
		if r == nil || r.Parent != "" {
			continue
		}
		switch r.State {
		case fw.StateQueued, fw.StateRunning, fw.StateSuspended:
			g.Working++
			agents[r.Member+"/"+r.Agent] = true
		case fw.StateInputRequired:
			g.Waiting++
		case fw.StateCompleted:
			if now.Sub(r.UpdatedAt) <= GlanceWindow {
				g.Finished++
			}
		case fw.StateFailed:
			if now.Sub(r.UpdatedAt) <= GlanceWindow {
				g.Failed++
			}
		}
	}
	g.Agents = len(agents)
	g.Sentence = g.sentence()
	return g
}

func (g Glance) sentence() string {
	var parts []string
	if g.Agents > 0 {
		verb := "are"
		if g.Agents == 1 {
			verb = "is"
		}
		parts = append(parts, count(g.Agents, "agent", "agents", true)+" "+verb+" working.")
	}
	if g.Waiting > 0 {
		verb := "need"
		if g.Waiting == 1 {
			verb = "needs"
		}
		parts = append(parts, count(g.Waiting, "run", "runs", true)+" "+verb+" you.")
	}
	if len(parts) == 0 {
		return "Nothing is running right now."
	}
	return strings.Join(parts, " ")
}

// numberWords spells the numbers that a sentence spells out.
var numberWords = [...]string{"No", "One", "Two", "Three", "Four", "Five", "Six", "Seven", "Eight", "Nine"}

// count writes a number with the singular or the plural noun. When spell
// is set, the numbers below ten are words, capitalized to start a sentence.
func count(n int, one, many string, spell ...bool) string {
	num := strconv.Itoa(n)
	if len(spell) > 0 && spell[0] && n >= 0 && n < len(numberWords) {
		num = numberWords[n]
	}
	if n == 1 {
		return num + " " + one
	}
	return num + " " + many
}
