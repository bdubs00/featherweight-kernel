package ax

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func unsetEnv(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
}

func TestApplyToConfig(t *testing.T) {
	unsetEnv(t, EnvAtespace)
	unsetEnv(t, EnvBootstrapTimeout)

	cfg := fw.DefaultConfig()
	secure := fw.MCPServerConfig{Name: "git", Endpoint: "https://git-mcp:8443/mcp", Trusted: true, Auth: fw.MCPAuthConfig{Bearer: "GIT_MCP_TOKEN"}}
	cfg.Tools.MCP = []fw.MCPServerConfig{secure}

	task := &Task{Metadata: ObjectMeta{Name: "reviewer", Atespace: "team-a"}}
	ws1 := &Workspace{Metadata: ObjectMeta{Name: "one"}, Spec: WorkspaceSpec{MCP: MCPConfig{
		Registries: []MCPRegistry{{Provider: "google", Query: "mcp.tags:build"}},
		Servers: []MCPServer{
			{Name: "git", Endpoint: "http://git-mcp.default.svc.cluster.local:8080"},
			{Name: "search", Command: "search-mcp", Args: []string{"--stdio", "-v"}},
			{Endpoint: "http://nameless"},
		},
	}}}
	ws2 := &Workspace{Metadata: ObjectMeta{Name: "two"}, Spec: WorkspaceSpec{MCP: MCPConfig{
		Servers: []MCPServer{{Name: "search", Endpoint: "http://second"}, {Name: "docs", Endpoint: "http://docs"}},
	}}}
	bindings := []Binding{
		{Ref: WorkspaceRef{Name: "one"}, Workspace: ws1, Path: "/workspace/one"},
		{Ref: WorkspaceRef{Name: "missing"}, Path: "/workspace/missing"},
		{Ref: WorkspaceRef{Name: "two"}, Workspace: ws2, Path: "/workspace/two"},
	}
	ApplyToConfig(cfg, task, bindings)

	if cfg.Listen != ":80" {
		t.Errorf("Listen = %q", cfg.Listen)
	}
	if cfg.Home != "/workspace/.fw" {
		t.Errorf("Home = %q", cfg.Home)
	}
	if cfg.Tools.Files.Root != "/workspace/one" {
		t.Errorf("Files.Root = %q", cfg.Tools.Files.Root)
	}
	want := []fw.MCPServerConfig{
		secure,
		{Name: "search", Command: "search-mcp", Args: []string{"--stdio", "-v"}},
		{Name: "docs", Endpoint: "http://docs"},
	}
	if !reflect.DeepEqual(cfg.Tools.MCP, want) {
		t.Errorf("MCP = %+v\nwant %+v", cfg.Tools.MCP, want)
	}
	ws1.Spec.MCP.Servers[1].Args[0] = "changed"
	if cfg.Tools.MCP[1].Args[0] != "--stdio" {
		t.Error("MCP args share memory with the manifest")
	}
	p, ok := cfg.Agents["bootstrap"]
	if !ok {
		t.Fatal("no bootstrap profile")
	}
	// The empty access block shows the profile to the admins only, even
	// when access.default is open.
	if p.Access == nil {
		t.Error("bootstrap has no access block")
	} else {
		access := cfg.Access
		access.Admins = []string{"root@example.com"}
		z := fw.NewRulesAuthz(access, func(name string) (fw.AgentProfile, bool) { q, ok := cfg.Agents[name]; return q, ok })
		alice := &fw.Principal{Subject: "alice@example.com", Kind: fw.KindUser}
		admin := &fw.Principal{Subject: "root@example.com", Kind: fw.KindUser}
		if err := z.Allow(context.Background(), alice, fw.ActionAgentView, fw.Target{Agent: "bootstrap"}); err == nil {
			t.Error("a person can see the bootstrap profile")
		}
		if err := z.Allow(context.Background(), admin, fw.ActionAgentView, fw.Target{Agent: "bootstrap"}); err != nil {
			t.Errorf("an admin cannot see the bootstrap profile: %v", err)
		}
	}
	if !reflect.DeepEqual(p.Tools, []string{"bash", "files"}) {
		t.Errorf("bootstrap tools = %v", p.Tools)
	}
	if p.Budget == nil || p.Budget.Deadline != 10*time.Minute {
		t.Errorf("bootstrap budget = %+v", p.Budget)
	}
	if !strings.Contains(p.System, "change files only inside it") {
		t.Errorf("bootstrap system = %q", p.System)
	}
	if got := os.Getenv(EnvAtespace); got != "team-a" {
		t.Errorf("FW_ATESPACE = %q", got)
	}

	// A second call changes nothing, and values already set win.
	cfg.Home = "/data/fw"
	cfg.Tools.Files.Root = "/workspace/one/src"
	ApplyToConfig(cfg, &Task{Metadata: ObjectMeta{Atespace: "other"}}, bindings)
	if cfg.Home != "/data/fw" || cfg.Tools.Files.Root != "/workspace/one/src" {
		t.Errorf("Home, Root = %q, %q", cfg.Home, cfg.Tools.Files.Root)
	}
	if len(cfg.Tools.MCP) != 3 {
		t.Errorf("second call: MCP %d", len(cfg.Tools.MCP))
	}
	if got := os.Getenv(EnvAtespace); got != "team-a" {
		t.Errorf("FW_ATESPACE overwritten: %q", got)
	}
	if cfg.Fleet.Name != "team-a/reviewer" || cfg.Fleet.URL != "ax:team-a/reviewer" {
		t.Errorf("fleet name %q, url %q", cfg.Fleet.Name, cfg.Fleet.URL)
	}
}

func TestApplyToConfigKeepsFleetIdentity(t *testing.T) {
	unsetEnv(t, EnvAtespace)
	cfg := fw.DefaultConfig()
	cfg.Fleet.Name, cfg.Fleet.URL = "mine", "http://mine:8080"
	ApplyToConfig(cfg, &Task{Metadata: ObjectMeta{Name: "reviewer", Atespace: "team-a"}}, nil)
	if cfg.Fleet.Name != "mine" || cfg.Fleet.URL != "http://mine:8080" {
		t.Errorf("fleet name %q, url %q", cfg.Fleet.Name, cfg.Fleet.URL)
	}
}

func TestApplyToConfigKeepsBootstrapProfile(t *testing.T) {
	t.Setenv(EnvBootstrapTimeout, "3m")
	own := fw.AgentProfile{System: "mine", Tools: []string{"bash"}}
	// The surfaces stay as they are: the metadata endpoints are no surface.
	cfg := &fw.Config{Agents: map[string]fw.AgentProfile{"bootstrap": own}, Surfaces: []string{"native"}}
	ApplyToConfig(cfg, nil, Bindings(nil, nil))
	if !reflect.DeepEqual(cfg.Agents["bootstrap"], own) {
		t.Errorf("bootstrap = %+v", cfg.Agents["bootstrap"])
	}
	if cfg.Tools.Files.Root != "/workspace" || !reflect.DeepEqual(cfg.Surfaces, []string{"native"}) {
		t.Errorf("Root %q, Surfaces %v", cfg.Tools.Files.Root, cfg.Surfaces)
	}

	cfg = &fw.Config{}
	ApplyToConfig(cfg, nil, nil)
	if d := cfg.Agents["bootstrap"].Budget.Deadline; d != 3*time.Minute {
		t.Errorf("deadline = %v, want 3m", d)
	}
}

func TestBootstrapTimeout(t *testing.T) {
	for raw, want := range map[string]time.Duration{
		"":      10 * time.Minute,
		"90s":   90 * time.Second,
		"later": 10 * time.Minute,
		"-5m":   10 * time.Minute,
	} {
		t.Setenv(EnvBootstrapTimeout, raw)
		if got := BootstrapTimeout(); got != want {
			t.Errorf("BootstrapTimeout(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestBootstrapInput(t *testing.T) {
	got := BootstrapInput("run the tests", "/workspace/app")
	if !strings.Contains(got, "/workspace/app") || !strings.HasSuffix(got, "run the tests") {
		t.Errorf("BootstrapInput = %q", got)
	}
}

func TestIdentity(t *testing.T) {
	if a, n := Identity(nil); a != "" || n != "" {
		t.Errorf("Identity(nil) = %q, %q", a, n)
	}
}
