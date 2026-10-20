package cli

import (
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// fw env dockerfile needs --from, says when the package env is missing,
// and prints what the package writes.
func TestEnvDockerfile(t *testing.T) {
	t.Chdir(t.TempDir())
	if code, _, _ := run(t, "", "env", "dockerfile"); code != exitUsage {
		t.Fatalf("no --from: exit %d", code)
	}
	saved := EnvDockerfile
	t.Cleanup(func() { EnvDockerfile = saved })
	EnvDockerfile = nil
	if code, _, errOut := run(t, "", "env", "dockerfile", "--from", "debian"); code != exitFailed || !strings.Contains(errOut, "not linked into this binary") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	EnvDockerfile = func(cfg *fw.Config, from string, w io.Writer) error {
		_, err := fmt.Fprintf(w, "FROM %s\n", from)
		return err
	}
	if code, out, errOut := run(t, "", "env", "dockerfile", "--from", "debian:13"); code != exitOK || out != "FROM debian:13\n" {
		t.Fatalf("exit %d: %q %s", code, out, errOut)
	}
}
