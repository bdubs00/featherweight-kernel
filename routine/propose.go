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
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Kinds of a hole of a proposal.
const (
	HoleFind     = "find"     // a value from the input or an earlier result
	HoleQuestion = "question" // a value from a small set: a question for the decider
	HoleFree     = "free"     // anything else: a person fills it
)

// A value becomes a question when it has at most maxOptions distinct
// values over at least minRuns runs.
const (
	maxOptions = 8
	minRuns    = 10
)

// ProposeOptions are the options of Propose: the name of the routine and
// the decider of its questions.
type ProposeOptions struct {
	Name    string
	Decider string
}

// Proposal says how Propose made a routine. Runs are the runs that it
// used: the runs with the most common sequence of tool calls, which is
// Sequence, and lists of the same length. Divergent are the completed runs
// that it did not use, with the reason, and Agreement is the share of the
// completed runs that it used. Excluded are the runs that it did not read.
// Holes lists every value that is not the same in every run, with its
// kind. Dropped names the keys that some runs leave out and the others
// set to one value, which the routine leaves out. Problems are what the
// kernel would refuse in the routine. Complete is false while a free hole
// or a problem is left.
type Proposal struct {
	Runs      []string     `json:"runs"`
	Divergent []Unreadable `json:"divergent,omitempty"`
	Excluded  []Unreadable `json:"excluded,omitempty"`
	Sequence  []string     `json:"sequence"`
	Agreement float64      `json:"agreement"`
	Holes     []Hole       `json:"holes"`
	Dropped   []string     `json:"dropped,omitempty"`
	Problems  []string     `json:"problems,omitempty"`
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
// common sequence of tool names is the sequence of the steps, and a list
// in an input has the length that most runs give it. For each value of the
// input of each call, and of a JSON answer:
//   - a value that is the same in every run is a constant;
//   - a key that some runs leave out and the others set to one value is
//     left out;
//   - a value that the input of the run or an earlier result holds becomes
//     a find over that source, with a context that reads every run: the
//     word before the value first, else the shortest context;
//   - a value from a small set becomes a question with those values as
//     options and other, observed where the agent wrote it;
//   - a value whose context in a result holds the value of a question
//     becomes a find with printf, such as the label of a type;
//   - anything else is a free hole, {{/* free <place> */}}.
func Propose(runs [][]fw.Event, opts ProposeOptions) (fw.Routine, Proposal) {
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
	used := majority(done, sequenceOf, "another sequence of calls", &prop)
	used = majority(used, listsOf, "lists of another length in its calls", &prop)
	prop.Agreement = float64(len(used)) / float64(len(done))
	for _, r := range used {
		prop.Runs = append(prop.Runs, r.id)
	}
	for _, c := range used[0].calls {
		prop.Sequence = append(prop.Sequence, c.tool)
	}
	p := &proposer{runs: used, names: map[string]bool{"input": true, "json": true, "meta": true}, answers: map[string][]string{}}
	r := p.build()
	if r.Name = opts.Name; r.Name == "" {
		r.Name = "proposed"
	}
	r.Version, r.Standing = 1, fw.RoutineShadow
	if len(r.Questions) > 0 {
		r.Decider = opts.Decider
	}
	prop.Holes, prop.Dropped = p.holes, p.dropped
	prop.Problems = fw.RoutineProblems(nil, []fw.Routine{r})
	prop.Complete = len(prop.Problems) == 0
	for _, h := range p.holes {
		if h.Kind == HoleFree {
			prop.Complete = false
		}
	}
	return r, prop
}

// majority keeps the runs whose key is the most common one, the first run
// breaking a tie, and lists the others as divergent with the reason.
func majority(runs []record, key func(record) string, reason string, prop *Proposal) []record {
	count := map[string]int{}
	best := key(runs[0])
	for _, r := range runs {
		k := key(r)
		count[k]++
		if count[k] > count[best] {
			best = k
		}
	}
	var out []record
	for _, r := range runs {
		if key(r) == best {
			out = append(out, r)
		} else {
			prop.Divergent = append(prop.Divergent, Unreadable{Run: r.id, Reason: reason})
		}
	}
	return out
}

// sequenceOf is the sequence of the tool names of a run.
func sequenceOf(r record) string {
	var names []string
	for _, c := range r.calls {
		names = append(names, c.tool)
	}
	return strings.Join(names, "\x00")
}

// listsOf names the length of each list in the inputs of the calls of a
// run, so runs with lists of the same lengths have the same key.
func listsOf(r record) string {
	var b strings.Builder
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for _, k := range slices.Sorted(maps.Keys(x)) {
				walk(path+"."+k, x[k])
			}
		case []any:
			fmt.Fprintf(&b, "%s=%d;", path, len(x))
			for i, e := range x {
				walk(fmt.Sprintf("%s[%d]", path, i), e)
			}
		}
	}
	for i, c := range r.calls {
		walk(strconv.Itoa(i), decodeJSON(c.input))
	}
	return b.String()
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

// proposer builds one routine from runs with the same calls.
type proposer struct {
	runs      []record
	names     map[string]bool // the fields of the template data in use
	questions map[string]fw.RoutineQuestion
	answers   map[string][]string // the value of each question, by run
	sources   []source            // the results that a find reads, with their as
	holes     []Hole
	dropped   []string
}

// source is the result of a call that a find reads, by its as.
type source struct {
	field string
	step  int
}

// node is the shape of a JSON value that is the same in every run: an
// object, or a list of the same length. Any other value is a leaf.
type node struct {
	keys  []string
	kids  map[string]*node
	items []*node
	leaf  *leafVal
}

// leafVal is one value of the input of a call, or of the answer, by run.
type leafVal struct {
	step int // the call, from 0, or -1 for the answer
	tool string
	path string
	vals []string // the value as text: a string unquoted, else its JSON
	raw  []any
	str  bool // a string in every run
	kind string
	name string // the question, or the source of a find
	tmpl string
}

func (p *proposer) build() fw.Routine {
	p.questions = map[string]fw.RoutineQuestion{}
	calls := len(p.runs[0].calls)
	trees := make([]*node, calls)
	var leaves []*leafVal
	for c := 0; c < calls; c++ {
		vals := make([]any, len(p.runs))
		for i, r := range p.runs {
			vals[i] = decodeJSON(r.calls[c].input)
		}
		trees[c] = p.shape(vals, "", c, p.runs[0].calls[c].tool, &leaves)
	}
	prefix, answerTree := p.answerShape(&leaves)
	// Each pass in turn, so a later pass can use the questions of an
	// earlier one. The values of the answer become questions first.
	passes := []func(*leafVal) bool{
		p.constant,
		func(l *leafVal) bool { return p.find(l, false) },
		func(l *leafVal) bool { return l.step < 0 && p.question(l) },
		func(l *leafVal) bool { return p.find(l, true) },
		func(l *leafVal) bool { return l.step >= 0 && p.question(l) },
	}
	for _, pass := range passes {
		for _, l := range leaves {
			if l.kind == "" {
				pass(l)
			}
		}
	}
	for _, l := range leaves {
		if l.kind == "" {
			p.free(l)
		}
		if l.kind != "constant" {
			p.holes = append(p.holes, hole(l))
		}
	}
	r := fw.Routine{Questions: p.questions}
	if len(r.Questions) == 0 {
		r.Questions = nil
	}
	as := map[int]string{}
	for _, s := range p.sources {
		as[s.step] = s.field
	}
	for c := 0; c < calls; c++ {
		r.Steps = append(r.Steps, fw.RoutineStep{Tool: p.runs[0].calls[c].tool, Input: render(trees[c]), As: as[c]})
	}
	if answerTree != nil {
		r.Answer = escapeText(prefix) + render(answerTree)
	} else {
		r.Answer = leaves[len(leaves)-1].tmpl
	}
	return r
}

// place names a leaf for a person, such as "step 2 labels[0]" or "answer
// type".
func place(l *leafVal) string {
	where := "answer"
	if l.step >= 0 {
		where = fmt.Sprintf("step %d", l.step+1)
	}
	switch path := strings.TrimPrefix(l.path, "."); {
	case path != "":
		return where + " " + path
	case l.step >= 0:
		return where + " input"
	}
	return where
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
	h := Hole{Step: l.step + 1, Tool: l.tool, Path: strings.TrimPrefix(l.path, "."), Kind: l.kind, Name: l.name,
		Template: l.tmpl, Values: values, Distinct: len(counts)}
	if h.Path == "" {
		h.Path = "(whole)"
	}
	return h
}

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
// each leaf to leaves. In an object, a key that some runs leave out is
// dropped when the others agree on its value, and is a free hole of its
// own otherwise. A list has the same length in every run (listsOf).
func (p *proposer) shape(vals []any, path string, step int, tool string, leaves *[]*leafVal) *node {
	objs := make([]map[string]any, len(vals))
	isObj := true
	for i, v := range vals {
		if objs[i], isObj = v.(map[string]any); !isObj {
			break
		}
	}
	if isObj {
		keys := map[string]bool{}
		for _, o := range objs {
			for k := range o {
				keys[k] = true
			}
		}
		n := &node{kids: map[string]*node{}}
		for _, k := range slices.Sorted(maps.Keys(keys)) {
			kv := make([]any, len(vals))
			var present []string
			for i, o := range objs {
				if v, ok := o[k]; ok {
					kv[i] = v
					b, _ := json.Marshal(v)
					present = append(present, string(b))
				}
			}
			if len(present) < len(vals) {
				if len(slices.Compact(slices.Sorted(slices.Values(present)))) == 1 {
					p.dropped = append(p.dropped, place(&leafVal{step: step, path: path + "." + k}))
					continue
				}
				l := &leafVal{step: step, tool: tool, path: path + "." + k, raw: kv}
				for _, v := range kv {
					b, _ := json.Marshal(v)
					l.vals = append(l.vals, string(b))
				}
				p.free(l)
				*leaves = append(*leaves, l)
				n.keys, n.kids[k] = append(n.keys, k), &node{leaf: l}
				continue
			}
			n.keys, n.kids[k] = append(n.keys, k), p.shape(kv, path+"."+k, step, tool, leaves)
		}
		return n
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
			n := &node{items: []*node{}}
			for j := range a0 {
				iv := make([]any, len(vals))
				for i, v := range vals {
					iv[i] = v.([]any)[j]
				}
				n.items = append(n.items, p.shape(iv, fmt.Sprintf("%s[%d]", path, j), step, tool, leaves))
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
func (p *proposer) answerShape(leaves *[]*leafVal) (string, *node) {
	var prefix string
	objs := make([]any, len(p.runs))
	ok := true
	for i, r := range p.runs {
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
		return prefix, p.shape(objs, "", -1, "", leaves)
	}
	l := &leafVal{step: -1, str: true}
	for _, r := range p.runs {
		l.vals = append(l.vals, r.output)
		l.raw = append(l.raw, r.output)
	}
	*leaves = append(*leaves, l)
	return "", nil
}

// whole reports whether a leaf is the whole answer as text, which renders
// without JSON.
func (l *leafVal) whole() bool { return l.step < 0 && l.path == "" }

// constant marks a leaf that is the same in every run.
func (p *proposer) constant(l *leafVal) bool {
	for _, v := range l.vals[1:] {
		if v != l.vals[0] {
			return false
		}
	}
	l.kind = "constant"
	if l.whole() {
		l.tmpl = escapeText(l.vals[0])
		return true
	}
	b, _ := json.Marshal(l.raw[0])
	l.tmpl = escapeText(string(b))
	return true
}

// free marks a leaf that a person fills.
func (p *proposer) free(l *leafVal) {
	l.kind, l.tmpl = HoleFree, "{{/* free "+place(l)+" */}}"
}

// find makes a leaf a find over the input or an earlier result. With
// withQuestion, the context of the value holds the value of a question,
// through printf, as the label of a type in a list of labels.
func (p *proposer) find(l *leafVal, withQuestion bool) bool {
	names := []string{""}
	if withQuestion {
		names = slices.Sorted(maps.Keys(p.questions))
	}
	for _, src := range p.sourcesOf(l) {
		for _, q := range names {
			texts := src.texts
			pattern, ok := p.context(texts, l.vals, isNumber(l), q, func(patterns []string) bool {
				return matchesEach(patterns, texts, l.vals)
			})
			if !ok {
				continue
			}
			field := p.field(src.step)
			expr := "find ." + field + " " + quoteArg(pattern)
			if q != "" {
				expr = "find ." + field + " (printf " + quoteArg(printfFormat(pattern)) + " ." + q + ")"
			}
			l.kind, l.name, l.tmpl = HoleFind, field, "{{ "+expr+" }}"
			if l.str && !l.whole() {
				l.tmpl = "{{ json (" + expr + ") }}"
			}
			return true
		}
	}
	return false
}

// printfFormat makes a pattern with one %s a format of printf: every other
// % doubles.
func printfFormat(pattern string) string {
	at := strings.Index(pattern, "%s")
	return strings.ReplaceAll(pattern[:at], "%", "%%") + "%s" + strings.ReplaceAll(pattern[at+2:], "%", "%%")
}

// textSource is a text that a find can read, by run: the input (step -1)
// or the result of a call.
type textSource struct {
	step  int
	texts []string
}

// sourcesOf returns the texts that a leaf can read: the input, and the
// results of the calls before it, or of every call for the answer.
func (p *proposer) sourcesOf(l *leafVal) []textSource {
	inputs := make([]string, len(p.runs))
	for i, r := range p.runs {
		inputs[i] = r.input
	}
	out := []textSource{{step: -1, texts: inputs}}
	last := l.step
	if last < 0 {
		last = len(p.runs[0].calls)
	}
	for c := 0; c < last; c++ {
		texts := make([]string, len(p.runs))
		for i, r := range p.runs {
			texts[i] = r.calls[c].result
		}
		out = append(out, textSource{step: c, texts: texts})
	}
	return out
}

// field returns the template field of a source: input, or the as of the
// step of a result, which it names the first time.
func (p *proposer) field(step int) string {
	if step < 0 {
		return "input"
	}
	for _, s := range p.sources {
		if s.step == step {
			return s.field
		}
	}
	tool := p.runs[0].calls[step].tool
	if i := strings.LastIndexByte(tool, '.'); i >= 0 {
		tool = tool[i+1:]
	}
	s := source{field: p.newName(tool), step: step}
	p.sources = append(p.sources, s)
	return s.field
}

// The capture groups of a context, and the word before a value that
// labels it, such as "Number: " or "id":.
var (
	numberGroup = `(-?\d+(?:\.\d+)?)`
	textGroup   = `(.+?)`
	labelBefore = regexp.MustCompile(`[A-Za-z][A-Za-z0-9_]*[^A-Za-z0-9\n]{0,4}$`)
)

// context finds a context around the value of each run in its text that a
// pattern with one capture group reads: the word that labels the value
// first, such as Number: before 12, else the shortest context, before and
// after. It builds the pattern of each run from the context in the text of
// the first run. With the question q, the context holds the value of the
// question, which the pattern of each run takes from its run, and the
// result has %s in its place. fits decides whether the patterns of the
// runs fit.
func (p *proposer) context(texts, vals []string, number bool, q string, fits func(patterns []string) bool) (string, bool) {
	for i, t := range texts {
		if vals[i] == "" || !strings.Contains(t, vals[i]) {
			return "", false
		}
	}
	group := textGroup
	if number {
		group = numberGroup
	}
	var qv []string
	if q != "" {
		qv = p.answers[q]
	}
	try := func(pre, post string) (string, bool) {
		if !number && post == "" {
			return "", false // a text needs a mark after it
		}
		if q == "" {
			pattern := regexp.QuoteMeta(pre) + group + regexp.QuoteMeta(post)
			return pattern, fits(slices.Repeat([]string{pattern}, len(texts)))
		}
		if qv[0] == "" || !strings.Contains(pre+"\x00"+post, qv[0]) {
			return "", false
		}
		patterns := make([]string, len(texts))
		for i := range texts {
			patterns[i] = regexp.QuoteMeta(strings.Replace(pre, qv[0], qv[i], 1)) + group + regexp.QuoteMeta(strings.Replace(post, qv[0], qv[i], 1))
		}
		if !fits(patterns) {
			return "", false
		}
		mark := "\x01"
		pattern := regexp.QuoteMeta(strings.Replace(pre, qv[0], mark, 1)) + group + regexp.QuoteMeta(strings.Replace(post, qv[0], mark, 1))
		return strings.Replace(pattern, mark, "%s", 1), true
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
	for _, pos := range at {
		if pre := labelBefore.FindString(t0[:pos]); pre != "" {
			post := ""
			if end := pos + len(v0); !number && end < len(t0) {
				_, n := utf8.DecodeRuneInString(t0[end:])
				post = t0[end : end+n]
			}
			if pattern, ok := try(pre, post); ok {
				return pattern, true
			}
		}
	}
	for size := 1; size <= 40; size++ {
		for before := size; before >= 0; before-- {
			after := size - before
			for _, pos := range at {
				end := pos + len(v0) + after
				if pos-before < 0 || end > len(t0) || !utf8.RuneStart(t0[pos-before]) || (end < len(t0) && !utf8.RuneStart(t0[end])) {
					continue
				}
				if pattern, ok := try(t0[pos-before:pos], t0[pos+len(v0):pos+len(v0)+after]); ok {
					return pattern, true
				}
			}
		}
	}
	return "", false
}

// matchesEach reports whether the pattern of each run reads its value
// first in its text.
func matchesEach(patterns, texts, vals []string) bool {
	for i, t := range texts {
		re, err := regexp.Compile(patterns[i])
		if err != nil {
			return false
		}
		if m := re.FindStringSubmatch(t); m == nil || m[1] != vals[i] {
			return false
		}
	}
	return true
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
// values as options and other. The observe block reads the value where the
// agent wrote it: in the answer, or in the input of its call.
func (p *proposer) question(l *leafVal) bool {
	counts := map[string]int{}
	for _, v := range l.vals {
		counts[v]++
	}
	if len(p.runs) < minRuns || len(counts) > maxOptions || len(counts) < 2 || (!l.str && !isNumber(l)) {
		return false
	}
	texts := make([]string, len(p.runs))
	obs := fw.Observe{}
	for i, r := range p.runs {
		texts[i] = r.output
		if l.step >= 0 {
			texts[i] = string(r.calls[l.step].input)
		}
	}
	if l.step >= 0 {
		obs.Tool = l.tool
	}
	// The pattern must read the value of every run as ObserveAnswer reads
	// it: over the answer, or over the calls of the tool, the first match.
	pattern, ok := p.context(texts, l.vals, isNumber(l), "", func(patterns []string) bool {
		o := obs
		if l.step >= 0 {
			o.Match = patterns[0]
		} else {
			o.Answer = patterns[0]
		}
		q := fw.RoutineQuestion{Question: fw.Question{Type: fw.QuestionChoice}, Observe: &o}
		for i, r := range p.runs {
			if got, _ := fw.ObserveAnswer(q, r.output, r.events); got != l.vals[i] {
				return false
			}
		}
		return true
	})
	if !ok {
		return false
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
	p.questions[name] = fw.RoutineQuestion{
		Question: fw.Question{Type: fw.QuestionChoice, Instructions: "Which value fits this run for " + place(l) + "?", Criteria: criteria},
		Act:      0.9, Expect: expect, Observe: &obs,
	}
	p.answers[name] = l.vals
	l.kind, l.name, l.tmpl = HoleQuestion, name, "{{ ."+name+" }}"
	if l.str && !l.whole() {
		l.tmpl = "{{ json ." + name + " }}"
	}
	return true
}

// notField finds the marks that a template field cannot hold.
var notField = regexp.MustCompile(`[^a-z0-9]+`)

// newName returns a template field for a base, such as a JSON key: a
// field name that no other field of the routine has.
func (p *proposer) newName(base string) string {
	b := strings.Trim(notField.ReplaceAllString(strings.ToLower(base), "_"), "_")
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
