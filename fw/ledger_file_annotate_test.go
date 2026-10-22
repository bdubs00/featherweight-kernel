package fw

import (
	"context"
	"testing"
)

// The file ledger releases the file of a finished run, and an annotation
// that arrives after the end leaves it released. A later event that works
// on the run again keeps the file open. A ledger that opens the files anew
// reads the same state from them.
func TestFileLedgerClosesAfterLateAnnotation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	l, err := OpenFileLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := NewID()
	append := func(l *FileLedger, typ string, data any) {
		t.Helper()
		e, err := NewEvent(id, typ, data)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := l.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	open := func(l *FileLedger) bool {
		r := l.run(id)
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.f != nil
	}
	append(l, EvRunQueued, RunQueuedData{Agent: "default", Input: "Do the work"})
	if !open(l) {
		t.Fatal("the file of a run that works is closed")
	}
	append(l, EvRunFinished, RunFinishedData{Outcome: OutcomeCompleted})
	if open(l) {
		t.Fatal("the file of a finished run stays open")
	}
	append(l, EvRunAnnotated, RunAnnotatedData{Metadata: map[string]string{MetaTitle: "Work"}})
	if open(l) {
		t.Fatal("an annotation after the end left the file open")
	}

	// A new ledger over the same files knows that the run is done.
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l2, err := OpenFileLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	append(l2, EvRunAnnotated, RunAnnotatedData{Usage: &Usage{InputTokens: 1}})
	if open(l2) {
		t.Fatal("an annotation after a restart left the file open")
	}
	// An event that works on the run again keeps the file open.
	append(l2, EvRunResumed, RunResumedData{FromSeq: 4})
	if !open(l2) {
		t.Fatal("a run that opened again has no open file")
	}
	if err := l2.Close(); err != nil {
		t.Fatal(err)
	}
}

// The file ledger keeps the file of a finished run released after a
// reaction too, before and after a restart, and the reaction reaches the
// disk before Append returns.
func TestFileLedgerClosesAfterLateReaction(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	l, err := OpenFileLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := NewID()
	append := func(l *FileLedger, typ string, data any) {
		t.Helper()
		e, err := NewEvent(id, typ, data)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := l.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	open := func(l *FileLedger) bool {
		r := l.run(id)
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.f != nil
	}
	append(l, EvRunQueued, RunQueuedData{Agent: "default", Input: "Do the work"})
	append(l, EvRunFinished, RunFinishedData{Outcome: OutcomeCompleted})
	append(l, EvRunReacted, ReactionData{Seq: 2, Reaction: ReactionUp, By: "alice"})
	if open(l) {
		t.Fatal("a reaction after the end left the file open")
	}
	if !needsIntentSync(EvRunReacted, nil) {
		t.Fatal("a reaction does not reach the disk before Append returns")
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l2, err := OpenFileLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	append(l2, EvRunReacted, ReactionData{Reaction: ReactionHeart, By: "alice"})
	if open(l2) {
		t.Fatal("a reaction after a restart left the file open")
	}
	if err := l2.Close(); err != nil {
		t.Fatal(err)
	}
}
