package fw

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTurnRetriesInterruptedStream(t *testing.T) {
	ctx := testCtx(t)
	var calls, tools atomic.Int32
	var first *Request
	m := ModelFunc{StreamFn: func(_ context.Context, req *Request, on func(Delta)) (*Response, error) {
		switch calls.Add(1) {
		case 1:
			first = req
			partial := callTools(use("discard", "echo", `{"text":"partial"}`))
			partial.Usage.InputTokens = 999
			on(Delta{Type: "text", Text: "partial"})
			on(Delta{Type: "block", Block: &partial.Content[2]})
			return partial, fmt.Errorf("stream: %w", &StreamInterruptedError{Err: io.ErrUnexpectedEOF})
		case 2:
			if !reflect.DeepEqual(req, first) {
				t.Error("retry changed the request or run ID")
			}
			return callTools(use("keep", "echo", `{"text":"complete"}`)), nil
		default:
			return endTurn("done"), nil
		}
	}}
	a := newTestAgent(t, m, WithTools(echoTool(&tools)))
	r, err := a.Run(ctx, "work")
	mustState(t, r, err, StateCompleted)
	if calls.Load() != 3 || tools.Load() != 1 || r.Output != "done" || r.Usage.InputTokens != 20 || r.Usage.Turns != 2 {
		t.Fatalf("calls=%d tools=%d run=%+v", calls.Load(), tools.Load(), r)
	}
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	if countType(evs, EvRunQueued) != 1 || countType(evs, EvModelRequested) != 2 || countType(evs, EvModelResponded) != 2 || countType(evs, EvError) != 1 {
		t.Fatalf("events = %v", eventTypes(evs))
	}
	var d ErrorData
	firstOf(t, evs, EvError, &d)
	if !d.Retryable {
		t.Fatal("interruption was not recorded as retryable")
	}
	for _, e := range evs {
		if strings.Contains(string(e.Data), "discard") || strings.Contains(string(e.Data), "partial") {
			t.Fatalf("partial response escaped into %s: %s", e.Type, e.Data)
		}
	}
}

func TestTurnInterruptedStreamRetryExhausted(t *testing.T) {
	ctx := testCtx(t)
	var calls, tools atomic.Int32
	m := ModelFunc{StreamFn: func(context.Context, *Request, func(Delta)) (*Response, error) {
		calls.Add(1)
		return callTools(use("discard", "echo", `{}`)), &StreamInterruptedError{Err: io.ErrUnexpectedEOF}
	}}
	a := newTestAgent(t, m, WithTools(echoTool(&tools)))
	r, err := a.Run(ctx, "work")
	mustState(t, r, err, StateFailed)
	if calls.Load() != 2 || tools.Load() != 0 || r.Output != "" || !strings.Contains(r.Error, "unexpected EOF") {
		t.Fatalf("calls=%d tools=%d run=%+v", calls.Load(), tools.Load(), r)
	}
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	if countType(evs, EvRunQueued) != 1 || countType(evs, EvModelRequested) != 1 || countType(evs, EvModelResponded) != 0 || countType(evs, EvRunFinished) != 1 {
		t.Fatalf("events = %v", eventTypes(evs))
	}
}

func TestTurnDoesNotRetryUnclassifiedErrors(t *testing.T) {
	for _, failure := range []error{io.EOF, errors.New("malformed payload"), context.Canceled, context.DeadlineExceeded} {
		t.Run(failure.Error(), func(t *testing.T) {
			var calls atomic.Int32
			a := newTestAgent(t, ModelFunc{StreamFn: func(context.Context, *Request, func(Delta)) (*Response, error) {
				calls.Add(1)
				return nil, failure
			}})
			r, err := a.Run(testCtx(t), "work")
			mustState(t, r, err, StateFailed)
			if calls.Load() != 1 {
				t.Fatalf("calls=%d, want 1", calls.Load())
			}
		})
	}
}

func TestTurnInterruptedStreamCanceledDuringBackoff(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	a := newTestAgent(t, ModelFunc{StreamFn: func(context.Context, *Request, func(Delta)) (*Response, error) {
		calls.Add(1)
		return nil, &StreamInterruptedError{Err: io.ErrUnexpectedEOF}
	}})
	r, err := a.Start(ctx, "work")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "interruption recorded", func() bool {
		return countType(ledgerEvents(t, a.Ledger(), r.ID), EvError) == 1
	})
	if err := a.Cancel(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCanceled)
	// Wait for the canceled driver as well as the already-terminal run.
	eventually(t, "canceled driver exited", func() bool {
		a.k.slotMu.Lock()
		defer a.k.slotMu.Unlock()
		return a.k.rootDrivers == 0
	})
	if calls.Load() != 1 || countType(ledgerEvents(t, a.Ledger(), r.ID), EvModelResponded) != 0 {
		t.Fatalf("calls=%d, want no retry or answer after cancellation", calls.Load())
	}
}

func TestTurnInterruptedStreamDeadline(t *testing.T) {
	for _, duringRetry := range []bool{false, true} {
		t.Run(fmt.Sprintf("duringRetry=%v", duringRetry), func(t *testing.T) {
			var calls atomic.Int32
			m := ModelFunc{StreamFn: func(ctx context.Context, _ *Request, _ func(Delta)) (*Response, error) {
				if calls.Add(1) == 1 {
					return nil, &StreamInterruptedError{Err: io.ErrUnexpectedEOF}
				}
				<-ctx.Done()
				return nil, ctx.Err()
			}}
			a := newTestAgent(t, m)
			limit, wantCalls := 100*time.Millisecond, int32(1)
			if duringRetry {
				limit, wantCalls = modelStreamRetryWait+300*time.Millisecond, 2
			}
			r, err := a.Run(testCtx(t), "work", WithRunBudget(Budget{Deadline: limit, FinalReport: noReport()}))
			mustState(t, r, err, StateFailed)
			if calls.Load() != wantCalls || r.Error != "budget exceeded: deadline" || r.Output != "" {
				t.Fatalf("calls=%d run=%+v", calls.Load(), r)
			}
			evs := ledgerEvents(t, a.Ledger(), r.ID)
			if countType(evs, EvModelResponded) != 0 || countType(evs, EvTurnFinished) != 1 {
				t.Fatalf("events = %v", eventTypes(evs))
			}
		})
	}
}

func TestStreamInterruptedErrorUnwrap(t *testing.T) {
	err := fmt.Errorf("model: %w", &StreamInterruptedError{Err: io.ErrUnexpectedEOF})
	var interrupted *StreamInterruptedError
	if !errors.As(err, &interrupted) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("lost underlying error: %v", err)
	}
}
