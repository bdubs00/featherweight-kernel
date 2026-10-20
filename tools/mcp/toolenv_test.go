package mcp

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// A stdio server gets the changes of a registered fw.ToolEnvHook, here the
// variable that makes the test binary serve MCP, and a hook that fails
// keeps the server from starting.
func TestStdioToolEnvHook(t *testing.T) {
	cfg, broken := fw.DefaultConfig(), fw.DefaultConfig()
	fw.RegisterToolEnv(func(_ context.Context, c *fw.Config, env []string) ([]string, error) {
		switch c {
		case cfg:
			return append(env, stdioEnv+"=1"), nil
		case broken:
			return nil, errors.New("env: the environment failed")
		}
		return env, nil
	})
	if os.Getenv(stdioEnv) != "" {
		t.Fatalf("%s is set in the test process", stdioEnv)
	}
	s := newServer(t, fw.MCPServerConfig{Name: "local", Command: os.Args[0], Args: []string{"-test.run=^$"}}, nil)
	s.conf = cfg
	if tools, err := s.Tools(context.Background()); err != nil || len(tools) != 5 {
		t.Fatalf("Tools = %d, %v", len(tools), err)
	}
	b := newServer(t, fw.MCPServerConfig{Name: "local", Command: os.Args[0], Args: []string{"-test.run=^$"}}, nil)
	b.conf = broken
	if _, err := b.Tools(context.Background()); err == nil || !strings.Contains(err.Error(), "cannot start: env: the environment failed") {
		t.Fatalf("Tools err = %v", err)
	}
}
