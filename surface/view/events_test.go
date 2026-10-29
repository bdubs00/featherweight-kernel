package view

import (
	"context"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func TestEventsPage(t *testing.T) {
	ctx := context.Background()
	l := fw.NewMemLedger()
	a, err := fw.New(fw.WithConfig(fw.DefaultConfig()), fw.WithLedger(l), fw.WithBus(fw.NewMemBus()),
		fw.WithAuthn(fw.NoAuthn{}), fw.WithoutDefaults(), fw.WithModel(fw.ModelFunc{}))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	for i := 0; i < 5; i++ {
		if _, err := l.Append(ctx, fw.MustEvent("r1", fw.EvTurnStarted, fw.TurnData{Turn: i})); err != nil {
			t.Fatal(err)
		}
	}
	events, next, err := EventsPage(ctx, a, "r1", 0, 2)
	if err != nil || len(events) != 2 || events[0].Seq != 4 || next != 0 {
		t.Fatalf("tail = %d events from %d, next %d, %v", len(events), events[0].Seq, next, err)
	}
	events, next, err = EventsPage(ctx, a, "r1", 2, 2)
	if err != nil || len(events) != 2 || events[0].Seq != 2 || next != 4 {
		t.Fatalf("page = %d events, next %d, %v", len(events), next, err)
	}
	events, next, err = EventsPage(ctx, a, "r1", 4, 5)
	if err != nil || len(events) != 2 || next != 0 {
		t.Fatalf("last page = %d events, next %d, %v", len(events), next, err)
	}
	if events, _, _ := EventsPage(ctx, a, "none", 0, 5); events == nil || len(events) != 0 {
		t.Errorf("unknown run = %v", events)
	}
}

func TestLineNamesANonZeroExitCode(t *testing.T) {
	code, zero := 1, 0
	cases := []struct {
		data fw.ToolRespondedData
		want string
	}{
		{fw.ToolRespondedData{Tool: "bash", ExitCode: &code, LatencyMS: 6952}, "the tool bash exited 1 in 6952 ms"},
		{fw.ToolRespondedData{Tool: "bash", ExitCode: &zero, LatencyMS: 12}, "the tool bash answered in 12 ms"},
		{fw.ToolRespondedData{Tool: "echo", LatencyMS: 3}, "the tool echo answered in 3 ms"},
	}
	for _, c := range cases {
		if got := describe(fw.MustEvent("r1", fw.EvToolResponded, c.data)); got != c.want {
			t.Errorf("line = %q, want %q", got, c.want)
		}
	}
}

func TestDescribeMemoryChanged(t *testing.T) {
	for _, c := range []struct {
		data fw.MemoryChangedData
		want string
	}{
		{fw.MemoryChangedData{Tool: "memory", Op: fw.FileCreated, Ref: "mem://agent/default/a.md", Size: 12}, "the tool memory created the note mem://agent/default/a.md (12 bytes)"},
		{fw.MemoryChangedData{Tool: "memory", Op: fw.FileRenamed, OldRef: "mem://agent/default/a.md", Ref: "mem://agent/default/b.md"}, "the tool memory renamed the note mem://agent/default/a.md to mem://agent/default/b.md"},
		{fw.MemoryChangedData{Tool: "memory", Op: fw.FileRemoved, Ref: "mem://agent/default/b.md"}, "the tool memory removed the note mem://agent/default/b.md"},
		{fw.MemoryChangedData{Tool: "memory", Op: fw.FileCreated, Ref: "mem://agent/default/c.md", Size: 3, Waiting: true},
			"the tool memory created the note mem://agent/default/c.md (3 bytes); the change waits for a person"},
	} {
		if got := describe(fw.MustEvent("r1", fw.EvMemoryChanged, c.data)); got != c.want {
			t.Errorf("describe = %q, want %q", got, c.want)
		}
	}
}

func TestLineNamesTheSender(t *testing.T) {
	for _, c := range []struct {
		data fw.MessageReceivedData
		want string
	}{
		{fw.MessageReceivedData{Role: fw.RoleUser, Origin: "http", By: "alice", Content: []fw.Block{fw.Text("again")}}, "user message from alice over http: again"},
		{fw.MessageReceivedData{Role: fw.RoleUser, Origin: "http", By: "token", From: &fw.MessageFrom{Kind: fw.FromAgent, Name: "reviewer"}, Content: []fw.Block{fw.Text("rebase")}},
			"user message from the agent reviewer through token over http: rebase"},
		{fw.MessageReceivedData{Role: fw.RoleUser, Origin: "http", By: "token", From: &fw.MessageFrom{Kind: fw.FromPerson, Name: "alice"}, Content: []fw.Block{fw.Text("ship")}},
			"user message from alice through token over http: ship"},
		{fw.MessageReceivedData{Role: fw.RoleUser, Origin: "cli", Content: []fw.Block{fw.Text("go")}}, "user message over cli: go"},
		{fw.MessageReceivedData{Role: fw.RoleUser, Origin: "http", By: "alice", Pinned: true, Content: []fw.Block{fw.Text("Use French.")}}, "pinned user message from alice over http: Use French."},
	} {
		if got := describe(fw.MustEvent("r1", fw.EvMessageReceived, c.data)); got != c.want {
			t.Errorf("line = %q, want %q", got, c.want)
		}
	}
}

func TestLineOfAReaction(t *testing.T) {
	for _, c := range []struct {
		data fw.ReactionData
		want string
	}{
		{fw.ReactionData{Seq: 118, Reaction: fw.ReactionHeart, By: "alice"}, "alice reacted heart to event 118"},
		{fw.ReactionData{Reaction: fw.ReactionDown, By: "alice", Note: "Too\nlong."}, "alice reacted down to the run: Too long."},
		{fw.ReactionData{Seq: 9, Reaction: fw.ReactionUp, By: "bob", Receipt: true}, "bob reacted up to event 9 and accepted the result"},
		{fw.ReactionData{Seq: 9, Reaction: fw.ReactionDown, Receipt: true}, "the process reacted down to event 9 and rejected the result"},
		{fw.ReactionData{Seq: 9, Reaction: fw.ReactionUp, By: "bob", Removed: true}, "bob took back up on event 9"},
		{fw.ReactionData{Reaction: fw.ReactionHeart, By: "bob", Removed: true}, "bob took back heart on the run"},
	} {
		if got := describe(fw.MustEvent("r1", fw.EvRunReacted, c.data)); got != c.want {
			t.Errorf("line = %q, want %q", got, c.want)
		}
	}
}

// A check run starts with no model, and its line says so.
func TestDescribeRunStarted(t *testing.T) {
	for _, c := range []struct {
		data fw.RunStartedData
		want string
	}{
		{fw.RunStartedData{Model: "anthropic/claude-opus-5-5", Tools: []string{"a", "b"}}, "started with the model anthropic/claude-opus-5-5 and 2 tools"},
		{fw.RunStartedData{Tools: []string{"a"}}, "started with 1 tools and no model"},
	} {
		if got := describe(fw.MustEvent("r1", fw.EvRunStarted, c.data)); got != c.want {
			t.Errorf("describe = %q, want %q", got, c.want)
		}
	}
}
