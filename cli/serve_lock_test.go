package cli

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// A server that boots while another fw process holds the writer lock of
// the ledger, such as fw run through ax ssh, waits for the lock instead of
// exiting, and serves once the other process lets the lock go.
func TestServeWaitsForTheLedgerLock(t *testing.T) {
	home := env(t, fw.WithModel(&scriptModel{}))
	t.Setenv("FW_AUTH_MODE", "none")
	l, err := fw.OpenFileLedger(filepath.Join(home, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	addr := freeAddr(t)
	stop := serveInBackground(t, "--listen", addr, "serve")
	time.Sleep(time.Second)
	if code, _ := get(t, "http://"+addr+"/readyz"); code == 200 {
		t.Fatal("the server is ready while another process holds the ledger")
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "http://"+addr+"/readyz", 200)
	if code, _, errOut := stop(); code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
}

// The wait has an end, and its error names the other holder. A verb such
// as fw run beside the holder still fails at once.
func TestServeLedgerLockWaitEnds(t *testing.T) {
	home := env(t, fw.WithModel(&scriptModel{}))
	l, err := fw.OpenFileLedger(filepath.Join(home, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	cfg := fw.DefaultConfig()
	cfg.Home = home
	cfg.Auth.Mode = "none"
	began := time.Now()
	a, err := newServeAgent(context.Background(), cfg, 700*time.Millisecond)
	if a != nil || !errors.Is(err, fw.ErrLedgerLocked) || !strings.Contains(err.Error(), "another fw process still holds the writer lock") {
		t.Fatalf("newServeAgent = %v, %v", a, err)
	}
	if waited := time.Since(began); waited < 700*time.Millisecond {
		t.Fatalf("the server gave up after %s", waited)
	}
	began = time.Now()
	code, _, errOut := run(t, "", "run", "hello")
	if code == 0 || !strings.Contains(errOut, "another writer owns this root") || time.Since(began) > 5*time.Second {
		t.Fatalf("fw run beside the holder: exit %d after %s\n%s", code, time.Since(began), errOut)
	}
}
