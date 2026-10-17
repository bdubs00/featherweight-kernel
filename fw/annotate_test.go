package fw

import (
	"context"
	"strings"
	"testing"
)

func TestRunAnnotators(t *testing.T) {
	ctx := testCtx(t)
	var seen []string
	note := func(ctx context.Context, cfg *Config, req *RunRequest, parent *Run) map[string]string {
		p := PrincipalFrom(ctx)
		if p == nil {
			if parent == nil {
				return nil
			}
			// A child takes the note of its parent.
			return map[string]string{"note": parent.Metadata["note"]}
		}
		seen = append(seen, req.Input)
		return map[string]string{"note": "by " + p.Subject, "fixed": "annotator"}
	}
	big := func(context.Context, *Config, *RunRequest, *Run) map[string]string {
		return map[string]string{"huge": strings.Repeat("x", MaxMetadataValueBytes+1)}
	}
	a := newTestAgent(t, newScript(endTurn("one"), endTurn("two")).Model(), WithRunAnnotators(note, big))
	alice := &Principal{Subject: "alice", Kind: KindUser}
	// The request wins over the annotator, the annotator adds what the
	// request lacks, and an annotator out of bounds adds nothing.
	r, err := a.Run(WithPrincipal(ctx, alice), "hi", WithMetadata(map[string]string{"fixed": "request"}))
	mustState(t, r, err, StateCompleted)
	if r.Metadata["note"] != "by alice" || r.Metadata["fixed"] != "request" || r.Metadata["huge"] != "" {
		t.Fatalf("metadata = %v", r.Metadata)
	}
	if len(seen) != 1 || seen[0] != "hi" {
		t.Fatalf("the annotator saw %v", seen)
	}
	// A child run gets the parent run, so an annotator can carry a note
	// down the tree.
	child, err := a.Run(ctx, "sub", WithParent(r.ID))
	mustState(t, child, err, StateCompleted)
	if child.Metadata["note"] != "by alice" {
		t.Fatalf("child metadata = %v", child.Metadata)
	}
}

func TestRegisteredRunAnnotators(t *testing.T) {
	ctx := testCtx(t)
	RegisterRunAnnotator(func(context.Context, *Config, *RunRequest, *Run) map[string]string {
		return map[string]string{"registered": "yes"}
	})
	t.Cleanup(func() {
		registry.mu.Lock()
		defer registry.mu.Unlock()
		registry.annotators = nil
	})
	// A test agent passes WithoutDefaults, which leaves the registered
	// annotators out.
	a := newTestAgent(t, newScript(endTurn("one")).Model())
	r, err := a.Run(ctx, "hi")
	mustState(t, r, err, StateCompleted)
	if r.Metadata["registered"] != "" {
		t.Fatalf("WithoutDefaults kept the registered annotator: %v", r.Metadata)
	}
	if n := len(RegisteredRunAnnotators()); n != 1 {
		t.Fatalf("%d registered annotators, want 1", n)
	}
}
