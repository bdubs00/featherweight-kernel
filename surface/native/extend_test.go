package native

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// TestExtension mounts an extension with NewWith: its protected route
// needs the credential of the run routes, its public route needs none, and
// its sign-in path reaches the 401 of every route.
func TestExtension(t *testing.T) {
	var got *fw.Agent
	ext := func(k *Kit) error {
		got = k.Agent()
		k.SetSigninPath("/sign/in")
		k.Handle("GET /v1/hello", func(w http.ResponseWriter, r *http.Request) {
			WriteJSON(w, http.StatusOK, map[string]string{"hello": fw.PrincipalFrom(r.Context()).Subject})
		})
		k.HandlePublic("GET /hello", func(w http.ResponseWriter, r *http.Request) {
			WriteProblem(w, r, http.StatusTeapot, "public")
		})
		return nil
	}
	a, err := fw.New(
		fw.WithoutDefaults(),
		fw.WithAuthn(fw.TokenAuthn{Token: "s3cret"}),
		fw.WithModel(newScript().model()),
		fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()),
		fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		fw.WithSurfaces(NewWith(ext)),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	srv := serve(t, a)
	if got != a {
		t.Fatal("the extension got another agent")
	}
	for _, path := range []string{"/v1/hello", "/v1/runs"} {
		resp, b := do(t, "GET", srv.URL+path, "")
		if p := decodeProblem(t, resp, b, http.StatusUnauthorized); p.Signin != "/sign/in" {
			t.Fatalf("%s: problem = %+v", path, p)
		}
	}
	resp, b := do(t, "GET", srv.URL+"/v1/hello", "", "Authorization", "Bearer s3cret")
	var hello map[string]string
	if resp.StatusCode != http.StatusOK || json.Unmarshal(b, &hello) != nil || hello["hello"] != fw.KindToken {
		t.Fatalf("hello = %d %s", resp.StatusCode, b)
	}
	resp, b = do(t, "GET", srv.URL+"/hello", "")
	if p := decodeProblem(t, resp, b, http.StatusTeapot); p.Detail != "public" {
		t.Fatalf("public = %+v", p)
	}

	// A failing extension stops the mount with its error.
	failing := NewWith(func(*Kit) error { return errors.New("boom") })
	if err := failing.Mount(http.NewServeMux(), a); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("mount = %v", err)
	}
}

// fakeRelay answers the route of one agent for the names of agents, as
// the relay of a fleet answers for the profile of a pool of workers. It
// records the caller of each call of TakeAgent and leaves every run route
// to the kernel.
type fakeRelay struct {
	agents  map[string]string // name to description
	callers []string          // the subject of the caller of each TakeAgent
}

func (f *fakeRelay) Take(http.ResponseWriter, *http.Request) bool { return false }

func (f *fakeRelay) TakeAgent(w http.ResponseWriter, r *http.Request, name string) bool {
	f.callers = append(f.callers, fw.PrincipalFrom(r.Context()).Subject)
	d, ok := f.agents[name]
	if !ok {
		return false
	}
	WriteJSON(w, http.StatusOK, map[string]string{"name": name, "description": d})
	return true
}

// runOnlyRelay is a relay without TakeAgent.
type runOnlyRelay struct{ takes int }

func (f *runOnlyRelay) Take(http.ResponseWriter, *http.Request) bool { f.takes++; return false }

func relayAgent(t *testing.T, rl Relay) *fw.Agent {
	t.Helper()
	a, err := fw.New(
		fw.WithoutDefaults(),
		fw.WithAuthn(fw.TokenAuthn{Token: "s3cret"}),
		fw.WithModel(newScript().model()),
		fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()),
		fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		fw.WithSurfaces(NewWith(func(k *Kit) error {
			if rl != nil {
				k.SetRelay(rl)
			}
			return nil
		})),
	)
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

// A relay that implements AgentRelay answers GET /v1/agents/{name} for a
// profile of another process, after the authentication of the caller and
// before the lookup of the kernel. A name that it leaves goes to the
// kernel, and a relay without TakeAgent, or no relay, changes nothing.
func TestAgentRelay(t *testing.T) {
	rl := &fakeRelay{agents: map[string]string{"migrator": "Migrates modules"}}
	srv := serve(t, relayAgent(t, rl))
	auth := []string{"Authorization", "Bearer s3cret"}

	// No credential: the authentication answers, and the relay never runs.
	resp, b := do(t, "GET", srv.URL+"/v1/agents/migrator", "")
	decodeProblem(t, resp, b, http.StatusUnauthorized)
	if len(rl.callers) != 0 {
		t.Fatalf("the relay ran before the authentication: %q", rl.callers)
	}

	// A profile of another process: the relay answers.
	var got map[string]string
	resp, b = do(t, "GET", srv.URL+"/v1/agents/migrator", "", auth...)
	decodeJSON(t, resp, b, http.StatusOK, &got)
	if got["name"] != "migrator" || got["description"] != "Migrates modules" {
		t.Fatalf("relayed agent = %s", b)
	}
	if len(rl.callers) != 1 || rl.callers[0] != fw.KindToken {
		t.Fatalf("callers of TakeAgent = %q", rl.callers)
	}

	// A name that the relay leaves: the kernel answers as it does without
	// a relay.
	var own struct{ Name string }
	resp, b = do(t, "GET", srv.URL+"/v1/agents/default", "", auth...)
	decodeJSON(t, resp, b, http.StatusOK, &own)
	if own.Name != "default" {
		t.Fatalf("own agent = %s", b)
	}
	resp, b = do(t, "GET", srv.URL+"/v1/agents/nope", "", auth...)
	if p := decodeProblem(t, resp, b, http.StatusNotFound); !strings.Contains(p.Detail, "nope") {
		t.Fatalf("unknown agent = %+v", p)
	}
	if len(rl.callers) != 3 {
		t.Fatalf("TakeAgent ran %d times, want 3", len(rl.callers))
	}

	// A relay without TakeAgent, and no relay, leave the route to the
	// kernel.
	ro := &runOnlyRelay{}
	for _, srv := range []string{serve(t, relayAgent(t, ro)).URL, serve(t, relayAgent(t, nil)).URL} {
		resp, b = do(t, "GET", srv+"/v1/agents/migrator", "", auth...)
		decodeProblem(t, resp, b, http.StatusNotFound)
		resp, b = do(t, "GET", srv+"/v1/agents/default", "", auth...)
		decodeJSON(t, resp, b, http.StatusOK, &own)
	}
	if ro.takes != 0 {
		t.Fatalf("the route of one agent called Take %d times", ro.takes)
	}
}
