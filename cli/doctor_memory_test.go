package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/ax"
	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/memory"
)

// TestDoctorMemoryScope checks the memory line of fw doctor: the count of
// the notes in the directory of the real scope, the default scope inside an
// ax Task, and the warning for the scope agent there.
func TestDoctorMemoryScope(t *testing.T) {
	t.Setenv(ax.EnvTaskYAML, "")
	home := t.TempDir()
	cfg := fw.DefaultConfig()
	cfg.Home = home
	cfg.Tools.Memory.Scope = fw.MemoryScopePrincipal
	store, err := memory.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	for _, who := range []string{"alice", "bob"} {
		scope := store.Scope(fw.RunInfo{Agent: "default", Principal: who})
		if _, err := store.Remember(ctx, scope, fw.MemoryRecord{Ref: memory.NoteRef(scope, who+".md"), Body: "x"}, ""); err != nil {
			t.Fatal(err)
		}
	}
	local := store.Scope(fw.RunInfo{})
	if _, err := store.Remember(ctx, local, fw.MemoryRecord{Ref: memory.NoteRef(local, "local.md"), Body: "x"}, ""); err != nil {
		t.Fatal(err)
	}
	line := func(scope string, inAX bool) string {
		t.Helper()
		t.Setenv(ax.EnvMetadataURL, "")
		if inAX {
			t.Setenv(ax.EnvMetadataURL, "http://127.0.0.1:1")
		}
		var out bytes.Buffer
		d := &doctor{g: &globals{stdout: &out}}
		c := *cfg
		c.Tools.Memory.Scope = scope
		d.checkMemory(&c)
		if d.failed != 0 {
			t.Fatalf("the memory check failed:\n%s", out.String())
		}
		return out.String()
	}
	if got := line(fw.MemoryScopePrincipal, false); !strings.Contains(got, "ok    memory: scope principal, index "+memory.IndexDriver+", 2 notes of 2 people in the profile default, and 1 note of its runs without a principal") {
		t.Errorf("principal line = %q", got)
	}
	if got := line("", false); !strings.Contains(got, "scope agent") || strings.Contains(got, "warn") {
		t.Errorf("default line = %q", got)
	}
	if got := line("", true); !strings.Contains(got, "scope principal") || strings.Contains(got, "warn") {
		t.Errorf("default line under ax = %q", got)
	}
	if got := line(fw.MemoryScopeAgent, true); !strings.Contains(got, "1 note of the profile default") || !strings.Contains(got, "warn  memory: tools.memory.scope is agent in an ax Task") {
		t.Errorf("agent line under ax = %q", got)
	}
}
