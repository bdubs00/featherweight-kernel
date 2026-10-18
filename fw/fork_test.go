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
