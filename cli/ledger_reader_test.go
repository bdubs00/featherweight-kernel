package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func TestEventsReadsOwnedFileLedger(t *testing.T) {
	home := env(t)
	l, err := fw.OpenFileLedger(filepath.Join(home, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := l.Append(context.Background(), fw.MustEvent("R", fw.EvRunQueued, fw.RunQueuedData{Input: "live"})); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := run(t, "", "events", "R")
	if code != 0 || !strings.Contains(out, fw.EvRunQueued) {
		t.Fatalf("events: %d, %s, %s", code, out, stderr)
	}
	if _, err := l.Append(context.Background(), fw.MustEvent("R", fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted})); err != nil {
		t.Fatal(err)
	}
	code, out, stderr = run(t, "", "events", "R", "--from", "2")
	if code != 0 || !strings.Contains(out, fw.EvRunFinished) || strings.Contains(out, fw.EvRunQueued) {
		t.Fatalf("events tail: %d, %s, %s", code, out, stderr)
	}
}
