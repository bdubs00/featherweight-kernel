//go:build lean && !propose

package cli

import (
	"strings"
	"testing"
)

// A lean build leaves out fw routine propose, says so, and keeps replay.
func TestRoutineProposeLeftOut(t *testing.T) {
	env(t)
	code, _, errOut := run(t, "", "routine", "propose", "--agent", "tracker", "--ledger", t.TempDir())
	if code == 0 || !strings.Contains(errOut, "build fw without the tag lean, or with the tag propose") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	code, _, errOut = run(t, "", "routine", "help")
	if code != exitOK || strings.Contains(errOut, "propose") || !strings.Contains(errOut, "fw routine replay") {
		t.Fatalf("help: exit %d: %s", code, errOut)
	}
}
