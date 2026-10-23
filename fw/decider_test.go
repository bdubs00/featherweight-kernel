package fw

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// readmeQuestions are the questions of the model card example. The answer
// in testdata/systemone_response.json is what Clef said to them.
var readmeQuestions = map[string]Question{
	"department": {Type: QuestionChoice, Instructions: "Which team should handle the message?",
		Criteria: map[string]any{"billing": "Payments or invoices", "technical": "Bugs or outages"}},
	"urgency": {Type: QuestionScore, Instructions: "How urgent is it?", Criteria: []any{"Can wait", "This week", "Today"}},
	"outage":  {Type: QuestionNoul, Instructions: "Is a service down?"},
}

// systemOneServer serves a recorded answer and keeps the last request.
func systemOneServer(t *testing.T, status int, body []byte) (*httptest.Server, *http.Request, *[]byte) {
	t.Helper()
	var got http.Request
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = *r
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &got, &gotBody
}

func TestSystemOneClient(t *testing.T) {
	recorded, err := os.ReadFile("testdata/systemone_response.json")
	if err != nil {
		t.Fatal(err)
	}
	srv, req, body := systemOneServer(t, 200, recorded)
	d := NewSystemOne(DeciderConfig{BaseURL: srv.URL + "/", Model: "clef"}, "s3cret-token-value")
	res, err := d.Decide(context.Background(), "Our checkout started returning errors and orders are blocked.", readmeQuestions)
	if err != nil {
		t.Fatal(err)
	}
	if req.URL.Path != "/v1/systemone" || req.Method != http.MethodPost {
		t.Errorf("request %s %s", req.Method, req.URL.Path)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer s3cret-token-value" {
		t.Errorf("Authorization = %q", got)
	}
	var sent struct {
		Model     string                     `json:"model"`
		State     string                     `json:"state"`
		Questions map[string]json.RawMessage `json:"questions"`
	}
	if err := json.Unmarshal(*body, &sent); err != nil || sent.Model != "clef" || !strings.HasPrefix(sent.State, "Our checkout") || len(sent.Questions) != 3 {
		t.Errorf("body = %s (%v)", *body, err)
	}
	if res.Model != "clef-exl3-4.0bpw" || res.InputTokens != 303 {
		t.Errorf("model %q, tokens %d", res.Model, res.InputTokens)
	}
	if a := res.Answers["department"]; a.Type != QuestionChoice || a.Choice != "technical" || a.Probability != 0.9255 || a.Probabilities["billing"] != 0.0745 || a.Confidence != 0.9255 {
		t.Errorf("department = %+v", a)
	}
	if a := res.Answers["urgency"]; a.Type != QuestionScore || a.Choice != "Today" || a.Probability != 0.836 || a.Probabilities["This week"] != 0.1114 {
		t.Errorf("urgency = %+v", a)
	}
	if a := res.Answers["outage"]; a.Type != QuestionNoul || a.Probability != 0.8963 {
		t.Errorf("outage = %+v", a)
	}

	// No token sends no Authorization header.
	none := NewSystemOne(DeciderConfig{BaseURL: srv.URL, Model: "clef"}, "")
	if _, err := none.Decide(context.Background(), "x", readmeQuestions); err != nil || req.Header.Get("Authorization") != "" {
		t.Errorf("without a token: %v, %q", err, req.Header.Get("Authorization"))
	}
}

func TestSystemOneErrors(t *testing.T) {
	q := map[string]Question{"outage": {Type: QuestionNoul}}
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"status", 401, `{"error": {"type": "authentication_error", "message": "missing or wrong bearer token"}}`, "answered 401: authentication_error: missing or wrong bearer token"},
		{"long input", 413, `{"error": {"type": "context_length_exceeded", "message": "the input holds 70000 tokens; the limit is 65536"}}`, "answered 413: context_length_exceeded: the input holds 70000 tokens"},
		{"no answers", 200, `{"model": "clef"}`, "the response has no answers"},
		{"missing answer", 200, `{"model": "clef", "answers": {}}`, "answers.outage is missing"},
		{"other type", 200, `{"model": "clef", "answers": {"outage": {"type": "choice", "choice": "a"}}}`, `answers.outage.type is "choice"`},
		{"not a probability", 200, `{"model": "clef", "answers": {"outage": {"type": "noul", "noul": 1.5}}}`, "answers.outage.noul is not a probability"},
		{"not JSON", 200, `<html>`, "not a System One answer"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, _, _ := systemOneServer(t, c.status, []byte(c.body))
			_, err := NewSystemOne(DeciderConfig{BaseURL: srv.URL, Model: "clef"}, "tok-12345678").Decide(context.Background(), "x", q)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v; want %q", err, c.want)
			}
			if strings.Contains(err.Error(), "tok-12345678") {
				t.Errorf("the error holds the token: %v", err)
			}
		})
	}
	// A choice names its probability.
	choice := map[string]Question{"team": {Type: QuestionChoice, Criteria: map[string]any{"a": "A", "b": "B"}}}
	if _, err := ParseSystemOne([]byte(`{"answers": {"team": {"type": "choice", "choice": "c", "probabilities": {"a": 0.5, "b": 0.5}}}}`), choice); err == nil || !strings.Contains(err.Error(), `no value for "c"`) {
		t.Errorf("choice: %v", err)
	}
	// The timeout bounds a call.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(500 * time.Millisecond):
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	start := time.Now()
	if _, err := NewSystemOne(DeciderConfig{BaseURL: slow.URL, Model: "clef", Timeout: 50 * time.Millisecond}, "").Decide(context.Background(), "x", q); err == nil || time.Since(start) > 400*time.Millisecond {
		t.Errorf("timeout: %v after %s", err, time.Since(start))
	}
}

func TestDeciderNamedResolvesTheSecret(t *testing.T) {
	recorded, _ := os.ReadFile("testdata/systemone_response.json")
	srv, req, _ := systemOneServer(t, 200, recorded)
	cfg := DefaultConfig()
	cfg.Deciders = map[string]DeciderConfig{
		"clef":    {BaseURL: srv.URL, APIKeyEnv: "CLEF_API_KEY", Model: "clef"},
		"missing": {BaseURL: srv.URL, APIKeyEnv: "NO_SUCH_KEY", Model: "clef"},
	}
	secrets := SecretsFunc(func(_ context.Context, name string) (string, error) {
		if name == "CLEF_API_KEY" {
			return "clef-bearer-0123456789", nil
		}
		return "", ErrSecretNotFound
	})
	a, err := New(WithoutDefaults(), WithAuthn(NoAuthn{}), WithConfig(cfg), WithLedger(NewMemLedger()), WithBus(NewMemBus()), WithSecrets(secrets), WithLogger(quietLogger()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(context.Background()) })
	if got := strings.Join(a.Deciders(), ","); got != "clef,missing" {
		t.Errorf("deciders = %s", got)
	}
	d, err := a.DeciderNamed(context.Background(), "clef")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Decide(context.Background(), "x", readmeQuestions); err != nil || req.Header.Get("Authorization") != "Bearer clef-bearer-0123456789" {
		t.Fatalf("decide: %v, %q", err, req.Header.Get("Authorization"))
	}
	// The redactor knows the token now, so no event holds it.
	if got := a.Redact("token clef-bearer-0123456789"); strings.Contains(got, "clef-bearer") {
		t.Errorf("redact = %q", got)
	}
	if _, err := a.DeciderNamed(context.Background(), "missing"); err == nil || !strings.Contains(err.Error(), "the secret NO_SUCH_KEY is not set") {
		t.Errorf("missing secret: %v", err)
	}
	if _, err := a.DeciderNamed(context.Background(), "jev"); err == nil || !strings.Contains(err.Error(), `no decider "jev"`) {
		t.Errorf("unknown decider: %v", err)
	}
}

// Both spellings of the answers parse: Clef and Workers AI (choice, noul,
// score with a legend) and TypeSafe (answer, probability, score by level).
func TestSystemOneSpellings(t *testing.T) {
	qs := map[string]Question{
		"team":    {Type: QuestionChoice, Criteria: map[string]any{"billing": "B", "technical": "T"}},
		"down":    {Type: QuestionNoul},
		"urgency": {Type: QuestionScore, Criteria: []any{"Can wait", "This week", "Today"}},
	}
	cases := []struct {
		name string
		body string
	}{
		{"clef", `{"model": "clef", "answers": {
			"team": {"type": "choice", "choice": "technical", "confidence": 0.9, "probabilities": {"billing": 0.1, "technical": 0.9}},
			"down": {"type": "noul", "noul": 0.8},
			"urgency": {"type": "score", "score": 1.8, "confidence": 0.7, "legend": {"0": "Can wait", "1": "This week", "2": "Today"}, "probabilities": {"0": 0.1, "1": 0.2, "2": 0.7}}},
			"usage": {"input_tokens": 9, "output_tokens": 0}}`},
		{"clef without legend", `{"model": "clef", "answers": {
			"team": {"type": "choice", "choice": "technical", "probabilities": {"billing": 0.1, "technical": 0.9}},
			"down": {"type": "noul", "noul": 0.8},
			"urgency": {"type": "score", "score": 1.8, "legend": {"0": {"text": "x"}}, "probabilities": {"0": 0.1, "1": 0.2, "2": 0.7}}}}`},
		{"typesafe", `{"model": "jev", "answers": {
			"team": {"type": "choice", "answer": "technical", "confidence": 0.9, "probabilities": {"billing": 0.1, "technical": 0.9}},
			"down": {"type": "noul", "probability": 0.8},
			"urgency": {"type": "score", "answer": "Today", "probabilities": {"Can wait": 0.1, "This week": 0.2, "Today": 0.7}}}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := ParseSystemOne([]byte(c.body), qs)
			if err != nil {
				t.Fatal(err)
			}
			if a := res.Answers["team"]; a.Choice != "technical" || a.Probability != 0.9 {
				t.Errorf("team = %+v", a)
			}
			if a := res.Answers["down"]; a.Probability != 0.8 {
				t.Errorf("down = %+v", a)
			}
			if a := res.Answers["urgency"]; a.Choice != "Today" || a.Probability != 0.7 || a.Probabilities["This week"] != 0.2 {
				t.Errorf("urgency = %+v", a)
			}
		})
	}
}

// A score whose levels are numbers reads the same in both spellings: keys
// that are all levels are levels, and other keys are places.
func TestSystemOneNumericLevels(t *testing.T) {
	qs := map[string]Question{"severity": {Type: QuestionScore, Criteria: []any{"1", "2", "3"}}}
	for name, body := range map[string]string{
		"typesafe": `{"answers": {"severity": {"type": "score", "answer": "3", "probabilities": {"1": 0.1, "2": 0.2, "3": 0.7}}}}`,
		"clef":     `{"answers": {"severity": {"type": "score", "score": 1.6, "legend": {"0": "1", "1": "2", "2": "3"}, "probabilities": {"0": 0.1, "1": 0.2, "2": 0.7}}}}`,
		"places":   `{"answers": {"severity": {"type": "score", "probabilities": {"0": 0.1, "1": 0.2, "2": 0.7}}}}`,
	} {
		res, err := ParseSystemOne([]byte(body), qs)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if a := res.Answers["severity"]; a.Choice != "3" || a.Probability != 0.7 || a.Probabilities["1"] != 0.1 {
			t.Errorf("%s: severity = %+v", name, a)
		}
	}
}
