package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func TestSPIFFEBearer(t *testing.T) {
	ctx := context.Background()
	hl := &headerLog{}
	ts := serveHTTP(t, newTestMCPServer(nil), hl.wrap)
	s := newServer(t, fw.MCPServerConfig{
		Name:     "test",
		Endpoint: ts.URL,
		Auth:     fw.MCPAuthConfig{SPIFFE: &fw.SPIFFEAuthConfig{Audience: "git-mcp"}},
	}, nil)
	if s.rt.svid == nil {
		t.Fatal("auth.spiffe set but no token source")
	}
	var fetched []string
	s.rt.svid.Fetch = func(_ context.Context, aud string) (string, time.Time, error) {
		fetched = append(fetched, aud)
		return "svid-token", time.Now().Add(time.Hour), nil
	}
	tools, err := s.Tools(ctx)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	res, err := findTool(t, tools, "test.echo").Call(ctx, json.RawMessage(`{"msg":"hi"}`))
	if err != nil || fw.TextOf(res.Content) != "echo: hi" {
		t.Fatalf("Call echo = %+v, %v", res, err)
	}
	seen := hl.all()
	if len(seen) == 0 {
		t.Fatal("no request reached the server")
	}
	for i, h := range seen {
		if got := h.Get("Authorization"); got != "Bearer svid-token" {
			t.Errorf("request %d: Authorization = %q, want the JWT-SVID", i, got)
		}
	}
	if len(fetched) != 1 || fetched[0] != "git-mcp" {
		t.Errorf("fetched = %v, want one fetch for git-mcp", fetched)
	}
}

func TestSPIFFEDefaultAudience(t *testing.T) {
	s := newServer(t, fw.MCPServerConfig{
		Name:     "git",
		Endpoint: "https://git-mcp.example.org/mcp",
		Auth:     fw.MCPAuthConfig{SPIFFE: &fw.SPIFFEAuthConfig{}},
	}, nil)
	if s.rt.svid == nil || s.rt.svid.Audience != "https://git-mcp.example.org/mcp" {
		t.Fatalf("token source = %+v, want the endpoint as the audience", s.rt.svid)
	}
	plain := newServer(t, fw.MCPServerConfig{Name: "plain", Endpoint: "https://git-mcp.example.org/mcp"}, nil)
	if plain.rt.svid != nil {
		t.Fatal("token source set without auth.spiffe")
	}
}

func TestSPIFFESocketFromAuth(t *testing.T) {
	cfg := fw.DefaultConfig()
	cfg.Auth.SPIFFE = &fw.SPIFFEConfig{TrustDomain: "example.org", Socket: "unix:///run/spire/agent.sock"}
	cfg.Tools.MCP = []fw.MCPServerConfig{{
		Name:     "git",
		Endpoint: "https://git-mcp.example.org/mcp",
		Auth:     fw.MCPAuthConfig{SPIFFE: &fw.SPIFFEAuthConfig{Audience: "git-mcp"}},
	}}
	out, err := newSources(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("newSources: %v", err)
	}
	s := out[0].(*Server)
	t.Cleanup(func() { _ = s.Close() })
	if s.rt.svid.Socket != "unix:///run/spire/agent.sock" {
		t.Fatalf("Socket = %q, want auth.spiffe.socket", s.rt.svid.Socket)
	}
}

func TestSPIFFEConfigErrors(t *testing.T) {
	cases := []struct {
		name string
		cfg  fw.MCPServerConfig
		want string
	}{
		{"with bearer", fw.MCPServerConfig{Name: "a", Endpoint: "https://x", Auth: fw.MCPAuthConfig{
			Bearer: "T", SPIFFE: &fw.SPIFFEAuthConfig{}}}, "auth.spiffe alone"},
		{"with oauth", fw.MCPServerConfig{Name: "a", Endpoint: "https://x", Auth: fw.MCPAuthConfig{
			OAuth: &fw.OAuthClientConfig{ClientID: "c", ClientSecret: "S"}, SPIFFE: &fw.SPIFFEAuthConfig{}}}, "auth.spiffe alone"},
		{"on command", fw.MCPServerConfig{Name: "a", Command: "srv", Auth: fw.MCPAuthConfig{
			SPIFFE: &fw.SPIFFEAuthConfig{}}}, "endpoint only"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := New(context.Background(), c.cfg, nil, slog.New(slog.DiscardHandler))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("New error = %v, want it to contain %q", err, c.want)
			}
		})
	}
}
