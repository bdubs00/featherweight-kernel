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
