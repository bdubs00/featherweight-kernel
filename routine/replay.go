package routine

import (
	"context"
	"fmt"
	"math"
	"slices"
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
// step and check matched, and Diverged the runs with a step that did not.
// Handed lists the runs that the routine hands to the agent: an answer of
// the agent that a question does not expect, or a find that finds no
// value. Unreadable lists the runs whose answers Replay could not read.
// Extra counts the keys that only the calls of the agent hold, which no
// step compares. Unchecked counts the checks afterward that the agent
// made no call for, which Replay skips. Questions holds the score of the
// decider per question, with a decider.
type ReplayResult struct {
	Routine    string                    `json:"routine"`
	Version    int                       `json:"version"`
	Runs       int                       `json:"runs"`
	Matched    []string                  `json:"matched"`
	Diverged   []Divergence              `json:"diverged,omitempty"`
	Handed     []Unreadable              `json:"handed,omitempty"`
	Unreadable []Unreadable              `json:"unreadable,omitempty"`
	Extra      []Extra                   `json:"extra,omitempty"`
	Unchecked  []Extra                   `json:"unchecked,omitempty"`
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

// Unreadable is a run that Replay did not compare, with the reason.
type Unreadable struct {
	Run    string `json:"run"`
	Reason string `json:"reason"`
}

// Extra is a key that the call of the agent holds and the rendered input
// of the step does not, such as an optional key, with the number of runs.
// In Unchecked, Key is empty, and the entry counts the runs without the
// call of a check.
type Extra struct {
	Step int    `json:"step"`
	Tool string `json:"tool"`
	Key  string `json:"key"`
	Runs int    `json:"runs"`
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
// 3.6). For each run, it reads the answers of the agent with observe. A
// run whose answers a question does not expect goes to the agent, and
// Replay counts it as handed. Otherwise Replay renders the steps with the
// answers and with the recorded results of the earlier steps, and
// compares each rendered input with the recorded call of the agent to the
// same tool at the same place among the calls of that tool. A step
// matches when each key that it compares, every key of the rendered input
// or the keys of compare, holds the recorded value as canonical JSON. A
// check afterward matches the next call of its tool in the same way, and
// the recorded result holds every contains and no lacks. An agent that
// made no such call leaves the check unchecked. A run matches when every
// step and every check that it has a call for matches. A find that finds no value
// hands the run to the agent. ctx bounds the calls to the decider.
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
	extra, unchecked := map[Extra]int{}, map[Extra]int{}
	for _, events := range runs {
		rec := readRun(events)
		observed, why := observeAll(r, rec)
		switch {
		case why != "":
			res.Unreadable = append(res.Unreadable, Unreadable{Run: rec.id, Reason: why})
		case expected(r, observed) != "":
			res.Handed = append(res.Handed, Unreadable{Run: rec.id, Reason: expected(r, observed)})
		default:
			out := replayRun(r, rec, observed, extra, unchecked)
			switch {
			case out.handed != "":
				res.Handed = append(res.Handed, Unreadable{Run: rec.id, Reason: out.handed})
			case out.div != nil:
				res.Diverged = append(res.Diverged, *out.div)
			default:
				res.Matched = append(res.Matched, rec.id)
			}
		}
		if opts.Decider != nil && rec.done && len(r.Questions) > 0 {
			res.score(ctx, opts.Decider, r, rec, observed)
		}
	}
	res.Extra, res.Unchecked = counted(extra), counted(unchecked)
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

// counted returns the entries of a count in the order of the steps.
func counted(m map[Extra]int) []Extra {
	var out []Extra
	for k, n := range m {
		k.Runs = n
		out = append(out, k)
	}
	slices.SortFunc(out, func(a, b Extra) int {
		if a.Step != b.Step {
			return a.Step - b.Step
		}
		return strings.Compare(a.Key, b.Key)
	})
	return out
}

// expected returns why the answers of the agent are ones that the routine
// does not act on, or "": an option that expect leaves out, or yes or no
// against the expect of a noul.
func expected(r fw.Routine, observed map[string]string) string {
	for _, name := range r.QuestionNames() {
		q := r.Questions[name]
		if len(q.Expect) > 0 && !slices.ContainsFunc(q.Expect, func(v string) bool { return strings.EqualFold(v, observed[name]) }) {
			return fmt.Sprintf("the agent answered %s %s, which the routine does not act on", name, observed[name])
		}
	}
	return ""
}

// runOutcome is what replayRun found: a divergence, or why the routine
// hands the run to the agent, or neither for a match.
type runOutcome struct {
	div    *Divergence
	handed string
}

// replayRun renders the steps and the checks of the routine for one run
// and compares them with the calls of the agent (fw.CompareRoutineCalls).
// It counts the keys that only the agent sets in extra, and the checks
// without a call of the agent in unchecked.
func replayRun(r fw.Routine, rec record, observed map[string]string, extra, unchecked map[Extra]int) runOutcome {
	data := fw.RoutineData(rec.input, rec.meta)
	for k, v := range observed {
		data[k] = v
	}
	cmp := fw.CompareRoutineCalls(r, data, rec.calls)
	for _, k := range cmp.Extra {
		extra[Extra{Step: k.Step, Tool: k.Tool, Key: k.Key}]++
	}
	for _, k := range cmp.Unchecked {
		unchecked[Extra{Step: k.Step, Tool: k.Tool}]++
	}
	switch {
	case cmp.Handed != "":
		return runOutcome{handed: cmp.Handed}
	case cmp.Step > 0:
		return runOutcome{div: &Divergence{Run: rec.id, Step: cmp.Step, Tool: cmp.Tool, Reason: cmp.Reason, Rendered: cmp.Rendered, Recorded: cmp.Recorded}}
	}
	return runOutcome{}
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
