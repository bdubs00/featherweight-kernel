package builtin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func newTestHTTP(t *testing.T, allow ...string) fw.Tool {
	t.Helper()
	cfg := fw.DefaultConfig()
	cfg.Tools.HTTP.Allow = allow
	tool, err := fw.NewBuiltinTool("http", cfg)
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

func TestHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/hello":
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("X-Hidden", "not shown")
			_, _ = io.WriteString(w, "hi there")
		case "/api/echo":
			b, _ := io.ReadAll(r.Body)
			_, _ = io.WriteString(w, r.Method+" "+r.Header.Get("X-Test")+" "+string(b))
		case "/api/redirect":
			http.Redirect(w, r, "/private", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	tool := newTestHTTP(t, srv.URL+"/api/*")
	ctx := context.Background()

	got := mustOK(t, call(t, ctx, tool, `{"url":"`+srv.URL+`/api/hello"}`))
	if !strings.HasPrefix(got, "HTTP/1.1 200 OK\n") || !strings.Contains(got, "Content-Type: text/plain\n") ||
		!strings.HasSuffix(got, "\n\nhi there") || strings.Contains(got, "X-Hidden") {
		t.Fatalf("GET: %q", got)
	}

	in, _ := json.Marshal(map[string]any{
		"url": srv.URL + "/api/echo", "method": "POST", "body": "payload",
		"headers": map[string]string{"X-Test": "yes"},
	})
	got = mustOK(t, call(t, ctx, tool, string(in)))
	if !strings.HasSuffix(got, "POST yes payload") {
		t.Fatalf("POST: %q", got)
	}

	got = mustOK(t, call(t, ctx, tool, `{"url":"`+srv.URL+`/api/missing"}`))
	if !strings.HasPrefix(got, "HTTP/1.1 404") {
		t.Fatalf("404 is a normal result: %q", got)
	}

	mustFail(t, call(t, ctx, tool, `{"url":"`+srv.URL+`/private"}`), "not on the allowlist")
	mustFail(t, call(t, ctx, tool, `{"url":"https://example.com/api/hello"}`), "not on the allowlist")
	mustFail(t, call(t, ctx, tool, `{"url":"`+srv.URL+`/api/redirect"}`), "redirect")
	mustFail(t, call(t, ctx, tool, `{"url":"`+srv.URL+`/api/hello","method":"DELETE"}`), "GET or POST")
	mustFail(t, call(t, ctx, tool, `{"url":"file:///etc/passwd"}`), "not a full http")
}

func TestHTTPEmptyAllowlist(t *testing.T) {
	tool := newTestHTTP(t)
	mustFail(t, call(t, context.Background(), tool, `{"url":"https://example.com/"}`), "tools.http.allow")
}

func TestHTTPAllowGlobs(t *testing.T) {
	h := &httpTool{allow: []string{"https://*.example.com/v1/*", "https://exact.test/path", "http://plain.test/*"}}
	for u, want := range map[string]bool{
		"https://api.example.com/v1/a/b?c=d": true,
		"https://api.example.com/v2/a":       false,
		"https://exact.test/path":            true,
		"https://exact.test/path/more":       false,
		"http://plain.test/a/b/c":            true,
		"http://plain.test.evil.com/":        false,
	} {
		if got := h.allowed(u); got != want {
			t.Errorf("allowed(%q) = %v, want %v", u, got, want)
		}
	}
}
