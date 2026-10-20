package builtin

import (
	"context"
	"testing"
)

// TestBashSurvivesSetE checks that a failure under set -e ends the command
// and keeps the shell with its state, and that set -e does not reach the
// next call (scenario1 K8).
func TestBashSurvivesSetE(t *testing.T) {
	b, _ := newTestBash(t)
	ctx := context.Background()
	mustOK(t, call(t, ctx, b, `{"command":"cd / && KEPT=yes && declare -A M=([k]=v)"}`))
	got := mustOK(t, call(t, ctx, b, `{"command":"set -e; false; echo not-reached"}`))
	if got != "exit code 1" {
		t.Fatalf("set -e; false: got %q", got)
	}
	got = mustOK(t, call(t, ctx, b, `{"command":"echo alive $KEPT ${M[k]} $PWD"}`))
	if got != "alive yes v /\nexit code 0" {
		t.Fatalf("after set -e: got %q", got)
	}
	// A set -e that does not fail does not reach the next call either.
	mustOK(t, call(t, ctx, b, `{"command":"set -euo pipefail"}`))
	got = mustOK(t, call(t, ctx, b, `{"command":"false; echo $UNSET_VAR next; false | true; echo pipe $?"}`))
	if got != "next\npipe 0\nexit code 0" {
		t.Fatalf("options of an earlier call: got %q", got)
	}
	// A failure in a function and in a loop ends the command with its code.
	got = mustOK(t, call(t, ctx, b, `{"command":"set -e; f() { return 7; }; for i in 1 2; do echo i$i; f; done; echo not-reached"}`))
	if got != "i1\nexit code 7" {
		t.Fatalf("function under set -e: got %q", got)
	}
	got = mustOK(t, call(t, ctx, b, `{"command":"echo alive"}`))
	if got != "alive\nexit code 0" {
		t.Fatalf("after a function: got %q", got)
	}
	// The most common idiom: set -euo pipefail and a failure at the top
	// level, where FUNCNAME is unset. The trap must not trip over set -u.
	got = mustOK(t, call(t, ctx, b, `{"command":"set -euo pipefail; false; echo not-reached"}`))
	if got != "exit code 1" {
		t.Fatalf("set -euo pipefail; false: got %q", got)
	}
	got = mustOK(t, call(t, ctx, b, `{"command":"echo alive $KEPT"}`))
	if got != "alive yes\nexit code 0" {
		t.Fatalf("after set -euo pipefail: got %q", got)
	}
}
