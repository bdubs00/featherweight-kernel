package fw

import (
	"errors"
	"strings"
	"testing"
)

// Annotate merges metadata into a finished run, counts the usage of the
// model call that produced it, and opens the run again in no way.
func TestAnnotate(t *testing.T) {
	ctx := testCtx(t)
	// A closed configuration: alice, an admin, starts the run, and bob can
	// do nothing with it.
	cfg := DefaultConfig()
	cfg.Access.Default = "closed"
	cfg.Access.Admins = []string{"alice"}
	alice := &Principal{Subject: "alice", Kind: KindUser}
	a := newTestAgent(t, newScript(endTurn("done"), endTurn("done")).Model(), WithConfig(cfg))
	r, err := a.Run(WithPrincipal(ctx, alice), "Investigate the intermittent checkout failure", WithMetadata(map[string]string{"team": "pay"}))
	mustState(t, r, err, StateCompleted)
	if r.Title != "Investigate the intermittent checkout failure" {
		t.Fatalf("title from the input = %q", r.Title)
	}
	before := r.Usage
	err = a.Annotate(ctx, r.ID, RunAnnotatedData{
		Metadata: map[string]string{MetaTitle: "Fix checkout retries", "label": "p1"},
		Usage:    &Usage{InputTokens: 30, OutputTokens: 4}, Provider: "fake", Model: "t",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.Get(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateCompleted || got.Title != "Fix checkout retries" || got.Metadata["label"] != "p1" || got.Metadata["team"] != "pay" {
		t.Fatalf("run after the annotation = %+v", got)
	}
	if got.Usage.InputTokens != before.InputTokens+30 || got.Usage.OutputTokens != before.OutputTokens+4 {
		t.Fatalf("usage = %+v, before %+v", got.Usage, before)
	}
	var d RunAnnotatedData
	firstOf(t, ledgerEvents(t, a.Ledger(), r.ID), EvRunAnnotated, &d)
	if d.Metadata[MetaTitle] != "Fix checkout retries" || d.Usage == nil || d.Provider != "fake" {
		t.Fatalf("annotated = %+v", d)
	}
	// Usage alone is an annotation too: a model call that wrote no title
	// still costs, and the ledger keeps the cost.
	if err := a.Annotate(ctx, r.ID, RunAnnotatedData{Usage: &Usage{InputTokens: 1}}); err != nil {
		t.Fatal(err)
	}
	// Nothing, the idempotency key, and metadata out of bounds are refused.
	for _, d := range []RunAnnotatedData{
		{},
		{Metadata: map[string]string{metaIdempotencyKey: "x"}},
		{Metadata: map[string]string{"big": strings.Repeat("x", MaxMetadataValueBytes+1)}},
	} {
		if err := a.Annotate(ctx, r.ID, d); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("Annotate(%+v) = %v, want ErrInvalidRequest", d, err)
		}
	}
	if err := a.Annotate(ctx, "nope", RunAnnotatedData{Metadata: map[string]string{"a": "b"}}); !errors.Is(err, ErrRunNotFound) {
		t.Errorf("unknown run = %v", err)
	}
	// The person who started the run annotates it, and a stranger cannot.
	if err := a.Annotate(WithPrincipal(ctx, alice), r.ID, RunAnnotatedData{Metadata: map[string]string{"a": "b"}}); err != nil {
		t.Errorf("alice = %v", err)
	}
	bob := &Principal{Subject: "bob", Kind: KindUser}
	if err := a.Annotate(WithPrincipal(ctx, bob), r.ID, RunAnnotatedData{Metadata: map[string]string{"a": "b"}}); !errors.Is(err, ErrForbidden) {
		t.Errorf("bob = %v, want ErrForbidden", err)
	}
}

// A run that finished and then took an annotation stays finished across a
// restart: the rebuild reads the state of the run, not its last event.
func TestAnnotatedRunIsNotResumed(t *testing.T) {
	ctx := testCtx(t)
	ledger := NewMemLedger()
	a := newTestAgent(t, newScript(endTurn("done")).Model(), WithLedger(ledger))
	r, err := a.Run(ctx, "Do the work")
	mustState(t, r, err, StateCompleted)
	if err := a.Annotate(ctx, r.ID, RunAnnotatedData{Metadata: map[string]string{MetaTitle: "Work"}}); err != nil {
		t.Fatal(err)
	}
	a.Close(ctx)

	b := newTestAgent(t, newScript().Model(), WithLedger(ledger))
	got, err := b.Get(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateCompleted || got.Title != "Work" {
		t.Fatalf("run after the restart = %+v", got)
	}
	for _, e := range ledgerEvents(t, ledger, r.ID) {
		if e.Type == EvRunResumed {
			t.Fatal("the finished run was resumed")
		}
	}
}
