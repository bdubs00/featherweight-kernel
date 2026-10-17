package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// connectionsLine returns the line of fw doctor about connections.
func connectionsLine(t *testing.T, out string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "connections: ") {
			return l
		}
	}
	t.Fatalf("no connections line:\n%s", out)
	return ""
}

func TestDoctorConnections(t *testing.T) {
	home := env(t)
	t.Setenv("FW_GRANT_KEY", "")
	t.Setenv("FW_GRANT_KEY_FILE", "")
	t.Setenv("ANTHROPIC_API_KEY", "sk-doctor-secret")
	t.Setenv("ANTHROPIC_API_KEY_FILE", "")
	doctorSeams(t, &scriptModel{}, nil, nil)
	dir := filepath.Join(home, "grants")

	// No service acts for a person. The line names the store and writes
	// nothing.
	_, out, _ := run(t, "", "doctor")
	want := "ok    connections: no service acts for a person; grants: file " + dir + ", key in " + filepath.Join(dir, "grant.key") + " (set FW_GRANT_KEY to share one key)"
	if got := connectionsLine(t, out); got != want {
		t.Errorf("line = %q\nwant   %q", got, want)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("fw doctor made the grant directory: %v", err)
	}

	// The key secret wins, and its value never shows.
	t.Setenv("FW_GRANT_KEY", "grant-key-secret")
	_, out, errOut := run(t, "", "doctor")
	if got := connectionsLine(t, out); !strings.HasSuffix(got, "grants: file "+dir+", key from FW_GRANT_KEY") {
		t.Errorf("line = %q", got)
	}
	if strings.Contains(out+errOut, "grant-key-secret") || strings.Contains(out+errOut, "sk-doctor") {
		t.Error("the grant key leaked")
	}

	// A service that acts for a person needs oidc.
	t.Setenv("FW_TOOLS_MCP_TICKETS_ENDPOINT", "https://tickets.example.com/mcp")
	t.Setenv("FW_TOOLS_MCP_TICKETS_AUTH_OAUTH_GRANT", "user")
	code, out, _ := run(t, "", "doctor")
	if got := connectionsLine(t, out); code != 1 || got != "FAIL  connections: tickets acts for a person, but auth.mode (FW_AUTH_MODE) has no oidc, so no person can connect" {
		t.Errorf("exit %d, line %q", code, got)
	}

	// Without a client of its own, it needs the sign-in client.
	issuer := fakeIssuer(t, true)
	t.Setenv("FW_AUTH_MODE", "oidc")
	t.Setenv("FW_AUTH_OIDC_ISSUER", issuer.URL)
	t.Setenv("FW_AUTH_OIDC_INSECURE", "true")
	code, out, _ = run(t, "", "doctor")
	if got := connectionsLine(t, out); code != 1 || got != "FAIL  connections: tickets uses the sign-in client, but auth.oidc.clientId (FW_AUTH_OIDC_CLIENT_ID) is not set" {
		t.Errorf("exit %d, line %q", code, got)
	}

	// A service with a client of its own works without the sign-in client,
	// and the line says what that costs.
	t.Setenv("FW_TOOLS_MCP_TICKETS_AUTH_OAUTH_CLIENT_ID", "fw-tickets")
	t.Setenv("FW_TOOLS_MCP_DOCS_ENDPOINT", "https://docs.example.com/mcp")
	t.Setenv("FW_TOOLS_MCP_DOCS_AUTH_OAUTH_GRANT", "user")
	t.Setenv("FW_TOOLS_MCP_DOCS_AUTH_OAUTH_CLIENT_ID", "fw-docs")
	code, out, _ = run(t, "", "doctor")
	if got := connectionsLine(t, out); code != 0 || !strings.HasPrefix(got, "ok    connections: docs, tickets act for a person; grants: file ") ||
		!strings.HasSuffix(got, "browser sign-in is off (FW_AUTH_OIDC_CLIENT_ID), so a person who opens a connect link pastes a token first") {
		t.Errorf("exit %d, line %q", code, got)
	}

	// With the sign-in client, the line is plain, and the memory store
	// says that grants end with the process.
	t.Setenv("FW_TOOLS_MCP_TICKETS_AUTH_OAUTH_CLIENT_ID", "")
	t.Setenv("FW_AUTH_OIDC_CLIENT_ID", "fw")
	t.Setenv("FW_GRANTS_STORE", "memory")
	code, out, _ = run(t, "", "doctor")
	if got := connectionsLine(t, out); code != 0 || got != "ok    connections: docs, tickets act for a person; grants: memory, so they end with the process" {
		t.Errorf("exit %d, line %q", code, got)
	}

	// A store that no driver provides fails the line.
	t.Setenv("FW_GRANTS_STORE", "bogus")
	code, out, _ = run(t, "", "doctor")
	if got := connectionsLine(t, out); code != 1 || !strings.HasPrefix(got, "FAIL  connections: ") || !strings.Contains(got, `grants.store "bogus"`) {
		t.Errorf("exit %d, line %q", code, got)
	}
}
