package fw

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileLedgerEgressCursorDurabilityAndEnumeration(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	l, err := OpenFileLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	ev := appendReplayEvent(t, l, "run", EvRunStarted) // not normally fsynced
	route := "../../outside/route"
	if err := l.SaveEgressCursor(ctx, route, "run", ev.Seq+1); err == nil {
		t.Fatal("accepted cursor beyond ledger")
	}
	l.failFileSyncs.Store(1)
	if err := l.SaveEgressCursor(ctx, route, "run", ev.Seq); err == nil {
		t.Fatal("accepted cursor without durable ledger prefix")
	}
	if seq, err := l.LoadEgressCursor(ctx, route, "run"); err != nil || seq != 0 {
		t.Fatalf("cursor after failed ledger fsync = %d, %v", seq, err)
	}
	if err := l.SaveEgressCursor(ctx, route, "run", ev.Seq); err != nil {
		t.Fatal(err)
	}
	if seq, err := l.LoadEgressCursor(ctx, "other-route", "run"); err != nil || seq != 0 {
		t.Fatalf("route isolation = %d, %v", seq, err)
	}
	if _, err := l.LoadEgressCursor(ctx, route, "../outside"); err == nil {
		t.Fatal("accepted unsafe run id")
	}
	if err := l.SaveEgressCursor(ctx, route, "../outside", 0); err == nil {
		t.Fatal("saved unsafe run id")
	}
	path := l.egressCursorPath(route, "run")
	if !strings.HasPrefix(path, filepath.Join(dir, ".egress")+string(os.PathSeparator)) {
		t.Fatalf("cursor escaped sidecar root: %q", path)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l2, err := OpenFileLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	if seq, err := l2.LoadEgressCursor(ctx, route, "run"); err != nil || seq != ev.Seq {
		t.Fatalf("reopened cursor = %d, %v", seq, err)
	}
	var ids []string
	for id, err := range l2.Runs(ctx) {
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if len(ids) != 1 || ids[0] != "run" {
		t.Fatalf("sidecars became run ledgers: %v", ids)
	}
}

func TestFileLedgerEgressCursorSyncFailuresRetry(t *testing.T) {
	for _, stage := range []string{"cursor-file", "cursor-rename"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			l, err := OpenFileLedger(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ev := appendReplayEvent(t, l, "run", EvRunFinished)
			route := "route"
			cursorDir := filepath.Dir(l.egressCursorPath(route, "run"))
			visits := 0
			l.beforeDirSync = func(path string) error {
				if path != cursorDir {
					return nil
				}
				visits++
				if stage == "cursor-file" && visits == 1 {
					// The next file fsync is the temp cursor, after the
					// run log and the sidecar directory chain were synced.
					l.failFileSyncs.Store(1)
				}
				if stage == "cursor-rename" && (visits == 2 || visits == 3) {
					// Fail after rename, then fail the first Load retry.
					return errors.New("cursor directory sync failed")
				}
				return nil
			}
			if err := l.SaveEgressCursor(ctx, route, "run", ev.Seq); err == nil {
				t.Fatal("cursor save ignored sync failure")
			}
			seq, err := l.LoadEgressCursor(ctx, route, "run")
			if stage == "cursor-rename" {
				if err == nil {
					t.Fatal("Load trusted visible cursor without paying directory sync debt")
				}
			} else if err != nil || seq != 0 {
				t.Fatalf("failed cursor file sync changed progress: %d, %v", seq, err)
			}
			l.beforeDirSync = nil
			if err := l.SaveEgressCursor(ctx, route, "run", ev.Seq); err != nil {
				t.Fatal(err)
			}
			if seq, err := l.LoadEgressCursor(ctx, route, "run"); err != nil || seq != ev.Seq {
				t.Fatalf("retried cursor = %d, %v", seq, err)
			}
			entries, err := os.ReadDir(cursorDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || strings.HasPrefix(entries[0].Name(), ".cursor-") {
				t.Fatalf("temporary sidecar leak: %v", entries)
			}
		})
	}
}

func TestFileLedgerEgressCursorBeyondRecoveredLogResets(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	l, err := OpenFileLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	first := appendReplayEvent(t, l, "run", EvRunStarted)
	second := appendReplayEvent(t, l, "run", EvRunFinished)
	if err := l.SaveEgressCursor(ctx, "route", "run", second.Seq); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	// Model a recovered/restored ledger prefix and an incomplete trailing line.
	data, err := os.ReadFile(l.ledgerPath("run"))
	if err != nil {
		t.Fatal(err)
	}
	end := strings.IndexByte(string(data), '\n') + 1
	data = append(data[:end], []byte(`{"seq":2`)...)
	if err := os.WriteFile(l.ledgerPath("run"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	l2, err := OpenFileLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	if seq, err := l2.LoadEgressCursor(ctx, "route", "run"); err != nil || seq != 0 {
		t.Fatalf("cursor beyond recovered prefix = %d, %v", seq, err)
	}
	newFinal := appendReplayEvent(t, l2, "run", EvRunFinished)
	if newFinal.Seq != first.Seq+1 {
		t.Fatalf("recovered seq = %d", newFinal.Seq)
	}
	bus := &replayBus{Bus: NewMemBus()}
	e := testReplay(t, l2, bus)
	e.route = "route"
	if _, err := e.replayRun(ctx, "run"); err != nil {
		t.Fatal(err)
	}
	got := bus.snapshot()
	if len(got) != 2 || got[0].ID != first.ID || got[1].ID != newFinal.ID {
		t.Fatalf("recovery skipped reused sequence: %+v", got)
	}
}

func TestBusEgressCursorFailureReplaysConfirmedPublication(t *testing.T) {
	l, err := OpenFileLedger(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ev := appendReplayEvent(t, l, "run", EvRunFinished)
	bus := &replayBus{Bus: NewMemBus()}
	e := testReplay(t, l, bus)
	l.failFileSyncs.Store(1)
	if _, err := e.replayRun(context.Background(), "run"); err == nil {
		t.Fatal("expected checkpoint failure")
	}
	if _, err := e.replayRun(context.Background(), "run"); err != nil {
		t.Fatal(err)
	}
	got := bus.snapshot()
	if len(got) != 2 || got[0].ID != ev.ID || got[1].ID != ev.ID {
		t.Fatalf("checkpoint failure did not replay stable id: %+v", got)
	}
}

func TestFileLedgerEgressCursorRejectsReusedSequence(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	l, err := OpenFileLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	appendReplayEvent(t, l, "run", EvRunStarted)
	final := appendReplayEvent(t, l, "run", EvRunFinished)
	if err := l.SaveEgressCursor(ctx, "route", "run", final.Seq); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(l.ledgerPath("run"))
	if err != nil {
		t.Fatal(err)
	}
	end := strings.IndexByte(string(data), '\n') + 1
	if err := os.WriteFile(l.ledgerPath("run"), data[:end], 0o600); err != nil {
		t.Fatal(err)
	}
	l2, err := OpenFileLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Recovery/resume can append before the egress worker loads its cursor.
	replacement := appendReplayEvent(t, l2, "run", EvRunFinished)
	if replacement.Seq != final.Seq || replacement.ID == final.ID {
		t.Fatal("test did not reuse sequence with a different event")
	}
	if seq, err := l2.LoadEgressCursor(ctx, "route", "run"); err != nil || seq != 0 {
		t.Fatalf("cursor skipped reused seq: %d, %v", seq, err)
	}
	if seq, err := l2.LoadEgressCursor(ctx, "route", "run"); err != nil || seq != 0 {
		t.Fatalf("stale cursor reset did not persist: %d, %v", seq, err)
	}
}
