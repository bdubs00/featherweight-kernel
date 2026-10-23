package fw

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// This file holds the decider: the model that answers typed questions with
// probabilities (docs/ROUTINES.md section 3.2). A routine asks it whether
// the routine applies to the input of a run, and the built-in tool decide
// asks it for a model. The kernel holds the interface, the configuration,
// the registry of the drivers, and the tool. A driver package registers
// its client in init (RegisterDecider), as a model provider does. The
// client of a System One server is the package decider/systemone.

// Decider answers typed questions about a state.
type Decider interface {
	// Decide asks typed questions about a state and returns one answer per
	// question, with probabilities. It is the only model-shaped thing a
	// routine touches, and it never generates text.
	Decide(ctx context.Context, state string, questions map[string]Question) (DecideResult, error)
}

// Types of a question.
const (
	QuestionChoice = "choice" // one option of several named options
	QuestionNoul   = "noul"   // the probability of yes
	QuestionScore  = "score"  // one level of several ordered levels
)

// Question is one typed question to a decider. Criteria is a map of
// option to description for a choice, a list of ordered levels for a
// score, and nil or a map with the keys true and false for a noul.
type Question struct {
	Type         string `yaml:"type" json:"type"`
	Instructions string `yaml:"instructions,omitempty" json:"instructions,omitempty"`
	Criteria     any    `yaml:"criteria,omitempty" json:"criteria,omitempty"`
}

// Answer is the answer of a decider to one question. Choice is the chosen
// option of a choice and the most probable level of a score. Probability
// is P(yes) for a noul and the probability of Choice otherwise. The
// client sets it from the probabilities, so a reader needs nothing else.
// Probabilities holds every option of a choice and every level of a score.
// Confidence is what the server reports, and the kernel never reads it.
type Answer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice,omitempty"`
	Probability   float64            `json:"probability,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Confidence    float64            `json:"confidence,omitempty"`
}

// DecideResult is the answer of a decider to one call: one answer per
// question. Model is the model that the server reports. (docs/ROUTINES.md
// calls it Decision; the policy of the kernel already has that name.)
type DecideResult struct {
	Model       string            `json:"model"`
	Answers     map[string]Answer `json:"answers"`
	InputTokens int               `json:"inputTokens,omitempty"`
	LatencyMS   int64             `json:"latencyMs"`
}

// DeciderConfig is the connection of one decider, keyed by its name in
// Config.Deciders. Driver names the registered client (RegisterDecider);
// empty means DeciderSystemOne. BaseURL is the address of the server: a
// System One server takes POST {baseUrl}/v1/systemone. APIKeyEnv names the
// secret that holds the bearer token, resolved through Secrets as a
// provider resolves its key; empty sends no token. Model is the model name
// that the client sends. It has no default, because a decider costs money
// or a GPU. Timeout bounds one call; zero means 30 seconds.
type DeciderConfig struct {
	Driver    string        `yaml:"driver,omitempty" json:"driver,omitempty"`
	BaseURL   string        `yaml:"baseUrl" json:"baseUrl"`
	APIKeyEnv string        `yaml:"apiKeyEnv,omitempty" json:"apiKeyEnv,omitempty"`
	Model     string        `yaml:"model" json:"model"`
	Timeout   time.Duration `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// DeciderSystemOne is the driver of a decider that names none: the client
// of a System One server, the API of TypeSafe that Clef and Jev speak.
const DeciderSystemOne = "systemone"

// DriverName returns the driver of the decider: Driver, else
// DeciderSystemOne.
func (c DeciderConfig) DriverName() string {
	if d := strings.ToLower(strings.TrimSpace(c.Driver)); d != "" {
		return d
	}
	return DeciderSystemOne
}

// DefaultDeciderTimeout bounds a call to a decider that sets no timeout.
const DefaultDeciderTimeout = 30 * time.Second

// MaxDeciderQuestions is the most questions of one call to a decider. The
// runner splits a larger set into several calls of the same decider.
const MaxDeciderQuestions = 64

// DeciderCtor builds the client of the decider name from its entry of
// Config.Deciders. secrets resolve the token that cfg.APIKeyEnv names, and
// the redactor of the agent learns each value that they return.
type DeciderCtor func(ctx context.Context, name string, cfg DeciderConfig, secrets Secrets) (Decider, error)

var deciderDrivers = struct {
	mu sync.RWMutex
	m  map[string]DeciderCtor
}{m: map[string]DeciderCtor{}}

// RegisterDecider registers the client of a decider by driver name, for
// deciders.<name>.driver. A driver package calls it in init.
func RegisterDecider(driver string, c DeciderCtor) {
	deciderDrivers.mu.Lock()
	defer deciderDrivers.mu.Unlock()
	deciderDrivers.m[driver] = c
}

// RegisteredDeciders lists the registered drivers of deciders, sorted.
func RegisteredDeciders() []string {
	deciderDrivers.mu.RLock()
	defer deciderDrivers.mu.RUnlock()
	return slices.Sorted(maps.Keys(deciderDrivers.m))
}

// Deciders returns the names of the deciders of the agent, sorted: the
// keys of Config.Deciders and the names that WithDecider gave.
func (a *Agent) Deciders() []string {
	names := map[string]bool{}
	for n := range a.cfg.Deciders {
		names[n] = true
	}
	for n := range a.deciders {
		names[n] = true
	}
	return slices.Sorted(maps.Keys(names))
}

// DeciderNamed returns the decider with that name: the one that
// WithDecider gave, or else the client that the driver of deciders.<name>
// builds. It resolves the bearer token through the secrets of the agent at
// each call, so a rotated secret takes effect, and the redactor keeps the
// token out of every event.
func (a *Agent) DeciderNamed(ctx context.Context, name string) (Decider, error) {
	if d, ok := a.deciders[name]; ok {
		return d, nil
	}
	if _, ok := a.cfg.Deciders[name]; !ok {
		return nil, fmt.Errorf("fw: no decider %q; the deciders are %s", name, strings.Join(a.Deciders(), ", "))
	}
	return NewDecider(ctx, a.cfg, a.secrets, name)
}

// NewDecider returns the client of deciders.<name> of cfg, from the driver
// that the entry names. A command that has no agent, such as fw routine
// replay, builds its decider this way.
func NewDecider(ctx context.Context, cfg *Config, secrets Secrets, name string) (Decider, error) {
	dc, ok := cfg.Deciders[name]
	if !ok {
		return nil, fmt.Errorf("fw: no decider %q in deciders", name)
	}
	driver := dc.DriverName()
	deciderDrivers.mu.RLock()
	c, ok := deciderDrivers.m[driver]
	deciderDrivers.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("fw: decider %s: no decider driver %q is linked into this binary (known: %s)", name, driver, strings.Join(RegisteredDeciders(), ", "))
	}
	return c(ctx, name, dc, secrets)
}

const decideSchema = `{
  "type": "object",
  "description": "Ask typed questions about a state and get one answer per question, with probabilities.",
  "properties": {
    "decider": {
      "type": "string",
      "description": "The name of a decider of the configuration. Optional when the configuration has one."
    },
    "state": {
      "type": "string",
      "description": "The text to decide about. The input of the run by default."
    },
    "questions": {
      "type": "object",
      "description": "Question id to question, at most 64. A choice maps each option to a description in criteria, a score lists 2 to 10 ordered levels in criteria, and a noul asks for the probability of yes.",
      "additionalProperties": {
        "type": "object",
        "properties": {
          "type": {"type": "string", "enum": ["choice", "noul", "score"]},
          "instructions": {"type": "string"},
          "criteria": {}
        },
        "required": ["type"]
      }
    }
  },
  "required": ["questions"],
  "additionalProperties": false
}`

const decideDescription = "Ask typed questions about a state and get one answer per question, with probabilities. " +
	"A choice picks one of named options, a score picks one of ordered levels, and a noul gives the probability of yes. " +
	"The answer never holds text beyond the options. Read the probabilities, not only the choice."

// questionID is the shape of a question id that a System One server
// accepts.
var questionID = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)

// DecideTool returns the tool decide (docs/ROUTINES.md section 3.2). It
// asks a decider of the agent in the context of the call, and its output
// is the DecideResult as JSON, so the probabilities of every decision sit
// in fw.tool.responded. Package builtin offers it to the model when the
// configuration has a decider. The routine runner calls it for its own
// decisions through the tool path, whatever tools.builtin holds.
func DecideTool() Tool { return decideTool{} }

type decideTool struct{}

// Spec implements Tool.
func (decideTool) Spec() ToolSpec {
	return ToolSpec{Name: "decide", Title: "Answer typed questions with probabilities", Description: decideDescription,
		InputSchema: json.RawMessage(decideSchema), ReadOnly: true, Idempotent: true, ParallelSafe: true, Source: "builtin", Trusted: true}
}

// Call implements Tool.
func (decideTool) Call(ctx context.Context, input json.RawMessage) (ToolResult, error) {
	var in struct {
		Decider   string              `json:"decider"`
		State     string              `json:"state"`
		Questions map[string]Question `json:"questions"`
	}
	if len(bytes.TrimSpace(input)) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return ErrorResult("decide: the input is not valid JSON for this tool: " + err.Error()), nil
		}
	}
	if len(in.Questions) == 0 {
		return ErrorResult("decide: ask at least one question."), nil
	}
	if len(in.Questions) > MaxDeciderQuestions {
		return ErrorResult(fmt.Sprintf("decide: %d questions; one call asks at most %d.", len(in.Questions), MaxDeciderQuestions)), nil
	}
	for id, q := range in.Questions {
		if !questionID.MatchString(id) {
			return ErrorResult(fmt.Sprintf("decide: the question id %q is not 1 to 100 of letters, digits, _, ., and -.", id)), nil
		}
		if _, err := QuestionOptions(q); err != nil {
			return ErrorResult(fmt.Sprintf("decide: question %s: %v.", id, err)), nil
		}
	}
	a := AgentFrom(ctx)
	if a == nil {
		return ErrorResult("decide: no agent in the context of the call."), nil
	}
	name := strings.TrimSpace(in.Decider)
	if name == "" {
		names := a.Deciders()
		if len(names) != 1 {
			return ErrorResult("decide: name a decider: " + strings.Join(names, ", ") + "."), nil
		}
		name = names[0]
	}
	state := in.State
	if state == "" {
		r, err := a.Get(ctx, RunInfoFrom(ctx).RunID)
		if err != nil || r.Input == "" {
			return ErrorResult("decide: give a state; this call has no run input to decide about."), nil
		}
		state = r.Input
	}
	d, err := a.DeciderNamed(ctx, name)
	if err != nil {
		return ErrorResult("decide: " + err.Error()), nil
	}
	res, err := d.Decide(ctx, state, in.Questions)
	if err != nil {
		if ctx.Err() != nil {
			return ErrorResult("decide: canceled"), fmt.Errorf("fw: decide: %w", context.Cause(ctx))
		}
		return ErrorResult("decide: " + err.Error()), nil
	}
	b, err := json.Marshal(res)
	if err != nil {
		return ErrorResult("decide: " + err.Error()), nil
	}
	return TextResult(string(b)), nil
}
