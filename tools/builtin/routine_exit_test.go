package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// routineBash returns an agent with the model, the real bash tool, every
// call allowed, and one profile with the routines.
func routineBash(t *testing.T, m fw.Model, routines ...fw.Routine) *fw.Agent {
	t.Helper()
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Tools.Files.Root = t.TempDir()
	cfg.Policy.Rules = fw.Rules{Default: "allow"}
	a, err := fw.New(
		fw.WithConfig(cfg),
		fw.WithoutDefaults(),
		fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()),
		fw.WithAuthn(fw.NoAuthn{}),
		fw.WithModel(m),
		fw.WithModelName("anthropic/claude-opus-5-5"),
		fw.WithTools(builtinTool(t, "bash", cfg)),
		fw.WithProfile(fw.AgentProfile{Name: "release", Routines: routines}),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(t.Context()) })
	return a
}

// A routine whose check is test -f on a missing file fails at the check,
// and so does a routine whose step is that command. The model then takes
// the run over.
func TestRoutineTestFOnAMissingFile(t *testing.T) {
	zero := 0.0
	missing := `{"command": "test -f missing.txt"}`
	for _, c := range []struct {
		name   string
		r      fw.Routine
		failed int
		prefix string
	}{
		{name: "check", failed: 2, prefix: "check 1 bash: exit code 1",
			r: fw.Routine{Name: "notes", Version: 1, Standing: fw.RoutineLive, Canary: &zero,
				Steps: []fw.RoutineStep{{Tool: "bash", Input: `{"command": "echo notes > notes.txt"}`}},
				After: []fw.RoutineCheck{{Tool: "bash", Input: missing}}, Answer: "wrote the notes"}},
		{name: "step", failed: 1, prefix: "step 1 bash: exit code 1",
			r: fw.Routine{Name: "notes", Version: 1, Standing: fw.RoutineLive, Canary: &zero,
				Steps: []fw.RoutineStep{{Tool: "bash", Input: missing}}, Answer: "found the file"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
				return endTurn("the model wrote them"), nil
			}}
			a := routineBash(t, m, c.r)
			r, err := a.Run(t.Context(), "Write the notes.", fw.AsAgent("release"))
			if err != nil || r.State != fw.StateCompleted || r.Output != "the model wrote them" {
				t.Fatalf("run = %+v, %v", r, err)
			}
			evs := ledgerOf(t, a, r.ID)
			var apps []fw.RoutineAppliedData
			for _, e := range evs {
				if e.Type == fw.EvRoutineApplied {
					var d fw.RoutineAppliedData
					if err := e.Decode(&d); err != nil {
						t.Fatal(err)
					}
					apps = append(apps, d)
				}
			}
			if len(apps) != 1 || apps[0].OK || apps[0].FailedStep != c.failed || !strings.HasPrefix(apps[0].Reason, c.prefix) {
				t.Fatalf("applied = %+v", apps)
			}
		})
	}
}
