package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/bdubs00/featherweight-kernel/fw"
)

// TestForbiddenIsPermissionDenied checks that a refusal of the kernel reaches
// the client as the A2A permission-denied error. The configuration is
// closed and names no admin, so the anonymous caller operates no agent.
func TestForbiddenIsPermissionDenied(t *testing.T) {
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Access.Default = "closed"
	e := newEnv(t, newScript(endTurn("never")).model(), fw.WithConfig(cfg), fw.WithProfile(fw.AgentProfile{Name: "reviewer"}))
	_, err := e.rpc.SendMessage(testCtx(t), &sdk.SendMessageRequest{Message: userMsg("review")})
	if !errors.Is(err, sdk.ErrUnauthorized) {
		t.Fatalf("SendMessage = %v, want the permission-denied error", err)
	}
	if err := mapErr(fw.ErrForbidden); !errors.Is(err, sdk.ErrUnauthorized) || !errors.Is(err, fw.ErrForbidden) {
		t.Fatalf("mapErr = %v", err)
	}
}

// TestAuthzUnavailableIsServerError checks that a control plane that does
// not answer reaches the client as the A2A server error, not as a refusal.
func TestAuthzUnavailableIsServerError(t *testing.T) {
	down := fw.AuthzFunc(func(context.Context, *fw.Principal, string, fw.Target) error {
		return fmt.Errorf("%w: test", fw.ErrAuthzUnavailable)
	})
	e := newEnv(t, newScript(endTurn("never")).model(), fw.WithAuthz(down))
	_, err := e.rpc.SendMessage(testCtx(t), &sdk.SendMessageRequest{Message: userMsg("review")})
	if !errors.Is(err, sdk.ErrServerError) || errors.Is(err, sdk.ErrUnauthorized) {
		t.Fatalf("SendMessage = %v, want the server error", err)
	}
	if err := mapErr(fmt.Errorf("%w: test", fw.ErrAuthzUnavailable)); !errors.Is(err, sdk.ErrServerError) || !errors.Is(err, fw.ErrAuthzUnavailable) {
		t.Fatalf("mapErr = %v", err)
	}
}

// downAuthz is a control plane that never answers.
var downAuthz = fw.AuthzFunc(func(context.Context, *fw.Principal, string, fw.Target) error {
	return fmt.Errorf("%w: the bus subject fw.authz did not answer", fw.ErrAuthzUnavailable)
})

// postRaw posts a JSON body and returns the response and its body.
func postRaw(t *testing.T, e *env, path, body string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Post(e.srv.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

// TestAuthzUnavailableREST checks that the REST binding answers a control
// plane that did not answer with a 503, Retry-After, and the reason.
func TestAuthzUnavailableREST(t *testing.T) {
	e := newEnv(t, newScript(endTurn("never")).model(), fw.WithAuthz(downAuthz))
	resp, body := postRaw(t, e, "/a2a/message:send", `{"message":{"role":"ROLE_USER","messageId":"m1","parts":[{"text":"review"}]}}`)
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "5" {
		t.Fatalf("status %s, Retry-After %q, body %s", resp.Status, resp.Header.Get("Retry-After"), body)
	}
	var doc struct {
		Error struct {
			Code    int              `json:"code"`
			Status  string           `json:"status"`
			Message string           `json:"message"`
			Details []map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("body %s: %v", body, err)
	}
	if doc.Error.Code != 503 || doc.Error.Status != "UNAVAILABLE" || !strings.Contains(doc.Error.Message, "did not answer") {
		t.Fatalf("error = %+v", doc.Error)
	}
	var reason, meta, retry bool
	for _, d := range doc.Error.Details {
		if d["reason"] == "SERVER_ERROR" {
			m, _ := d["metadata"].(map[string]any)
			meta = m["fwReason"] == reasonUnavailable && m["retryAfter"] == "5"
		}
		if d["reason"] == reasonUnavailable {
			reason, retry = true, d["retryAfterSeconds"] == float64(5)
		}
	}
	if !reason || !meta || !retry {
		t.Fatalf("details = %v", doc.Error.Details)
	}
}

// TestAuthzUnavailableJSONRPC checks that the JSON-RPC binding carries the
// reason and the retry hint in the data of the server error.
func TestAuthzUnavailableJSONRPC(t *testing.T) {
	e := newEnv(t, newScript(endTurn("never")).model(), fw.WithAuthz(downAuthz))
	resp, body := postRaw(t, e, PathJSONRPC, `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"role":"ROLE_USER","messageId":"m1","parts":[{"text":"review"}]}}}`)
	var doc struct {
		Error struct {
			Code int             `json:"code"`
			Data json.RawMessage `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("status %s, body %s: %v", resp.Status, body, err)
	}
	if doc.Error.Code != -32000 || !strings.Contains(string(doc.Error.Data), reasonUnavailable) || !strings.Contains(string(doc.Error.Data), `"retryAfterSeconds":5`) {
		t.Fatalf("status %s, error = %d %s", resp.Status, doc.Error.Code, doc.Error.Data)
	}
}

// TestForbiddenRESTStays403 checks that the wrapper leaves a refusal alone.
func TestForbiddenRESTStays403(t *testing.T) {
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Access.Default = "closed"
	e := newEnv(t, newScript(endTurn("never")).model(), fw.WithConfig(cfg))
	resp, body := postRaw(t, e, "/a2a/message:send", `{"message":{"role":"ROLE_USER","messageId":"m1","parts":[{"text":"review"}]}}`)
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get("Retry-After") != "" {
		t.Fatalf("status %s, body %s", resp.Status, body)
	}
}

func TestUnavailableStatusWrapper(t *testing.T) {
	serve := func(h http.HandlerFunc) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		unavailableStatus(h).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", nil))
		return rec
	}
	other := `{"error":{"code":500,"status":"INTERNAL","message":"boom","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"INTERNAL_ERROR","domain":"a2a-protocol.org"}]}}` + "\n"
	rec := serve(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, other)
	})
	if rec.Code != 500 || rec.Body.String() != other || rec.Header().Get("Retry-After") != "" {
		t.Fatalf("another 500 = %d %q", rec.Code, rec.Body.String())
	}
	rec = serve(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write(bytes.Repeat([]byte("x"), maxHeldBody+10))
	})
	if rec.Code != 500 || rec.Body.Len() != maxHeldBody+10 {
		t.Fatalf("a large 500 = %d, %d bytes", rec.Code, rec.Body.Len())
	}
	rec = serve(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "data: two\n\n")
	})
	if rec.Code != 200 || !rec.Flushed || rec.Body.String() != "data: one\n\ndata: two\n\n" {
		t.Fatalf("a stream = %d flushed=%v %q", rec.Code, rec.Flushed, rec.Body.String())
	}
}

// TestAuthzUnavailableRESTStream checks that a stream that fails at its
// first event gets the same 503 as a plain call, and that a stream that
// works passes through as a stream.
func TestAuthzUnavailableRESTStream(t *testing.T) {
	msg := `{"message":{"role":"ROLE_USER","messageId":"m1","parts":[{"text":"review"}]}}`
	e := newEnv(t, newScript(endTurn("never")).model(), fw.WithAuthz(downAuthz))
	resp, body := postRaw(t, e, "/a2a/message:stream", msg)
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "5" ||
		!strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") ||
		!strings.Contains(string(body), `"status":"UNAVAILABLE"`) || !strings.Contains(string(body), `"code":503`) {
		t.Fatalf("status %s, type %q, body %s", resp.Status, resp.Header.Get("Content-Type"), body)
	}

	ok := newEnv(t, newScript(endTurn("reviewed")).model())
	resp, body = postRaw(t, ok, "/a2a/message:stream", msg)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") ||
		!strings.Contains(string(body), "reviewed") {
		t.Fatalf("a working stream: status %s, type %q, body %s", resp.Status, resp.Header.Get("Content-Type"), body)
	}
}

func TestUnavailableStatusWrapperStreams(t *testing.T) {
	serve := func(first string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			_, _ = io.WriteString(w, "id: 1\n")
			_, _ = io.WriteString(w, first)
			w.(http.Flusher).Flush()
			_, _ = io.WriteString(w, "data: after\n\n")
		})
		unavailableStatus(h).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", nil))
		return rec
	}
	rec := serve("data: {\"task\":{}}\n\n")
	if rec.Code != 200 || rec.Body.String() != "id: 1\ndata: {\"task\":{}}\n\ndata: after\n\n" || !rec.Flushed {
		t.Fatalf("a normal stream = %d %q", rec.Code, rec.Body.String())
	}
	errEvent := `data: {"error":{"code":500,"status":"INTERNAL","message":"down","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"SERVER_ERROR","domain":"a2a-protocol.org","metadata":{"fwReason":"AUTHZ_UNAVAILABLE"}}]}}` + "\n\n"
	rec = serve(errEvent)
	if rec.Code != 503 || rec.Header().Get("Retry-After") != "5" || strings.Contains(rec.Body.String(), "after") ||
		!strings.Contains(rec.Body.String(), `"status":"UNAVAILABLE"`) {
		t.Fatalf("an unavailable stream = %d %q", rec.Code, rec.Body.String())
	}
}
