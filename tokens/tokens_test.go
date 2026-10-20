package tokens

import (
	"context"
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

	"github.com/bdubs00/featherweight-kernel/fw"
)

// fakeBroker is a broker that knows the people who connected the service
// tickets. It accepts one bearer credential.
type fakeBroker struct {
	*httptest.Server
	auth string

	mu        sync.Mutex
	connected map[string]bool
	expired   map[string]bool
	runs      map[string]string // run id to subject; nil trusts every run
	n         int
	requests  []TokenRequest
	rejects   []RejectRequest
	forgets   []ForgetRequest
	expiresIn time.Duration
}

func newFakeBroker(t *testing.T) *fakeBroker {
	t.Helper()
	b := &fakeBroker{auth: "member-secret", connected: map[string]bool{}, expired: map[string]bool{}, expiresIn: time.Hour}
	mux := http.NewServeMux()
	check := func(w http.ResponseWriter, r *http.Request, v any) bool {
		if r.Header.Get("Authorization") != "Bearer "+b.auth {
			w.WriteHeader(http.StatusUnauthorized)
			return false
		}
		if err := json.NewDecoder(r.Body).Decode(v); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return false
		}
		return true
	}
	refuse := func(w http.ResponseWriter, status int, reason, detail string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(Refusal{Reason: reason, Detail: detail})
	}
	mux.HandleFunc("POST "+PathToken, func(w http.ResponseWriter, r *http.Request) {
		var req TokenRequest
		if !check(w, r, &req) {
			return
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		b.requests = append(b.requests, req)
		if b.runs != nil && b.runs[req.Run] != req.Subject {
			refuse(w, http.StatusNotFound, ReasonUnknownRun, "the fleet does not know the run")
			return
		}
		if req.Server != "tickets" {
			refuse(w, http.StatusNotFound, ReasonUnknownService, "no service "+req.Server)
			return
		}
		if b.expired[req.Subject] {
			delete(b.expired, req.Subject)
			delete(b.connected, req.Subject)
			refuse(w, http.StatusConflict, ReasonExpired, "the authorization server refused the grant")
			return
		}
		if !b.connected[req.Subject] {
			refuse(w, http.StatusConflict, ReasonNotConnected, "the person has not connected tickets")
			return
		}
		b.n++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(TokenAnswer{Token: fmt.Sprintf("at-%s-%d", req.Subject, b.n), ExpiresAt: time.Now().Add(b.expiresIn)})
	})
	mux.HandleFunc("POST "+PathReject, func(w http.ResponseWriter, r *http.Request) {
		var req RejectRequest
		if !check(w, r, &req) {
			return
		}
		b.mu.Lock()
		b.rejects = append(b.rejects, req)
		b.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST "+PathForget, func(w http.ResponseWriter, r *http.Request) {
		var req ForgetRequest
		if !check(w, r, &req) {
			return
		}
		b.mu.Lock()
		b.forgets = append(b.forgets, req)
		delete(b.connected, req.Subject)
		b.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	b.Server = httptest.NewServer(mux)
	t.Cleanup(b.Close)
	return b
}

func (b *fakeBroker) connect(subject string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.connected[subject] = true
}

func (b *fakeBroker) disconnect(subject string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.connected, subject)
}

func (b *fakeBroker) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.requests)
}

// ticketsSource is a tool source whose tool acts for the person of the
// run through the PersonTokens of the agent.
type ticketsSource struct {
	mu      sync.Mutex
	results []string
}

func (f *ticketsSource) Tools(context.Context) ([]fw.Tool, error) {
	return []fw.Tool{fw.NewTool(fw.ToolSpec{Name: "tickets.list", Source: "tickets"}, func(ctx context.Context, _ json.RawMessage) (fw.ToolResult, error) {
		info := fw.RunInfoFrom(ctx)
		tokens := fw.AgentFrom(ctx).PersonTokens()
		if tokens == nil {
			return fw.ErrorResult("no connections of people"), nil
		}
		_, err := tokens.Token(ctx, info.Principal, "tickets")
		if errors.Is(err, fw.ErrNotConnected) || errors.Is(err, fw.ErrConnectionExpired) {
			reason := fw.ConnectMissing
			if errors.Is(err, fw.ErrConnectionExpired) {
				reason = fw.ConnectExpired
			}
			if err := fw.RequestConnect(ctx, fw.ConnectRequest{Server: "tickets", Title: "Tickets", Reason: reason}); err != nil {
				return fw.ToolResult{}, err
			}
			_, err = tokens.Token(ctx, info.Principal, "tickets")
		}
		if err != nil {
			return fw.ErrorResult(err.Error()), nil
		}
		res := "tickets of " + info.Principal
		f.mu.Lock()
		f.results = append(f.results, res)
		f.mu.Unlock()
		return fw.TextResult(res), nil
	})}, nil
}

// toolThenDone is a model that calls tickets.list once and then ends.
func toolThenDone() fw.Model {
	var mu sync.Mutex
	step := 0
	return fw.ModelFunc{StreamFn: func(context.Context, *fw.Request, func(fw.Delta)) (*fw.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		step++
		if step == 1 {
			return &fw.Response{Content: []fw.Block{{Type: fw.BlockToolUse, ID: "c1", Name: "tickets.list", Input: json.RawMessage(`{}`)}}, StopReason: fw.StopToolUse}, nil
		}
		return &fw.Response{Content: []fw.Block{fw.Text("done")}, StopReason: fw.StopEndTurn}, nil
	}}
}

// newAgent builds an agent with the tokens service against the broker at
// base, and the tickets source.
func newAgent(t *testing.T, base string, src *ticketsSource) *fw.Agent {
	t.Helper()
	t.Setenv("FW_TEST_API_TOKEN", "member-secret")
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Auth.TokenEnv = "FW_TEST_API_TOKEN"
	cfg.Tokens.URL = base
	opts := []fw.Option{fw.WithoutDefaults(), fw.WithConfig(cfg), fw.WithAuthn(fw.NoAuthn{}), fw.WithModel(toolThenDone()),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()), fw.WithLogger(slog.New(slog.DiscardHandler)), fw.WithServices(Start)}
	if src != nil {
		opts = append(opts, fw.WithToolSources(src))
	}
	a, err := fw.New(opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	return a
}

var ada = &fw.Principal{Subject: "ada", Kind: fw.KindUser}

// A disconnect at the broker reaches the member on the bus, and the member
// stops using the token that it cached for the person.
func TestDisconnectDropsTheCachedToken(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	b := newFakeBroker(t)
	a := newAgent(t, b.URL, &ticketsSource{})
	b.connect("ada")
	tok, err := a.PersonTokens().Token(ctx, "ada", "tickets")
	if err != nil || tok == "" {
		t.Fatalf("Token = %q, %v", tok, err)
	}
	n := b.count()
	if _, err := a.PersonTokens().Token(ctx, "ada", "tickets"); err != nil || b.count() != n {
		t.Fatalf("the second token did not come from the cache: %v, requests %d -> %d", err, n, b.count())
	}
	b.disconnect("ada")
	if err := a.Bus().Publish(ctx, fw.DefaultTokensSubject, fw.MustEvent("", EvDisconnected, Disconnected{Subject: "ada", Servers: []string{"tickets"}})); err != nil {
		t.Fatal(err)
	}
	// The notice travels on the bus, so the drop takes a moment.
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err = a.PersonTokens().Token(ctx, "ada", "tickets")
		if errors.Is(err, fw.ErrNotConnected) || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !errors.Is(err, fw.ErrNotConnected) {
		t.Fatalf("Token after the disconnect = %v, want ErrNotConnected", err)
	}
}

func TestRunPausesUntilTheBrokerSaysConnected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	b := newFakeBroker(t)
	src := &ticketsSource{}
	a := newAgent(t, b.URL, src)
	if a.PersonTokens() == nil {
		t.Fatal("the service set no PersonTokens")
	}
	r, err := a.Run(fw.WithPrincipal(ctx, ada), "list my tickets")
	if err != nil || r.State != fw.StateInputRequired || r.Pending == nil || r.Pending.Connect == nil || r.Pending.Connect.Reason != fw.ConnectMissing {
		t.Fatalf("Run = %+v, %v", r, err)
	}
	b.mu.Lock()
	req := b.requests[0]
	b.mu.Unlock()
	if req.Subject != "ada" || req.Server != "tickets" || req.Run != r.ID {
		t.Fatalf("token request = %+v", req)
	}
	// The person connects at the front, which announces it on the bus.
	b.connect("ada")
	if err := a.Bus().Publish(ctx, fw.DefaultTokensSubject, fw.MustEvent("", EvConnected, Connected{Subject: "ada", Servers: []string{"tickets"}})); err != nil {
		t.Fatal(err)
	}
	// The notice travels on the bus, so the decision takes a moment.
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err = a.Wait(ctx, r.ID)
		if err != nil || r.State != fw.StateInputRequired || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || r.State != fw.StateCompleted {
		t.Fatalf("Wait = %+v, %v", r, err)
	}
	src.mu.Lock()
	results := strings.Join(src.results, ",")
	src.mu.Unlock()
	if results != "tickets of ada" {
		t.Fatalf("results = %q", results)
	}
	// The second token comes from the cache.
	n := b.count()
	tok, err := a.PersonTokens().Token(ctx, "ada", "tickets")
	if err != nil || !strings.HasPrefix(tok, "at-ada-") || b.count() != n {
		t.Fatalf("Token = %q, %v, requests %d -> %d", tok, err, n, b.count())
	}
	// A rejected token goes to the broker and leaves the cache.
	a.PersonTokens().Reject(ctx, "ada", "tickets", tok)
	tok2, err := a.PersonTokens().Token(ctx, "ada", "tickets")
	b.mu.Lock()
	rejects := len(b.rejects)
	b.mu.Unlock()
	if err != nil || tok2 == tok || rejects != 1 || b.count() != n+1 {
		t.Fatalf("Token after Reject = %q (was %q), %v, rejects %d", tok2, tok, err, rejects)
	}
	// A forgotten grant is gone at the broker, and the next token asks the
	// person to connect.
	a.PersonTokens().Forget(ctx, "ada", "tickets")
	if _, err := a.PersonTokens().Token(ctx, "ada", "tickets"); !errors.Is(err, fw.ErrNotConnected) {
		t.Fatalf("Token after Forget = %v", err)
	}
	b.mu.Lock()
	forgets := len(b.forgets)
	b.mu.Unlock()
	if forgets != 1 {
		t.Fatalf("forgets = %d", forgets)
	}
}

func TestRefusals(t *testing.T) {
	ctx := context.Background()
	b := newFakeBroker(t)
	a := newAgent(t, b.URL, nil)
	tokens := a.PersonTokens()
	b.connect("ada")
	b.mu.Lock()
	b.expired["ada"] = true
	b.mu.Unlock()
	if _, err := tokens.Token(ctx, "ada", "tickets"); !errors.Is(err, fw.ErrConnectionExpired) {
		t.Fatalf("expired = %v", err)
	}
	if _, err := tokens.Token(ctx, "ada", "tickets"); !errors.Is(err, fw.ErrNotConnected) {
		t.Fatalf("after the refusal = %v", err)
	}
	b.connect("ada")
	err := func() error { _, err := tokens.Token(ctx, "ada", "docs"); return err }()
	if err == nil || !strings.Contains(err.Error(), ReasonUnknownService) || errors.Is(err, fw.ErrNotConnected) {
		t.Fatalf("unknown service = %v", err)
	}
	b.mu.Lock()
	b.runs = map[string]string{"r9": "bob"}
	b.mu.Unlock()
	if _, err := tokens.Token(fw.WithRunInfo(ctx, fw.RunInfo{RunID: "r9", Principal: "ada"}), "ada", "tickets"); err == nil || !strings.Contains(err.Error(), ReasonUnknownRun) {
		t.Fatalf("unknown run = %v", err)
	}
	// A wrong credential is an error, not a connect request.
	b.mu.Lock()
	b.auth = "rotated"
	b.runs = nil
	b.mu.Unlock()
	if _, err := tokens.Token(ctx, "ada", "tickets"); err == nil || !strings.Contains(err.Error(), "did not accept the credential FW_TEST_API_TOKEN") || strings.Contains(err.Error(), "member-secret") {
		t.Fatalf("bad credential = %v", err)
	}
}

func TestStartAndNew(t *testing.T) {
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	a, err := fw.New(fw.WithoutDefaults(), fw.WithConfig(cfg), fw.WithAuthn(fw.NoAuthn{}), fw.WithModel(toolThenDone()),
		fw.WithLedger(fw.NewMemLedger()), fw.WithBus(fw.NewMemBus()), fw.WithLogger(slog.New(slog.DiscardHandler)), fw.WithServices(Start))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	if a.PersonTokens() != nil {
		t.Fatal("the service set PersonTokens without tokens.url")
	}
	for _, bad := range []string{"ftp://x", "front:8080", "http://"} {
		if _, err := New(a, bad, ""); err == nil {
			t.Fatalf("New took %q", bad)
		}
	}
	c, err := New(a, "http://front.example:8080/", "")
	if err != nil || c.base != "http://front.example:8080" || c.cred != "FW_API_TOKEN" {
		t.Fatalf("New = %+v, %v", c, err)
	}
	// The service closes with the agent.
	b := newFakeBroker(t)
	a2 := newAgent(t, b.URL, nil)
	if a2.PersonTokens() == nil {
		t.Fatal("no PersonTokens")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a2.Close(ctx)
	if a2.PersonTokens() != nil {
		t.Fatal("Close left the PersonTokens in place")
	}
}
