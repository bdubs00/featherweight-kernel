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
