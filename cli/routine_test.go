package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// seedRun appends a run of the agent to a file ledger: a label call with
// its result, then the answer.
func seedRun(t *testing.T, l fw.Ledger, id, agent, input, label, output string) {
	t.Helper()
	evs := []fw.Event{
		fw.MustEvent(id, fw.EvRunQueued, fw.RunQueuedData{Agent: agent, Input: input}),
		fw.MustEvent(id, fw.EvRunStarted, fw.RunStartedData{}),
		fw.MustEvent(id, fw.EvToolRequested, fw.ToolRequestedData{Tool: "tracker.label", CallID: "c1", Input: json.RawMessage(`{"label":"` + label + `","issue":7}`)}),
		fw.MustEvent(id, fw.EvToolResponded, fw.ToolRespondedData{Tool: "tracker.label", CallID: "c1", Content: []fw.Block{fw.Text("ok")}}),
		fw.MustEvent(id, fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Output: output}),
	}
	for _, e := range evs {
		if _, err := l.Append(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
}

const labelRoutine = `name: label
version: 1
standing: shadow
decider: clef
questions:
  kind:
    type: choice
    criteria: {bug: Broken, idea: Wanted}
    act: 0.9
    observe: {answer: 'LABEL (\w+)'}
steps:
  - tool: tracker.label
    input: '{"issue": {{ find .input "#(\\d+)" }}, "label": {{ json .kind }}}'
answer: 'LABEL {{ .kind }}'
`

func TestRoutineReplayVerb(t *testing.T) {
	env(t)
	dir := filepath.Join(t.TempDir(), "runs")
	l, err := fw.OpenFileLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedRun(t, l, "R1", "tracker", "Issue #7 crashes", "bug", "LABEL bug")
	seedRun(t, l, "R2", "tracker", "Issue #7 crashes", "idea", "LABEL bug")
	seedRun(t, l, "R3", "other", "Issue #7 crashes", "bug", "LABEL bug")
	_ = l.Close()
	file := filepath.Join(t.TempDir(), "r.yaml")
	if err := os.WriteFile(file, []byte(labelRoutine), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := run(t, "", "routine", "replay", "--routine", file, "--ledger", dir, "--agent", "tracker")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{"label, version 1: 2 runs", "matched     1", "diverged    1", "R2  step 1 tracker.label: the input differs",
		`routine: {"issue":7,"label":"bug"}`, `agent:   {"issue":7,"label":"idea"}`} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	code, out, errOut = run(t, "", "routine", "replay", "--routine", file, "--ledger", dir, "--json")
	var res struct {
		Runs    int      `json:"runs"`
		Matched []string `json:"matched"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &res) != nil || res.Runs != 3 || len(res.Matched) != 2 {
		t.Fatalf("json: %d %s %s", code, out, errOut)
	}
	// A routine that does not load is an error that names the problem.
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	_ = os.WriteFile(bad, []byte(strings.Replace(labelRoutine, "version: 1", "version: 0", 1)), 0o600)
	if code, _, errOut := run(t, "", "routine", "replay", "--routine", bad, "--ledger", dir); code == 0 || !strings.Contains(errOut, "version 0 is not a positive integer") {
		t.Fatalf("bad routine: %d %s", code, errOut)
	}
	if code, _, _ := run(t, "", "routine", "replay"); code != exitUsage {
		t.Fatalf("no routine: exit %d", code)
	}
}

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
	if code != 0 || !strings.Contains(stdout, "matched     2") {
		t.Fatalf("replay of the proposal: %d %s %s", code, stdout, errOut)
	}
	if code, _, errOut := run(t, "", "routine", "propose", "--agent", "nobody", "--ledger", dir); code == 0 || !strings.Contains(errOut, "no completed run of the agent nobody") {
		t.Fatalf("no runs: %d %s", code, errOut)
	}
}
