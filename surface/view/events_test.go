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
	} {
		if got := describe(fw.MustEvent("r1", fw.EvMemoryChanged, c.data)); got != c.want {
			t.Errorf("describe = %q, want %q", got, c.want)
		}
	}
}
