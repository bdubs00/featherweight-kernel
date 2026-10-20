//go:build linux

package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// With tools.user, the shell runs as that user with no other group and
// with its own HOME, and it cannot read the home directory of fw or the
// environment of the fw process (section 7.12). The test needs root.
func TestBashRunsAsToolUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to start the shell as another user")
	}
	ws := t.TempDir()
	for d := ws; d != filepath.Dir(d); d = filepath.Dir(d) {
		if info, err := os.Stat(d); err == nil && info.Mode().Perm()&0o005 != 0o005 {
			if err := os.Chmod(d, info.Mode().Perm()|0o005); err != nil {
				t.Fatal(err)
			}
		}
	}
	root := filepath.Join(ws, "work")
	home := filepath.Join(ws, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(root, 54321, 54321); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "fw.yaml"), []byte("secret: yes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := fw.DefaultConfig()
	cfg.Home = home
	cfg.Tools.Files.Root = root
	cfg.Tools.User = "54321:54321"
	tool, err := newBash(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b := tool.(*bashTool)
	t.Cleanup(func() { _ = b.Close() })
	ctx := context.Background()
	got := mustOK(t, call(t, ctx, b, `{"command":"id -u; id -g; id -G; echo $HOME; touch made && stat -c %u made"}`))
	if got != "54321\n54321\n54321\n/\n54321\nexit code 0" {
		t.Fatalf("got %q", got)
	}
	got = mustOK(t, call(t, ctx, b, `{"command":"cat \"$FW_HOME/fw.yaml\"; cat /proc/`+strconv.Itoa(os.Getpid())+`/environ"}`))
	if strings.Contains(got, "secret") || !strings.Contains(got, "Permission denied") || strings.HasSuffix(got, "exit code 0") {
		t.Fatalf("the shell read the state of fw: %q", got)
	}
}
