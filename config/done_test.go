package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDoneKeysLoad(t *testing.T) {
	cleanEnv(t)
	path := writeFile(t, filepath.Join(t.TempDir(), "fw.yaml"), `
agents:
  fixer:
    done:
      checks:
        - name: the tests pass
          tool: bash
          input: '{"command": "go test ./..."}'
        - tool: bash
          input: '{"command": "git status --porcelain"}'
          lacks: ["??"]
      attempts: 2
      timeout: 3m
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.Agents["fixer"].Done
	if d == nil || len(d.Checks) != 2 || d.Checks[0].Name != "the tests pass" || d.Checks[1].Lacks[0] != "??" ||
		d.Attempts != 2 || d.Timeout != 3*time.Minute {
		t.Fatalf("done = %+v", d)
	}
	// The variables win over the file.
	t.Setenv("FW_AGENTS_FIXER_DONE_ATTEMPTS", "4")
	if cfg, err = Load(path); err != nil {
		t.Fatal(err)
	}
	if d := cfg.Agents["fixer"].Done; d == nil || d.Attempts != 4 {
		t.Fatalf("done from the environment = %+v", d)
	}
}

func TestBadDoneIsNamed(t *testing.T) {
	cleanEnv(t)
	for _, body := range []string{
		"agents:\n  fixer:\n    done: {attempts: 2}\n",
		"agents:\n  fixer:\n    done: {checks: [{tool: bash}], attempts: 99}\n",
		"agents:\n  fixer:\n    done: {checks: [{tool: bash, input: 'go test'}]}\n",
	} {
		path := writeFile(t, filepath.Join(t.TempDir(), "fw.yaml"), body)
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "agents.fixer.done.") {
			t.Errorf("Load(%q) = %v", body, err)
		}
	}
}
