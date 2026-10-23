package fw

// This file holds the routine: a vetted sequence of tool calls with typed
// holes, a decision schema, and a check afterward, that the kernel runs
// under the policy of the run (docs/ROUTINES.md section 2). The runner is
// in routine_run.go, and the decider is in decider.go.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"regexp"
	"slices"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

// Standings of a routine, and the modes of fw.routine.decided.
const (
	// RoutineShadow runs the decision beside the agent and never acts.
	RoutineShadow = "shadow"
	// RoutineLive acts in place of the model when the decision applies.
	RoutineLive = "live"
	// RoutineCanary is the mode of a live routine on a run that the
	// canary share picked: the agent handles the run, and the routine
	// stays in shadow.
	RoutineCanary = "canary"
)

// DefaultCanary is the canary share of a live routine that sets none.
const DefaultCanary = 0.05

// Bounds of the routines of a profile. The kernel refuses a profile past
// them when it loads the profile.
const (
	MaxRoutines         = 64
	MaxRoutineSteps     = 16
	MaxRoutineQuestions = 16
	MaxRoutineTemplate  = 4 << 10
)

// Routine is a vetted sequence of tool calls that the kernel can run in
// place of the model. A profile carries its routines in
// AgentProfile.Routines, and the kernel pins them in fw.run.queued.
//
// Name is unique per agent. Version is a positive integer that the
// workbench bumps on every change. Standing is RoutineShadow or
// RoutineLive. Canary is the share of the runs of a live routine that the
// agent handles anyway, with the routine in shadow; nil means
// DefaultCanary. Decider names a key of Config.Deciders, and a routine
// with questions needs one. Questions are answered in one decider call per
// run. Steps are tool calls in order, After the checks afterward, and
// Answer the output of the run when the routine completes. Every input,
// the answer, and each string of contains and lacks is a template
// (RenderRoutine).
type Routine struct {
	Name      string                     `yaml:"name" json:"name"`
	Version   int                        `yaml:"version" json:"version"`
	Standing  string                     `yaml:"standing" json:"standing"`
	Canary    *float64                   `yaml:"canary,omitempty" json:"canary,omitempty"`
	Decider   string                     `yaml:"decider,omitempty" json:"decider,omitempty"`
	Questions map[string]RoutineQuestion `yaml:"questions,omitempty" json:"questions,omitempty"`
	Steps     []RoutineStep              `yaml:"steps,omitempty" json:"steps,omitempty"`
	After     []RoutineCheck             `yaml:"after,omitempty" json:"after,omitempty"`
	Answer    string                     `yaml:"answer,omitempty" json:"answer,omitempty"`
}

// RoutineQuestion is one question of the decision schema of a routine.
// Act is the band: the routine acts only when the probability of the
// chosen option is at least Act, or for a noul, when P(yes) is at least
// Act or at most 1 - Act. Expect lists the options of a choice or a score
// that let the routine act, or holds yes or no for a noul. Observe reads
// the answer of the agent to the question from a run, for the comparison.
type RoutineQuestion struct {
	Question `yaml:",inline"`
	Act      float64  `yaml:"act,omitempty" json:"act,omitempty"`
	Expect   Expect   `yaml:"expect,omitempty" json:"expect,omitempty"`
	Observe  *Observe `yaml:"observe,omitempty" json:"observe,omitempty"`
}

// Expect is RoutineQuestion.Expect. In YAML and in JSON, it is one value,
// such as no, or a list.
type Expect []string

// UnmarshalYAML takes a scalar, such as no, or a list.
func (e *Expect) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		var v string
		if err := n.Decode(&v); err != nil {
			return err
		}
		*e = Expect{v}
		return nil
	}
	var list []string
	if err := n.Decode(&list); err != nil {
		return err
	}
	*e = list
	return nil
}

// UnmarshalJSON takes a string, such as "no", or an array.
func (e *Expect) UnmarshalJSON(b []byte) error {
	var v string
	if json.Unmarshal(b, &v) == nil {
		*e = Expect{v}
		return nil
	}
	var list []string
	if err := json.Unmarshal(b, &list); err != nil {
		return fmt.Errorf("fw: expect is one value or a list of values: %w", err)
	}
	*e = list
	return nil
}

// MarshalJSON writes one value as a string and more as an array.
func (e Expect) MarshalJSON() ([]byte, error) {
	if len(e) == 1 {
		return json.Marshal(e[0])
	}
	return json.Marshal([]string(e))
}

// Observe says how to read the answer of the agent to a question out of a
// run (docs/ROUTINES.md section 3.5). Answer is a regular expression over
// the output text of the run. Tool and Match run a regular expression over
// the input of each call of that tool by the model, and the first match
// wins. The first capture group is the observed value. For a noul, the
// observed value is yes when the expression matches and no otherwise.
type Observe struct {
	Answer string `yaml:"answer,omitempty" json:"answer,omitempty"`
	Tool   string `yaml:"tool,omitempty" json:"tool,omitempty"`
	Match  string `yaml:"match,omitempty" json:"match,omitempty"`
}

// RoutineStep is one tool call of a routine. Input renders to a JSON
// object. As names the result text for the later templates.
type RoutineStep struct {
	Tool  string `yaml:"tool" json:"tool"`
	Input string `yaml:"input" json:"input"`
	As    string `yaml:"as,omitempty" json:"as,omitempty"`
}

// RoutineCheck is one check afterward: a call, and the substrings that its
// result text must hold (Contains) and must not hold (Lacks).
type RoutineCheck struct {
	Tool     string   `yaml:"tool" json:"tool"`
	Input    string   `yaml:"input" json:"input"`
	Contains []string `yaml:"contains,omitempty" json:"contains,omitempty"`
	Lacks    []string `yaml:"lacks,omitempty" json:"lacks,omitempty"`
}

// CanaryShare returns the canary share of the routine: Canary, or
// DefaultCanary when it is nil.
func (r Routine) CanaryShare() float64 {
	if r.Canary == nil {
		return DefaultCanary
	}
	return *r.Canary
}

// QuestionNames returns the names of the questions in sorted order.
func (r Routine) QuestionNames() []string { return slices.Sorted(maps.Keys(r.Questions)) }

// CanaryPick reports whether the canary share picks a run: a stable hash of
// the run id, as a number in [0, 1), is below share. The same run id and
// share give the same pick on every process.
func CanaryPick(runID string, share float64) bool {
	if share <= 0 {
		return false
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(runID))
	return float64(h.Sum64()>>11)/float64(1<<53) < share
}

// Names in a routine. A question name and the name of a step result are
// fields of the template data, so they are identifiers.
var (
	routineName  = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
	routineField = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
)

// routineReserved holds the fields of the template data that the kernel
// sets.
var routineReserved = []string{"input", "json", "meta"}

// CheckRoutineName reports with ErrInvalidRequest whether a name is the
// name of a routine: 1 to 64 of a-z, 0-9, and -.
func CheckRoutineName(name string) error {
	if !routineName.MatchString(name) {
		return fmt.Errorf("%w: %q is not the name of a routine; a name holds 1 to 64 of a-z, 0-9, and -", ErrInvalidRequest, name)
	}
	return nil
}

// CheckRoutines reports with ErrInvalidRequest whether the kernel accepts
// the routines of a profile. The error names the first problem. cfg names
// the deciders. A nil cfg checks no decider, so a
// routine without its configuration, such as a proposal, can pass.
func CheckRoutines(cfg *Config, rs []Routine) error {
	if p := RoutineProblems(cfg, rs); len(p) > 0 {
		return fmt.Errorf("%w: %s", ErrInvalidRequest, p[0])
	}
	return nil
}

// RoutineProblems lists every problem of the routines of a profile, each
// keyed from routines, such as "routines[0].steps[1].input: ...". It checks
// the bounds (MaxRoutines, MaxRoutineSteps, MaxRoutineQuestions,
// MaxRoutineTemplate), the names, the questions, the decider, the regular
// expressions, and that every template parses.
func RoutineProblems(cfg *Config, rs []Routine) []string {
	var p []string
	if len(rs) > MaxRoutines {
		p = append(p, fmt.Sprintf("routines has %d routines; the limit is %d", len(rs), MaxRoutines))
	}
	seen := map[string]bool{}
	for i, r := range rs {
		key := fmt.Sprintf("routines[%d]", i)
		if err := CheckRoutineName(r.Name); err != nil {
			p = append(p, fmt.Sprintf("%s.name: %s", key, strings.TrimPrefix(err.Error(), ErrInvalidRequest.Error()+": ")))
		} else {
			if seen[r.Name] {
				p = append(p, fmt.Sprintf("%s.name %q is the name of an earlier routine", key, r.Name))
			}
			seen[r.Name] = true
			key = fmt.Sprintf("routines[%d] (%s)", i, r.Name)
		}
		p = append(p, routineProblems(cfg, key, r)...)
	}
	return p
}

func routineProblems(cfg *Config, key string, r Routine) []string {
	var p []string
	if r.Version < 1 {
		p = append(p, fmt.Sprintf("%s.version %d is not a positive integer", key, r.Version))
	}
	if r.Standing != RoutineShadow && r.Standing != RoutineLive {
		p = append(p, fmt.Sprintf("%s.standing %q is not %s or %s", key, r.Standing, RoutineShadow, RoutineLive))
	}
	if r.Canary != nil && (*r.Canary < 0 || *r.Canary > 1) {
		p = append(p, fmt.Sprintf("%s.canary %g is not from 0 to 1", key, *r.Canary))
	}
	if len(r.Questions) > MaxRoutineQuestions {
		p = append(p, fmt.Sprintf("%s.questions has %d questions; the limit is %d", key, len(r.Questions), MaxRoutineQuestions))
	}
	if len(r.Steps) > MaxRoutineSteps {
		p = append(p, fmt.Sprintf("%s.steps has %d steps; the limit is %d", key, len(r.Steps), MaxRoutineSteps))
	}
	if len(r.After) > MaxRoutineSteps {
		p = append(p, fmt.Sprintf("%s.after has %d checks; the limit is %d", key, len(r.After), MaxRoutineSteps))
	}
	switch {
	case len(r.Questions) > 0 && r.Decider == "" && cfg != nil:
		p = append(p, fmt.Sprintf("%s: the routine asks questions and names no model to answer them", key))
	case r.Decider != "" && cfg != nil && !hasDecider(cfg, r.Decider):
		p = append(p, fmt.Sprintf("%s: no model answers questions under the name %q", key, r.Decider))
	}
	fields := map[string]string{}
	for _, name := range r.QuestionNames() {
		qkey := key + ".questions." + name
		if !routineField.MatchString(name) || slices.Contains(routineReserved, name) {
			p = append(p, fmt.Sprintf("%s: a question name is 1 to 32 of a-z, 0-9, and _, starts with a letter, and is not input, json, or meta", qkey))
		}
		fields[name] = qkey
		p = append(p, questionProblems(qkey, r.Questions[name])...)
	}
	call := func(key, tool, input string) {
		if strings.TrimSpace(tool) == "" {
			p = append(p, key+".tool is empty")
		}
		if strings.TrimSpace(input) == "" {
			p = append(p, key+".input is empty; a call without input has {}")
		}
		p = append(p, templateProblems(key+".input", input)...)
	}
	for i, s := range r.Steps {
		skey := fmt.Sprintf("%s.steps[%d]", key, i)
		call(skey, s.Tool, s.Input)
		if s.As != "" {
			switch {
			case !routineField.MatchString(s.As) || slices.Contains(routineReserved, s.As):
				p = append(p, fmt.Sprintf("%s.as %q is 1 to 32 of a-z, 0-9, and _, starts with a letter, and is not input, json, or meta", skey, s.As))
			case fields[s.As] != "":
				p = append(p, fmt.Sprintf("%s.as %q is also %s", skey, s.As, fields[s.As]))
			}
			fields[s.As] = skey + ".as"
		}
	}
	for i, c := range r.After {
		ckey := fmt.Sprintf("%s.after[%d]", key, i)
		call(ckey, c.Tool, c.Input)
		for j, s := range c.Contains {
			p = append(p, templateProblems(fmt.Sprintf("%s.contains[%d]", ckey, j), s)...)
		}
		for j, s := range c.Lacks {
			p = append(p, templateProblems(fmt.Sprintf("%s.lacks[%d]", ckey, j), s)...)
		}
	}
	p = append(p, templateProblems(key+".answer", r.Answer)...)
	return p
}

func hasDecider(cfg *Config, name string) bool {
	_, ok := cfg.Deciders[name]
	return ok
}

// questionProblems checks one question: its type, its criteria, its band,
// its expected answers, and its observe block.
func questionProblems(key string, q RoutineQuestion) []string {
	var p []string
	options, err := QuestionOptions(q.Question)
	if err != nil {
		p = append(p, fmt.Sprintf("%s: %v", key, err))
	}
	if q.Act < 0 || q.Act > 1 {
		p = append(p, fmt.Sprintf("%s.act %g is not from 0 to 1", key, q.Act))
	}
	if q.Type == QuestionNoul {
		if len(q.Expect) > 1 || (len(q.Expect) == 1 && q.Expect[0] != "yes" && q.Expect[0] != "no") {
			p = append(p, fmt.Sprintf("%s.expect of a noul is yes or no", key))
		}
	} else if err == nil {
		for _, v := range q.Expect {
			if !slices.Contains(options, v) {
				p = append(p, fmt.Sprintf("%s.expect %q is not an option of the question", key, v))
			}
		}
	}
	if o := q.Observe; o != nil {
		switch {
		case o.Answer != "" && (o.Tool != "" || o.Match != ""):
			p = append(p, key+".observe has answer or tool with match, not both")
		case o.Answer == "" && (o.Tool == "" || o.Match == ""):
			p = append(p, key+".observe needs answer, or tool with match")
		}
		for _, re := range []string{o.Answer, o.Match} {
			if re == "" {
				continue
			}
			c, err := regexp.Compile(re)
			switch {
			case err != nil:
				p = append(p, fmt.Sprintf("%s.observe %q is not a regular expression: %v", key, re, err))
			case q.Type != QuestionNoul && c.NumSubexp() < 1:
				p = append(p, fmt.Sprintf("%s.observe %q has no capture group for the answer", key, re))
			}
		}
	}
	return p
}

// QuestionOptions returns the options of a choice and the levels of a
// score, in their order: the keys of a choice sorted, and the levels of a
// score as listed. It returns nil for a noul. An error names what is wrong
// with the type or the criteria.
func QuestionOptions(q Question) ([]string, error) {
	switch q.Type {
	case QuestionChoice:
		m, ok := stringMap(q.Criteria)
		if !ok || len(m) < 2 || len(m) > 255 {
			return nil, fmt.Errorf("the criteria of a choice map 2 to 255 options to descriptions")
		}
		return slices.Sorted(maps.Keys(m)), nil
	case QuestionScore:
		l, ok := stringList(q.Criteria)
		if !ok || len(l) < 2 || len(l) > 10 {
			return nil, fmt.Errorf("the criteria of a score list 2 to 10 ordered levels")
		}
		return l, nil
	case QuestionNoul:
		if q.Criteria == nil {
			return nil, nil
		}
		m, ok := stringMap(q.Criteria)
		if !ok {
			return nil, fmt.Errorf("the criteria of a noul describe true and false")
		}
		for k := range m {
			if k != "true" && k != "false" {
				return nil, fmt.Errorf("the criteria of a noul describe true and false")
			}
		}
		return nil, nil
	}
	return nil, fmt.Errorf("type %q is not %s, %s, or %s", q.Type, QuestionChoice, QuestionNoul, QuestionScore)
}

// stringMap reads criteria as a map of strings, from YAML or from JSON.
func stringMap(v any) (map[string]string, bool) {
	out := map[string]string{}
	switch m := v.(type) {
	case map[string]string:
		return m, true
	case map[string]any:
		for k, x := range m {
			s, ok := x.(string)
			if !ok || strings.TrimSpace(k) == "" {
				return nil, false
			}
			out[k] = s
		}
		return out, true
	}
	return nil, false
}

// stringList reads criteria as a list of strings, from YAML or from JSON.
func stringList(v any) ([]string, bool) {
	switch l := v.(type) {
	case []string:
		return l, true
	case []any:
		out := make([]string, 0, len(l))
		for _, x := range l {
			s, ok := x.(string)
			if !ok || strings.TrimSpace(s) == "" {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	return nil, false
}

// templateProblems checks the size of one template and parses it.
func templateProblems(key, text string) []string {
	if len(text) > MaxRoutineTemplate {
		return []string{fmt.Sprintf("%s is %d bytes; the limit is %d", key, len(text), MaxRoutineTemplate)}
	}
	if _, err := parseRoutine(text); err != nil {
		return []string{fmt.Sprintf("%s: %v", key, err)}
	}
	return nil
}

// routineFuncs are the functions of a routine template, beside the
// built-in functions of text/template such as printf.
var routineFuncs = template.FuncMap{
	"find":  routineFind,
	"json":  routineJSON,
	"lower": func(v any) string { return strings.ToLower(fmt.Sprint(v)) },
}

func parseRoutine(text string) (*template.Template, error) {
	return template.New("routine").Funcs(routineFuncs).Option("missingkey=error").Parse(text)
}

// routineFind returns the first capture group of the first match of
// pattern in text, or the whole match when the pattern has no group. A
// pattern that matches nothing is an error, so a routine never calls a
// tool with a missing value.
func routineFind(text any, pattern string) (string, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("find: %q is not a regular expression: %w", pattern, err)
	}
	v, ok := firstMatch(re, fmt.Sprint(text))
	if !ok || v == "" {
		return "", fmt.Errorf("hole %s empty", pattern)
	}
	return v, nil
}

// firstMatch returns the first capture group of the first match of re in
// text, or the whole match when re has no group, and whether it matched.
func firstMatch(re *regexp.Regexp, text string) (string, bool) {
	m := re.FindStringSubmatch(text)
	switch {
	case m == nil:
		return "", false
	case len(m) > 1:
		return m[1], true
	}
	return m[0], true
}

// routineJSON returns a value as JSON: a string as a JSON string literal.
func routineJSON(v any) (string, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", fmt.Errorf("json: %w", err)
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// RoutineData returns the template data of a run before any answer: input
// is the input as text, json is the input parsed as JSON or nil, and meta
// is the metadata of the run. The runner adds one field per question and
// one per step with as.
func RoutineData(input string, meta map[string]string) map[string]any {
	var parsed any
	if json.Unmarshal([]byte(input), &parsed) != nil {
		parsed = nil
	}
	if meta == nil {
		meta = map[string]string{}
	}
	return map[string]any{"input": input, "json": parsed, "meta": meta}
}

// RenderRoutine renders one template of a routine with the data. The
// functions are find, json, and lower. A missing field or a find that
// matches nothing is an error.
func RenderRoutine(text string, data map[string]any) (string, error) {
	t, err := parseRoutine(text)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := t.Execute(&b, data); err != nil {
		return "", unwrapExec(err)
	}
	return b.String(), nil
}

// RenderRoutineInput renders the input of a step or a check and checks
// that it is a JSON object. It returns the object in compact form.
func RenderRoutineInput(text string, data map[string]any) (json.RawMessage, error) {
	s, err := RenderRoutine(text, data)
	if err != nil {
		return nil, err
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &obj); err != nil || obj == nil {
		if err == nil {
			err = fmt.Errorf("null")
		}
		return nil, fmt.Errorf("the input is not a JSON object: %v", err)
	}
	var b bytes.Buffer
	if err := json.Compact(&b, []byte(s)); err != nil {
		return nil, fmt.Errorf("the input is not a JSON object: %v", err)
	}
	return b.Bytes(), nil
}

// unwrapExec returns the error of a template function without the text of
// text/template around it, so a reason reads "hole #(\d+) empty".
func unwrapExec(err error) error {
	var ee template.ExecError
	if errors.As(err, &ee) {
		if u := errors.Unwrap(ee.Err); u != nil {
			return u
		}
	}
	return err
}

// ObserveAnswer reads the answer of the agent to a question from a run:
// its output text and its events (docs/ROUTINES.md section 3.5). Only the
// calls of the model count: a call of the runner (ToolRequestedData.
// Routine) does not. It returns the first capture group, or for a noul yes
// when the expression matches and no otherwise. It returns "" when nothing
// matched, and false when the question has no observe block.
func ObserveAnswer(q RoutineQuestion, output string, events []Event) (string, bool) {
	o := q.Observe
	if o == nil {
		return "", false
	}
	pattern := o.Answer
	if pattern == "" {
		pattern = o.Match
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", true
	}
	match := func(text string) (string, bool) { return firstMatch(re, text) }
	var got string
	found := false
	if o.Answer != "" {
		got, found = match(output)
	} else {
		for _, e := range events {
			if e.Type != EvToolRequested {
				continue
			}
			var d ToolRequestedData
			if e.Decode(&d) != nil || d.Routine != "" || d.Tool != o.Tool {
				continue
			}
			if got, found = match(string(d.Input)); found {
				break
			}
		}
	}
	if q.Type == QuestionNoul {
		if found {
			return "yes", true
		}
		return "no", true
	}
	return got, true
}

// SaidOf returns what a routine said to a question from the answer of the
// decider: the chosen option of a choice or a score, and for a noul yes
// when P(yes) is at least 0.5, else no.
func SaidOf(q RoutineQuestion, a Answer) string {
	if q.Type == QuestionNoul {
		if a.Probability >= 0.5 {
			return "yes"
		}
		return "no"
	}
	return a.Choice
}

// RoutineApplies decides whether a routine applies from the answers of the
// decider, keyed by question name: every question passes its band and its
// expected answers. It returns the reason when the routine does not apply,
// one clause per question that fails, such as "type below act: 0.61" or
// "beyond expected no: 0.12".
func RoutineApplies(r Routine, answers map[string]Answer) (bool, string) {
	var why []string
	for _, name := range r.QuestionNames() {
		q := r.Questions[name]
		a, ok := answers[name]
		if !ok {
			why = append(why, name+" has no answer")
			continue
		}
		if q.Type == QuestionNoul {
			py := a.Probability
			switch {
			case len(q.Expect) == 1 && q.Expect[0] == "no":
				if py > 1-q.Act {
					why = append(why, fmt.Sprintf("%s expected no: %.2f", name, py))
				}
			case len(q.Expect) == 1 && q.Expect[0] == "yes":
				if py < q.Act {
					why = append(why, fmt.Sprintf("%s expected yes: %.2f", name, py))
				}
			default:
				if p := max(py, 1-py); p < q.Act {
					why = append(why, fmt.Sprintf("%s below act: %.2f", name, p))
				}
			}
			continue
		}
		if a.Probability < q.Act {
			why = append(why, fmt.Sprintf("%s below act: %.2f", name, a.Probability))
		}
		if len(q.Expect) > 0 && !slices.Contains(q.Expect, a.Choice) {
			why = append(why, fmt.Sprintf("%s %s not expected", name, a.Choice))
		}
	}
	return len(why) == 0, strings.Join(why, "; ")
}
