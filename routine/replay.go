package routine

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// bands are the lower bounds of the bands of the reliability table: the
// probability of the answer that the decider chose falls in one of them.
var bands = [...]float64{0, 0.5, 0.7, 0.85, 0.95}

// ReplayOptions are the options of Replay. Decider, when set, answers the
// questions of the routine on the input of each run.
type ReplayOptions struct {
	Decider fw.Decider
}

// ReplayResult is what Replay found. Matched lists the runs whose every
// step and check matched. Diverged and Unreadable list the others.
// Questions holds the score of the decider per question, with a decider.
type ReplayResult struct {
	Routine    string                    `json:"routine"`
	Version    int                       `json:"version"`
	Runs       int                       `json:"runs"`
	Matched    []string                  `json:"matched"`
	Diverged   []Divergence              `json:"diverged,omitempty"`
	Unreadable []Unreadable              `json:"unreadable,omitempty"`
	Questions  map[string]*QuestionScore `json:"questions,omitempty"`
	// DeciderErrors counts the runs on which the decider failed, and
	// DeciderError is the first error.
	DeciderErrors int    `json:"deciderErrors,omitempty"`
	DeciderError  string `json:"deciderError,omitempty"`
}

// Divergence is the first step of a run that does not match. Step is its
// place, from 1, among the steps and then the checks. Rendered is the
// input of the routine and Recorded the input of the agent, each bounded
// to 200 bytes.
type Divergence struct {
	Run      string `json:"run"`
	Step     int    `json:"step"`
	Tool     string `json:"tool"`
	Reason   string `json:"reason"`
	Rendered string `json:"rendered,omitempty"`
	Recorded string `json:"recorded,omitempty"`
}

// Unreadable is a run that Replay could not read the answers of.
type Unreadable struct {
	Run    string `json:"run"`
	Reason string `json:"reason"`
}

// QuestionScore is the score of the decider on one question over the runs
// with an observed answer: how often its answer agreed, the Brier score
// (the mean squared error of its probabilities, 0 is perfect), and the
// reliability table by band.
type QuestionScore struct {
	Compared int     `json:"compared"`
	Agreed   int     `json:"agreed"`
	Brier    float64 `json:"brier"`
	Bands    []Band  `json:"bands"`
	sum      float64
}

// Band is one row of the reliability table: the runs whose probability of
// the chosen answer was at least From and below the next band, and how
// many of them agreed with the agent.
type Band struct {
	From   float64 `json:"from"`
	Runs   int     `json:"runs"`
	Agreed int     `json:"agreed"`
}

// Replay checks a routine against recorded runs (docs/ROUTINES.md section
// 3.6). For each run, it reads the answers of the agent with observe,
// renders the steps with those answers and with the recorded results of
// the earlier steps, and compares each rendered input with the recorded
// call of the agent to the same tool at the same place among the calls of
// that tool, as canonical JSON. A check afterward matches when its input
// matches the next call of its tool and the recorded result holds every
// contains and no lacks. A run matches when every step and every check
// matches. ctx bounds the calls to the decider.
func Replay(ctx context.Context, r fw.Routine, runs [][]fw.Event, opts ReplayOptions) ReplayResult {
	res := ReplayResult{Routine: r.Name, Version: r.Version, Runs: len(runs), Matched: []string{}}
	if opts.Decider != nil {
		res.Questions = map[string]*QuestionScore{}
		for _, name := range r.QuestionNames() {
			s := &QuestionScore{}
			for _, from := range bands {
				s.Bands = append(s.Bands, Band{From: from})
			}
			res.Questions[name] = s
		}
	}
	for _, events := range runs {
		rec := readRun(events)
		observed, why := observeAll(r, rec)
		if why == "" {
			if d := replayRun(r, rec, observed); d != nil {
				res.Diverged = append(res.Diverged, *d)
			} else {
				res.Matched = append(res.Matched, rec.id)
			}
		} else {
			res.Unreadable = append(res.Unreadable, Unreadable{Run: rec.id, Reason: why})
		}
		if opts.Decider != nil && rec.done && len(r.Questions) > 0 {
			res.score(ctx, opts.Decider, r, rec, observed)
		}
	}
	for _, s := range res.Questions {
		if s.Compared > 0 {
			s.Brier = s.sum / float64(s.Compared)
		}
	}
	return res
}

// observeAll reads the answer of the agent to every question of the
// routine. It returns why the run is unreadable, or "".
func observeAll(r fw.Routine, rec record) (map[string]string, string) {
	if !rec.done {
		return nil, "the run did not complete"
	}
	out := map[string]string{}
	for _, name := range r.QuestionNames() {
		v, ok := fw.ObserveAnswer(r.Questions[name], rec.output, rec.events)
		switch {
		case !ok:
			return nil, name + " has no observe block, so the answer of the agent is unknown"
		case v == "":
			return nil, name + ": no answer of the agent observed"
		}
		out[name] = v
	}
	return out, ""
}

// replayRun renders the steps and the checks of the routine for one run
// and compares them with the calls of the agent. It returns the first
// divergence, or nil when the run matches.
func replayRun(r fw.Routine, rec record, observed map[string]string) *Divergence {
	data := fw.RoutineData(rec.input, rec.meta)
	for k, v := range observed {
		data[k] = v
	}
	seen := map[string]int{}
	match := func(place int, tool, input string) (call, *Divergence) {
		div := &Divergence{Run: rec.id, Step: place, Tool: tool}
		j := seen[tool]
		seen[tool]++
		rendered, err := fw.RenderRoutineInput(input, data)
		if err != nil {
			div.Reason = err.Error()
			return call{}, div
		}
		div.Rendered = bound(fw.CanonicalJSON(rendered))
		agent := rec.callsOf(tool)
		if j >= len(agent) {
			div.Reason = fmt.Sprintf("the agent made %d calls of %s, and this is call %d", len(agent), tool, j+1)
			return call{}, div
		}
		c := agent[j]
		div.Recorded = bound(fw.CanonicalJSON(c.input))
		if fw.CanonicalJSON(rendered) != fw.CanonicalJSON(c.input) {
			div.Reason = "the input differs"
			return call{}, div
		}
		if c.isError {
			div.Reason = "the call of the agent failed"
			return call{}, div
		}
		return c, nil
	}
	for i, s := range r.Steps {
		c, div := match(i+1, s.Tool, s.Input)
		if div != nil {
			return div
		}
		if s.As != "" {
			data[s.As] = c.result
		}
	}
	for j, ck := range r.After {
		place := len(r.Steps) + j + 1
		c, div := match(place, ck.Tool, ck.Input)
		if div != nil {
			return div
		}
		for _, want := range ck.Contains {
			text, err := fw.RenderRoutine(want, data)
			if err != nil {
				return &Divergence{Run: rec.id, Step: place, Tool: ck.Tool, Reason: err.Error()}
			}
			if !strings.Contains(c.result, text) {
				return &Divergence{Run: rec.id, Step: place, Tool: ck.Tool, Reason: fmt.Sprintf("the result lacks %q", text)}
			}
		}
		for _, bad := range ck.Lacks {
			text, err := fw.RenderRoutine(bad, data)
			if err != nil {
				return &Divergence{Run: rec.id, Step: place, Tool: ck.Tool, Reason: err.Error()}
			}
			if strings.Contains(c.result, text) {
				return &Divergence{Run: rec.id, Step: place, Tool: ck.Tool, Reason: fmt.Sprintf("the result holds %q", text)}
			}
		}
	}
	return nil
}

// score asks the decider the questions of the routine on the input of the
// run, as the runner asks them, and scores each answer against the answer
// of the agent.
func (res *ReplayResult) score(ctx context.Context, d fw.Decider, r fw.Routine, rec record, observed map[string]string) {
	qs := map[string]fw.Question{}
	for name, q := range r.Questions {
		qs[r.Name+"."+name] = q.Question
	}
	dec, err := d.Decide(ctx, rec.input, qs)
	if err != nil {
		if res.DeciderErrors == 0 {
			res.DeciderError = err.Error()
		}
		res.DeciderErrors++
		return
	}
	for _, name := range r.QuestionNames() {
		q := r.Questions[name]
		obs := observed[name]
		if obs == "" {
			v, ok := fw.ObserveAnswer(q, rec.output, rec.events)
			if !ok || v == "" {
				continue
			}
			obs = v
		}
		a, ok := dec.Answers[r.Name+"."+name]
		if !ok {
			continue
		}
		s := res.Questions[name]
		agree := strings.EqualFold(fw.SaidOf(q, a), obs)
		s.Compared++
		if agree {
			s.Agreed++
		}
		s.sum += brier(q, a, obs)
		p := chosenProbability(q, a)
		for i := len(s.Bands) - 1; i >= 0; i-- {
			if p >= s.Bands[i].From {
				s.Bands[i].Runs++
				if agree {
					s.Bands[i].Agreed++
				}
				break
			}
		}
	}
}

// chosenProbability is the probability of the answer that the decider
// chose: P(yes) or P(no) of a noul, whichever is larger, and the
// probability of the chosen option otherwise.
func chosenProbability(q fw.RoutineQuestion, a fw.Answer) float64 {
	if q.Type == fw.QuestionNoul {
		return math.Max(a.Probability, 1-a.Probability)
	}
	return a.Probability
}

// brier is the squared error of the probabilities of one answer against
// the answer of the agent: (P(yes) - y)² for a noul, and the sum over the
// options of a choice or a score.
func brier(q fw.RoutineQuestion, a fw.Answer, observed string) float64 {
	if q.Type == fw.QuestionNoul {
		y := 0.0
		if strings.EqualFold(observed, "yes") {
			y = 1
		}
		return (a.Probability - y) * (a.Probability - y)
	}
	sum, hit := 0.0, false
	for option, p := range a.Probabilities {
		y := 0.0
		if strings.EqualFold(option, observed) {
			y, hit = 1, true
		}
		sum += (p - y) * (p - y)
	}
	if !hit {
		sum++ // the agent chose an option that the decider gave no probability
	}
	return sum
}
