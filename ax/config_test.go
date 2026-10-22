package ax

import (
	"context"
	"os"
	"path/filepath"
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

// Under ax a process that runs as root starts its tools as the tool user
// of the images, and the tool user owns the volume and every binding.
func TestApplyToConfigToolUser(t *testing.T) {
	unsetEnv(t, EnvAtespace)
	bindings := []Binding{{Ref: WorkspaceRef{Name: "a"}, Path: "/workspace/a"}, {Ref: WorkspaceRef{Name: "b"}, Path: "/src/b"}}
	cfg := fw.DefaultConfig()
	ApplyToConfig(cfg, &Task{}, bindings)
	want := ""
	if os.Geteuid() == 0 && len(fw.MissingToolCaps()) == 0 {
		want = DefaultToolUser
	}
	if cfg.Tools.User != want {
		t.Errorf("Tools.User = %q, want %q", cfg.Tools.User, want)
	}
	if !reflect.DeepEqual(cfg.Workspaces, []string{DefaultWorkspacePath, "/workspace/a", "/src/b"}) {
		t.Errorf("Workspaces = %v", cfg.Workspaces)
	}

	cfg = fw.DefaultConfig()
	cfg.Tools.User = fw.ToolUserNone
	ApplyToConfig(cfg, &Task{}, bindings)
	if cfg.Tools.User != fw.ToolUserNone {
		t.Errorf("an explicit tools.user became %q", cfg.Tools.User)
	}
}

// Under ax each run gets a directory of its own unless the configuration
// says otherwise.
func TestApplyToConfigPerRun(t *testing.T) {
	unsetEnv(t, EnvAtespace)
	cfg := fw.DefaultConfig()
	ApplyToConfig(cfg, &Task{}, nil)
	if !cfg.Tools.Files.PerRunOn() {
		t.Error("perRun is off under ax")
	}
	off := false
	cfg = fw.DefaultConfig()
	cfg.Tools.Files.PerRun = &off
	ApplyToConfig(cfg, &Task{}, nil)
	if cfg.Tools.Files.PerRunOn() {
		t.Error("an explicit perRun: false became true")
	}
}

// Under ax each person keeps the notes of the memory apart unless the
// configuration names a scope.
func TestApplyToConfigMemoryScope(t *testing.T) {
	unsetEnv(t, EnvAtespace)
	cfg := fw.DefaultConfig()
	ApplyToConfig(cfg, &Task{}, nil)
	if cfg.Tools.Memory.Scope != fw.MemoryScopePrincipal {
		t.Errorf("scope under ax = %q, want principal", cfg.Tools.Memory.Scope)
	}
	for _, scope := range []string{fw.MemoryScopeAgent, fw.MemoryScopeShared} {
		cfg = fw.DefaultConfig()
		cfg.Tools.Memory.Scope = scope
		ApplyToConfig(cfg, &Task{}, nil)
		if cfg.Tools.Memory.Scope != scope {
			t.Errorf("an explicit scope %s became %q", scope, cfg.Tools.Memory.Scope)
		}
	}
}

// The goal of a binding prepares the workspace that every run shares, so
// its run works in the root of the files tool, also with perRun.
func TestBootstrapWorksInTheRoot(t *testing.T) {
	unsetEnv(t, EnvAtespace)
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Tools.Files.Root = t.TempDir()
	cfg.Tools.User = fw.ToolUserNone
	ApplyToConfig(cfg, &Task{}, nil)
	ledger := fw.NewMemLedger()
	a, err := fw.New(fw.WithConfig(cfg), fw.WithoutDefaults(), fw.WithAuthn(fw.NoAuthn{}), fw.WithLedger(ledger), fw.WithBus(fw.NewMemBus()),
		fw.WithModel(fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
			return &fw.Response{Content: []fw.Block{fw.Text("ready")}, StopReason: fw.StopEndTurn}, nil
		}}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(context.Background()) })
	if err := Bootstrap(context.Background(), a, "install", cfg.Tools.Files.Root, time.Minute); err != nil {
		t.Fatal(err)
	}
	runs, err := a.List(context.Background())
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %v, %v", runs, err)
	}
	var q fw.RunQueuedData
	for e, err := range ledger.Read(context.Background(), runs[0].ID, 0) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Type == fw.EvRunQueued {
			if err := e.Decode(&q); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if !q.FilesRoot || q.Agent != BootstrapProfile {
		t.Fatalf("queued = %+v", q)
	}
}

// The skills directory of a Workspace joins the skill directories after
// the directories of the process, relative to its binding when it is not
// absolute, so a local skill shadows a Workspace skill of the same name.
func TestApplyToConfigSkillsPath(t *testing.T) {
	unsetEnv(t, EnvAtespace)
	unsetEnv(t, EnvBootstrapTimeout)
	home := t.TempDir()
	ws1 := t.TempDir()
	cfg := fw.DefaultConfig()
	cfg.Home = home
	task := &Task{Metadata: ObjectMeta{Name: "reviewer", Atespace: "team-a"}}
	bindings := []Binding{
		{Ref: WorkspaceRef{Name: "one"}, Path: ws1, Workspace: &Workspace{Metadata: ObjectMeta{Name: "one"}, Spec: WorkspaceSpec{Skills: SkillsConfig{Path: ".skills"}}}},
		{Ref: WorkspaceRef{Name: "two"}, Path: "/workspace/two", Workspace: &Workspace{Metadata: ObjectMeta{Name: "two"}, Spec: WorkspaceSpec{Skills: SkillsConfig{Path: "/opt/skills"}}}},
		{Ref: WorkspaceRef{Name: "three"}, Path: "/workspace/three", Workspace: &Workspace{Metadata: ObjectMeta{Name: "three"}}},
	}
	ApplyToConfig(cfg, task, bindings)
	ApplyToConfig(cfg, task, bindings)
	want := []string{filepath.Join(home, "skills"), filepath.Join(ws1, ".skills"), "/opt/skills"}
	if got := cfg.SkillDirs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("SkillDirs = %v, want %v", got, want)
	}
	cfg.Tools.Skills = []string{"/etc/fw/skills"}
	if got := cfg.SkillDirs(); !reflect.DeepEqual(got, []string{"/etc/fw/skills", filepath.Join(ws1, ".skills"), "/opt/skills"}) {
		t.Fatalf("SkillDirs with tools.skills = %v", got)
	}
	cfg.Tools.Skills = nil

	// A skill of the home shadows a Workspace skill of the same name, and a
	// Workspace skill with a name of its own joins the list.
	write := func(dir, name, desc string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, fw.SkillFile), []byte("---\nname: "+name+"\ndescription: "+desc+"\n---\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(home, "skills"), "deploy", "local")
	write(filepath.Join(ws1, ".skills"), "deploy", "from the workspace")
	write(filepath.Join(ws1, ".skills"), "triage", "from the workspace")
	skills, err := fw.LoadSkills(cfg.SkillDirs()...)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 2 || skills[0].Name != "deploy" || skills[0].Description != "local" || skills[1].Name != "triage" {
		t.Fatalf("skills = %+v", skills)
	}
}
