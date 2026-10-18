package fw

import (
	"errors"
	"strconv"
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
	if b.Member != "other" || b.Metadata[MetaForkOf] != src.ID {
		t.Fatalf("branch = %+v", b)
	}
	got, err := a.WaitFor(ctx, b.ID, func(r *Run) bool { return r.State.Terminal() })
	if err != nil || got.Output != "elsewhere" {
		t.Fatalf("branch in the other process = %+v, %v", got, err)
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
	if list, _ := other.List(ctx); len(list) != 1 || list[0].Output != "elsewhere" || list[0].Metadata[MetaForkOf] != src.ID {
		t.Fatalf("the other process holds %+v", list)
	}
}

// A branch from events needs a fw.run.queued event first.
func TestForkEventsNeedsAQueuedEvent(t *testing.T) {
	a := newTestAgent(t, newScript(endTurn("x")).Model())
	if _, err := a.ForkEvents(testCtx(t), []Event{{Type: EvRunStarted}}, ForkRequest{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("ForkEvents without queued = %v", err)
	}
}
