package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestOutcomeKeysLoad(t *testing.T) {
	cleanEnv(t)
	path := writeFile(t, filepath.Join(t.TempDir(), "fw.yaml"), `
models: {judge: anthropic/claude-haiku-5-5}
agents:
  migrator:
    outcome:
      rubric: |
        Every claim names the command that shows it.
      grader: judge
      iterations: 2
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	o := cfg.Agents["migrator"].Outcome
	if o == nil || o.Rubric != "Every claim names the command that shows it.\n" || o.Grader != "judge" || o.Iterations != 2 {
		t.Fatalf("outcome = %+v", o)
	}
	// The variables win over the file.
	t.Setenv("FW_AGENTS_MIGRATOR_OUTCOME_ITERATIONS", "3")
	if cfg, err = Load(path); err != nil {
		t.Fatal(err)
	}
	if o := cfg.Agents["migrator"].Outcome; o == nil || o.Iterations != 3 {
		t.Fatalf("outcome from the environment = %+v", o)
	}
}

func TestBadOutcomeIsNamed(t *testing.T) {
	cleanEnv(t)
	for _, body := range []string{
		"agents:\n  migrator:\n    outcome: {rubric: x, iterations: 99}\n",
		"agents:\n  migrator:\n    outcome: {iterations: 2}\n",
		"agents:\n  migrator:\n    outcome: {rubric: x, grader: nothing}\n",
	} {
		path := writeFile(t, filepath.Join(t.TempDir(), "fw.yaml"), body)
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "agents.migrator.outcome.") {
			t.Errorf("Load(%q) = %v", body, err)
		}
	}
}
