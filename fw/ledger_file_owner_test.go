//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package fw

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestFileLedgerOwnerLifetime(t *testing.T) {
	ctx := testCtx(t)
	root := filepath.Join(t.TempDir(), "runs")
	l, err := OpenFileLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	if _, err := l.Append(ctx, MustEvent("R", EvRunQueued, RunQueuedData{Input: "one"})); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, alias, filepath.Join(root, ".")} {
		other, err := OpenFileLedger(path)
		if other != nil {
			_ = other.Close()
		}
		if !errors.Is(err, ErrLedgerLocked) {
			t.Fatalf("competing owner of %s = %v", path, err)
		}
	}
	if err := l.Flush(); err != nil {
		t.Fatal(err)
	}
	if other, err := OpenFileLedger(root); !errors.Is(err, ErrLedgerLocked) {
		if other != nil {
			_ = other.Close()
		}
		t.Fatalf("Flush released ownership: %v", err)
	}
	watch, err := l.Watch(ctx, "R", 2)
	if err != nil {
		t.Fatal(err)
	}
	// A batched event still reaches stable storage when Close releases ownership.
	if _, err := l.Append(ctx, MustEvent("R", EvTurnStarted, TurnData{Turn: 1})); err != nil {
		t.Fatal(err)
	}
	before := l.syncs.Load()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if l.syncs.Load() <= before {
		t.Fatal("Close did not flush the batched event")
	}
	for {
		select {
		case _, ok := <-watch:
			if !ok {
				goto closed
			}
		case <-ctx.Done():
			t.Fatal("Close left the watch open")
		}
	}
closed:
	if _, err := l.Append(ctx, MustEvent("R", EvRunQueued, nil)); !errors.Is(err, ErrLedgerClosed) {
		t.Fatalf("append after Close: %v", err)
	}
	if _, err := l.Watch(ctx, "R", 0); !errors.Is(err, ErrLedgerClosed) {
		t.Fatalf("watch after Close: %v", err)
	}
	if err := l.SaveEgressCursor(ctx, "route", "R", 1); !errors.Is(err, ErrLedgerClosed) {
		t.Fatalf("cursor after Close: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("repeated Close: %v", err)
	}
	next, err := OpenFileLedger(alias)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if seq, err := next.Append(ctx, MustEvent("R", EvTurnFinished, TurnData{Turn: 1})); err != nil || seq != 3 {
		t.Fatalf("new owner append = %d, %v", seq, err)
	}
}

// This helper runs in another process so ownership does not depend on Go locks.
func TestFileLedgerOwnerProcessHelper(t *testing.T) {
	root := os.Getenv("FW_TEST_LEDGER_OWNER_ROOT")
	if root == "" {
		return
	}
	l, err := OpenFileLedger(root)
	if os.Getenv("FW_TEST_LEDGER_EXPECT_LOCKED") == "1" {
		if l != nil {
			_ = l.Close()
		}
		if !errors.Is(err, ErrLedgerLocked) {
			t.Fatalf("competing process acquired the writer: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := l.Append(context.Background(), MustEvent("R", EvRunQueued, RunQueuedData{Input: "child"})); err != nil {
		t.Fatal(err)
	}
	fmt.Println("owner-ready")
	// The parent kills this process without Close to exercise OS lock release.
	time.Sleep(time.Minute)
	t.Fatal("parent did not kill the owner")
}

func TestFileLedgerOwnerCrossProcess(t *testing.T) {
	ctx, cancel := context.WithTimeout(testCtx(t), 15*time.Second)
	defer cancel()
	root := filepath.Join(t.TempDir(), "runs")
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFileLedgerOwnerProcessHelper$")
	cmd.Env = append(os.Environ(), "FW_TEST_LEDGER_OWNER_ROOT="+root)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "owner-ready\n" {
		t.Fatalf("child ready = %q, %v", line, err)
	}
	other := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFileLedgerOwnerProcessHelper$")
	other.Env = append(os.Environ(), "FW_TEST_LEDGER_OWNER_ROOT="+root, "FW_TEST_LEDGER_EXPECT_LOCKED=1")
	if out, err := other.CombinedOutput(); err != nil {
		t.Fatalf("competing process: %v\n%s", err, out)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	l, err := OpenFileLedger(root)
	if err != nil {
		t.Fatalf("dead process retained ownership: %v", err)
	}
	defer l.Close()
	if seq, err := l.Append(ctx, MustEvent("R", EvRunStarted, nil)); err != nil || seq != 2 {
		t.Fatalf("new process sequence = %d, %v", seq, err)
	}
}

func TestFileLedgerSetupFailureReleasesOwnership(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runs")
	_, err := openFileLedger(root, func(string) error { return errors.New("setup sync failed") })
	if err == nil {
		t.Fatal("setup accepted failed sync")
	}
	l, err := OpenFileLedger(root)
	if err != nil {
		t.Fatalf("failed setup retained writer: %v", err)
	}
	defer l.Close()
}

func TestFileLedgerReadOnlyLiveTail(t *testing.T) {
	ctx := testCtx(t)
	root := t.TempDir()
	l, err := OpenFileLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := l.Append(ctx, MustEvent("R", EvRunQueued, nil)); err != nil {
		t.Fatal(err)
	}
	r, err := OpenFileLedgerReadOnly(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for want := 1; want <= 2; want++ {
		events, err := readAll(ctx, r, "R")
		if err != nil || len(events) != want {
			t.Fatalf("live read = %v, %v; want %d events", events, err, want)
		}
		if want == 1 {
			if _, err := l.Append(ctx, MustEvent("R", EvRunStarted, nil)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := r.Append(ctx, MustEvent("R", EvRunFinished, nil)); !errors.Is(err, ErrLedgerReadOnly) {
		t.Fatalf("read-only Append = %v", err)
	}
	if _, err := r.Watch(ctx, "R", 0); !errors.Is(err, ErrLedgerReadOnly) {
		t.Fatalf("read-only Watch = %v", err)
	}
	if err := r.SaveEgressCursor(ctx, "route", "R", 1); !errors.Is(err, ErrLedgerReadOnly) {
		t.Fatalf("read-only cursor = %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "R", ledgerFileName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(`{"seq":3`)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	events, err := readAll(ctx, r, "R")
	if err != nil || len(events) != 2 {
		t.Fatalf("partial tail read = %v, %v", events, err)
	}
	after, _ := os.Stat(path)
	if before.Size() != after.Size() {
		t.Fatal("read-only read repaired the writer tail")
	}
	missing := filepath.Join(root, "missing")
	if _, err := OpenFileLedgerReadOnly(missing); err == nil {
		t.Fatal("read-only open created a root")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only missing root = %v", err)
	}
}
