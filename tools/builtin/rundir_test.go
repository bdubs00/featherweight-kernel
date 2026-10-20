package builtin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// perRunTools returns an agent with the real bash and files tools, a
// scripted model, tools.files.perRun on, and every call allowed.
func perRunTools(t *testing.T, s *scripted, user string) (*fw.Agent, string) {
	t.Helper()
	root := t.TempDir()
	if user != "" {
		// The tool user must reach the workspace, as it reaches /workspace.
		for d := root; d != filepath.Dir(d); d = filepath.Dir(d) {
			if info, err := os.Stat(d); err == nil && info.Mode().Perm()&0o005 != 0o005 {
				if err := os.Chmod(d, info.Mode().Perm()|0o005); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Tools.Files.Root = root
	cfg.Tools.User = user
	on := true
	cfg.Tools.Files.PerRun = &on
	cfg.Policy.Rules = fw.Rules{Default: "allow"}
	a, err := fw.New(
		fw.WithConfig(cfg),
		fw.WithoutDefaults(),
		fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()),
		fw.WithAuthn(fw.NoAuthn{}),
		fw.WithModel(s.Model()),
		fw.WithTools(builtinTool(t, "bash", cfg), builtinTool(t, "files", cfg)),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(t.Context()) })
	return a, root
}

// With tools.files.perRun, bash starts in the directory of the run and
// names it in FW_RUN_DIR, the files tool takes its paths there, and the
// next run starts in a directory of its own, without the files of the
// first run.
func TestToolsWorkInTheDirectoryOfTheRun(t *testing.T) {
	s := newScripted()
	var results []string
	keep := func(next *fw.Response) func(*fw.Request) *fw.Response {
		return func(req *fw.Request) *fw.Response {
			for _, b := range lastToolResults(t, req) {
				results = append(results, fw.TextOf(b.Content))
			}
			return next
		}
	}
	s.say("first", callTools(use("b1", "bash", `{"command":"pwd -P; echo $FW_RUN_DIR; mkdir src && echo cloned > src/README"}`)))
	s.then("first", keep(callTools(use("f1", "files", `{"command":"view","path":"src/README"}`))))
	s.then("first", keep(endTurn("done")))
	s.say("second", callTools(use("b2", "bash", `{"command":"ls; pwd -P"}`)))
	s.then("second", keep(endTurn("done")))
	a, root := perRunTools(t, s, "")
	first, err := a.Run(t.Context(), "first")
	if err != nil || first.State != fw.StateCompleted {
		t.Fatalf("first = %+v, %v", first, err)
	}
	second, err := a.Run(t.Context(), "second")
	if err != nil || second.State != fw.StateCompleted {
		t.Fatalf("second = %+v, %v", second, err)
	}
	real, _ := filepath.EvalSymlinks(root)
	dir1 := filepath.Join(real, fw.RunsDir, first.ID)
	dir2 := filepath.Join(real, fw.RunsDir, second.ID)
	if len(results) != 3 {
		t.Fatalf("results = %q", results)
	}
	if !strings.HasPrefix(results[0], dir1+"\n"+filepath.Join(root, fw.RunsDir, first.ID)+"\n") {
		t.Errorf("bash of the first run: %q", results[0])
	}
	if !strings.Contains(results[1], "cloned") {
		t.Errorf("files of the first run: %q", results[1])
	}
	if results[2] != dir2+"\nexit code 0" {
		t.Errorf("the second run starts with %q, want an empty directory", results[2])
	}
	if _, err := os.Stat(filepath.Join(dir1, "src", "README")); err != nil {
		t.Error(err)
	}
}
