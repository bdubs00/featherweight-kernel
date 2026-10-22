package fw

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// countText counts the messages whose text is text.
func countText(msgs []Message, text string) int {
	n := 0
	for _, m := range msgs {
		if TextOf(m.Content) == text {
			n++
		}
	}
	return n
}

// specNames returns the names of tool specifications.
func specNames(specs []ToolSpec) []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}

// After a compaction, the next model request still carries every tool of
// the run, the system prompt still lists the skills of the profile, the
// memory index comes back at the start of the first user message, and a
// pinned message is there verbatim. A second compaction keeps the same
// pinned message once.
func TestCompactionKeepsThePinnedMessageAndTheRun(t *testing.T) {
	ctx := testCtx(t)
	root := t.TempDir()
	writeSkill(t, root, "deploy", "---\nname: deploy\ndescription: \"Ship a release.\"\n---\n", "# Deploy\n")
	cfg := DefaultConfig()
	cfg.Tools.Skills = []string{root}
	entered, release := make(chan struct{}), make(chan struct{})
	slow := NewTool(ToolSpec{Name: "slow", ReadOnly: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
		close(entered)
		<-release
		return TextResult("slow done"), nil
	})
	notes := &contextTool{name: "notes", text: "- /memories/a.md: How to build"}
	s := &stepsModel{steps: []any{
		callTools(use("c1", "slow", `{}`)),
		callTools(use("c2", "echo", `{"text":"two"}`)),
		endTurn("done"),
	}}
	a := newTestAgent(t, s.Model(), WithConfig(cfg), WithTools(slow, echoTool(nil), notes))
	a.k.compactAt = 1
	r, err := a.Start(ctx, "work")
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	pin := "Use the skill deploy for every release, and keep the changelog."
	if err := a.Send(ctx, r.ID, UserText(pin), OriginHTTP, WithMessagePinned()); err != nil {
		t.Fatal(err)
	}
	close(release)
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)

	if len(s.summaries) != 2 || len(s.turns) != 3 {
		t.Fatalf("summaries = %d, turns = %d", len(s.summaries), len(s.turns))
	}
	first := s.turns[0]
	for i, req := range s.turns[1:] {
		if !reflect.DeepEqual(specNames(req.Tools), specNames(first.Tools)) || len(req.Tools) != 3 {
			t.Fatalf("request %d after a compaction has the tools %v, want %v", i+1, specNames(req.Tools), specNames(first.Tools))
		}
		if !strings.Contains(req.System, skillsHeading) || !strings.Contains(req.System, "- deploy: Ship a release.") {
			t.Fatalf("request %d lists no skill: %q", i+1, req.System)
		}
		m0 := req.Messages[0]
		if m0.Role != RoleUser || !strings.HasPrefix(m0.Content[0].Text, `<context source="notes">`) || !strings.Contains(m0.Content[0].Text, "How to build") {
			t.Fatalf("request %d starts with %+v", i+1, m0)
		}
		if n := countText(req.Messages, pin); n != 1 {
			t.Fatalf("request %d holds the pinned message %d times: %+v", i+1, n, req.Messages)
		}
		if !strings.Contains(TextOf(req.Messages[0].Content), "SUMMARY") {
			t.Fatalf("request %d does not start from a summary", i+1)
		}
	}
	// Each compaction records the pinned message among the kept messages,
	// once, right after the summary.
	var kept [][]Message
	for _, e := range ledgerEvents(t, a.Ledger(), r.ID) {
		var d ContextCompactedData
		if e.Type == EvContextCompacted && e.Decode(&d) == nil {
			kept = append(kept, d.Kept)
		}
	}
	if len(kept) != 2 {
		t.Fatalf("compactions = %d", len(kept))
	}
	for i, k := range kept {
		if len(k) != 1 || TextOf(k[0].Content) != pin {
			t.Fatalf("compaction %d kept %+v", i+1, k)
		}
	}
	// The event line says that the message is pinned.
	var line MessageReceivedData
	for _, e := range ledgerEvents(t, a.Ledger(), r.ID) {
		var d MessageReceivedData
		if e.Type == EvMessageReceived && e.Decode(&d) == nil && d.Pinned {
			line = d
		}
	}
	if TextOf(line.Content) != pin {
		t.Fatalf("pinned message in the ledger = %+v", line)
	}
}

// The pinned messages come after the summary in ledger order, before the
// messages that the compactor kept, and a message that is not pinned is
// summarized away.
func TestPinnedMessagesKeepTheirOrder(t *testing.T) {
	ctx := testCtx(t)
	s := &stepsModel{steps: []any{
		endTurn("one"), endTurn("two"), endTurn("three"), endTurn("four"), endTurn("done"),
	}}
	a := newTestAgent(t, s.Model())
	a.k.compactAt = 1
	r, err := a.Run(ctx, "work")
	mustState(t, r, err, StateCompleted)
	for _, m := range []struct {
		text   string
		pinned bool
	}{{"first pin", true}, {"plain", false}, {"second pin", true}} {
		var opts []SendOption
		if m.pinned {
			opts = append(opts, WithMessagePinned())
		}
		if err := a.Send(ctx, r.ID, UserText(m.text), OriginHTTP, opts...); err != nil {
			t.Fatal(err)
		}
		if r, err = a.Wait(ctx, r.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Send(ctx, r.ID, UserText("go on"), OriginHTTP); err != nil {
		t.Fatal(err)
	}
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)
	// Each follow-up compacts. The last compaction keeps both pinned
	// messages and the new question, and the plain message is gone.
	var d ContextCompactedData
	for _, e := range ledgerEvents(t, a.Ledger(), r.ID) {
		if e.Type == EvContextCompacted {
			d = ContextCompactedData{}
			_ = e.Decode(&d)
		}
	}
	if d.Epoch != 4 {
		t.Fatalf("epoch of the last compaction = %d", d.Epoch)
	}
	var texts []string
	for _, m := range d.Kept {
		texts = append(texts, TextOf(m.Content))
	}
	if strings.Join(texts, "|") != "first pin|second pin|go on" {
		t.Fatalf("kept = %q", texts)
	}
}

// The pinned messages of a run hold at most MaxPinnedBytes together, and a
// message past the bound is refused with a text that names it.
func TestPinnedBound(t *testing.T) {
	ctx := testCtx(t)
	a := newTestAgent(t, newScript(endTurn("one"), endTurn("two"), endTurn("three")).Model())
	r, err := a.Run(ctx, "work")
	mustState(t, r, err, StateCompleted)
	if err := a.Send(ctx, r.ID, UserText(strings.Repeat("a", MaxPinnedBytes-10)), OriginHTTP, WithMessagePinned()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Wait(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	err = a.Send(ctx, r.ID, UserText(strings.Repeat("b", 11)), OriginHTTP, WithMessagePinned())
	if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "65536 bytes (64 KB)") {
		t.Fatalf("a pinned message past the bound = %v", err)
	}
	// A message that is not pinned does not count.
	if err := a.Send(ctx, r.ID, UserText(strings.Repeat("c", 100)), OriginHTTP); err != nil {
		t.Fatalf("a message that is not pinned = %v", err)
	}
	if _, err := a.Wait(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	// A restart counts the pinned messages again from the ledger.
	b := newTestAgent(t, newScript().Model(), WithLedger(a.Ledger()))
	a.Close(ctx)
	if got, err := b.Get(ctx, r.ID); err != nil || got.pinnedBytes != MaxPinnedBytes-10 {
		t.Fatalf("pinned bytes after a restart = %d, %v", got.pinnedBytes, err)
	}
}
