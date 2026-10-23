package fw

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
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
// WithDecider gave, or else the System One client of deciders.<name>. It
// resolves the bearer token through the secrets of the agent at each call,
// so a rotated secret takes effect, and the redactor keeps the token out
// of every event.
func (a *Agent) DeciderNamed(ctx context.Context, name string) (Decider, error) {
	if d, ok := a.deciders[name]; ok {
		return d, nil
	}
	if _, ok := a.cfg.Deciders[name]; !ok {
		return nil, fmt.Errorf("fw: no decider %q; the deciders are %s", name, strings.Join(a.Deciders(), ", "))
	}
	return NewDecider(ctx, a.cfg, a.secrets, name)
}

// NewDecider returns the System One client of deciders.<name> of cfg, with
// the bearer token from the secret that apiKeyEnv names. A command that
// has no agent, such as fw routine replay, builds its decider this way.
func NewDecider(ctx context.Context, cfg *Config, secrets Secrets, name string) (Decider, error) {
	dc, ok := cfg.Deciders[name]
	if !ok {
		return nil, fmt.Errorf("fw: no decider %q in deciders", name)
	}
	token := ""
	if key := strings.TrimSpace(dc.APIKeyEnv); key != "" {
		if secrets == nil {
			return nil, fmt.Errorf("fw: decider %s: no secrets source to read %s", name, key)
		}
		v, err := secrets.Get(ctx, key)
		if err != nil || strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("fw: decider %s: the secret %s is not set", name, key)
		}
		token = strings.TrimSpace(v)
	}
	return NewSystemOne(dc, token), nil
}

// maxDeciderResponse bounds the body of a response of a System One server.
const maxDeciderResponse = 4 << 20

// systemOne is the client of a System One server, the API of TypeSafe
// that Jev and Clef speak: POST {baseUrl}/v1/systemone with {model, state,
// questions}, and the answer {model, answers, usage}.
type systemOne struct {
	url    string
	model  string
	token  string
	client *http.Client
}

// NewSystemOne returns the System One client of a decider. token is the
// bearer token, or "" for none. The client sends the token only in the
// Authorization header, and never logs the token or the state.
func NewSystemOne(cfg DeciderConfig, token string) Decider {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultDeciderTimeout
	}
	return &systemOne{
		url:    strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/") + "/v1/systemone",
		model:  cfg.Model,
		token:  token,
		client: &http.Client{Timeout: timeout},
	}
}

// Decide implements Decider.
func (s *systemOne) Decide(ctx context.Context, state string, questions map[string]Question) (DecideResult, error) {
	if len(questions) == 0 {
		return DecideResult{}, errors.New("fw: decider: no question")
	}
	if len(questions) > MaxDeciderQuestions {
		return DecideResult{}, fmt.Errorf("fw: decider: %d questions; one call holds at most %d", len(questions), MaxDeciderQuestions)
	}
	body, err := json.Marshal(map[string]any{"model": s.model, "state": state, "questions": questions})
	if err != nil {
		return DecideResult{}, fmt.Errorf("fw: decider: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return DecideResult{}, fmt.Errorf("fw: decider: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	start := time.Now()
	resp, err := s.client.Do(req)
	if err != nil {
		return DecideResult{}, fmt.Errorf("fw: decider: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxDeciderResponse+1))
	if err != nil {
		return DecideResult{}, fmt.Errorf("fw: decider: read the response: %w", err)
	}
	latency := time.Since(start).Milliseconds()
	if len(raw) > maxDeciderResponse {
		return DecideResult{}, fmt.Errorf("fw: decider: the response is larger than %d bytes", maxDeciderResponse)
	}
	if resp.StatusCode/100 != 2 {
		return DecideResult{}, fmt.Errorf("fw: decider: the server answered %d: %s", resp.StatusCode, serverMessage(raw))
	}
	out, err := ParseSystemOne(raw, questions)
	if err != nil {
		return DecideResult{}, err
	}
	out.LatencyMS = latency
	return out, nil
}

// serverMessage returns the message of an error body of a System One
// server, {"error": {"type", "message"}}, or the start of the body.
func serverMessage(raw []byte) string {
	var e struct {
		Error json.RawMessage `json:"error"`
	}
	msg := strings.TrimSpace(string(raw))
	if json.Unmarshal(raw, &e) == nil && len(e.Error) > 0 {
		var d struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		var s string
		switch {
		case json.Unmarshal(e.Error, &d) == nil && d.Message != "":
			msg = strings.TrimSpace(d.Type + ": " + d.Message)
		case json.Unmarshal(e.Error, &s) == nil:
			msg = s
		}
	}
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	return msg
}

// ParseSystemOne reads the response body of a System One server for the
// questions that the call asked, in both spellings of the answers. Clef
// and Workers AI send a choice as choice, probabilities, and confidence, a
// noul as noul, and a score as score, legend, and probabilities keyed by
// the place of each level. TypeSafe sends answer in place of choice,
// probability in place of noul, and a score as answer and probabilities
// keyed by level. A missing answer or a field of another shape is an error
// that names the field.
func ParseSystemOne(raw []byte, questions map[string]Question) (DecideResult, error) {
	var w struct {
		Model   string                     `json:"model"`
		Answers map[string]json.RawMessage `json:"answers"`
		Usage   struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		return DecideResult{}, fmt.Errorf("fw: decider: the response is not a System One answer: %w", err)
	}
	if w.Answers == nil {
		return DecideResult{}, errors.New("fw: decider: the response has no answers")
	}
	out := DecideResult{Model: w.Model, Answers: map[string]Answer{}, InputTokens: w.Usage.InputTokens}
	for _, id := range slices.Sorted(maps.Keys(questions)) {
		a, ok := w.Answers[id]
		if !ok {
			return DecideResult{}, fmt.Errorf("fw: decider: answers.%s is missing", id)
		}
		ans, err := parseAnswer(id, questions[id], a)
		if err != nil {
			return DecideResult{}, err
		}
		out.Answers[id] = ans
	}
	return out, nil
}

func parseAnswer(id string, q Question, raw json.RawMessage) (Answer, error) {
	var w struct {
		Type          string             `json:"type"`
		Choice        *string            `json:"choice"`
		Answer        *string            `json:"answer"`
		Noul          *float64           `json:"noul"`
		Probability   *float64           `json:"probability"`
		Confidence    *float64           `json:"confidence"`
		Probabilities map[string]float64 `json:"probabilities"`
		Legend        map[string]any     `json:"legend"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		return Answer{}, fmt.Errorf("fw: decider: answers.%s: %w", id, err)
	}
	if w.Type != "" && w.Type != q.Type {
		return Answer{}, fmt.Errorf("fw: decider: answers.%s.type is %q; the question is a %s", id, w.Type, q.Type)
	}
	a := Answer{Type: q.Type}
	if w.Confidence != nil {
		a.Confidence = *w.Confidence
	}
	switch q.Type {
	case QuestionNoul:
		p := w.Noul
		if p == nil {
			p = w.Probability
		}
		if p == nil || *p < 0 || *p > 1 {
			return Answer{}, fmt.Errorf("fw: decider: answers.%s.noul is not a probability", id)
		}
		a.Probability = *p
	case QuestionChoice:
		c := w.Choice
		if c == nil {
			c = w.Answer
		}
		if c == nil {
			return Answer{}, fmt.Errorf("fw: decider: answers.%s.choice is missing", id)
		}
		if len(w.Probabilities) == 0 {
			return Answer{}, fmt.Errorf("fw: decider: answers.%s.probabilities is missing", id)
		}
		p, ok := w.Probabilities[*c]
		if !ok {
			return Answer{}, fmt.Errorf("fw: decider: answers.%s.probabilities has no value for %q", id, *c)
		}
		a.Choice, a.Probability, a.Probabilities = *c, p, w.Probabilities
	case QuestionScore:
		named := w.Choice
		if named == nil {
			named = w.Answer
		}
		if len(w.Probabilities) == 0 {
			// TypeSafe can name the level with its probability alone.
			if named == nil || w.Probability == nil {
				return Answer{}, fmt.Errorf("fw: decider: answers.%s.probabilities is missing", id)
			}
			a.Choice, a.Probability = *named, *w.Probability
			break
		}
		// Clef keys the probabilities by the place of each level, which its
		// legend names. TypeSafe keys them by level, without a legend: keys
		// that are all levels are levels, also levels such as "1" and "2".
		levels, _ := stringList(q.Criteria)
		byLevel := len(w.Legend) == 0
		for k := range w.Probabilities {
			if !slices.Contains(levels, k) {
				byLevel = false
				break
			}
		}
		a.Probabilities = map[string]float64{}
		for k, p := range w.Probabilities {
			level := k
			if !byLevel {
				if name, ok := w.Legend[k].(string); ok && name != "" {
					level = name
				} else if i, err := strconv.Atoi(k); err == nil && i >= 0 && i < len(levels) {
					level = levels[i]
				}
			}
			a.Probabilities[level] = p
		}
		if named != nil {
			p, ok := a.Probabilities[*named]
			if !ok {
				return Answer{}, fmt.Errorf("fw: decider: answers.%s.probabilities has no value for %q", id, *named)
			}
			a.Choice, a.Probability = *named, p
			break
		}
		for _, level := range slices.Sorted(maps.Keys(a.Probabilities)) {
			if p := a.Probabilities[level]; a.Choice == "" || p > a.Probability {
				a.Choice, a.Probability = level, p
			}
		}
	default:
		return Answer{}, fmt.Errorf("fw: decider: the question %s has the type %q", id, q.Type)
	}
	return a, nil
}
