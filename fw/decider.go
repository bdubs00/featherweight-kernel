package fw

import (
	"context"
	"time"
)

// This file holds the decider: the model that answers typed questions with
// probabilities (docs/ROUTINES.md section 3.2). A routine asks it whether
// the routine applies to the input of a run, and the built-in tool decide
// asks it for a model.

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
// is P(yes) for a noul and the probability of Choice otherwise.
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
// Config.Deciders. BaseURL is a System One server: the client posts to
// {baseUrl}/v1/systemone. APIKeyEnv names the secret that holds the bearer
// token, resolved through Secrets as a provider resolves its key; empty
// sends no token. Model is the model name that the client sends. It has no
// default, because a decider costs money or a GPU. Timeout bounds one call;
// zero means DefaultDeciderTimeout.
type DeciderConfig struct {
	BaseURL   string        `yaml:"baseUrl" json:"baseUrl"`
	APIKeyEnv string        `yaml:"apiKeyEnv,omitempty" json:"apiKeyEnv,omitempty"`
	Model     string        `yaml:"model" json:"model"`
	Timeout   time.Duration `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// DefaultDeciderTimeout bounds a call to a decider that sets no timeout.
const DefaultDeciderTimeout = 30 * time.Second

// MaxDeciderQuestions is the most questions of one call to a decider. The
// runner splits a larger set into several calls.
const MaxDeciderQuestions = 64
