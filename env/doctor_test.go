//go:build unix

package env

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// The doctor line names the state, the managers on PATH, and the hosts of
// the lock files.
func TestDoctor(t *testing.T) {
	ctx := context.Background()
	if ok, line := doctor(ctx, fw.DefaultConfig(), nil, time.Second); !ok || line != "environment: none (no block environment)" {
		t.Fatalf("no block: %v %q", ok, line)
	}
	f := newFixture(t, fullBlock)
	f.write("uv.lock", "version = 1\npackage pyyaml 6.0.2\nsource = { registry = \"https://pypi.org/simple\" }\nwheel = \"https://files.pythonhosted.org/p/pyyaml.whl\"\nmirror = \"http://deb.debian.org/debian\"\n")
	ok, line := doctor(ctx, f.cfg, nil, time.Second)
	for _, want := range []string{"none installed for", "uv 0.0.0-fake on PATH", "pixi 0.0.0-fake on PATH", "tls conda.anaconda.org", "tls files.pythonhosted.org", "tls pypi.org", "http deb.debian.org"} {
		if !strings.Contains(line, want) {
			t.Errorf("line lacks %q: %s", want, line)
		}
	}
	if !ok {
		t.Errorf("not ok: %s", line)
	}
	if _, err := fw.ExtendToolEnv(f.runCtx(), f.cfg, nil); err != nil {
		t.Fatal(err)
	}
	if ok, line := doctor(ctx, f.cfg, nil, time.Second); !ok || !strings.Contains(line, "environment: ready sha256:") {
		t.Fatalf("ready: %v %q", ok, line)
	}
	f.write("fail", "")
	f.write("uv.lock", "version = 1\npackage pyyaml 6.0.3\n")
	if _, err := fw.ExtendToolEnv(f.runCtx(), f.cfg, nil); err == nil {
		t.Fatal("no failure")
	}
	if ok, line := doctor(ctx, f.cfg, nil, time.Second); ok || !strings.Contains(line, "failed at the step python: uv sync --frozen: exit status 2 (attempt 1, next try in") {
		t.Fatalf("failed: %v %q", ok, line)
	}
	t.Setenv("PATH", filepath.Join(t.TempDir(), "empty"))
	if ok, line := doctor(ctx, f.cfg, nil, time.Second); ok || !strings.Contains(line, "uv is not on PATH; the image must carry it") {
		t.Fatalf("no uv: %v %q", ok, line)
	}
	bad := newFixture(t, "pyhton: x")
	if ok, line := doctor(ctx, bad.cfg, nil, time.Second); ok || !strings.Contains(line, "is not a key") {
		t.Fatalf("bad: %v %q", ok, line)
	}
}
