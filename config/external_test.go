package config

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func TestValidateExternal(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*fw.Config)
		want []string // every problem names its key
	}{
		{"both transports", func(c *fw.Config) {
			c.Access.External = &fw.ExternalPolicyConfig{MCP: "control", Bus: "fw.authz"}
		}, []string{`access.external sets both mcp "control" and bus "fw.authz"`}},
		{"no transport", func(c *fw.Config) {
			c.Policy.External = &fw.ExternalPolicyConfig{Tool: "decide"}
		}, []string{"policy.external sets neither mcp nor bus"}},
		{"negative timeout", func(c *fw.Config) {
			c.Access.External = &fw.ExternalPolicyConfig{Bus: "fw.authz", Timeout: -time.Second}
		}, []string{"access.external.timeout -1s is negative"}},
		{"bus wildcard", func(c *fw.Config) {
			c.Policy.External = &fw.ExternalPolicyConfig{Bus: "fw.policy.>"}
		}, []string{`policy.external.bus "fw.policy.>" is not a subject`}},
		{"unknown server", func(c *fw.Config) {
			c.Access.External = &fw.ExternalPolicyConfig{MCP: "cp"}
			c.Policy.External = &fw.ExternalPolicyConfig{MCP: "cp"}
		}, []string{
			`access.external.mcp names "cp", but tools.mcp has no entry with that name`,
			`policy.external.mcp names "cp", but tools.mcp has no entry with that name`,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fw.DefaultConfig()
			tc.set(cfg)
			err := Validate(cfg)
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("Validate = %v", err)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error lacks %q:\n%v", w, err)
				}
			}
		})
	}
}

func TestValidateExternalAccepts(t *testing.T) {
	cfg := fw.DefaultConfig()
	cfg.Tools.MCP = []fw.MCPServerConfig{{Name: "control-plane", Endpoint: "https://cp.example.com/mcp"}}
	// Case and '-' against '_' do not matter, as for FW_* variables.
	cfg.Access.External = &fw.ExternalPolicyConfig{MCP: "Control_Plane", Tool: "authorize"}
	cfg.Policy.External = &fw.ExternalPolicyConfig{Bus: "fw.policy", Timeout: time.Second}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
}

// fw serve under ax adds the MCP servers of the Workspace after the load,
// so LoadBeforeMerge skips the server check and CheckControlServers runs
// after the merge.
func TestLoadBeforeMerge(t *testing.T) {
	cleanEnv(t)
	path := writeFile(t, filepath.Join(t.TempDir(), "fw.yaml"), `
access:
  external: {mcp: control}
policy:
  external: {mcp: control, tool: decide}
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), `access.external.mcp names "control"`) {
		t.Fatalf("Load = %v", err)
	}
	cfg, err := LoadBeforeMerge(path, nil)
	if err != nil {
		t.Fatalf("LoadBeforeMerge = %v", err)
	}
	if err := CheckControlServers(cfg); err == nil || !strings.Contains(err.Error(), `policy.external.mcp names "control"`) {
		t.Fatalf("CheckControlServers before the merge = %v", err)
	}
	cfg.Tools.MCP = append(cfg.Tools.MCP, fw.MCPServerConfig{Name: "control", Endpoint: "http://control.cp.svc:8080/mcp"})
	if err := CheckControlServers(cfg); err != nil {
		t.Fatalf("CheckControlServers after the merge = %v", err)
	}
	// The other checks still run.
	bad := writeFile(t, filepath.Join(t.TempDir(), "fw.yaml"), "access:\n  external: {mcp: control, bus: fw.authz}\n")
	if _, err := LoadBeforeMerge(bad, nil); err == nil || !strings.Contains(err.Error(), "access.external sets both") {
		t.Fatalf("LoadBeforeMerge with two transports = %v", err)
	}
}

func TestExternalFromTheEnvironment(t *testing.T) {
	cleanEnv(t)
	t.Setenv("FW_TOOLS_MCP_CONTROL_ENDPOINT", "https://cp.example.com/mcp")
	t.Setenv("FW_ACCESS_EXTERNAL_MCP", "control")
	t.Setenv("FW_POLICY_EXTERNAL_BUS", "fw.policy")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Access.External == nil || cfg.Access.External.Transport() != "mcp" || cfg.Policy.External == nil || cfg.Policy.External.Transport() != "bus" {
		t.Fatalf("access %+v, policy %+v", cfg.Access.External, cfg.Policy.External)
	}
	if got := cfg.ControlServers(); !got["control"] {
		t.Fatalf("control servers = %v", got)
	}
}
