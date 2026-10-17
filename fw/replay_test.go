package fw

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
)

func TestReplayAnswersFromRecordedResults(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	count := NewTool(ToolSpec{Name: "count", Idempotent: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		n := calls.Add(1)
		return TextResult(strings.Repeat("n", int(n))), nil
	})
	rec := newTestAgent(t, newScript(callTools(use("c1", "count", `{}`)), endTurn("counted")).Model(), WithTools(count))
	orig, err := rec.Run(ctx, "count once", WithMetadata(map[string]string{"case": "7"}), WithIdempotencyKey("orig"))
	mustState(t, orig, err, StateCompleted)
	if calls.Load() != 1 {
		t.Fatalf("calls = %d", calls.Load())
	}

	s := newScript(callTools(use("x1", "count", `{}`), use("x2", "count", `{}`)), endTurn("replayed"))
	b := newTestAgent(t, s.Model(), WithTools(count))
	r, err := Replay(ctx, b, rec.Ledger(), orig.ID)
	mustState(t, r, err, StateCompleted)
	if calls.Load() != 1 {
		t.Fatalf("replay ran the real tool: calls = %d", calls.Load())
	}
	if r.Input != "count once" || r.Metadata["replayOf"] != orig.ID || r.Metadata["case"] != "7" || r.Metadata["idempotencyKey"] != "" {
		t.Fatalf("replay run = %+v", r)
	}
	res := lastToolResults(t, s.requests()[1])
	if TextOf(res[0].Content) != "n" || res[0].IsError {
		t.Fatalf("first result = %+v", res[0])
	}
	if !res[1].IsError || !strings.Contains(TextOf(res[1].Content), "no recorded result") {
		t.Fatalf("second result = %+v", res[1])
	}
	if _, err := Replay(ctx, b, rec.Ledger(), "missing"); err == nil {
		t.Fatal("replay of a missing run succeeded")
	}
}

func TestCompaction(t *testing.T) {
	ctx := testCtx(t)
	s := newScript(
		callTools(use("c1", "echo", `{"text":"one"}`)),
		callTools(use("c2", "echo", `{"text":"two"}`)),
		endTurn("done"),
	)
	a := newTestAgent(t, s.Model(), WithTools(echoTool(nil)))
	a.k.compactAt = 1
	r, err := a.Run(ctx, "work")
	mustState(t, r, err, StateCompleted)
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	// Simple compaction replaces the whole history, so each later turn
	// starts from a summary.
	if n := countType(evs, EvContextCompacted); n != 2 {
		t.Fatalf("compactions = %d in %v", n, eventTypes(evs))
	}
	var d ContextCompactedData
	firstOf(t, evs, EvContextCompacted, &d)
	if d.Epoch != 1 || !strings.Contains(d.Summary.Text, "SUMMARY") || len(d.Kept) != 0 {
		t.Fatalf("compacted = %+v", d)
	}
	reqs := s.requests()
	msgs := reqs[2].Messages
	if len(msgs) != 1 || !strings.Contains(TextOf(msgs[0].Content), "SUMMARY") {
		t.Fatalf("history after compaction = %+v", msgs)
	}
	if reqs[2].System != reqs[0].System || len(reqs[2].Tools) != len(reqs[0].Tools) {
		t.Fatalf("the prefix changed after compaction")
	}
	h := ProjectHistory(evs)
	if h.Epoch != 2 || len(h.Messages) != 2 {
		t.Fatalf("projection = epoch %d, %d messages", h.Epoch, len(h.Messages))
	}
}

func TestCustomCompactorError(t *testing.T) {
	ctx := testCtx(t)
	failing := CompactorFunc(func(context.Context, Model, History) (Block, []Message, error) {
		return Block{}, nil, context.DeadlineExceeded
	})
	a := newTestAgent(t, newScript(callTools(use("c1", "echo", `{}`)), endTurn("done")).Model(), WithTools(echoTool(nil)), WithCompactor(failing))
	a.k.compactAt = 1
	r, err := a.Run(ctx, "work")
	mustState(t, r, err, StateCompleted)
	var d ErrorData
	firstOf(t, ledgerEvents(t, a.Ledger(), r.ID), EvError, &d)
	if !d.Retryable || !strings.HasPrefix(d.Message, "compaction:") {
		t.Fatalf("error = %+v", d)
	}
}
