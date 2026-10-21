//go:build linux

package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// alive reports whether the process pid runs: it exists and is not a
// zombie.
func alive(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i >= 0 && i+2 < len(s) && s[i+2] != 'Z' && s[i+2] != 'X'
}

// TestOpencodeCrashKillsItsGroup kills opencode serve alone, as a crash
// does, while a process that it started runs: the host kills that process
// too.
func TestOpencodeCrashKillsItsGroup(t *testing.T) {
	d := newDirs(t)
	f := newFakeOC(t, answerOnly)
	script := fmt.Sprintf(`sleep 600 & echo $! > %[1]s/oc-child; exec sleep 600`, d.root)
	cmd, _ := json.Marshal([]string{"sh", "-c", script, "fake-opencode"})
	cfg := testConfig(t, d, fmt.Sprintf("kind: opencode\nport: %d\ncommand: %s\n", f.port(), cmd))
	a := newAgent(t, cfg, fw.NewMemLedger())
	r, err := a.Start(testCtx(t), "hello")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, a, r.ID, terminal)
	var child int
	waitFor(t, func() bool {
		b, err := os.ReadFile(filepath.Join(d.root, "oc-child"))
		child, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		return err == nil && child > 0
	})
	t.Cleanup(func() { syscall.Kill(child, syscall.SIGKILL) })
	o := a.Runner().(*host).drv.(*opencode)
	o.mu.Lock()
	leader := o.srv.cmd.Process.Pid
	o.mu.Unlock()
	if !alive(child) {
		t.Fatal("the child of the server does not run")
	}
	if err := syscall.Kill(leader, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !alive(child) })
}
