package fw

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
)

// intentEvent makes an fw.tool.requested event with raw data, so a test
// controls the exact shape of the payload, including the shapes of a
// legacy event and a malformed event.
func intentEvent(t *testing.T, id, data string) Event {
	t.Helper()
	e := MustEvent(id, EvToolRequested, nil)
	e.Data = json.RawMessage(data)
	return e
}

// The file ledger fsyncs the intent of a non-idempotent tool call, an
// approval, and a terminal event. An explicitly idempotent intent saves
// the fsync, and a legacy or malformed event must not bypass the durable
// write. Events that need no durability stay cheap. The chain of
// directory entries behind a file of a new run is on disk through one
// fsync apiece, and a reopen of an established run adds none.
func TestFileLedgerSyncsToolIntent(t *testing.T) {
	ctx := testCtx(t)
	l, err := OpenFileLedger(filepath.Join(t.TempDir(), "runs"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	setupSyncs := l.rsyncs.Load()
	check := func(syncs, dsyncs, rsyncs int64) {
		t.Helper()
		if n := l.syncs.Load(); n != syncs {
			t.Fatalf("fsync calls = %d, want %d", n, syncs)
		}
		if n := l.dsyncs.Load(); n != dsyncs {
			t.Fatalf("run directory fsync calls = %d, want %d", n, dsyncs)
		}
		if n := l.rsyncs.Load() - setupSyncs; n != rsyncs {
			t.Fatalf("root fsync calls = %d, want %d", n, rsyncs)
		}
	}
	appendEvent := func(e Event) {
		t.Helper()
		if _, err := l.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	// A non-idempotent intent fsyncs the file, its directory entry, and
	// the entry of the run directory in the root. The entry of the index
	// file came before them, in the same append.
	appendEvent(intentEvent(t, "A", `{"tool":"bash","callId":"c1","input":{},"idempotent":false}`))
	check(1, 1, 2)

	// An explicitly idempotent intent stays cheap.
	appendEvent(intentEvent(t, "A", `{"tool":"clock","callId":"c2","input":{},"idempotent":true}`))
	check(1, 1, 2)

	// A legacy event without the idempotent field counts as not
	// idempotent.
	appendEvent(intentEvent(t, "A", `{"tool":"bash","callId":"c3","input":{}}`))
	check(2, 1, 2)

	// A typed event without the field does the same. This is the shape
	// that a direct caller or an older version of the kernel produces.
	appendEvent(MustEvent("A", EvToolRequested, ToolRequestedData{Tool: "files", CallID: "c4"}))
	check(3, 1, 2)

	// An event with a payload that is not valid JSON cannot reach the
	// file at all: the marshal of the line refuses it. Append fails, and
	// no line lands.
	if _, err := l.Append(ctx, intentEvent(t, "A", `{"tool":`)); err == nil {
		t.Fatal("the ledger accepted a payload that is not JSON")
	}
	check(3, 1, 2)

	// A payload that is valid JSON but not an object cannot carry the
	// classification. The ledger writes it and counts it as not
	// idempotent.
	if _, err := l.Append(ctx, intentEvent(t, "A", `[1,2]`)); err != nil {
		t.Fatal(err)
	}
	check(4, 1, 2)
	appendEvent(MustEvent("A", EvToolRequested, nil))
	check(5, 1, 2)

	// An approval and a terminal event still fsync. Events of no other
	// type still do not.
	appendEvent(MustEvent("A", EvApprovalRequested, ApprovalRequestedData{ApprovalID: "a1", Tool: "bash"}))
	check(6, 1, 2)
	appendEvent(MustEvent("A", EvApprovalDecided, ApprovalDecidedData{ApprovalID: "a1", Decision: "allow"}))
	check(7, 1, 2)
	appendEvent(MustEvent("A", EvTurnStarted, TurnData{Turn: 1}))
	appendEvent(MustEvent("A", EvToolResponded, ToolRespondedData{CallID: "c1"}))
	check(7, 1, 2)
	appendEvent(MustEvent("A", EvRunFinished, RunFinishedData{Outcome: OutcomeCompleted}))
	check(8, 1, 2)

	// An event after the end opens the ledger file again. The chain
	// behind it is established, so the reopen adds no fsync of any kind.
	appendEvent(MustEvent("A", EvRunResumed, RunResumedData{FromSeq: 8}))
	check(8, 1, 2)

	// Every line that the ledger accepted is in the file in order.
	want := []string{
		EvToolRequested, EvToolRequested, EvToolRequested, EvToolRequested,
		EvToolRequested, EvToolRequested, EvApprovalRequested, EvApprovalDecided,
		EvTurnStarted, EvToolResponded, EvRunFinished, EvRunResumed,
	}
	var got []string
	for e, err := range l.Read(ctx, "A", 0) {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, e.Type)
	}
	if len(got) != len(want) {
		t.Fatalf("the file holds %d events, want %d: %v", len(got), len(want), got)
	}
	for i, typ := range want {
		if got[i] != typ {
			t.Fatalf("line %d = %s, want %s", i+1, got[i], typ)
		}
	}
}

// Every surface acknowledges the queue, so every origin needs the boundary.
func TestFileLedgerSyncsTheQueueOfEveryOrigin(t *testing.T) {
	ctx := testCtx(t)
	l, err := OpenFileLedger(filepath.Join(t.TempDir(), "runs"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, origin := range []string{OriginHTTP, OriginBus, OriginCLI, OriginA2A, OriginMCP, OriginKernel, ""} {
		before := l.syncs.Load()
		if _, err := l.Append(ctx, MustEvent("R-"+origin, EvRunQueued, RunQueuedData{Agent: "default", Input: "x", Origin: origin})); err != nil {
			t.Fatal(err)
		}
		synced := l.syncs.Load() > before
		if !synced {
			t.Errorf("origin %s: queue was not synced", origin)
		}
	}
}

// A failed fsync reports failure and keeps the debt of the entries behind
// the file. A retry pays every debt, however many appends later it is,
// and a path that now exists does not end the debt for it.
func TestFileLedgerSyncFailureAndRetry(t *testing.T) {
	ctx := testCtx(t)
	l, err := OpenFileLedger(filepath.Join(t.TempDir(), "runs"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	// A batched event establishes the file without paying its entry debts.
	staged := func() Event { return MustEvent("A", EvRunStarted, RunStartedData{Model: "test"}) }
	setupSyncs := l.rsyncs.Load()
	check := func(syncs, dsyncs, rsyncs int64) {
		t.Helper()
		if n := l.syncs.Load(); n != syncs {
			t.Fatalf("fsync calls = %d, want %d", n, syncs)
		}
		if n := l.dsyncs.Load(); n != dsyncs {
			t.Fatalf("directory fsync calls = %d, want %d", n, dsyncs)
		}
		if n := l.rsyncs.Load() - setupSyncs; n != rsyncs {
			t.Fatalf("root fsync calls = %d, want %d", n, rsyncs)
		}
	}

	// A planned failure of the fsync of the directory behind the index
	// entry fails the append: the entry of index.jsonl in the root is not
	// on disk, so the ledger cannot promise the run yet.
	l.failDirSyncs.Store(1)
	if _, err := l.Append(ctx, staged()); err == nil {
		t.Fatal("a failed fsync reported success")
	}
	check(0, 0, 0)
	lines := 0
	for _, err := range l.Read(ctx, "A", 0) {
		if err != nil {
			t.Fatal(err)
		}
		lines++
	}
	if lines != 0 {
		t.Fatalf("a failed append left %d lines in the ledger file", lines)
	}

	// The retry pays the debt of the root entry of the index file, and
	// only then reports success.
	if _, err := l.Append(ctx, staged()); err != nil {
		t.Fatal(err)
	}
	check(0, 0, 1)

	// A planned failure of the fsync of the run file fails the append of
	// the intent to run a non-idempotent tool. The line is in the file,
	// but the ledger did not report it.
	l.failFileSyncs.Store(1)
	if _, err := l.Append(ctx, intentEvent(t, "A", `{"tool":"bash","callId":"c1","input":{},"idempotent":false}`)); err == nil {
		t.Fatal("a failed fsync reported success")
	}
	check(0, 0, 1)
	lines = 0
	for _, err := range l.Read(ctx, "A", 0) {
		if err != nil {
			t.Fatal(err)
		}
		lines++
	}
	if lines != 2 {
		t.Fatalf("the file holds %d lines, want 2 with the unreported one", lines)
	}

	// A retry pays the fsync of the file and fails on the entry in the
	// directory of the run. The debts of the directory and of the root
	// stay, and a path that now exists does not end them for it.
	l.failDirSyncs.Store(1)
	if _, err := l.Append(ctx, intentEvent(t, "A", `{"tool":"bash","callId":"c2","input":{},"idempotent":false}`)); err == nil {
		t.Fatal("a failed fsync reported success")
	}
	check(1, 0, 1)

	// The next append pays both debts: one fsync of the directory, then
	// one of the root, each after the fsync of the file.
	if _, err := l.Append(ctx, MustEvent("A", EvApprovalRequested, ApprovalRequestedData{ApprovalID: "a1", Tool: "bash"})); err != nil {
		t.Fatal(err)
	}
	check(2, 1, 2)

	// A second run walks its own debts. The fsync of its directory fails,
	// and the append after it pays it, and the entry of the directory in
	// the root, before it reports success.
	if _, err := l.Append(ctx, MustEvent("B", EvRunStarted, nil)); err != nil {
		t.Fatal(err)
	}
	l.failDirSyncs.Store(1)
	if _, err := l.Append(ctx, intentEvent(t, "B", `{"tool":"bash","callId":"d1","input":{},"idempotent":false}`)); err == nil {
		t.Fatal("a failed fsync reported success")
	}
	check(3, 1, 2)
	if _, err := l.Append(ctx, MustEvent("B", EvApprovalRequested, ApprovalRequestedData{ApprovalID: "a2", Tool: "bash"})); err != nil {
		t.Fatal(err)
	}
	check(4, 2, 3)
}

// Nested MkdirAll creates several entries. A setup failure can leave every
// path present, so a second Open must still sync the complete parent chain.
func TestFileLedgerNestedSetupRetry(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "one", "two", "runs")
	want := []string{root, filepath.Dir(root), filepath.Join(base, "one"), base}
	failed := errors.New("setup sync failed")
	var attempted []string
	_, err = openFileLedger(root, func(path string) error {
		attempted = append(attempted, path)
		if path == filepath.Join(base, "one") {
			return failed
		}
		return nil
	})
	if !errors.Is(err, failed) || !slices.Equal(attempted, want[:3]) {
		t.Fatalf("first open: syncs=%v, err=%v", attempted, err)
	}
	attempted = nil
	l, err := openFileLedger(root, func(path string) error {
		attempted = append(attempted, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if len(attempted) < len(want) || !slices.Equal(attempted[:len(want)], want) {
		t.Fatalf("retry skipped a parent entry: syncs=%v, want prefix=%v", attempted, want)
	}
	if last := attempted[len(attempted)-1]; filepath.Dir(last) != last {
		t.Fatalf("setup did not sync through the filesystem root: %v", attempted)
	}
}

func TestFileLedgerRepairsPartialIndexBeforeAppend(t *testing.T) {
	for _, reopen := range []bool{false, true} {
		t.Run(map[bool]string{false: "same instance", true: "reopen"}[reopen], func(t *testing.T) {
			ctx := testCtx(t)
			root := filepath.Join(t.TempDir(), "runs")
			l, err := OpenFileLedger(root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = l.Close() })
			if _, err := l.Append(ctx, MustEvent("A", EvRunFinished, RunFinishedData{Outcome: OutcomeCompleted})); err != nil {
				t.Fatal(err)
			}
			f, err := os.OpenFile(filepath.Join(root, indexFileName), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.WriteString(`{"id":"torn`)
			_ = f.Close()
			if err != nil {
				t.Fatal(err)
			}
			if reopen {
				_ = l.Close()
				l, err = OpenFileLedger(root)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := l.Append(ctx, intentEvent(t, "B", `{"tool":"bash","callId":"b","input":{}}`)); err != nil {
				t.Fatal(err)
			}
			var ids []string
			for id, err := range l.Runs(ctx) {
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, id)
			}
			if !slices.Equal(ids, []string{"A", "B"}) {
				t.Fatalf("durable intent is not discoverable: runs=%v", ids)
			}
		})
	}
}

func TestFileLedgerIntentFailurePreventsToolExecution(t *testing.T) {
	ctx := testCtx(t)
	l, err := OpenFileLedger(filepath.Join(t.TempDir(), "runs"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var calls atomic.Int32
	tool := NewTool(ToolSpec{Name: "effect", ReadOnly: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		calls.Add(1)
		return TextResult("ran"), nil
	})
	seed(t, l, "intent-failure", MustEvent("intent-failure", EvRunQueued, RunQueuedData{Agent: "default", Input: "perform the effect"}))
	a := newTestAgent(t, newScript().Model(), WithLedger(l), WithTools(tool), WithoutResume())
	rs, err := a.k.lookup("intent-failure")
	if err != nil {
		t.Fatal(err)
	}
	// Test the execution boundary directly. A storage failure stops the
	// driver without assuming it can persist a terminal event on that disk.
	l.failFileSyncs.Store(1)
	err = a.runBatch(ctx, rs, []plannedCall{{use: use("effect-1", "effect", `{}`), tool: tool, spec: tool.Spec(), run: true}})
	if err == nil {
		t.Fatal("the batch ignored an intent sync failure")
	}
	if calls.Load() != 0 {
		t.Fatal("tool ran after the intent failed to become durable")
	}
}

// idemProbe is a read-only, idempotent tool that checks, inside its own
// body, that the ledger recorded the classification of its spec and did
// not spend an fsync on the intent.
type idemProbe struct {
	t      *testing.T
	l      *FileLedger
	before int64
}

func (p *idemProbe) Spec() ToolSpec {
	return ToolSpec{
		Name:         "idem",
		Description:  "test tool",
		InputSchema:  json.RawMessage(`{"type":"object"}`),
		ReadOnly:     true,
		Idempotent:   true,
		ParallelSafe: true,
	}
}

func (p *idemProbe) Call(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
	if n := p.l.syncs.Load(); n != p.before {
		p.t.Errorf("the body of an idempotent tool ran after %d fsync calls, want %d (the intent must not fsync)", n, p.before)
	}
	found := false
	for e, err := range p.l.Read(context.Background(), RunInfoFrom(ctx).RunID, 0) {
		if err != nil {
			p.t.Errorf("read the ledger: %v", err)
			return ToolResult{IsError: true}, nil
		}
		if e.Type != EvToolRequested {
			continue
		}
		var d ToolRequestedData
		if err := e.Decode(&d); err != nil {
			p.t.Errorf("decode the intent: %v", err)
			return ToolResult{IsError: true}, nil
		}
		if d.CallID == "call-1" {
			found = true
			if !d.Idempotent {
				p.t.Error("the ledger did not record the idempotency of the call")
			}
		}
	}
	if !found {
		p.t.Error("the intent of the call was not in the ledger file when the body ran")
	}
	return TextResult("ran once"), nil
}

// The intent to run an idempotent tool carries the classification of the
// spec of the tool, and its arrival costs no fsync: the run saves both
// the intent fsync and, through nothing else, the file stays cheap. This
// test fails when the kernel drops the field on the way to the ledger.
func TestFileLedgerSkipsSyncForIdempotentIntent(t *testing.T) {
	ctx := testCtx(t)
	l, err := OpenFileLedger(filepath.Join(t.TempDir(), "runs"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	probe := &idemProbe{t: t, l: l, before: l.syncs.Load() + 1} // accepted queue
	a := newTestAgent(t, newScript(
		callTools(use("call-1", "idem", `{}`)),
		endTurn("done"),
	).Model(), WithLedger(l), WithTools(probe))
	r, err := a.Run(ctx, "use the idem tool")
	mustState(t, r, err, StateCompleted)

	// The intent added no fsync. The queue and terminal event did.
	if n := l.syncs.Load(); n != probe.before+1 {
		t.Fatalf("fsync calls after the run = %d, want %d (queue and terminal event only)", n, probe.before+1)
	}
	var intent ToolRequestedData
	var have bool
	for e, err := range l.Read(ctx, r.ID, 0) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Type == EvToolRequested {
			if err := e.Decode(&intent); err != nil {
				t.Fatal(err)
			}
			if intent.CallID != "call-1" {
				t.Fatalf("intent call id = %s, want call-1", intent.CallID)
			}
			have = true
		}
	}
	if !have {
		t.Fatal("no intent for the call in the ledger")
	}
	if !intent.Idempotent {
		t.Error("the ledger recorded the idempotent tool as not idempotent")
	}
}

// intentProbe is a read-only, non-idempotent tool that checks, inside its
// own body, that the intent to run it reached the disk before the body
// ran.
type intentProbe struct {
	t      *testing.T
	l      *FileLedger
	before int64
}

func (p *intentProbe) Spec() ToolSpec {
	return ToolSpec{
		Name:         "probe",
		Description:  "test tool",
		InputSchema:  json.RawMessage(`{"type":"object"}`),
		ReadOnly:     true,
		ParallelSafe: true,
		// Idempotent stays false: the kernel must not lose the request.
	}
}

func (p *intentProbe) Call(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
	if n := p.l.syncs.Load(); n != p.before+1 {
		p.t.Errorf("the tool body ran after %d fsync calls, want %d (one for the intent)", n, p.before+1)
	}
	found := false
	for e, err := range p.l.Read(context.Background(), RunInfoFrom(ctx).RunID, 0) {
		if err != nil {
			p.t.Errorf("read the ledger: %v", err)
			return ToolResult{IsError: true}, nil
		}
		if e.Type != EvToolRequested {
			continue
		}
		var d ToolRequestedData
		if err := e.Decode(&d); err != nil {
			p.t.Errorf("decode the intent: %v", err)
			return ToolResult{IsError: true}, nil
		}
		if d.CallID == "call-1" {
			found = true
			if d.Idempotent {
				p.t.Error("the ledger recorded the tool as idempotent")
			}
		}
	}
	if !found {
		p.t.Error("the intent of the call was not in the ledger file when the body ran")
	}
	return TextResult("ran once"), nil
}

// The intent to run a non-idempotent tool is in the ledger file, and
// fsynced, before the body of the tool runs, in a full run of an agent
// over the file ledger.
func TestFileLedgerIntentDurableBeforeToolBody(t *testing.T) {
	ctx := testCtx(t)
	l, err := OpenFileLedger(filepath.Join(t.TempDir(), "runs"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	probe := &intentProbe{t: t, l: l, before: l.syncs.Load() + 1} // accepted queue
	a := newTestAgent(t, newScript(
		callTools(use("call-1", "probe", `{}`)),
		endTurn("done"),
	).Model(), WithLedger(l), WithTools(probe))
	r, err := a.Run(ctx, "use the probe")
	mustState(t, r, err, StateCompleted)

	// The ledger file on disk holds the fsynced intent.
	found := false
	for e, err := range l.Read(ctx, r.ID, 0) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Type != EvToolRequested {
			continue
		}
		var d ToolRequestedData
		if err := e.Decode(&d); err != nil {
			t.Fatal(err)
		}
		if d.CallID == "call-1" {
			found = true
			if d.Idempotent {
				t.Error("the ledger recorded the tool as idempotent")
			}
		}
	}
	if !found {
		t.Fatal("no intent for the call in the ledger")
	}
}
