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
