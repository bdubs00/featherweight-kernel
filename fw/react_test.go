package fw

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
)

// reactionsOf returns the data of every fw.run.reacted of a run.
func reactionsOf(t *testing.T, l Ledger, run string) []ReactionData {
	t.Helper()
	var out []ReactionData
	for _, e := range ledgerEvents(t, l, run) {
		var d ReactionData
		if e.Type == EvRunReacted && e.Decode(&d) == nil {
			out = append(out, d)
		}
	}
	return out
}

// receiptsOf returns the quality receipts of a run.
func receiptsOf(t *testing.T, l Ledger, run string) []QualityObservation {
	t.Helper()
	var out []QualityObservation
	for _, e := range ledgerEvents(t, l, run) {
		var d QualityAnnotatedData
		if e.Type == EvRunAnnotated && e.Decode(&d) == nil && d.Quality != nil {
			out = append(out, *d.Quality)
		}
	}
	return out
}

// finishSeq returns the sequence number of the latest fw.run.finished.
func finishSeq(t *testing.T, l Ledger, run string) uint64 {
	t.Helper()
	var seq uint64
	for _, e := range ledgerEvents(t, l, run) {
		if e.Type == EvRunFinished {
			seq = e.Seq
		}
	}
	if seq == 0 {
		t.Fatalf("run %s has no fw.run.finished", run)
	}
	return seq
}

// React records a reaction on a finished run, counts it on the run object,
// and leaves the run finished. A thumb on the end of a completed run is a
// quality receipt once per person and result; a thumb that conflicts with
// a receipt still records, without one.
func TestReact(t *testing.T) {
	ctx := testCtx(t)
	cfg := DefaultConfig()
	cfg.Access.Default = "closed"
	cfg.Access.Admins = []string{"alice", "bob"}
	alice := WithPrincipal(ctx, &Principal{Subject: "alice", Kind: KindUser})
	bob := WithPrincipal(ctx, &Principal{Subject: "bob", Kind: KindUser})
	a := newTestAgent(t, newScript(endTurn("done")).Model(), WithConfig(cfg))
	r, err := a.Run(alice, "Do the work")
	mustState(t, r, err, StateCompleted)
	fin := finishSeq(t, a.Ledger(), r.ID)

	if err := a.React(alice, r.ID, ReactionData{Reaction: ReactionHeart, Note: "Nice work."}); err != nil {
		t.Fatal(err)
	}
	if err := a.React(alice, r.ID, ReactionData{Seq: fin, Reaction: ReactionUp}); err != nil {
		t.Fatal(err)
	}
	got := reactionsOf(t, a.Ledger(), r.ID)
	if len(got) != 2 || got[0] != (ReactionData{Reaction: ReactionHeart, By: "alice", Note: "Nice work."}) ||
		got[1] != (ReactionData{Seq: fin, Reaction: ReactionUp, By: "alice", Receipt: true}) {
		t.Fatalf("reactions = %+v", got)
	}
	receipts := receiptsOf(t, a.Ledger(), r.ID)
	want := QualityObservation{ID: reactionReceipt(r.ID, fin, "alice"), Completion: fin, Verification: "accepted"}
	if len(receipts) != 1 || receipts[0] != want || want.ID != "reaction:"+r.ID+":"+strconv.FormatUint(fin, 10)+":alice" {
		t.Fatalf("receipts = %+v, want %+v", receipts, want)
	}

	// The same thumb again appends nothing, also with another note.
	if err := a.React(alice, r.ID, ReactionData{Seq: fin, Reaction: ReactionUp, Note: "again"}); err != nil {
		t.Fatal(err)
	}
	if n, m := len(reactionsOf(t, a.Ledger(), r.ID)), len(receiptsOf(t, a.Ledger(), r.ID)); n != 2 || m != 1 {
		t.Fatalf("a second thumb appended: %d reactions, %d receipts", n, m)
	}
	// A thumb down of alice conflicts with her receipt, and a thumb of bob
	// finds the completion verified: both record without a receipt.
	if err := a.React(alice, r.ID, ReactionData{Seq: fin, Reaction: ReactionDown}); err != nil {
		t.Fatal(err)
	}
	if err := a.React(bob, r.ID, ReactionData{Seq: fin, Reaction: ReactionUp}); err != nil {
		t.Fatal(err)
	}
	got = reactionsOf(t, a.Ledger(), r.ID)
	if len(got) != 4 || got[2].Receipt || got[2].By != "alice" || got[3].Receipt || got[3].By != "bob" {
		t.Fatalf("reactions after the conflicts = %+v", got)
	}
	if n := len(receiptsOf(t, a.Ledger(), r.ID)); n != 1 {
		t.Fatalf("%d receipts after the conflicts, want 1", n)
	}
	// A thumb on an event that is not the end of a completed run is no
	// receipt.
	if err := a.React(bob, r.ID, ReactionData{Seq: 1, Reaction: ReactionDown}); err != nil {
		t.Fatal(err)
	}
	if got := reactionsOf(t, a.Ledger(), r.ID); got[4].Receipt || len(receiptsOf(t, a.Ledger(), r.ID)) != 1 {
		t.Fatalf("a thumb on fw.run.queued = %+v", got[4])
	}

	run, err := a.Get(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != StateCompleted || run.Output != "done" || len(run.Reactions) != 3 ||
		run.Reactions[ReactionHeart] != 1 || run.Reactions[ReactionUp] != 2 || run.Reactions[ReactionDown] != 2 {
		t.Fatalf("run after the reactions = %+v", run)
	}
}

// A person takes a reaction back: the removal appends one event, takes one
// from the count, and keeps the receipt that the thumb recorded. A removal
// of a reaction that the person does not have appends nothing, and the
// person can react again after a removal.
func TestReactRemove(t *testing.T) {
	ctx := testCtx(t)
	cfg := DefaultConfig()
	cfg.Access.Default = "closed"
	cfg.Access.Admins = []string{"alice", "bob"}
	alice := WithPrincipal(ctx, &Principal{Subject: "alice", Kind: KindUser})
	bob := WithPrincipal(ctx, &Principal{Subject: "bob", Kind: KindUser})
	a := newTestAgent(t, newScript(endTurn("done")).Model(), WithConfig(cfg))
	r, err := a.Run(alice, "Do the work")
	mustState(t, r, err, StateCompleted)
	fin := finishSeq(t, a.Ledger(), r.ID)
	count := func() map[string]int {
		t.Helper()
		run, err := a.Get(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		return run.Reactions
	}

	// Nothing to take back appends nothing.
	if err := a.React(alice, r.ID, ReactionData{Seq: fin, Reaction: ReactionUp, Removed: true}); err != nil {
		t.Fatal(err)
	}
	if n := len(reactionsOf(t, a.Ledger(), r.ID)); n != 0 {
		t.Fatalf("a removal of nothing appended %d events", n)
	}
	for _, c := range []struct {
		ctx context.Context
		d   ReactionData
	}{
		{alice, ReactionData{Seq: fin, Reaction: ReactionUp}},
		{bob, ReactionData{Seq: fin, Reaction: ReactionUp}},
		{alice, ReactionData{Seq: fin, Reaction: ReactionUp, Note: "gone", Removed: true}},
		{alice, ReactionData{Seq: fin, Reaction: ReactionUp, Removed: true}},
	} {
		if err := a.React(c.ctx, r.ID, c.d); err != nil {
			t.Fatal(err)
		}
	}
	got := reactionsOf(t, a.Ledger(), r.ID)
	if len(got) != 3 || got[2] != (ReactionData{Seq: fin, Reaction: ReactionUp, By: "alice", Removed: true}) {
		t.Fatalf("reactions = %+v", got)
	}
	if c := count(); len(c) != 1 || c[ReactionUp] != 1 {
		t.Fatalf("counts after a removal = %v", c)
	}
	if n := len(receiptsOf(t, a.Ledger(), r.ID)); n != 1 {
		t.Fatalf("%d receipts after a removal, want the first one", n)
	}
	// Alice reacts again, and bob takes his back: the count of up is the
	// one of alice, and a count that reaches zero leaves the map.
	if err := a.React(alice, r.ID, ReactionData{Seq: fin, Reaction: ReactionUp}); err != nil {
		t.Fatal(err)
	}
	if err := a.React(bob, r.ID, ReactionData{Seq: fin, Reaction: ReactionUp, Removed: true}); err != nil {
		t.Fatal(err)
	}
	if err := a.React(alice, r.ID, ReactionData{Seq: fin, Reaction: ReactionUp, Removed: true}); err != nil {
		t.Fatal(err)
	}
	if c := count(); c != nil {
		t.Fatalf("counts after every removal = %v", c)
	}
	if n := len(reactionsOf(t, a.Ledger(), r.ID)); n != 6 {
		t.Fatalf("%d reaction events, want 6", n)
	}
}

// React refuses what it cannot record, and only a trusted caller names
// another person.
func TestReactRefusals(t *testing.T) {
	ctx := testCtx(t)
	cfg := DefaultConfig()
	cfg.Access.Default = "closed"
	cfg.Access.Admins = []string{"alice"}
	alice := WithPrincipal(ctx, &Principal{Subject: "alice", Kind: KindUser})
	a := newTestAgent(t, newScript(endTurn("done")).Model(), WithConfig(cfg))
	r, err := a.Run(alice, "Do the work")
	mustState(t, r, err, StateCompleted)
	last := r.Seq
	for _, d := range []ReactionData{
		{Reaction: "fire"},
		{Reaction: ""},
		{Seq: last + 1, Reaction: ReactionHeart},
		{Reaction: ReactionHeart, Note: strings.Repeat("x", MaxReactionNoteBytes+1)},
		{Reaction: ReactionHeart, By: "two\nlines"},
	} {
		if err := a.React(ctx, r.ID, d); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("React(%+v) = %v, want ErrInvalidRequest", d, err)
		}
	}
	if err := a.React(ctx, "nope", ReactionData{Reaction: ReactionHeart}); !errors.Is(err, ErrRunNotFound) {
		t.Errorf("unknown run = %v", err)
	}
	if err := a.React(alice, r.ID, ReactionData{Reaction: ReactionHeart, By: "carol"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("a person reacted for another person: %v", err)
	}
	stranger := WithPrincipal(ctx, &Principal{Subject: "mallory", Kind: KindUser})
	if err := a.React(stranger, r.ID, ReactionData{Reaction: ReactionHeart}); !errors.Is(err, ErrForbidden) {
		t.Errorf("a stranger reacted: %v", err)
	}
	if n := len(reactionsOf(t, a.Ledger(), r.ID)); n != 0 {
		t.Fatalf("a refused reaction is in the ledger: %d", n)
	}
	// The process itself and a trusted caller name the person.
	if err := a.React(ctx, r.ID, ReactionData{Seq: last, Reaction: ReactionHeart, By: "carol"}); err != nil {
		t.Fatal(err)
	}
	if got := reactionsOf(t, a.Ledger(), r.ID); len(got) != 1 || got[0].By != "carol" || got[0].Seq != last {
		t.Fatalf("reactions = %+v", got)
	}
}

// A run that finished and then took a reaction stays finished across a
// restart: the rebuild reads the state of the run, not its last event, and
// counts the reactions again.
func TestReactedRunIsNotResumed(t *testing.T) {
	ctx := testCtx(t)
	ledger := NewMemLedger()
	a := newTestAgent(t, newScript(endTurn("done")).Model(), WithLedger(ledger))
	r, err := a.Run(ctx, "Do the work")
	mustState(t, r, err, StateCompleted)
	if err := a.React(ctx, r.ID, ReactionData{Seq: finishSeq(t, ledger, r.ID), Reaction: ReactionUp, By: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := a.React(ctx, r.ID, ReactionData{Reaction: ReactionHeart, By: "alice"}); err != nil {
		t.Fatal(err)
	}
	a.Close(ctx)

	b := newTestAgent(t, newScript().Model(), WithLedger(ledger))
	got, err := b.Get(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateCompleted || got.Reactions[ReactionUp] != 1 || got.Reactions[ReactionHeart] != 1 {
		t.Fatalf("run after the restart = %+v", got)
	}
	for _, e := range ledgerEvents(t, ledger, r.ID) {
		if e.Type == EvRunResumed {
			t.Fatal("the finished run was resumed")
		}
	}
	// The event stream of the finished run ends at once, as before the
	// reaction.
	evs, err := b.Events(ctx, r.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range evs {
		n++
	}
	if n != len(ledgerEvents(t, ledger, r.ID)) {
		t.Fatalf("the stream gave %d events", n)
	}
}

// A copy of a run object keeps its count when the run takes a reaction.
func TestReactionCountIsCopied(t *testing.T) {
	r := &Run{}
	r.Apply(MustEvent("R", EvRunReacted, ReactionData{Reaction: ReactionHeart}), nil)
	cp := cloneRun(r)
	shallow := *r
	r.Apply(MustEvent("R", EvRunReacted, ReactionData{Reaction: ReactionHeart}), nil)
	if r.Reactions[ReactionHeart] != 2 || cp.Reactions[ReactionHeart] != 1 || shallow.Reactions[ReactionHeart] != 1 {
		t.Fatalf("counts = %v, %v, %v", r.Reactions, cp.Reactions, shallow.Reactions)
	}
}
