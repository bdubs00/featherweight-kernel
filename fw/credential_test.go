package fw

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRequireAuthKeepsCredential(t *testing.T) {
	a := newTestAgent(t, newScript(endTurn("hi")).Model(), WithAuthn(TokenAuthn{Token: "s3cret"}))
	var got string
	h := RequireAuth(a, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = CredentialFrom(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/runs", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got != "Bearer s3cret" {
		t.Fatalf("credential = %q", got)
	}
	if CredentialFrom(context.Background()) != "" {
		t.Fatal("an empty context carries a credential")
	}
}

func TestTokenFromAFileWithANewline(t *testing.T) {
	t.Setenv("FW_API_TOKEN", "s3cret\n")
	cfg := DefaultConfig()
	authn, err := NewAuthn(context.Background(), cfg, EnvSecrets{})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/runs", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	if _, err := authn.Authenticate(req); err != nil {
		t.Fatalf("a token with a trailing newline in its secret = %v", err)
	}
}

func TestIDTime(t *testing.T) {
	before := time.Now().Add(-time.Millisecond)
	at, ok := IDTime(NewID())
	if !ok || at.Before(before.Truncate(time.Millisecond)) || at.After(time.Now()) {
		t.Fatalf("IDTime(NewID()) = %v, %v", at, ok)
	}
	// Too short, a first character past 7, and a letter outside the alphabet.
	for _, bad := range []string{"", "short", "8ZZZZZZZZZZZZZZZZZZZZZZZZZ", "0IJA3Q9Z6K2M4N8P0R1S2T3U4V"} {
		if _, ok := IDTime(bad); ok {
			t.Errorf("IDTime(%q) reports a time", bad)
		}
	}
}
