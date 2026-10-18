package fw

// Tests for the argument-aware matching of replay. Every replay answers a
// tool call only from a recorded result of the same tool with the same
// canonical input. The tests inspect the results that the model sees and
// check that no live tool runs.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
)

// replayReader returns a read_files tool that answers from a table and
// counts every live call in calls.
func replayReader(files map[string]string, calls *atomic.Int32) Tool {
	return NewTool(ToolSpec{Name: "read_files", ReadOnly: true, ParallelSafe: true}, func(_ context.Context, input json.RawMessage) (ToolResult, error) {
		if calls != nil {
			calls.Add(1)
		}
		var in struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return ToolResult{}, fmt.Errorf("replay test: %w", err)
		}
		return TextResult(files[in.Path]), nil
	})
}

// replayRecorder runs a recorded run whose script calls a tool. calls holds
// one tool_use block per call.
func replayRecorder(t *testing.T, ctx context.Context, tool Tool, calls ...Block) (*Agent, *Run) {
	t.Helper()
	a := newTestAgent(t, newScript(callTools(calls...), endTurn("done")).Model(), WithTools(tool))
	r, err := a.Run(ctx, "gather the files")
	mustState(t, r, err, StateCompleted)
	return a, r
}

func TestCanonicalJSON(t *testing.T) {
	same := [][2]string{
		{"", `{}`},
		{`{}`, `  {} `},
		{`{"a":1,"b":2}`, `{"b":2, "a":1}`},
		{`{ "path" : "a.txt" }`, `{"path":"a.txt"}`},
		{"\n\t{\"a\": 9007199254740993}\n", `{"a":9007199254740993}`},
		{`{"a":1,"deep":{"b":[true,false,null,"<x>"]}}`, `{"deep":{"b":[true,false,null,"\u003cx\u003e"]},"a":1}`},
	}
	for _, c := range same {
		if canonicalJSON([]byte(c[0])) != canonicalJSON([]byte(c[1])) {
			t.Errorf("canonicalJSON(%q) != canonicalJSON(%q)", c[0], c[1])
		}
	}
	diff := [][2]string{
		{`{"a":9007199254740993}`, `{"a":9007199254740992}`},
		{`{"n":18446744073709551616}`, `{"n":18446744073709551617}`},
		{`[1,2]`, `[2,1]`},
		{`1.0`, `1`},
		{`{"a":"1"}`, `{"a":1}`},
		{`{oops`, `{"a":1}`},
		{`{oops`, `oops`},
	}
	for _, c := range diff {
		if canonicalJSON([]byte(c[0])) == canonicalJSON([]byte(c[1])) {
			t.Errorf("canonicalJSON(%q) == canonicalJSON(%q)", c[0], c[1])
		}
	}
	if s := canonicalJSON([]byte("{oops")); !strings.HasPrefix(s, "\x00") || !strings.Contains(s, "oops") {
		t.Errorf("invalid input canonicalized to %q", s)
	}
	if s := canonicalJSON([]byte(`{"a":1}`)); strings.HasPrefix(s, "\x00") {
		t.Errorf("valid input carries the invalid marker: %q", s)
	}
}

// TestReplayServesOnlyTheMatchingInput records one run that reads two files,
// and replays it with the same calls in another order plus one call that no
// recorded run made. Each call receives the result of its own input, the
// mismatched call receives an explicit divergence result, and the live tool
// of the replay agent never runs.
func TestReplayServesOnlyTheMatchingInput(t *testing.T) {
	ctx := testCtx(t)
	rec, orig := replayRecorder(t, ctx,
		replayReader(map[string]string{"a.txt": "AAA", "b.txt": "BBB"}, nil),
		use("c1", "read_files", `{"path":"a.txt"}`),
		use("c2", "read_files", `{"path":"b.txt"}`),
	)
	var live atomic.Int32
	s := newScript(callTools(
		use("x1", "read_files", `{"path":"b.txt"}`),
		use("x2", "read_files", `{"path":"a.txt"}`),
		use("x3", "read_files", `{"path":"c.txt"}`),
	), endTurn("replayed"))
	b := newTestAgent(t, s.Model(), WithTools(replayReader(map[string]string{"a.txt": "live A"}, &live)))
	r, err := Replay(ctx, b, rec.Ledger(), orig.ID)
	mustState(t, r, err, StateCompleted)
	if live.Load() != 0 {
		t.Fatalf("replay ran the live tool %d times", live.Load())
	}
	res := lastToolResults(t, s.requests()[1])
	if TextOf(res[0].Content) != "BBB" || res[0].IsError {
		t.Fatalf("result of b.txt = %+v", res[0])
	}
	if TextOf(res[1].Content) != "AAA" || res[1].IsError {
		t.Fatalf("result of a.txt = %+v", res[1])
	}
	if !res[2].IsError {
		t.Fatalf("result of c.txt is not an error: %+v", res[2])
	}
	div := TextOf(res[2].Content)
	if !strings.Contains(div, "no recorded result is left for tool read_files") ||
		!strings.Contains(div, `input {"path":"c.txt"}`) {
		t.Fatalf("divergence result = %q", div)
	}
}

// TestReplayMatchesCanonicalKeyOrder records a call with one key order and
// replays it with another. The canonical form of the input matches, so the
// recorded result is served.
func TestReplayMatchesCanonicalKeyOrder(t *testing.T) {
	ctx := testCtx(t)
	rec, orig := replayRecorder(t, ctx,
		replayReader(map[string]string{"a.txt": "AAA"}, nil),
		use("c1", "read_files", `{"path": "a.txt",  "mode":"r"}`),
	)
	var live atomic.Int32
	s := newScript(callTools(use("x1", "read_files", `{"mode":"r","path":"a.txt"}`)), endTurn("replayed"))
	b := newTestAgent(t, s.Model(), WithTools(replayReader(map[string]string{"a.txt": "live A"}, &live)))
	r, err := Replay(ctx, b, rec.Ledger(), orig.ID)
	mustState(t, r, err, StateCompleted)
	if live.Load() != 0 {
		t.Fatalf("replay ran the live tool %d times", live.Load())
	}
	res := lastToolResults(t, s.requests()[1])
	if TextOf(res[0].Content) != "AAA" || res[0].IsError {
		t.Fatalf("result = %+v", res[0])
	}
}

// TestReplayMatchesBigNumbers records a call whose argument is past 2^53 and
// replays it. The exact integer matches, the neighbor above 2^53 does not,
// and the divergence names both literals.
func TestReplayMatchesBigNumbers(t *testing.T) {
	ctx := testCtx(t)
	rec, orig := replayRecorder(t, ctx,
		NewTool(ToolSpec{Name: "bignum", ReadOnly: true}, func(_ context.Context, input json.RawMessage) (ToolResult, error) {
			return TextResult("big"), nil
		}),
		use("c1", "bignum", `{"n":9007199254740993}`),
	)
	var live atomic.Int32
	s := newScript(callTools(
		use("x1", "bignum", `{"n":9007199254740993}`),
		use("x2", "bignum", `{"n":9007199254740992}`),
	), endTurn("replayed"))
	tool := NewTool(ToolSpec{Name: "bignum", ReadOnly: true}, func(_ context.Context, input json.RawMessage) (ToolResult, error) {
		live.Add(1)
		return TextResult("live big"), nil
	})
	b := newTestAgent(t, s.Model(), WithTools(tool))
	r, err := Replay(ctx, b, rec.Ledger(), orig.ID)
	mustState(t, r, err, StateCompleted)
	if live.Load() != 0 {
		t.Fatalf("replay ran the live tool %d times", live.Load())
	}
	res := lastToolResults(t, s.requests()[1])
	if TextOf(res[0].Content) != "big" || res[0].IsError {
		t.Fatalf("exact number result = %+v", res[0])
	}
	if !res[1].IsError {
		t.Fatalf("neighbor number result = %+v", res[1])
	}
	div := TextOf(res[1].Content)
	if !strings.Contains(div, "9007199254740992") {
		t.Fatalf("divergence does not name the requested number: %q", div)
	}
}

// TestReplayErrorResultsArePreserved records a call whose tool returned an
// error and replays it. The replayed call receives the recorded error result,
// not the live success of the tool.
func TestReplayErrorResultsArePreserved(t *testing.T) {
	ctx := testCtx(t)
	flaky := NewTool(ToolSpec{Name: "flaky", ReadOnly: true, Idempotent: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		return ToolResult{}, fmt.Errorf("boom")
	})
	rec, orig := replayRecorder(t, ctx, flaky, use("c1", "flaky", `{}`))
	var live atomic.Int32
	working := NewTool(ToolSpec{Name: "flaky", ReadOnly: true, Idempotent: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		live.Add(1)
		return TextResult("fixed in the new version"), nil
	})
	s := newScript(callTools(use("x1", "flaky", `{}`)), endTurn("replayed"))
	b := newTestAgent(t, s.Model(), WithTools(working))
	r, err := Replay(ctx, b, rec.Ledger(), orig.ID)
	mustState(t, r, err, StateCompleted)
	if live.Load() != 0 {
		t.Fatalf("replay ran the live tool %d times", live.Load())
	}
	res := lastToolResults(t, s.requests()[1])
	if !res[0].IsError || !strings.Contains(TextOf(res[0].Content), "tool flaky: boom") {
		t.Fatalf("recorded error not preserved: %+v", res[0])
	}
}

// TestReplayConsumesRepeatedCallsInOrder records three identical calls and
// replays them with a mismatching call between them. The identical calls
// consume the recorded results in occurrence order, and the mismatch
// consumes nothing.
func TestReplayConsumesRepeatedCallsInOrder(t *testing.T) {
	ctx := testCtx(t)
	var recCalls atomic.Int32
	ping := NewTool(ToolSpec{Name: "ping", ReadOnly: true}, func(_ context.Context, input json.RawMessage) (ToolResult, error) {
		return TextResult(fmt.Sprintf("p%d", recCalls.Add(1))), nil
	})
	rec, orig := replayRecorder(t, ctx, ping,
		use("c1", "ping", `{}`), use("c2", "ping", `{}`), use("c3", "ping", `{}`),
	)
	var live atomic.Int32
	livePing := NewTool(ToolSpec{Name: "ping", ReadOnly: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		live.Add(1)
		return TextResult("live ping"), nil
	})
	s := newScript(callTools(
		use("x1", "ping", `{}`),
		use("x2", "ping", `{"mode":"2"}`),
		use("x3", "ping", `{}`),
	), endTurn("replayed"))
	b := newTestAgent(t, s.Model(), WithTools(livePing))
	r, err := Replay(ctx, b, rec.Ledger(), orig.ID)
	mustState(t, r, err, StateCompleted)
	if live.Load() != 0 {
		t.Fatalf("replay ran the live tool %d times", live.Load())
	}
	res := lastToolResults(t, s.requests()[1])
	if TextOf(res[0].Content) != "p1" || TextOf(res[2].Content) != "p2" {
		t.Fatalf("identical calls consumed out of order: %q, %q", TextOf(res[0].Content), TextOf(res[2].Content))
	}
	if !res[1].IsError {
		t.Fatalf("mismatch result = %+v", res[1])
	}
	div := TextOf(res[1].Content)
	if !strings.Contains(div, `input {"mode":"2"}`) || !strings.Contains(div, "{} (left: 2)") {
		t.Fatalf("divergence does not name the leftovers: %q", div)
	}
}

// TestReplayMissingResults records a run with no call of two tools, and
// replays a call of each. Both get explicit divergence results, one because
// no call of the tool is recorded and one because the ledger holds only a
// result whose input is not recorded.
func TestReplayMissingResults(t *testing.T) {
	ctx := testCtx(t)
	src := NewMemLedger()
	id := "with-stray"
	seed(t, src, id,
		MustEvent(id, EvRunQueued, RunQueuedData{Agent: "default", Input: "hi"}),
		MustEvent(id, EvRunStarted, RunStartedData{}),
		MustEvent(id, EvToolResponded, ToolRespondedData{CallID: "ghost", Tool: "reader", Content: []Block{Text("orphan")}}),
		MustEvent(id, EvRunFinished, RunFinishedData{Outcome: OutcomeCompleted, Output: "hi"}),
	)
	var live atomic.Int32
	reader := NewTool(ToolSpec{Name: "reader", ReadOnly: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		live.Add(1)
		return TextResult("live"), nil
	})
	absent := NewTool(ToolSpec{Name: "absent", ReadOnly: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		live.Add(1)
		return TextResult("live absent"), nil
	})
	s := newScript(callTools(
		use("x1", "reader", `{"path":"z.txt"}`),
		use("x2", "absent", `{}`),
	), endTurn("replayed"))
	a := newTestAgent(t, s.Model(), WithTools(reader, absent))
	r, err := Replay(ctx, a, src, id)
	mustState(t, r, err, StateCompleted)
	if live.Load() != 0 {
		t.Fatalf("replay ran the live tools %d times", live.Load())
	}
	res := lastToolResults(t, s.requests()[1])
	div1, div2 := TextOf(res[0].Content), TextOf(res[1].Content)
	if !res[0].IsError || !strings.Contains(div1, `tool reader with input {"path":"z.txt"}`) || !strings.Contains(div1, "whose call input is not recorded") {
		t.Fatalf("stray divergence = %q", div1)
	}
	if !res[1].IsError || !strings.Contains(div2, "tool absent with input {}") || !strings.Contains(div2, "no unused result for this tool") {
		t.Fatalf("missing divergence = %q", div2)
	}
}

// TestReplayCapturesDeniedCalls records a run whose policy denied one call of
// two. The ledger holds a fw.tool.responded for the denied call and no
// fw.tool.requested. A replay that makes the denied call again receives the
// recorded denial, and the call that ran receives its recorded result.
func TestReplayCapturesDeniedCalls(t *testing.T) {
	ctx := testCtx(t)
	src := NewMemLedger()
	id := "with-denied"
	seed(t, src, id,
		MustEvent(id, EvRunQueued, RunQueuedData{Agent: "default", Input: "hi"}),
		MustEvent(id, EvRunStarted, RunStartedData{}),
		MustEvent(id, EvTurnStarted, TurnData{Turn: 1}),
		MustEvent(id, EvModelResponded, ModelRespondedData{
			Content: []Block{
				use("c1", "read_files", `{"path":"secret.env"}`),
				use("c2", "read_files", `{"path":"ok.txt"}`),
			},
			StopReason: StopToolUse,
		}),
		// c1 was denied by the policy, so the ledger holds only the response.
		MustEvent(id, EvToolResponded, ToolRespondedData{
			CallID:  "c1",
			Tool:    "read_files",
			Content: []Block{Text("denied by policy: rule secret")},
			IsError: true,
		}),
		MustEvent(id, EvToolRequested, ToolRequestedData{Tool: "read_files", CallID: "c2", Input: json.RawMessage(`{"path":"ok.txt"}`)}),
		MustEvent(id, EvToolResponded, ToolRespondedData{CallID: "c2", Tool: "read_files", Content: []Block{Text("OK")}}),
		MustEvent(id, EvRunFinished, RunFinishedData{Outcome: OutcomeCompleted, Output: "hi"}),
	)
	var live atomic.Int32
	s := newScript(callTools(
		use("x1", "read_files", `{"path":"secret.env"}`),
		use("x2", "read_files", `{"path":"ok.txt"}`),
	), endTurn("replayed"))
	a := newTestAgent(t, s.Model(), WithTools(replayReader(map[string]string{"ok.txt": "live OK"}, &live)))
	r, err := Replay(ctx, a, src, id)
	mustState(t, r, err, StateCompleted)
	if live.Load() != 0 {
		t.Fatalf("replay ran the live tool %d times", live.Load())
	}
	res := lastToolResults(t, s.requests()[1])
	if !res[0].IsError || !strings.Contains(TextOf(res[0].Content), "denied by policy: rule secret") {
		t.Fatalf("denied result = %+v", res[0])
	}
	if TextOf(res[1].Content) != "OK" || res[1].IsError {
		t.Fatalf("ran result = %+v", res[1])
	}
}

// TestReplayListsOtherInputsDeterministically records nine calls of one tool
// with inputs other than the replayed one, and replays a call that no
// recorded run made. The divergence result lists a stable, sorted subset of
// one entry more than replayMaxInputs...
func TestReplayListsOtherInputsDeterministically(t *testing.T) {
	ctx := testCtx(t)
	var recCalls atomic.Int32
	ping := NewTool(ToolSpec{Name: "ping", ReadOnly: true}, func(_ context.Context, input json.RawMessage) (ToolResult, error) {
		return TextResult(fmt.Sprintf("p%d", recCalls.Add(1))), nil
	})
	calls := make([]Block, 0, 9)
	for i := 1; i <= 9; i++ {
		calls = append(calls, use(fmt.Sprintf("c%d", i), "ping", fmt.Sprintf(`{"path":"d%02d.txt"}`, i)))
	}
	rec, orig := replayRecorder(t, ctx, ping, calls...)

	var live atomic.Int32
	livePing := NewTool(ToolSpec{Name: "ping", ReadOnly: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		live.Add(1)
		return TextResult("live ping"), nil
	})
	s := newScript(callTools(use("x1", "ping", `{"path":"zz.txt"}`)), endTurn("replayed"))
	b := newTestAgent(t, s.Model(), WithTools(livePing))
	r, err := Replay(ctx, b, rec.Ledger(), orig.ID)
	mustState(t, r, err, StateCompleted)
	if live.Load() != 0 {
		t.Fatalf("replay ran the live tool %d times", live.Load())
	}
	div := TextOf(lastToolResults(t, s.requests()[1])[0].Content)
	if !strings.Contains(div, "made 9 call(s) of this tool with other inputs: ") {
		t.Fatalf("divergence does not count the leftovers: %q", div)
	}
	// The listed subset is the first eight sorted inputs, so the result
	// stays the same on every run and on every map order.
	for i := 1; i <= 8; i++ {
		want := fmt.Sprintf(`{"path":"d%02d.txt"} (left: 1)`, i)
		if !strings.Contains(div, want) {
			t.Fatalf("divergence lacks the sorted entry %s: %q", want, div)
		}
	}
	if strings.Contains(div, "d09.txt") {
		t.Fatalf("divergence shows more than the bounded subset: %q", div)
	}
	want := `inputs: {"path":"d01.txt"} (left: 1), {"path":"d02.txt"} (left: 1), ` +
		`{"path":"d03.txt"} (left: 1), {"path":"d04.txt"} (left: 1), ` +
		`{"path":"d05.txt"} (left: 1), {"path":"d06.txt"} (left: 1), ` +
		`{"path":"d07.txt"} (left: 1), {"path":"d08.txt"} (left: 1).`
	if !strings.Contains(div, want) {
		t.Fatalf("divergence list is not a sorted, bounded list: %q", div)
	}
}

// TestReplayConsumesIdenticalCallsInModelOrder records two identical
// parallel-safe calls and replays the two calls in one response. The replay
// wrapper is not parallel-safe, so the kernel batches the calls one at a
// time, in model call order, and each call consumes the recorded result at
// its own position in the ledger: the replayed first call receives the
// result that the recorded c1 call left and the second the result of c2.
// The recording tool keys its text on the call id from the context, so the
// ledger holds a deterministic content per call and the assertion tests the
// replay, not the goroutine timing of the recording. The live tool never
// runs.
func TestReplayConsumesIdenticalCallsInModelOrder(t *testing.T) {
	ctx := testCtx(t)
	ping := NewTool(ToolSpec{Name: "ping", ReadOnly: true, ParallelSafe: true}, func(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
		return TextResult("text of " + RunInfoFrom(ctx).CallID), nil
	})
	rec, orig := replayRecorder(t, ctx, ping, use("c1", "ping", `{}`), use("c2", "ping", `{}`))
	var live atomic.Int32
	livePing := NewTool(ToolSpec{Name: "ping", ReadOnly: true, ParallelSafe: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		live.Add(1)
		return TextResult("live ping"), nil
	})
	s := newScript(callTools(use("x1", "ping", `{}`), use("x2", "ping", `{}`)), endTurn("replayed"))
	b := newTestAgent(t, s.Model(), WithTools(livePing))
	r, err := Replay(ctx, b, rec.Ledger(), orig.ID)
	mustState(t, r, err, StateCompleted)
	if live.Load() != 0 {
		t.Fatalf("replay ran the live tool %d times", live.Load())
	}
	res := lastToolResults(t, s.requests()[1])
	if text := TextOf(res[0].Content); text != "text of c1" || res[0].IsError {
		t.Fatalf("first consume = %+v, want the recorded result of c1", res[0])
	}
	if text := TextOf(res[1].Content); text != "text of c2" || res[1].IsError {
		t.Fatalf("second consume = %+v, want the recorded result of c2", res[1])
	}
}
