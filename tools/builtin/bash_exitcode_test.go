package builtin

import (
	"context"
	"encoding/json"
	"testing"
)

// TestBashResultCarriesTheExitCode checks the exit code of a command in
// the result, beside the text, and after the shell exits.
func TestBashResultCarriesTheExitCode(t *testing.T) {
	b, _ := newTestBash(t)
	ctx := context.Background()
	for cmd, want := range map[string]int{`true`: 0, `(exit 42)`: 42, `false`: 1} {
		in, _ := json.Marshal(map[string]string{"command": cmd})
		res := call(t, ctx, b, string(in))
		if res.ExitCode == nil || *res.ExitCode != want {
			t.Fatalf("%s: exit code %v, want %d", cmd, res.ExitCode, want)
		}
	}
	res := call(t, ctx, b, `{"command":"exit 5"}`)
	if !res.IsError || res.ExitCode == nil || *res.ExitCode != 5 {
		t.Fatalf("exit 5: %+v", res)
	}
	if res := call(t, ctx, b, `{}`); res.ExitCode != nil {
		t.Fatalf("an empty command has the exit code %d", *res.ExitCode)
	}
}
