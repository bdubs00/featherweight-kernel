package ax

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestSuperviseExitCode(t *testing.T) {
	code, err := Supervise(context.Background(), []string{"sh", "-c", "exit 3"}, t.TempDir(), nil, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
}

func TestSuperviseDirAndEnv(t *testing.T) {
	dir := t.TempDir()
	code, err := Supervise(context.Background(), []string{"sh", "-c", `printf '%s' "$AX_TEST_VALUE" > out.txt`}, dir, []string{"AX_TEST_VALUE=from-extra-env"}, quietLog())
	if err != nil || code != 0 {
		t.Fatalf("Supervise = %d, %v", code, err)
	}
	if got := readFile(t, filepath.Join(dir, "out.txt")); got != "from-extra-env" {
		t.Errorf("out.txt = %q", got)
	}
}

func TestSuperviseCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	code, err := Supervise(ctx, []string{"sleep", "30"}, t.TempDir(), nil, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if code != -1 {
		t.Errorf("exit code = %d, want -1", code)
	}
	if d := time.Since(start); d > 12*time.Second {
		t.Errorf("Supervise took %v", d)
	}
}

func TestSuperviseStartError(t *testing.T) {
	code, err := Supervise(context.Background(), []string{"/does/not/exist"}, t.TempDir(), nil, quietLog())
	if err == nil || code != -1 {
		t.Errorf("Supervise = %d, %v", code, err)
	}
	if _, err := Supervise(context.Background(), nil, t.TempDir(), nil, nil); err == nil {
		t.Error("empty argv: no error")
	}
}

func TestReapZombiesReturnsWhenNotPID1(t *testing.T) {
	if os.Getpid() == 1 {
		t.Skip("the test process is PID 1")
	}
	done := make(chan struct{})
	go func() {
		ReapZombies(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ReapZombies did not return")
	}
}

func TestReaperCollectsOnlyOldZombies(t *testing.T) {
	// The child is never waited for, so it stays a zombie of this process,
	// like an orphan of PID 1.
	p, err := os.StartProcess("/bin/sh", []string{"sh", "-c", "exit 0"}, &os.ProcAttr{})
	if err != nil {
		t.Skip("cannot start /bin/sh:", err)
	}
	defer func() { _ = p.Release() }()
	self := os.Getpid()
	deadline := time.Now().Add(5 * time.Second)
	for {
		z, err := zombieChildren(self)
		if err != nil {
			t.Skip("no /proc:", err)
		}
		if slices.Contains(z, p.Pid) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not become a zombie")
		}
		time.Sleep(20 * time.Millisecond)
	}

	r := newReaper(self)
	now := time.Now()
	if got, _ := r.reap(now); slices.Contains(got, p.Pid) {
		t.Fatal("reaped a new zombie at once")
	}
	if got, _ := r.reap(now.Add(reapGrace / 2)); slices.Contains(got, p.Pid) {
		t.Fatal("reaped a zombie before the grace period")
	}
	got, err := r.reap(now.Add(reapGrace))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got, p.Pid) {
		t.Fatalf("reaped %v, want %d", got, p.Pid)
	}
	z, _ := zombieChildren(self)
	if slices.Contains(z, p.Pid) {
		t.Error("child is still a zombie")
	}
}

func TestParseStat(t *testing.T) {
	tests := []struct {
		in    string
		state byte
		ppid  int
		ok    bool
	}{
		{"42 (sleep) S 1 42 42 0 -1", 'S', 1, true},
		{"77 (a (b) c) Z 12 77 77 0", 'Z', 12, true},
		{"9 (x y) R", 0, 0, false},
		{"garbage", 0, 0, false},
	}
	for _, tt := range tests {
		state, ppid, ok := parseStat([]byte(tt.in))
		if state != tt.state || ppid != tt.ppid || ok != tt.ok {
			t.Errorf("parseStat(%q) = %c %d %v", tt.in, state, ppid, ok)
		}
	}
}
