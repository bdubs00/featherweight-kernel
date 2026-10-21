package fw

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// readOnlyCalls is a tool whose read calls are safe to repeat and whose
// other calls are not.
type readOnlyCalls struct{ *FuncTool }

func (readOnlyCalls) IdempotentCall(input json.RawMessage) bool {
	return strings.Contains(string(input), `"read"`)
}

// wrapped is middleware with an Unwrap method.
type wrapped struct{ Tool }

func (w wrapped) Unwrap() Tool { return w.Tool }

// TestRerunSafe checks that the decision of a resume follows the tool for
// each call when the tool implements CallIdempotency, behind middleware
// too, and the spec of the tool otherwise.
func TestRerunSafe(t *testing.T) {
	noop := func(context.Context, json.RawMessage) (ToolResult, error) { return TextResult("ok"), nil }
	per := readOnlyCalls{NewTool(ToolSpec{Name: "per"}, noop)}
	for _, tool := range []Tool{per, wrapped{per}, wrapped{wrapped{per}}} {
		if !rerunSafe(tool, tool.Spec(), json.RawMessage(`{"op":"read"}`)) {
			t.Errorf("%T: a read call is not safe", tool)
		}
		if rerunSafe(tool, tool.Spec(), json.RawMessage(`{"op":"write"}`)) {
			t.Errorf("%T: a write call is safe", tool)
		}
	}
	// The interface decides over the flag of the spec.
	flagged := readOnlyCalls{NewTool(ToolSpec{Name: "flagged", Idempotent: true}, noop)}
	if rerunSafe(flagged, flagged.Spec(), json.RawMessage(`{"op":"write"}`)) {
		t.Error("the spec flag won over the call of the tool")
	}
	plain := NewTool(ToolSpec{Name: "plain", Idempotent: true}, noop)
	if !rerunSafe(plain, plain.Spec(), nil) {
		t.Error("an idempotent spec is not safe")
	}
	other := NewTool(ToolSpec{Name: "other"}, noop)
	if rerunSafe(other, other.Spec(), nil) {
		t.Error("a tool without a flag is safe")
	}
}

// TestWithRunPrincipal checks that the principal of a run goes into a
// context as a surface puts it there, and that a run without one leaves the
// context as it is.
func TestWithRunPrincipal(t *testing.T) {
	ctx := WithRunPrincipal(context.Background(), RunInfo{Principal: "alice", PrincipalKind: KindUser})
	if p := PrincipalFrom(ctx); p == nil || p.Subject != "alice" || p.Kind != KindUser {
		t.Fatalf("principal = %+v", p)
	}
	if p := PrincipalFrom(WithRunPrincipal(context.Background(), RunInfo{})); p != nil {
		t.Fatalf("a run without a principal gave %+v", p)
	}
	if PrincipalOfRun(RunInfo{}) != nil {
		t.Fatal("PrincipalOfRun of a run without a principal is not nil")
	}
}
