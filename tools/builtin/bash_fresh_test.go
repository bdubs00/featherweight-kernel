package builtin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// A call with fw.RunInfo.Fresh runs in a new shell in the directory of the
// run, and the shell of the run keeps its directory and its variables.
func TestBashFreshShell(t *testing.T) {
	b, root := newTestBash(t)
	real, _ := filepath.EvalSymlinks(root)
	run := fw.WithRunInfo(context.Background(), fw.RunInfo{RunID: "r1", Dir: real})
	fresh := fw.WithRunInfo(context.Background(), fw.RunInfo{RunID: "r1", Dir: real, Fresh: true})
	mustOK(t, call(t, run, b, `{"command":"mkdir -p sub && cd sub && export FW_STATE=model"}`))
	if got := mustOK(t, call(t, fresh, b, `{"command":"pwd -P; echo ${FW_STATE:-fresh}"}`)); got != real+"\nfresh\nexit code 0" {
		t.Fatalf("fresh shell: got %q", got)
	}
	if got := mustOK(t, call(t, run, b, `{"command":"pwd -P; echo $FW_STATE"}`)); got != filepath.Join(real, "sub")+"\nmodel\nexit code 0" {
		t.Fatalf("the shell of the run: got %q", got)
	}
	if n := b.open(); n != 1 {
		t.Fatalf("sessions = %d, want the one of the run", n)
	}
	// A fresh call reports its exit code as any call does.
	res := call(t, fresh, b, `{"command":"exit 3"}`)
	if res.ExitCode == nil || *res.ExitCode != 3 {
		t.Fatalf("exit code = %v, result %+v", res.ExitCode, res)
	}
	if n := b.open(); n != 1 {
		t.Fatalf("sessions after a fresh exit = %d", n)
	}
}

// A check of a definition of done runs in a fresh shell in the directory
// of the run, so a cd or a variable that the model left in the shell of
// the run does not reach it.
func TestDoneCheckRunsInAFreshShell(t *testing.T) {
	s := newScripted()
	s.say("Tidy the workspace.", callTools(use("b1", "bash", `{"command":"mkdir sub && cd sub && export FW_STATE=model"}`)), endTurn("tidy"))
	root := t.TempDir()
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Tools.Files.Root = root
	on := true
	cfg.Tools.Files.PerRun = &on
	cfg.Policy.Rules = fw.Rules{Default: "allow"}
	check := fw.DoneCheck{Name: "a fresh shell", Tool: "bash", Input: `{"command":"pwd -P; test -d sub; echo ${FW_STATE:-fresh}"}`,
		Contains: []string{"fresh"}}
	a, err := fw.New(
		fw.WithConfig(cfg),
		fw.WithoutDefaults(),
		fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()),
		fw.WithAuthn(fw.NoAuthn{}),
		fw.WithModel(s.Model()),
		fw.WithModelName("anthropic/claude-opus-5-5"),
		fw.WithTools(builtinTool(t, "bash", cfg)),
		fw.WithProfile(fw.AgentProfile{Name: "fixer", Done: &fw.Done{Checks: []fw.DoneCheck{check}, Attempts: 1}}),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(t.Context()) })
	r, err := a.Run(t.Context(), "Tidy the workspace.", fw.AsAgent("fixer"))
	if err != nil || r.State != fw.StateCompleted || r.Done == nil || r.Done.State != fw.DoneChecked {
		t.Fatalf("run = %+v, done %+v, %v", r, r.Done, err)
	}
	// The check started in the directory of the run, where the model made
	// sub, and not in sub.
	real, _ := filepath.EvalSymlinks(root)
	if _, err := os.Stat(filepath.Join(real, fw.RunsDir, r.ID, "sub")); err != nil {
		t.Fatal(err)
	}
}
