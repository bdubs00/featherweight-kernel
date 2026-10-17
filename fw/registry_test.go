package fw

import (
	"slices"
	"sync/atomic"
	"testing"
)

// TestRegisterDefaults checks that DefaultConfig applies a registered
// change after the kernel defaults, on every call. The change here only
// counts, so the other tests of the package see the same defaults.
func TestRegisterDefaults(t *testing.T) {
	var calls atomic.Int32
	RegisterDefaults(func(c *Config) {
		if c.MaxRuns == 16 && slices.Contains(c.Surfaces, "native") {
			calls.Add(1)
		}
	})
	DefaultConfig()
	DefaultConfig()
	if n := calls.Load(); n != 2 {
		t.Fatalf("the change ran %d times with the kernel defaults in place, want 2", n)
	}
	c := DefaultConfig()
	if slices.Contains(c.Surfaces, "mcp") || slices.Contains(c.Tools.Builtin, "propose_agent") {
		t.Fatalf("kernel defaults: surfaces %v, built-in tools %v", c.Surfaces, c.Tools.Builtin)
	}
}
