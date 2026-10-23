//go:build !lean || propose

package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func TestRoutineProposeVerb(t *testing.T) {
	env(t)
	dir := filepath.Join(t.TempDir(), "runs")
	l, err := fw.OpenFileLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedRun(t, l, "R1", "tracker", "Issue #7 crashes", "bug", "LABEL bug")
	seedRun(t, l, "R2", "tracker", "Issue #7 crashes", "bug", "LABEL bug")
	seedRun(t, l, "R3", "other", "Issue #8 is slow", "idea", "LABEL idea")
	_ = l.Close()
	out := filepath.Join(t.TempDir(), "proposed.yaml")
	code, stdout, errOut := run(t, "", "routine", "propose", "--agent", "tracker", "--ledger", dir, "--out", out)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(stdout, "tracker: 2 of 2 completed runs make the calls tracker.label (100%)") || !strings.Contains(stdout, "The routine is complete.") {
		t.Errorf("summary =\n%s", stdout)
	}
	// The proposal is a routine that replay checks.
	code, stdout, errOut = run(t, "", "routine", "replay", "--routine", out, "--ledger", dir, "--agent", "tracker")
	if code != 0 || !strings.Contains(stdout, "matched     2 of 2") {
		t.Fatalf("replay of the proposal: %d %s %s", code, stdout, errOut)
	}
	if code, _, errOut := run(t, "", "routine", "propose", "--agent", "nobody", "--ledger", dir); code == 0 || !strings.Contains(errOut, "no completed run of the agent nobody") {
		t.Fatalf("no runs: %d %s", code, errOut)
	}
}

func TestRoutineName(t *testing.T) {
	for in, want := range map[string]string{"Triage": "triage", "Identity Desk": "identity-desk", "a_b..c": "a-b-c", "--": "proposed", "x" + strings.Repeat("y", 70): "x" + strings.Repeat("y", 63)} {
		if got := routineName(in); got != want {
			t.Errorf("routineName(%q) = %q; want %q", in, got, want)
		}
	}
}
