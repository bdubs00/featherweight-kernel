package routine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Kinds of a hole of a proposal.
const (
	HoleFind     = "find"     // a value from the input or an earlier result
	HoleQuestion = "question" // a value from a small set: a question for the decider
	HoleTemplate = "template" // a text made of other holes
	HoleFree     = "free"     // anything else: a person templates it
)

// ProposeOptions are the options of Propose. Name and Decider name the
// routine and its decider. A value becomes a question when it has at most
// MaxOptions distinct values over at least MinRuns runs; zero takes 8 and
// 10.
type ProposeOptions struct {
	Name       string
	Decider    string
	MaxOptions int
	MinRuns    int
}

// Proposal says how Propose made a routine. Runs are the runs that it
// used: the runs with the most common sequence of tool calls, which is
// Sequence. Divergent are the completed runs with another sequence, and
// Agreement is the share of the completed runs with the sequence. Excluded
// are the runs that it did not read, with the reason. Holes lists every
// value that is not the same in every run, with its kind. Complete is
// false while a free hole is left.
type Proposal struct {
	Runs      []string     `json:"runs"`
	Divergent []string     `json:"divergent,omitempty"`
	Excluded  []Unreadable `json:"excluded,omitempty"`
	Sequence  []string     `json:"sequence"`
	Agreement float64      `json:"agreement"`
	Holes     []Hole       `json:"holes"`
	Complete  bool         `json:"complete"`
}

// Hole is one value of a proposal that differs between runs. Step is the
// place of the call, from 1, and 0 for the answer. Path names the value in
// the input of the call, such as labels[0]. Name is the question, or the
// source of a find. Values counts the values, at most 8 of them, and
// Distinct is the number of distinct values.
type Hole struct {
	Step     int            `json:"step"`
	Tool     string         `json:"tool,omitempty"`
	Path     string         `json:"path"`
	Kind     string         `json:"kind"`
	Name     string         `json:"name,omitempty"`
	Template string         `json:"template"`
	Values   map[string]int `json:"values,omitempty"`
	Distinct int            `json:"distinct"`
}

// Propose makes a routine from verified runs of an agent (docs/ROUTINES.md
// section 3.6): the runs that completed and that no one sent back. The most
// common sequence of tool names is the sequence of the steps. For each
// value of the input of each call, and of a JSON answer: a value that is
// the same in every run is a constant; a value that the input of the run
// or an earlier result holds becomes a find over that source, with the
// shortest context that finds it in every run; a value from a small set
// becomes a question with those values as options and other, observed from
// the tool and the context that showed it; a text made of such values
// becomes a template; anything else is a free hole, {{/* free <path> */}},
// which a person templates or hands to the agent.
func Propose(runs [][]fw.Event, opts ProposeOptions) (fw.Routine, Proposal) {
	if opts.MaxOptions <= 0 {
		opts.MaxOptions = 8
	}
	if opts.MinRuns <= 0 {
		opts.MinRuns = 10
	}
	prop := Proposal{Runs: []string{}, Holes: []Hole{}}
	var done []record
	for _, events := range runs {
		rec := readRun(events)
		switch {
		case !rec.done:
			prop.Excluded = append(prop.Excluded, Unreadable{Run: rec.id, Reason: "the run did not complete"})
		case sentBack(events):
			prop.Excluded = append(prop.Excluded, Unreadable{Run: rec.id, Reason: "the run was sent back"})
		default:
			done = append(done, rec)
		}
	}
	if len(done) == 0 {
		return fw.Routine{}, prop
	}
	// The most common sequence of tool names; the first run breaks a tie.
	key := func(r record) string {
		var names []string
		for _, c := range r.calls {
			names = append(names, c.tool)
		}
		return strings.Join(names, "\x00")
	}
	count := map[string]int{}
	best := ""
	for _, r := range done {
		k := key(r)
		count[k]++
		if count[k] > count[best] || best == "" {
			best = k
		}
	}
	var used []record
	for _, r := range done {
		if key(r) == best {
			used = append(used, r)
			prop.Runs = append(prop.Runs, r.id)
		} else {
			prop.Divergent = append(prop.Divergent, r.id)
		}
	}
	prop.Agreement = float64(len(used)) / float64(len(done))
	for _, c := range used[0].calls {
		prop.Sequence = append(prop.Sequence, c.tool)
	}
	p := &proposer{opts: opts, runs: used, names: map[string]bool{}}
	r := p.build()
	if r.Name = opts.Name; r.Name == "" {
		r.Name = "proposed"
	}
	r.Version, r.Standing = 1, fw.RoutineShadow
	if len(r.Questions) > 0 {
		r.Decider = opts.Decider
	}
	prop.Holes = p.holes
	prop.Complete = true
	for _, h := range p.holes {
		if h.Kind == HoleFree {
			prop.Complete = false
		}
	}
	return r, prop
}

// sentBack reports whether a quality receipt of the run says rejected.
func sentBack(events []fw.Event) bool {
	for _, e := range events {
		if e.Type != fw.EvRunAnnotated {
			continue
		}
		var d fw.QualityAnnotatedData
		if e.Decode(&d) == nil && d.Quality != nil && d.Quality.Verification == "rejected" {
			return true
		}
	}
	return false
}

// proposer builds one routine from runs with the same sequence of calls.
type proposer struct {
	opts      ProposeOptions
	runs      []record
	names     map[string]bool // the fields of the template data in use
	questions map[string]fw.RoutineQuestion
	holes     []Hole
	sources   []source // the input, then each result that a template reads
	known     []known  // the values of finds and questions, for templates
}

// source is a text that a find reads: the input of the run, or the result
// of a call, by run.
type source struct {
	field string // the template field: input, or the as of a step
	step  int    // the call, from 0, or -1 for the input
	texts []string
}

// known is a hole whose value a template of text can use.
type known struct {
	expr string   // a template expression, such as .type or (find .input `#(\d+)`)
	vals []string // by run
}

// node is the shape of a JSON value that is the same in every run: an
// object with the same keys, or an array of the same length. Any other
// value is a leaf.
type node struct {
	keys  []string
	kids  map[string]*node
	items []*node
	leaf  *leafVal
}

// leafVal is one value of the input of a call, by run.
type leafVal struct {
	step int // the call, from 0, or -1 for the answer
	tool string
	path string
	vals []string // the value as text: a string unquoted, else its JSON
	raw  []any
	str  bool // a string in every run
	tmpl string
	kind string
}

func (p *proposer) build() fw.Routine {
	n := len(p.runs)
	p.questions = map[string]fw.RoutineQuestion{}
	p.names["input"], p.names["json"], p.names["meta"] = true, true, true
	inputs := make([]string, n)
	for i, r := range p.runs {
		inputs[i] = r.input
	}
	p.sources = []source{{field: "input", step: -1, texts: inputs}}
	// The tree of the input of each call, and the leaves in order.
	calls := len(p.runs[0].calls)
	trees := make([]*node, calls)
	var leaves []*leafVal
	for c := 0; c < calls; c++ {
		vals := make([]any, n)
		for i, r := range p.runs {
			vals[i] = decodeJSON(r.calls[c].input)
		}
		trees[c] = shape(vals, "", c, p.runs[0].calls[c].tool, &leaves)
	}
	// The answer: a text before a JSON object, the same in every run, or a
	// text as a whole.
	prefix, answerTree := answerShape(p.runs, &leaves)
	// Each kind in turn, so a later kind can use the holes of an earlier one.
	var outputs []string
	for _, r := range p.runs {
		outputs = append(outputs, r.output)
	}
	for _, l := range leaves {
		p.constant(l)
	}
	for _, l := range leaves {
		if l.kind == "" {
			p.find(l, false)
		}
	}
	for _, l := range leaves {
		if l.kind == "" && l.step < 0 {
			p.question(l, outputs)
		}
	}
	for _, l := range leaves {
		if l.kind == "" {
			p.find(l, true)
		}
	}
	for _, l := range leaves {
		if l.kind == "" {
			p.template(l)
		}
	}
	for _, l := range leaves {
		if l.kind == "" && l.step >= 0 {
			p.question(l, outputs)
		}
	}
	for _, l := range leaves {
		if l.kind == "" {
			l.kind, l.tmpl = HoleFree, "{{/* free "+holeName(l)+" */}}"
		}
	}
	for _, l := range leaves {
		if l.kind != "constant" {
			p.holes = append(p.holes, hole(l))
		}
	}
	r := fw.Routine{Questions: p.questions}
	if len(r.Questions) == 0 {
		r.Questions = nil
	}
	as := map[int]string{}
	for _, s := range p.sources[1:] {
		as[s.step] = s.field
	}
	for c := 0; c < calls; c++ {
		r.Steps = append(r.Steps, fw.RoutineStep{Tool: p.runs[0].calls[c].tool, Input: render(trees[c]), As: as[c]})
	}
	switch {
	case answerTree != nil:
		r.Answer = escapeText(prefix) + render(answerTree)
	default:
		r.Answer = render(&node{leaf: leaves[len(leaves)-1]})
	}
	return r
}

// holeName names a leaf in the comment of a free hole.
func holeName(l *leafVal) string {
	if l.step < 0 {
		return "answer" + dotted(l.path)
	}
	return fmt.Sprintf("step %d %s", l.step+1, strings.TrimPrefix(l.path, "."))
}

func dotted(path string) string {
	if path == "" {
		return ""
	}
	return " " + strings.TrimPrefix(path, ".")
}

func hole(l *leafVal) Hole {
	counts := map[string]int{}
	for _, v := range l.vals {
		counts[v]++
	}
	top := slices.SortedFunc(maps.Keys(counts), func(a, b string) int {
		if counts[a] != counts[b] {
			return counts[b] - counts[a]
		}
		return strings.Compare(a, b)
	})
	values := map[string]int{}
	for _, v := range top[:min(8, len(top))] {
		values[bound(v)] = counts[v]
	}
	h := Hole{Step: l.step + 1, Tool: l.tool, Path: strings.TrimPrefix(l.path, "."), Kind: l.kind, Template: l.tmpl, Values: values, Distinct: len(counts)}
	if h.Path == "" {
		h.Path = "(whole)"
	}
	if l.step < 0 {
		h.Step = 0
	}
	if m := fieldRef.FindStringSubmatch(l.tmpl); m != nil {
		h.Name = m[1]
	}
	return h
}

// fieldRef finds the first field of a template, for the name of a hole.
var fieldRef = regexp.MustCompile(`\.([a-z][a-z0-9_]*)`)

// decodeJSON decodes an input with its numbers kept as written.
func decodeJSON(raw []byte) any {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return string(raw)
	}
	return v
}

// shape builds the tree of the values of the runs at one place, and adds
// each leaf to leaves.
func shape(vals []any, path string, step int, tool string, leaves *[]*leafVal) *node {
	if m0, ok := vals[0].(map[string]any); ok {
		keys := slices.Sorted(maps.Keys(m0))
		same := true
		for _, v := range vals[1:] {
			m, ok := v.(map[string]any)
			if !ok || !slices.Equal(slices.Sorted(maps.Keys(m)), keys) {
				same = false
				break
			}
		}
		if same {
			n := &node{keys: keys, kids: map[string]*node{}}
			for _, k := range keys {
				kv := make([]any, len(vals))
				for i, v := range vals {
					kv[i] = v.(map[string]any)[k]
				}
				n.kids[k] = shape(kv, path+"."+k, step, tool, leaves)
			}
			return n
		}
	}
	if a0, ok := vals[0].([]any); ok {
		same := true
		for _, v := range vals[1:] {
			if a, ok := v.([]any); !ok || len(a) != len(a0) {
				same = false
				break
			}
		}
		if same {
			n := &node{}
			for j := range a0 {
				iv := make([]any, len(vals))
				for i, v := range vals {
					iv[i] = v.([]any)[j]
				}
				n.items = append(n.items, shape(iv, fmt.Sprintf("%s[%d]", path, j), step, tool, leaves))
			}
			return n
		}
	}
	l := &leafVal{step: step, tool: tool, path: path, raw: vals, str: true}
	for _, v := range vals {
		s, ok := v.(string)
		if !ok {
			l.str = false
			b, _ := json.Marshal(v)
			s = string(b)
		}
		l.vals = append(l.vals, s)
	}
	*leaves = append(*leaves, l)
	return &node{leaf: l}
}

// answerShape reads the answers of the runs: a text before a JSON object
// that every run shares, and the tree of the object. Without such an
// object, the answer is one text leaf, the last of leaves.
func answerShape(runs []record, leaves *[]*leafVal) (string, *node) {
	var prefix string
	objs := make([]any, len(runs))
	ok := true
	for i, r := range runs {
		at := strings.IndexByte(r.output, '{')
		if at < 0 {
			ok = false
			break
		}
		var v map[string]any
		dec := json.NewDecoder(strings.NewReader(r.output[at:]))
		dec.UseNumber()
		if dec.Decode(&v) != nil || dec.More() || (i > 0 && r.output[:at] != prefix) {
			ok = false
			break
		}
		prefix, objs[i] = r.output[:at], v
	}
	if ok {
		return prefix, shape(objs, "", -1, "", leaves)
	}
	l := &leafVal{step: -1, str: true}
	for _, r := range runs {
		l.vals = append(l.vals, r.output)
		l.raw = append(l.raw, r.output)
	}
	*leaves = append(*leaves, l)
	return "", nil
}

// constant marks a leaf that is the same in every run.
func (p *proposer) constant(l *leafVal) {
	for _, v := range l.vals[1:] {
		if v != l.vals[0] {
			return
		}
	}
	l.kind = "constant"
	if l.step < 0 && l.path == "" {
		l.tmpl = escapeText(l.vals[0]) // a whole answer is text
		return
	}
	b, _ := json.Marshal(l.raw[0])
	l.tmpl = escapeText(string(b))
}

// find makes a leaf a find over the input or an earlier result: the
// shortest context that finds its value first in every run. With printf,
// the context can hold the value of a question, as in the label of a
// type.
func (p *proposer) find(l *leafVal, printf bool) {
	if printf && len(p.questions) == 0 {
		return
	}
	for _, src := range p.candidates(l) {
		pattern, q, ok := p.context(src.texts, l.vals, isNumber(l), printf, nil)
		if !ok {
			continue
		}
		field := p.sourceField(src, l)
		expr := "find ." + field + " " + quoteArg(pattern)
		if q != "" {
			expr = "find ." + field + " (printf " + quoteArg(strings.ReplaceAll(pattern, "%", "%%")) + " ." + q + ")"
			expr = strings.Replace(expr, "%%s", "%s", 1)
		}
		l.kind = HoleFind
		l.tmpl = "{{ " + expr + " }}"
		if l.str && !(l.step < 0 && l.path == "") {
			l.tmpl = "{{ json (" + expr + ") }}"
		}
		p.known = append(p.known, known{expr: "(" + expr + ")", vals: l.vals})
		return
	}
}

// candidates returns the sources that a leaf can read: the input, and the
// results of the calls before it, or of every call for the answer.
func (p *proposer) candidates(l *leafVal) []source {
	out := []source{p.sources[0]}
	last := l.step
	if last < 0 {
		last = len(p.runs[0].calls)
	}
	for c := 0; c < last; c++ {
		texts := make([]string, len(p.runs))
		for i, r := range p.runs {
			texts[i] = r.calls[c].result
		}
		out = append(out, source{step: c, texts: texts})
	}
	return out
}

// sourceField returns the template field of a source, and names the
// result of a call with the as of its step the first time.
func (p *proposer) sourceField(src source, l *leafVal) string {
	if src.step < 0 {
		return "input"
	}
	for _, s := range p.sources[1:] {
		if s.step == src.step {
			return s.field
		}
	}
	tool := p.runs[0].calls[src.step].tool
	if i := strings.LastIndexByte(tool, '.'); i >= 0 {
		tool = tool[i+1:]
	}
	src.field = p.newName(tool)
	p.sources = append(p.sources, src)
	return src.field
}

// context searches the shortest context around the value of each run in
// its text, before and after, that a pattern with one capture group can
// use: the pattern finds the value of the run first in every text. With
// printf, the context can hold the value of a question in place of %s; it
// returns the name of that question. check, when set, decides whether a
// pattern without printf fits, in place of the match over texts.
func (p *proposer) context(texts, vals []string, number, printf bool, check func(string) bool) (string, string, bool) {
	if check == nil {
		check = func(pattern string) bool { return matchesAll(pattern, texts, vals) }
	}
	for i, t := range texts {
		if vals[i] == "" || !strings.Contains(t, vals[i]) {
			return "", "", false
		}
	}
	capture := `(.+?)`
	if number {
		capture = `(-?\d+(?:\.\d+)?)`
	}
	t0, v0 := texts[0], vals[0]
	var at []int
	for from := 0; len(at) < 8; {
		i := strings.Index(t0[from:], v0)
		if i < 0 {
			break
		}
		at = append(at, from+i)
		from += i + 1
	}
	var names []string
	if printf {
		names = slices.Sorted(maps.Keys(p.questions))
	}
	for size := 1; size <= 40; size++ {
		for before := size; before >= 0; before-- {
			after := size - before
			if !number && after == 0 {
				continue // a text needs a mark after it
			}
			for _, pos := range at {
				if pos-before < 0 || pos+len(v0)+after > len(t0) {
					continue
				}
				pre, post := t0[pos-before:pos], t0[pos+len(v0):pos+len(v0)+after]
				if !printf {
					pattern := regexp.QuoteMeta(pre) + capture + regexp.QuoteMeta(post)
					if check(pattern) {
						return pattern, "", true
					}
					continue
				}
				for _, q := range names {
					qv := p.answers(q)
					if qv[0] == "" || !strings.Contains(pre+"\x00"+post, qv[0]) {
						continue
					}
					ok := true
					for i := range texts {
						pi := strings.Replace(pre, qv[0], qv[i], 1)
						si := strings.Replace(post, qv[0], qv[i], 1)
						if !matchesAll(regexp.QuoteMeta(pi)+capture+regexp.QuoteMeta(si), texts[i:i+1], vals[i:i+1]) {
							ok = false
							break
						}
					}
					if ok {
						pattern := regexp.QuoteMeta(strings.Replace(pre, qv[0], "\x01", 1)) + capture + regexp.QuoteMeta(strings.Replace(post, qv[0], "\x01", 1))
						return strings.Replace(pattern, "\x01", "%s", 1), q, true
					}
				}
			}
		}
	}
	return "", "", false
}

// answers returns the value of a question in each run.
func (p *proposer) answers(q string) []string {
	for _, k := range p.known {
		if k.expr == "."+q {
			return k.vals
		}
	}
	return make([]string, len(p.runs))
}

func matchesAll(pattern string, texts, vals []string) bool {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false
	}
	for i, t := range texts {
		m := re.FindStringSubmatch(t)
		if m == nil || m[1] != vals[i] {
			return false
		}
	}
	return true
}

// keyPattern returns a pattern that reads the value of a leaf after its
// JSON key, such as "type"\s*:\s*"(.+?)", or "" for a leaf without a key.
func keyPattern(l *leafVal) string {
	i := strings.LastIndexByte(l.path, '.')
	if i < 0 || strings.ContainsAny(l.path[i+1:], "[]") {
		return ""
	}
	key := regexp.QuoteMeta(`"`+l.path[i+1:]+`"`) + `\s*:\s*`
	if isNumber(l) {
		return key + `(-?\d+(?:\.\d+)?)`
	}
	if !l.str {
		return ""
	}
	return key + `"(.+?)"`
}

func isNumber(l *leafVal) bool {
	for _, v := range l.raw {
		if _, ok := v.(json.Number); !ok {
			return false
		}
	}
	return true
}

// question makes a leaf from a small set of values a question with those
// values as options and other. outputs are the answers of the runs, for
// the observe block of a value of the answer.
func (p *proposer) question(l *leafVal, outputs []string) {
	counts := map[string]int{}
	for _, v := range l.vals {
		counts[v]++
	}
	if len(p.runs) < p.opts.MinRuns || len(counts) > p.opts.MaxOptions || len(counts) < 2 {
		return
	}
	if !l.str && !isNumber(l) {
		return
	}
	// observe reads the value where the agent wrote it.
	texts := outputs
	obs := &fw.Observe{}
	if l.step >= 0 {
		texts = make([]string, len(p.runs))
		for i, r := range p.runs {
			texts[i] = string(r.calls[l.step].input)
		}
		obs.Tool = l.tool
	}
	// The pattern must read the value of every run as ObserveAnswer reads
	// it: over the output, or over the calls of the tool, the first match.
	check := func(pattern string) bool {
		o := *obs
		if l.step >= 0 {
			o.Match = pattern
		} else {
			o.Answer = pattern
		}
		q := fw.RoutineQuestion{Question: fw.Question{Type: fw.QuestionChoice}, Observe: &o}
		for i, r := range p.runs {
			if got, _ := fw.ObserveAnswer(q, r.output, r.events); got != l.vals[i] {
				return false
			}
		}
		return true
	}
	// A JSON key before the value reads best and stands a change of the
	// spaces; else the shortest context that reads every run.
	pattern, ok := keyPattern(l), false
	if pattern != "" && check(pattern) {
		ok = true
	} else {
		pattern, _, ok = p.context(texts, l.vals, isNumber(l), false, check)
	}
	if !ok {
		return
	}
	if l.step >= 0 {
		obs.Match = pattern
	} else {
		obs.Answer = pattern
	}
	base := strings.TrimPrefix(l.path, ".")
	if base == "" {
		base = "answer"
	}
	name := p.newName(base)
	criteria := map[string]any{"other": "None of the values above"}
	var expect fw.Expect
	for _, v := range slices.Sorted(maps.Keys(counts)) {
		criteria[v] = fmt.Sprintf("The agent chose %s in %d of %d runs; describe when it applies", v, counts[v], len(p.runs))
		expect = append(expect, v)
	}
	where := "the answer"
	if l.step >= 0 {
		where = fmt.Sprintf("%s of the call of %s", strings.TrimPrefix(l.path, "."), l.tool)
	}
	p.questions[name] = fw.RoutineQuestion{
		Question: fw.Question{Type: fw.QuestionChoice, Instructions: "Which value fits this run for " + where + "?", Criteria: criteria},
		Act:      0.9, Expect: expect, Observe: obs,
	}
	l.kind, l.tmpl = HoleQuestion, "{{ ."+name+" }}"
	if l.str && !(l.step < 0 && l.path == "") {
		l.tmpl = "{{ json ." + name + " }}"
	}
	p.known = append(p.known, known{expr: "." + name, vals: l.vals})
}

// template makes a text leaf a template of the values of other holes:
// each run gives the same text once those values are taken out.
func (p *proposer) template(l *leafVal) {
	if !l.str || len(p.known) == 0 {
		return
	}
	var format string
	var args []string
	for i, v := range l.vals {
		f, a := v, []string(nil)
		// The longest values first, so a value inside another stays.
		order := slices.Clone(p.known)
		slices.SortStableFunc(order, func(x, y known) int { return len(y.vals[i]) - len(x.vals[i]) })
		marks := map[string]string{}
		for k, h := range order {
			if len(h.vals[i]) < 2 || !strings.Contains(f, h.vals[i]) {
				continue
			}
			mark := fmt.Sprintf("\x00%d\x00", k)
			marks[mark] = h.expr
			f = strings.ReplaceAll(f, h.vals[i], mark)
		}
		var used []string
		for _, m := range regexp.MustCompile("\x00[0-9]+\x00").FindAllString(f, -1) {
			used = append(used, marks[m])
		}
		f = regexp.MustCompile("\x00[0-9]+\x00").ReplaceAllString(strings.ReplaceAll(f, "%", "%%"), "%s")
		a = used
		if i == 0 {
			format, args = f, a
			if len(args) == 0 {
				return
			}
			continue
		}
		if f != format || !slices.Equal(a, args) {
			return
		}
	}
	expr := "printf " + quoteArg(format) + " " + strings.Join(args, " ")
	l.kind = HoleTemplate
	if l.step < 0 && l.path == "" {
		l.tmpl = "{{ " + expr + " }}"
		return
	}
	l.tmpl = "{{ json (" + expr + ") }}"
}

// newName returns a template field for a base, such as a JSON key: a
// field name that no other field of the routine has.
func (p *proposer) newName(base string) string {
	b := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 'a' - 'A'
		}
		return '_'
	}, base)
	b = strings.Trim(regexp.MustCompile(`_+`).ReplaceAllString(b, "_"), "_")
	if b == "" || b[0] < 'a' || b[0] > 'z' {
		b = "v_" + b
	}
	if len(b) > 28 {
		b = strings.TrimRight(b[:28], "_")
	}
	name := b
	for i := 2; p.names[name]; i++ {
		name = b + "_" + strconv.Itoa(i)
	}
	p.names[name] = true
	return name
}

// quoteArg writes a string argument of a template: raw when it can be.
func quoteArg(s string) string {
	if !strings.Contains(s, "`") {
		return "`" + s + "`"
	}
	return strconv.Quote(s)
}

// escapeText keeps a text of a constant from opening a template action.
func escapeText(s string) string { return strings.ReplaceAll(s, "{{", "{{`{{`}}") }

// render writes the template of a tree: JSON with every leaf replaced by
// its template.
func render(n *node) string {
	switch {
	case n.leaf != nil:
		return n.leaf.tmpl
	case n.kids != nil:
		parts := make([]string, 0, len(n.keys))
		for _, k := range n.keys {
			b, _ := json.Marshal(k)
			parts = append(parts, escapeText(string(b))+": "+render(n.kids[k]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	parts := make([]string, 0, len(n.items))
	for _, it := range n.items {
		parts = append(parts, render(it))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
