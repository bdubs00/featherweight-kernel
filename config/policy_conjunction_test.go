package config

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"gopkg.in/yaml.v3"
)

func TestComposedPolicySurvivesYAMLConfiguration(t *testing.T) {
	cleanEnv(t)
	path := writeFile(t, filepath.Join(t.TempDir(), "fw.yaml"), `
agents:
  worker:
    policy:
      all:
        - default: deny
          allow: ["files: path=docs/*"]
        - ask: ["files: operation=write"]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Agents["worker"]
	encoded, err := yaml.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var restored fw.AgentProfile
	if err := yaml.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	for _, profile := range []fw.AgentProfile{p, restored} {
		if profile.Policy == nil || len(profile.Policy.All) != 2 {
			t.Fatalf("YAML dropped the independent clauses: %+v", profile.Policy)
		}
		policy := fw.NewRulesPolicy(*profile.Policy)
		for _, tc := range []struct {
			input string
			want  fw.Verdict
		}{
			{`{"path":"docs/a","operation":"read"}`, fw.VerdictAllow},
			{`{"path":"docs/a","operation":"write"}`, fw.VerdictAsk},
			{`{"path":"secrets/a","operation":"write"}`, fw.VerdictDeny},
		} {
			got := policy.Decide(context.Background(), fw.ToolCall{Spec: fw.ToolSpec{Name: "files"}, Input: json.RawMessage(tc.input)})
			if got.Verdict != tc.want {
				t.Fatalf("policy after YAML for %s = %+v, want %s", tc.input, got, tc.want)
			}
		}
	}
}
