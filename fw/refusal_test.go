package fw

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuffer is a buffer that a logger and a test share.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// lines returns the JSON log lines so far.
func (l *lockedBuffer) lines(t *testing.T) []map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, s := range strings.Split(strings.TrimSpace(l.b.String()), "\n") {
		if s == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatalf("log line %q: %v", s, err)
		}
		out = append(out, m)
	}
	return out
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// reasonAuthn refuses every request with one reason, as a driver does.
type reasonAuthn string

func (a reasonAuthn) Authenticate(*http.Request) (*Principal, error) {
	return nil, fmt.Errorf("%w: %s", ErrUnauthorized, string(a))
}

func (reasonAuthn) Challenge(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusUnauthorized)
}

// A refused request leaves one info line with the method, the route, the
// path, the reason, and the node, and no token. A client that loops gets
// one line per route and reason per minute, and the next line counts the
// refusals that the log left out.
func TestRequireAuthLogsRefusals(t *testing.T) {
	logs := &lockedBuffer{}
	clock := NewManualClock(time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC))
	a := newTestAgent(t, newScript(endTurn("hi")).Model(),
		WithAuthn(ChainAuthn{reasonAuthn("oidc: token expired"), TokenAuthn{Token: "s3cret"}}),
		WithLogger(slog.New(slog.NewJSONHandler(logs, nil))),
		WithClock(clock))
	mux := http.NewServeMux()
	mux.Handle("GET /v1/runs/{id}", RequireAuth(a, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))
	get := func(auth string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/runs/run-1", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	refusals := func() []map[string]any {
		t.Helper()
		var out []map[string]any
		for _, l := range logs.lines(t) {
			if l["msg"] == "request refused without a valid credential" {
				out = append(out, l)
			}
		}
		return out
	}

	if code := get("Bearer s3cret"); code != http.StatusOK {
		t.Fatalf("the static token = %d", code)
	}
	if n := len(refusals()); n != 0 {
		t.Fatalf("an accepted request left %d lines", n)
	}
	if code := get(""); code != http.StatusUnauthorized {
		t.Fatalf("no credential = %d", code)
	}
	if code := get("Bearer eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJhbGljZSJ9.c2lnbmF0dXJl"); code != http.StatusUnauthorized {
		t.Fatalf("an expired token = %d", code)
	}
	got := refusals()
	if len(got) != 2 {
		t.Fatalf("two refusals left %d lines: %s", len(got), logs)
	}
	for i, want := range []string{"no credential", "oidc: token expired; token: the bearer token is not the static token"} {
		l := got[i]
		if l["level"] != "INFO" || l["method"] != "GET" || l["route"] != "/v1/runs/{id}" || l["path"] != "/v1/runs/run-1" || l["reason"] != want || l["node"] != a.Node() || l["skipped"] != nil {
			t.Errorf("line %d = %v, want the reason %q", i, l, want)
		}
	}
	if strings.Contains(logs.String(), "eyJ") || strings.Contains(logs.String(), "c2lnbmF0dXJl") {
		t.Fatalf("the log holds the token: %s", logs)
	}

	// A client that loops on the same refusal writes no more lines until a
	// minute passed, and that line counts what the log left out.
	for range 5 {
		get("Bearer wrong")
	}
	if got := refusals(); len(got) != 2 {
		t.Fatalf("a loop of refusals left %d lines, want 2: %s", len(got), logs)
	}
	clock.Advance(refusalEvery)
	get("Bearer wrong")
	got = refusals()
	if len(got) != 3 || got[2]["skipped"] != float64(5) {
		t.Fatalf("the line after a minute = %v, want skipped 5", got[len(got)-1])
	}
	if strings.Contains(logs.String(), "Bearer") {
		t.Fatalf("the log holds a header value: %s", logs)
	}
}

// A chain names the reason of each authenticator once, keeps
// ErrUnauthorized, and a driver without a reason adds none.
func TestChainAuthnNamesEachReason(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/runs", nil)
	req.Header.Set("Authorization", "Bearer nope")
	_, err := ChainAuthn{reasonAuthn("oidc: the token is not a JWT"), TokenAuthn{Token: "s3cret"}, reasonAuthn("oidc: the token is not a JWT")}.Authenticate(req)
	if !errors.Is(err, ErrUnauthorized) || refusalReason(err) != "oidc: the token is not a JWT; token: the bearer token is not the static token" {
		t.Fatalf("chain = %v, reason %q", err, refusalReason(err))
	}
	if _, err := (ChainAuthn{NoAuthnRefuses{}}).Authenticate(req); !errors.Is(err, ErrUnauthorized) || refusalReason(err) != "" {
		t.Fatalf("a chain without reasons = %v", err)
	}
	// The SPIFFE driver puts its words around ErrUnauthorized.
	if got := refusalReason(fmt.Errorf("spiffe: no bearer token: %w", ErrUnauthorized)); got != "spiffe: no bearer token" {
		t.Fatalf("the reason of the spiffe driver = %q", got)
	}
}

// NoAuthnRefuses refuses with ErrUnauthorized alone.
type NoAuthnRefuses struct{}

func (NoAuthnRefuses) Authenticate(*http.Request) (*Principal, error) { return nil, ErrUnauthorized }

func (NoAuthnRefuses) Challenge(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusUnauthorized)
}

// The log of refusals keeps a bounded set of keys: a full set drops the
// keys whose minute passed, and refuses a new key while none did.
func TestRefusalLogBound(t *testing.T) {
	var l refusalLog
	now := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	for i := range maxRefusalKeys {
		if ok, _ := l.note(fmt.Sprint(i), now); !ok {
			t.Fatalf("key %d got no line", i)
		}
	}
	if ok, _ := l.note("one more", now); ok {
		t.Fatal("a full log took a new key")
	}
	if ok, _ := l.note("one more", now.Add(refusalEvery)); !ok {
		t.Fatal("a log whose keys are a minute old took no new key")
	}
	if len(l.seen) != 1 {
		t.Fatalf("the log kept %d keys, want 1", len(l.seen))
	}
}
