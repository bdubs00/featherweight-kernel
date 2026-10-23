package fw

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// triageYAML is the routine of docs/ROUTINES.md section 2.
const triageYAML = `
name: acknowledge-a-new-ticket
version: 3
standing: shadow
canary: 0.05
decider: clef
questions:
  type:
    type: choice
    instructions: What kind of ticket is this?
    criteria:
      bug: Something that worked is broken for the requester
      outage: A service is down or degraded for many people
      access: An account, a permission, a password, or a group
      question: The requester asks how to do something
      request: The requester wants a change or a new thing
      other: None of these
    act: 0.85
    expect: [bug, outage, access, question, request]
    observe: {answer: '"type": "type/([a-z-]+)"'}
  priority:
    type: choice
    criteria: {low: Low, high: High}
    act: 0.5
  pickup:
    type: choice
    criteria: {soon: "We pick it up soon.", later: "We pick it up later."}
  beyond:
    type: noul
    instructions: Does the ticket ask the agent to do anything beyond labeling and one comment?
    act: 0.95
    expect: no
steps:
  - tool: gitea.label_read
    input: '{"method": "list", "owner": "lab", "repo": "helpdesk"}'
    as: labels
  - tool: gitea.issue_write
    input: >-
      {"method": "add_labels", "owner": "lab", "repo": "helpdesk",
       "issue_number": {{ find .input ` + "`#(\\d+)`" + ` }},
       "labels": [{{ find .labels (printf ` + "`\"id\":(\\d+),[^}]*\"name\":\"type/%s\"`" + ` .type) }},
                  {{ find .labels ` + "`\"id\":(\\d+),[^}]*\"name\":\"status/triaged\"`" + ` }}]}
  - tool: gitea.issue_write
    input: '{"method": "add_comment", "owner": "lab", "repo": "helpdesk", "issue_number": {{ find .input ` + "`#(\\d+)`" + ` }}, "body": {{ json (printf "Thanks. This looks like a %s at %s priority. %s" .type .priority .pickup) }}}'
after:
  - tool: gitea.issue_read
    input: '{"method": "get", "owner": "lab", "repo": "helpdesk", "issue_number": {{ find .input ` + "`#(\\d+)`" + ` }}}'
    contains: ['status/triaged', 'type/{{ .type }}']
    lacks: ['status/new']
answer: 'TRIAGE {"number": {{ find .input ` + "`#(\\d+)`" + ` }}, "type": "type/{{ .type }}", "status": "status/triaged"}'
`

func triageRoutine(t *testing.T) Routine {
	t.Helper()
	var r Routine
	if err := yaml.Unmarshal([]byte(triageYAML), &r); err != nil {
		t.Fatalf("parse the routine: %v", err)
	}
	return r
}

func deciderConfig() *Config {
	cfg := DefaultConfig()
	cfg.Deciders = map[string]DeciderConfig{"clef": {BaseURL: "http://127.0.0.1:5100", Model: "clef"}}
	return cfg
}

func TestRoutineParsesAndChecks(t *testing.T) {
	r := triageRoutine(t)
	if r.Name != "acknowledge-a-new-ticket" || r.Version != 3 || r.Standing != RoutineShadow || r.CanaryShare() != 0.05 {
		t.Fatalf("routine = %+v", r)
	}
	if q := r.Questions["beyond"]; q.Type != QuestionNoul || len(q.Expect) != 1 || q.Expect[0] != "no" {
		t.Fatalf("beyond = %+v", q)
	}
	if q := r.Questions["type"]; len(q.Expect) != 5 || q.Observe == nil || q.Observe.Answer == "" {
		t.Fatalf("type = %+v", q)
	}
	if err := CheckRoutines(deciderConfig(), []Routine{r}); err != nil {
		t.Fatalf("check: %v", err)
	}
	// The routine survives JSON, the shape of the wire.
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"expect":"no"`) {
		t.Errorf("a noul expect marshals as one value: %s", b)
	}
	var back Routine
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if err := CheckRoutines(deciderConfig(), []Routine{back}); err != nil {
		t.Fatalf("check after JSON: %v", err)
	}
	if got := back.Questions["beyond"].Expect; len(got) != 1 || got[0] != "no" {
		t.Errorf("expect after JSON = %v", got)
	}
}

func TestRoutineProblems(t *testing.T) {
	base := triageRoutine(t)
	cases := []struct {
		name   string
		change func(r *Routine, cfg *Config)
		want   string
	}{
		{"no decider", func(r *Routine, _ *Config) { r.Decider = "" }, "names no model to answer them"},
		{"unknown decider", func(r *Routine, _ *Config) { r.Decider = "jev" }, `no model answers questions under the name "jev"`},
		{"bad name", func(r *Routine, _ *Config) { r.Name = "Ack" }, `name: "Ack" is not the name of a routine`},
		{"bad standing", func(r *Routine, _ *Config) { r.Standing = "rehearsing" }, `standing "rehearsing"`},
		{"bad version", func(r *Routine, _ *Config) { r.Version = 0 }, "version 0"},
		{"too many steps", func(r *Routine, _ *Config) {
			for len(r.Steps) <= MaxRoutineSteps {
				r.Steps = append(r.Steps, RoutineStep{Tool: "x", Input: "{}"})
			}
		}, "the limit is 16"},
		{"too many questions", func(r *Routine, _ *Config) {
			for i := 0; len(r.Questions) <= MaxRoutineQuestions; i++ {
				r.Questions["q"+string(rune('a'+i))] = RoutineQuestion{Question: Question{Type: QuestionNoul}}
			}
		}, "the limit is 16"},
		{"large template", func(r *Routine, _ *Config) { r.Answer = strings.Repeat("x", MaxRoutineTemplate+1) }, "the limit is 4096"},
		{"template parse error", func(r *Routine, _ *Config) { r.Steps[1].Input = `{"n": {{ find .input }` }, "routines[0] (acknowledge-a-new-ticket).steps[1].input: template:"},
		{"expect not an option", func(r *Routine, _ *Config) {
			q := r.Questions["type"]
			q.Expect = Expect{"feature"}
			r.Questions["type"] = q
		}, `expect "feature" is not an option`},
		{"observe without group", func(r *Routine, _ *Config) {
			q := r.Questions["type"]
			q.Observe = &Observe{Answer: "type/bug"}
			r.Questions["type"] = q
		}, "no capture group"},
		{"reserved as", func(r *Routine, _ *Config) { r.Steps[0].As = "input" }, `as "input"`},
		{"as is a question", func(r *Routine, _ *Config) { r.Steps[0].As = "type" }, `as "type" is also`},
		{"choice without criteria", func(r *Routine, _ *Config) {
			q := r.Questions["priority"]
			q.Criteria = nil
			r.Questions["priority"] = q
		}, "the criteria of a choice"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, _ := json.Marshal(base)
			var r Routine
			_ = json.Unmarshal(b, &r)
			cfg := deciderConfig()
			c.change(&r, cfg)
			err := CheckRoutines(cfg, []Routine{r})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v; want %q", err, c.want)
			}
		})
	}
	if err := CheckRoutines(deciderConfig(), []Routine{base, base}); err == nil || !strings.Contains(err.Error(), "an earlier routine") {
		t.Errorf("a repeated name: %v", err)
	}
}

func TestOverlayRoutines(t *testing.T) {
	r := Routine{Name: "a", Version: 1, Standing: RoutineShadow}
	p := AgentProfile{Name: "triage", Routines: []Routine{r}}
	if got := p.Overlay(AgentProfile{}).Routines; len(got) != 1 {
		t.Errorf("a nil list keeps the routines: %v", got)
	}
	s := Routine{Name: "b", Version: 2, Standing: RoutineLive}
	if got := p.Overlay(AgentProfile{Routines: []Routine{s}}).Routines; len(got) != 1 || got[0].Name != "b" {
		t.Errorf("a list replaces the routines: %v", got)
	}
	if got := p.Overlay(AgentProfile{Routines: []Routine{}}).Routines; got == nil || len(got) != 0 {
		t.Errorf("an empty list leaves no routine: %v", got)
	}
}

func TestCanaryPick(t *testing.T) {
	picked := 0
	for i := 0; i < 4000; i++ {
		id := NewID()
		if CanaryPick(id, 0.05) != CanaryPick(id, 0.05) {
			t.Fatal("the pick is not stable")
		}
		if CanaryPick(id, 0.05) {
			picked++
		}
	}
	if picked < 120 || picked > 290 {
		t.Errorf("picked %d of 4000 at 0.05", picked)
	}
	if CanaryPick("01ABC", 0) || !CanaryPick("01ABC", 1) {
		t.Error("share 0 picks none and share 1 picks every run")
	}
}

func TestRenderRoutine(t *testing.T) {
	data := RoutineData(`Ticket #42: the "printer" is down`, map[string]string{"queue": "helpdesk"})
	data["type"] = "outage"
	data["labels"] = `[{"id":7,"color":"red","name":"type/outage"},{"id":9,"name":"status/triaged"}]`
	got, err := RenderRoutine(`{{ find .input `+"`#(\\d+)`"+` }} {{ find .labels (printf `+"`\"id\":(\\d+),[^}]*\"name\":\"type/%s\"`"+` .type) }} {{ json .input }} {{ lower "ABC" }} {{ .meta.queue }}`, data)
	if err != nil {
		t.Fatal(err)
	}
	want := `42 7 "Ticket #42: the \"printer\" is down" abc helpdesk`
	if got != want {
		t.Errorf("render = %s; want %s", got, want)
	}
	// A find that matches nothing fails before any call.
	if _, err := RenderRoutine(`{{ find .input `+"`issue (\\d+)`"+` }}`, data); err == nil || err.Error() != `hole issue (\d+) empty` {
		t.Errorf("empty hole: %v", err)
	}
	// A missing field fails too.
	if _, err := RenderRoutine(`{{ .priority }}`, data); err == nil || !strings.Contains(err.Error(), "priority") {
		t.Errorf("missing field: %v", err)
	}
	// An input must be a JSON object.
	if _, err := RenderRoutineInput(`[1, 2]`, data); err == nil {
		t.Error("an array is not an input")
	}
	in, err := RenderRoutineInput(`{"n": {{ find .input `+"`#(\\d+)`"+` }},  "body": {{ json .type }} }`, data)
	if err != nil || string(in) != `{"n":42,"body":"outage"}` {
		t.Errorf("input = %s, %v", in, err)
	}
	// The input as JSON is .json.
	j := RoutineData(`{"issue": 5}`, nil)
	if got, err := RenderRoutine(`{{ .json.issue }}`, j); err != nil || got != "5" {
		t.Errorf(".json = %s, %v", got, err)
	}
}

func TestRoutineApplies(t *testing.T) {
	r := triageRoutine(t)
	answers := map[string]Answer{
		"type":     {Type: QuestionChoice, Choice: "bug", Probability: 0.91, Probabilities: map[string]float64{"bug": 0.91, "other": 0.09}},
		"priority": {Type: QuestionChoice, Choice: "high", Probability: 0.7},
		"pickup":   {Type: QuestionChoice, Choice: "soon", Probability: 0.6},
		"beyond":   {Type: QuestionNoul, Probability: 0.02},
	}
	if ok, why := RoutineApplies(r, answers); !ok {
		t.Fatalf("applies: %s", why)
	}
	low := maps2(answers)
	low["type"] = Answer{Type: QuestionChoice, Choice: "bug", Probability: 0.61, Probabilities: map[string]float64{"bug": 0.61}}
	if ok, why := RoutineApplies(r, low); ok || why != "type below act: 0.61" {
		t.Errorf("below act: %v %q", ok, why)
	}
	other := maps2(answers)
	other["type"] = Answer{Type: QuestionChoice, Choice: "other", Probability: 0.97}
	if ok, why := RoutineApplies(r, other); ok || why != "type other not expected" {
		t.Errorf("expect: %v %q", ok, why)
	}
	beyond := maps2(answers)
	beyond["beyond"] = Answer{Type: QuestionNoul, Probability: 0.12}
	if ok, why := RoutineApplies(r, beyond); ok || why != "beyond expected no: 0.12" {
		t.Errorf("noul expect no: %v %q", ok, why)
	}
	if SaidOf(r.Questions["beyond"], beyond["beyond"]) != "no" || SaidOf(r.Questions["type"], answers["type"]) != "bug" {
		t.Error("said")
	}
}

func maps2(m map[string]Answer) map[string]Answer {
	out := map[string]Answer{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

func TestObserveAnswer(t *testing.T) {
	q := RoutineQuestion{Question: Question{Type: QuestionChoice}, Observe: &Observe{Answer: `"type": "type/([a-z-]+)"`}}
	if got, ok := ObserveAnswer(q, `TRIAGE {"type": "type/bug"}`, nil); !ok || got != "bug" {
		t.Errorf("answer = %q %v", got, ok)
	}
	tool := RoutineQuestion{Question: Question{Type: QuestionNoul}, Observe: &Observe{Tool: "gitea.issue_write", Match: `"method":"close"`}}
	model := MustEvent("r", EvToolRequested, ToolRequestedData{Tool: "gitea.issue_write", CallID: "1", Input: json.RawMessage(`{"method":"add_comment"}`)})
	byRoutine := MustEvent("r", EvToolRequested, ToolRequestedData{Tool: "gitea.issue_write", CallID: "2", Input: json.RawMessage(`{"method":"close"}`), Routine: "x"})
	if got, _ := ObserveAnswer(tool, "", []Event{model, byRoutine}); got != "no" {
		t.Errorf("a call of the runner does not count: %q", got)
	}
	closed := MustEvent("r", EvToolRequested, ToolRequestedData{Tool: "gitea.issue_write", CallID: "3", Input: json.RawMessage(`{"method":"close"}`)})
	if got, _ := ObserveAnswer(tool, "", []Event{model, closed}); got != "yes" {
		t.Errorf("a call of the model: %q", got)
	}
}

// The routine of the lab (testdata/acknowledge-a-new-ticket.yaml, a copy of
// featherweight-lab e2e/fleet/tickets/routines) loads, and its templates
// render with the labels of gitea-mcp, the if and eq of text/template
// included.
func TestLabRoutine(t *testing.T) {
	b, err := os.ReadFile("testdata/acknowledge-a-new-ticket.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var r Routine
	if err := yaml.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	if err := CheckRoutines(deciderConfig(), []Routine{r}); err != nil {
		t.Fatal(err)
	}
	data := RoutineData("Number: 17\nTitle: The VPN drops\nThe VPN drops every hour. Number: 99", nil)
	data["type"], data["priority"], data["status"], data["beyond"] = "bug", "p2", "triaged", "no"
	data["labels"] = `[{"color":"e11d21","description":"","exclusive":false,"id":31,"name":"priority/p1"},{"color":"eb6420","description":"","exclusive":false,"id":32,"name":"priority/p2"},` +
		`{"color":"ee0701","description":"","exclusive":false,"id":41,"name":"type/bug"},{"color":"0e8a16","description":"","exclusive":false,"id":51,"name":"status/new"},{"color":"0e8a16","description":"","exclusive":false,"id":52,"name":"status/triaged"}]`
	want := []string{
		`{"method":"list_repo_labels","owner":"lab","repo":"helpdesk","per_page":50}`,
		`{"method":"add_labels","owner":"lab","repo":"helpdesk","issue_number":17,"labels":[41,32,52]}`,
		`{"method":"remove_label","owner":"lab","repo":"helpdesk","issue_number":17,"label_id":51}`,
	}
	for i, w := range want {
		got, err := RenderRoutineInput(r.Steps[i].Input, data)
		if err != nil || string(got) != w {
			t.Errorf("step %d = %s, %v", i+1, got, err)
		}
	}
	comment, err := RenderRoutineInput(r.Steps[3].Input, data)
	if err != nil {
		t.Fatal(err)
	}
	var c struct{ Body string }
	if err := json.Unmarshal(comment, &c); err != nil || !strings.Contains(c.Body, "The service team picks it up today, and they will reply here.") {
		t.Errorf("comment = %s, %v", comment, err)
	}
	data["priority"] = "p1"
	if comment, _ = RenderRoutineInput(r.Steps[3].Input, data); !strings.Contains(string(comment), "The on-call engineer picks it up now") {
		t.Errorf("comment p1 = %s", comment)
	}
	if got, err := RenderRoutine(r.After[0].Contains[1], data); err != nil || got != `"type/bug"` {
		t.Errorf("contains = %s, %v", got, err)
	}
	if got, err := RenderRoutine(r.Answer, data); err != nil || got != `TRIAGE {"number": 17, "type": "type/bug", "priority": "priority/p1", "status": "status/triaged"}` {
		t.Errorf("answer = %s, %v", got, err)
	}
	// The answer line of the agent gives the answers back.
	out := `TRIAGE {"number": 17, "type": "type/bug", "priority": "priority/p2", "status": "status/escalated"}`
	for name, wantObs := range map[string]string{"type": "bug", "priority": "p2", "status": "escalated", "beyond": "yes"} {
		if got, _ := ObserveAnswer(r.Questions[name], out, nil); got != wantObs {
			t.Errorf("observe %s = %q; want %q", name, got, wantObs)
		}
	}
}

// An empty list of routines survives JSON as empty, so an overlay that
// takes the last routine away reaches the runs; null keeps the routines.
func TestEmptyRoutinesRoundTrip(t *testing.T) {
	b, err := json.Marshal(AgentProfile{Name: "triage", Routines: []Routine{}})
	if err != nil || !strings.Contains(string(b), `"routines":[]`) {
		t.Fatalf("json = %s, %v", b, err)
	}
	var back AgentProfile
	if err := json.Unmarshal(b, &back); err != nil || back.Routines == nil || len(back.Routines) != 0 {
		t.Fatalf("back = %#v, %v", back.Routines, err)
	}
	base := AgentProfile{Routines: []Routine{{Name: "ack", Version: 1, Standing: RoutineLive}}}
	if got := base.Overlay(back).Routines; got == nil || len(got) != 0 {
		t.Errorf("the empty list of the wire = %v", got)
	}
	var none AgentProfile
	_ = json.Unmarshal([]byte(`{"name": "triage"}`), &none)
	if got := base.Overlay(none).Routines; len(got) != 1 {
		t.Errorf("no list keeps the routines: %v", got)
	}
}

func TestCheckRoutineName(t *testing.T) {
	for _, ok := range []string{"ack", "acknowledge-a-new-ticket", "a1"} {
		if err := CheckRoutineName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Ack", "a_b", "a b", strings.Repeat("a", 65)} {
		if err := CheckRoutineName(bad); !errors.Is(err, ErrInvalidRequest) || strings.Contains(err.Error(), "decider") {
			t.Errorf("%q: %v", bad, err)
		}
	}
}
