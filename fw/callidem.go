package fw

import "encoding/json"

// CallIdempotency is a tool that tells, for each call, whether the call is
// safe to run again. ToolSpec.Idempotent classifies every call of a tool
// together, so a tool whose commands differ, such as the memory tool, where
// view is safe and insert is not, implements this interface. When a
// restart cuts a call before its result, the kernel asks the tool through
// IdempotentCall in place of ToolSpec.Idempotent: a call that is safe runs
// again, and any other call waits for an approval with the reason "rerun
// after resume" (section 4.5). The kernel finds the interface behind
// middleware that has an Unwrap method.
type CallIdempotency interface {
	// IdempotentCall reports whether the call with this input is safe to
	// run again after it ran once without a recorded result. An input that
	// the tool cannot read is not safe.
	IdempotentCall(input json.RawMessage) bool
}

// callIdempotencyOf returns the CallIdempotency of a tool, or of a tool
// behind its middleware.
func callIdempotencyOf(t Tool) (CallIdempotency, bool) {
	for depth := 0; t != nil && depth < 16; depth++ {
		if c, ok := t.(CallIdempotency); ok {
			return c, true
		}
		t = unwrapTool(t)
	}
	return nil, false
}
