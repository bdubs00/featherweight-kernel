// Package systemone is the fw.Decider client of a System One server, the
// API of TypeSafe that Clef and Jev speak (docs/ROUTINES.md section 3.2).
// It registers itself as the decider driver "systemone", which is the
// driver of an entry of deciders that names none. It posts {model, state,
// questions} to {baseUrl}/v1/systemone and reads {model, answers, usage}
// in both spellings of the answers. It links only net/http, so every build
// of fw carries it.
package systemone

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

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Driver is the name of the driver, for deciders.<name>.driver.
const Driver = fw.DeciderSystemOne

func init() { fw.RegisterDecider(Driver, New) }

// maxResponse bounds the body of a response of a System One server.
const maxResponse = 4 << 20

// client is the client of one System One server.
type client struct {
	url    string
	model  string
	token  string
	client *http.Client
}

// New returns the client of the decider name, with the bearer token from
// the secret that cfg.APIKeyEnv names. It is the fw.DeciderCtor of the
// driver.
func New(ctx context.Context, name string, cfg fw.DeciderConfig, secrets fw.Secrets) (fw.Decider, error) {
	token := ""
	if key := strings.TrimSpace(cfg.APIKeyEnv); key != "" {
		if secrets == nil {
			return nil, fmt.Errorf("fw: decider %s: no secrets source to read %s", name, key)
		}
		v, err := secrets.Get(ctx, key)
		if err != nil || strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("fw: decider %s: the secret %s is not set", name, key)
		}
		token = strings.TrimSpace(v)
	}
	return newClient(cfg, token), nil
}

// newClient returns the client of a decider. token is the bearer token,
// or "" for none. The client sends the token only in the Authorization
// header, and never logs the token or the state.
func newClient(cfg fw.DeciderConfig, token string) *client {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = fw.DefaultDeciderTimeout
	}
	return &client{
		url:    strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/") + "/v1/systemone",
		model:  cfg.Model,
		token:  token,
		client: &http.Client{Timeout: timeout},
	}
}

// Decide implements fw.Decider.
func (s *client) Decide(ctx context.Context, state string, questions map[string]fw.Question) (fw.DecideResult, error) {
	if len(questions) == 0 {
		return fw.DecideResult{}, errors.New("systemone: no question")
	}
	if len(questions) > fw.MaxDeciderQuestions {
		return fw.DecideResult{}, fmt.Errorf("systemone: %d questions; one call holds at most %d", len(questions), fw.MaxDeciderQuestions)
	}
	body, err := json.Marshal(map[string]any{"model": s.model, "state": state, "questions": questions})
	if err != nil {
		return fw.DecideResult{}, fmt.Errorf("systemone: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return fw.DecideResult{}, fmt.Errorf("systemone: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	start := time.Now()
	resp, err := s.client.Do(req)
	if err != nil {
		return fw.DecideResult{}, fmt.Errorf("systemone: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return fw.DecideResult{}, fmt.Errorf("systemone: read the response: %w", err)
	}
	latency := time.Since(start).Milliseconds()
	if len(raw) > maxResponse {
		return fw.DecideResult{}, fmt.Errorf("systemone: the response is larger than %d bytes", maxResponse)
	}
	if resp.StatusCode/100 != 2 {
		return fw.DecideResult{}, fmt.Errorf("systemone: the server answered %d: %s", resp.StatusCode, serverMessage(raw))
	}
	out, err := parse(raw, questions)
	if err != nil {
		return fw.DecideResult{}, err
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
	return fw.ClipText(msg, 300, "the message")
}

// parse reads the response body of a System One server for the questions
// that the call asked, in both spellings of the answers. Clef and Workers
// AI send a choice as choice, probabilities, and confidence, a noul as
// noul, and a score as score, legend, and probabilities keyed by the place
// of each level. TypeSafe sends answer in place of choice, probability in
// place of noul, and a score as answer and probabilities keyed by level. A
// missing answer or a field of another shape is an error that names the
// field.
func parse(raw []byte, questions map[string]fw.Question) (fw.DecideResult, error) {
	var w struct {
		Model   string                     `json:"model"`
		Answers map[string]json.RawMessage `json:"answers"`
		Usage   struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		return fw.DecideResult{}, fmt.Errorf("systemone: the response is not a System One answer: %w", err)
	}
	if w.Answers == nil {
		return fw.DecideResult{}, errors.New("systemone: the response has no answers")
	}
	out := fw.DecideResult{Model: w.Model, Answers: map[string]fw.Answer{}, InputTokens: w.Usage.InputTokens}
	for _, id := range slices.Sorted(maps.Keys(questions)) {
		a, ok := w.Answers[id]
		if !ok {
			return fw.DecideResult{}, fmt.Errorf("systemone: answers.%s is missing", id)
		}
		ans, err := parseAnswer(id, questions[id], a)
		if err != nil {
			return fw.DecideResult{}, err
		}
		out.Answers[id] = ans
	}
	return out, nil
}

func parseAnswer(id string, q fw.Question, raw json.RawMessage) (fw.Answer, error) {
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
		return fw.Answer{}, fmt.Errorf("systemone: answers.%s: %w", id, err)
	}
	if w.Type != "" && w.Type != q.Type {
		return fw.Answer{}, fmt.Errorf("systemone: answers.%s.type is %q; the question is a %s", id, w.Type, q.Type)
	}
	a := fw.Answer{Type: q.Type}
	if w.Confidence != nil {
		a.Confidence = *w.Confidence
	}
	switch q.Type {
	case fw.QuestionNoul:
		p := w.Noul
		if p == nil {
			p = w.Probability
		}
		if p == nil || *p < 0 || *p > 1 {
			return fw.Answer{}, fmt.Errorf("systemone: answers.%s.noul is not a probability", id)
		}
		a.Probability = *p
	case fw.QuestionChoice:
		c := w.Choice
		if c == nil {
			c = w.Answer
		}
		if c == nil {
			return fw.Answer{}, fmt.Errorf("systemone: answers.%s.choice is missing", id)
		}
		if len(w.Probabilities) == 0 {
			return fw.Answer{}, fmt.Errorf("systemone: answers.%s.probabilities is missing", id)
		}
		p, ok := w.Probabilities[*c]
		if !ok {
			return fw.Answer{}, fmt.Errorf("systemone: answers.%s.probabilities has no value for %q", id, *c)
		}
		a.Choice, a.Probability, a.Probabilities = *c, p, w.Probabilities
	case fw.QuestionScore:
		named := w.Choice
		if named == nil {
			named = w.Answer
		}
		if len(w.Probabilities) == 0 {
			// TypeSafe can name the level with its probability alone.
			if named == nil || w.Probability == nil {
				return fw.Answer{}, fmt.Errorf("systemone: answers.%s.probabilities is missing", id)
			}
			a.Choice, a.Probability = *named, *w.Probability
			break
		}
		// Clef keys the probabilities by the place of each level, which its
		// legend names. TypeSafe keys them by level, without a legend: keys
		// that are all levels are levels, also levels such as "1" and "2".
		levels, _ := fw.QuestionOptions(q)
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
				return fw.Answer{}, fmt.Errorf("systemone: answers.%s.probabilities has no value for %q", id, *named)
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
		return fw.Answer{}, fmt.Errorf("systemone: the question %s has the type %q", id, q.Type)
	}
	return a, nil
}
