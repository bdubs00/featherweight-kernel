package fw

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"reflect"
	"sync"
	"testing"
	"time"
)

func cursorEvent(t testing.TB, seq int, typ string, data any) Event {
	t.Helper()
	e, err := NewEvent("cursor-test", typ, data)
	if err != nil {
		t.Fatal(err)
	}
	e.Seq, e.Time = uint64(seq), time.Unix(int64(seq), 0).UTC()
	return e
}

// analyze is the original full-history, two-pass analyzer. Compare every
// execution field at every prefix, not just the action the driver selects.
// prints and priorN are its unbounded reference representation; loop and
// priorCount represent the same information needed by the repeat guard.
func checkCursorPrefixes(t *testing.T, events []Event) {
	t.Helper()
	b := &cursorBuilder{}
	for i, e := range events {
		b.apply(e)
		got, want := b.snapshot(), analyze(events[:i+1])
		want.prints, want.priorN = nil, 0
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("prefix %d (%s):\n got %+v\nwant %+v", i+1, e.Type, *got, *want)
		}
	}
}

func TestCursorIncrementalHistories(t *testing.T) {
	var events []Event
	add := func(typ string, data any) { events = append(events, cursorEvent(t, len(events)+1, typ, data)) }
	response := func(stop StopReason, calls ...string) {
		blocks := []Block{Text("response")}
		for _, id := range calls {
			blocks = append(blocks, Block{Type: BlockToolUse, ID: id, Name: "ask", Input: json.RawMessage(`{"x":1}`)})
		}
		add(EvModelResponded, ModelRespondedData{StopReason: stop, Content: blocks})
	}
	add(EvRunQueued, RunQueuedData{Agent: "test", Kept: 6})
	add(EvRunStarted, RunStartedData{Brief: "brief", TimeZone: "UTC"})
	add(EvTurnStarted, TurnData{Turn: 1})
	add(EvModelRequested, ModelRequestedData{})
	response(StopToolUse, "a", "b")
	add(EvToolRequested, ToolRequestedData{CallID: "a"})
	add(EvApprovalRequested, ApprovalRequestedData{ApprovalID: "q1", CallID: "a", Schema: json.RawMessage(answerSchema)})
	add(EvApprovalRequested, ApprovalRequestedData{ApprovalID: "q2", CallID: "a", Schema: json.RawMessage(answerSchema)})
	add(EvRunResumed, RunResumedData{})
	add(EvApprovalDecided, ApprovalDecidedData{ApprovalID: "q1", Decision: "allow", Content: json.RawMessage(`{"answer":"yes"}`)})
	add(EvApprovalDecided, ApprovalDecidedData{ApprovalID: "q2", Decision: "deny"})
	add(EvApprovalRequested, ApprovalRequestedData{ApprovalID: "conn", CallID: "b", Connect: &ConnectRequest{}})
	add(EvApprovalDecided, ApprovalDecidedData{ApprovalID: "conn", Decision: "deny"})
	add(EvApprovalRequested, ApprovalRequestedData{ApprovalID: "gate", CallID: "b"})
	add(EvApprovalDecided, ApprovalDecidedData{ApprovalID: "gate", Decision: "allow"})
	add(EvToolRequested, ToolRequestedData{CallID: "b"})
	add(EvToolResponded, ToolRespondedData{CallID: "a"})
	add(EvToolResponded, ToolRespondedData{CallID: "b"})
	add(EvMessageReceived, MessageReceivedData{Role: RoleSystem, Origin: OriginKernel, Note: NoteLoop})
	add(EvTurnFinished, TurnData{Turn: 1})
	add(EvContextCompacted, ContextCompactedData{})
	add(EvTurnStarted, TurnData{Turn: 2})
	add(EvModelRequested, ModelRequestedData{})
	response(StopMaxTokens)
	add(EvMessageReceived, MessageReceivedData{Role: RoleUser, Origin: OriginKernel, Content: []Block{Text("Continue.")}})
	add(EvTurnFinished, TurnData{Turn: 2})
	add(EvTurnStarted, TurnData{Turn: 3})
	add(EvModelRequested, ModelRequestedData{})
	response(StopMaxTokens)
	add(EvMessageReceived, MessageReceivedData{Role: RoleSystem, Origin: OriginKernel, Note: NoteTime})
	add(EvMessageReceived, MessageReceivedData{Role: RoleSystem, Origin: OriginKernel, Note: NoteBudget, Budget: &BudgetNoteData{Limit: "maxTurns", Max: 4, Percent: 90}})
	add(EvBudgetExceeded, BudgetExceededData{Limit: "maxTurns", Report: true})
	add(EvMessageReceived, MessageReceivedData{Role: RoleSystem, Origin: OriginKernel, Note: NoteReport})
	add(EvModelRequested, ModelRequestedData{Report: true})
	add(EvMessageReceived, MessageReceivedData{Role: RoleUser, Origin: "http"})
	add(EvBudgetChanged, BudgetChangedData{Budget: Budget{MaxTurns: 10}})
	response(StopEndTurn, "ignored-report-call")
	add(EvMessageReceived, MessageReceivedData{Role: RoleSystem, Origin: OriginKernel, Note: NoteBudget, Budget: &BudgetNoteData{Changed: true}})
	add(EvModelRequested, ModelRequestedData{})
	response(StopToolUse, "a")
	// A late decision for an earlier approval still addresses its recorded
	// call ID, even when a later response reused that call ID.
	add(EvApprovalDecided, ApprovalDecidedData{ApprovalID: "gate", Decision: "deny"})
	add(EvBudgetExceeded, BudgetExceededData{Limit: "maxTurns", Report: true})
	add(EvBudgetChanged, BudgetChangedData{Budget: Budget{MaxTurns: 12}})
	add(EvBudgetExceeded, BudgetExceededData{Limit: "maxTurns", Report: true})
	add(EvModelRequested, ModelRequestedData{Report: true})
	add(EvError, ErrorData{Message: "report failed"})
	add(EvRunFinished, RunFinishedData{Outcome: OutcomeFailed})
	add(EvRunAnnotated, RunAnnotatedData{})
	add(EvChildReported, ChildReportedData{})
	add(EvBudgetChanged, BudgetChangedData{Budget: Budget{MaxTurns: 20}})
	add(EvMessageReceived, MessageReceivedData{Role: RoleUser, Origin: "http"})
	add(EvModelRequested, ModelRequestedData{})
	response(StopPauseTurn)
	checkCursorPrefixes(t, events)
	// Every branch cut can leave a kept turn, report, question, or gate open.
	for kept := range len(events) {
		fork := append([]Event(nil), events...)
		fork[0] = cursorEvent(t, 1, EvRunQueued, RunQueuedData{Agent: "branch", Kept: uint64(kept + 1)})
		checkCursorPrefixes(t, fork)
	}
}

func TestCursorIncrementalMixedPrefixes(t *testing.T) {
	// Include unusual but decodable histories: repeated approval IDs,
	// decisions after finish, report responses without an open report, and
	// malformed payloads. The reference defines their existing behavior.
	rng := rand.New(rand.NewPCG(7, 19))
	for trial := 0; trial < 30; trial++ {
		events := []Event{cursorEvent(t, 1, EvRunQueued, RunQueuedData{Kept: uint64(rng.IntN(30))})}
		for range 100 {
			id := fmt.Sprint(rng.IntN(4))
			var typ string
			var data any
			switch rng.IntN(17) {
			case 0:
				typ, data = EvModelRequested, ModelRequestedData{Report: rng.IntN(2) == 0}
			case 1, 2, 3:
				typ = EvModelResponded
				data = ModelRespondedData{StopReason: []StopReason{StopToolUse, StopMaxTokens, StopEndTurn}[rng.IntN(3)], Content: []Block{{Type: BlockToolUse, ID: id, Name: "tool", Input: json.RawMessage(`{}`)}}}
			case 4:
				typ, data = EvApprovalRequested, ApprovalRequestedData{ApprovalID: id, CallID: id}
			case 5:
				typ, data = EvApprovalRequested, ApprovalRequestedData{ApprovalID: id, CallID: id, Schema: json.RawMessage(answerSchema)}
			case 6:
				typ, data = EvApprovalRequested, ApprovalRequestedData{ApprovalID: id, CallID: id, Connect: &ConnectRequest{}}
			case 7:
				typ, data = EvApprovalDecided, ApprovalDecidedData{ApprovalID: id, Decision: []string{"allow", "deny"}[rng.IntN(2)]}
			case 8:
				typ, data = EvToolRequested, ToolRequestedData{CallID: id}
			case 9:
				typ, data = EvToolResponded, ToolRespondedData{CallID: id}
			case 10:
				typ, data = EvRunFinished, RunFinishedData{}
			case 11:
				typ, data = EvBudgetExceeded, BudgetExceededData{Report: true}
			case 12:
				typ, data = EvBudgetChanged, BudgetChangedData{}
			case 13:
				typ, data = EvMessageReceived, MessageReceivedData{Role: RoleSystem, Origin: OriginKernel, Note: []string{NoteLoop, NoteTime, NoteReport}[rng.IntN(3)]}
			case 14:
				typ, data = EvMessageReceived, MessageReceivedData{Role: RoleUser}
			case 15:
				typ, data = EvTurnStarted, TurnData{}
			case 16:
				typ, data = EvError, ErrorData{}
			}
			e := cursorEvent(t, len(events)+1, typ, data)
			if rng.IntN(10) == 0 {
				e.Data = json.RawMessage(`{"content":false,"stopReason":"max_tokens"}`)
			}
			events = append(events, e)
		}
		checkCursorPrefixes(t, events)
	}
}

func TestCursorLoopSuffix(t *testing.T) {
	rng := rand.New(rand.NewPCG(123, 456))
	for trial := 0; trial < 100; trial++ {
		var prints []uint64
		var suffix loopSuffix
		for i := range 1000 {
			v := uint64(rng.IntN(4))
			if trial < 3 {
				v = uint64(i % (trial + 1))
			}
			prints = append(prints, v)
			suffix.add(v)
			gp, gc := suffix.repeats()
			wp, wc := repeats(prints)
			if gp != wp || gc != wc {
				t.Fatalf("trial %d prefix %d: got (%d,%d), want (%d,%d)", trial, i+1, gp, gc, wp, wc)
			}
		}
	}
}

func TestCursorSnapshotIsolation(t *testing.T) {
	rs := &runState{}
	appendEvent := func(typ string, data any) {
		rs.emitMu.Lock()
		defer rs.emitMu.Unlock()
		rs.events = append(rs.events, cursorEvent(t, len(rs.events)+1, typ, data))
	}
	appendEvent(EvModelResponded, ModelRespondedData{StopReason: StopToolUse, Content: []Block{{Type: BlockToolUse, ID: "a"}}})
	appendEvent(EvApprovalRequested, ApprovalRequestedData{ApprovalID: "q", CallID: "a", Schema: json.RawMessage(answerSchema)})
	appendEvent(EvMessageReceived, MessageReceivedData{Origin: OriginKernel, Role: RoleSystem, Note: NoteBudget, Budget: &BudgetNoteData{Limit: "maxTurns", Max: 10, Percent: 75}})
	appendEvent(EvBudgetExceeded, BudgetExceededData{Report: true})
	first, boundary := rs.executionCursor()
	if boundary != 4 {
		t.Fatal(boundary)
	}
	raw := rs.eventLog()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		appendEvent(EvApprovalDecided, ApprovalDecidedData{ApprovalID: "q", Decision: "allow"})
		appendEvent(EvMessageReceived, MessageReceivedData{Origin: OriginKernel, Role: RoleSystem, Note: NoteBudget, Budget: &BudgetNoteData{Limit: "maxTurns", Max: 10, Percent: 90}})
		appendEvent(EvModelRequested, ModelRequestedData{Report: true})
		for range 100 {
			rs.executionCursor()
		}
	}()
	for range 100 {
		if first.calls["a"].questions[0].decided != nil || first.rep.reqSeq != 0 || first.noted[noteKey{limit: "maxTurns", max: 10}] != 75 {
			t.Fatal("snapshot changed")
		}
		if len(raw) != 4 || raw[3].Type != EvBudgetExceeded {
			t.Fatal("held event slice changed")
		}
	}
	wg.Wait()
	latest, _ := rs.executionCursor()
	if latest.calls["a"].questions[0].decided == nil || latest.rep.reqSeq == 0 || latest.noted[noteKey{limit: "maxTurns", max: 10}] != 90 {
		t.Fatal("new events were not consumed")
	}
	latest.calls["a"].responded = true
	latest.calls["a"].questions[0].decided = nil
	latest.rep.done = true
	next, _ := rs.executionCursor()
	if next.calls["a"].responded || next.calls["a"].questions[0].decided == nil || next.rep.done {
		t.Fatal("snapshot mutation reached cache")
	}
}

func TestCursorCacheReleaseReload(t *testing.T) {
	l := NewMemLedger()
	e := cursorEvent(t, 1, EvRunQueued, RunQueuedData{Input: "hello"})
	if _, err := l.Append(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	a := &Agent{ledger: l}
	rs := &runState{id: "cursor-test"}
	if err := a.holdEvents(rs); err != nil {
		t.Fatal(err)
	}
	c, n := rs.executionCursor()
	if c.queued.Input != "hello" || n != 1 {
		t.Fatal("cold reconstruction failed")
	}
	if err := a.holdEvents(rs); err != nil {
		t.Fatal(err)
	}
	rs.releaseEvents()
	if rs.exec == nil {
		t.Fatal("released a shared cache")
	}
	rs.releaseEvents()
	if rs.exec != nil || rs.execN != 0 || rs.events != nil {
		t.Fatal("cache retained after release")
	}
	e = cursorEvent(t, 2, EvTurnStarted, TurnData{})
	if _, err := l.Append(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if err := a.holdEvents(rs); err != nil {
		t.Fatal(err)
	}
	defer rs.releaseEvents()
	c, n = rs.executionCursor()
	if c.turns != 1 || n != 2 {
		t.Fatal("reload did not reconstruct new ledger state")
	}
}

func TestCursorReleasesApprovalPayloads(t *testing.T) {
	b := &cursorBuilder{}
	req := ApprovalRequestedData{ApprovalID: "q", CallID: "a", Input: json.RawMessage(`{"large":"payload"}`), Schema: json.RawMessage(answerSchema)}
	b.apply(cursorEvent(t, 1, EvApprovalRequested, req))
	b.apply(cursorEvent(t, 2, EvApprovalRequested, req))
	b.apply(cursorEvent(t, 3, EvApprovalDecided, ApprovalDecidedData{ApprovalID: "q", Decision: "allow"}))
	if b.approvals["q"].req == nil || b.c.pending == nil {
		t.Fatal("duplicate open ID lost its payload")
	}
	b.apply(cursorEvent(t, 4, EvApprovalDecided, ApprovalDecidedData{ApprovalID: "q", Decision: "allow"}))
	if b.approvals["q"].req != nil || b.c.pending != nil {
		t.Fatal("decided approval retained its payload")
	}
	b.apply(cursorEvent(t, 5, EvApprovalRequested, req))
	b.apply(cursorEvent(t, 6, EvRunFinished, RunFinishedData{}))
	if b.approvals["q"].req != nil || b.c.pending != nil {
		t.Fatal("finished run retained an open approval payload")
	}
	if link := b.approvals["q"]; link.callID != "a" || !link.question {
		t.Fatal("lost late-decision link")
	}
}

func TestCursorToolInputIsolation(t *testing.T) {
	const original = `{"x":1}`
	tool := NewTool(ToolSpec{Name: "mutate", ReadOnly: true}, func(ctx context.Context, input json.RawMessage) (ToolResult, error) {
		input[0] = '['
		a := AgentFrom(ctx)
		rs, err := a.k.lookup(RunInfoFrom(ctx).RunID)
		if err != nil {
			return ToolResult{}, err
		}
		c, _ := rs.executionCursor()
		if got := string(toolUses(c.resp.Content)[0].Input); got != original {
			t.Errorf("tool changed cached input: %q", got)
		}
		return TextResult("done"), nil
	})
	a := newTestAgent(t, newScript(callTools(use("a", "mutate", original)), endTurn("done")).Model(), WithTools(tool))
	r, err := a.Run(testCtx(t), "go")
	mustState(t, r, err, StateCompleted)
	q := &questionRecord{req: ApprovalRequestedData{Input: json.RawMessage(`{}`), Schema: json.RawMessage(answerSchema)}, decided: &ApprovalDecidedData{Content: json.RawMessage(`{}`)}}
	answers, ok := continueAsk(&continuerTool{}, &callRecord{questions: []*questionRecord{q}})
	if !ok {
		t.Fatal("recorded answer was not continued")
	}
	answers[0].Request.Input[0] = '['
	answers[0].Request.Schema[0] = '['
	answers[0].Decision.Content[0] = '['
	if q.req.Input[0] != '{' || q.req.Schema[0] != '{' || q.decided.Content[0] != '{' {
		t.Fatal("continuation changed cached question payloads")
	}
}

func BenchmarkCursorIncremental(b *testing.B) {
	for _, turns := range []int{250, 500, 1000, 2000} {
		b.Run(fmt.Sprint(turns), func(b *testing.B) {
			events := make([]Event, 0, turns*5)
			for i := range turns {
				for _, entry := range []struct {
					typ  string
					data any
				}{
					{EvTurnStarted, TurnData{Turn: i + 1}},
					{EvModelRequested, ModelRequestedData{}},
					{EvModelResponded, ModelRespondedData{StopReason: StopPauseTurn, Content: []Block{Text("step")}}},
					{EvTurnFinished, TurnData{Turn: i + 1}},
					{EvContextCompacted, ContextCompactedData{}},
				} {
					events = append(events, cursorEvent(b, len(events)+1, entry.typ, entry.data))
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				rs := &runState{events: events[:0:len(events)]}
				// The driver reads before/after model and response steps.
				for end := 5; end <= len(events); end += 5 {
					rs.events = events[:end:len(events)]
					for range 3 {
						rs.executionCursor()
					}
				}
			}
		})
	}
}
