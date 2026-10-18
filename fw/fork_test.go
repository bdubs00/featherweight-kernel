package fw

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// A branch keeps the events of its source to a point and goes on from
// there; the source does not change.
func TestForkContinuesFromThePrefix(t *testing.T) {
	a := newTestAgent(t, newScript(endTurn("first"), endTurn("second"), endTurn("third")).Model())
	ctx := testCtx(t)
	src, err := a.Start(ctx, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if src, err = a.WaitFor(ctx, src.ID, func(r *Run) bool { return r.State.Terminal() }); err != nil || src.Output != "first" {
		t.Fatalf("source = %+v, %v", src, err)
	}
	evs, err := readAll(ctx, a.ledger, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := evs[len(evs)-1].Seq

	// A branch at the end with a new message goes on in another direction.
	b, err := a.Fork(ctx, src.ID, ForkRequest{Input: "and then?", Origin: OriginHTTP})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if b.ID == src.ID || b.Metadata[MetaForkOf] != src.ID || b.Metadata[MetaForkSeq] != strconv.FormatUint(last, 10) {
		t.Fatalf("branch = %+v", b)
	}
	if b, err = a.WaitFor(ctx, b.ID, func(r *Run) bool { return r.State.Terminal() }); err != nil || b.Output != "second" {
		t.Fatalf("branch = %+v, %v", b, err)
	}
	bevs, err := readAll(ctx, a.ledger, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(bevs) <= len(evs) {
		t.Fatalf("the branch has %d events, the source %d; the branch must hold the prefix and its own events", len(bevs), len(evs))
	}
	for i, e := range evs {
		if bevs[i].Type != e.Type || bevs[i].Subject != b.ID || bevs[i].ID == e.ID {
			t.Fatalf("event %d of the branch = %s %s %s; source %s %s", i, bevs[i].Type, bevs[i].Subject, bevs[i].ID, e.Type, e.ID)
		}
	}
	if again, err := a.Get(ctx, src.ID); err != nil || again.Output != "first" {
		t.Fatalf("the source changed: %+v, %v", again, err)
	}

	// A branch before the answer of the model asks the model again.
	var started uint64
	for _, e := range evs {
		if e.Type == EvRunStarted {
			started = e.Seq
		}
	}
	c, err := a.Fork(ctx, src.ID, ForkRequest{Seq: started})
	if err != nil {
		t.Fatalf("Fork at %d: %v", started, err)
	}
	if c, err = a.WaitFor(ctx, c.ID, func(r *Run) bool { return r.State.Terminal() }); err != nil || c.Output != "third" {
		t.Fatalf("early branch = %+v, %v", c, err)
	}
	if c.Metadata[MetaForkSeq] != strconv.FormatUint(started, 10) {
		t.Fatalf("early branch metadata = %v", c.Metadata)
	}
	if list, _ := a.List(ctx); len(list) != 3 {
		t.Fatalf("runs = %d, want the source and two branches", len(list))
	}

	// Errors: a sequence number beyond the ledger, and an unknown run.
	if _, err := a.Fork(ctx, src.ID, ForkRequest{Seq: last + 100}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Fork beyond the ledger = %v", err)
	}
	if _, err := a.Fork(ctx, "01NOPE", ForkRequest{}); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("Fork of an unknown run = %v", err)
	}
}

// With spawn, a branch runs in another process through a Forker: the other
// process gets the kept events, the source records the branch as a child,
// and Get reaches the branch on its member.
func TestForkInAnotherProcess(t *testing.T) {
	other := newTestAgent(t, newScript(endTurn("elsewhere")).Model())
	sp := &otherSpawner{other: other}
	a := newTestAgent(t, newScript(endTurn("here")).Model(), WithSpawner(sp))
	ctx := testCtx(t)
	src, err := a.Start(ctx, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.WaitFor(ctx, src.ID, func(r *Run) bool { return r.State.Terminal() }); err != nil {
		t.Fatal(err)
	}
	b, err := a.Fork(ctx, src.ID, ForkRequest{Input: "try again elsewhere", Spawn: true})
	if err != nil {
		t.Fatalf("Fork with spawn: %v", err)
	}
	if b.Member != "other" || b.Metadata[MetaForkOf] != src.ID || b.Parent != src.ID || b.ID == "" {
		t.Fatalf("branch = %+v", b)
	}
	// The source answers for the branch until the other process has it.
	if r, err := a.Get(ctx, b.ID); err != nil || r.ID != b.ID || r.Member != "other" {
		t.Fatalf("Get of the branch while it starts = %+v, %v", r, err)
	}
	got, err := a.WaitFor(ctx, b.ID, func(r *Run) bool { return r.State.Terminal() })
	if err != nil || got.Output != "elsewhere" || got.Member != "other" {
		t.Fatalf("branch in the other process = %+v, %v", got, err)
	}
	if kids, err := a.Children(ctx, src.ID); err != nil || len(kids) != 1 || kids[0].ID != b.ID {
		t.Fatalf("children of the source = %+v, %v", kids, err)
	}
	evs, _ := readAll(ctx, a.ledger, src.ID)
	recorded := false
	for _, e := range evs {
		var d RunChildData
		if e.Type == EvRunChild && e.Decode(&d) == nil && d.Child == b.ID && d.Member == "other" {
			recorded = true
		}
	}
	if !recorded {
		t.Fatal("the source did not record the branch as a child")
	}
	if list, _ := other.List(ctx); len(list) != 1 || list[0].ID != b.ID || list[0].Output != "elsewhere" || list[0].Metadata[MetaForkOf] != src.ID {
		t.Fatalf("the other process holds %+v", list)
	}
	// The same branch sent again is the branch that the other process has.
	evs, _ = readAll(ctx, other.ledger, b.ID)
	again, err := other.ForkEvents(ctx, evs[:1], ForkRequest{ID: b.ID})
	if err != nil || again.ID != b.ID {
		t.Fatalf("the branch sent again = %+v, %v", again, err)
	}
	if list, _ := other.List(ctx); len(list) != 1 {
		t.Fatalf("the other process holds %d runs after the repeat", len(list))
	}
}

// A branch whose process never takes it is recorded in the source process
// as failed, with the reason, and the source stops counting it as a child
// in another process.
func TestForkInAnotherProcessThatFails(t *testing.T) {
	other := newTestAgent(t, newScript(endTurn("elsewhere")).Model())
	sp := &otherSpawner{other: other, branchErr: errors.New("the Task is Pending after 10m")}
	a := newTestAgent(t, newScript(endTurn("here")).Model(), WithSpawner(sp))
	ctx := testCtx(t)
	src, err := a.Start(ctx, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.WaitFor(ctx, src.ID, func(r *Run) bool { return r.State.Terminal() }); err != nil {
		t.Fatal(err)
	}
	b, err := a.Fork(ctx, src.ID, ForkRequest{Input: "elsewhere", Spawn: true})
	if err != nil {
		t.Fatalf("Fork with spawn: %v", err)
	}
	got, err := a.WaitFor(ctx, b.ID, func(r *Run) bool { return r.State.Terminal() })
	if err != nil || got.State != StateFailed || !strings.Contains(got.Error, "did not start in other") || !strings.Contains(got.Error, "Pending after 10m") {
		t.Fatalf("branch that did not start = %+v, %v", got, err)
	}
	if got.Metadata[MetaForkOf] != src.ID || got.Member != "" {
		t.Fatalf("the record of the branch = %+v", got)
	}
	if list, _ := other.List(ctx); len(list) != 0 {
		t.Fatalf("the other process holds %+v", list)
	}
	if _, ok := a.remoteChild(b.ID); ok {
		t.Fatal("the source still counts the branch as a child in another process")
	}
	// Place that fails is an error of Fork itself, and nothing is recorded.
	sp.placeErr = errors.New("no room")
	if _, err := a.Fork(ctx, src.ID, ForkRequest{Spawn: true}); err == nil || !strings.Contains(err.Error(), "no room") {
		t.Fatalf("Fork with a Place that fails = %v", err)
	}
}

// Cancel during the start of a branch ends the start, and the branch is
// recorded as canceled.
func TestForkInAnotherProcessCanceledWhileItStarts(t *testing.T) {
	other := newTestAgent(t, newScript(endTurn("elsewhere")).Model())
	sp := &otherSpawner{other: other, branchWait: make(chan struct{})}
	a := newTestAgent(t, newScript(endTurn("here")).Model(), WithSpawner(sp))
	ctx := testCtx(t)
	src, err := a.Start(ctx, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.WaitFor(ctx, src.ID, func(r *Run) bool { return r.State.Terminal() }); err != nil {
		t.Fatal(err)
	}
	b, err := a.Fork(ctx, src.ID, ForkRequest{Input: "elsewhere", Spawn: true})
	if err != nil {
		t.Fatalf("Fork with spawn: %v", err)
	}
	if r, err := a.Get(ctx, b.ID); err != nil || r.State != StateQueued {
		t.Fatalf("the branch while it starts = %+v, %v", r, err)
	}
	if a.Starting() != 1 {
		t.Fatalf("Starting = %d while the branch starts", a.Starting())
	}
	if err := a.Decide(ctx, b.ID, ApprovalDecision{ApprovalID: "x", Decision: "allow"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Decide while the branch starts = %v", err)
	}
	if err := a.Cancel(ctx, b.ID); err != nil {
		t.Fatalf("Cancel while the branch starts: %v", err)
	}
	got, err := a.Get(ctx, b.ID)
	if err != nil || got.State != StateCanceled {
		t.Fatalf("the canceled branch = %+v, %v", got, err)
	}
	if list, _ := other.List(ctx); len(list) != 0 {
		t.Fatalf("the other process holds %+v", list)
	}
	if a.Starting() != 0 {
		t.Fatalf("Starting = %d after the start ended", a.Starting())
	}
}

// A branch from events needs a fw.run.queued event first.
func TestForkEventsNeedsAQueuedEvent(t *testing.T) {
	a := newTestAgent(t, newScript(endTurn("x")).Model())
	if _, err := a.ForkEvents(testCtx(t), []Event{{Type: EvRunStarted}}, ForkRequest{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("ForkEvents without queued = %v", err)
	}
}
